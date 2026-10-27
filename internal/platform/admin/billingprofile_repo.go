package admin

// billingprofile_repo.go holds the BillingProfile-admin domain queries the crud.go helpers do not
// cover: the delete-time "is the billing profile in use?" existence checks and the reseller-disable
// guard. Names are domain-prefixed; all use r.db directly.

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// ExistsByBillingProfileID reports whether any document in `collection` references `billingProfileId`
// (a top-level `billingProfileId` field). Backs the DELETE in-use guards:
//   - bill        → is the profile referenced by any bill?
//   - project     → is the profile referenced by any project?
//   - creditCard  → is the profile referenced by any credit card?
//
// billingProfileId is stored as the hex string on these collections (it is the BillingProfile's String
// id), so the filter matches the raw string — no id coercion.
func (r *Repo) ExistsByBillingProfileID(ctx context.Context, collection, billingProfileID string) (bool, error) {
	return r.c(collection).Exists(ctx, pgdoc.M{"billingProfileId": billingProfileID})
}

// ExistsExternalServiceByReseller reports whether an externalService uses the profile as reseller:
// exists an externalService where config.openstackReseller.enabled == true AND
// config.openstackReseller.billingProfileId == billingProfileId. Backs the reseller-disable guard.
func (r *Repo) ExistsExternalServiceByReseller(ctx context.Context, billingProfileID string) (bool, error) {
	filter := pgdoc.M{
		"config.openstackReseller.enabled":          true,
		"config.openstackReseller.billingProfileId": billingProfileID,
	}
	return r.c("externalService").Exists(ctx, filter)
}
