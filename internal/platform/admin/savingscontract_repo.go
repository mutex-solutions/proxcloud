package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

// savingscontract_repo.go holds the savings-contract-specific repo methods (the
// generic crud.go helpers cover the plain store/replace/delete; these add the billing-profile
// existence check, the available-savings-plan lookup decoded through the typed billing domain so
// the decimal tier compares are faithful, the active-duplicate check, and the two list reads
// incl. the billingProfile join).

// savingsContractFindBillingProfile loads a billing profile by id
// (the create flow only needs the profile's existence + id, both already the path param). Returns
// the raw doc (nil when absent → 404 at the handler).
func (r *Repo) savingsContractFindBillingProfile(ctx context.Context, id string) (pgdoc.M, error) {
	return r.FindDoc(ctx, "billingProfile", id)
}

// savingsContractAvailablePlan loads a savings plan only when it's marked available.
// Returns nil when the plan is absent OR not available (the query filters on available==true, so a
// non-available plan is "not found"). Ids are plain strings, so a single {_id, available} filter
// covers it.
func (r *Repo) savingsContractAvailablePlan(ctx context.Context, id string) (*billing.SavingsPlan, error) {
	if id == "" {
		return nil, nil
	}
	var plan billing.SavingsPlan
	found, err := r.c("savingsPlan").FindOne(ctx, pgdoc.M{"_id": id, "available": true}, &plan)
	if err != nil || !found {
		return nil, err
	}
	return &plan, nil
}

// savingsContractActiveExists reports whether a contract, ACTIVE or awaiting its payment, already exists
// for the given (savingsPlanId, billingProfileId) pair: both of them would stack their discounts.
func (r *Repo) savingsContractActiveExists(ctx context.Context, savingsPlanID, billingProfileID string) (bool, error) {
	return r.c(savingsContractCollection).Exists(ctx, pgdoc.M{
		"savingsPlanId":    savingsPlanID,
		"billingProfileId": billingProfileID,
		"status":           pgdoc.M{"$in": []string{billing.SavingsStatusActive, billing.SavingsStatusPendingPayment}},
	})
}

// savingsContractsByBillingProfile loads the contracts for a billing profile: the raw
// SavingsContract docs (still carrying `_id`; the handler shapeDoc's each). Never nil.
func (r *Repo) savingsContractsByBillingProfile(ctx context.Context, billingProfileID string) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := r.c(savingsContractCollection).Find(ctx, pgdoc.M{"billingProfileId": billingProfileID}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// savingsContractsBySavingsPlanWithBillingProfile returns the contracts for a savings plan joined to
// their billing profile: it matches a savingsPlanId, embeds the billingProfile
// (savingsContract.billingProfileId → billingProfile id column), and sorts newest first.
// Returns raw docs (each carrying `_id` + the embedded `billingProfile`; a contract with no matching
// profile keeps NO billingProfile field); the handler shapeDoc's the top-level doc.
func (r *Repo) savingsContractsBySavingsPlanWithBillingProfile(ctx context.Context, savingsPlanID string) ([]pgdoc.M, error) {
	// Raw SQL bypasses the Store's implicit table auto-create; on a fresh DB either
	// table may not exist yet (42P01), so Ensure both first (cheap, idempotent).
	if err := r.db.C(savingsContractCollection).Ensure(ctx); err != nil {
		return nil, err
	}
	if err := r.db.C("billingProfile").Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT c.id, c.doc, b.id, b.doc
		FROM "savingsContract" c
		LEFT JOIN "billingProfile" b ON b.id = c.doc->>'billingProfileId'
		WHERE c.doc->>'savingsPlanId' = $1
		ORDER BY (c.doc->>'createdAt')::timestamptz DESC`, savingsPlanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []pgdoc.M{}
	for rows.Next() {
		var cID string
		var cDoc []byte
		var bID *string
		var bDoc []byte
		if err := rows.Scan(&cID, &cDoc, &bID, &bDoc); err != nil {
			return nil, err
		}
		var contract pgdoc.M
		if err := pgdoc.Unmarshal(cDoc, cID, &contract); err != nil {
			return nil, err
		}
		if bID != nil && len(bDoc) > 0 {
			var profile pgdoc.M
			if err := pgdoc.Unmarshal(bDoc, *bID, &profile); err != nil {
				return nil, err
			}
			contract["billingProfile"] = profile
		}
		out = append(out, contract)
	}
	return out, rows.Err()
}
