package client

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/objectstorage/v1/containers"
	"github.com/gophercloud/gophercloud/v2/openstack/objectstorage/v1/objects"
	"github.com/shopspring/decimal"
)

// objectstore.go = the Swift (object-store) bucket read/write surface on the CloudClient facade. A bucket
// is a Swift container; its externalId == the bucket name.
// data = DataBucket{bucketName, objectCount, sizeInGb, sizeInBytes}.

func (c *Client) objectStore() (*gophercloud.ServiceClient, error) {
	return openstack.NewObjectStorageV1(c.provider, c.endpointOpts())
}

// bucketGiB converts bytes → GiB: bytes / 1073741824, 2dp HALF_UP.
func bucketGiB(bytes int64) string {
	return decimal.NewFromInt(bytes).DivRound(decimal.NewFromInt(1073741824), 2).String()
}

// ErrBucketFeatureUnsupported is returned when an S3-only bucket feature (website, versioning, object
// lock, quota, lifecycle, CORS, tagging, policy, per-key grants) is requested on a Swift bucket. Swift has
// no equivalent for any of them, so the whole surface exists only on the ceph-s3 backend.
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

// Object-store backends a BUCKET can ride on. Swift (OpenStack) and Ceph RGW S3 run SIDE BY SIDE
// permanently — they are two disjoint bucket namespaces, never migrated between. Every bucket carries
// its backend so the client UI can label which storage the bucket lives on.
const (
	BackendSwift  = "SWIFT"
	BackendCephS3 = "CEPH_S3"
)

// bucketData builds the DataBucket map for a container, stamped with its object-store backend.
func bucketData(backend, name string, objectCount, bytes int64) map[string]any {
	return map[string]any{
		"bucketName":     name,
		"objectCount":    objectCount,
		"sizeInBytes":    bytes,
		"sizeInGb":       decimal.RequireFromString(bucketGiB(bytes)),
		"storageBackend": backend,
	}
}

// CreateBucket creates a Swift container. A fresh bucket has 0
// objects / 0 bytes; externalId = the bucket name. Returns the DataBucket map.
func (c *Client) CreateBucket(ctx context.Context, o CreateBucketOpts) (map[string]any, error) {
	if c.ceph != nil {
		return c.ceph.createBucket(ctx, o)
	}
	if o.ObjectLockEnabled {
		return nil, ErrBucketFeatureUnsupported // Swift has no object lock
	}
	oc, err := c.objectStore()
	if err != nil {
		return nil, err
	}
	if _, err := containers.Create(ctx, oc, o.Name, containers.CreateOpts{}).Extract(); err != nil {
		return nil, err
	}
	return bucketData(BackendSwift, o.Name, 0, 0), nil
}

// GetBucket fetches a Swift container's metadata (object count + bytes used).
func (c *Client) GetBucket(ctx context.Context, name string) (map[string]any, error) {
	if c.ceph != nil {
		return c.ceph.getBucket(ctx, name)
	}
	oc, err := c.objectStore()
	if err != nil {
		return nil, err
	}
	h, err := containers.Get(ctx, oc, name, containers.GetOpts{}).Extract()
	if err != nil {
		return nil, err
	}
	return bucketData(BackendSwift, name, h.ObjectCount, h.BytesUsed), nil
}

// ListBuckets lists the project's Swift containers.
func (c *Client) ListBuckets(ctx context.Context) ([]map[string]any, error) {
	if c.ceph != nil {
		return c.ceph.listBuckets(ctx)
	}
	oc, err := c.objectStore()
	if err != nil {
		return nil, err
	}
	pages, err := containers.List(oc, containers.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := containers.ExtractInfo(pages)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(cs))
	for i := range cs {
		out = append(out, bucketData(BackendSwift, cs[i].Name, cs[i].Count, cs[i].Bytes))
	}
	return out, nil
}

// ListBucketObjects lists a Swift container's objects in the cache/FE display shape. `prefix` scopes
// to a folder
// (the FE's folderName); empty = the container root. Subdir entries are folded into directory rows.
func (c *Client) ListBucketObjects(ctx context.Context, container, prefix string) ([]map[string]any, error) {
	if c.ceph != nil {
		return c.ceph.listBucketObjects(ctx, container, prefix)
	}
	oc, err := c.objectStore()
	if err != nil {
		return nil, err
	}
	opts := objects.ListOpts{Delimiter: "/"}
	if prefix != "" {
		opts.Prefix = strings.TrimSuffix(prefix, "/") + "/"
	}
	pages, err := objects.List(oc, container, opts).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	os, err := objects.ExtractInfo(pages)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for i := range os {
		o := &os[i]
		if o.Subdir != "" { // a pseudo-folder
			name := strings.TrimSuffix(o.Subdir, "/")
			display := name
			if prefix != "" {
				display = strings.TrimPrefix(strings.TrimSuffix(name, "/"), strings.TrimSuffix(prefix, "/")+"/")
			}
			out = append(out, map[string]any{
				"name": o.Subdir, "bucketName": container, "displayName": display,
				"directoryName": prefix, "directory": true, "sizeInBytes": 0, "mimeType": "", "lastModified": "",
			})
			continue
		}
		display := o.Name
		if prefix != "" {
			display = strings.TrimPrefix(o.Name, strings.TrimSuffix(prefix, "/")+"/")
		}
		out = append(out, map[string]any{
			"name": o.Name, "bucketName": container, "displayName": display, "directoryName": prefix,
			"directory": false, "sizeInBytes": o.Bytes, "mimeType": o.ContentType,
			"lastModified": o.LastModified.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return out, nil
}

// IsBucketPublic reports whether a Swift container's read ACL grants public read (`.r:*`).
func (c *Client) IsBucketPublic(ctx context.Context, container string) (bool, error) {
	if c.ceph != nil {
		return c.ceph.isBucketPublic(ctx, container)
	}
	oc, err := c.objectStore()
	if err != nil {
		return false, err
	}
	h, err := containers.Get(ctx, oc, container, containers.GetOpts{}).Extract()
	if err != nil {
		return false, err
	}
	for _, acl := range h.Read {
		if strings.Contains(acl, ".r:*") {
			return true, nil
		}
	}
	return false, nil
}

// BucketAPIs returns the container's Swift + S3 access URLs. The Swift URL is the object-store
// endpoint + container; the S3 URL is omitted
// (the swift→s3 endpoint transform is provider-specific and not modeled here).
func (c *Client) BucketAPIs(ctx context.Context, container string) (swift, s3 []string, err error) {
	if c.ceph != nil {
		sw, s3urls := c.ceph.bucketAPIs(container)
		return sw, s3urls, nil
	}
	oc, err := c.objectStore()
	if err != nil {
		return nil, nil, err
	}
	base := strings.TrimSuffix(oc.Endpoint, "/")
	return []string{base + "/" + container}, []string{}, nil
}

// DeleteBucket removes a Swift container (must be empty — Swift rejects a non-empty container delete).
func (c *Client) DeleteBucket(ctx context.Context, name string) error {
	if c.ceph != nil {
		return c.ceph.deleteBucket(ctx, name)
	}
	oc, err := c.objectStore()
	if err != nil {
		return err
	}
	_, err = containers.Delete(ctx, oc, name).Extract()
	return err
}

// swiftEncodeObjectName URL-encodes each "/"-separated path segment of a Swift object name
// (split on "/", raw-path-encode each segment, rejoin) so names with
// spaces/special chars produce a valid object URL while keeping "/" as the pseudo-folder separator.
func swiftEncodeObjectName(name string) string {
	if name == "" {
		return name
	}
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// CreateFolder creates a Swift pseudo-folder marker
// (a 0-byte object named "<folderName>/" with type application/directory).
func (c *Client) CreateFolder(ctx context.Context, container, folderName string) error {
	if c.ceph != nil {
		return c.ceph.createFolder(ctx, container, folderName)
	}
	oc, err := c.objectStore()
	if err != nil {
		return err
	}
	name := strings.TrimSuffix(folderName, "/") + "/"
	res := objects.Create(ctx, oc, container, swiftEncodeObjectName(name), objects.CreateOpts{
		Content: strings.NewReader(""), ContentType: "application/directory", ContentLength: 0,
	})
	return res.Err
}

// UploadBucketObject uploads (creates/replaces) an object into a Swift container
// (objects.put with the raw payload + content-type). The reader is
// streamed; contentLength/contentType come from the upload request.
func (c *Client) UploadBucketObject(ctx context.Context, container, objectName, contentType string, contentLength int64, payload io.Reader) error {
	if c.ceph != nil {
		return c.ceph.uploadBucketObject(ctx, container, objectName, contentType, contentLength, payload)
	}
	oc, err := c.objectStore()
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	res := objects.Create(ctx, oc, container, swiftEncodeObjectName(objectName), objects.CreateOpts{
		Content: payload, ContentType: contentType, ContentLength: contentLength,
	})
	return res.Err
}

// DeleteBucketObject deletes an object (or a folder + everything under it):
// lists by prefix(objectName) and deletes each match, so a
// folder marker and its contents go together. No match = no-op success.
func (c *Client) DeleteBucketObject(ctx context.Context, container, objectName string) error {
	if c.ceph != nil {
		return c.ceph.deleteBucketObject(ctx, container, objectName)
	}
	oc, err := c.objectStore()
	if err != nil {
		return err
	}
	pages, err := objects.List(oc, container, objects.ListOpts{Prefix: objectName}).AllPages(ctx)
	if err != nil {
		return err
	}
	names, err := objects.ExtractNames(pages)
	if err != nil {
		return err
	}
	for _, n := range names {
		if e := objects.Delete(ctx, oc, container, swiftEncodeObjectName(n), nil).Err; e != nil {
			return e
		}
	}
	return nil
}

// UpdateBucketObject updates an object's custom metadata
// (objects.updateMetadata, X-Object-Meta-* headers).
func (c *Client) UpdateBucketObject(ctx context.Context, container, objectName string, metadata map[string]string) error {
	if c.ceph != nil {
		return c.ceph.updateBucketObject(ctx, container, objectName, metadata)
	}
	oc, err := c.objectStore()
	if err != nil {
		return err
	}
	res := objects.Update(ctx, oc, container, swiftEncodeObjectName(objectName), objects.UpdateOpts{Metadata: metadata})
	return res.Err
}

// UpdateBucketMetadata updates a container's custom metadata
// (containers.updateMetadata, X-Container-Meta-* headers).
func (c *Client) UpdateBucketMetadata(ctx context.Context, container string, metadata map[string]string) error {
	if c.ceph != nil {
		// ceph: RGW has no per-bucket custom metadata like Swift's X-Container-Meta-*; no-op success.
		// ponytail: map to S3 bucket tagging if the FE ever needs to read it back.
		return nil
	}
	oc, err := c.objectStore()
	if err != nil {
		return err
	}
	res := containers.Update(ctx, oc, container, containers.UpdateOpts{Metadata: metadata})
	return res.Err
}

// DownloadObject downloads a Swift object's bytes + content-type (swift get-object). The whole object
// is loaded into memory (fine for the statement/object sizes
// this serves).
func (c *Client) DownloadObject(ctx context.Context, container, objectName string) ([]byte, string, error) {
	if c.ceph != nil {
		return c.ceph.downloadObject(ctx, container, objectName)
	}
	oc, err := c.objectStore()
	if err != nil {
		return nil, "", err
	}
	res := objects.Download(ctx, oc, container, swiftEncodeObjectName(objectName), nil)
	data, err := res.ExtractContent()
	if err != nil {
		return nil, "", err
	}
	ct := "application/octet-stream"
	if h, herr := res.Extract(); herr == nil && h.ContentType != "" {
		ct = h.ContentType
	}
	return data, ct, nil
}

// SetBucketRead sets a container's read ACL (public ".r:*,.rlistings" / private "").
// Empty acl removes public read.
func (c *Client) SetBucketRead(ctx context.Context, container, acl string) error {
	if c.ceph != nil {
		return c.ceph.setBucketRead(ctx, container, acl)
	}
	oc, err := c.objectStore()
	if err != nil {
		return err
	}
	res := containers.Update(ctx, oc, container, containers.UpdateOpts{ContainerRead: &acl})
	return res.Err
}
