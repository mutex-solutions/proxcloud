package pricing_test

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/deploy/seed"
	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// The seed's sandbox CPU + RAM rule is the default price decided on 2026-11-28 (docs/pricing-rate-card.md): AWS
// Lambda's x86 price at the memory size that gives one vCPU, split 8 to 1 between vCPU and memory. This holds the
// rule to the two decimal strings, to the shape the charge reads (resource type, the two priced attributes, the
// lowercase time unit the rule query matches exactly) and to its name: seed-operator.py matches rules by name, so a
// renamed rule would be added beside the old one on a seeded site and both would be charged.
//
// An external test package, as billing's cloud-type parity test: pricing itself does not import internal/cloud.
func TestSeedSandboxRule(t *testing.T) {
	var file struct {
		Rules []json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(seed.PricePlan, &file); err != nil {
		t.Fatal(err)
	}
	var rules []pricing.PricePlanRule
	found := 0
	for _, raw := range file.Rules {
		var r pricing.PricePlanRule
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		rules = append(rules, r)
		if r.ResourceType != "sandbox" || len(r.Prices) == 0 || r.Prices[0].AttributeName == "gpu_count" {
			continue
		}
		found++
		// The values as the file writes them: a decimal would read "0.001420" as the same number.
		var written struct {
			Prices []struct {
				AttributeName string `json:"attributeName"`
				Tiers         []struct {
					From, To *string
					Value    string `json:"value"`
				} `json:"tiers"`
			} `json:"prices"`
		}
		if err := json.Unmarshal(raw, &written); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, p := range written.Prices {
			if len(p.Tiers) != 1 || p.Tiers[0].From != nil || p.Tiers[0].To != nil {
				t.Fatalf("%s: want one flat tier, got %+v", p.AttributeName, p.Tiers)
			}
			got[p.AttributeName] = p.Tiers[0].Value
		}
		if want := map[string]string{"vcpus": "0.00142", "ram_gb": "0.00018"}; !maps.Equal(got, want) {
			t.Errorf("prices %v, want %v", got, want)
		}
		if r.Name != "PLACEHOLDER sandbox: CPU + RAM per minute" || r.TimeUnit != pricing.TimeUnitMinute ||
			r.ApplyMethod != pricing.ApplyAddToTotal || len(r.Filters) != 0 || len(r.Modifiers) != 0 {
			t.Errorf("rule %+v", r)
		}
	}
	if found != 1 {
		t.Fatalf("%d sandbox CPU + RAM rules in the seed, want 1: every matching rule adds up", found)
	}

	// One minute of a sandbox as its billing provider hands it to the charge, rated with every rule of the seed.
	for _, c := range []struct {
		cpu, memoryGiB float64
		want           string
	}{{1, 1, "0.00160"}, {2, 1, "0.00302"}, {2, 4, "0.00356"}} {
		out, err := billingresource.NewSandboxProvider().GetBillingInformation(context.Background(), billingapi.BillingContext{},
			&cloud.CloudResource{ID: "r1", ProjectID: "p1", Type: cloud.TypeSandbox, Data: map[string]any{
				"status": "RUNNING", "runningSince": "2026-11-28T10:00:00Z", "cpu": c.cpu, "memoryGiB": c.memoryGiB,
			}})
		if err != nil || len(out) != 1 {
			t.Fatalf("out=%v err=%v", out, err)
		}
		results, err := pricing.NewEngine(nil).ApplyPricePlanRules(rules, pricing.FromAPIResource(out[0]), pricing.TimeUnitMinute)
		if err != nil {
			t.Fatal(err)
		}
		if got := pricing.SumNetAmount(results); !got.Equal(decimal.RequireFromString(c.want)) {
			t.Errorf("%v vCPU, %v GiB: %s a minute, want %s", c.cpu, c.memoryGiB, got, c.want)
		}
	}
}
