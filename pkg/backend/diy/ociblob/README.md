# Native OCI Object Storage driver

`oci://namespace/bucket/prefix` uses the executor's OCI configuration provider and
an existing bucket. The URL contains no credentials. The URL prefix remains a
literal physical ownership boundary. Object keys below that boundary escape `%`,
line feed, carriage return, and NUL reversibly; metadata keys and values use URL
query escaping to fit OCI HTTP headers.

OCI only accepts `/` as a list delimiter and orders encoded names. The driver
therefore reads every OCI page matching the prefix, decodes names, groups arbitrary
Go Cloud delimiters, sorts, and applies the requested logical page size. Each
logical page reads the matching prefix again. Writes buffer the object in memory;
copy streams through the same read/write contracts. Signed URLs are unsupported;
the driver does not create pre-authenticated requests. `Bucket.As` exposes a
separate `ConsoleURL` capability: an authenticated bucket page in the effective OCI
region, plus the physical object key. The DIY backend displays this as
`State bucket (OCI Console)` with a separate `Checkpoint key`; it is not a signed
checkpoint URL. The bucket route follows the [Oracle Console URL example][console].

[console]: https://docs.oracle.com/en/middleware/bi/analytics-server/user-oas/visualizing-data-oracle-analytics-server.pdf

## Real conformance

From `pkg/`, with the declared Go toolchain and selected OCI identity:

```sh
OCI_BLOB_TEST_URL=oci://namespace/existing-bucket \
  go test -count=1 -tags ociintegration -run '^TestConformance$' -v ./backend/diy/ociblob
```

`OCI_BLOB_TEST_URL` is required and must identify an existing bucket root. The
harness creates a new `pulumi-conformance/<uuid>/` prefix for every test. It snapshots
all object versions and delete markers with `ListObjectVersions`, validates their
ownership before deletion, deletes each version by ID, then verifies that both
versions and visible objects are absent. The identity needs list/read/write/delete
and version-management permissions for this test prefix. Cleanup is registered
immediately and runs once even when test preparation fails. The harness never
creates or deletes buckets.

The official Go Cloud conformance suite skips its SignedURL capability test when
the driver returns Unimplemented. Record this as unsupported coverage, separately
from tests executed against OCI. A compile-only invocation does not prove runtime
conformance.
