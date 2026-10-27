package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// externalservicemut_repo.go holds the domain repo method backing the ExternalService DELETE in-use
// guards. The mutations themselves reuse the crud.go helpers (FindDoc / ReplaceDoc / DeleteDoc); the
// only thing crud.go lacks is the existence probe the three in-use checks run before a delete.

// externalServiceInUse runs the three in-use checks before deleting an
// external service, all of the same shape:
//
//	project doc where some services[] element has serviceId == id
//	user doc where some services[] element has serviceId == id
//	cloudResource doc where the top-level serviceId == id
//
// The cloudResource check keys off the top-level `serviceId` field, while the
// project/user checks key off the embedded `services` array (some element's serviceId — array
// containment). The collection name selects which filter to apply.
func (r *Repo) externalServiceInUse(ctx context.Context, collection, externalServiceID string) (bool, error) {
	filter := pgdoc.M{"services": pgdoc.M{"$contains": pgdoc.M{"serviceId": externalServiceID}}}
	if collection == "cloudResource" {
		filter = pgdoc.M{"serviceId": externalServiceID}
	}
	return r.c(collection).Exists(ctx, filter)
}
