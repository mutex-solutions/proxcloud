package project

// clientcloud.go holds the client-facing cloud read/glue endpoints the project dashboard needs
// (init, external-service list/details, search, project /{id}/init). These are READ/glue only —
// no cloud writes. The actual provisioning writes are the cloud-write endpoints in cloud_writes.go.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/feature"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// jsonNum renders a decimal string as an unquoted JSON number (BigDecimal money).
func jsonNum(s string) json.Number { return json.Number(s) }

// emptyCostInfo is the zero CostInfo envelope (every field present) used when a project or its
// billing profile has no bills yet — mirrors billing.CostInfoMap's shape.
func emptyCostInfo(zero json.Number) map[string]any {
	return map[string]any{
		"lastMonthCosts": zero, "currentMonthCosts": zero,
		"currentMonthCostsByType": map[string]any{}, "forecastedMonthEndCostsByType": map[string]any{},
		"lastMonthCostsByType": map[string]any{}, "forecastedMonthEndCosts": zero, "topResourcePrices": []any{},
	}
}

// initUI returns {id, menu, kycRequests}. The menu = base
// provider items (greenfield: none) + the OpenStack service items derived from every non-disabled
// CLOUD externalService's config.services (keyed by serviceName, {newMenuItem:false, enabled}).
// KYC requests are empty (no KYC integration). Project-membership-gated.
func (h *Handler) initUI(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	projectID := chi.URLParam(r, "projectId")
	p, err := h.svc.GetProject(r.Context(), u.Sub, projectID)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"id":          p.ID,
		"menu":        map[string]any{"items": h.uiMenuItems(r)},
		"kycRequests": []any{},
	})
}

// uiMenuItems builds the OpenStack menu items: for each non-disabled OpenStack CLOUD
// service, add a menu item per service-name that has at least one enabled region. Shared services
// disable container-infra/object-store/orchestration.
func (h *Handler) uiMenuItems(r *http.Request) map[string]any {
	// Base provider items first (admin Custom Menu docs, the "More" section);
	// OpenStack service items are merged AFTER and overwrite colliding slugs
	// (base providers first, then the openstack items).
	items := map[string]any{}
	if h.customMenu != nil {
		items = h.customMenu.Items(r.Context())
	}
	if h.esSvc == nil {
		return items
	}
	services, err := h.esSvc.ListByType(r.Context(), externalservice.TypeCloud)
	if err != nil {
		return items
	}
	for i := range services {
		es := &services[i]
		// OpenStack (compute/network/… + Swift object-store), ceph-s3 (object-store only),
		// kamaji (kubernetes only) AND dbaas (database only) all contribute menu items — a
		// ceph-only project must still get its Object Storage entry, and the kamaji/dbaas slugs
		// are what un-hide the client Kubernetes/Databases nav (AppShell gates on menu.items).
		if es.IsDisabled() || (es.Provider() != "openstack" && !es.IsCephS3() && !es.IsKamaji() && !es.IsDbaas() && !es.IsProxmox()) {
			continue
		}
		svcMap, _ := es.Config["services"].(map[string]any)
		for name, regionsAny := range svcMap {
			regions, _ := regionsAny.(map[string]any)
			enabledRegion := false
			for _, v := range regions {
				if b, _ := v.(bool); b {
					enabledRegion = true
					break
				}
			}
			if !enabledRegion {
				continue
			}
			if v, exists := items[name]; exists {
				if m, ok := v.(map[string]any); !ok || m["newMenuItem"] != true {
					continue // first openstack service wins; custom slugs get overwritten
				}
			}
			enabled := true
			if es.Shared() {
				switch name {
				case "container-infra", "object-store", "orchestration":
					enabled = false
				}
			}
			items[name] = map[string]any{"newMenuItem": false, "enabled": enabled}
		}
	}
	return items
}

// projectServices returns the CLOUD services attached to the project, as ExternalServiceDto.
// Empty until the project is bootstrapped.
func (h *Handler) projectServices(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "projectId"))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := []map[string]any{}
	for _, id := range p.ServiceIDs() {
		es, err := h.esSvc.Get(r.Context(), id)
		if err != nil || es == nil {
			continue
		}
		out = append(out, externalServiceDto(es))
	}
	httpx.List(w, out)
}

// serviceOfProject loads an external service that must be ATTACHED to the project: an unknown or
// unattached service → 404 "Service not found: {id}". Shared by the by-id read + the auth endpoint.
func (h *Handler) serviceOfProject(w http.ResponseWriter, r *http.Request, p *Project, svcID string) (*externalservice.ExternalService, bool) {
	if !p.HasService(svcID) {
		h.fail(w, httpx.NotFound("Service not found: "+svcID))
		return nil, false
	}
	es, err := h.esSvc.Get(r.Context(), svcID)
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	if es == nil {
		h.fail(w, httpx.NotFound("Service not found: "+svcID))
		return nil, false
	}
	return es, true
}

// projectServiceByID handles GET /{projectId}/service/{serviceId}. Only the entity's top-level
// id/name/type/status bind (config never flattens), so vhi/shared are always false and the
// config-derived DTO fields stay null → dropped as null. Note: this by-id read is THIN, unlike
// the list's config-derived DTO (externalServiceDto).
func (h *Handler) projectServiceByID(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "projectId"))
	if err != nil {
		h.fail(w, err)
		return
	}
	es, ok := h.serviceOfProject(w, r, p, chi.URLParam(r, "serviceType"))
	if !ok {
		return
	}
	httpx.OK(w, map[string]any{
		"id": es.ID, "name": es.Name, "type": es.Type, "status": es.Status,
		"vhi": false, "shared": false,
	})
}

// projectServiceAuth handles POST /{projectId}/service/{serviceId}/auth:
// a shared config can't be user-authenticated (400), else the USER's keystone credentials are
// authenticated scoped to the project's externalProjectId — bad creds → 400 with the translated
// message. All cloud calls run through admin-scoped tenant clients, so a successful check
// returns 200 with nothing persisted.
// ponytail: no user-token store — add one only if user-scoped cloud calls are ever needed.
func (h *Handler) projectServiceAuth(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "projectId"))
	if err != nil {
		h.fail(w, err)
		return
	}
	svcID := chi.URLParam(r, "serviceType")
	es, ok := h.serviceOfProject(w, r, p, svcID)
	if !ok {
		return
	}
	if es.Shared() {
		h.fail(w, httpx.BadRequest("Shared openstack service can not be authenticated "))
		return
	}
	// The keystone credential check died with the OpenStack backend; no per-user
	// cloud credential exists on this fork.
	_ = svcID
	h.fail(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented, "Service authentication is not available on this provider"))
}

// projectCostInfo handles GET /{id}/cost-info: the project's billing-usage overview. Live
// cloud-usage costs are 0 (no metering); balance/credits are real. projects map +
// billingProfileCostInfo populated so the dashboard charts render. Billing profile = the
// project's, else the org's.
func (h *Handler) projectCostInfo(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	bpID := p.BillingProfileID
	if bpID == "" {
		if o, err := h.orgSvc.GetOrganizationForUser(r.Context(), p.OrganizationID, u.Sub); err == nil && o != nil {
			bpID = o.BillingProfileID
		}
	}
	now := time.Now().UTC()
	zero := jsonNum("0")
	credit, balance, promo, due := zero, zero, zero, zero
	if bpID != "" {
		bal := billing.NewBalanceService(h.billing)
		if v, err := h.billing.AccountCreditTotal(r.Context(), bpID); err == nil {
			credit = jsonNum(v.String())
		}
		if v, err := h.billing.AvailablePromotionalTotal(r.Context(), bpID, now); err == nil {
			promo = jsonNum(v.String())
		}
		if v, err := bal.CurrentBalance(r.Context(), bpID, now); err == nil {
			balance = jsonNum(v.String())
		}
		if v, err := bal.CurrentDue(r.Context(), bpID); err == nil {
			due = jsonNum(v.String())
		}
	}
	// Cost overview from the profile's bills, split by scope so a project dashboard isn't shown the
	// whole org's cost:
	//   - projects[p.ID] + the top-level cost cards = THIS project's slice (bill items grouped by
	//     projectId), so only this project's cost + top resources show.
	//   - billingProfileCostInfo = the org (billing-profile) aggregate across every project.
	// Balance/credits/due stay org-level — funds are pooled at the billing profile, not per project.
	projCostInfo, bpCostInfo := emptyCostInfo(zero), emptyCostInfo(zero)
	if bpID != "" {
		if bills, err := h.billing.BillsByBillingProfile(r.Context(), bpID); err == nil {
			// resource-id → createdAt (this project's cache) so each topResourcePrices entry carries
			// the resource's real creation time; without it the FE renders "a few seconds ago".
			created := map[string]*time.Time{}
			if rs, e := h.cloud.FindAllByProjectID(r.Context(), p.ID); e == nil {
				for i := range rs {
					if rs[i].CreatedAt != nil {
						created[rs[i].ID] = rs[i].CreatedAt
					}
				}
			}
			createdFn := func(id string) *time.Time { return created[id] }
			bpCostInfo = billing.CostInfoMap(billing.BillCostBreakdown(bills, now, createdFn))
			if ci, ok := billing.ProjectCostInfoMap(bills, now, createdFn)[p.ID].(map[string]any); ok {
				projCostInfo = ci
			}
		}
	}
	// The FE dashboard reads the per-project cost from `projects[projectId]` (Top-Cost-Generators +
	// the "from the current project" lines); billingProfileCostInfo is the org aggregate.
	httpx.OK(w, map[string]any{
		"projects":               map[string]any{p.ID: projCostInfo},
		"billingProfileCostInfo": bpCostInfo,
		"balance":                balance, "dueAmount": due, "accountCredit": credit, "promotionalCredits": promo,
		"currentMonthCosts": projCostInfo["currentMonthCosts"], "lastMonthCosts": projCostInfo["lastMonthCosts"],
		"proratedMonthEndCosts": projCostInfo["currentMonthCosts"], "forecastedMonthEndCosts": projCostInfo["currentMonthCosts"],
	})
}

// projectLocations handles GET /{projectId}/service/{serviceType}/location: one Location per
// (attached service × region) — the create-resource form's Location dropdown, which carries the
// serviceId + region the create then sends as x-service-id / x-region-id. (The per-resourceType
// region filter is simplified: every region of each attached service of the type is offered.)
func (h *Handler) projectLocations(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "projectId"))
	if err != nil {
		h.fail(w, err)
		return
	}
	serviceType := chi.URLParam(r, "serviceType")
	out := []map[string]any{}
	for _, svcID := range p.ServiceIDs() {
		es, err := h.esSvc.Get(r.Context(), svcID)
		if err != nil || es == nil || es.Type != serviceType {
			continue
		}
		regions, _ := es.Config["regions"].(map[string]any)
		// A ceph-s3 provider is configured with a single config.region (its RGW zonegroup) rather than the
		// OpenStack regions map. Without this the Location picker is empty and the create-bucket form can
		// never send the x-service-id / x-region-id the write path needs.
		if len(regions) == 0 && es.IsCephS3() {
			if r := es.CephRegion(); r != "" {
				regions = map[string]any{r: map[string]any{}}
			}
		}
		for regionName, rcfgAny := range regions {
			rcfg, _ := rcfgAny.(map[string]any)
			country, _ := rcfg["country"].(string)
			displayName, _ := rcfg["displayName"].(string)
			out = append(out, map[string]any{
				"name": es.Name, "serviceId": es.ID, "region": regionName, "provider": es.Provider(),
				"country": country, "displayName": displayName, "order": 0,
			})
		}
	}
	httpx.List(w, out)
}

// instanceMetadataOptions returns the admin-configured metadata options the client create-form
// metadata panel renders, scoped by the x-service-id / x-region-id headers (global options apply
// regardless). Member-gated.
func (h *Handler) instanceMetadataOptions(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.resolveForMember(w, r); !ok {
		return
	}
	if h.metadata == nil {
		httpx.List(w, []any{})
		return
	}
	opts, err := h.metadata.AvailableOptions(r.Context(), r.Header.Get("x-service-id"), r.Header.Get("x-region-id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.List(w, opts)
}

// pricingSingle / pricingTypes live in clientpricing.go — they rate the FE's
// BillingResource(s) against the project's price plans.

// projectServiceDetails returns the OpenStack domain/project ids for the project on a given service
// (headers x-service-id/x-region-id). Live Keystone read; deferred until bootstrap wires the
// project's externalProjectId. Returns an empty shape pre-bootstrap.
func (h *Handler) projectServiceDetails(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.principal(w, r); !ok {
		return
	}
	httpx.OK(w, map[string]any{})
}

// projectInit handles POST /{id}/init: for each attached non-shared service, an unscoped Keystone
// token is fetched and 307+authRedirect returned when SSO is required. With no attached services
// there is nothing to authorize → OK (empty). Bootstrap-time SSO is deferred.
func (h *Handler) projectInit(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	// Entry into a project provisions its OpenStack tenant + ENABLEs (the cloud-bootstrap slice of
	// project bootstrap) and ensures the admin role grant on the tenant.
	// Idempotent — safe to call on an already-bootstrapped project.
	if err := h.enableAndBootstrap(r.Context(), p); err != nil {
		h.fail(w, err)
		return
	}
	httpx.Empty(w)
}

// search returns the prefilled search items the project header search box filters CLIENT-SIDE
// (there is no query param — the FE filters the full set). Aggregates three search sources, each
// gated on its feature: cloud-resource search (feature "search") groups the project's cloud
// resources by type → {type, data:{name,…,id,region}}; project search (feature "search") → every
// project the user can see as {type:"PROJECT", data:{id,name}}; bill search (feature
// "billing"&"search") → the billing profile's bills as {type:"BILL", data:{id,status}}.
func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "projectId"))
	if err != nil {
		h.fail(w, err)
		return
	}
	items := []map[string]any{}

	if feature.IsEnabled("search") {
		// CloudResourceSearchService: list the project's cloud resources, group by type, flatten.
		if resources, err := h.cloud.FindAllByProjectID(r.Context(), p.ID); err == nil {
			for i := range resources {
				cr := &resources[i]
				vals := searchValuesForResource(cr)
				vals["id"] = cr.ID
				vals["region"] = cr.Region
				items = append(items, map[string]any{"type": cr.Type, "data": vals})
			}
		}
		// ProjectSearchService: every project the user can see.
		if views, err := h.svc.ListForSub(r.Context(), u.Sub); err == nil {
			for _, v := range views {
				items = append(items, map[string]any{
					"type": "PROJECT",
					"data": map[string]any{"id": v.ID, "name": v.Name},
				})
			}
		}
		// BillSearchProvider: the project's billing-profile bills (also feature-gated on "billing").
		if feature.IsEnabled("billing") {
			bpID := p.BillingProfileID
			if bpID == "" {
				if o, err := h.orgSvc.FindOrganization(r.Context(), p.OrganizationID); err == nil && o != nil {
					bpID = o.BillingProfileID
				}
			}
			if bpID != "" {
				if bills, err := h.billing.BillsByBillingProfile(r.Context(), bpID); err == nil {
					for i := range bills {
						items = append(items, map[string]any{
							"type": "BILL",
							"data": map[string]any{"id": bills[i].ID, "status": bills[i].Status},
						})
					}
				}
			}
		}
	}
	httpx.List(w, items)
}

// searchValuesForResource returns the per-type searchable flat fields the FE Fuse.js filters on,
// dispatched per resource type:
//   - SERVER:           name, ipv4 (v4 addrs), flavor
//   - VOLUME:           name, type (volume_type)
//   - PORT:             name, device_id, device_owner, mac_address
//   - SECURITY_GROUP:   name, description
//   - IMAGE:            name, image_type, status
//   - FLOATING_IP:      name == floatingIpAddress (the address is stored under `name`)
//   - everything else:  name only (Network/Router/Subnet/Keypair/Zone/Stack/…)
//
// Without these, the FE could only match a resource by name — never a port by MAC, a volume by type,
// or an image by status. Each value lives under the per-type nested data key (data.server.name,
// data.floatingIp.floating_ip_address, …) — note FLOATING_IP's key is the camelCase `floatingIp`.
func searchValuesForResource(cr *cloud.CloudResource) map[string]any {
	vals := map[string]any{}
	if cr.Data == nil {
		return vals
	}
	if n, _ := cr.Data["name"].(string); n != "" {
		vals["name"] = n
	}
	nested := func(key string) map[string]any { m, _ := cr.Data[key].(map[string]any); return m }
	setName := func(m map[string]any) {
		if _, has := vals["name"]; has || m == nil {
			return
		}
		if n, _ := m["name"].(string); n != "" {
			vals["name"] = n
		}
	}
	setStr := func(dst string, m map[string]any, src string) {
		if m == nil {
			return
		}
		if v, _ := m[src].(string); v != "" {
			vals[dst] = v
		}
	}

	switch cr.Type {
	case cloud.TypeServer:
		s := nested("server")
		setName(s)
		if s != nil {
			if fl, ok := s["flavor"].(map[string]any); ok {
				setStr("flavor", fl, "name")
			}
			vals["ipv4"] = serverIPv4s(s)
		}
	case cloud.TypeVolume:
		v := nested("volume")
		setName(v)
		setStr("type", v, "volume_type")
	case cloud.TypePort:
		p := nested("port")
		setName(p)
		setStr("device_id", p, "device_id")
		setStr("device_owner", p, "device_owner")
		setStr("mac_address", p, "mac_address")
	case cloud.TypeSecurityGroup:
		sg := nested("securityGroup")
		setName(sg)
		setStr("description", sg, "description")
	case cloud.TypeImage:
		im := nested("image")
		setName(im)
		setStr("image_type", im, "image_type")
		setStr("status", im, "status")
	case cloud.TypeFloatingIP:
		// The floating-IP address itself is stored under the `name` key.
		setStr("name", nested("floatingIp"), "floating_ip_address")
	default:
		// name-only types (just name): pull it from whichever per-type object is present.
		for _, k := range []string{"network", "router", "zone", "keypair", "subnet", "snapshot",
			"cluster", "bucket", "loadbalancer", "share", "secret", "stack"} {
			setName(nested(k))
		}
	}
	return vals
}

// serverIPv4s extracts the v4 addresses from a nova server's addresses map (version==4)
// for the SERVER search values.
func serverIPv4s(server map[string]any) []string {
	out := []string{}
	addrs, ok := server["addresses"].(map[string]any)
	if !ok {
		return out
	}
	for _, netAny := range addrs {
		list, ok := netAny.([]any)
		if !ok {
			continue
		}
		for _, aAny := range list {
			a, ok := aAny.(map[string]any)
			if !ok {
				continue
			}
			if v, _ := a["version"].(float64); v == 4 {
				if ip, _ := a["addr"].(string); ip != "" {
					out = append(out, ip)
				}
			}
		}
	}
	return out
}

// externalServiceDto maps an ExternalService to the client ExternalServiceDto shape (secret-free):
// the cloud config the provisioning UI needs (regions/services/availabilityZones), id/name/type/status.
func externalServiceDto(es *externalservice.ExternalService) map[string]any {
	cfg := es.Config
	get := func(k string) any { return cfg[k] }
	dto := map[string]any{
		"id":   es.ID,
		"name": es.Name,
		"type": es.Type,
		// provider ("openstack" | "ceph-s3" | "kamaji") lets the client UI label which backend a
		// service rides on — they run side by side and never mix.
		"provider":          es.Provider(),
		"status":            es.Status,
		"vhi":               boolOf(cfg["vhi"]),
		"shared":            es.Shared(),
		"components":        orEmptyList(get("features")),
		"groups":            orEmptyList(get("groups")),
		"regions":           orEmptyMap(get("regions")),
		"services":          orEmptyMap(get("services")),
		"availabilityZones": orEmptyMap(get("availabilityZones")),
	}
	// kamaji: the curated k8s VERSION list (keys only — image ids are operator detail) feeds the
	// client create/upgrade pickers. Config is not a secret; still expose the minimum needed.
	if es.IsKamaji() {
		defaults := es.KamajiConfig().Defaults
		versions := []any{}
		for v := range defaults.Versions {
			versions = append(versions, v)
		}
		dto["kubernetesVersions"] = versions
		// The provider's pinned platform (chart) version — the client compares it against a
		// cluster's own pin to offer the opt-in "platform update".
		if pin := es.KamajiConfig().ChartVersion; pin != "" {
			dto["kubernetesPlatformVersion"] = pin
		}
		// The default StorageClass every cluster ships with — informational, shown at create.
		// Named after the pinned volume type when there is one.
		storage := map[string]any{"className": kamaji.StorageClassName(defaults.StorageVolumeType)}
		if vt := defaults.StorageVolumeType; vt != "" {
			storage["volumeType"] = vt
		}
		dto["kubernetesStorage"] = storage
		// Per-version image-variant names (e.g. "nvidia") — the node-group image picker. Names
		// only; the ids stay operator detail like the version matrix above.
		variantsByVersion := map[string]any{}
		for v := range defaults.Versions {
			if names := defaults.VariantsForVersion(v); len(names) > 0 {
				variantsByVersion[v] = names
			}
		}
		if len(variantsByVersion) > 0 {
			dto["kubernetesImageVariants"] = variantsByVersion
		}
		// Optional node-flavor allowlist: when set, the client node-group picker offers only these.
		if len(defaults.Flavors) > 0 {
			dto["kubernetesFlavorIds"] = defaults.Flavors
		}
	}
	// dbaas: the curated engine catalog feeds the client create pickers (versions/replica choices
	// + the beta badge). Size limits and storage classes ride along for form validation.
	if es.IsDbaas() {
		dcfg := es.DbaasConfig()
		engines := map[string]any{}
		for name, offer := range dcfg.Engines {
			entry := map[string]any{"versions": offer.Versions, "default": offer.Default}
			replicas := []any{}
			for _, n := range offer.ReplicaChoices() {
				replicas = append(replicas, n)
			}
			entry["replicas"] = replicas
			if offer.Beta {
				entry["beta"] = true
			}
			engines[name] = entry
		}
		dto["databaseEngines"] = engines
		if pin := dcfg.ChartVersion; pin != "" {
			dto["databasePlatformVersion"] = pin
		}
		if len(dcfg.StorageClasses) > 0 {
			dto["databaseStorageClasses"] = dcfg.StorageClasses
		}
		dto["databaseLimits"] = map[string]any{
			"maxCpu": dcfg.Limits.MaxCPU, "maxMemoryGiB": dcfg.Limits.MaxMemoryGiB, "maxStorageGiB": dcfg.Limits.MaxStorageGiB,
		}
	}
	return dto
}

func boolOf(v any) bool { b, _ := v.(bool); return b }

func orEmptyMap(v any) any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func orEmptyList(v any) any {
	if l, ok := v.([]any); ok {
		return l
	}
	return []any{}
}
