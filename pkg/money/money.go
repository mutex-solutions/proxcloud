// Package money provides a decimal money type with DECIMAL128 (34-digit)
// precision semantics, serialized as a JSON string (never an IEEE float). Money
// is NEVER a float — all arithmetic uses shopspring/decimal. In the JSONB store
// the string sits at the field directly and comparisons cast it to numeric.
package money

import (
	"encoding/json"

	"github.com/shopspring/decimal"
)

// Money wraps a decimal.Decimal and (de)serializes as a JSON string.
type Money struct {
	D decimal.Decimal
}

func New(d decimal.Decimal) Money { return Money{D: d} }

func FromString(s string) (Money, error) {
	d, err := decimal.NewFromString(s)
	return Money{D: d}, err
}

func (m Money) String() string { return m.D.String() }

func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.D.String())
}

func (m *Money) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// tolerate a bare number too
		d, derr := decimal.NewFromString(string(b))
		if derr != nil {
			return err
		}
		m.D = d
		return nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return err
	}
	m.D = d
	return nil
}
