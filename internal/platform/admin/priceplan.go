package admin

// priceplan.go implements the MUTATIONS (and the missing reads) of the price-plan surface
// (/api/v1/admin/price-plan) — PricePlan create/update/delete + resource-types,
// PricePlanRule create/list/update/delete/usage + clone, and PricePlan clone.
//
// The plain reads that already live in handler.go are intentionally NOT re-registered here:
//   - GET /price-plan            (list, h.listRaw)
//   - GET /price-plan/{id}       (by-id 404, h.rawByID)
//   - GET /price-plan/rule/{id}  (rule by-id 404, h.rawByID — this IS the single-rule read)
//
// Follows the custommenu.go / promotioncode.go reference: id-aware* CRUD via the crud.go +
// priceplan_repo.go helpers, exact perms / error strings / response envelopes, `_id`→`id`
// shaping on the way out. (*PricePlan/PricePlanRule use a String id stored as a String `_id`
// like savingsPlan, so the repo helpers match `_id` as a raw string — see priceplan_repo.go.)
//
// Perms: read = admin:price_plan:read; mutate = admin:price_plan:manage. create/update/delete write
// audit events — deferred this pass (// TODO(audit)); the persisted state + response are faithful.
//
// EXTERNAL/DEFERRED POINTS:
//   - GET /price-plan/resource-types and GET /price-plan/{id}/resource-types return the local
//     billing-resource catalog (the resource types + attribute schemas the cloud provider bills).
//   - GET /price-plan/rule/{id}/usage would sum applied money across OPEN bills (a decimal-money
//     total) — per the money rule we do NOT recompute money here; the rule-404 pre-step is
//     faithful, the sum is not wired (returns 0).

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const (
	pricePlanReadPerm   = "admin:price_plan:read"
	pricePlanManagePerm = "admin:price_plan:manage"

	pricePlanCollection     = "pricePlan"
	pricePlanRuleCollection = "pricePlanRule"
)

// routePricePlan registers the price-plan admin endpoints NOT already in handler.go. chi
// requires a single param name at a given path position, so every route under /price-plan/{...}
// reuses `{id}` and every route under /price-plan/rule/{...} reuses `{id}` (handler.go already
// registered /price-plan/{id} and /price-plan/rule/{id} with the name `id`). The path variables
// are pricePlanId/ruleId, but the value is read by the same name here.
func (h *Handler) routePricePlan(r chi.Router) {
	// PricePlan mutations.
	r.Post("/price-plan", h.pricePlanCreate)
	r.Put("/price-plan/{id}", h.pricePlanUpdate)
	r.Delete("/price-plan/{id}", h.pricePlanDelete)
	r.Post("/price-plan/clone", h.pricePlanClone)

	// PricePlan resource-types (local billing-resource catalog).
	r.Get("/price-plan/resource-types", h.pricePlanResourceTypesAll)
	r.Get("/price-plan/{id}/resource-types", h.pricePlanResourceTypes)

	// PricePlanRule reads/mutations. (GET /price-plan/rule/{id} is the single-rule read, already
	// registered in handler.go.) `/price-plan/rule/clone` is a static sibling of `/rule/{id}`.
	r.Get("/price-plan/{id}/rule", h.pricePlanListRules)
	r.Post("/price-plan/rule", h.pricePlanRuleCreate)
	r.Post("/price-plan/rule/clone", h.pricePlanRuleClone)
	r.Put("/price-plan/rule/{id}", h.pricePlanRuleUpdate)
	r.Delete("/price-plan/rule/{id}", h.pricePlanRuleDelete)
	r.Get("/price-plan/rule/{id}/usage", h.pricePlanRuleUsage)
}

// ── PricePlan ───────────────────────────────────────────────────────────────────────────────────

// pricePlanReq holds the PricePlan's mutable request-body fields. serviceProviders is
// passed through as its decoded JSON (a list of {serviceId,...}); accessMode is a string enum
// (PUBLIC / SCOPED). A pointer distinguishes "absent" from a zero value
// (accessMode: create defaults null→PUBLIC; update only overwrites when non-null).
type pricePlanReq struct {
	Name             string           `json:"name"`
	Enabled          bool             `json:"enabled"`
	AccessMode       *string          `json:"accessMode"`
	ServiceProviders *json.RawMessage `json:"serviceProviders"`
}

// pricePlanNotFound is the exact 404 for a missing price plan
// ("Could not find price plan with id %s", interpolated).
func pricePlanNotFound(id string) *httpx.HTTPError {
	return httpx.NotFound(fmt.Sprintf("Could not find price plan with id %s", id))
}

// pricePlanCreate creates a price plan: a null accessMode defaults to PUBLIC, then save and return it.
func (h *Handler) pricePlanCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pricePlanManagePerm) {
		return
	}
	var req pricePlanReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	doc := req.doc()
	// A null accessMode defaults to PUBLIC.
	if req.AccessMode != nil && *req.AccessMode != "" {
		doc["accessMode"] = *req.AccessMode
	} else {
		doc["accessMode"] = "PUBLIC"
	}
	saved, err := h.repo.InsertPricePlanDoc(r.Context(), doc)
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a price plan is created.
	httpx.OK(w, shapeDoc(saved))
}

// pricePlanUpdate loads the plan (404 if absent), sets name/enabled/serviceProviders, overwrites
// accessMode only when the request supplies it (non-null), then saves and returns it.
func (h *Handler) pricePlanUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pricePlanManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req pricePlanReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.PricePlanByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, pricePlanNotFound(id))
		return
	}
	before := maps.Clone(existing)
	// Set: name, enabled, serviceProviders (always); accessMode only when non-null. Drop the
	// overwritten optional keys first so an omitted field becomes absent (nulls omitted).
	for _, k := range []string{"name", "serviceProviders"} {
		delete(existing, k)
	}
	for k, v := range req.doc() {
		existing[k] = v
	}
	if req.AccessMode != nil && *req.AccessMode != "" {
		existing["accessMode"] = *req.AccessMode
	}
	if err := h.repo.ReplacePricePlanDoc(r.Context(), id, existing); httpx.WriteError(w, err) {
		return
	}
	// Record an audit event with a field-level diff of the before/after documents.
	after, _ := h.repo.PricePlanByID(r.Context(), id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(existing))
}

// pricePlanDelete runs the use-in-external-services / use-in-projects guards (both 400), then loads
// the plan (404 if absent), cascade-deletes the plan's rules, deletes the plan, and returns success.
func (h *Handler) pricePlanDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pricePlanManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	// guard 1: reject with 400 when the plan is referenced by any external service.
	usedInExt, err := h.repo.PricePlanUsedInExternalServices(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if usedInExt {
		httpx.WriteError(w, httpx.BadRequest("Price plan is in use in external services"))
		return
	}
	// guard 2: reject with 400 when the plan is referenced by any project.
	usedInProj, err := h.repo.PricePlanUsedInProjects(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if usedInProj {
		httpx.WriteError(w, httpx.BadRequest("Price plan is in use in projects"))
		return
	}
	existing, err := h.repo.PricePlanByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, pricePlanNotFound(id))
		return
	}
	// TODO(audit): write an admin audit event when a price plan is deleted.
	// cascade: delete every rule for the plan, then delete the plan.
	if err := h.repo.DeletePricePlanRulesByPlanID(r.Context(), id); httpx.WriteError(w, err) {
		return
	}
	if _, err := h.repo.DeletePricePlanDoc(r.Context(), id); httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, "Successful operation")
}

// billingResourceTypeDTO is the BillingResourceType wire shape:
// {resourceType, attributes:[{type,name,isUsage}]}. The pricing structs have no json tags, so the
// catalog is mapped through this DTO for the admin PricePlanRule form.
type billingResourceTypeDTO struct {
	ResourceType string                 `json:"resourceType"`
	Attributes   []resourceAttributeDTO `json:"attributes"`
}

type resourceAttributeDTO struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	IsUsage *bool  `json:"isUsage,omitempty"`
}

// billingResourceTypeCatalog maps the billingresource catalog to the wire DTOs.
func billingResourceTypeCatalog() []billingResourceTypeDTO {
	cat := billingresource.Catalog()
	out := make([]billingResourceTypeDTO, 0, len(cat))
	for _, t := range cat {
		attrs := make([]resourceAttributeDTO, 0, len(t.Attributes))
		for _, a := range t.Attributes {
			attrs = append(attrs, resourceAttributeDTO{Type: a.Type, Name: a.Name, IsUsage: a.IsUsage})
		}
		out = append(out, billingResourceTypeDTO{ResourceType: t.ResourceType, Attributes: attrs})
	}
	return out
}

// pricePlanResourceTypesAll returns the global billing-resource-type catalog (the resource types +
// attribute schemas the cloud provider bills). Backs the price-plan-rule form.
func (h *Handler) pricePlanResourceTypesAll(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pricePlanReadPerm) {
		return
	}
	httpx.List(w, billingResourceTypeCatalog())
}

// pricePlanResourceTypes returns the same catalog for a specific plan (scoped by the plan's external
// services; OpenStack's billing-resource set is the one catalog).
func (h *Handler) pricePlanResourceTypes(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pricePlanReadPerm) {
		return
	}
	httpx.List(w, billingResourceTypeCatalog())
}

// doc builds the stored JSON for a PricePlan mutation (name + enabled always; serviceProviders only
// when present — a null serviceProviders is omitted, not emitted as []). accessMode is set
// by the callers (create defaults PUBLIC; update overwrites only when supplied).
func (req pricePlanReq) doc() pgdoc.M {
	d := pgdoc.M{"name": req.Name, "enabled": req.Enabled}
	if req.ServiceProviders != nil {
		d["serviceProviders"] = rawJSON(*req.ServiceProviders)
	}
	return d
}

// pricePlanClone clones price plans: for each item, load the source (404 if absent), create a
// disabled copy (name = newName or "<src> (Copy)", same accessMode + serviceProviders), and — when
// includeRules — clone all the source's rules into the new plan. Returns the per-item results.
func (h *Handler) pricePlanClone(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pricePlanManagePerm) {
		return
	}
	var req clonePricePlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	results := []clonedPricePlanResult{}
	for _, item := range req.PricePlans {
		src, err := h.repo.PricePlanByID(r.Context(), item.PricePlanID)
		if httpx.WriteError(w, err) {
			return
		}
		if src == nil {
			httpx.WriteError(w, pricePlanNotFound(item.PricePlanID))
			return
		}
		srcName, _ := src["name"].(string)
		newName := strings.TrimSpace(item.NewName)
		if newName == "" {
			newName = srcName + " (Copy)"
		}
		// Build the new (disabled) plan copying accessMode + serviceProviders from the source.
		newDoc := pgdoc.M{"name": newName, "enabled": false}
		if v, ok := src["accessMode"]; ok {
			newDoc["accessMode"] = v
		} else {
			newDoc["accessMode"] = "PUBLIC" // a null accessMode defaults to PUBLIC on create.
		}
		if v, ok := src["serviceProviders"]; ok {
			newDoc["serviceProviders"] = v
		}
		saved, err := h.repo.InsertPricePlanDoc(r.Context(), newDoc)
		if httpx.WriteError(w, err) {
			return
		}
		// TODO(audit): write an admin audit event when the cloned plan is created.
		newID, _ := pricePlanDocID(saved)
		rulesCloned := 0
		if item.includeRules() {
			sourceRules, err := h.repo.PricePlanRulesByPlanID(r.Context(), item.PricePlanID)
			if httpx.WriteError(w, err) {
				return
			}
			for _, rule := range sourceRules {
				n, err := h.cloneSingleRule(r, rule, newID, "", false)
				if err != nil {
					httpx.WriteError(w, err)
					return
				}
				if n != "" {
					rulesCloned++
				}
			}
		}
		results = append(results, clonedPricePlanResult{
			SourcePricePlanID: item.PricePlanID,
			NewPricePlanID:    newID,
			NewPricePlanName:  newName,
			RulesCloned:       rulesCloned,
		})
	}
	httpx.OK(w, clonePricePlanResponse{ClonedPricePlans: results})
}

// ── PricePlanRule ─────────────────────────────────────────────────────────────────────────────────

// pricePlanRuleReq holds the PricePlanRule's mutable request-body fields. prices /
// filters / modifiers are passed through as their decoded JSON (these carry tier money — see the
// money fidelity note, deferred). applyMethod is a string enum.
type pricePlanRuleReq struct {
	Name         string           `json:"name"`
	TimeUnit     string           `json:"timeUnit"`
	ResourceType string           `json:"resourceType"`
	PricePlanID  string           `json:"pricePlanId"`
	ApplyMethod  *string          `json:"applyMethod"`
	Prices       *json.RawMessage `json:"prices"`
	Filters      *json.RawMessage `json:"filters"`
	Modifiers    *json.RawMessage `json:"modifiers"`
}

// validatePricePlanRule validates a rule: name/timeUnit/resourceType
// must be non-null, and any tier with both from+to must have to >= from. The tier check runs against
// the decoded prices (when present). Exact messages.
func validatePricePlanRule(req pricePlanRuleReq) *httpx.HTTPError {
	if req.Name == "" {
		return httpx.BadRequest("Name must not be null")
	}
	if req.TimeUnit == "" {
		return httpx.BadRequest("Time unit must not be null")
	}
	if req.ResourceType == "" {
		return httpx.BadRequest("Resource type must not be null")
	}
	if req.Prices != nil {
		if err := validateTiers(*req.Prices); err != nil {
			return err
		}
	}
	return nil
}

// validateTiers checks every price's tiers for to >= from (else a 400 tier-ordering error). Tier
// from/to are compared numerically; non-numeric / absent bounds are skipped (compared only when
// both are non-null).
func validateTiers(pricesJSON json.RawMessage) *httpx.HTTPError {
	var prices []struct {
		Tiers []struct {
			From *json.Number `json:"from"`
			To   *json.Number `json:"to"`
		} `json:"tiers"`
	}
	if err := json.Unmarshal(pricesJSON, &prices); err != nil {
		// Malformed prices are left to the decode/store path; not a tier-ordering error.
		return nil
	}
	for _, p := range prices {
		for _, t := range p.Tiers {
			if t.From == nil || t.To == nil {
				continue
			}
			from, ferr := t.From.Float64()
			to, terr := t.To.Float64()
			if ferr == nil && terr == nil && to < from {
				return httpx.BadRequest("Price tier 'to' must be greater than or equal to 'from'")
			}
		}
	}
	return nil
}

// pricePlanRuleCreate validates the rule, saves it, and returns it. Requires manage permission.
func (h *Handler) pricePlanRuleCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pricePlanManagePerm) {
		return
	}
	var req pricePlanRuleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	if verr := validatePricePlanRule(req); verr != nil {
		httpx.WriteError(w, verr)
		return
	}
	saved, err := h.repo.InsertPricePlanRuleDoc(r.Context(), req.doc())
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a rule is created.
	httpx.OK(w, shapeDoc(saved))
}

// pricePlanListRules lists the rules for a plan → list envelope. The default for a null applyMethod
// (ADD_TO_TOTAL, persisted) is applied on read.
func (h *Handler) pricePlanListRules(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pricePlanReadPerm) {
		return
	}
	rules, err := h.repo.PricePlanRulesByPlanID(r.Context(), chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	for i := range rules {
		h.applyRuleDefault(r, rules[i])
		shapeDoc(rules[i])
	}
	httpx.List(w, rules)
}

// pricePlanRuleUpdate validates, loads the rule (404 if absent), sets name/prices/filters/
// modifiers/resourceType/applyMethod/timeUnit, then saves and returns it.
func (h *Handler) pricePlanRuleUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pricePlanManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req pricePlanRuleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	if verr := validatePricePlanRule(req); verr != nil {
		httpx.WriteError(w, verr)
		return
	}
	existing, err := h.repo.PricePlanRuleByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("PricePlanRule not found. "))
		return
	}
	before := maps.Clone(existing)
	// Overwrite: name, prices, filters, modifiers, resourceType, applyMethod, timeUnit (NOT
	// pricePlanId). Drop the overwritten optional keys first (an omitted field → absent).
	for _, k := range []string{"name", "prices", "filters", "modifiers", "resourceType", "applyMethod", "timeUnit"} {
		delete(existing, k)
	}
	d := req.doc()
	delete(d, "pricePlanId") // update does NOT touch pricePlanId — preserve the existing value.
	for k, v := range d {
		existing[k] = v
	}
	if err := h.repo.ReplacePricePlanRuleDoc(r.Context(), id, existing); httpx.WriteError(w, err) {
		return
	}
	// Record an audit event with a field-level diff of the before/after documents.
	after, _ := h.repo.PricePlanRuleByID(r.Context(), id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(existing))
}

// pricePlanRuleDelete loads the rule (404 if absent), deletes it, and returns success ("Successful operation").
func (h *Handler) pricePlanRuleDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pricePlanManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.PricePlanRuleByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("PricePlanRule not found. "))
		return
	}
	if _, err := h.repo.DeletePricePlanRuleDoc(r.Context(), id); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a rule is deleted.
	httpx.OK(w, "Successful operation")
}

// pricePlanRuleUsage loads the rule (404 if absent), then returns {ruleId, ruleName,
// openBillsCount, totalAppliedAmount}. The Σ-applied money sum stays deferred (money rule) → 0 here,
// and with no bills the count is 0 too — {…, openBillsCount:0, totalAppliedAmount:0}, which
// un-blocks the rule page.
func (h *Handler) pricePlanRuleUsage(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pricePlanReadPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.PricePlanRuleByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("PricePlanRule not found. "))
		return
	}
	name, _ := existing["name"].(string)
	httpx.OK(w, map[string]any{
		"ruleId": id, "ruleName": name, "openBillsCount": 0, "totalAppliedAmount": 0,
	})
}

// pricePlanRuleClone resolves the target price plan first (404 when absent), then clones each
// requested rule into it. Per-item name-conflict handling: overwrite → delete the existing same-name
// rule first; else append " (Copy)".
func (h *Handler) pricePlanRuleClone(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pricePlanManagePerm) {
		return
	}
	var req clonePricePlanRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	// Resolve the target price plan FIRST (→ 404 when absent).
	target, err := h.repo.PricePlanByID(r.Context(), req.TargetPricePlanID)
	if httpx.WriteError(w, err) {
		return
	}
	if target == nil {
		httpx.WriteError(w, pricePlanNotFound(req.TargetPricePlanID))
		return
	}
	results := []clonedRuleResult{}
	for _, item := range req.Rules {
		src, err := h.repo.PricePlanRuleByID(r.Context(), item.RuleID)
		if httpx.WriteError(w, err) {
			return
		}
		if src == nil {
			httpx.WriteError(w, httpx.NotFound("PricePlanRule not found. "))
			return
		}
		newID, overwritten, name, cerr := h.doCloneRule(r, src, req.TargetPricePlanID, item.NewName, item.Overwrite)
		if cerr != nil {
			httpx.WriteError(w, cerr)
			return
		}
		results = append(results, clonedRuleResult{
			SourceRuleID: item.RuleID,
			NewRuleID:    newID,
			NewRuleName:  name,
			Overwritten:  overwritten,
		})
	}
	httpx.OK(w, clonePricePlanRuleResponse{ClonedRules: results})
}

// cloneSingleRule is the plan-clone path's rule copy (always overwrite=false, blank newName → keep
// the source name). Returns the new rule id ("" only on a logical no-op, never here).
func (h *Handler) cloneSingleRule(r *http.Request, src pgdoc.M, targetPlanID, newName string, overwrite bool) (string, *httpx.HTTPError) {
	id, _, _, err := h.doCloneRule(r, src, targetPlanID, newName, overwrite)
	return id, err
}

// doCloneRule performs the rule copy: resolve the final name (newName or source name), on a same-name
// conflict either delete-and-overwrite or append " (Copy)", then create the copy carrying the
// source's timeUnit/resourceType/applyMethod/prices/filters/modifiers (money copied as-is from the
// stored decimal string — no float conversion). Returns (newRuleId, overwritten, finalName, err).
func (h *Handler) doCloneRule(r *http.Request, src pgdoc.M, targetPlanID, newName string, overwrite bool) (string, bool, string, *httpx.HTTPError) {
	srcName, _ := src["name"].(string)
	finalName := srcName
	if strings.TrimSpace(newName) != "" {
		finalName = newName
	}
	overwritten := false
	existing, err := h.repo.PricePlanRuleByPlanIDAndName(r.Context(), targetPlanID, finalName)
	if err != nil {
		return "", false, "", httpx.NewError(http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
	}
	if existing != nil {
		if overwrite {
			exID, _ := pricePlanDocID(existing)
			// TODO(audit): write an admin audit event when the overwritten rule is deleted.
			if _, derr := h.repo.DeletePricePlanRuleDoc(r.Context(), exID); derr != nil {
				return "", false, "", httpx.NewError(http.StatusInternalServerError, http.StatusInternalServerError, derr.Error())
			}
			overwritten = true
		} else {
			finalName = finalName + " (Copy)"
		}
	}
	newDoc := pgdoc.M{
		"name":        finalName,
		"pricePlanId": targetPlanID,
	}
	for _, k := range []string{"timeUnit", "resourceType", "applyMethod", "prices", "filters", "modifiers"} {
		if v, ok := src[k]; ok {
			newDoc[k] = v
		}
	}
	saved, serr := h.repo.InsertPricePlanRuleDoc(r.Context(), newDoc)
	if serr != nil {
		return "", false, "", httpx.NewError(http.StatusInternalServerError, http.StatusInternalServerError, serr.Error())
	}
	// TODO(audit): write an admin audit event when the cloned rule is created.
	newID, _ := pricePlanDocID(saved)
	return newID, overwritten, finalName, nil
}

// applyRuleDefault applies the default for a rule with no applyMethod: it gets ADD_TO_TOTAL,
// persisted (the default is saved back) and reflected in the read.
func (h *Handler) applyRuleDefault(r *http.Request, rule pgdoc.M) {
	if _, ok := rule["applyMethod"]; ok && rule["applyMethod"] != nil {
		return
	}
	rule["applyMethod"] = "ADD_TO_TOTAL"
	if id, ok := pricePlanDocID(rule); ok {
		_, _ = h.repo.SetPricePlanRuleField(r.Context(), id, "applyMethod", "ADD_TO_TOTAL")
	}
}

// doc builds the stored JSON for a PricePlanRule mutation. name/timeUnit/resourceType are required
// (validated upstream); pricePlanId/applyMethod/prices/filters/modifiers are set when present. Money-
// bearing prices/filters/modifiers are stored as their decoded JSON pass-through (money fidelity
// note, deferred).
func (req pricePlanRuleReq) doc() pgdoc.M {
	d := pgdoc.M{"name": req.Name, "timeUnit": req.TimeUnit, "resourceType": req.ResourceType}
	if req.PricePlanID != "" {
		d["pricePlanId"] = req.PricePlanID
	}
	if req.ApplyMethod != nil && *req.ApplyMethod != "" {
		d["applyMethod"] = *req.ApplyMethod
	}
	if req.Prices != nil {
		d["prices"] = rawJSON(*req.Prices)
	}
	if req.Filters != nil {
		d["filters"] = rawJSON(*req.Filters)
	}
	if req.Modifiers != nil {
		d["modifiers"] = rawJSON(*req.Modifiers)
	}
	return d
}

// pricePlanDocID reads the `_id` of a stored doc as a string (PricePlan/PricePlanRule key `_id` by a
// String).
func pricePlanDocID(doc pgdoc.M) (string, bool) {
	if doc == nil {
		return "", false
	}
	if v, ok := doc["_id"]; ok {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	if v, ok := doc["id"]; ok {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}

// ── Clone request/response DTOs (nulls omitted) ───────────────────────────────

type clonePricePlanRequest struct {
	PricePlans []clonePricePlanItem `json:"pricePlans"`
}

type clonePricePlanItem struct {
	PricePlanID  string `json:"pricePlanId"`
	NewName      string `json:"newName"`
	IncludeRules *bool  `json:"includeRules"`
}

// includeRules defaults to true: an absent includeRules is treated as true.
func (i clonePricePlanItem) includeRules() bool {
	return i.IncludeRules == nil || *i.IncludeRules
}

type clonePricePlanResponse struct {
	ClonedPricePlans []clonedPricePlanResult `json:"clonedPricePlans"`
}

type clonedPricePlanResult struct {
	SourcePricePlanID string `json:"sourcePricePlanId"`
	NewPricePlanID    string `json:"newPricePlanId"`
	NewPricePlanName  string `json:"newPricePlanName"`
	RulesCloned       int    `json:"rulesCloned"`
}

type clonePricePlanRuleRequest struct {
	TargetPricePlanID string          `json:"targetPricePlanId"`
	Rules             []cloneRuleItem `json:"rules"`
}

type cloneRuleItem struct {
	RuleID    string `json:"ruleId"`
	NewName   string `json:"newName"`
	Overwrite bool   `json:"overwrite"`
}

type clonePricePlanRuleResponse struct {
	ClonedRules []clonedRuleResult `json:"clonedRules"`
}

type clonedRuleResult struct {
	SourceRuleID string `json:"sourceRuleId"`
	NewRuleID    string `json:"newRuleId"`
	NewRuleName  string `json:"newRuleName"`
	Overwritten  bool   `json:"overwritten"`
}
