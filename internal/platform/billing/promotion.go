package billing

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// promotion.go = the client promo-code REDEEM support:
// look up a code, check it has not already been redeemed by the org, mint a PromotionalCredit, and
// record the redemption. The admin create/update/delete of promotionCode lives in the admin package.

// FindPromotionCodeByCode finds a promotionCode by its `code`, CASE-INSENSITIVELY and trimmed
// (a differently-cased or padded code must still resolve, not spuriously 404). The match is
// anchored full-string (`^code$`, metachars escaped) so a code containing regex specials can't
// widen the match. Raw doc (the code carries dynamic validity/target fields); (nil,nil) when none.
func (r *Repo) FindPromotionCodeByCode(ctx context.Context, code string) (pgdoc.M, error) {
	pattern := "^" + regexp.QuoteMeta(strings.TrimSpace(code)) + "$"
	var doc pgdoc.M
	found, err := r.promoCodes.FindOne(ctx, pgdoc.M{"code": pgdoc.M{"$regex": pattern, "$options": "i"}}, &doc)
	if err != nil || !found {
		return nil, err
	}
	return doc, nil
}

// PromotionRedemptionExists reports whether the org has already redeemed the given promotion code.
func (r *Repo) PromotionRedemptionExists(ctx context.Context, promotionCodeID, organizationID string) (bool, error) {
	return r.promoRedeem.Exists(ctx, pgdoc.M{
		"promotionCodeId": promotionCodeID, "organizationId": organizationID,
	})
}

// SavePromotionRedemption records a PromotionCodeRedemption (dedup key promotionCodeId+organizationId).
func (r *Repo) SavePromotionRedemption(ctx context.Context, promotionCodeID, organizationID, billingProfileID, source string, now time.Time) error {
	_, err := r.promoRedeem.InsertOne(ctx, pgdoc.M{
		"promotionCodeId":  promotionCodeID,
		"organizationId":   organizationID,
		"billingProfileId": billingProfileID,
		"source":           source,
		"createdAt":        now,
	})
	return err
}

// InsertPromotionalCredit inserts a freshly-minted PromotionalCredit (the store assigns _id) and
// returns it with the generated id.
func (r *Repo) InsertPromotionalCredit(ctx context.Context, pc *pricing.PromotionalCredit) (*pricing.PromotionalCredit, error) {
	pc.ID = ""
	id, err := r.promoCredit.InsertOne(ctx, pc)
	if err != nil {
		return nil, err
	}
	pc.ID = id
	return pc, nil
}
