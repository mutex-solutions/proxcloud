// Package order serves the order read (GET one order by id). An order belongs to a billing
// profile: the read answers only a caller who may read that profile, and answers the same 404
// "Order not found" for an absent order and for one the caller may not see.
package order

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type Repo struct{ orders *pgdoc.Store }

func NewRepo(db *pgdoc.DB) *Repo { return &Repo{orders: db.C("order")} }

// OrderStatus values. CREATED on create; PAID via payment.
const (
	OrderStatusCreated = "CREATED"
	OrderStatusPaid    = "PAID"
)

// OrderItem is one line of an Order. Money = decimal.
type OrderItem struct {
	Name       string           `json:"name,omitempty"`
	Type       string           `json:"type,omitempty"`
	NetAmount  *decimal.Decimal `json:"netAmount,omitempty"`
	TaxAmount  *decimal.Decimal `json:"taxAmount,omitempty"`
	ResourceID string           `json:"resourceId,omitempty"`
	Metadata   map[string]any   `json:"metadata,omitempty"`
}

// Order is the aggregate stored in the "order" collection.
type Order struct {
	ID               string           `json:"id,omitempty"`
	BillingProfileID string           `json:"billingProfileId,omitempty"`
	NetAmount        *decimal.Decimal `json:"netAmount,omitempty"`
	TaxAmount        *decimal.Decimal `json:"taxAmount,omitempty"`
	Items            []OrderItem      `json:"items,omitempty"`
	Status           string           `json:"status,omitempty"`
	CreatedAt        *time.Time       `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time       `json:"updatedAt,omitempty"`
}

// Create inserts a new order.
func (r *Repo) Create(ctx context.Context, o *Order) (*Order, error) {
	now := time.Now().UTC()
	o.CreatedAt, o.UpdatedAt = &now, &now
	o.ID = ""
	id, err := r.orders.InsertOne(ctx, o)
	if err != nil {
		return nil, err
	}
	o.ID = id
	return o, nil
}

// UpdateStatus flips an order's status (payment SUCCESS with an orderId flips the order PAID).
// No-op when the order is absent (the payment path treats it best-effort).
func (r *Repo) UpdateStatus(ctx context.Context, id, status string) error {
	now := time.Now().UTC()
	_, err := r.orders.SetByID(ctx, id, pgdoc.M{"status": status, "updatedAt": now}, nil)
	return err
}

// UpdateStatusForProfile flips an order's status ONLY when it belongs to the given billing profile —
// the payment path must not flip another profile's order (a foreign txn.OrderID → no match, no-op).
// No-op when the order is absent / owned by another profile.
func (r *Repo) UpdateStatusForProfile(ctx context.Context, id, billingProfileID, status string) error {
	now := time.Now().UTC()
	_, err := r.orders.SetFieldsOne(ctx,
		pgdoc.M{"_id": id, "billingProfileId": billingProfileID},
		pgdoc.M{"status": status, "updatedAt": now}, nil)
	return err
}

// ShouldMarkPaid reports whether a settlement should flip this order PAID: the order must exist,
// belong to the PAYING profile, and the settled gross must cover its total. The payment seam uses it
// to bind an order flip to the paying profile + amount (a foreign or short settlement returns false).
func ShouldMarkPaid(o *Order, payingProfileID string, gross decimal.Decimal) bool {
	return o != nil && o.BillingProfileID == payingProfileID && CoversOrderTotal(o, gross)
}

// CoversOrderTotal reports the settled gross is enough to pay the order's NetAmount+TaxAmount — the
// payment path must NOT mark an order PAID for less than it owes. Nil amounts count as zero.
func CoversOrderTotal(o *Order, gross decimal.Decimal) bool {
	if o == nil {
		return false
	}
	net := decimal.Zero
	if o.NetAmount != nil {
		net = *o.NetAmount
	}
	tax := decimal.Zero
	if o.TaxAmount != nil {
		tax = *o.TaxAmount
	}
	return gross.GreaterThanOrEqual(net.Add(tax))
}

// Get loads a typed order by id (nil when missing) — the payment seam uses it to check
// ownership (billingProfileId) + CoversOrderTotal before flipping the order PAID.
func (r *Repo) Get(ctx context.Context, id string) (*Order, error) {
	var o Order
	found, err := r.orders.Get(ctx, id, &o)
	if err != nil || !found {
		return nil, err
	}
	return &o, nil
}

// ProfileReader reports whether a user may read a billing profile. The order package cannot
// import org (org imports order), so the API wires the organization policy in through this.
type ProfileReader func(ctx context.Context, sub, billingProfileID string) (bool, error)

type Handler struct {
	repo    *Repo
	users   *user.Repo
	canRead ProfileReader
}

// NewHandler serves the order read. canRead decides who may see an order through its billing
// profile; without one every read answers 404.
func NewHandler(repo *Repo, users *user.Repo, canRead ProfileReader) *Handler {
	return &Handler{repo: repo, users: users, canRead: canRead}
}

func (h *Handler) Routes(r chi.Router) {
	r.Get("/orders/{id}", h.get)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	o, err := h.repo.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	// One answer for "no such order" and "not yours": an order id must not be confirmable by
	// someone who cannot read its billing profile.
	allowed := false
	if o != nil && o.BillingProfileID != "" && h.canRead != nil {
		if allowed, err = h.canRead(r.Context(), u.Sub, o.BillingProfileID); err != nil {
			httpx.WriteError(w, err)
			return
		}
	}
	if !allowed {
		httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "Order not found")
		return
	}
	httpx.OK(w, o)
}
