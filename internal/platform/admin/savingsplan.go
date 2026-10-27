package admin

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// savingsplan.go implements the MUTATIONS of the savings-plan surface (/api/v1/admin/savings-plans) —
// create / update / delete — plus the read still missing on the Go admin surface
// (GET /available/{billingProfileId}). The plain list (GET /savings-plans) and the by-id read
// (GET /savings-plans/{id}) are ALREADY registered in handler.go (listRaw / rawByID); they are NOT
// re-registered here (the faithful-DTO upgrade of those reads is out of scope this pass).
//
// Endpoints:
//   - create: store the plan → respond with the saved plan.         ADMIN_SAVINGS_PLAN_MANAGE
//   - update: load-or-404 → overwrite 7 mutable fields → store.      ADMIN_SAVINGS_PLAN_MANAGE
//   - delete: load-or-404 → delete by id → empty body.              ADMIN_SAVINGS_PLAN_MANAGE
//   - available-for-billing-profile: load the available plans, keep
//     those eligible for the billing profile → list.                ADMIN_SAVINGS_PLAN_READ
//
// The bare list and by-id reads are already registered elsewhere.
//
// create/update write audit events, and delete should write an admin audit event too —
// deferred this pass (// TODO(audit)); state + response are faithful.
//
// The admin endpoints return the RAW SavingsPlan document, NOT a
// client DTO — but the raw JSON shape (id not _id, money as a JSON number, filters:[] always,
// no _class) is exactly what billing.SavingsPlanToDto already emits, so the response reuses it.

const (
	savingsPlanManagePerm = "admin:savings_plan:manage"
	savingsPlanReadPerm   = "admin:savings_plan:read"
	savingsPlanCollection = "savingsPlan"
)

// routeSavingsPlan registers ONLY the new SavingsPlan admin routes (the mutations + the available
// read). The bare list and by-id reads are already registered in handler.go and are skipped here.
func (h *Handler) routeSavingsPlan(r chi.Router) {
	r.Post("/savings-plans", h.savingsPlanCreate)
	// `available` is a STATIC sibling of the `{id}` param at the same tree position — chi routes
	// the literal segment before the wildcard, so no conflict with GET /savings-plans/{id}.
	r.Get("/savings-plans/available/{billingProfileId}", h.savingsPlanAvailable)
	r.Put("/savings-plans/{id}", h.savingsPlanUpdate)
	r.Delete("/savings-plans/{id}", h.savingsPlanDelete)
}

// savingsPlanReq holds the SavingsPlan's mutable request-body fields. Money is decimal.Decimal so
// it stores as a decimal string in jsonb (pgdoc codec) and round-trips as a JSON number — never a
// float. shopspring decimal decodes both JSON numbers and strings.
type savingsPlanReq struct {
	Name            string                              `json:"name"`
	Available       bool                                `json:"available"`
	Description     string                              `json:"description"`
	Targets         []billing.SavingsPlanTarget         `json:"targets"`
	SavingSchedule  []savingsPlanScheduleReq            `json:"savingSchedule"`
	AccessMode      string                              `json:"accessMode"`
	BillingProfiles []billing.SavingsPlanBillingProfile `json:"billingProfiles"`
}

type savingsPlanScheduleReq struct {
	DurationMonths int                  `json:"durationMonths"`
	MaxAmount      *decimal.Decimal     `json:"maxAmount"`
	NoUpfrontTiers []savingsPlanTierReq `json:"noUpfrontTiers"`
	UpfrontTiers   []savingsPlanTierReq `json:"upfrontTiers"`
}

type savingsPlanTierReq struct {
	StartAmount *decimal.Decimal `json:"startAmount"`
	Discount    *decimal.Decimal `json:"discount"`
}

// toDomain builds the stored billing.SavingsPlan from the request. Optional blank strings are left
// blank → omitted on the wire (`omitempty`), so a null field is dropped.
func (req savingsPlanReq) toDomain() billing.SavingsPlan {
	return billing.SavingsPlan{
		Name:            req.Name,
		Available:       req.Available,
		Description:     req.Description,
		Targets:         req.Targets,
		SavingSchedule:  schedulesToDomain(req.SavingSchedule),
		AccessMode:      req.AccessMode,
		BillingProfiles: req.BillingProfiles,
	}
}

func schedulesToDomain(in []savingsPlanScheduleReq) []billing.SavingsPlanSchedule {
	if in == nil {
		return nil
	}
	out := make([]billing.SavingsPlanSchedule, 0, len(in))
	for i := range in {
		out = append(out, billing.SavingsPlanSchedule{
			DurationMonths: in[i].DurationMonths,
			MaxAmount:      in[i].MaxAmount,
			NoUpfrontTiers: tiersToDomain(in[i].NoUpfrontTiers),
			UpfrontTiers:   tiersToDomain(in[i].UpfrontTiers),
		})
	}
	return out
}

func tiersToDomain(in []savingsPlanTierReq) []billing.SavingsPlanTier {
	if in == nil {
		return nil
	}
	out := make([]billing.SavingsPlanTier, 0, len(in))
	for i := range in {
		out = append(out, billing.SavingsPlanTier{StartAmount: in[i].StartAmount, Discount: in[i].Discount})
	}
	return out
}

// savingsPlanCreate stores a new plan and responds with the saved plan. ADMIN_SAVINGS_PLAN_MANAGE.
func (h *Handler) savingsPlanCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, savingsPlanManagePerm) {
		return
	}
	var req savingsPlanReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	plan := req.toDomain()
	saved, err := h.repo.InsertSavingsPlan(r.Context(), savingsPlanCollection, plan)
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a plan is created.
	httpx.OK(w, billing.SavingsPlanToDto(saved))
}

// savingsPlanUpdate loads the plan (404 if absent), overwrites the 7 mutable fields
// (available/name/description/targets/savingSchedule/accessMode/billingProfiles), and stores it.
// ADMIN_SAVINGS_PLAN_MANAGE. id/createdAt/updatedAt are preserved (not overwritten).
func (h *Handler) savingsPlanUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, savingsPlanManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req savingsPlanReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.SavingsPlanByID(r.Context(), savingsPlanCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Savings plan not found"))
		return
	}
	// Overwrite exactly the 7 fields the update copies; keep id + audit timestamps.
	before, _ := h.repo.FindDoc(r.Context(), savingsPlanCollection, id) // raw pre-mutation snapshot for the audit diff
	existing.Name = req.Name
	existing.Available = req.Available
	existing.Description = req.Description
	existing.Targets = req.Targets
	existing.SavingSchedule = schedulesToDomain(req.SavingSchedule)
	existing.AccessMode = req.AccessMode
	existing.BillingProfiles = req.BillingProfiles
	if err := h.repo.ReplaceSavingsPlan(r.Context(), savingsPlanCollection, id, *existing); httpx.WriteError(w, err) {
		return
	}
	// UPDATE audit: field-level diff (the audit middleware compares the before/after snapshots).
	after, _ := h.repo.FindDoc(r.Context(), savingsPlanCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, billing.SavingsPlanToDto(existing))
}

// savingsPlanDelete loads the plan (404 if absent) and deletes it by id. The
// handler returns an empty HTTP 200
// body. ADMIN_SAVINGS_PLAN_MANAGE.
func (h *Handler) savingsPlanDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, savingsPlanManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.SavingsPlanByID(r.Context(), savingsPlanCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Savings plan not found"))
		return
	}
	if _, err := h.repo.DeleteSavingsPlan(r.Context(), savingsPlanCollection, id); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a plan is deleted.
	w.WriteHeader(http.StatusOK) // void → HTTP 200, empty body
}

// savingsPlanAvailable loads the available plans and keeps those eligible for the given
// billing profile → list. ADMIN_SAVINGS_PLAN_READ.
func (h *Handler) savingsPlanAvailable(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, savingsPlanReadPerm) {
		return
	}
	bpID := chi.URLParam(r, "billingProfileId")
	plans, err := h.repo.AvailableSavingsPlans(r.Context(), savingsPlanCollection)
	if httpx.WriteError(w, err) {
		return
	}
	out := make([]billing.SavingsPlanDto, 0, len(plans))
	for i := range plans {
		if isEligibleForBillingProfile(&plans[i], bpID) {
			out = append(out, billing.SavingsPlanToDto(&plans[i]))
		}
	}
	httpx.List(w, out)
}

// isEligibleForBillingProfile checks eligibility: a PUBLIC (or null
// accessMode) plan is eligible for everyone; a SCOPED plan with a null billingProfiles list is also
// eligible for everyone; otherwise eligible iff some billingProfiles entry matches bpId.
func isEligibleForBillingProfile(p *billing.SavingsPlan, billingProfileID string) bool {
	if p.AccessMode == "" || p.AccessMode == "PUBLIC" {
		return true
	}
	if p.BillingProfiles == nil {
		return true
	}
	for i := range p.BillingProfiles {
		if p.BillingProfiles[i].BillingProfileID == billingProfileID {
			return true
		}
	}
	return false
}
