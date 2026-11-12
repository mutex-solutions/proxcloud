package pve

import "context"

// UploadSpace is the room pveproxy has on the API node's root disk for a spooled upload
// (exported for internal/templateupdates, which streams published VM artifacts to the import store).
func (c *Client) UploadSpace(ctx context.Context) (StorageNodeState, error) {
	return c.uploadSpace(ctx)
}
