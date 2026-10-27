package payment

import (
	"context"
	"strings"
	"testing"
)

// TestEscapeSearchValue pins the escaping of a Stripe Search Query Language literal.
//
// Stripe's search grammar is SQL-like — clauses combine with AND/OR and can reference other fields —
// so a raw double quote in an interpolated value closes the literal and everything after it is
// parsed as query syntax. The consequence is not a syntax error: the search silently widens and can
// resolve a DIFFERENT customer, which GetOrCreateCustomer then charges or credits.
func TestEscapeSearchValue(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"ordinary hex id is untouched", "0f8a1c2b3d4e", "0f8a1c2b3d4e"},
		{"quote is escaped", `a"b`, `a\"b`},
		{"backslash is escaped", `a\b`, `a\\b`},
		// Backslash must be escaped BEFORE the quote, or the attacker's own backslash would
		// re-escape the quote we add and re-open the injection.
		{"backslash cannot re-escape the quote", `a\"b`, `a\\\"b`},
		{"injection attempt is neutralised", `x" OR metadata["k"]:"y`, `x\" OR metadata[\"k\"]:\"y`},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeSearchValue(tc.in); got != tc.want {
				t.Errorf("escapeSearchValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEscapedValueCannotCloseTheLiteral is the property that actually matters: after escaping, the
// value contains no unescaped quote, so it cannot terminate the string literal it is placed in.
func TestEscapedValueCannotCloseTheLiteral(t *testing.T) {
	hostile := []string{
		`x" OR metadata["billingProfileId"]:"other`,
		`" OR id:"cus_victim`,
		`x\" OR amount>0`,
		`""""`,
		`\\\\"`,
	}
	for _, in := range hostile {
		escaped := escapeSearchValue(in)
		if unescapedQuote(escaped) {
			t.Errorf("escapeSearchValue(%q) = %q still contains an unescaped quote — the literal can be closed", in, escaped)
		}
	}
}

// unescapedQuote reports whether s contains a double quote that is not preceded by an odd number of
// backslashes (i.e. one that would terminate a string literal).
func unescapedQuote(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		back := 0
		for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
			back++
		}
		if back%2 == 0 {
			return true
		}
	}
	return false
}

// TestGetOrCreateCustomerRejectsEmptyID: an empty search value matches broadly, and the customer
// returned would belong to somebody else. Refuse rather than guess.
func TestGetOrCreateCustomerRejectsEmptyID(t *testing.T) {
	g := &StripeGateway{} // never reached: the guard runs before any Stripe call
	for _, id := range []string{"", "   ", "\t"} {
		_, err := g.GetOrCreateCustomer(context.Background(), CustomerInput{BillingProfileID: id})
		if err == nil {
			t.Errorf("BillingProfileID %q should be refused", id)
			continue
		}
		if !strings.Contains(err.Error(), "billingProfileId") {
			t.Errorf("error should name the offending field, got: %v", err)
		}
	}
}
