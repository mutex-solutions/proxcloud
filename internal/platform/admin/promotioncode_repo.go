package admin

// promotioncode_repo.go holds the PromotionCode-specific repo method the generic crud.go helpers do
// not cover: a case-insensitive check for whether a code already exists.

import (
	"context"
	"regexp"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// PromotionCodeExistsByCode reports whether any promotionCode doc has a `code` equal to the given
// value, case-insensitively. Matched with an anchored, case-insensitive regex over the QuoteMeta'd
// code (so the code is matched literally, not as a pattern).
func (r *Repo) PromotionCodeExistsByCode(ctx context.Context, code string) (bool, error) {
	pattern := "^" + regexp.QuoteMeta(code) + "$"
	filter := pgdoc.M{"code": pgdoc.M{"$regex": pattern, "$options": "i"}}
	return r.c(promotionCodeCollection).Exists(ctx, filter)
}
