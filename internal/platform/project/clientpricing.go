package project

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// clientpricing.go implements the client price-preview (POST /pricing/{projectId}/service/{esId}
// and .../types): rate the BillingResource(s) the FE sends against the project's price plans →
// PricingResource{currency, billingCycles:[{type:"month", value}]}. The FE's server-detail Pricing
// tab + the create-server estimate read billingCycles[0].value. Reuses the pure rating layer
// (pricing.Engine + SelectPricePlansForService) and the billingresource catalog (attribute schemas).

// pricingResourceReq is the FE's BillingResource body (the fields the rating needs).
type pricingResourceReq struct {
	ResourceID   string         `json:"resourceId"`
	ResourceType string         `json:"resourceType"`
	Values       map[string]any `json:"values"`
}

type pricingTypesReq struct {
	BillingResources []pricingResourceReq `json:"billingResources"`
}

// pricingResourceDTO is the PricingResource response (resourceId/currency omitted when
// blank; billingCycles is present as [] in the not-priced case so the FE binds a stable array).
type pricingResourceDTO struct {
	ResourceID    string            `json:"resourceId,omitempty"`
	Currency      string            `json:"currency,omitempty"`
	BillingCycles []pricingCycleDTO `json:"billingCycles"`
}

type pricingCycleDTO struct {
	Type  string      `json:"type"`
	Value json.Number `json:"value"`
}

// timeUnits is the iteration order (MINUTE/HOUR/MONTH).
var pricingTimeUnits = []string{"minute", "hour", "month"}

// pricingSingle handles POST /pricing/{projectId}/service/{esId}: rate one
// BillingResource (the request body) → single PricingResource.
func (h *Handler) pricingSingle(w http.ResponseWriter, r *http.Request) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		h.fail(w, err)
		return
	}
	profile, ok := h.pricingProfile(r, u.Sub)
	if !ok {
		// billing not created or no billing profile → empty.
		httpx.OK(w, pricingResourceDTO{BillingCycles: []pricingCycleDTO{}})
		return
	}
	var req pricingResourceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.fail(w, httpx.BadRequest("invalid request body"))
		return
	}
	esID := chi.URLParam(r, "externalServiceId")
	dto := h.priceResource(r.Context(), esID, profile, req)
	httpx.OK(w, dto)
}

// pricingTypes handles POST /pricing/{projectId}/service/{esId}/types: rate a
// list of BillingResources → a list of PricingResource (each carrying its resourceId).
func (h *Handler) pricingTypes(w http.ResponseWriter, r *http.Request) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		h.fail(w, err)
		return
	}
	profile, ok := h.pricingProfile(r, u.Sub)
	if !ok {
		httpx.List(w, []pricingResourceDTO{})
		return
	}
	var req pricingTypesReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.fail(w, httpx.BadRequest("invalid request body"))
		return
	}
	esID := chi.URLParam(r, "externalServiceId")
	out := make([]pricingResourceDTO, 0, len(req.BillingResources))
	for _, br := range req.BillingResources {
		out = append(out, h.priceResource(r.Context(), esID, profile, br))
	}
	httpx.List(w, out)
}

// pricingProfile resolves the project's BillingProfile when billing is created and the project has
// one (billing must be created and the project must have a profile). ok=false → empty.
func (h *Handler) pricingProfile(r *http.Request, sub string) (*billing.BillingProfile, bool) {
	if _, _, created, err := h.billing.Configuration(r.Context()); err != nil || !created {
		return nil, false
	}
	proj, err := h.svc.GetProject(r.Context(), sub, chi.URLParam(r, "projectId"))
	if err != nil || proj == nil {
		return nil, false
	}
	bpID := h.resolveBillingProfileID(r.Context(), proj)
	if bpID == "" {
		return nil, false
	}
	profile, err := h.billing.FindByID(r.Context(), bpID)
	if err != nil || profile == nil {
		return nil, false
	}
	return profile, true
}

// resolveBillingProfileID returns the project's own billingProfileId, falling back to the owning
// org's billing profile id.
func (h *Handler) resolveBillingProfileID(ctx context.Context, proj *Project) string {
	if proj.BillingProfileID != "" {
		return proj.BillingProfileID
	}
	if proj.OrganizationID == "" {
		return ""
	}
	o, err := h.orgSvc.FindOrganization(ctx, proj.OrganizationID)
	if err != nil || o == nil {
		return ""
	}
	return o.BillingProfileID
}

// priceResource rates one BillingResource → PricingResource:
// select the profile's price plans (public + scoped to the external service), apply every plan's rules
// per time unit, sum the net amounts converted to a monthly figure.
func (h *Handler) priceResource(ctx context.Context, esID string, profile *billing.BillingProfile, req pricingResourceReq) pricingResourceDTO {
	res := &pricing.BillingResource{
		ResourceID:          req.ResourceID,
		ResourceType:        req.ResourceType,
		Values:              h.resourceValues(ctx, req),
		DisplayPrice:        true,
		BillingResourceType: billingResourceTypeFor(req.ResourceType),
	}

	var planIDs []string
	includePublic := true
	if profile.PricePlanConfig != nil {
		planIDs = profile.PricePlanConfig.PricePlanIDs
		includePublic = profile.PricePlanConfig.IncludePublicPricePlans
	}
	plans := pricing.SelectPricePlansForService(h.pricing.PlanSource(ctx), planIDs, includePublic, esID)

	// billingConfiguration.settings.timeUnitLimits — the per-unit month-size override (threaded
	// into toMonthlyPrice); nil → the per-unit defaults apply.
	limits, _ := h.billing.TimeUnitLimits(ctx)
	total := decimal.Zero
	for _, plan := range plans {
		for _, tu := range pricingTimeUnits {
			rules, err := h.pricing.RulesByPricePlanIDAndTimeUnit(ctx, plan.ID, tu)
			if err != nil || len(rules) == 0 {
				continue
			}
			results, err := h.engine.ApplyPricePlanRules(rules, res, tu)
			if err != nil {
				continue // attribute missing / not eligible → no contribution (best-effort preview)
			}
			for _, rr := range results {
				for _, a := range rr.Amounts {
					total = total.Add(toMonthlyPrice(rr.PricePlanRule, a.NetAmount, tu, limits))
				}
			}
		}
	}

	dto := pricingResourceDTO{ResourceID: req.ResourceID, Currency: profile.Currency, BillingCycles: []pricingCycleDTO{}}
	if len(plans) > 0 {
		dto.BillingCycles = []pricingCycleDTO{{Type: "month", Value: json.Number(total.String())}}
	}
	return dto
}

// resourceValues returns the request's values, or — when the FE sends an instance resource with no
// values — derives them from the cached server's flavor (vcpus/ram/disk) so the per-vCPU/RAM rules
// still rate. Non-instance types fall back to whatever the request carried.
func (h *Handler) resourceValues(ctx context.Context, req pricingResourceReq) map[string]any {
	if len(req.Values) > 0 || req.ResourceID == "" {
		return req.Values
	}
	cr, err := h.cloud.FindByID(ctx, req.ResourceID)
	if err != nil || cr == nil {
		return req.Values
	}
	if req.ResourceType == "instance" || req.ResourceType == "" {
		if vals := instanceValuesFromServer(cr); vals != nil {
			return vals
		}
	}
	return req.Values
}

// instanceValuesFromServer builds the "instance" billing-resource attribute values (instance_type/
// vcpus/ram_mb/ram_gb/root_disk_gb) from a cached SERVER cloud resource's flavor. nil if not a server.
func instanceValuesFromServer(cr *cloud.CloudResource) map[string]any {
	srv, ok := cr.Data["server"].(map[string]any)
	if !ok {
		return nil
	}
	fl, ok := srv["flavor"].(map[string]any)
	if !ok {
		return nil
	}
	vals := map[string]any{
		"instance_type": strAny(cr.Data["flavorName"]),
		"vcpus":         fl["vcpus"], "ram_mb": fl["ram"], "root_disk_gb": fl["disk"],
	}
	if cloud.ServerIsVolumeBacked(cr.Data) {
		vals["root_disk_gb"] = 0
	}
	if name, ok := fl["name"].(string); ok && vals["instance_type"] == "" {
		vals["instance_type"] = name
	}
	if ram, ok := toDecAny(fl["ram"]); ok {
		vals["ram_gb"] = ram.DivRound(decimal.NewFromInt(1024), 2)
	}
	// Mirror the billing cron's GPU attributes so the client price preview rates GPUs too.
	gpuModel, gpuCount := cloud.GPUFromFlavor(fl["extra_specs"])
	vals["gpu_model"] = gpuModel
	vals["gpu_count"] = gpuCount
	return vals
}

// billingResourceTypeName maps a cloud resource Type to its billing-resource type (the FE's
// pricingservice.billingResourceTypes mapping) — the key the price-plan rules + catalog use.
func billingResourceTypeName(cloudType string) string {
	switch cloudType {
	case cloud.TypeServer, cloud.TypeBaremetalServer:
		return "instance"
	case cloud.TypeVolume:
		return "volume"
	case cloud.TypeFloatingIP:
		return "floating_ip"
	case cloud.TypeLoadBalancer:
		return "load_balancer"
	}
	return ""
}

// attachPricePlan computes a cloud resource's monthly price and sets cr.PricePlan (the FE's
// server-detail Pricing tab reads model.vps.pricePlan.{currency,billingCycles}). No-op when billing
// isn't created, the project has no profile, or the type isn't billed. Best-effort (never fails the read).
func (h *Handler) attachPricePlan(ctx context.Context, proj *Project, cr *cloud.CloudResource) {
	if cr == nil {
		return
	}
	rt := billingResourceTypeName(cr.Type)
	if rt == "" {
		return
	}
	if _, _, created, err := h.billing.Configuration(ctx); err != nil || !created {
		return
	}
	bpID := h.resolveBillingProfileID(ctx, proj)
	if bpID == "" {
		return
	}
	profile, err := h.billing.FindByID(ctx, bpID)
	if err != nil || profile == nil {
		return
	}
	req := pricingResourceReq{ResourceID: cr.ExternalID, ResourceType: rt}
	if rt == "instance" {
		req.Values = instanceValuesFromServer(cr)
	}
	cr.PricePlan = h.priceResource(ctx, cr.ServiceID, profile, req)
}

// billingResourceTypeFor looks up a resource type's attribute schema in the catalog (nil when the
// type isn't billed — the engine then treats every attribute reference as not-found and skips).
func billingResourceTypeFor(resourceType string) *pricing.BillingResourceType {
	for _, t := range billingresource.Catalog() {
		if t.ResourceType == resourceType {
			// The catalog speaks the billingapi wire contract (resolution side); the engine wants
			// its own type.
			return pricing.FromAPIResourceType(t)
		}
	}
	return nil
}

// toMonthlyPrice: OVERWRITE_TOTAL keeps the amount; otherwise the
// per-unit amount is multiplied by the unit's size-in-a-month.
func toMonthlyPrice(rule pricing.PricePlanRule, amount decimal.Decimal, timeUnit string, limits map[string]int) decimal.Decimal {
	if rule.ApplyMethod == "OVERWRITE_TOTAL" {
		return amount
	}
	return amount.Mul(decimal.NewFromInt(int64(timeUnitSizeInMonth(timeUnit, limits))))
}

// timeUnitSizeInMonth: the billingConfiguration.settings
// .timeUnitLimits override for the time unit if present, else the per-unit default (MINUTE 43200 /
// HOUR 720 / MONTH 1). Honouring the config lets a deployment customise the month size (e.g. a 730-hr
// month).
func timeUnitSizeInMonth(timeUnit string, limits map[string]int) int {
	if v, ok := limits[timeUnit]; ok {
		return v
	}
	switch timeUnit {
	case "minute":
		return 43200
	case "hour":
		return 720
	default:
		return 1
	}
}

// toDecAny coerces a free-form numeric value to a decimal.
func toDecAny(v any) (decimal.Decimal, bool) {
	switch n := v.(type) {
	case int:
		return decimal.NewFromInt(int64(n)), true
	case int64:
		return decimal.NewFromInt(n), true
	case float64:
		return decimal.NewFromFloat(n), true
	case decimal.Decimal:
		return n, true
	}
	return decimal.Zero, false
}
