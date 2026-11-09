package payment

import (
	"errors"
	"fmt"
	"testing"

	stripe "github.com/stripe/stripe-go/v82"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func TestMapStatusTerminalStates(t *testing.T) {
	cases := map[string]string{
		"succeeded":               "SUCCESS",
		"failed":                  "FAILED",
		"canceled":                "CANCELLED", // distinct terminal state (cancellation), NOT FAILED
		"requires_payment_method": "PENDING",
		"processing":              "PENDING",
		"":                        "PENDING",
	}
	for in, want := range cases {
		if got := mapStatus(in); got != want {
			t.Errorf("mapStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIntentGone(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&stripe.Error{HTTPStatusCode: 404}, true},
		{fmt.Errorf("retrieve: %w", &AirwallexError{HTTPStatus: 404}), true},
		{&stripe.Error{HTTPStatusCode: 500}, false},
		{&stripe.Error{HTTPStatusCode: 401}, false},
		{&stripe.Error{HTTPStatusCode: 429}, false},
		{&AirwallexError{HTTPStatus: 503}, false},
		{errors.New("connection reset by peer"), false},
		{httpx.NotFound("Payment gateway not found"), false},
	} {
		if got := intentGone(c.err); got != c.want {
			t.Errorf("intentGone(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestGatewayErrorMessage(t *testing.T) {
	if got := gatewayErrorMessage("Your card was declined.", "card_declined"); got != "Your card was declined. (card_declined)" {
		t.Errorf("with code = %q", got)
	}
	if got := gatewayErrorMessage("Generic failure", ""); got != "Generic failure" {
		t.Errorf("no code = %q", got)
	}
}
