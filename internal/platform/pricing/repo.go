package pricing

import (
	"context"
	"sync"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Repo is the pgdoc-backed reader for the pricing config collections (pricePlan,
// pricePlanRule, taxRate). Decimal money fields round-trip via the decimal.Decimal
// codec in pgdoc. _id is the config-as-code slug string (admin-created plans are a
// later slice). Read-only here; admin CRUD + bill/credit persistence land separately.
type Repo struct {
	db          *pgdoc.DB
	plans       *pgdoc.Store
	rules       *pgdoc.Store
	taxes       *pgdoc.Store
	bills       *pgdoc.Store
	ensureOnce  sync.Once
	ensureError error
}

func NewRepo(db *pgdoc.DB) *Repo {
	return &Repo{
		db:    db,
		plans: db.C("pricePlan"),
		rules: db.C("pricePlanRule"),
		taxes: db.C("taxRate"),
		bills: db.C("bill"),
	}
}

// ensureBills creates the bill table on first use. The bill lock/create path
// runs inside a transaction as its first statement, where the store's
// on-demand table-create can't fire, so the table must exist beforehand.
func (r *Repo) ensureBills(ctx context.Context) error {
	r.ensureOnce.Do(func() { r.ensureError = r.bills.Ensure(ctx) })
	return r.ensureError
}

// FindPricePlanByID loads a price plan by id (nil when absent).
func (r *Repo) FindPricePlanByID(ctx context.Context, id string) (*PricePlan, error) {
	var pp PricePlan
	found, err := r.plans.FindOne(ctx, pgdoc.M{"_id": id}, &pp)
	if err != nil || !found {
		return nil, err
	}
	return &pp, nil
}

// PublicPricePlans returns the enabled plans with PUBLIC access mode.
func (r *Repo) PublicPricePlans(ctx context.Context) ([]PricePlan, error) {
	var out []PricePlan
	if err := r.plans.Find(ctx, pgdoc.M{"accessMode": AccessPublic, "enabled": true}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RulesByPricePlanIDAndTimeUnit returns the rules for a price plan and time unit.
func (r *Repo) RulesByPricePlanIDAndTimeUnit(ctx context.Context, pricePlanID, timeUnit string) ([]PricePlanRule, error) {
	var out []PricePlanRule
	if err := r.rules.Find(ctx, pgdoc.M{"pricePlanId": pricePlanID, "timeUnit": timeUnit}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AllTaxRates returns every configured tax rate (the country/audience/time + global-
// fallback selection is applied in-memory by SelectTaxRates — the taxRate collection
// is small config-as-code).
func (r *Repo) AllTaxRates(ctx context.Context) ([]TaxRate, error) {
	var out []TaxRate
	if err := r.taxes.Find(ctx, pgdoc.M{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PlanSource adapts the Repo to the pure PricePlanSource for SelectPricePlans. Query
// errors collapse to not-found/empty (rating resilience — the per-profile job
// catches failures); a caller that needs the error should use the Repo directly.
func (r *Repo) PlanSource(ctx context.Context) PricePlanSource { return repoPlanSource{ctx: ctx, r: r} }

type repoPlanSource struct {
	ctx context.Context
	r   *Repo
}

func (s repoPlanSource) FindByID(id string) (PricePlan, bool) {
	pp, err := s.r.FindPricePlanByID(s.ctx, id)
	if err != nil || pp == nil {
		return PricePlan{}, false
	}
	return *pp, true
}

func (s repoPlanSource) PublicPricePlans() []PricePlan {
	ps, _ := s.r.PublicPricePlans(s.ctx)
	return ps
}

// RuleSource adapts the Repo to the pure PricePlanRuleSource for ApplicableRules.
func (r *Repo) RuleSource(ctx context.Context) PricePlanRuleSource {
	return repoRuleSource{ctx: ctx, r: r}
}

type repoRuleSource struct {
	ctx context.Context
	r   *Repo
}

func (s repoRuleSource) RulesByPricePlanIDAndTimeUnit(id, tu string) []PricePlanRule {
	rs, _ := s.r.RulesByPricePlanIDAndTimeUnit(s.ctx, id, tu)
	return rs
}
