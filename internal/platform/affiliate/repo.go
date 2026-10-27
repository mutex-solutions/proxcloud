// Package affiliate serves the client affiliate endpoints:
// GET /affiliate/check (cfy exists-check), GET /affiliate/project/{id}/config, and
// GET /affiliate/project/{id}/log. All three return BARE JSON (no envelope).
package affiliate

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Entry is one document in the "affiliateEntry" collection. amount/originAmount are
// modeled as any here (deferred precise typing — the client /log path returns an empty
// list under the current seed, so they are never serialized).
type Entry struct {
	ID               string     `json:"id,omitempty"`
	BillingProfileID string     `json:"billingProfileId,omitempty"`
	Amount           any        `json:"amount,omitempty"`
	OriginAmount     any        `json:"originAmount,omitempty"`
	CreatedAt        *time.Time `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time `json:"updatedAt,omitempty"`
}

type Repo struct {
	entries *pgdoc.Store
}

func NewRepo(db *pgdoc.DB) *Repo {
	return &Repo{entries: db.C("affiliateEntry")}
}

// EntriesByBillingProfile returns a profile's affiliate entries, newest first. Never nil
// (so JSON is [], not null).
func (r *Repo) EntriesByBillingProfile(ctx context.Context, bpID string) ([]Entry, error) {
	out := []Entry{}
	if err := r.entries.Find(ctx, pgdoc.M{"billingProfileId": bpID}, &out,
		pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime))); err != nil {
		return nil, err
	}
	return out, nil
}
