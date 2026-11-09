package billing

import (
	"context"
	"errors"
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

// ErrPromotionRedeemed: the organization has already redeemed this promotion code.
var ErrPromotionRedeemed = errors.New("promotion code already redeemed by the organization")

// RedeemPromotion records the organization's redemption of a promotion code (dedup key promotionCodeId+organizationId)
// and mints its credit, in one transaction under a lock on that key. The record goes first and guards the credit: a
// failed write leaves neither, so a retry redeems once, and two redeems at once give one credit (the second gets
// ErrPromotionRedeemed).
func (r *Repo) RedeemPromotion(ctx context.Context, credit *pricing.PromotionalCredit, promotionCodeID, organizationID, source string, now time.Time) (*pricing.PromotionalCredit, error) {
	// An undefined table aborts the transaction: a fresh install may not have these yet.
	for _, t := range []*pgdoc.Store{r.promoRedeem, r.promoCredit} {
		if err := t.Ensure(ctx); err != nil {
			return nil, err
		}
	}
	db := r.promoRedeem.DB()
	var saved *pricing.PromotionalCredit
	err := db.WithTx(ctx, func(ctx context.Context) error {
		if err := db.LockXact(ctx, "promotionRedemption:"+promotionCodeID+":"+organizationID); err != nil {
			return err
		}
		key := pgdoc.M{"promotionCodeId": promotionCodeID, "organizationId": organizationID}
		if exists, err := r.promoRedeem.Exists(ctx, key); err != nil {
			return err
		} else if exists {
			return ErrPromotionRedeemed
		}
		if _, err := r.promoRedeem.InsertOne(ctx, pgdoc.M{
			"promotionCodeId":  promotionCodeID,
			"organizationId":   organizationID,
			"billingProfileId": credit.BillingProfileID,
			"source":           source,
			"createdAt":        now,
		}); err != nil {
			return err
		}
		var err error
		saved, err = r.InsertPromotionalCredit(ctx, credit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
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
