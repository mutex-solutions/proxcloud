package billing

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// gateway.go provides the payment-gateway listing (listAvailableGateways
// + the per-gateway PaymentGatewayFactory/PaymentOperations). Payment
// gateways are `thirdPartyIntegration` docs; each enabled one maps to a PaymentGatewayDTO with
// its supported methods (addFunds/addCard) + a UI metadata blob + minDeposit. Under the
// default seed the collection is empty → []. Stripe is the wired gateway.

// integration is the slice of a thirdPartyIntegration doc the gateway listing reads.
type integration struct {
	ID         string         `json:"id,omitempty"`
	Name       string         `json:"name,omitempty"`
	ThirdParty string         `json:"thirdParty,omitempty"`
	Config     map[string]any `json:"config,omitempty"`
}

// The payment-gateway integration kinds (thirdPartyIntegration.thirdParty).
const (
	ThirdPartyStripe       = "Stripe"
	ThirdPartyAirwallex    = "Airwallex"
	ThirdPartyBankTransfer = "BankTransfer"
)

// Gateway is a payment-gateway integration with its secret (the full doc add-funds needs:
// thirdParty to pick the provider, config for params, secret for the API key).
type Gateway struct {
	ID         string
	Name       string
	ThirdParty string
	Config     map[string]any
	Secret     map[string]any
}

// IsCard reports whether the integration is a card gateway (Stripe / Airwallex): it can store a
// card, charge it off-session and refund — as opposed to the manual BankTransfer gateway.
func (g *Gateway) IsCard() bool {
	return g != nil && (g.ThirdParty == ThirdPartyStripe || g.ThirdParty == ThirdPartyAirwallex)
}

// Sandbox reads config.sandbox — for Airwallex it selects the demo API host.
func (g *Gateway) Sandbox() bool {
	if g == nil || g.Config == nil {
		return false
	}
	b, _ := g.Config["sandbox"].(bool)
	return b
}

// SecretString reads a secret field (e.g. "privateKey"). The secret map is already decrypted at
// load time (GetGateway → repo encryptor), so this is a plain field read.
func (g *Gateway) SecretString(key string) string {
	if g.Secret == nil {
		return ""
	}
	s, _ := g.Secret[key].(string)
	return s
}

// ConfigString reads a config field (e.g. "publicKey").
func (g *Gateway) ConfigString(key string) string { return strVal(g.Config, key) }

// MinDeposit reads config.minDeposit as a decimal-ish float (0 if absent).
func (g *Gateway) MinDeposit() float64 {
	if g.Config == nil {
		return 0
	}
	switch v := g.Config["minDeposit"].(type) {
	case float64:
		return v
	case int32:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

// PaymentGatewayDTO is the payment-gateway DTO (field set: id/name/addCard/
// addFunds/thirdParty/metadata/minDeposit). minDeposit is a number → json.Number so it
// serializes unquoted.
type PaymentGatewayDTO struct {
	ID         string      `json:"id,omitempty"`
	Name       string      `json:"name,omitempty"`
	AddCard    bool        `json:"addCard"`
	AddFunds   bool        `json:"addFunds"`
	ThirdParty string      `json:"thirdParty,omitempty"`
	Metadata   any         `json:"metadata,omitempty"`
	MinDeposit json.Number `json:"minDeposit"`
}

// InvoiceGatewayDTO is the invoice-gateway DTO ({id, name, metadata}) — the client Bill History
// Transactions tab reads GET /api/v1/bill/gateways to render each bill's invoice gateway.
type InvoiceGatewayDTO struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Metadata any    `json:"metadata"`
}

// ListInvoiceGateways lists the thirdPartyIntegrations the
// FE can resolve a transaction's invoiceGatewayId against → {id, name=thirdParty, metadata}. The
// canonical listing is only category "Invoice", but the demo has no Invoice integration AND our txns set
// invoiceGatewayId = the PAYMENT gateway (getDefaultInvoiceGatewayId resolution is deferred). The FE
// download does `invoiceGateways.findBy("id", txn.invoiceGatewayId).metadata.source` and CRASHES if
// the gateway is absent, so we also return Payment-category gateways here. metadata is empty → the
// FE's `source` switch falls to the default (download) branch → ProxCloud generates the receipt PDF.
func (r *Repo) ListInvoiceGateways(ctx context.Context) ([]InvoiceGatewayDTO, error) {
	docs := []pgdoc.M{}
	if err := r.gateways.Find(ctx, pgdoc.M{}, &docs); err != nil {
		return nil, err
	}
	out := []InvoiceGatewayDTO{}
	for _, d := range docs {
		// Every gateway integration (non-blank thirdParty) is download-resolvable — our txns set
		// invoiceGatewayId to a payment gateway, and ProxCloud generates the receipt itself, so any
		// gateway "supports" download. metadata empty → the FE source switch → default → download.
		if strField(d["thirdParty"]) == "" {
			continue
		}
		out = append(out, InvoiceGatewayDTO{
			ID:       strField(d["_id"]),
			Name:     strField(d["thirdParty"]),
			Metadata: map[string]any{},
		})
	}
	return out, nil
}

func strField(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ListGateways lists every gateway (all gateway factories flat-mapped) + the DTO build.
// It lists every thirdPartyIntegration that is a known payment gateway and maps it; unknown
// thirdParty values are skipped (no factory for them).
func (r *Repo) ListGateways(ctx context.Context) ([]PaymentGatewayDTO, error) {
	var docs []integration
	if err := r.gateways.Find(ctx, pgdoc.M{}, &docs); err != nil {
		return nil, err
	}
	out := []PaymentGatewayDTO{}
	for _, in := range docs {
		dto, ok := gatewayDTO(in)
		if !ok {
			continue
		}
		out = append(out, dto)
	}
	return out, nil
}

// BankTransferGatewayID returns the id of the manual BankTransfer gateway integration, or ""
// when none is configured (an offline payment is still recordable without one).
func (r *Repo) BankTransferGatewayID(ctx context.Context) (string, error) {
	var doc integration
	found, err := r.gateways.FindOne(ctx, pgdoc.M{"thirdParty": "BankTransfer"}, &doc)
	if err != nil || !found {
		return "", err
	}
	return doc.ID, nil
}

// GetGateway loads one payment-gateway integration (with its secret) by id.
// nil if not found.
func (r *Repo) GetGateway(ctx context.Context, id string) (*Gateway, error) {
	var doc struct {
		ID         string         `json:"id"`
		Name       string         `json:"name"`
		ThirdParty string         `json:"thirdParty"`
		Config     map[string]any `json:"config"`
		Secret     map[string]any `json:"secret"`
	}
	found, err := r.gateways.Get(ctx, id, &doc)
	if err != nil || !found {
		return nil, err
	}
	// Decrypt the at-rest secret leaves (privateKey, …) with the wired encryptor. Nil-safe: no
	// encryptor → return as-stored; textcrypt is fail-open so a legacy plaintext value round-trips.
	if r.enc != nil {
		if m, ok := r.enc.DecryptObject(doc.Secret).(map[string]any); ok {
			doc.Secret = m
		}
	}
	return &Gateway{ID: doc.ID, Name: doc.Name, ThirdParty: doc.ThirdParty, Config: doc.Config, Secret: doc.Secret}, nil
}

// PaymentSettings is the customer-editable payment preference (Organization → Billing → Payment
// settings; the tenant PUT /billing-profile/{id} with only these keys). A nil field is left as is.
type PaymentSettings struct {
	PaymentMode      *string `json:"paymentMode"`
	PaymentGatewayID *string `json:"paymentGatewayId"`
}

// ValidPaymentMode normalizes a payment mode (case-insensitive) — "" when it is not one of
// PREPAID / POSTPAID.
func ValidPaymentMode(mode string) string {
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case PaymentModePrepaid:
		return PaymentModePrepaid
	case PaymentModePostpaid:
		return PaymentModePostpaid
	}
	return ""
}

// UpdatePaymentSettings writes the profile's payment mode / preferred gateway with a partial
// write (no billing-details validation). The gateway must be a configured payment gateway
// (Stripe / Airwallex / BankTransfer); an empty id clears the preference.
func (r *Repo) UpdatePaymentSettings(ctx context.Context, profile *BillingProfile, in PaymentSettings) (*BillingProfile, error) {
	set := pgdoc.M{}
	if in.PaymentMode != nil {
		mode := ValidPaymentMode(*in.PaymentMode)
		if mode == "" {
			return nil, httpx.BadRequest("Invalid payment mode: use PREPAID or POSTPAID.")
		}
		set["paymentMode"] = mode
		profile.PaymentMode = mode
	}
	if in.PaymentGatewayID != nil {
		id := strings.TrimSpace(*in.PaymentGatewayID)
		if id != "" {
			gw, err := r.GetGateway(ctx, id)
			if err != nil {
				return nil, err
			}
			if gw == nil {
				return nil, httpx.NotFound("Payment gateway not found")
			}
			if _, ok := gatewayDTO(integration{ID: gw.ID, Name: gw.Name, ThirdParty: gw.ThirdParty, Config: gw.Config}); !ok {
				return nil, httpx.BadRequest("Not a payment gateway: " + gw.ThirdParty)
			}
		}
		set["paymentGatewayId"] = id
		profile.PaymentGatewayID = id
	}
	if len(set) == 0 {
		return profile, nil
	}
	now := time.Now().UTC()
	set["updatedAt"] = now
	if _, err := r.profiles.SetByID(ctx, profile.ID, set, nil); err != nil {
		return nil, err
	}
	profile.UpdatedAt = &now
	return profile, nil
}

// ClaimAccountCreditTransaction atomically moves a PENDING deposit to PROCESSING (a conditional
// single-row update) so exactly one caller settles it: the confirm callback is unauthenticated and
// can be called concurrently (two tabs, a retry, the scanner) — without the claim every caller
// reads PENDING and each mints a credit. Returns whether this caller won the claim.
func (r *Repo) ClaimAccountCreditTransaction(ctx context.Context, id string) (bool, error) {
	return r.credits.SetFieldsOne(ctx, pgdoc.M{"_id": id, "status": "PENDING"}, pgdoc.M{"status": "PROCESSING", "updatedAt": time.Now().UTC()}, nil)
}

// ReleaseAccountCreditTransaction puts a claimed (PROCESSING) deposit back to PENDING after a
// settle error, so the callback / scanner can retry it.
func (r *Repo) ReleaseAccountCreditTransaction(ctx context.Context, id string) error {
	_, err := r.credits.SetFieldsOne(ctx, pgdoc.M{"_id": id, "status": "PROCESSING"}, pgdoc.M{"status": "PENDING", "updatedAt": time.Now().UTC()}, nil)
	return err
}

// SaveAccountCreditTransaction upserts an account-credit transaction (create on a blank id,
// else replace by id) and returns the post-image with its id.
func (r *Repo) SaveAccountCreditTransaction(ctx context.Context, t *AccountCreditTransaction) (*AccountCreditTransaction, error) {
	now := time.Now().UTC()
	if t.CreatedAt == nil {
		t.CreatedAt = &now
	}
	t.UpdatedAt = &now
	if t.ID == "" {
		id, err := r.credits.InsertOne(ctx, t)
		if err != nil {
			return nil, err
		}
		t.ID = id
		return t, nil
	}
	if _, err := r.credits.Replace(ctx, t.ID, t); err != nil {
		return nil, err
	}
	return t, nil
}

// gatewayDTO maps one integration to its PaymentGatewayDTO per its thirdParty's PaymentOperations.
// ok=false for a non-payment-gateway integration (no factory).
func gatewayDTO(in integration) (PaymentGatewayDTO, bool) {
	dto := PaymentGatewayDTO{ID: in.ID, Name: in.Name, ThirdParty: in.ThirdParty, MinDeposit: minDeposit(in.Config)}
	switch in.ThirdParty {
	case "Stripe":
		// StripeOperations.getPaymentMethods → addCard/addFunds true; getMetadataForUI →
		// StripeMetadataUI{publicKey: config.publicKey}.
		dto.AddCard, dto.AddFunds = true, true
		dto.Metadata = map[string]any{"publicKey": strVal(in.Config, "publicKey")}
		return dto, true
	case ThirdPartyAirwallex:
		// Card gateway through the Airwallex Elements SDK: the console only needs the environment
		// (`demo` for a sandbox account, `prod` otherwise) — every secret stays server-side.
		dto.AddCard, dto.AddFunds = true, true
		env := "prod"
		if b, _ := in.Config["sandbox"].(bool); b {
			env = "demo"
		}
		dto.Metadata = map[string]any{"env": env}
		return dto, true
	case "BankTransfer":
		// Manual gateway: deposits only (no card). The UI metadata carries the operator-entered
		// transfer instructions (config.bankTransferInstructions) — this integration has no secrets.
		dto.AddFunds = true
		dto.Metadata = map[string]any{"instructions": strVal(in.Config, "bankTransferInstructions"), "minDeposit": dto.MinDeposit}
		return dto, true
	default:
		return PaymentGatewayDTO{}, false // gateway types beyond Stripe/BankTransfer land as their factories are added
	}
}

// minDeposit reads config.minDeposit as a number (defaults to 0).
func minDeposit(config map[string]any) json.Number {
	if config != nil {
		switch v := config["minDeposit"].(type) {
		case float64:
			return json.Number(strconv.FormatFloat(v, 'f', -1, 64))
		case int32:
			return json.Number(strconv.FormatInt(int64(v), 10))
		case int64:
			return json.Number(strconv.FormatInt(v, 10))
		case string:
			if v != "" {
				return json.Number(v)
			}
		}
	}
	return json.Number("0")
}

func strVal(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

// BankTransferInstructions returns the manual BankTransfer gateway's config.bankTransferInstructions
// (the bank details the customer wires to), or "" when no such gateway is configured.
func (r *Repo) BankTransferInstructions(ctx context.Context) (string, error) {
	var doc integration
	found, err := r.gateways.FindOne(ctx, pgdoc.M{"thirdParty": "BankTransfer"}, &doc)
	if err != nil || !found {
		return "", err
	}
	return strVal(doc.Config, "bankTransferInstructions"), nil
}
