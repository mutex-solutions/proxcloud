// Package client is the ceph-s3 (RGW) object-store client. Proxmox VE is driven through
// internal/cloud/pve; buckets are backend-agnostic, so a homelab with a Ceph RGW attaches a
// ceph-s3 provider and gets the bucket surface from here.
//
// The *Client facade keeps the bucket write/sync/billing plumbing (providers.WriteService,
// BucketProvider, the platform bucket-settings surface) on one type; NewCephS3 is the only
// constructor.
package client

// Client is the facade over one ceph-s3 project scope.
type Client struct {
	// ceph is the Ceph RGW (S3) backend. Always non-nil on a built client.
	ceph *cephBackend
}
