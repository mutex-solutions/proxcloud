package client

import (
	"context"
	"errors"
	"io"

	"github.com/shopspring/decimal"
)

// objectstore.go = what every object-store backend implements. A bucket's externalId == the bucket name.
// Bucket data = {bucketName, objectCount, sizeInGb, sizeInBytes, quotaBytes?, storageBackend, usageUnknown?}.

// objectBackend is the bucket surface. cephBackend talks to RGW; rustfsBackend reuses its plain-S3 data plane
// and replaces everything RGW does through Admin Ops or bucket ownership (users, quota, usage, ownership).
type objectBackend interface {
	Caps() ObjectCaps

	// Tenant identity.
	EnsureProjectUser(ctx context.Context, displayName string) (access, secret string, err error)
	PurgeProjectUser(ctx context.Context) error
	ChildUID(name string) string
	CreateChildKey(ctx context.Context, uid, displayName string) (access, secret string, err error)
	DeleteChildKey(ctx context.Context, uid string) error
	RotateKey(ctx context.Context, uid, oldAccessKey string) (access, secret string, err error)
	Endpoints() (s3Endpoint, websiteEndpoint, region string)

	// Buckets.
	CreateBucket(ctx context.Context, o CreateBucketOpts) (map[string]any, error)
	GetBucket(ctx context.Context, name string) (map[string]any, error)
	ListBuckets(ctx context.Context) ([]map[string]any, error)
	DeleteBucket(ctx context.Context, name string) error
	ForceDeleteBucket(ctx context.Context, bucket string) error
	BucketAPIs(ctx context.Context, bucket string) ([]string, error)
	UpdateBucketMetadata(ctx context.Context, bucket string, metadata map[string]string) error

	// Objects.
	ListBucketObjects(ctx context.Context, bucket, prefix string) ([]map[string]any, error)
	CreateFolder(ctx context.Context, bucket, folderName string) error
	UploadBucketObject(ctx context.Context, bucket, objectName, contentType string, contentLength int64, payload io.Reader) error
	DeleteBucketObject(ctx context.Context, bucket, objectName string) error
	UpdateBucketObject(ctx context.Context, bucket, objectName string, metadata map[string]string) error
	DownloadObject(ctx context.Context, bucket, objectName string) ([]byte, string, error)

	// Configuration.
	GetBucketSettings(ctx context.Context, bucket string) (*BucketSettings, error)
	SetBucketVersioning(ctx context.Context, bucket string, enabled bool) error
	SetObjectLockDefaults(ctx context.Context, bucket, mode string, days int32) error
	SetBucketQuota(ctx context.Context, bucket string, maxSizeBytes, maxObjects int64, enabled bool) error
	SetBucketLifecycle(ctx context.Context, bucket string, rules []LifecycleRule) error
	SetBucketCORS(ctx context.Context, bucket string, rules []CORSRule) error
	SetBucketTags(ctx context.Context, bucket string, tags map[string]string) error

	// Access.
	IsBucketPublic(ctx context.Context, bucket string) (bool, error)
	SetBucketRead(ctx context.Context, bucket, acl string) error
	GrantBucketAccess(ctx context.Context, bucket, uid string, p BucketPermission) error
	RevokeBucketAccess(ctx context.Context, bucket, uid string) error
	ListBucketGrants(ctx context.Context, bucket string) ([]BucketGrant, error)
	GetBucketPolicyJSON(ctx context.Context, bucket string) (string, error)
	SetBucketPolicyJSON(ctx context.Context, bucket, policyJSON string) error

	// Static website.
	GetBucketWebsite(ctx context.Context, bucket string) (*BucketWebsite, error)
	EnableBucketWebsite(ctx context.Context, bucket, indexDoc, errorDoc string) (*BucketWebsite, error)
	DisableBucketWebsite(ctx context.Context, bucket string) error
}

// ObjectCaps says what the backend offers, so the UI hides the rest. The API enforces it too: a backend
// returns ErrBucketFeatureUnsupported for anything marked false here.
type ObjectCaps struct {
	Backend      string `json:"backend"`
	Website      bool   `json:"website"`
	QuotaObjects bool   `json:"quotaObjects"` // a bucket quota may cap the object count, not only bytes
}

// bucketGiB converts bytes → GiB: bytes / 1073741824, 2dp HALF_UP.
func bucketGiB(bytes int64) string {
	return decimal.NewFromInt(bytes).DivRound(decimal.NewFromInt(1073741824), 2).String()
}

// ErrBucketFeatureUnsupported is returned when the provider's object-store backend does not offer the
// requested bucket feature (see ObjectCaps).
var ErrBucketFeatureUnsupported = errors.New("cloud: this object storage backend does not offer this bucket feature")

// ErrWebsiteUnsupported is the same sentinel, kept for the website call sites' readability.
var ErrWebsiteUnsupported = ErrBucketFeatureUnsupported

// ErrProjectQuotaExceeded is returned when a bucket or bucket quota would exceed the project's object-store
// quota, on backends where ProxCloud has to enforce it (RustFS has no per-user quota).
var ErrProjectQuotaExceeded = errors.New("cloud: project object storage quota exceeded")

// ErrBucketNotEmpty is S3's refusal to delete a bucket that still holds objects.
var ErrBucketNotEmpty = errors.New("cloud: the bucket is not empty; delete its objects first")

// ErrObjectStoreBusy is RustFS answering 503 on its quota API for too long: its scanner has not caught up.
var ErrObjectStoreBusy = errors.New("cloud: object storage is busy, please try again")

// BucketWebsite is a bucket's static-website state. PublicObjects records the security-relevant fact that
// an enabled website makes every object in the bucket anonymously readable (via a bucket policy).
type BucketWebsite struct {
	Enabled       bool   `json:"enabled"`
	IndexDocument string `json:"indexDocument,omitempty"`
	ErrorDocument string `json:"errorDocument,omitempty"`
	URL           string `json:"url,omitempty"`
	PublicObjects bool   `json:"publicObjects"`
}

// Storage backends stamped on the bucket data so the client UI can label the storage.
const (
	BackendCephS3 = "CEPH_S3"
	BackendRustFS = "RUSTFS"
)

// KeepLastUsage is how scanner-based usage reaches the cache: when fresh says usageUnknown, it keeps the old
// figures. A delayed or failed scan must never rate a bucket at 0.
func KeepLastUsage(old, fresh map[string]any) {
	if unknown, _ := fresh["usageUnknown"].(bool); !unknown {
		return
	}
	delete(fresh, "usageUnknown")
	for _, k := range []string{"objectCount", "sizeInBytes", "sizeInGb"} {
		if v, ok := old[k]; ok {
			fresh[k] = v
		}
	}
	if _, has := fresh["quotaBytes"]; !has {
		if v, ok := old["quotaBytes"]; ok {
			fresh["quotaBytes"] = v
		}
	}
}

// bucketData builds the DataBucket map for a bucket, stamped with its object-store backend.
func bucketData(backend, name string, objectCount, bytes int64) map[string]any {
	return map[string]any{
		"bucketName":     name,
		"objectCount":    objectCount,
		"sizeInBytes":    bytes,
		"sizeInGb":       decimal.RequireFromString(bucketGiB(bytes)),
		"storageBackend": backend,
	}
}
