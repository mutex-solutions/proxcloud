package client

import (
	"context"
	"errors"
	"io"

	"github.com/shopspring/decimal"
)

// objectstore.go = the bucket read/write surface on the Client facade. Every call dispatches to the
// Ceph RGW (S3) backend; a bucket's externalId == the bucket name.
// data = DataBucket{bucketName, objectCount, sizeInGb, sizeInBytes, storageBackend}.

// bucketGiB converts bytes → GiB: bytes / 1073741824, 2dp HALF_UP.
func bucketGiB(bytes int64) string {
	return decimal.NewFromInt(bytes).DivRound(decimal.NewFromInt(1073741824), 2).String()
}

// ErrBucketFeatureUnsupported is returned when an S3 bucket feature (website, versioning, object lock,
// quota, lifecycle, CORS, tagging, policy, per-key grants) is requested on a client without a ceph-s3
// backend.
var ErrBucketFeatureUnsupported = errors.New("cloud: this bucket feature is only available on S3 (Ceph) object storage")

// ErrWebsiteUnsupported is the same sentinel, kept for the website call sites' readability.
var ErrWebsiteUnsupported = ErrBucketFeatureUnsupported

// BucketWebsite is a bucket's static-website state. PublicObjects records the security-relevant fact that
// an enabled website makes every object in the bucket anonymously readable (via a bucket policy).
type BucketWebsite struct {
	Enabled       bool   `json:"enabled"`
	IndexDocument string `json:"indexDocument,omitempty"`
	ErrorDocument string `json:"errorDocument,omitempty"`
	URL           string `json:"url,omitempty"`
	PublicObjects bool   `json:"publicObjects"`
}

// GetBucketWebsite reports the bucket's static-website configuration (ceph-s3 only).
func (c *Client) GetBucketWebsite(ctx context.Context, bucket string) (*BucketWebsite, error) {
	if c.ceph == nil {
		return nil, ErrWebsiteUnsupported
	}
	enabled, index, errDoc, err := c.ceph.getWebsite(ctx, bucket)
	if err != nil {
		return nil, err
	}
	w := &BucketWebsite{Enabled: enabled, IndexDocument: index, ErrorDocument: errDoc, PublicObjects: enabled}
	if enabled {
		w.URL = c.ceph.websiteURL(bucket)
	}
	return w, nil
}

// EnableBucketWebsite serves the bucket as a static website. ⚠ This also installs a bucket policy that
// makes EVERY object in the bucket anonymously readable — the returned BucketWebsite says so
// (PublicObjects), and callers must show that to the user.
func (c *Client) EnableBucketWebsite(ctx context.Context, bucket, indexDoc, errorDoc string) (*BucketWebsite, error) {
	if c.ceph == nil {
		return nil, ErrWebsiteUnsupported
	}
	if err := c.ceph.enableWebsite(ctx, bucket, indexDoc, errorDoc); err != nil {
		return nil, err
	}
	return c.GetBucketWebsite(ctx, bucket)
}

// DisableBucketWebsite stops serving the bucket as a website and removes the public-read policy.
func (c *Client) DisableBucketWebsite(ctx context.Context, bucket string) error {
	if c.ceph == nil {
		return ErrWebsiteUnsupported
	}
	return c.ceph.disableWebsite(ctx, bucket)
}

// BackendCephS3 is the object-store backend every bucket rides on (Ceph RGW S3); stamped on the
// bucket data so the client UI can label the storage.
const BackendCephS3 = "CEPH_S3"

// bucketData builds the DataBucket map for a bucket, stamped with its object-store backend.
func bucketData(name string, objectCount, bytes int64) map[string]any {
	return map[string]any{
		"bucketName":     name,
		"objectCount":    objectCount,
		"sizeInBytes":    bytes,
		"sizeInGb":       decimal.RequireFromString(bucketGiB(bytes)),
		"storageBackend": BackendCephS3,
	}
}

// CreateBucket creates a bucket. A fresh bucket has 0 objects / 0 bytes; externalId = the bucket
// name. Returns the DataBucket map.
func (c *Client) CreateBucket(ctx context.Context, o CreateBucketOpts) (map[string]any, error) {
	return c.ceph.createBucket(ctx, o)
}

// GetBucket fetches a bucket's metadata (object count + bytes used).
func (c *Client) GetBucket(ctx context.Context, name string) (map[string]any, error) {
	return c.ceph.getBucket(ctx, name)
}

// ListBuckets lists the project's buckets.
func (c *Client) ListBuckets(ctx context.Context) ([]map[string]any, error) {
	return c.ceph.listBuckets(ctx)
}

// ListBucketObjects lists a bucket's objects in the cache/FE display shape. `prefix` scopes to a
// folder (the FE's folderName); empty = the bucket root. Common prefixes become directory rows.
func (c *Client) ListBucketObjects(ctx context.Context, bucket, prefix string) ([]map[string]any, error) {
	return c.ceph.listBucketObjects(ctx, bucket, prefix)
}

// IsBucketPublic reports whether a bucket grants anonymous read.
func (c *Client) IsBucketPublic(ctx context.Context, bucket string) (bool, error) {
	return c.ceph.isBucketPublic(ctx, bucket)
}

// BucketAPIs returns the bucket's S3 access URLs.
func (c *Client) BucketAPIs(ctx context.Context, bucket string) ([]string, error) {
	return c.ceph.bucketAPIs(bucket), nil
}

// DeleteBucket removes a bucket (must be empty — RGW rejects a non-empty bucket delete).
func (c *Client) DeleteBucket(ctx context.Context, name string) error {
	return c.ceph.deleteBucket(ctx, name)
}

// CreateFolder creates a pseudo-folder marker (a 0-byte object named "<folderName>/").
func (c *Client) CreateFolder(ctx context.Context, bucket, folderName string) error {
	return c.ceph.createFolder(ctx, bucket, folderName)
}

// UploadBucketObject uploads (creates/replaces) an object. The reader is streamed;
// contentLength/contentType come from the upload request.
func (c *Client) UploadBucketObject(ctx context.Context, bucket, objectName, contentType string, contentLength int64, payload io.Reader) error {
	return c.ceph.uploadBucketObject(ctx, bucket, objectName, contentType, contentLength, payload)
}

// DeleteBucketObject deletes an object (or a folder + everything under it): every key under the
// prefix objectName goes, so a folder marker and its contents go together. No match = no-op success.
func (c *Client) DeleteBucketObject(ctx context.Context, bucket, objectName string) error {
	return c.ceph.deleteBucketObject(ctx, bucket, objectName)
}

// UpdateBucketObject updates an object's custom metadata.
func (c *Client) UpdateBucketObject(ctx context.Context, bucket, objectName string, metadata map[string]string) error {
	return c.ceph.updateBucketObject(ctx, bucket, objectName, metadata)
}

// UpdateBucketMetadata would update a bucket's custom metadata. RGW has no per-bucket custom
// metadata, so this is a no-op success.
// ponytail: map to S3 bucket tagging if the FE ever needs to read it back.
func (c *Client) UpdateBucketMetadata(ctx context.Context, bucket string, metadata map[string]string) error {
	return nil
}

// DownloadObject downloads an object's bytes + content-type. The whole object is loaded into memory
// (fine for the statement/object sizes this serves).
func (c *Client) DownloadObject(ctx context.Context, bucket, objectName string) ([]byte, string, error) {
	return c.ceph.downloadObject(ctx, bucket, objectName)
}

// SetBucketRead sets a bucket's read ACL (public ".r:*,.rlistings" / private ""). Empty acl removes
// public read.
func (c *Client) SetBucketRead(ctx context.Context, bucket, acl string) error {
	return c.ceph.setBucketRead(ctx, bucket, acl)
}
