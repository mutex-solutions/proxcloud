package admin

// promotionalcredit_repo.go — domain-specific Repo method for the promotional-credit
// update path. The generic crud.go helpers (FindDoc / InsertDoc / SetFields / DeleteDoc) and
// admin.go's ListRawFiltered cover the other endpoints; only the combined (id AND billingProfileId)
// lookup needs a bespoke method.

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// PromotionalCreditByIDAndBillingProfile loads a credit by (id, billingProfileId) — a document
// matches only when both `_id` AND `billingProfileId` equal the given values.
// Returns (nil,nil) when no document matches.
func (r *Repo) PromotionalCreditByIDAndBillingProfile(ctx context.Context, id, billingProfileID string) (pgdoc.M, error) {
	var doc pgdoc.M
	found, err := r.c(promotionalCreditColl).FindOne(ctx,
		pgdoc.M{"_id": id, "billingProfileId": billingProfileID}, &doc)
	if err != nil || !found {
		return nil, err
	}
	return doc, nil
}
