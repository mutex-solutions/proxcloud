package pve

import "context"

// UploadSpace is the room pveproxy has on the API node's root disk for a spooled upload
// (exported for internal/templateupdates, which streams published VM artifacts to the import store).
func (c *Client) UploadSpace(ctx context.Context) (StorageNodeState, error) {
	return c.uploadSpace(ctx)
}

// LockImportUpload takes the provider-wide upload lock customer image imports hold
// ("pve-image-upload-<service>"), so a template update and a customer import never spool to the API
// node's disk at once. Nil HolderLock = no lock (tools/tests), as for ImportImage.
func (s *Service) LockImportUpload(ctx context.Context) (func(), error) {
	return s.lockImportUpload(ctx)
}
