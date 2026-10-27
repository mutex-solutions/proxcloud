package pve

import (
	"context"
	"net/url"
)

// StorageDef is one row of GET /storage — the cluster-level storage definition.
type StorageDef struct {
	Storage string `json:"storage"`
	Type    string `json:"type"`    // "zfspool" | "lvmthin" | "rbd" | "nfs" | "dir" | ...
	Content string `json:"content"` // comma-separated: "images,rootdir,iso,..."
	Nodes   string `json:"nodes"`   // comma-separated node restriction; "" = all nodes
	Shared  int    `json:"shared"`
	Disable int    `json:"disable"`
}

// ListStorage lists cluster storage definitions (validates admin storage-profile config).
func (c *Client) ListStorage(ctx context.Context) ([]StorageDef, error) {
	var out []StorageDef
	if err := c.get(ctx, "/storage", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// NodeStorage is one row of GET /nodes/{node}/storage — live status + capacity.
type NodeStorage struct {
	Storage string `json:"storage"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Active  int    `json:"active"`
	Enabled int    `json:"enabled"`
	Shared  int    `json:"shared"`
	Total   int64  `json:"total"`
	Used    int64  `json:"used"`
	Avail   int64  `json:"avail"`
}

// NodeStorages lists storage visible on one node (scheduler capacity input).
func (c *Client) NodeStorages(ctx context.Context, node string) ([]NodeStorage, error) {
	var out []NodeStorage
	if err := c.get(ctx, "/nodes/"+node+"/storage", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// StorageContent is one volume/ISO/template on a storage.
type StorageContent struct {
	VolID   string `json:"volid"` // "local-zfs:vm-101-disk-0"
	Content string `json:"content"`
	Format  string `json:"format"`
	Size    int64  `json:"size"`
	Used    int64  `json:"used"`
	VMID    int    `json:"vmid"`
}

// StorageContents lists a storage's content, optionally filtered by content type
// ("images", "iso", "vztmpl", ...) and owning vmid.
func (c *Client) StorageContents(ctx context.Context, node, storage, content string, vmid int) ([]StorageContent, error) {
	form := formVals{"content": content, "vmid": itoa(vmid)}.values()
	var out []StorageContent
	if err := c.get(ctx, "/nodes/"+node+"/storage/"+url.PathEscape(storage)+"/content", form, &out); err != nil {
		return nil, err
	}
	return out, nil
}
