package adminapi

// bills.go serves /admin-api/v1/bills. Read-only.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

type apiBillItem struct {
	Name         string         `json:"name,omitempty"`
	ResourceID   string         `json:"resource_id,omitempty"`
	ProjectID    string         `json:"project_id,omitempty"`
	ResourceType string         `json:"resource_type,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"`
	Currency     string         `json:"currency,omitempty"`
	Amount       json.Number    `json:"amount,omitempty"`
	CreatedAt    *time.Time     `json:"created_at,omitempty"`
	UpdatedAt    *time.Time     `json:"updated_at,omitempty"`
}

type apiBill struct {
	ID               string        `json:"id,omitempty"`
	Status           string        `json:"status,omitempty"`
	Currency         string        `json:"currency,omitempty"`
	BillingProfileID string        `json:"billing_profile_id,omitempty"`
	Items            []apiBillItem `json:"items"` // mapItems: null → []
	StartDate        *time.Time    `json:"start_date,omitempty"`
	EndDate          *time.Time    `json:"end_date,omitempty"`
	CreatedAt        *time.Time    `json:"created_at,omitempty"`
	UpdatedAt        *time.Time    `json:"updated_at,omitempty"`
}

func mapBill(b *pricing.Bill) apiBill {
	items := make([]apiBillItem, 0, len(b.Items))
	for i := range b.Items {
		it := &b.Items[i]
		items = append(items, apiBillItem{
			Name: it.Name, ResourceID: it.ResourceID, ProjectID: it.ProjectID,
			ResourceType: string(it.ResourceType), Attributes: it.Metadata,
			Currency: it.Currency, Amount: json.Number(it.NetAmount.String()),
			CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
		})
	}
	out := apiBill{
		ID: b.ID, Status: string(b.Status), Currency: b.InvoiceCurrency,
		BillingProfileID: b.BillingProfileID, Items: items,
		CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
	}
	if b.BillingCycle != nil {
		out.StartDate, out.EndDate = b.BillingCycle.StartDate, b.BillingCycle.EndDate
	}
	return out
}

func (h *Handler) routeBills(r chi.Router) {
	r.Get("/bills", h.billsList)
	r.Get("/bills/{id}", h.billGet)
}

func (h *Handler) billsList(w http.ResponseWriter, r *http.Request) {
	req, ok := listParams(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := pgdoc.M{}
	if v := q.Get("billing_profile_id"); v != "" {
		f["billingProfileId"] = v
	}
	if v := q.Get("status"); v != "" {
		f["status"] = v
	}
	if v := q.Get("start_date"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			badRequest(w, "Invalid start_date")
			return
		}
		f["billingCycle.startDate"] = pgdoc.M{"$gte": t}
	}
	if v := q.Get("end_date"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			badRequest(w, "Invalid end_date")
			return
		}
		f["billingCycle.endDate"] = pgdoc.M{"$lte": t}
	}
	bills, err := findPage[pricing.Bill](r.Context(), h.db.C("bill"), f, req)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	page, next := pageOut(req, bills, func(b pricing.Bill) string { return b.ID })
	// include_items=false (the default) drops the items array before mapping → items: [].
	// (The former driver-level projection is gone; full docs are fetched.)
	includeItems := q.Get("include_items") == "true"
	out := make([]apiBill, 0, len(page))
	for i := range page {
		if !includeItems {
			page[i].Items = nil
		}
		out = append(out, mapBill(&page[i]))
	}
	writeList(w, out, next)
}

func (h *Handler) billGet(w http.ResponseWriter, r *http.Request) {
	var b pricing.Bill
	if found, err := h.db.C("bill").Get(r.Context(), chi.URLParam(r, "id"), &b); err != nil || !found {
		apiNotFound(w)
		return
	}
	writeEntity(w, mapBill(&b))
}
