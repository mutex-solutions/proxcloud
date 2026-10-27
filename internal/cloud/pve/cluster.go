package pve

import (
	"context"
	"net/url"
	"strconv"
)

// ClusterResource is one row of GET /cluster/resources — the sync workhorse: a
// single call returns every VM, storage, node and pool in the cluster.
type ClusterResource struct {
	ID       string  `json:"id"`   // "qemu/101", "storage/pve1/local-zfs", "node/pve1", "/pool/st-abc"
	Type     string  `json:"type"` // "qemu" | "lxc" | "storage" | "node" | "sdn" | "pool"
	Node     string  `json:"node"`
	VMID     int     `json:"vmid"`
	Name     string  `json:"name"`
	Status   string  `json:"status"` // qemu: "running"|"stopped"; node: "online"; storage: "available"
	Pool     string  `json:"pool"`
	Template int     `json:"template"` // 1 when the qemu row is a template
	Tags     string  `json:"tags"`     // semicolon-separated
	Lock     string  `json:"lock"`
	MaxCPU   float64 `json:"maxcpu"`
	MaxMem   int64   `json:"maxmem"`
	MaxDisk  int64   `json:"maxdisk"`
	CPU      float64 `json:"cpu"`
	Mem      int64   `json:"mem"`
	Disk     int64   `json:"disk"`
	Uptime   int64   `json:"uptime"`
	Storage  string  `json:"storage"` // storage rows: the storage id
	Shared   int     `json:"shared"`  // storage rows
	Content  string  `json:"content"` // storage rows: comma-separated content types
}

// ClusterResources lists cluster-wide resources. kind filters server-side:
// "vm" | "storage" | "node" | "sdn" (empty = all).
func (c *Client) ClusterResources(ctx context.Context, kind string) ([]ClusterResource, error) {
	var form url.Values
	if kind != "" {
		form = formVals{"type": kind}.values()
	}
	var out []ClusterResource
	if err := c.get(ctx, "/cluster/resources", form, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Node is one row of GET /nodes.
type Node struct {
	Node    string  `json:"node"`
	Status  string  `json:"status"` // "online" | "offline" | "unknown"
	MaxCPU  int     `json:"maxcpu"`
	MaxMem  int64   `json:"maxmem"`
	CPU     float64 `json:"cpu"`
	Mem     int64   `json:"mem"`
	Uptime  int64   `json:"uptime"`
	Level   string  `json:"level"`
	SSLFing string  `json:"ssl_fingerprint"`
}

// Nodes lists cluster nodes.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var out []Node
	if err := c.get(ctx, "/nodes", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// NextID reserves nothing — it just returns the lowest free VMID. Racy by nature;
// creation retries on "already exists" with a fresh id.
func (c *Client) NextID(ctx context.Context) (int, error) {
	// PVE returns the id as a JSON string.
	var s string
	if err := c.get(ctx, "/cluster/nextid", nil, &s); err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}
