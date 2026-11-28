package admin

// setupdefaults.go — the defaults a new install needs before tenants can use a Proxmox provider, applied from the
// console instead of deploy/seed/seed-operator.py: GET /admin/setup/defaults previews them with the state of each item,
// POST /admin/setup/defaults {serviceId, items, confirmRates} applies the chosen items. The documents are the embedded
// deploy/seed files.
//
// Items: flavors and pricePlan need no provider; quota and region are per provider (serviceId).
//
// Apply only creates. It adds what is missing and never changes or removes a document that exists, so a second apply
// writes nothing. One transaction under one advisory lock does the whole apply: two operators clicking at once get one
// price plan, and a failure half way leaves nothing behind.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/deploy/seed"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/platformconfig"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// The items of the defaults, in the order they are applied and listed.
const (
	defaultsFlavors   = "flavors"
	defaultsPricePlan = "pricePlan"
	defaultsQuota     = "quota"
	defaultsRegion    = "region"
)

// States of a setup item. A preview and the setup status use done, attention, todo, blocked and unknown; an apply
// answers created, skipped or blocked.
const (
	stateDone      = "done"
	stateAttention = "attention"
	stateTodo      = "todo"
	stateBlocked   = "blocked"
	stateUnknown   = "unknown"
	stateCreated   = "created"
	stateSkipped   = "skipped"
)

const (
	billingConfigReadPerm = "admin:billing_config:read"
	pcfgReadPerm          = "admin:platform_config:read"
	serviceReadPerm       = "admin:service:read"
)

// defaultsItemPerms is what applying an item needs: the permission of the endpoint that writes the same document by
// hand, so this route is no way around any of them. The price plan also reads the billing currency its rates are
// charged in.
var defaultsItemPerms = map[string][]string{
	defaultsFlavors:   {externalServiceManagePerm},
	defaultsPricePlan: {pricePlanManagePerm, billingConfigReadPerm},
	defaultsQuota:     {externalServiceManagePerm},
	defaultsRegion:    {pcfgUpdatePerm},
}

// defaultsReadPerms gate the preview: it shows documents of all four families.
var defaultsReadPerms = []string{serviceReadPerm, pricePlanReadPerm, pcfgReadPerm, billingConfigReadPerm}

// defaultsPerms lists the permissions an apply of items needs (items are validated by the caller).
func defaultsPerms(items []string) []string {
	var out []string
	for _, it := range items {
		for _, p := range defaultsItemPerms[it] {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

func (h *Handler) routeSetup(r chi.Router) {
	r.Get("/setup/defaults", h.setupDefaultsPreview)
	r.Post("/setup/defaults", h.setupDefaultsApply)
	r.Get("/setup/status", h.setupStatusGet)
}

// setupItem is one line of a setup answer.
type setupItem struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Message string `json:"message"`
}

// defaultsItem is one item of the defaults. Preview is exactly what an apply would write for it (absent when it would
// write nothing).
type defaultsItem struct {
	setupItem
	Preview any `json:"preview,omitempty"`
}

// ── the embedded documents ──────────────────────────────────────────────────────────────────────

type seedDefaults struct {
	Flavors  []pveflavor.Flavor
	Plan     pricePlanReq
	Rules    []pricePlanRuleReq
	Currency string  // the currency the rates were written in
	Platform pgdoc.M // the platform configuration of an install that has none
}

// loadSeed parses the embedded documents once. Every flavor and rule passes the validation of its own admin endpoint,
// so an apply stores nothing those endpoints would refuse.
var loadSeed = sync.OnceValues(func() (*seedDefaults, error) {
	var sd seedDefaults
	var plan struct {
		Plan  pricePlanReq       `json:"plan"`
		Rules []pricePlanRuleReq `json:"rules"`
	}
	var bill struct {
		BaseCurrency string `json:"baseCurrency"`
	}
	for _, f := range []struct {
		name string
		raw  []byte
		out  any
	}{
		{"flavors.json", seed.Flavors, &sd.Flavors}, {"price-plan-seed.json", seed.PricePlan, &plan},
		{"billing-configuration.json", seed.BillingConfiguration, &bill}, {"platform-configuration.json", seed.PlatformConfiguration, &sd.Platform},
	} {
		if err := json.Unmarshal(f.raw, f.out); err != nil {
			return nil, fmt.Errorf("deploy/seed/%s: %w", f.name, err)
		}
	}
	sd.Plan, sd.Rules, sd.Currency = plan.Plan, plan.Rules, bill.BaseCurrency
	for i := range sd.Flavors {
		if err := sd.Flavors[i].Validate(); err != nil {
			return nil, fmt.Errorf("deploy/seed/flavors.json: %s: %w", sd.Flavors[i].Slug, err)
		}
	}
	for i := range sd.Rules {
		sd.Rules[i].PricePlanID = "" // the file carries a placeholder; the apply sets the new plan's id
		if herr := validatePricePlanRule(&sd.Rules[i]); herr != nil {
			return nil, fmt.Errorf("deploy/seed/price-plan-seed.json: rule %q: %s", sd.Rules[i].Name, herr.Msg)
		}
	}
	if sd.Plan.Name == "" || len(sd.Rules) == 0 || sd.Currency == "" {
		return nil, fmt.Errorf("deploy/seed: the price plan needs a name, rules and a base currency")
	}
	return &sd, nil
})

// defaultRuleTypes are the resource types every Proxmox install bills. The file also prices managed Kubernetes,
// databases and sandboxes; this apply adds none of those rules and nothing else does: the operator adds them to the
// plan when the service is offered. A resource no rule matches is charged nothing, so a platform's sandbox provider
// is refused live until a sandbox rule exists (sandboxUnpriced).
var defaultRuleTypes = map[string]bool{
	"instance": true, "instance_traffic": true, "volume": true, "floating_ip": true, "image": true,
	"server_snapshot": true, "server_protection": true,
}

// isGPURule reports a rule that prices GPUs: it prices gpu_count or filters on gpu_model (the same test as the
// unpriced-flavors guard).
func isGPURule(r pricePlanRuleReq) bool {
	for _, c := range []struct {
		raw  *json.RawMessage
		attr string
	}{{r.Prices, "gpu_count"}, {r.Filters, "gpu_model"}} {
		var rows []struct {
			AttributeName string `json:"attributeName"`
		}
		if c.raw != nil && json.Unmarshal(*c.raw, &rows) == nil {
			for _, row := range rows {
				if row.AttributeName == c.attr {
					return true
				}
			}
		}
	}
	return false
}

// defaultRules picks the rules an install gets: the base types always, GPU rules only when a provider maps GPUs,
// share and bucket rules only when such a provider exists. A rule for something the install cannot sell is noise in
// the rate table the operator has to confirm.
func defaultRules(rules []pricePlanRuleReq, gpu, share, bucket bool) []pricePlanRuleReq {
	var out []pricePlanRuleReq
	for _, r := range rules {
		keep := defaultRuleTypes[r.ResourceType]
		switch {
		case r.ResourceType == "share":
			keep = share
		case r.ResourceType == "bucket":
			keep = bucket
		case r.ResourceType == "instance" && isGPURule(r):
			keep = gpu
		}
		if keep {
			out = append(out, r)
		}
	}
	return out
}

// ── what an apply would write ───────────────────────────────────────────────────────────────────

// defaultsState is everything the defaults depend on.
type defaultsState struct {
	Flavors  map[string]bool                   // slugs in the catalog
	Plans    []pricing.PricePlan               // every price plan
	Services []externalservice.ExternalService // every provider (secrets still encrypted, never read)
	Currency string                            // billingConfiguration.baseCurrency ("" = no billing configuration)
	Provider *externalservice.ExternalService  // the provider of serviceId (nil = none given)
	// PlatformID and Regions are the current platform configuration ("" = the install has none).
	PlatformID string
	Regions    []platformconfig.RegionDisplayConfig
}

// defaultsWrite is what an apply creates. Nothing in it exists yet.
type defaultsWrite struct {
	Flavors []pveflavor.Flavor
	Plan    pgdoc.M
	Rules   []pgdoc.M
	Quota   map[string]any // the whole merged default quota (nil = unchanged)
	Region  *platformconfig.RegionDisplayConfig
}

// planDefaults decides, from the state alone, what an apply would create and the state of each item. It is the one
// place the rules live: the preview shows its answer and the apply executes it.
func planDefaults(sd *seedDefaults, st defaultsState) (defaultsWrite, []defaultsItem) {
	var w defaultsWrite
	item := func(id, state, msg string, preview any) defaultsItem {
		return defaultsItem{setupItem: setupItem{ID: id, State: state, Message: msg}, Preview: preview}
	}
	items := make([]defaultsItem, 0, 4)

	for _, f := range sd.Flavors {
		if !st.Flavors[f.Slug] {
			w.Flavors = append(w.Flavors, f)
		}
	}
	if n := len(w.Flavors); n > 0 {
		items = append(items, item(defaultsFlavors, stateTodo, fmt.Sprintf("%d of the %d default flavors are missing", n, len(sd.Flavors)), w.Flavors))
	} else {
		items = append(items, item(defaultsFlavors, stateDone, fmt.Sprintf("The %d default flavors exist", len(sd.Flavors)), nil))
	}

	items = append(items, planPricePlan(sd, st, &w))

	switch p := st.Provider; {
	case p == nil:
		items = append(items, item(defaultsQuota, stateBlocked, "Needs a Proxmox provider (serviceId)", nil),
			item(defaultsRegion, stateBlocked, "Needs a Proxmox provider (serviceId)", nil))
	case !p.IsProxmox():
		items = append(items, item(defaultsQuota, stateBlocked, "The provider is not a Proxmox VE provider", nil),
			item(defaultsRegion, stateBlocked, "The provider is not a Proxmox VE provider", nil))
	default:
		if merged, added := defaultQuotaFor(p); len(added) > 0 {
			w.Quota = merged
			items = append(items, item(defaultsQuota, stateTodo, "The default project quota does not limit "+strings.Join(added, ", ")+"; limits already set are kept", merged))
		} else {
			items = append(items, item(defaultsQuota, stateDone, "The default project quota sets every limit", nil))
		}
		region := p.PVERegion()
		switch has := slices.ContainsFunc(st.Regions, func(r platformconfig.RegionDisplayConfig) bool { return r.ServiceID == p.ID }); {
		case has:
			items = append(items, item(defaultsRegion, stateDone, "The provider has a region row in the platform configuration", nil))
		case region == "":
			items = append(items, item(defaultsRegion, stateBlocked, "The provider has no region (config.region)", nil))
		default:
			w.Region = &platformconfig.RegionDisplayConfig{ServiceID: p.ID, Region: region, Order: len(st.Regions)}
			items = append(items, item(defaultsRegion, stateTodo, "Region "+region+" is not listed in the platform configuration", w.Region))
		}
	}
	return w, items
}

// defaultQuotaFor is the provider's stored default quota with the default limits added where it sets none, and the
// limits it added. The apply writes it; the setup status calls the quota done only when it adds nothing.
func defaultQuotaFor(p *externalservice.ExternalService) (map[string]any, []string) {
	prov, _ := p.Config["provisioning"].(map[string]any)
	stored, _ := prov["quota"].(map[string]any)
	return externalservice.MergePVEDefaultQuota(stored, seed.DefaultQuota)
}

// planPricePlan is the price plan item. Every enabled PUBLIC plan is billed (pricing.SelectPricePlans), so a second
// one would charge every resource twice: the default is added only when there is none. When every one is scoped to
// other providers than serviceId, the default (unscoped) plan would bill those twice, so the item is blocked.
func planPricePlan(sd *seedDefaults, st defaultsState, w *defaultsWrite) defaultsItem {
	it := defaultsItem{setupItem: setupItem{ID: defaultsPricePlan}}
	var elsewhere []string
	for _, p := range st.Plans {
		if !p.Enabled || p.AccessMode != pricing.AccessPublic {
			continue
		}
		if st.Provider == nil || p.IsServiceProviderScoped(st.Provider.ID) {
			it.State, it.Message = stateDone, fmt.Sprintf("The enabled public price plan %q exists; the default plan is not added, because every enabled public plan is billed", p.Name)
			return it
		}
		elsewhere = append(elsewhere, fmt.Sprintf("%q", p.Name))
	}
	if len(elsewhere) > 0 {
		it.State, it.Message = stateBlocked, "No enabled public price plan covers this provider: "+strings.Join(elsewhere, ", ")+
			" bills other providers only. The default plan is not added, because it would bill those a second time; add this provider to one of them or add a plan scoped to it"
		return it
	}
	for _, p := range st.Plans {
		if p.Name == sd.Plan.Name {
			it.State, it.Message = stateBlocked, fmt.Sprintf("A price plan named %q exists but is not an enabled public plan; enable it or delete it, the default is not added twice", p.Name)
			return it
		}
	}
	if st.Currency == "" {
		it.State, it.Message = stateBlocked, "The billing configuration has no base currency; set it first, the rates are amounts in that currency"
		return it
	}
	var gpu, share, bucket bool
	for i := range st.Services {
		s := &st.Services[i]
		gpu = gpu || s.IsProxmox() && len(s.PVEGPUAliases()) > 0
		share = share || s.IsTrueNAS()
		bucket = bucket || s.IsObjectStore()
	}
	w.Plan = sd.Plan.doc()
	w.Plan["accessMode"] = pricing.AccessPublic
	for _, r := range defaultRules(sd.Rules, gpu, share, bucket) {
		w.Rules = append(w.Rules, r.doc())
	}
	it.State = stateTodo
	it.Message = fmt.Sprintf("No enabled public price plan: %q with %d rules would be added, enabled, in %s", sd.Plan.Name, len(w.Rules), st.Currency)
	if st.Currency != sd.Currency {
		it.Message += fmt.Sprintf(". The default rates were written as %s amounts and would be charged as %s amounts", sd.Currency, st.Currency)
	}
	it.Preview = map[string]any{"plan": w.Plan, "rules": w.Rules, "currency": st.Currency}
	return it
}

// ── database ────────────────────────────────────────────────────────────────────────────────────

// currentConfigDoc is the default document of a configuration collection, else its first (nil = none).
func (r *Repo) currentConfigDoc(ctx context.Context, collection string) (pgdoc.M, error) {
	doc, err := r.FindOneBy(ctx, collection, pgdoc.M{"defaultConfiguration": true})
	if err != nil || doc != nil {
		return doc, err
	}
	return r.FindOneBy(ctx, collection, pgdoc.M{})
}

// readDefaultsState reads the state. A serviceId that names no provider is a 404.
func (h *Handler) readDefaultsState(ctx context.Context, serviceID string) (defaultsState, error) {
	st := defaultsState{Flavors: map[string]bool{}}
	fls, err := h.pveFlavors.List(ctx, false)
	if err != nil {
		return st, err
	}
	for _, f := range fls {
		st.Flavors[f.Slug] = true
	}
	if err := h.repo.c(pricePlanCollection).Find(ctx, nil, &st.Plans); err != nil {
		return st, err
	}
	if err := h.repo.c(externalServiceCollection).Find(ctx, nil, &st.Services); err != nil {
		return st, err
	}
	if serviceID != "" {
		i := slices.IndexFunc(st.Services, func(s externalservice.ExternalService) bool { return s.ID == serviceID })
		if i < 0 {
			return st, serviceNotFoundErr(serviceID)
		}
		st.Provider = &st.Services[i]
	}
	bill, err := h.repo.currentConfigDoc(ctx, billingConfigCollection)
	if err != nil {
		return st, err
	}
	st.Currency = str(bill["baseCurrency"])
	st.PlatformID, st.Regions, err = h.repo.currentRegions(ctx)
	return st, err
}

// currentRegions is the region list of the current platform configuration and that document's id ("" = the install
// has none). A list that is not a region list reads as empty.
func (r *Repo) currentRegions(ctx context.Context) (string, []platformconfig.RegionDisplayConfig, error) {
	plat, err := r.currentConfigDoc(ctx, pcfgCollection)
	if err != nil {
		return "", nil, err
	}
	var regions []platformconfig.RegionDisplayConfig
	if raw, err := json.Marshal(plat["regions"]); err == nil {
		_ = json.Unmarshal(raw, &regions)
	}
	id, _ := pricePlanDocID(plat)
	return id, regions, nil
}

// defaultsLockKey serializes applies: the check for an enabled public plan and the insert of the default one must not
// interleave between two requests.
const defaultsLockKey = "admin-setup-defaults"

// applyDefaults creates the wanted items in one transaction and answers one result per wanted item. The quota and the
// region are written as one key each, so a provider or platform configuration save made at the same moment keeps its
// other fields.
func (h *Handler) applyDefaults(ctx context.Context, sd *seedDefaults, serviceID string, want []string) ([]defaultsItem, error) {
	// Inside a transaction a statement on a missing table fails the transaction, so the tables come first.
	if err := h.pveFlavors.Ensure(ctx); err != nil {
		return nil, err
	}
	for _, c := range []string{pricePlanCollection, pricePlanRuleCollection, externalServiceCollection, billingConfigCollection, pcfgCollection} {
		if err := h.repo.c(c).Ensure(ctx); err != nil {
			return nil, err
		}
	}
	var out []defaultsItem
	err := h.repo.db.WithTx(ctx, func(ctx context.Context) error {
		if err := h.repo.db.LockXact(ctx, defaultsLockKey); err != nil {
			return err
		}
		st, err := h.readDefaultsState(ctx, serviceID)
		if err != nil {
			return err
		}
		w, items := planDefaults(sd, st)
		for _, it := range items {
			if !slices.Contains(want, it.ID) {
				continue
			}
			switch it.State {
			case stateTodo:
				if it.Message, err = h.writeDefault(ctx, sd, it.ID, w, st); err != nil {
					return fmt.Errorf("%s: %w", it.ID, err)
				}
				it.State = stateCreated
			case stateDone:
				it.State = stateSkipped
			}
			it.Preview = nil
			out = append(out, it)
		}
		return nil
	})
	return out, err
}

// writeDefault creates one item and says what it created.
func (h *Handler) writeDefault(ctx context.Context, sd *seedDefaults, id string, w defaultsWrite, st defaultsState) (string, error) {
	switch id {
	case defaultsFlavors:
		slugs := make([]string, len(w.Flavors))
		for i := range w.Flavors {
			f := w.Flavors[i]
			if err := h.pveFlavors.Save(ctx, &f); err != nil {
				return "", err
			}
			slugs[i] = f.Slug
		}
		return "Added " + strings.Join(slugs, ", "), nil
	case defaultsPricePlan:
		plan, err := h.repo.InsertPricePlanDoc(ctx, w.Plan)
		if err != nil {
			return "", err
		}
		planID, _ := pricePlanDocID(plan)
		for _, r := range w.Rules {
			r["pricePlanId"] = planID
			if _, err := h.repo.InsertPricePlanRuleDoc(ctx, r); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("Added the enabled public price plan %q with %d rules", sd.Plan.Name, len(w.Rules)), nil
	case defaultsQuota:
		// jsonb_set creates the last key only, so a provider without config.provisioning gets the whole object.
		set := pgdoc.M{"config.provisioning.quota": w.Quota}
		if _, ok := st.Provider.Config["provisioning"].(map[string]any); !ok {
			set = pgdoc.M{"config.provisioning": pgdoc.M{"quota": w.Quota}}
		}
		if _, err := h.repo.c(externalServiceCollection).SetByID(ctx, st.Provider.ID, set, nil); err != nil {
			return "", err
		}
		return "Added the missing limits to the default project quota", nil
	case defaultsRegion:
		if st.PlatformID == "" {
			doc := pgdoc.M{}
			for k, v := range sd.Platform {
				doc[k] = v
			}
			doc["regions"] = []platformconfig.RegionDisplayConfig{*w.Region}
			if _, err := h.repo.InsertDoc(ctx, pcfgCollection, doc); err != nil {
				return "", err
			}
			return "Created the platform configuration with region " + w.Region.Region, nil
		}
		if _, err := h.repo.SetFields(ctx, pcfgCollection, st.PlatformID, pgdoc.M{"regions": append(slices.Clone(st.Regions), *w.Region)}); err != nil {
			return "", err
		}
		return "Added region " + w.Region.Region + " to the platform configuration", nil
	}
	return "", fmt.Errorf("unknown item %q", id)
}

// ── handlers ────────────────────────────────────────────────────────────────────────────────────

// setupDefaultsGate checks every permission; false = the 403 went out.
func (h *Handler) setupDefaultsGate(w http.ResponseWriter, r *http.Request, perms []string) bool {
	_, granted, ok := h.adminContext(r)
	if missing := missingPerm(granted, perms); !ok || missing != "" {
		if missing == "" {
			missing = perms[0]
		}
		httpx.WriteError(w, httpx.Forbidden("You do not have the required permission: "+missing))
		return false
	}
	return true
}

// setupDefaultsPreview handles GET /admin/setup/defaults?serviceId=: the state of each item and what an apply would
// write for it. It changes nothing.
func (h *Handler) setupDefaultsPreview(w http.ResponseWriter, r *http.Request) {
	if !h.setupDefaultsGate(w, r, defaultsReadPerms) || !h.pveFlavorsReady(w) {
		return
	}
	sd, err := loadSeed()
	if httpx.WriteError(w, err) {
		return
	}
	st, err := h.readDefaultsState(r.Context(), r.URL.Query().Get("serviceId"))
	if httpx.WriteError(w, err) {
		return
	}
	_, items := planDefaults(sd, st)
	httpx.OK(w, map[string]any{"currency": st.Currency, "items": items})
}

type setupDefaultsReq struct {
	ServiceID string   `json:"serviceId"`
	Items     []string `json:"items"`
	// ConfirmRates must be true to apply pricePlan: the plan is created enabled and its rates become invoices.
	ConfirmRates bool `json:"confirmRates"`
}

// setupDefaultsApply handles POST /admin/setup/defaults.
func (h *Handler) setupDefaultsApply(w http.ResponseWriter, r *http.Request) {
	// An admin first, whatever the body says; the permissions of the chosen items after it is read.
	if _, _, ok := h.adminContext(r); !ok {
		httpx.WriteError(w, httpx.Forbidden("You do not have the required permission: "+externalServiceManagePerm))
		return
	}
	var req setupDefaultsReq
	if !decodeStrictBody(w, r, &req) {
		return
	}
	if len(req.Items) == 0 {
		httpx.WriteError(w, httpx.BadRequest("items is required: any of flavors, pricePlan, quota, region"))
		return
	}
	for _, it := range req.Items {
		if defaultsItemPerms[it] == nil {
			httpx.WriteError(w, httpx.BadRequest(fmt.Sprintf("Unknown item %q: use flavors, pricePlan, quota or region", it)))
			return
		}
	}
	if !h.setupDefaultsGate(w, r, defaultsPerms(req.Items)) {
		return
	}
	if slices.Contains(req.Items, defaultsPricePlan) && !req.ConfirmRates {
		httpx.WriteError(w, httpx.BadRequest("confirmRates must be true to apply pricePlan: the default plan is created enabled and its rates are billed. Review them in GET /admin/setup/defaults first"))
		return
	}
	if !h.pveFlavorsReady(w) {
		return
	}
	sd, err := loadSeed()
	if httpx.WriteError(w, err) {
		return
	}
	items, err := h.applyDefaults(r.Context(), sd, req.ServiceID, req.Items)
	if httpx.WriteError(w, err) {
		return
	}
	audit.RecordSnapshots(r.Context(), nil, map[string]any{"serviceId": req.ServiceID, "items": items})
	httpx.OK(w, map[string]any{"items": items})
}
