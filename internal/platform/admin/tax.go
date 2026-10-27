package admin

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// tax.go implements the MUTATIONS of the tax surface (/api/v1/admin/tax). The list +
// by-id reads are ALREADY in handler.go (h.taxList / h.taxByID, gated ADMIN_TAX_READ) and are NOT
// re-registered here. create/update/delete are pure datastore writes on the taxRate
// table (no external side effects), so the whole surface is in-scope and
// handled via the crud.go helpers.
//
// TaxRate persisted fields (no createdAt/updatedAt on the entity): rateLevels
// [{level:int,percentage:int}], level (BUSINESS_ONLY|CONSUMERS_ONLY|ALL), name, state, country,
// startDate, endDate, accessMode (PUBLIC|SCOPED), startDateEnabled, endDateEnabled. The FE also POSTs
// companyConstraint/allCountries — NOT entity fields, so they are dropped on serialization; we drop them too.
// create/update emit audit events, and delete should write an admin audit event too — the group
// audit middleware already auto-emits an ADMIN_AREA/PLATFORM event per 2xx mutation (TODO(audit):
// field-level before/after diff deferred, matching the other mutations).

const taxPerm = "admin:tax:manage"

const taxCollection = "taxRate"

// routeTax registers the TaxRate admin mutation routes. The bare list + by-id reads are in handler.go.
func (h *Handler) routeTax(r chi.Router) {
	r.Post("/tax", h.taxCreate)
	r.Put("/tax/{id}", h.taxUpdate)
	r.Delete("/tax/{id}", h.taxDelete)
}

// taxLevelReq holds a tax level (primitive ints level + percentage).
type taxLevelReq struct {
	Level      int `json:"level"`
	Percentage int `json:"percentage"`
}

// taxRateReq holds the mutable fields of TaxRate. country/startDate/endDate/level
// are pointers so a null vs an absent field can drive the null omission (a stored null
// still matches an absent-country filter on the read path).
type taxRateReq struct {
	Name             string        `json:"name"`
	State            string        `json:"state"`
	Country          *string       `json:"country"`
	Level            *string       `json:"level"`
	AccessMode       string        `json:"accessMode"`
	RateLevels       []taxLevelReq `json:"rateLevels"`
	StartDate        *time.Time    `json:"startDate"`
	EndDate          *time.Time    `json:"endDate"`
	StartDateEnabled bool          `json:"startDateEnabled"`
	EndDateEnabled   bool          `json:"endDateEnabled"`
}

// doc builds the stored JSON for the TaxRate fields. Optional fields are omitted when blank/nil so
// the round-tripped JSON drops nulls. rateLevels is always present
// (a tax rate without levels is meaningless; an empty list round-trips as []).
func (req taxRateReq) doc() pgdoc.M {
	rl := make([]pgdoc.M, 0, len(req.RateLevels))
	for _, l := range req.RateLevels {
		rl = append(rl, pgdoc.M{"level": l.Level, "percentage": l.Percentage})
	}
	d := pgdoc.M{
		"rateLevels":       rl,
		"name":             req.Name,
		"accessMode":       req.AccessMode,
		"startDateEnabled": req.StartDateEnabled,
		"endDateEnabled":   req.EndDateEnabled,
	}
	if req.Level != nil {
		d["level"] = *req.Level
	}
	if req.State != "" {
		d["state"] = req.State
	}
	if req.Country != nil && *req.Country != "" {
		d["country"] = *req.Country
	}
	if req.StartDate != nil {
		d["startDate"] = *req.StartDate
	}
	if req.EndDate != nil {
		d["endDate"] = *req.EndDate
	}
	return d
}

// taxCreate stores a new tax rate (level required) and responds with the saved rate. The store
// assigns the _id (insert with no id).
func (h *Handler) taxCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, taxPerm) {
		return
	}
	var req taxRateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	if req.Level == nil || *req.Level == "" {
		httpx.WriteError(w, httpx.BadRequest("Tax rate level must not be null"))
		return
	}
	saved, err := h.repo.InsertDoc(r.Context(), taxCollection, req.doc())
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): CREATE TAX_RATE audit (field-level snapshot)
	httpx.OK(w, shapeDoc(saved))
}

// taxUpdate loads the tax rate (404 if absent), overwrites name, rateLevels, level, country,
// startDate, endDate, startDateEnabled, endDateEnabled, accessMode (state is NOT touched), and
// stores it. Drop the updated keys first so an omitted/null field becomes absent (nulls omitted).
func (h *Handler) taxUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, taxPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req taxRateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), taxCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Tax rate not found"))
		return
	}
	before := maps.Clone(existing)
	for _, k := range []string{"name", "rateLevels", "level", "country", "startDate", "endDate", "startDateEnabled", "endDateEnabled", "accessMode"} {
		delete(existing, k)
	}
	ud := req.doc()
	delete(ud, "state") // update never changes state — leave the persisted state untouched.
	for k, v := range ud {
		existing[k] = v
	}
	if err := h.repo.ReplaceDoc(r.Context(), taxCollection, id, existing); httpx.WriteError(w, err) {
		return
	}
	// UPDATE audit: field-level diff (the audit middleware compares the before/after snapshots).
	after, _ := h.repo.FindDoc(r.Context(), taxCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(existing))
}

// taxDelete loads the tax rate (404 if absent): if accessMode==SCOPED and any billingProfile
// references it (taxConfiguration.taxRuleId == id) → 400; else delete → 202 Accepted (no body).
func (h *Handler) taxDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, taxPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), taxCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Tax rate not found"))
		return
	}
	if mode, _ := existing["accessMode"].(string); mode == "SCOPED" {
		// Count billing profiles whose taxConfiguration.taxRuleId equals this id.
		n, err := h.repo.CountBy(r.Context(), "billingProfile", pgdoc.M{"taxConfiguration.taxRuleId": id})
		if httpx.WriteError(w, err) {
			return
		}
		if n > 0 {
			httpx.WriteError(w, httpx.BadRequest(fmt.Sprintf("Cannot delete tax rate because it is used in %d billing profiles", n)))
			return
		}
	}
	if _, err := h.repo.DeleteDoc(r.Context(), taxCollection, id); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a tax rate is deleted.
	w.WriteHeader(http.StatusAccepted)
}
