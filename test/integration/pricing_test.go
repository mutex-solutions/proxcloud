//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

func decp(s string) *decimal.Decimal { d := decimal.RequireFromString(s); return &d }

// TestPricingRepoEndToEnd exercises the whole pricing I/O path against real Postgres:
// the decimal↔JSON-string codec (tier/percent values round-trip), the repos, the
// price-plan selection over the repo source, and the engine + tax on the result.
func TestPricingRepoEndToEnd(t *testing.T) {
	db := freshPG(t)
	ctx := context.Background()

	if _, err := db.C("pricePlan").InsertOne(ctx, pricing.PricePlan{
		ID: "pp-pub", Name: "Public", Enabled: true, AccessMode: pricing.AccessPublic,
	}); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	if _, err := db.C("pricePlanRule").InsertOne(ctx, pricing.PricePlanRule{
		ID: "r1", TimeUnit: "hour", ResourceType: "compute", PricePlanID: "pp-pub",
		Prices: []pricing.PricePlanRulePrice{{AttributeName: "vcpu", Tiers: []pricing.PriceTier{
			{From: decp("0"), To: decp("10"), Value: decp("5")},
		}}},
	}); err != nil {
		t.Fatalf("insert rule: %v", err)
	}
	if _, err := db.C("taxRate").InsertOne(ctx, pricing.TaxRate{
		ID: "tax-de", Country: "DE", Level: pricing.TaxAudienceAll, AccessMode: pricing.AccessPublic,
		RateLevels: []pricing.TaxLevel{{Level: 0, Percentage: 19}},
	}); err != nil {
		t.Fatalf("insert tax: %v", err)
	}

	repo := pricing.NewRepo(db)

	// Repo reads + codec round-trip.
	pp, err := repo.FindPricePlanByID(ctx, "pp-pub")
	if err != nil || pp == nil || pp.Name != "Public" {
		t.Fatalf("FindPricePlanByID = %v, %v", pp, err)
	}
	if pubs, _ := repo.PublicPricePlans(ctx); len(pubs) != 1 {
		t.Fatalf("PublicPricePlans = %d, want 1", len(pubs))
	}
	rules, err := repo.RulesByPricePlanIDAndTimeUnit(ctx, "pp-pub", "hour")
	if err != nil || len(rules) != 1 {
		t.Fatalf("rules = %d, %v", len(rules), err)
	}
	if v := rules[0].Prices[0].Tiers[0].Value; v == nil || !v.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("tier value round-trip = %v, want 5 (decimal↔Decimal128)", v)
	}

	// Selection (repo source) → engine end-to-end.
	plans := pricing.SelectPricePlans(repo.PlanSource(ctx), nil, false) // not scoped → public
	appRules := pricing.ApplicableRules(plans, repo.RuleSource(ctx), "hour")
	if len(appRules) != 1 {
		t.Fatalf("applicable rules = %d, want 1", len(appRules))
	}
	res := &pricing.BillingResource{
		ResourceType: "compute", Values: map[string]any{"vcpu": 8},
		BillingResourceType: &pricing.BillingResourceType{ResourceType: "compute", Attributes: []pricing.ResourceAttribute{{Name: "vcpu", Type: "number"}}},
	}
	results, err := pricing.NewEngine(pricing.SystemClock()).ApplyPricePlanRules(appRules, res, "hour")
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	net := pricing.SumNetAmount(results)
	if !net.Equal(decimal.RequireFromString("40")) { // 8 vcpu * 5
		t.Fatalf("net = %s, want 40", net)
	}

	// Tax from the repo on the rated net.
	taxes, _ := repo.AllTaxRates(ctx)
	sel := pricing.SelectTaxRates(taxes, "DE", false, time.Now().UTC())
	gross := pricing.CalculateGrossAmount(net, sel)
	if !gross.Equal(decimal.RequireFromString("47.60")) { // 40 * 1.19
		t.Errorf("gross = %s, want 47.60", gross)
	}
}
