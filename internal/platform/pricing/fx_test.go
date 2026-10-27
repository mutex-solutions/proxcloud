package pricing

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

type stubClient struct {
	rate  decimal.Decimal
	err   error
	calls int
}

func (s *stubClient) GetExchangeRate(_, _ string, _ time.Time) (decimal.Decimal, error) {
	s.calls++
	return s.rate, s.err
}

func TestExchange(t *testing.T) {
	now := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)

	t.Run("same_currency_identity_no_fetch", func(t *testing.T) {
		c := &stubClient{rate: mustDec("4.5")}
		x := NewExchanger(c)
		got, err := x.Exchange(mustDec("100"), "USD", "USD", now)
		if err != nil || !got.Equal(mustDec("100")) {
			t.Fatalf("got %s err %v, want 100", got, err)
		}
		if c.calls != 0 {
			t.Errorf("same-currency must not fetch a rate, calls=%d", c.calls)
		}
	})
	t.Run("multiply_exact", func(t *testing.T) {
		x := NewExchanger(&stubClient{rate: mustDec("4.5")})
		got, _ := x.Exchange(mustDec("100"), "USD", "RON", now)
		if !got.Equal(mustDec("450")) {
			t.Errorf("100 USD→RON@4.5 = %s, want 450", got)
		}
	})
	t.Run("rate_error_propagates", func(t *testing.T) {
		x := NewExchanger(&stubClient{err: errors.New("client down")})
		if _, err := x.Exchange(mustDec("100"), "USD", "RON", now); err == nil {
			t.Error("expected the client error to propagate")
		}
	})
}

func TestGetExchangeRate(t *testing.T) {
	now := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	c := &stubClient{rate: mustDec("4.5")}
	x := NewExchanger(c)
	if r, _ := x.GetExchangeRate("USD", "USD", now); !r.Equal(mustDec("1")) || c.calls != 0 {
		t.Errorf("same currency must be 1 without a fetch, got %s calls %d", r, c.calls)
	}
	if r, _ := x.GetExchangeRate("USD", "RON", now); !r.Equal(mustDec("4.5")) {
		t.Errorf("USD→RON = %s, want 4.5", r)
	}
}

func TestExchangeToProductCurrency(t *testing.T) {
	now := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)

	t.Run("base_equals_currency_identity", func(t *testing.T) {
		c := &stubClient{rate: mustDec("4.5")}
		x := NewExchanger(c)
		got, _ := x.ExchangeToProductCurrency(mustDec("100"), "USD", "USD", now)
		if !got.Equal(mustDec("100")) || c.calls != 0 {
			t.Errorf("identity expected, got %s calls %d", got, c.calls)
		}
	})
	t.Run("divide_half_up_at_amount_scale_2", func(t *testing.T) {
		x := NewExchanger(&stubClient{rate: mustDec("4.5")})
		got, _ := x.ExchangeToProductCurrency(mustDec("100.00"), "RON", "USD", now)
		if !got.Equal(mustDec("22.22")) { // 100.00/4.5 = 22.2222 → HALF_UP @ scale 2
			t.Errorf("= %s, want 22.22", got)
		}
	})
	t.Run("divide_half_up_at_amount_scale_0", func(t *testing.T) {
		x := NewExchanger(&stubClient{rate: mustDec("3")})
		got, _ := x.ExchangeToProductCurrency(mustDec("100"), "RON", "USD", now)
		if !got.Equal(mustDec("33")) { // 100/3 = 33.33 → HALF_UP @ scale 0
			t.Errorf("= %s, want 33", got)
		}
	})
}

func TestExchangeToBillingProfileCurrency(t *testing.T) {
	now := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	t.Run("already_profile_currency", func(t *testing.T) {
		c := &stubClient{rate: mustDec("2")}
		x := NewExchanger(c)
		got, _ := x.ExchangeToBillingProfileCurrency(mustDec("100"), "RON", "RON", now)
		if !got.Equal(mustDec("100")) || c.calls != 0 {
			t.Errorf("identity expected, got %s calls %d", got, c.calls)
		}
	})
	t.Run("multiply_to_profile_currency", func(t *testing.T) {
		x := NewExchanger(&stubClient{rate: mustDec("2")})
		got, _ := x.ExchangeToBillingProfileCurrency(mustDec("100"), "RON", "USD", now)
		if !got.Equal(mustDec("200")) {
			t.Errorf("= %s, want 200", got)
		}
	})
}
