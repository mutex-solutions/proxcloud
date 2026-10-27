package pricing

import (
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// SortedRateLevels returns the rate's levels sorted
// by level ascending (the compounding order in calculateGrossAmount). Stable; does
// not mutate the receiver.
func (r TaxRate) SortedRateLevels() []TaxLevel {
	out := make([]TaxLevel, len(r.RateLevels))
	copy(out, r.RateLevels)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Level < out[j].Level })
	return out
}

// CalculateGrossAmount computes the gross: within each rate the
// levels COMPOUND on the running taxable amount (tax-on-tax, ascending level order);
// across rates the taxes are ADDITIVE (each rate restarts from the original amount).
// The percentage fraction is at 2 sig figs, HALF_UP; the final gross is
// scaleHalfUp (2 dp, HALF_UP). Net amount math is otherwise exact.
func CalculateGrossAmount(amount decimal.Decimal, rates []TaxRate) decimal.Decimal {
	totalTax := dZero
	for _, rate := range rates {
		taxable := amount
		for _, lvl := range rate.SortedRateLevels() {
			currentTax := taxable.Mul(percentFraction(lvl.Percentage))
			taxable = taxable.Add(currentTax)
		}
		totalTax = totalTax.Add(taxable.Sub(amount))
	}
	return scaleHalfUp(amount.Add(totalTax))
}

// CalculateTaxAmount returns gross − net.
func CalculateTaxAmount(amount decimal.Decimal, rates []TaxRate) decimal.Decimal {
	return CalculateGrossAmount(amount, rates).Sub(amount)
}

// CalculateTotalPercentage sums all
// levels' whole-percent values across the rates.
func CalculateTotalPercentage(rates []TaxRate) int {
	total := 0
	for _, r := range rates {
		for _, l := range r.RateLevels {
			total += l.Percentage
		}
	}
	return total
}

// SelectTaxRates implements the automatic (non-manual) tax-rate selection,
// operating on an in-memory rate list (the datastore repo + the automaticTaxDisabled
// manual-rule path are deferred). Prefer rates matching the profile's country (and
// not SCOPED), filtered by active time window + audience; if none match, fall back
// to country-less (global) rates. `now` is injected for deterministic tests.
func SelectTaxRates(rates []TaxRate, country string, isCompany bool, now time.Time) []TaxRate {
	countryBased := filterTaxRates(rates, func(r TaxRate) bool {
		return r.Country == country && r.AccessMode != AccessScoped &&
			taxRateInTimeRange(r, now) && taxRateAppliesTo(r, isCompany)
	})
	if len(countryBased) > 0 {
		return countryBased
	}
	return filterTaxRates(rates, func(r TaxRate) bool {
		return r.Country == "" && r.AccessMode != AccessScoped &&
			taxRateInTimeRange(r, now) && taxRateAppliesTo(r, isCompany)
	})
}

func filterTaxRates(rates []TaxRate, keep func(TaxRate) bool) []TaxRate {
	out := make([]TaxRate, 0, len(rates))
	for _, r := range rates {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// taxRateAppliesTo reports whether the rate applies to the billing profile (audience level).
func taxRateAppliesTo(r TaxRate, isCompany bool) bool {
	switch r.Level {
	case TaxAudienceBusinessOnly:
		return isCompany
	case TaxAudienceConsumersOnly:
		return !isCompany
	case TaxAudienceAll:
		return true
	default:
		return false
	}
}

// taxRateInTimeRange reports whether now is within the rate's active window.
func taxRateInTimeRange(r TaxRate, now time.Time) bool {
	switch {
	case r.StartDateEnabled && r.EndDateEnabled:
		return r.StartDate != nil && r.EndDate != nil && now.After(*r.StartDate) && now.Before(*r.EndDate)
	case r.StartDateEnabled:
		return r.StartDate != nil && now.After(*r.StartDate)
	case r.EndDateEnabled:
		return r.EndDate != nil && now.Before(*r.EndDate)
	default:
		return true
	}
}
