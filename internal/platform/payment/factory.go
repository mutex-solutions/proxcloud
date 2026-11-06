package payment

import (
	"strconv"
	"sync"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// GatewayFactory builds the concrete card gateway for a payment integration (nil when the
// integration is not a card gateway — BankTransfer, or an unknown thirdParty). The services take
// it as a dependency so tests inject a fake.
type GatewayFactory func(gw *billing.Gateway) Gateway

// NewGateway is the production factory: Stripe keyed by secret.privateKey, Airwallex by
// config.clientId + secret.apiKey (+ config.sandbox for the demo host).
func NewGateway(gw *billing.Gateway) Gateway {
	if gw == nil {
		return nil
	}
	switch gw.ThirdParty {
	case billing.ThirdPartyStripe:
		return NewStripeGateway(gw.SecretString("privateKey"))
	case billing.ThirdPartyAirwallex:
		return airwallexFor(gw.ConfigString("clientId"), gw.SecretString("apiKey"), gw.Sandbox())
	}
	return nil
}

// airwallexFor returns one AirwallexGateway per (client id, key, host) so the login token cache is
// shared across calls instead of logging in on every operation.
func airwallexFor(clientID, apiKey string, sandbox bool) *AirwallexGateway {
	key := clientID + "\x00" + apiKey + "\x00" + strconv.FormatBool(sandbox)
	awxMu.Lock()
	defer awxMu.Unlock()
	if g, ok := awxCache[key]; ok {
		return g
	}
	g := NewAirwallexGateway(clientID, apiKey, sandbox)
	awxCache[key] = g
	return g
}

var (
	awxMu    sync.Mutex
	awxCache = map[string]*AirwallexGateway{}
)

// cardGateway resolves the card gateway for an integration, or the 400 the flows return for a
// non-card gateway (BankTransfer deposits are dispatched before this is reached).
func cardGateway(factory GatewayFactory, gw *billing.Gateway) (Gateway, error) {
	if !gw.IsCard() {
		return nil, httpx.BadRequest("Unsupported payment gateway: " + gw.ThirdParty)
	}
	g := factory(gw)
	if g == nil {
		return nil, httpx.BadRequest("Unsupported payment gateway: " + gw.ThirdParty)
	}
	return g, nil
}
