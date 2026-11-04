// Copyright 2026, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ociblob implements native OCI Object Storage URLs for the DIY backend.
// URLs have the form oci://namespace/bucket/prefix and contain no credentials.
// Authentication uses the OCI_CLI_AUTH, OCI_CLI_CONFIG_FILE, and OCI_CLI_PROFILE environment variables.
package ociblob

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
	"gocloud.dev/blob"
	"gocloud.dev/blob/driver"
	"gocloud.dev/gcerrors"

	"github.com/pulumi/pulumi/pkg/v3/util/ociutil"
)

// Scheme is the native OCI Object Storage backend URL scheme.
const Scheme = "oci"

func init() {
	blob.DefaultURLMux().RegisterBucket(Scheme, &URLOpener{})
}

// URLOpener opens OCI Object Storage buckets with the selected executor identity.
type URLOpener struct{}

// OpenBucketURL opens oci://namespace/bucket/prefix without changing the bucket.
func (o *URLOpener) OpenBucketURL(ctx context.Context, u *url.URL) (*blob.Bucket, error) {
	b, err := openBucket(ctx, u)
	if err != nil {
		return nil, err
	}
	return blob.NewBucket(b), nil
}

func openBucket(ctx context.Context, u *url.URL) (driver.Bucket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if u.Scheme != Scheme || u.User != nil || u.Host == "" || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("OCI backend URL requires oci://namespace/bucket/prefix " +
			"without credentials or query parameters")
	}
	bucketName, prefix, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	if bucketName == "" {
		return nil, errors.New("OCI backend URL requires a bucket name")
	}
	provider, err := ociutil.ConfigurationProvider(nil)
	if err != nil {
		return nil, err
	}
	client, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	region := os.Getenv("OCI_CLI_REGION")
	if region == "" {
		region, err = provider.Region()
		if err != nil {
			return nil, err
		}
	}
	client.SetRegion(region)
	policy := common.NoRetryPolicy()
	client.SetCustomClientConfiguration(common.CustomClientConfiguration{RetryPolicy: &policy})
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	var b driver.Bucket = &bucket{client: &client, namespace: u.Host, name: bucketName, prefix: prefix, region: region}
	if prefix != "" {
		b = driver.NewPrefixedBucket(b, prefix)
	}
	return b, nil
}

type bucket struct {
	client    *objectstorage.ObjectStorageClient
	namespace string
	name      string
	prefix    string
	region    string
}

func (b *bucket) ErrorCode(err error) gcerrors.ErrorCode { return ociutil.ErrorCode(err) }
func (b *bucket) ErrorAs(err error, target any) bool     { return ociutil.ErrorAs(err, target) }
func (b *bucket) Close() error                           { return nil }

// ConsoleURL exposes an authenticated OCI Console bucket page and physical object keys.
// It does not grant access to the bucket or sign an object URL.
type ConsoleURL interface {
	ConsoleURL() (string, error)
	ObjectKey(key string) string
}

func (b *bucket) ConsoleURL() (string, error) {
	if b.region == "" {
		return "", errors.New("OCI Console URL requires the bucket's effective region")
	}
	u := url.URL{
		Scheme: "https", Host: "cloud.oracle.com",
		Path:     "/object-storage/buckets/" + b.namespace + "/" + b.name + "/objects",
		RawQuery: url.Values{"region": {b.region}}.Encode(),
	}
	return u.String(), nil
}

func (b *bucket) ObjectKey(key string) string { return b.escapeKey(b.prefix + key) }

func (b *bucket) As(target any) bool {
	if console, ok := target.(*ConsoleURL); ok && console != nil {
		*console = b
		return true
	}
	if client, ok := target.(**objectstorage.ObjectStorageClient); ok {
		*client = b.client
		return true
	}
	return false
}

func (b *bucket) Attributes(ctx context.Context, key string) (*driver.Attributes, error) {
	key = b.escapeKey(key)
	response, err := b.client.HeadObject(ctx, objectstorage.HeadObjectRequest{
		NamespaceName: &b.namespace, BucketName: &b.name, ObjectName: &key,
	})
	if err != nil {
		return nil, err
	}
	if response.ContentLength == nil || response.ContentType == nil || response.LastModified == nil {
		return nil, errors.New("OCI HeadObject response omitted required object attributes")
	}
	var md5 []byte
	if response.ContentMd5 != nil {
		md5, err = base64.StdEncoding.DecodeString(*response.ContentMd5)
		if err != nil {
			return nil, fmt.Errorf("invalid OCI object checksum: %w", err)
		}
	}
	metadata, err := decodeMetadata(response.OpcMeta)
	if err != nil {
		return nil, err
	}
	etag := value(response.ETag)
	if etag != "" && !strings.HasPrefix(etag, "\"") && !strings.HasPrefix(etag, "W/\"") {
		etag = strconv.Quote(etag)
	}
	return &driver.Attributes{
		ContentType: *response.ContentType, Size: *response.ContentLength,
		ModTime: response.LastModified.Time, Metadata: metadata, MD5: md5,
		CacheControl: value(response.CacheControl), ContentEncoding: value(response.ContentEncoding),
		ContentDisposition: value(response.ContentDisposition), ContentLanguage: value(response.ContentLanguage),
		ETag: etag, AsFunc: func(target any) bool {
			if result, ok := target.(*objectstorage.HeadObjectResponse); ok {
				*result = response
				return true
			}
			return false
		},
	}, nil
}

func (b *bucket) ListPaged(ctx context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	request := objectstorage.ListObjectsRequest{
		NamespaceName: &b.namespace, BucketName: &b.name,
		Prefix: new(b.escapeKey(opts.Prefix)), Fields: new("name,size,timeModified"),
	}
	if opts.BeforeList != nil {
		if err := opts.BeforeList(func(target any) bool {
			if result, ok := target.(**objectstorage.ListObjectsRequest); ok {
				*result = &request
				return true
			}
			return false
		}); err != nil {
			return nil, err
		}
	}
	// OCI sorts encoded names and supports only '/' as a delimiter. Group and
	// paginate decoded names to preserve Go Cloud's ordering for every delimiter.
	objects := make(map[string]*driver.ListObject)
	for {
		response, err := b.client.ListObjects(ctx, request)
		if err != nil {
			return nil, err
		}
		for _, object := range response.Objects {
			key, err := b.unescapeKey(value(object.Name))
			if err != nil {
				return nil, err
			}
			if !strings.HasPrefix(key, opts.Prefix) {
				continue
			}
			item := &driver.ListObject{Key: key, Size: value(object.Size)}
			if opts.Delimiter != "" {
				if i := strings.Index(key[len(opts.Prefix):], opts.Delimiter); i >= 0 {
					item = &driver.ListObject{Key: key[:len(opts.Prefix)+i+len(opts.Delimiter)], IsDir: true}
				}
			}
			if object.TimeModified != nil && !item.IsDir {
				item.ModTime = object.TimeModified.Time
			}
			if len(opts.PageToken) == 0 || item.Key > string(opts.PageToken) {
				objects[item.Key] = item
			}
		}
		if value(response.NextStartWith) == "" {
			break
		}
		request.Start = response.NextStartWith
	}
	page := &driver.ListPage{}
	for _, item := range objects {
		page.Objects = append(page.Objects, item)
	}
	slices.SortFunc(page.Objects, func(a, b *driver.ListObject) int { return strings.Compare(a.Key, b.Key) })
	if opts.PageSize > 0 && len(page.Objects) > opts.PageSize {
		page.Objects = page.Objects[:opts.PageSize]
		page.NextPageToken = []byte(page.Objects[len(page.Objects)-1].Key)
	}
	return page, nil
}

type reader struct {
	io.ReadCloser
	attributes driver.ReaderAttributes
}

func (r *reader) Attributes() *driver.ReaderAttributes { return &r.attributes }
func (r *reader) As(any) bool                          { return false }

func (b *bucket) NewRangeReader(ctx context.Context, key string, offset, length int64,
	opts *driver.ReaderOptions,
) (driver.Reader, error) {
	attributes, err := b.Attributes(ctx, key)
	if err != nil {
		return nil, err
	}
	return b.newRangeReader(ctx, key, offset, length, opts, attributes)
}

func (b *bucket) newRangeReader(ctx context.Context, key string, offset, length int64,
	opts *driver.ReaderOptions, attributes *driver.Attributes,
) (driver.Reader, error) {
	key = b.escapeKey(key)
	var head objectstorage.HeadObjectResponse
	if !attributes.AsFunc(&head) {
		return nil, errors.New("OCI reader attributes omitted the native HeadObject response")
	}
	request := objectstorage.GetObjectRequest{
		NamespaceName: &b.namespace, BucketName: &b.name, ObjectName: &key,
		IfMatch: head.ETag,
	}
	if length > 0 {
		request.Range = new(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	} else if offset > 0 {
		request.Range = new(fmt.Sprintf("bytes=%d-", offset))
	}
	if opts.BeforeRead != nil {
		if err := opts.BeforeRead(func(target any) bool {
			if result, ok := target.(**objectstorage.GetObjectRequest); ok {
				*result = &request
				return true
			}
			return false
		}); err != nil {
			return nil, err
		}
	}
	result := &reader{attributes: driver.ReaderAttributes{
		ContentType: attributes.ContentType, ModTime: attributes.ModTime, Size: attributes.Size,
	}}
	if length == 0 {
		result.ReadCloser = io.NopCloser(bytes.NewReader(nil))
		return result, nil
	}
	response, err := b.client.GetObject(ctx, request)
	if err != nil {
		return nil, err
	}
	result.ReadCloser = response.Content
	return result, nil
}

type writer struct {
	bytes.Buffer
	ctx     context.Context
	client  *objectstorage.ObjectStorageClient
	request objectstorage.PutObjectRequest
}

func (w *writer) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.Buffer.Write(data)
}

func (w *writer) Close() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.request.ContentLength = new(int64(w.Len()))
	w.request.PutObjectBody = common.NewOCIReadSeekCloser(io.NopCloser(bytes.NewReader(w.Bytes())))
	_, err := w.client.PutObject(w.ctx, w.request)
	return err
}

func (b *bucket) NewTypedWriter(ctx context.Context, key, contentType string,
	opts *driver.WriterOptions,
) (driver.Writer, error) {
	key = b.escapeKey(key)
	metadata := make(map[string]string, len(opts.Metadata))
	for k, v := range opts.Metadata {
		metadata[url.QueryEscape(k)] = url.QueryEscape(v)
	}
	w := &writer{ctx: ctx, client: b.client, request: objectstorage.PutObjectRequest{
		NamespaceName: &b.namespace, BucketName: &b.name, ObjectName: &key,
		ContentType: &contentType, CacheControl: &opts.CacheControl,
		ContentEncoding: &opts.ContentEncoding, ContentDisposition: &opts.ContentDisposition,
		ContentLanguage: &opts.ContentLanguage, OpcMeta: metadata,
	}}
	if opts.IfNotExist {
		w.request.IfNoneMatch = new("*")
	}
	if opts.BeforeWrite != nil {
		if err := opts.BeforeWrite(func(target any) bool {
			if result, ok := target.(**objectstorage.PutObjectRequest); ok {
				*result = &w.request
				return true
			}
			return false
		}); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func (b *bucket) Copy(ctx context.Context, dst, src string, opts *driver.CopyOptions) error {
	if opts.BeforeCopy != nil {
		if err := opts.BeforeCopy(func(any) bool { return false }); err != nil {
			return err
		}
	}
	attributes, err := b.Attributes(ctx, src)
	if err != nil {
		return err
	}
	r, err := b.newRangeReader(ctx, src, 0, -1, &driver.ReaderOptions{}, attributes)
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := b.NewTypedWriter(ctx, dst, attributes.ContentType, &driver.WriterOptions{
		CacheControl: attributes.CacheControl, ContentEncoding: attributes.ContentEncoding,
		ContentDisposition: attributes.ContentDisposition, ContentLanguage: attributes.ContentLanguage,
		Metadata: attributes.Metadata,
	})
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		return err
	}
	return w.Close()
}

func (b *bucket) Delete(ctx context.Context, key string, opts *driver.DeleteOptions) error {
	key = b.escapeKey(key)
	request := objectstorage.DeleteObjectRequest{
		NamespaceName: &b.namespace, BucketName: &b.name, ObjectName: &key,
	}
	if opts.BeforeDelete != nil {
		if err := opts.BeforeDelete(func(target any) bool {
			if result, ok := target.(**objectstorage.DeleteObjectRequest); ok {
				*result = &request
				return true
			}
			return false
		}); err != nil {
			return err
		}
	}
	_, err := b.client.DeleteObject(ctx, request)
	return err
}

func (b *bucket) SignedURL(context.Context, string, *driver.SignedURLOptions) (string, error) {
	return "", gcerrors.ErrUnimplemented
}

func value[T any](pointer *T) (result T) {
	if pointer != nil {
		return *pointer
	}
	return result
}

// escapeKey leaves the URL ownership prefix literal, including any percent signs.
func (b *bucket) escapeKey(key string) string {
	escaped := strings.NewReplacer("%", "%25", "\n", "%0A", "\r", "%0D", "\x00", "%00")
	return b.prefix + escaped.Replace(strings.TrimPrefix(key, b.prefix))
}

func (b *bucket) unescapeKey(key string) (string, error) {
	if !strings.HasPrefix(key, b.prefix) {
		return "", fmt.Errorf("OCI object name is outside bucket prefix %q", b.prefix)
	}
	decoded, err := url.PathUnescape(strings.TrimPrefix(key, b.prefix))
	if err != nil {
		return "", fmt.Errorf("invalid escaped OCI object name: %w", err)
	}
	return b.prefix + decoded, nil
}

func decodeMetadata(metadata map[string]string) (map[string]string, error) {
	decoded := make(map[string]string, len(metadata))
	for key, value := range metadata {
		key, err := url.QueryUnescape(key)
		if err != nil {
			return nil, fmt.Errorf("invalid escaped OCI metadata key: %w", err)
		}
		value, err = url.QueryUnescape(value)
		if err != nil {
			return nil, fmt.Errorf("invalid escaped OCI metadata value: %w", err)
		}
		decoded[key] = value
	}
	return decoded, nil
}
