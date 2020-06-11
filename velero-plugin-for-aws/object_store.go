/*
Copyright 2017, 2019 the Velero contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/md5"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	veleroplugin "github.com/vmware-tanzu/velero/pkg/plugin/framework"
)

const (
	s3URLKey                     = "s3Url"
	publicURLKey                 = "publicUrl"
	kmsKeyIDKey                  = "kmsKeyId"
	s3ForcePathStyleKey          = "s3ForcePathStyle"
	bucketKey                    = "bucket"
	signatureVersionKey          = "signatureVersion"
	credentialProfileKey         = "profile"
	serverSideEncryptionKey      = "serverSideEncryption"
	customerEncryptionKeyFileKey = "customerEncryptionKeyFile"
	insecureSkipTLSVerifyKey     = "insecureSkipTLSVerify"
	caCertKey                    = "caCert"
)

type s3Interface interface {
	HeadObject(input *s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	GetObject(input *s3.GetObjectInput) (*s3.GetObjectOutput, error)
	ListObjectsV2Pages(input *s3.ListObjectsV2Input, fn func(*s3.ListObjectsV2Output, bool) bool) error
	DeleteObject(input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error)
	GetObjectRequest(input *s3.GetObjectInput) (req *request.Request, output *s3.GetObjectOutput)
}

type ObjectStore struct {
	log                      logrus.FieldLogger
	s3                       s3Interface
	preSignS3                s3Interface
	s3Uploader               *s3manager.Uploader
	kmsKeyID                 string
	signatureVersion         string
	serverSideEncryption     string
	customerEncryptionKeyB64 string
	customerEncryptionKey    []byte
}

func newObjectStore(logger logrus.FieldLogger) *ObjectStore {
	return &ObjectStore{log: logger}
}

func isValidSignatureVersion(signatureVersion string) bool {
	switch signatureVersion {
	case "1", "4":
		return true
	}
	return false
}

func (o *ObjectStore) Init(config map[string]string) error {
	if err := veleroplugin.ValidateObjectStoreConfigKeys(config,
		regionKey,
		s3URLKey,
		publicURLKey,
		kmsKeyIDKey,
		s3ForcePathStyleKey,
		signatureVersionKey,
		credentialProfileKey,
		serverSideEncryptionKey,
		customerEncryptionKeyFileKey,
		insecureSkipTLSVerifyKey,
	); err != nil {
		return err
	}

	var (
		region                    = config[regionKey]
		s3URL                     = config[s3URLKey]
		publicURL                 = config[publicURLKey]
		kmsKeyID                  = config[kmsKeyIDKey]
		s3ForcePathStyleVal       = config[s3ForcePathStyleKey]
		signatureVersion          = config[signatureVersionKey]
		credentialProfile         = config[credentialProfileKey]
		serverSideEncryption      = config[serverSideEncryptionKey]
		customerEncryptionKeyFile = config[customerEncryptionKeyFileKey]
		insecureSkipTLSVerifyVal  = config[insecureSkipTLSVerifyKey]

		// note that bucket is automatically added to the config map
		// by the server from the ObjectStorageProviderConfig so
		// doesn't need to be explicitly set by the user within
		// config.
		bucket                = config[bucketKey]
		caCert                = config[caCertKey]
		s3ForcePathStyle      bool
		insecureSkipTLSVerify bool
		err                   error
	)

	if s3ForcePathStyleVal != "" {
		if s3ForcePathStyle, err = strconv.ParseBool(s3ForcePathStyleVal); err != nil {
			return errors.Wrapf(err, "could not parse %s (expected bool)", s3ForcePathStyleKey)
		}
	}

	// AWS (not an alternate S3-compatible API) and region not
	// explicitly specified: determine the bucket's region
	if s3URL == "" && region == "" {
		var err error

		region, err = GetBucketRegion(bucket)
		if err != nil {
			return err
		}
	}

	serverConfig, err := newAWSConfig(s3URL, region, s3ForcePathStyle)
	if err != nil {
		return err
	}

	if insecureSkipTLSVerifyVal != "" {
		if insecureSkipTLSVerify, err = strconv.ParseBool(insecureSkipTLSVerifyVal); err != nil {
			return errors.Wrapf(err, "could not parse %s (expected bool)", insecureSkipTLSVerifyKey)
		}
	}

	if insecureSkipTLSVerify {
		defaultTransport := http.DefaultTransport.(*http.Transport)
		serverConfig.HTTPClient = &http.Client{
			// Copied from net/http
			Transport: &http.Transport{
				Proxy:                 defaultTransport.Proxy,
				DialContext:           defaultTransport.DialContext,
				MaxIdleConns:          defaultTransport.MaxIdleConns,
				IdleConnTimeout:       defaultTransport.IdleConnTimeout,
				TLSHandshakeTimeout:   defaultTransport.TLSHandshakeTimeout,
				ExpectContinueTimeout: defaultTransport.ExpectContinueTimeout,
				// Set insecureSkipVerify true
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		}
	}

	sessionOptions := session.Options{Config: *serverConfig, Profile: credentialProfile}
	if len(caCert) > 0 {
		sessionOptions.CustomCABundle = strings.NewReader(caCert)
	}
	serverSession, err := getSession(sessionOptions)
	if err != nil {
		return err
	}

	o.s3 = s3.New(serverSession)
	o.s3Uploader = s3manager.NewUploader(serverSession)
	o.kmsKeyID = kmsKeyID
	o.serverSideEncryption = serverSideEncryption

	if signatureVersion != "" {
		if !isValidSignatureVersion(signatureVersion) {
			return errors.Errorf("invalid signature version: %s", signatureVersion)
		}
		o.signatureVersion = signatureVersion
	}

	if publicURL != "" {
		publicConfig, err := newAWSConfig(publicURL, region, s3ForcePathStyle)
		if err != nil {
			return err
		}
		sessionOptions := session.Options{Config: *publicConfig, Profile: credentialProfile}
		if len(caCert) > 0 {
			sessionOptions.CustomCABundle = strings.NewReader(caCert)
		}
		publicSession, err := getSession(sessionOptions)
		if err != nil {
			return err
		}
		o.preSignS3 = s3.New(publicSession)
	} else {
		o.preSignS3 = o.s3
	}

	if customerEncryptionKeyFile != "" {
		dat, err := ioutil.ReadFile(customerEncryptionKeyFile)
		if err != nil {
			return err
		}
		o.customerEncryptionKeyB64 = string(dat)
		o.customerEncryptionKey, err = base64.StdEncoding.DecodeString(string(dat))
		if err != nil {
			return fmt.Errorf("Failed to decode sse_customer_key: %s", err.Error())
		}
	}

	return nil
}

func newAWSConfig(url, region string, forcePathStyle bool) (*aws.Config, error) {
	awsConfig := aws.NewConfig().
		WithRegion(region).
		WithS3ForcePathStyle(forcePathStyle)

	if url != "" {
		if !IsValidS3URLScheme(url) {
			return nil, errors.Errorf("Invalid s3 url %s, URL must be valid according to https://golang.org/pkg/net/url/#Parse and start with http:// or https://", url)
		}

		awsConfig = awsConfig.WithEndpointResolver(
			endpoints.ResolverFunc(func(service, region string, optFns ...func(*endpoints.Options)) (endpoints.ResolvedEndpoint, error) {
				if service == endpoints.S3ServiceID {
					return endpoints.ResolvedEndpoint{
						URL: url,
					}, nil
				}

				return endpoints.DefaultResolver().EndpointFor(service, region, optFns...)
			}),
		)
	}

	return awsConfig, nil
}

func (o *ObjectStore) PutObject(bucket, key string, body io.Reader) error {
	req := &s3manager.UploadInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   body,
	}

	switch {
	// if kmsKeyID is not empty, assume a server-side encryption (SSE)
	// algorithm of "aws:kms"
	case o.kmsKeyID != "":
		req.ServerSideEncryption = aws.String("aws:kms")
		req.SSEKMSKeyId = &o.kmsKeyID
	case o.serverSideEncryption != "" && o.customerEncryptionKey != nil:
		req.SSECustomerAlgorithm = aws.String(o.serverSideEncryption)
		req.SSECustomerKey = aws.String(string(o.customerEncryptionKey))
		req.SSECustomerKeyMD5 = aws.String(o.getSSECustomerKeyMD5())
	// otherwise, use the SSE algorithm specified, if any
	case o.serverSideEncryption != "":
		req.ServerSideEncryption = aws.String(o.serverSideEncryption)
	}

	_, err := o.s3Uploader.Upload(req)

	return errors.Wrapf(err, "error putting object %s", key)
}

func (o *ObjectStore) getSSECustomerKeyMD5() string {
	b := md5.Sum(o.customerEncryptionKey)
	return base64.StdEncoding.EncodeToString(b[:])
}

const notFoundCode = "NotFound"

// ObjectExists checks if there is an object with the given key in the object storage bucket.
func (o *ObjectStore) ObjectExists(bucket, key string) (bool, error) {
	log := o.log.WithFields(
		logrus.Fields{
			"bucket": bucket,
			"key":    key,
		},
	)

	req := &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}

	if o.serverSideEncryption != "" && o.customerEncryptionKey != nil {
		req.SSECustomerAlgorithm = aws.String(o.serverSideEncryption)
		req.SSECustomerKey = aws.String(string(o.customerEncryptionKey))
		req.SSECustomerKeyMD5 = aws.String(o.getSSECustomerKeyMD5())
	}

	log.Debug("Checking if object exists")
	if _, err := o.s3.HeadObject(req); err != nil {
		log.Debug("Checking for AWS specific error information")
		if aerr, ok := err.(awserr.Error); ok {
			log.WithFields(
				logrus.Fields{
					"code":    aerr.Code(),
					"message": aerr.Message(),
				},
			).Debugf("awserr.Error contents (origErr=%v)", aerr.OrigErr())

			// The code will be NotFound if the key doesn't exist.
			// See https://github.com/aws/aws-sdk-go/issues/1208 and https://github.com/aws/aws-sdk-go/pull/1213.
			log.Debugf("Checking for code=%s", notFoundCode)
			log.Debugf("Current code=%s", aerr.Code())
			if aerr.Code() == notFoundCode {
				log.Debug("Object doesn't exist - got not found")
				return false, nil
			}
		}
		return false, errors.WithStack(err)
	}

	log.Debug("Object exists")
	return true, nil
}

func (o *ObjectStore) GetObject(bucket, key string) (io.ReadCloser, error) {
	req := &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}

	if o.serverSideEncryption != "" && o.customerEncryptionKey != nil {
		req.SSECustomerAlgorithm = aws.String(o.serverSideEncryption)
		req.SSECustomerKey = aws.String(string(o.customerEncryptionKey))
		req.SSECustomerKeyMD5 = aws.String(o.getSSECustomerKeyMD5())
	}

	res, err := o.s3.GetObject(req)
	if err != nil {
		return nil, errors.Wrapf(err, "error getting object %s", key)
	}

	return res.Body, nil
}

func (o *ObjectStore) ListCommonPrefixes(bucket, prefix, delimiter string) ([]string, error) {
	req := &s3.ListObjectsV2Input{
		Bucket:    &bucket,
		Prefix:    &prefix,
		Delimiter: &delimiter,
	}

	var ret []string
	err := o.s3.ListObjectsV2Pages(req, func(page *s3.ListObjectsV2Output, lastPage bool) bool {
		for _, prefix := range page.CommonPrefixes {
			ret = append(ret, *prefix.Prefix)
		}
		return !lastPage
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return ret, nil
}

func (o *ObjectStore) ListObjects(bucket, prefix string) ([]string, error) {
	req := &s3.ListObjectsV2Input{
		Bucket: &bucket,
		Prefix: &prefix,
	}

	var ret []string
	err := o.s3.ListObjectsV2Pages(req, func(page *s3.ListObjectsV2Output, lastPage bool) bool {
		for _, obj := range page.Contents {
			ret = append(ret, *obj.Key)
		}
		return !lastPage
	})

	if err != nil {
		return nil, errors.WithStack(err)
	}

	// ensure that returned objects are in a consistent order so that the deletion logic deletes the objects before
	// the pseudo-folder prefix object for s3 providers (such as Quobyte) that return the pseudo-folder as an object.
	// See https://github.com/vmware-tanzu/velero/pull/999
	sort.Sort(sort.Reverse(sort.StringSlice(ret)))

	return ret, nil
}

func (o *ObjectStore) DeleteObject(bucket, key string) error {
	req := &s3.DeleteObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}

	_, err := o.s3.DeleteObject(req)

	return errors.Wrapf(err, "error deleting object %s", key)
}

func (o *ObjectStore) CreateSignedURL(bucket, key string, ttl time.Duration) (string, map[string]string, error) {
	log := o.log.WithFields(
		logrus.Fields{
			"bucket": bucket,
			"key":    key,
		},
	)
	log.Info("Start CreateSignedURL")

	req, _ := o.preSignS3.GetObjectRequest(&s3.GetObjectInput{
		Bucket:               aws.String(bucket),
		Key:                  aws.String(key),
		SSECustomerAlgorithm: aws.String(o.serverSideEncryption),
		SSECustomerKey:       aws.String(string(o.customerEncryptionKey)),
		SSECustomerKeyMD5:    aws.String(o.getSSECustomerKeyMD5()),
	})

	if o.signatureVersion == "1" {
		req.Handlers.Sign.Remove(v4.SignRequestHandler)
		req.Handlers.Sign.PushBackNamed(v1SignRequestHandler)
	}

	headers := map[string]string(nil)
	if o.serverSideEncryption != "" && o.customerEncryptionKey != nil {
		headers = make(map[string]string)
		headers["x-amz-server-side-encryption-customer-algorithm"] = o.serverSideEncryption
		headers["x-amz-server-side-encryption-customer-key"] = o.customerEncryptionKeyB64
		headers["x-amz-server-side-encryption-customer-key-md5"] = o.getSSECustomerKeyMD5()

		log.Info("CreateSignedURL add sse headers")

	}

	url, err := req.Presign(ttl)

	return url, headers, err
}
