//go:build ociintegration

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

package ociblob

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
	"gocloud.dev/blob/driver"
	"gocloud.dev/blob/drivertest"
)

// TestConformance uses the selected OCI identity and an existing bucket root.
// Each harness owns a fresh prefix; cleanup also removes versions and delete markers.
func TestConformance(t *testing.T) {
	t.Parallel()
	drivertest.RunConformanceTests(t, newHarness, nil)
}

type harness struct {
	t      *testing.T
	root   *bucket
	u      url.URL
	prefix string
	once   sync.Once
}

func newHarness(ctx context.Context, t *testing.T) (drivertest.Harness, error) {
	raw := os.Getenv("OCI_BLOB_TEST_URL")
	if raw == "" {
		return nil, errors.New("OCI_BLOB_TEST_URL must name an existing oci://namespace/bucket root")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if strings.Count(strings.Trim(u.Path, "/"), "/") != 0 || strings.Trim(u.Path, "/") == "" {
		return nil, errors.New("OCI_BLOB_TEST_URL must name a bucket root without an object prefix")
	}
	drv, err := openBucket(ctx, u)
	if err != nil {
		return nil, err
	}
	root, ok := drv.(*bucket)
	if !ok {
		return nil, errors.New("OCI_BLOB_TEST_URL must name a bucket root")
	}
	prefix := "pulumi-conformance/" + uuid.NewString() + "/"
	u.Path = "/" + root.name + "/" + prefix
	u.RawPath = ""
	h := &harness{t: t, root: root, u: *u, prefix: prefix}
	t.Cleanup(h.Close)
	return h, nil
}

func (h *harness) MakeDriver(ctx context.Context) (driver.Bucket, error) {
	return openBucket(ctx, &h.u)
}

func (h *harness) MakeDriverForNonexistentBucket(ctx context.Context) (driver.Bucket, error) {
	u := h.u
	u.Path = "/pulumi-conformance-" + uuid.NewString() + "/" + h.prefix
	return openBucket(ctx, &u)
}

func (h *harness) HTTPClient() *http.Client { return nil }

func (h *harness) Close() {
	h.t.Helper()
	h.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := h.cleanup(ctx); err != nil {
			h.t.Errorf("cleaning OCI conformance prefix %q: %v", h.prefix, err)
		}
	})
}

func (h *harness) versions(ctx context.Context) ([]objectstorage.ObjectVersionSummary, error) {
	request := objectstorage.ListObjectVersionsRequest{
		NamespaceName: &h.root.namespace, BucketName: &h.root.name, Prefix: &h.prefix,
	}
	var versions []objectstorage.ObjectVersionSummary
	for {
		response, err := h.root.client.ListObjectVersions(ctx, request)
		if err != nil {
			return nil, err
		}
		versions = append(versions, response.Items...)
		if value(response.OpcNextPage) == "" {
			return versions, nil
		}
		request.Page = response.OpcNextPage
	}
}

func (h *harness) cleanup(ctx context.Context) error {
	id := strings.TrimSuffix(strings.TrimPrefix(h.prefix, "pulumi-conformance/"), "/")
	if _, err := uuid.Parse(id); err != nil || h.prefix != "pulumi-conformance/"+id+"/" {
		return errors.New("refusing cleanup outside the exact generated conformance prefix")
	}
	versions, err := h.versions(ctx)
	if err != nil {
		return err
	}
	// Validate the complete snapshot before any deletion, including delete markers.
	for _, version := range versions {
		if version.Name == nil || !strings.HasPrefix(*version.Name, h.prefix) || value(version.VersionId) == "" {
			return errors.New("refusing cleanup of an object outside the generated prefix or without a version ID")
		}
	}
	for _, version := range versions {
		if _, err := h.root.client.DeleteObject(ctx, objectstorage.DeleteObjectRequest{
			NamespaceName: &h.root.namespace, BucketName: &h.root.name,
			ObjectName: version.Name, VersionId: version.VersionId,
		}); err != nil {
			return err
		}
	}
	remaining, err := h.versions(ctx)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return fmt.Errorf("conformance prefix still contains %d versions or delete markers", len(remaining))
	}
	objects, err := h.root.client.ListObjects(ctx, objectstorage.ListObjectsRequest{
		NamespaceName: &h.root.namespace, BucketName: &h.root.name,
		Prefix: &h.prefix, Limit: new(1),
	})
	if err != nil {
		return err
	}
	if len(objects.Objects) != 0 || value(objects.NextStartWith) != "" {
		return errors.New("conformance prefix still contains objects")
	}
	return nil
}
