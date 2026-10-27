package pricing

import (
	"testing"
	"time"
)

func lvl(level, pct int) TaxLevel { return TaxLevel{Level: level, Percentage: pct} }

func taxRate(levels ...TaxLevel) TaxRate {
	return TaxRate{Level: TaxAudienceAll, AccessMode: AccessPublic, RateLevels: levels}
}

func TestCalculateGrossAmount(t *testing.T) {
	cases := []struct {
		name      string
		amount    string
		rates     []TaxRate
		wantGross string
	}{
		{"single_19pct", "100", []TaxRate{taxRate(lvl(0, 19))}, "119"},
		{"compound_within_rate", "100", []TaxRate{taxRate(lvl(0, 10), lvl(1, 5))}, "115.5"},         // 100→110→115.5 (tax on tax)
		{"additive_across_rates", "100", []TaxRate{taxRate(lvl(0, 10)), taxRate(lvl(0, 5))}, "115"}, // each restarts from 100
		{"scale_half_up_final", "33.33", []TaxRate{taxRate(lvl(0, 19))}, "39.66"},                   // 33.33*1.19=39.6627 → setScale(2,HALF_UP)
		{"sort_levels_ascending", "100", []TaxRate{taxRate(lvl(2, 5), lvl(1, 10))}, "115.5"},        // sorted → 10% then 5%
		{"math_context_2_half_up_fraction", "100", []TaxRate{taxRate(lvl(0, 125))}, "230"},          // 125/100 → 2-sig HALF_UP → 1.3
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CalculateGrossAmount(mustDec(c.amount), c.rates)
			if !got.Equal(mustDec(c.wantGross)) {
				t.Errorf("gross = %s, want %s", got, c.wantGross)
			}
		})
	}
}

func TestCalculateTaxAmountAndTotalPercentage(t *testing.T) {
	rates := []TaxRate{taxRate(lvl(0, 10), lvl(1, 5)), taxRate(lvl(0, 3))}
	if tax := CalculateTaxAmount(mustDec("100"), []TaxRate{taxRate(lvl(0, 19))}); !tax.Equal(mustDec("19")) {
		t.Errorf("tax = %s, want 19", tax)
	}
	if p := CalculateTotalPercentage(rates); p != 18 {
		t.Errorf("totalPercentage = %d, want 18", p)
	}
}

func TestSelectTaxRates(t *testing.T) {
	now := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	de := TaxRate{Name: "DE-19", Country: "DE", Level: TaxAudienceAll, AccessMode: AccessPublic, RateLevels: []TaxLevel{lvl(0, 19)}}
	deBiz := TaxRate{Name: "DE-biz", Country: "DE", Level: TaxAudienceBusinessOnly, AccessMode: AccessPublic, RateLevels: []TaxLevel{lvl(0, 5)}}
	global := TaxRate{Name: "global", Country: "", Level: TaxAudienceAll, AccessMode: AccessPublic, RateLevels: []TaxLevel{lvl(0, 10)}}
	scoped := TaxRate{Name: "scoped", Country: "DE", Level: TaxAudienceAll, AccessMode: AccessScoped, RateLevels: []TaxLevel{lvl(0, 99)}}
	all := []TaxRate{de, deBiz, global, scoped}

	t.Run("country_match_consumer_excludes_business_and_scoped", func(t *testing.T) {
		got := SelectTaxRates(all, "DE", false, now)
		if len(got) != 1 || got[0].Name != "DE-19" {
			t.Errorf("got %v, want [DE-19]", names(got))
		}
	})
	t.Run("country_match_company_includes_business", func(t *testing.T) {
		got := SelectTaxRates(all, "DE", true, now)
		if len(got) != 2 {
			t.Errorf("got %v, want [DE-19 DE-biz]", names(got))
		}
	})
	t.Run("fallback_to_global_when_no_country_match", func(t *testing.T) {
		got := SelectTaxRates(all, "FR", false, now)
		if len(got) != 1 || got[0].Name != "global" {
			t.Errorf("got %v, want [global]", names(got))
		}
	})
	t.Run("time_range_excludes_future_start", func(t *testing.T) {
		start := now.Add(24 * time.Hour)
		future := TaxRate{Name: "future", Country: "IT", Level: TaxAudienceAll, AccessMode: AccessPublic, StartDateEnabled: true, StartDate: &start, RateLevels: []TaxLevel{lvl(0, 22)}}
		// IT has only the future rate → excluded → fall back to global.
		got := SelectTaxRates([]TaxRate{future, global}, "IT", false, now)
		if len(got) != 1 || got[0].Name != "global" {
			t.Errorf("got %v, want [global] (future rate excluded)", names(got))
		}
	})
}

func names(rates []TaxRate) []string {
	out := make([]string, len(rates))
	for i, r := range rates {
		out[i] = r.Name
	}
	return out
}
