package admin

// unpricedflavors.go — the silent-zero-billing guard. The pricing engine bills a resource
// that matches NO rule as zero, without any error — so a (GPU) flavor shipped before its
// price rule exists runs free. GET /admin/service/{id}/unpriced-flavors lists the live
// flavors whose synthetic instance resource matches no rule of the service's enabled
// PUBLIC plans on any time unit. Profile-SCOPED plans are ignored: the guard targets the
// public default rate card. ADMIN_SERVICE_READ; cloud errors skip that region.

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// UnpricedFlavor is one live flavor with a pricing gap. Reason distinguishes a flavor
// matching NO rule at all from a GPU flavor whose generic (CPU/RAM) rules match but no
// GPU-aware rule does — the GPU devices themselves would bill zero.
type UnpricedFlavor struct {
	Region   string `json:"region"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	GpuModel string `json:"gpuModel,omitempty"`
	GpuCount int    `json:"gpuCount,omitempty"`
	Reason   string `json:"reason"` // "no rule" | "no gpu rule"
}

// pricePlanTimeUnits are the rule time units a plan can price on.
var pricePlanTimeUnits = []string{"hour", "month", "minute"}

func (h *Handler) unpricedFlavors(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	ctx := r.Context()
	plans := pricing.SelectPricePlansForService(h.pricing.PlanSource(ctx), nil, true, es.ID)
	rules := []pricing.PricePlanRule{}
	for _, tu := range pricePlanTimeUnits {
		rules = append(rules, pricing.ApplicableRules(plans, h.pricing.RuleSource(ctx), tu)...)
	}
	// GPU-aware rules: price gpu_count or filter on gpu_model. A GPU flavor that matches
	// only generic rules (per-vCPU/RAM) still runs its GPU devices for free — flag it.
	gpuRules := []pricing.PricePlanRule{}
	for _, rule := range rules {
		aware := false
		for _, p := range rule.Prices {
			if p.AttributeName == "gpu_count" {
				aware = true
			}
		}
		for _, f := range rule.Filters {
			if f.AttributeName == "gpu_model" {
				aware = true
			}
		}
		if aware {
			gpuRules = append(gpuRules, rule)
		}
	}
	var instanceType *pricing.BillingResourceType
	for _, t := range billingresource.Catalog() {
		if t.ResourceType == "instance" {
			// Catalog speaks the billingapi wire contract; the engine wants its own type.
			instanceType = pricing.FromAPIResourceType(t)
			break
		}
	}
	engine := pricing.NewEngine(nil)
	out := []UnpricedFlavor{}
	if h.pveFlavors != nil {
		if fls, ferr := h.pveFlavors.List(ctx, false); ferr == nil {
			for _, f := range fls {
				model, count := cloud.GPUFromFlavor(f.ExtraSpecs())
				res := &pricing.BillingResource{
					ResourceType: "instance",
					Values: map[string]any{
						"instance_type": f.Slug, "vcpus": f.VCPUs, "ram_mb": f.RAMMb,
						"ram_gb":       decimal.NewFromInt(int64(f.RAMMb)).DivRound(decimal.NewFromInt(1024), 2),
						"root_disk_gb": f.DiskGb, "gpu_model": model, "gpu_count": count,
						"is_bareMetal": false,
					},
					BillingResourceType: instanceType,
				}
				matchesAny := func(set []pricing.PricePlanRule) bool {
					for _, tu := range pricePlanTimeUnits {
						if rs, err := engine.ApplyPricePlanRules(set, res, tu); err == nil && len(rs) > 0 {
							return true
						}
					}
					return false
				}
				switch {
				case !matchesAny(rules):
					out = append(out, UnpricedFlavor{Region: es.PVERegion(), ID: f.Slug, Name: f.Slug,
						GpuModel: model, GpuCount: count, Reason: "no rule"})
				case count > 0 && !matchesAny(gpuRules):
					out = append(out, UnpricedFlavor{Region: es.PVERegion(), ID: f.Slug, Name: f.Slug,
						GpuModel: model, GpuCount: count, Reason: "no gpu rule"})
				}
			}
		}
	}
	httpx.List(w, out)
}
