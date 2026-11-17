package platformlife

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// preflightPVE is a one-node PVE that answers each token's effective permissions from perms (token id → path → privs)
// and records every request that is not a GET.
type preflightPVE struct {
	perms   map[string]map[string][]string
	writes  []string
	bootISO bool   // the boot store local-lvm takes ISO images too
	outside string // a service user that is not in the group proxcloud-kube: PVE refuses to show it to the portal token
	vnets   string // the cluster's vnets, [] when empty
	zones   string // the cluster's SDN zones, [] when empty; "!" answers 403
	vm9999  bool   // a guest holds the id proxmox-csi names its volumes after
}

func (f *preflightPVE) client(t *testing.T, url, token string) *pve.Client {
	t.Helper()
	c, err := pve.New(pve.Config{APIURL: url, TokenID: token, Secret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *preflightPVE) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "PVEAPIToken=")
	token = token[:strings.LastIndex(token, "=")]
	data := map[string]string{
		"/cluster/status":           `[{"type":"node","name":"pve1","ip":"192.0.2.11"}]`,
		"/nodes/pve1/network":       `[{"iface":"vmbr0","address":"192.0.2.11","gateway":"192.0.2.1","cidr":"192.0.2.11/24"}]`,
		"/cluster/firewall/options": `{"enable":1}`,
		"/cluster/firewall/rules":   `[]`,
		"/cluster/sdn/vnets":        `[]`,
		"/storage": `[{"storage":"local","type":"dir","content":"iso,import"},{"storage":"local-lvm","type":"lvmthin","content":"images"},
			{"storage":"nas-iso","type":"nfs","content":"iso","shared":1}]`,
		"/cluster/resources": `[{"type":"storage","storage":"local","node":"pve1","status":"available"},
			{"type":"storage","storage":"local-lvm","node":"pve1","status":"available"},{"type":"storage","storage":"nas-iso","node":"pve1","status":"available"}]`,
		// The two token rows are what an older setup left on the lab (M0): they reach every store and every tenant pool.
		"/access/acl": `[{"path":"/storage/local","type":"user","ugid":"capmox@pve","roleid":"PVEDatastoreAdmin","propagate":0},
			{"path":"/storage","type":"token","ugid":"capmox@pve!capi","roleid":"ProxCloudCAPI","propagate":1},
			{"path":"/pool","type":"token","ugid":"capmox@pve!capi","roleid":"ProxCloudCAPI","propagate":1},
			{"path":"/storage/local","type":"token","ugid":"capmox@pve!capi","roleid":"PVEDatastoreAdmin","propagate":0}]`,
		"/access/users/capmox@pve": `{"groups":["proxcloud-kube"]}`,
		"/access/users/csi@pve":    `{"groups":["proxcloud-kube","ops"]}`,
		"/access/roles":            `[{"roleid":"PVEDatastoreAdmin","privs":"Datastore.Allocate,Datastore.AllocateSpace,Datastore.AllocateTemplate,Datastore.Audit"}]`,
	}
	if f.bootISO {
		data["/storage"] = strings.Replace(data["/storage"], `"content":"images"`, `"content":"images,iso"`, 1)
	}
	if f.vnets != "" {
		data["/cluster/sdn/vnets"] = f.vnets
	}
	data["/cluster/sdn/zones"] = "[]"
	if f.zones != "" {
		data["/cluster/sdn/zones"] = f.zones
	}
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	if path == "/cluster/nextid" && f.vm9999 {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"data":null,"errors":{"vmid":"VM 9999 already exists"}}`)
		return
	}
	data["/cluster/nextid"] = `"` + r.URL.Query().Get("vmid") + `"`
	body, ok := data[path]
	if path == "/access/permissions" {
		p := r.URL.Query().Get("path")
		privs := map[string]int{}
		for _, v := range f.perms[token][p] {
			privs[v] = 1
		}
		b, _ := json.Marshal(map[string]any{p: privs})
		body, ok = string(b), true
	}
	if f.outside != "" && path == "/access/users/"+f.outside || body == "!" {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"data":null}`)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotImplemented)
		fmt.Fprintf(w, `{"errors":{"path":"no fake for %s"}}`, path)
		return
	}
	fmt.Fprintf(w, `{"data":%s}`, body)
}

func checkMap(checks []Check) map[string]Check {
	out := map[string]Check{}
	for _, c := range checks {
		out[c.ID] = c
	}
	return out
}

// A site set up by the managed-services script passes every check, writing nothing; a missing grant, a portal token
// on the seed's pool and platform addresses outside blockedCidrs each fail their own row.
func TestPreflight(t *testing.T) {
	in := validInstallation(t)
	in.Spec.Storage.Import = "local"
	for i := range in.Spec.Clusters[0].NodeGroups {
		in.Spec.Clusters[0].NodeGroups[i].Nodes = []string{"pve1"}
	}
	s := in.Spec.Storage
	grant := func(checks []pve.PrivCheck) map[string][]string {
		out := map[string][]string{}
		for _, c := range checks {
			out[c.Path] = append(out[c.Path], c.Privs...)
		}
		return out
	}
	capi := grant(pve.CAPIPrivileges(s.Boot, s.ISO))
	f := &preflightPVE{perms: map[string]map[string][]string{
		pve.CAPITokenID:            capi,
		pve.CSITokenID:             grant(pve.CSIPrivileges(s.Boot)),
		pve.DefaultPlatformTokenID: grant(pve.KubeMintPrivileges("nas-templates")),
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	pf := Preflight{
		Installation:      in,
		Site:              Site{CIDRPool: "10.100.0.0/16", Blocked: []string{"10.200.200.0/25"}},
		ProviderImport:    "nas-templates",
		Portal:            &pve.Service{C: f.client(t, srv.URL, pve.DefaultPlatformTokenID), ServiceID: "svc-pve"},
		CAPI:              f.client(t, srv.URL, pve.CAPITokenID),
		CSI:               f.client(t, srv.URL, pve.CSITokenID),
		TemplateUpdatesVM: true, GitHub: true,
		Blocked: func(context.Context, string) (bool, error) { return false, nil },
	}
	ctx := context.Background()
	checks := pf.Run(ctx)
	if !OK(checks) {
		t.Fatalf("a set-up site: %+v", checks)
	}
	if got := len(checks); got != 17 {
		t.Errorf("%d rows, want 17: %+v", got, checks)
	}
	if c := checkMap(checks)["csi-vmid"]; c.Status != CheckOK {
		t.Errorf("VM id 9999 free: %+v", c)
	}
	// The owner's own template on VM id 9999 (production acceptance, A2): the CSI volumes are named as its disks. A
	// warning that says what must never be done, and the preflight still passes.
	f.vm9999 = true
	held := pf.Run(ctx)
	if c := checkMap(held)["csi-vmid"]; c.Status != CheckWarn || !strings.Contains(c.Message, "vm-9999-pvc-") ||
		!strings.Contains(c.Message, "Never destroy VM 9999") || !strings.Contains(c.Message, "--destroy-unreferenced-disks 1") || !OK(held) {
		t.Errorf("VM id 9999 in use: %+v ok=%v", c, OK(held))
	}
	f.vm9999 = false
	if c := checkMap(checks)["isolation"]; !strings.Contains(c.Message, "first tenant network") {
		t.Errorf("no tenant network yet: %+v", c)
	}
	if len(f.writes) != 0 {
		t.Fatalf("the preflight wrote to PVE: %v", f.writes)
	}
	// A simple default zone: tenant clusters cannot be created on it. A warning, and the preflight still passes.
	pf.Site.DefaultZone, pf.Site.DefaultZoneKind = "pcm0", "simple"
	warned := pf.Run(ctx)
	if c := checkMap(warned)["tenant-network"]; c.Status != CheckWarn || !strings.Contains(c.Message, "pcm0") || !strings.Contains(c.Message, "EVPN or a VLAN zone") || !OK(warned) {
		t.Errorf("a simple default zone: %+v ok=%v", c, OK(warned))
	}
	if c := checkMap(warned)["tenant-network"]; !strings.Contains(c.Message, "cluster.shared") || strings.Contains(c.Message, "every cluster create") {
		t.Errorf("the warning must name the shared-workers exception and not claim every create: %+v", c)
	}
	// The create reads the live zone's dhcp field, and an existing zone is never changed: so does the row.
	f.zones = `[{"zone":"pcm0","type":"simple"}]`
	if c := checkMap(pf.Run(ctx))["tenant-network"]; c.Status != CheckOK {
		t.Errorf("a simple zone made without DHCP: %+v", c)
	}
	pf.Site.DefaultZoneKind = "evpn"
	f.zones = `[{"zone":"pcm0","type":"simple","dhcp":"dnsmasq"}]`
	if c := checkMap(pf.Run(ctx))["tenant-network"]; c.Status != CheckWarn || !strings.Contains(c.Message, "has DHCP") {
		t.Errorf("a live zone with DHCP under another configured kind: %+v", c)
	}
	f.zones = "!"
	if got := pf.Run(ctx); checkMap(got)["tenant-network"].Status != CheckWarn || !strings.Contains(checkMap(got)["tenant-network"].Message, "Could not read") || !OK(got) {
		t.Errorf("zones unreadable: %+v", checkMap(got)["tenant-network"])
	}
	f.zones = ""
	if c := checkMap(pf.Run(ctx))["tenant-network"]; c.Status != CheckOK {
		t.Errorf("an EVPN default zone not made yet: %+v", c)
	}
	pf.Site.DefaultZone, pf.Site.DefaultZoneKind = "", ""
	// A provider sync skips a disabled provider, so the row must not send the operator to one without saying so.
	pf.ProviderDisabled = true
	if c := checkMap(pf.Run(ctx))["isolation"]; c.Status != CheckOK || !strings.Contains(c.Message, "provider is disabled") {
		t.Errorf("a disabled provider, no tenant network yet: %+v", c)
	}
	f.vnets = `[{"vnet":"vother1","zone":"pc8","alias":"st:other:web"}]`
	if c := checkMap(pf.Run(ctx))["isolation"]; c.Status != CheckFail || !strings.Contains(c.Message, "provider is disabled") || !strings.Contains(c.Message, "0 in place") {
		t.Errorf("a disabled provider on a cluster with another portal's tenant network: %+v", c)
	}
	pf.ProviderDisabled = false
	if c := checkMap(pf.Run(ctx))["isolation"]; c.Status != CheckFail || strings.Contains(c.Message, "disabled") {
		t.Errorf("an enabled provider on a cluster with another portal's tenant network: %+v", c)
	}
	f.vnets = ""

	// A site an earlier script set up: the mint role on /access/groups, which reaches every user. Minting works there
	// (the group's path inherits it), so only this row tells the operator.
	f.perms[pve.DefaultPlatformTokenID]["/access/groups"] = []string{"User.Modify", "Permissions.Modify"}
	if c := checkMap(pf.Run(ctx)); c["mint-scope"].Status != CheckFail || c["privileges:kubemint"].Status != CheckOK ||
		!strings.Contains(c["mint-scope"].Message, "the portal token holds User.Modify, Permissions.Modify on /access/groups") ||
		!strings.Contains(c["mint-scope"].Message, "Run the managed-services script again as root on a node (GET /api/v1/admin/setup/proxmox-script?managedServices=true&bootStorage=<store>") {
		t.Errorf("the mint role on /access/groups: %+v, %+v", c["mint-scope"], c["privileges:kubemint"])
	}
	delete(f.perms[pve.DefaultPlatformTokenID], "/access/groups")
	// An operator took csi@pve out of the group (pveum user modify without --append): the privileges read complete,
	// yet PVE refuses every token delete for it, so a cluster release fails.
	f.outside = "csi@pve"
	if c := checkMap(pf.Run(ctx)); c["mint-scope"].Status != CheckFail || c["privileges:kubemint"].Status != CheckOK ||
		!strings.Contains(c["mint-scope"].Message, "csi@pve is not in the group proxcloud-kube") ||
		!strings.Contains(c["mint-scope"].Message, "pveum user modify csi@pve --groups proxcloud-kube --append 1") ||
		strings.Contains(c["mint-scope"].Message, "capmox@pve") {
		t.Errorf("a service user outside the group: %+v", c["mint-scope"])
	}
	f.outside = ""
	// The same site's row on /pool: the portal needs no Permissions.Modify there.
	onPool := f.perms[pve.DefaultPlatformTokenID]["/pool"]
	f.perms[pve.DefaultPlatformTokenID]["/pool"] = append(slices.Clone(onPool), "Permissions.Modify")
	if c := checkMap(pf.Run(ctx)); c["mint-scope"].Status != CheckFail || c["privileges:kubemint"].Status != CheckOK ||
		!strings.Contains(c["mint-scope"].Message, "the portal token holds Permissions.Modify on /pool: ") {
		t.Errorf("the mint role on /pool: %+v, %+v", c["mint-scope"], c["privileges:kubemint"])
	}
	f.perms[pve.DefaultPlatformTokenID]["/pool"] = onPool

	capi["/pool/"+pve.SeedPool] = []string{"VM.Audit"}
	f.perms[pve.DefaultPlatformTokenID]["/pool/"+pve.SeedPool] = []string{"VM.Audit"}
	f.perms[pve.CAPITokenID]["/storage/nas-iso"] = []string{"Datastore.Audit"}
	f.perms[pve.CAPITokenID]["/pool"] = []string{"VM.Audit", "VM.Allocate"}
	// The mint role is checked on the provider's import store, not on the one of the settings ("local").
	delete(f.perms[pve.DefaultPlatformTokenID], "/storage/nas-templates")
	pf.Site.Blocked = nil
	pf.Blocked = func(context.Context, string) (bool, error) { return true, nil }
	// Each token is asked about each of the three paths: the CSI token on /storage, the CAPI token on /vms.
	f.perms[pve.CSITokenID]["/storage"] = []string{"Datastore.Audit"}
	f.perms[pve.CAPITokenID]["/vms"] = []string{"VM.Audit"}
	scope := checkMap(pf.Run(ctx))["token-scope"]
	for _, want := range []string{pve.CAPITokenID + " holds 2 privileges on /pool", pve.CAPITokenID + " holds 1 privileges on /vms", pve.CSITokenID + " holds 1 privileges on /storage"} {
		if scope.Status != CheckFail || !strings.Contains(scope.Message, want) {
			t.Errorf("token-scope: want a failure naming %q, got %+v", want, scope)
		}
	}
	delete(f.perms[pve.CAPITokenID], "/vms")
	pf.CSI = nil
	got := checkMap(pf.Run(ctx))
	for id, want := range map[string]string{
		"privileges:capi":      "on /pool/" + pve.SeedPool,
		"privileges:kubemint":  "on /storage/nas-templates",
		"pool-fence":           "the portal token holds 1 privileges on /pool/" + pve.SeedPool,
		"iso-store-visibility": "[local, nas-iso]",
		"token-scope":          pve.CAPITokenID + " holds 2 privileges on /pool",
		"isolation":            "add 10.200.200.10/32, 10.200.200.20/32, 10.200.200.30/31",
		"credentials":          pve.CSITokenID + " is not stored",
		"images":               "is on the blocklist",
	} {
		if c := got[id]; c.Status != CheckFail || !strings.Contains(c.Message, want) {
			t.Errorf("%s: want a failure naming %q, got %+v", id, want, c)
		}
	}
	// The script's address as the API serves it: /api/v1 and the boot store, without which it answers 400.
	if m := got["privileges:kubemint"].Message; !strings.Contains(m, "GET /api/v1/admin/setup/proxmox-script?managedServices=true&bootStorage="+s.Boot+",") {
		t.Errorf("privileges:kubemint does not say where the script is: %s", m)
	}
	// Both name the row to remove and the command, and never a row the script writes (the token's own on the ISO store).
	if m := got["token-scope"].Message; !strings.Contains(m, "pveum acl delete /pool --tokens 'capmox@pve!capi' --roles ProxCloudCAPI") || strings.Contains(m, "delete /storage") {
		t.Errorf("token-scope does not name the /pool row alone: %s", m)
	}
	if m := got["iso-store-visibility"].Message; !strings.Contains(m, "pveum acl delete /storage --tokens 'capmox@pve!capi' --roles ProxCloudCAPI") || strings.Contains(m, "delete /storage/local") {
		t.Errorf("iso-store-visibility does not name the /storage row alone: %s", m)
	}
	if _, ok := got["privileges:csi"]; ok {
		t.Error("no CSI token, yet its privileges were checked")
	}

	// The boot store takes ISO images and is the only extra store the token sees: the row behind it is the script's,
	// so no removal is advised, neither a command nor the recipe that finds a row.
	delete(f.perms[pve.CAPITokenID], "/storage/nas-iso")
	f.bootISO = true
	c := checkMap(pf.Run(ctx))["iso-store-visibility"]
	if c.Status != CheckFail || !strings.Contains(c.Message, "[local, local-lvm]") || !strings.Contains(c.Message, "boot-iso") ||
		strings.Contains(c.Message, "pveum acl") {
		t.Errorf("a boot store that takes ISO images: %+v", c)
	}
}

// SiteOf carries the kind of the zone tenant networks are made in: the tenant-network row reads it.
func TestSiteOfDefaultZone(t *testing.T) {
	es := func(network map[string]any) *externalservice.ExternalService {
		return &externalservice.ExternalService{Config: map[string]any{"network": network}}
	}
	if s := SiteOf(es(map[string]any{"zones": map[string]any{"pcm0": map[string]any{"kind": "simple"}}})); s.DefaultZone != "pcm0" || s.DefaultZoneKind != "simple" {
		t.Errorf("one simple zone: %+v", s)
	}
	if s := SiteOf(es(map[string]any{"defaultZone": "overlay", "zones": map[string]any{"overlay": map[string]any{}, "s": map[string]any{"kind": "simple"}}})); s.DefaultZoneKind != "evpn" {
		t.Errorf("a default zone without a kind is EVPN: %+v", s)
	}
	if s := SiteOf(es(nil)); s.DefaultZone != "" || s.DefaultZoneKind != "" {
		t.Errorf("no zone: %+v", s)
	}
}
