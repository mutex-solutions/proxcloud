// Package client is the tenant object-store client: Ceph RGW on Full-tier sites, RustFS on Standard-tier
// sites. Proxmox VE is driven through internal/cloud/pve; buckets are a separate provider a project attaches.
//
// *Client keeps one type for the bucket write/sync/billing plumbing (providers.WriteService, BucketProvider,
// the platform bucket-settings surface); NewCephS3 and NewRustFS pick the backend behind it.
package client

// Client is the facade over one project's scope on one object store.
type Client struct {
	objectBackend
}
