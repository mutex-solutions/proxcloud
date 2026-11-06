package payment

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

func TestNewGatewaySelectsByIntegration(t *testing.T) {
	stripe := &billing.Gateway{ThirdParty: billing.ThirdPartyStripe, Secret: map[string]any{"privateKey": "sk_test_x"}}
	if _, ok := NewGateway(stripe).(*StripeGateway); !ok {
		t.Fatalf("Stripe integration did not build a StripeGateway")
	}
	awx := &billing.Gateway{ThirdParty: billing.ThirdPartyAirwallex,
		Config: map[string]any{"clientId": "cid", "sandbox": true}, Secret: map[string]any{"apiKey": "k"}}
	g, ok := NewGateway(awx).(*AirwallexGateway)
	if !ok {
		t.Fatalf("Airwallex integration did not build an AirwallexGateway")
	}
	if g.host != airwallexDemoHost {
		t.Errorf("sandbox host = %s, want the demo host", g.host)
	}
	if NewGateway(awx) != Gateway(g) {
		t.Errorf("the same credentials must reuse one gateway (login token cache)")
	}
	live := &billing.Gateway{ThirdParty: billing.ThirdPartyAirwallex, Config: map[string]any{"clientId": "cid"}, Secret: map[string]any{"apiKey": "k"}}
	if lg := NewGateway(live).(*AirwallexGateway); lg.host != airwallexProdHost || lg == g {
		t.Errorf("a non-sandbox integration must use the production host and its own instance")
	}
	// manual / unknown integrations are not card gateways.
	if NewGateway(&billing.Gateway{ThirdParty: billing.ThirdPartyBankTransfer}) != nil || NewGateway(nil) != nil {
		t.Errorf("BankTransfer / nil must not build a card gateway")
	}
	if _, err := cardGateway(NewGateway, &billing.Gateway{ThirdParty: billing.ThirdPartyBankTransfer}); err == nil {
		t.Errorf("cardGateway must refuse a BankTransfer integration")
	}
}
