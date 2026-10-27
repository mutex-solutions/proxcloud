package payment

import "testing"

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

func TestGatewayErrorMessage(t *testing.T) {
	if got := gatewayErrorMessage("Your card was declined.", "card_declined"); got != "Your card was declined. (card_declined)" {
		t.Errorf("with code = %q", got)
	}
	if got := gatewayErrorMessage("Generic failure", ""); got != "Generic failure" {
		t.Errorf("no code = %q", got)
	}
}
