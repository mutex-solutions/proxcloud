package admin

// instancemetadata_repo.go holds the one InstanceMetadataOption-specific repo method the generic
// crud.go helpers do not cover: the active-key-uniqueness check.

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// InstanceMetadataKeyEnabledExists runs the existence query behind the key-uniqueness check:
//   - excludeID == "" → any enabled doc with this exact key.
//   - excludeID != "" → an enabled doc with this key whose _id != excludeID.
//
// The key match is exact (case-sensitive); ids are plain strings.
func (r *Repo) InstanceMetadataKeyEnabledExists(ctx context.Context, key, excludeID string) (bool, error) {
	filter := pgdoc.M{"key": key, "enabled": true}
	if excludeID != "" {
		filter["_id"] = pgdoc.M{"$ne": excludeID}
	}
	return r.c(instanceMetadataCollection).Exists(ctx, filter)
}
