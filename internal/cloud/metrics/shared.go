// shared.go — the measure-fetcher shared types/helpers that outlived the Gnocchi
// client (removed with the OpenStack backend; Prometheus is the only live source).
package metrics

import (
	"fmt"

	"github.com/cockroachdb/apd/v3"
	"github.com/shopspring/decimal"
)

// DefaultGranularity is kept for the config plumbing (gnocchi-era knob; the
// prometheus path ignores it).
const (
	DefaultGranularity = 300
	bytesPerMB         = 1048576
)

// decimal64 = 16 significant digits, HALF_EVEN — significant-digit division
// before feeding the rating engine (shopspring rounds by decimal places).
var decimal64 = apd.Context{Precision: 16, Rounding: apd.RoundHalfEven, MaxExponent: apd.MaxExponent, MinExponent: apd.MinExponent}

type Resource struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Metrics map[string]string `json:"metrics"`
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
func divDecimal64(a, b decimal.Decimal) (decimal.Decimal, error) {
	if b.IsZero() {
		return decimal.Zero, fmt.Errorf("gnocchi: divide by zero")
	}
	ad, _, err := apd.NewFromString(a.String())
	if err != nil {
		return decimal.Zero, err
	}
	bd, _, err := apd.NewFromString(b.String())
	if err != nil {
		return decimal.Zero, err
	}
	res := new(apd.Decimal)
	if _, err := decimal64.Quo(res, ad, bd); err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromString(res.Text('f'))
}
