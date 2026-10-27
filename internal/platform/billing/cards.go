package billing

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// cards.go provides the add-card domains (CreditCard / CreditCardTransaction) + their
// repo writes. The card registration uses a Stripe SetupIntent (no charge); on success a
// CreditCard (tokenId = pm_*) is stored for future off-session collection.

// CreditCardTransaction is the "creditCardTransaction" document — the register-card transaction.
type CreditCardTransaction struct {
	ID               string           `json:"id,omitempty"`
	BillingProfileID string           `json:"billingProfileId,omitempty"`
	Status           string           `json:"status,omitempty"`
	ExternalID       string           `json:"externalId,omitempty"`
	PaymentGatewayID string           `json:"paymentGatewayId,omitempty"`
	InvoiceGatewayID string           `json:"invoiceGatewayId,omitempty"`
	Currency         string           `json:"currency,omitempty"`
	Amount           *decimal.Decimal `json:"amount,omitempty"`
	ExchangeRate     *decimal.Decimal `json:"exchangeRate,omitempty"`
	ErrorMessage     string           `json:"errorMessage,omitempty"`
	CreatedAt        *time.Time       `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time       `json:"updatedAt,omitempty"`
}

// CreditCard is the "creditCard" document — a stored card.
type CreditCard struct {
	ID                  string     `json:"id,omitempty"`
	BillingProfileID    string     `json:"billingProfileId,omitempty"`
	TokenID             string     `json:"tokenId,omitempty"`
	TokenExpirationDate *time.Time `json:"tokenExpirationDate,omitempty"`
	PanMasked           string     `json:"panMasked,omitempty"`
	PaymentGatewayID    string     `json:"paymentGatewayId,omitempty"`
	// Metadata: NO omitempty — the EMPTY map is stored on registration and omitempty
	// would silently drop it at insert. CreateCreditCard
	// guards nil → {} so a literal null is never stored.
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt *time.Time     `json:"createdAt,omitempty"`
	UpdatedAt *time.Time     `json:"updatedAt,omitempty"`
}

// MarshalJSON handles metadata null-omission: nil → omitted, an EMPTY map → "metadata":{}.
// The card store writes metadata = {} on registration, so the card
// JSON always carries "metadata":{} — a bare omitempty would drop the empty map and diverge.
func (c CreditCard) MarshalJSON() ([]byte, error) {
	type alias CreditCard
	if c.Metadata == nil {
		return json.Marshal(alias(c))
	}
	return json.Marshal(struct {
		alias
		Metadata map[string]any `json:"metadata"`
	}{alias(c), c.Metadata})
}

// SaveCreditCardTransaction upserts (insert on blank id, else replace by id).
func (r *Repo) SaveCreditCardTransaction(ctx context.Context, t *CreditCardTransaction) (*CreditCardTransaction, error) {
	now := time.Now().UTC()
	if t.CreatedAt == nil {
		t.CreatedAt = &now
	}
	t.UpdatedAt = &now
	if t.ID == "" {
		id, err := r.cardTxns.InsertOne(ctx, t)
		if err != nil {
			return nil, err
		}
		t.ID = id
		return t, nil
	}
	if _, err := r.cardTxns.Replace(ctx, t.ID, t); err != nil {
		return nil, err
	}
	return t, nil
}

// CreditCardTransactionByID loads one transaction, or (nil,nil).
func (r *Repo) CreditCardTransactionByID(ctx context.Context, id string) (*CreditCardTransaction, error) {
	var t CreditCardTransaction
	found, err := r.cardTxns.Get(ctx, id, &t)
	if err != nil || !found {
		return nil, err
	}
	return &t, nil
}

// CreateCreditCard inserts a stored card.
func (r *Repo) CreateCreditCard(ctx context.Context, c *CreditCard) error {
	now := time.Now().UTC()
	if c.CreatedAt == nil {
		c.CreatedAt = &now
	}
	c.UpdatedAt = &now
	if c.Metadata == nil {
		c.Metadata = map[string]any{} // never store a literal null (empty map is stored instead)
	}
	id, err := r.cards.InsertOne(ctx, c)
	if err != nil {
		return err
	}
	c.ID = id
	return nil
}

// CreditCardByID loads one stored card by id (→ 404). nil if absent.
func (r *Repo) CreditCardByID(ctx context.Context, id string) (*CreditCard, error) {
	var c CreditCard
	found, err := r.cards.Get(ctx, id, &c)
	if err != nil || !found {
		return nil, err
	}
	return &c, nil
}

// DeleteCreditCard removes a stored card scoped to its billing profile
// (matched by billing-profile id and card id; no-op if absent).
func (r *Repo) DeleteCreditCard(ctx context.Context, bpID, id string) error {
	_, err := r.cards.DeleteOne(ctx, pgdoc.M{"_id": id, "billingProfileId": bpID})
	return err
}

// CreditCardByIDAndBillingProfile loads a card scoped to its profile
// (matched by id and billing-profile id, used when setting the default card). nil if absent.
func (r *Repo) CreditCardByIDAndBillingProfile(ctx context.Context, id, bpID string) (*CreditCard, error) {
	var c CreditCard
	found, err := r.cards.FindOne(ctx, pgdoc.M{"_id": id, "billingProfileId": bpID}, &c)
	if err != nil || !found {
		return nil, err
	}
	return &c, nil
}

// CreditCardTransactionByIDAndBillingProfile loads a card transaction scoped to its profile
// (matched by id and billing-profile id). nil if absent.
func (r *Repo) CreditCardTransactionByIDAndBillingProfile(ctx context.Context, id, bpID string) (*CreditCardTransaction, error) {
	var t CreditCardTransaction
	found, err := r.cardTxns.FindOne(ctx, pgdoc.M{"_id": id, "billingProfileId": bpID}, &t)
	if err != nil || !found {
		return nil, err
	}
	return &t, nil
}

// CurrentCreditCard returns the profile's non-expired card
// (tokenExpirationDate after the first of this month), preferring the profile's defaultCardId,
// else the first. nil if none. (CreditCard has no status field — validity is by expiry.)
func (r *Repo) CurrentCreditCard(ctx context.Context, bpID, defaultCardID string, now time.Time) (*CreditCard, error) {
	firstOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var cards []CreditCard
	err := r.cards.Find(ctx, pgdoc.M{
		"billingProfileId":    bpID,
		"tokenExpirationDate": pgdoc.M{"$gt": firstOfMonth},
	}, &cards)
	if err != nil {
		return nil, err
	}
	if len(cards) == 0 {
		return nil, nil
	}
	if defaultCardID != "" {
		for i := range cards {
			if strings.EqualFold(cards[i].ID, defaultCardID) {
				return &cards[i], nil
			}
		}
	}
	return &cards[0], nil
}

// CreditCardsByBillingProfile lists a profile's stored cards.
func (r *Repo) CreditCardsByBillingProfile(ctx context.Context, bpID string) ([]CreditCard, error) {
	return findTyped[CreditCard](ctx, r.cards, pgdoc.M{"billingProfileId": bpID})
}
