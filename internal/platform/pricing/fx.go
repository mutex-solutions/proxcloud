package pricing

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// ExchangeClient fetches a spot rate. The
// concrete BNR/ProxCloud HTTP clients are deferred; the engine takes the interface.
type ExchangeClient interface {
	GetExchangeRate(baseCurrency, exchangedCurrency string, date time.Time) (decimal.Decimal, error)
}

// Exchanger holds the currency-conversion methods.
// Same-currency conversions short-circuit to the input (no rate fetch). The
// multiply paths are exact; exchangeToProductCurrency divides HALF_UP at the
// amount's scale.
type Exchanger struct{ client ExchangeClient }

// NewExchanger builds an Exchanger over the given rate client.
func NewExchanger(c ExchangeClient) *Exchanger { return &Exchanger{client: c} }

// GetExchangeRate returns 1 for the same
// currency, else the client rate.
func (x *Exchanger) GetExchangeRate(baseCurrency, exchangedCurrency string, date time.Time) (decimal.Decimal, error) {
	if baseCurrency == exchangedCurrency {
		return dOne, nil
	}
	// Live FX is deferred (no rate client wired); a cross-currency conversion must fail cleanly
	// rather than nil-deref the absent client.
	if x.client == nil {
		return decimal.Zero, fmt.Errorf("exchange rate client not configured (%s→%s)", baseCurrency, exchangedCurrency)
	}
	return x.client.GetExchangeRate(baseCurrency, exchangedCurrency, date)
}

// Exchange returns the identity for same currency, else
// amount × rate (exact).
func (x *Exchanger) Exchange(amount decimal.Decimal, baseCurrency, exchangedCurrency string, date time.Time) (decimal.Decimal, error) {
	if baseCurrency == exchangedCurrency {
		return amount, nil
	}
	rate, err := x.GetExchangeRate(baseCurrency, exchangedCurrency, date)
	if err != nil {
		return decimal.Zero, err
	}
	return amount.Mul(rate), nil
}

// ExchangeToProductCurrency returns the identity when
// the currency is the base; else amount ÷ rate(base→currency), rounded HALF_UP at
// the amount's scale.
func (x *Exchanger) ExchangeToProductCurrency(amount decimal.Decimal, currency, baseCurrency string, date time.Time) (decimal.Decimal, error) {
	if baseCurrency == currency {
		return amount, nil
	}
	rate, err := x.GetExchangeRate(baseCurrency, currency, date)
	if err != nil {
		return decimal.Zero, err
	}
	return divHalfUpScale(amount, rate), nil
}

// ExchangeToBillingProfileCurrency returns the identity when already the profile
// currency, else amount × rate
// (current→profile).
func (x *Exchanger) ExchangeToBillingProfileCurrency(amount decimal.Decimal, profileCurrency, currentCurrency string, date time.Time) (decimal.Decimal, error) {
	if profileCurrency == currentCurrency {
		return amount, nil
	}
	rate, err := x.GetExchangeRate(currentCurrency, profileCurrency, date)
	if err != nil {
		return decimal.Zero, err
	}
	return amount.Mul(rate), nil
}

// divHalfUpScale divides HALF_UP, with the
// result scale equal to the dividend's scale. For non-negative money HALF_UP == half
// away from zero (shopspring DivRound).
func divHalfUpScale(amount, rate decimal.Decimal) decimal.Decimal {
	scale := int32(0)
	if e := amount.Exponent(); e < 0 {
		scale = -e
	}
	return amount.DivRound(rate, scale)
}
