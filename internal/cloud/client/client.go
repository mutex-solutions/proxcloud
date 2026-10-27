// Package client is the ceph-s3 (RGW) object-store client — the surviving slice
// of the old OpenStack CloudClient facade. The Keystone/compute/network surface
// was removed with the OpenStack backend (this fork drives Proxmox VE through
// internal/cloud/pve instead); buckets stayed because they are backend-agnostic:
// a homelab with a Ceph RGW attaches a ceph-s3 provider unchanged.
//
// The *Client shape survives so the bucket write/sync/billing plumbing
// (providers.WriteService, BucketProvider, the platform bucket-settings surface)
// did not have to move; NewCephS3 is the only constructor.
package client

import (
	"errors"

	"github.com/gophercloud/gophercloud/v2"
)

// Client is the facade over one ceph-s3 project scope.
type Client struct {
	// provider is a STUB whose EndpointLocator always errors — the swift legs of
	// objectstore.go funnel through it and fail with ErrNotOpenStack instead of
	// panicking. ponytail: swift legs are dead code behind the ceph dispatch;
	// delete objectstore.go's swift halves (and this stub) in a polish pass.
	provider  *gophercloud.ProviderClient
	region    string
	projectID string
	// ceph is the Ceph RGW (S3) backend. Always non-nil on a built client.
	ceph *cephBackend
}

// IsCephS3 reports whether this client fronts a Ceph RGW backend.
func (c *Client) IsCephS3() bool { return c.ceph != nil }

// ErrNotOpenStack marks a call that needs an OpenStack surface this fork no
// longer has (the swift legs of objectstore.go; a few platform guards test for it).
var ErrNotOpenStack = errors.New("cloud: not an OpenStack-backed provider")

func (c *Client) endpointOpts() gophercloud.EndpointOpts {
	return gophercloud.EndpointOpts{Region: c.region}
}
