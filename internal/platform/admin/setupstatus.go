package admin

// setupstatus.go — GET /admin/setup/status?serviceId=&live=1: how far an install is from its first tenant, as one
// list of steps for the Dashboard card and the setup page. The rules are one pure function (setupStatus) over a
// snapshot, so they are tested as a table and exist once, not once per page.
//
// Without live the snapshot is read from the database only. With live=1 it also asks the Proxmox cluster: the
// connection test's checks, where an import store is active, the published OS images and the imported templates.
//
// It does not count rows. The Compose seed creates the billing and the platform configuration, so "a billing
// configuration exists" says nothing about whether the operator set anything up.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/platformconfig"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// The steps, in the order of the setup page. Preparing Proxmox (the script run by hand on a node) has no state of its
// own to read: the connect step shows whether it worked.
const (
	stepConnect   = "connect"
	stepNetwork   = "network"
	stepStorage   = "storage"
	stepTemplates = "templates"
	stepCatalog   = "catalog"
	stepGoLive    = "golive"
)

// setupStep is one step: its state, one sentence, and the checks behind it.
type setupStep struct {
	ID      string      `json:"id"`
	State   string      `json:"state"`
	Summary string      `json:"summary"`
	Items   []setupItem `json:"items"`
}

// setupSnapshot is everything the status is computed from.
type setupSnapshot struct {
	Provider      *externalservice.ExternalService // the Proxmox provider looked at (nil = none is saved)
	Flavors       int                              // flavors in the catalog
	Unpriced      []UnpricedFlavor                 // those no rule of an enabled public plan prices (needs Provider)
	PublicPlans   int                              // enabled PUBLIC price plans that apply to Provider (all without one)
	InstanceRules int                              // their rules for resource type instance
	Regions       []platformconfig.RegionDisplayConfig
	Issuer        billing.IssuerInfo
	Scheduler     bool       // PROXCLOUD_JOBS_SCHEDULER_ENABLED: syncs and charges run on a timer
	Live          *setupLive // nil = database only
}

// setupLive is what only the cluster and the running API know.
type setupLive struct {
	Checks []PVECheck // the connection test's rows (pveConnectionTest with checks=all), placed by checkSteps
	// ImportNodes are the nodes with an active store that takes import content, of Nodes nodes; ImportErr says why
	// that could not be read.
	ImportNodes  []string
	Nodes        int
	ImportErr    string
	Images       int            // published OS images, as GET /admin/service/{id}/os-images lists them
	TemplateFlag bool           // PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED
	GitHubToken  bool           // a GitHub integration is saved
	Imports      map[string]int // the provider's imported templates by lifecycle state (nil = not read)
}

func stateRank(state string) int {
	switch state {
	case stateTodo, stateBlocked:
		return 3
	case stateAttention:
		return 2
	case stateUnknown:
		return 1
	}
	return 0
}

// newStep rolls items up into a step: the worst item decides the state (a blocked item makes the step todo, a step is
// blocked only when it cannot start), and its message is the summary. done is the summary when every item is done.
func newStep(id, done string, items ...setupItem) setupStep {
	st := setupStep{ID: id, State: stateDone, Summary: done, Items: items}
	worst := 0
	for _, it := range items {
		if r := stateRank(it.State); r > worst {
			worst, st.Summary = r, it.Message
		}
	}
	st.State = [...]string{stateDone, stateUnknown, stateAttention, stateTodo}[worst]
	return st
}

// checkSteps places a row of the connection test (PVECheck.ID) in the step it is about, and names the database item
// it replaces there: the row asks that item's question and reads the cluster as well. A row not listed is connect's.
var checkSteps = map[string]struct{ step, replaces string }{
	"zone":            {stepNetwork, "zone"},
	"cidr-pool":       {stepNetwork, "cidrPool"},
	"frr":             {stepNetwork, ""},
	"public-networks": {stepNetwork, ""},
	"store-reach":     {stepNetwork, ""},
	"storage-profile": {stepStorage, "defaultProfile"},
	"import-store":    {stepStorage, ""},
	"boot-iso":        {stepStorage, ""},
}

var checkStates = map[string]string{checkOK: stateDone, checkWarn: stateAttention, checkFail: stateTodo}

// withChecks adds the live rows of step to its items. A row that replaces an item takes its place and its id, so an
// item has the same id with and without live.
func withChecks(step string, items []setupItem, l *setupLive) []setupItem {
	if l == nil {
		return items
	}
	for _, c := range l.Checks {
		place := checkSteps[c.ID]
		if place.step == "" {
			place.step = stepConnect
		}
		if place.step != step {
			continue
		}
		it := setupItem{ID: c.ID, State: checkStates[c.Status], Message: c.Message}
		if i := slices.IndexFunc(items, func(x setupItem) bool { return x.ID == place.replaces }); place.replaces != "" && i >= 0 {
			it.ID = place.replaces
			items[i] = it
			continue
		}
		items = append(items, it)
	}
	return items
}

func checkItem(id string, ok bool, done, todo string) setupItem {
	if ok {
		return setupItem{ID: id, State: stateDone, Message: done}
	}
	return setupItem{ID: id, State: stateTodo, Message: todo}
}

// setupStatus computes every step from the snapshot. It reads nothing else.
func setupStatus(s setupSnapshot) []setupStep {
	p := s.Provider
	if p == nil {
		blocked := func(id string) setupStep {
			return setupStep{ID: id, State: stateBlocked, Summary: "Needs a saved Proxmox provider", Items: []setupItem{}}
		}
		return []setupStep{
			{ID: stepConnect, State: stateTodo, Summary: "No Proxmox provider is saved", Items: []setupItem{}},
			blocked(stepNetwork), blocked(stepStorage), blocked(stepTemplates), catalogStep(s), blocked(stepGoLive),
		}
	}
	steps := []setupStep{connectStep(s), networkStep(s), storageStep(s), templatesStep(s), catalogStep(s)}
	return append(steps, goLiveStep(s, steps))
}

func connectStep(s setupSnapshot) setupStep {
	p := s.Provider
	items := []setupItem{{ID: "provider", State: stateDone, Message: "Provider " + p.Name + " is saved"}}
	if p.IsDisabled() {
		items = append(items, setupItem{ID: "status", State: stateAttention, Message: "The provider is disabled: nothing syncs and templates cannot be imported"})
	}
	return newStep(stepConnect, "Provider "+p.Name+" is saved", withChecks(stepConnect, items, s.Live)...)
}

func networkStep(s setupSnapshot) setupStep {
	n := s.Provider.PVENetworkConfig()
	zone, hasZone := n.Zones[n.DefaultZone]
	return newStep(stepNetwork, "Project networks use zone "+n.DefaultZone+" and "+n.CIDRPool, withChecks(stepNetwork, []setupItem{
		checkItem("cidrPool", n.CIDRPool != "", "Project networks are cut from "+n.CIDRPool, "No address pool for project networks (config.network.cidrPool): creating a network fails"),
		checkItem("zone", hasZone, "Default zone "+n.DefaultZone+" ("+zone.Kind+")", "No default zone (config.network.defaultZone and zones): creating a network fails"),
	}, s.Live)...)
}

func storageStep(s setupSnapshot) setupStep {
	p := s.Provider
	caps, profiles, def := p.PVECapabilities(), p.PVEStorageProfiles(), p.PVEDefaultStorageProfile()
	slugs := make([]string, 0, len(profiles))
	for slug := range profiles {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	_, hasDefault := profiles[def]
	items := []setupItem{
		checkItem("detected", !caps.DetectedAt.IsZero(), "Storage was detected on "+caps.DetectedAt.Format("2006-01-02"), "Storage was never detected (POST /admin/service/{id}/storage/detect)"),
		checkItem("profiles", len(profiles) > 0, "Storage profiles: "+strings.Join(slugs, ", "), "No storage profile is saved: servers and volumes have nowhere to go"),
		checkItem("defaultProfile", hasDefault, "Default storage profile "+def, "No default storage profile (config.defaultStorageProfile)"),
	}
	// An import store is needed for template and image import only, so a missing one is a warning.
	const noImport = "No active store takes import content, so templates and images cannot be imported. Add it to a store: pvesm set <storage> --content <its content>,import"
	switch l := s.Live; {
	case l != nil && l.ImportErr != "":
		items = append(items, setupItem{ID: "importStore", State: stateUnknown, Message: "The import stores could not be read: " + l.ImportErr})
	case l != nil && len(l.ImportNodes) == 0:
		items = append(items, setupItem{ID: "importStore", State: stateAttention, Message: noImport})
	case l != nil:
		items = append(items, setupItem{ID: "importStore", State: stateDone, Message: fmt.Sprintf("An import store is active on %d of %d nodes: %s", len(l.ImportNodes), l.Nodes, strings.Join(l.ImportNodes, ", "))})
	case !caps.DetectedAt.IsZero():
		has := false
		for _, c := range caps.Storages {
			has = has || c.HasContent("import")
		}
		it := setupItem{ID: "importStore", State: stateAttention, Message: noImport}
		if has {
			it = setupItem{ID: "importStore", State: stateDone, Message: "A store takes import content (as last detected)"}
		}
		items = append(items, it)
	}
	return newStep(stepStorage, "Storage profiles are saved, default "+def, withChecks(stepStorage, items, s.Live)...)
}

// templatesStep is done as soon as one OS image is published, however it got there: a site that built its templates
// with images/build.py has nothing to do here. Only the cluster knows those templates, so without live the step is
// unknown unless the provider configuration pins one.
func templatesStep(s setupSnapshot) setupStep {
	p, l := s.Provider, s.Live
	images := len(p.PVEImageCatalog(nil))
	if l != nil {
		images = l.Images
	}
	if images > 0 {
		msg := fmt.Sprintf("%d OS images are published", images)
		return newStep(stepTemplates, msg, setupItem{ID: "images", State: stateDone, Message: msg})
	}
	if l == nil {
		return newStep(stepTemplates, "", setupItem{ID: "images", State: stateUnknown, Message: "No template is pinned in the provider configuration; the templates on the cluster are counted with live=1"})
	}
	items := []setupItem{
		{ID: "images", State: stateTodo, Message: "No OS image is published: tenants cannot create a server"},
		checkItem("flag", l.TemplateFlag, "Template updates are on", "Template updates are off: set PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true and restart the API"),
		checkItem("githubToken", l.GitHubToken, "A GitHub token is saved", "No GitHub token is saved (System → Integrations → GitHub): image versions cannot be listed"),
		checkItem("smokeNetwork", SmokeNetworkOf(p).Bridge != "", "The smoke-test network is set", "No smoke-test network (config.templateSmoke): imported templates cannot be tested"),
	}
	if l.Imports != nil {
		states := make([]string, 0, len(l.Imports))
		for state, n := range l.Imports {
			states = append(states, fmt.Sprintf("%d %s", n, state))
		}
		sort.Strings(states)
		it := setupItem{ID: "imports", State: stateTodo, Message: "No template import was started"}
		if len(states) > 0 {
			it.Message = "Imported templates, none published yet: " + strings.Join(states, ", ")
		}
		items = append(items, it)
	}
	return newStep(stepTemplates, "", items...)
}

func catalogStep(s setupSnapshot) setupStep {
	p := s.Provider
	items := []setupItem{checkItem("flavors", s.Flavors > 0, fmt.Sprintf("%d flavors in the catalog", s.Flavors), "No flavor in the catalog: tenants cannot create a server")}
	switch {
	case s.PublicPlans == 0 && p != nil:
		items = append(items, setupItem{ID: "pricePlan", State: stateTodo, Message: "No enabled public price plan covers this provider: its usage is not billed"})
	case s.PublicPlans == 0:
		items = append(items, setupItem{ID: "pricePlan", State: stateTodo, Message: "No enabled public price plan: nothing is billed"})
	case s.InstanceRules == 0:
		items = append(items, setupItem{ID: "pricePlan", State: stateTodo, Message: "The enabled public price plan has no rule for servers: they are billed as zero"})
	case s.PublicPlans > 1:
		items = append(items, setupItem{ID: "pricePlan", State: stateAttention, Message: fmt.Sprintf("%d enabled public price plans: every one of them is billed, so a resource priced in two is charged twice", s.PublicPlans)})
	default:
		items = append(items, setupItem{ID: "pricePlan", State: stateDone, Message: fmt.Sprintf("An enabled public price plan with %d server rules", s.InstanceRules)})
	}
	// Without a plan that prices servers every flavor is unpriced, and the line above already says so.
	if s.PublicPlans > 0 && s.InstanceRules > 0 && len(s.Unpriced) > 0 {
		names := make([]string, len(s.Unpriced))
		for i, u := range s.Unpriced {
			names[i] = u.ID
		}
		items = append(items, setupItem{ID: "unpricedFlavors", State: stateAttention, Message: "Flavors that match no price rule (or no GPU rule) are billed as zero: " + strings.Join(names, ", ")})
	}
	if p == nil {
		items = append(items, setupItem{ID: "quota", State: stateBlocked, Message: "Needs a saved Proxmox provider"}, setupItem{ID: "region", State: stateBlocked, Message: "Needs a saved Proxmox provider"})
	} else {
		_, unset := defaultQuotaFor(p)
		items = append(items,
			checkItem("quota", len(unset) == 0, "The provider's default project quota sets every limit", "The default project quota does not limit "+strings.Join(unset, ", ")+": new projects are unlimited there"),
			checkItem("region", slices.ContainsFunc(s.Regions, func(r platformconfig.RegionDisplayConfig) bool { return r.ServiceID == p.ID }),
				"The provider's region is listed in the platform configuration", "The provider's region is not listed in the platform configuration"))
	}
	// Recommended, not required: invoices are issued without them, they only look unfinished.
	if s.Issuer.BusinessName == "" || s.Issuer.Address == "" || s.Issuer.Country == "" {
		items = append(items, setupItem{ID: "billingDetails", State: stateAttention, Message: "The billing configuration lacks the business name, address or country that invoices carry"})
	} else {
		items = append(items, setupItem{ID: "billingDetails", State: stateDone, Message: "Invoices carry the details of " + s.Issuer.BusinessName})
	}
	return newStep(stepCatalog, "Flavors, prices, quota and region are set", items...)
}

// goLiveStep is the last switch: attach new projects to the provider. It waits for connect, network, storage and
// templates; a warning there does not hold it back, but a state that was not read does: go live is then unknown, not
// todo. The catalog is the operator's call (usage without a rule is billed as zero), and so is running without the job
// scheduler: both are shown as warnings and block nothing.
func goLiveStep(s setupSnapshot, steps []setupStep) setupStep {
	p := s.Provider
	var open, unread []string
	catalog := stateDone
	for _, st := range steps {
		switch {
		case st.ID == stepCatalog:
			catalog = st.State
		case st.State == stateTodo || st.State == stateBlocked:
			open = append(open, st.ID)
		case st.State == stateUnknown:
			unread = append(unread, st.ID)
		}
	}
	attach := setupItem{ID: "attachPolicy", State: stateTodo, Message: "New projects are not attached to the provider yet (attach policy off)"}
	switch on := p.AttachToNewProjects(); {
	case on && len(open) > 0:
		attach = setupItem{ID: "attachPolicy", State: stateAttention, Message: "New projects are attached to the provider although these steps are open: " + strings.Join(open, ", ")}
	case on:
		attach = setupItem{ID: "attachPolicy", State: stateDone, Message: "New projects are attached to the provider"}
	case len(open) > 0:
		return setupStep{ID: stepGoLive, State: stateBlocked, Summary: "Finish these steps first: " + strings.Join(open, ", "), Items: []setupItem{attach}}
	case len(unread) > 0:
		msg := "These steps could not be checked: " + strings.Join(unread, ", ")
		if s.Live == nil {
			msg += "; ask again with live=1"
		}
		return setupStep{ID: stepGoLive, State: stateUnknown, Summary: msg, Items: []setupItem{attach}}
	}
	items := []setupItem{attach}
	if catalog == stateTodo {
		items = append(items, setupItem{ID: "catalog", State: stateAttention, Message: "Catalog and pricing is not complete: usage that matches no rule is billed as zero"})
	}
	sched := setupItem{ID: "scheduler", State: stateDone, Message: "The job scheduler runs: providers are synced and usage is charged on a timer"}
	if !s.Scheduler {
		sched = setupItem{ID: "scheduler", State: stateAttention, Message: "The job scheduler is off (PROXCLOUD_JOBS_SCHEDULER_ENABLED): nothing is synced or charged on a timer, so run one sync after going live"}
	}
	return newStep(stepGoLive, attach.Message, append(items, sched)...)
}

// ── reading the snapshot ────────────────────────────────────────────────────────────────────────

// setupProvider is the provider the status looks at: the one named, else the first Proxmox provider by name. A named
// provider that does not exist is a 404; none at all is not an error.
func (h *Handler) setupProvider(ctx context.Context, serviceID string) (*externalservice.ExternalService, error) {
	if serviceID != "" {
		if h.esSvc == nil {
			return nil, serviceNotFoundErr(serviceID)
		}
		es, err := h.esSvc.Get(ctx, serviceID)
		if err != nil || es == nil {
			return nil, serviceNotFoundErr(serviceID)
		}
		if !es.IsProxmox() {
			return nil, httpx.BadRequest("The setup status needs a Proxmox VE provider")
		}
		return es, nil
	}
	if h.esSvc == nil {
		return nil, nil
	}
	all, err := h.esSvc.ListByType(ctx, externalservice.TypeCloud)
	if err != nil {
		return nil, err
	}
	all = slices.DeleteFunc(all, func(es externalservice.ExternalService) bool { return !es.IsProxmox() })
	if len(all) == 0 {
		return nil, nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name+"\x00"+all[i].ID < all[j].Name+"\x00"+all[j].ID })
	return &all[0], nil
}

// readSetupSnapshot reads the snapshot from the database, and from the cluster when live.
func (h *Handler) readSetupSnapshot(ctx context.Context, serviceID string, live bool) (setupSnapshot, error) {
	s := setupSnapshot{Scheduler: h.jobsScheduler}
	var err error
	if s.Provider, err = h.setupProvider(ctx, serviceID); err != nil {
		return s, err
	}
	if h.pveFlavors != nil {
		fls, err := h.pveFlavors.List(ctx, false)
		if err != nil {
			return s, err
		}
		s.Flavors = len(fls)
	}
	if h.pricing != nil {
		plans, err := h.pricing.PublicPricePlans(ctx)
		if err != nil {
			return s, err
		}
		for _, plan := range plans {
			// A plan scoped to other providers bills nothing here (pricing.SelectPricePlansForService).
			if s.Provider != nil && !plan.IsServiceProviderScoped(s.Provider.ID) {
				continue
			}
			s.PublicPlans++
			for _, tu := range pricePlanTimeUnits {
				rules, err := h.pricing.RulesByPricePlanIDAndTimeUnit(ctx, plan.ID, tu)
				if err != nil {
					return s, err
				}
				for _, r := range rules {
					if strings.EqualFold(r.ResourceType, "instance") {
						s.InstanceRules++
					}
				}
			}
		}
		if s.Provider != nil {
			s.Unpriced = h.unpricedFlavorsOf(ctx, s.Provider)
		}
	}
	if _, s.Regions, err = h.repo.currentRegions(ctx); err != nil {
		return s, err
	}
	if h.billing != nil {
		if s.Issuer, err = h.billing.IssuerInfo(ctx); err != nil {
			return s, err
		}
	}
	if live && s.Provider != nil {
		s.Live = h.readSetupLive(ctx, s.Provider)
	}
	return s, nil
}

// readSetupLive asks the cluster. A cluster that does not answer is a failed check, not a failed request.
func (h *Handler) readSetupLive(ctx context.Context, es *externalservice.ExternalService) *setupLive {
	l := &setupLive{TemplateFlag: h.templateUpdates != nil && h.templateUpdates.VMEnabled}
	if n, err := h.githubCount(ctx); err == nil {
		l.GitHubToken = n > 0
	}
	// The connection test as POST /admin/service/proxmox/auth?checks=all&externalServiceId= runs it for a caller who
	// may only read: the stored connection, every check. With all, a failed check is a row, never an error.
	test, _ := pveConnectionTest(ctx, es.PVEConfig(), es, true, false)
	l.Checks = test.Checks
	if h.pveFor == nil {
		l.ImportErr = "Proxmox access is not wired in this API"
		return l
	}
	ps, err := h.pveFor(es)
	if err != nil {
		l.ImportErr = "the provider configuration is incomplete"
		return l
	}
	if opts, err := h.placementOptions(ctx, es); err != nil {
		l.ImportErr = err.Error()
	} else {
		l.Nodes = len(opts)
		for _, o := range opts {
			if len(o.ImportStorages) > 0 {
				l.ImportNodes = append(l.ImportNodes, o.Node)
			}
		}
	}
	l.Images = len(h.pveTemplateImages(ctx, es))
	if h.templateUpdates != nil {
		if rows, err := ps.C.ClusterResources(ctx, "vm"); err == nil {
			if rows == nil {
				rows = []pve.ClusterResource{}
			}
			if states, err := h.templateUpdates.States(ctx, map[string]templateupdates.ProviderRows{es.ID: {Rows: rows}}); err == nil {
				l.Imports = map[string]int{}
				for _, st := range states {
					if st.ServiceID == es.ID {
						l.Imports[st.State]++
					}
				}
			}
		}
	}
	return l
}

// setupStatusResponse is the answer: the provider the steps were computed for ("" = none is saved), whether the
// cluster was asked, and the steps.
type setupStatusResponse struct {
	ServiceID string      `json:"serviceId"`
	Live      bool        `json:"live"`
	Steps     []setupStep `json:"steps"`
}

// SetJobsScheduler records whether the job scheduler runs (PROXCLOUD_JOBS_SCHEDULER_ENABLED); the go live step warns
// without it.
func (h *Handler) SetJobsScheduler(on bool) { h.jobsScheduler = on }

// setupStatusGet handles GET /admin/setup/status. Like the defaults preview it shows providers, price plans, the
// platform and the billing configuration, so it needs the four reads.
func (h *Handler) setupStatusGet(w http.ResponseWriter, r *http.Request) {
	if !h.setupDefaultsGate(w, r, defaultsReadPerms) {
		return
	}
	q := r.URL.Query()
	live := q.Get("live") == "1" || q.Get("live") == "true"
	s, err := h.readSetupSnapshot(r.Context(), q.Get("serviceId"), live)
	if httpx.WriteError(w, err) {
		return
	}
	out := setupStatusResponse{Live: s.Live != nil, Steps: setupStatus(s)}
	if s.Provider != nil {
		out.ServiceID = s.Provider.ID
	}
	httpx.OK(w, out)
}
