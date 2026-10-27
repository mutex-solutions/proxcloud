package pricing

import (
	"time"

	"github.com/cockroachdb/apd/v3"
	"github.com/shopspring/decimal"
)

// Clock supplies "now" for monthly proration; injected so golden tests are
// deterministic. Production uses SystemClock (UTC).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// SystemClock returns the production UTC clock.
func SystemClock() Clock { return systemClock{} }

// fixedClock is a deterministic Clock for tests.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// dec128Ctx is DECIMAL128 = 34 significant digits,
// HALF_EVEN (banker's rounding). shopspring/decimal cannot do
// significant-digit HALF_EVEN division, so we route the two DECIMAL128 division
// sites through cockroachdb/apd.
var dec128Ctx = func() *apd.Context {
	c := apd.BaseContext.WithPrecision(34)
	c.Rounding = apd.RoundHalfEven
	return c
}()

// mathCtx2 is a 2-significant-digit, HALF_UP context, used for the tax percentage/100
// fraction in calculateGrossAmount.
var mathCtx2 = func() *apd.Context {
	c := apd.BaseContext.WithPrecision(2)
	c.Rounding = apd.RoundHalfUp
	return c
}()

// divDecimal128 divides at DECIMAL128 precision.
func divDecimal128(a, b decimal.Decimal) decimal.Decimal {
	ad, _, _ := apd.NewFromString(a.String())
	bd, _, _ := apd.NewFromString(b.String())
	res := new(apd.Decimal)
	_, _ = dec128Ctx.Quo(res, ad, bd)
	out, _ := decimal.NewFromString(res.Text('f'))
	return out
}

// percentFraction computes percentage/100 —
// the whole-percent tax fraction at 2 significant digits HALF_UP.
func percentFraction(percentage int) decimal.Decimal {
	res := new(apd.Decimal)
	_, _ = mathCtx2.Quo(res, apd.New(int64(percentage), 0), apd.New(100, 0))
	out, _ := decimal.NewFromString(res.Text('f'))
	return out
}

// scaleHalfUp rounds to 2 decimal places, HALF_UP. Amounts
// are non-negative, where shopspring Round (half away from zero) == HALF_UP.
func scaleHalfUp(d decimal.Decimal) decimal.Decimal { return d.Round(2) }

// scaleTotal rounds to 4 decimal places, HALF_UP.
func scaleTotal(d decimal.Decimal) decimal.Decimal { return d.Round(4) }

// daysInMonth returns the number of days in t's month (day 0 of the next month).
func daysInMonth(t time.Time) int {
	return time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// firstDayOfCurrentMonth returns UTC midnight on the 1st of now's month.
func firstDayOfCurrentMonth(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// lastDayOfCurrentMonth returns the last day of now's month at
// 23:59:59.999999999 UTC.
func lastDayOfCurrentMonth(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), daysInMonth(now), 23, 59, 59, 999999999, time.UTC)
}

// totalHoursCurrentMonth returns the number of hours in now's month.
func totalHoursCurrentMonth(now time.Time) int { return daysInMonth(now) * 24 }
