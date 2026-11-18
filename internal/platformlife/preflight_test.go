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
	// machines are the VMs of pool proxcloud-system, VMID → {config JSON, status}; the one with VMID seed is in the
	// seed's pool instead.
	machines map[int][2]string
	seed     int
	extra    map[string]string // further paths, and other answers for the ones above; "!" answers 403
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
		"/cluster/status":                       `[{"type":"node","name":"pve1","ip":"192.0.2.11"}]`,
		"/nodes/pve1/network":                   `[{"iface":"vmbr0","address":"192.0.2.11","gateway":"192.0.2.1","cidr":"192.0.2.11/24"},{"iface":"vmbr1","type":"bridge"}]`,
		"/cluster/firewall/options":             `{"enable":1}`,
		"/cluster/firewall/rules":               `[]`,
		"/nodes/pve1/storage/local-lvm/content": `[]`,
		"/cluster/sdn/vnets":                    `[]`,
		"/nodes":                                `[{"node":"pve1","status":"online","maxmem":68719476736,"mem":1073741824}]`,
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
	members := map[bool][]string{}
	for id, m := range f.machines {
		members[id == f.seed] = append(members[id == f.seed], fmt.Sprintf(`{"id":"qemu/%d","type":"qemu","vmid":%d,"node":"pve1"}`, id, id))
		data[fmt.Sprintf("/nodes/pve1/qemu/%d/config", id)] = m[0]
		data[fmt.Sprintf("/nodes/pve1/qemu/%d/status/current", id)] = `{"status":"` + m[1] + `"}`
	}
	data["/pools/"+pve.SeedPool] = `{"members":[` + strings.Join(members[true], ",") + `]}`
	data["/pools/"+pve.SystemPool] = `{"members":[` + strings.Join(members[false], ",") + `]}`
	for k, v := range f.extra {
		data[k] = v
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
		pve.DefaultPlatformTokenID: grant(pve.KubeMintPrivileges([]string{"nas-templates", "nvme-u2"}, []string{"pc"})),
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	pf := Preflight{
		Installation:      in,
		Site:              Site{CIDRPool: "10.100.0.0/16", Blocked: []string{"10.200.200.0/25"}},
		MintStores:        []string{"nas-templates", "nvme-u2"},
		MintZones:         []string{"pc"},
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
	if got := len(checks); got != 21 {
		t.Errorf("%d rows, want 21: %+v", got, checks)
	}
	for _, c := range checks {
		if c.Status != CheckOK {
			t.Errorf("a set-up site: row %s is %s: %s", c.ID, c.Status, c.Message)
		}
	}
	// The scheduler is the operator's call: a warning with the command while it is off, no row while it is on.
	pf.SchedulerOff = true
	if c := checkMap(pf.Run(ctx))["scheduler"]; c.Status != CheckWarn || !strings.Contains(c.Message, "--scheduler") || !strings.Contains(c.Message, "delete a project") {
		t.Errorf("scheduler off: %+v", c)
	}
	pf.SchedulerOff = false
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
	// The machines row: absent above (no VM yet). After a node reboot the machines without onboot are stopped: a
	// warning that names each with what to do, and the preflight still passes. Another installation's VM is not ours.
	tags := `"tags":"proxcloud;pc-platform;pc-platform-` + in.ID + `"`
	f.seed = 106
	f.machines = map[int][2]string{
		106: {`{"name":"pc-platform-seed","onboot":1,` + tags + `}`, "stopped"},
		107: {`{"name":"pc-platform-cp-1",` + tags + `}`, "stopped"},
		108: {`{"name":"pc-platform-kamaji-1","onboot":1,` + tags + `}`, "running"},
		109: {`{"name":"other","tags":"pc-platform-other"}`, "stopped"},
	}
	down := pf.Run(ctx)
	if c := checkMap(down)["machines"]; c.Status != CheckWarn || !OK(down) || !strings.Contains(c.Message, "VM 107 (pc-platform-cp-1) on pve1 is stopped") ||
		!strings.Contains(c.Message, "qm start 107") || !strings.Contains(c.Message, "qm set 107 --onboot 1") || strings.Contains(c.Message, "108") || strings.Contains(c.Message, "109") ||
		!strings.Contains(c.Message, "VM 106 (pc-platform-seed) on pve1 is stopped") || strings.Contains(c.Message, "qm set 106") {
		t.Errorf("a stopped machine without onboot, and the stopped seed in its own pool: %+v ok=%v", c, OK(down))
	}
	f.machines[106] = [2]string{`{"name":"pc-platform-seed","onboot":1,` + tags + `}`, "running"}
	f.machines[107] = [2]string{`{"name":"pc-platform-cp-1","onboot":1,` + tags + `}`, "running"}
	if c := checkMap(pf.Run(ctx))["machines"]; c.Status != CheckOK || !strings.Contains(c.Message, "3 VMs") {
		t.Errorf("every machine runs and has onboot: %+v", c)
	}
	// One VM that cannot be read is named and does not hide the others (the apply still sets onboot on those).
	f.machines[107] = [2]string{"!", "running"}
	f.machines[108] = [2]string{`{"name":"pc-platform-kamaji-1",` + tags + `}`, "running"}
	if c := checkMap(pf.Run(ctx))["machines"]; c.Status != CheckWarn || !strings.Contains(c.Message, "VM 107 on pve1 could not be read") || !strings.Contains(c.Message, "qm set 108 --onboot 1") {
		t.Errorf("a machine that cannot be read: %+v", c)
	}
	f.machines, f.seed = nil, 0
	// A simple default zone: tenant clusters cannot be created on it. A warning, and the preflight still passes.
	pf.Site.DefaultZone, pf.Site.DefaultZoneKind = "pcm0", "simple"
	warned := pf.Run(ctx)
	if c := checkMap(warned)["tenant-network"]; c.Status != CheckWarn || !strings.Contains(c.Message, "pcm0") || !strings.Contains(c.Message, "EVPN or a VLAN zone") || !OK(warned) {
		t.Errorf("a simple default zone: %+v ok=%v", c, OK(warned))
	}
	if c := checkMap(warned)["tenant-network"]; !strings.Contains(c.Message, "cluster.shared") || strings.Contains(c.Message, "every cluster create") {
		t.Errorf("the warning must name the shared-workers exception and not claim every create: %+v", c)
	}
	// "dhcp": false in the zone's config: ProxCloud makes it without DHCP, so nothing to warn about.
	pf.Site.DefaultZoneNoDHCP = true
	if c := checkMap(pf.Run(ctx))["tenant-network"]; c.Status != CheckOK || !strings.Contains(c.Message, "created without DHCP") {
		t.Errorf("a simple default zone configured without DHCP, not made yet: %+v", c)
	}
	pf.Site.DefaultZoneNoDHCP = false
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
	// The blocklist is asked about the images of the installed BOM, not of this build's own.
	old := newerBuild(t)
	in.Status.BOM = &BOMRef{Release: old.Release, SHA256: old.SHA256}
	var asked []string
	pf.Blocked = func(_ context.Context, digest string) (bool, error) { asked = append(asked, digest); return false, nil }
	pf.Run(ctx)
	if !slices.Equal(asked, []string{old.Seed.Image.Digest, old.Kubernetes.NodeImage.Digest}) {
		t.Errorf("blocklist asked about %v", asked)
	}
	// A blocked image of an installed platform: no release changes its images, and the message says so.
	pf.Blocked = func(context.Context, string) (bool, error) { return true, nil }
	if c := checkMap(pf.Run(ctx))["images"]; !strings.Contains(c.Message, "stays on release "+old.Release) || strings.Contains(c.Message, "install a newer") {
		t.Errorf("images of an installed platform: %+v", c)
	}
	in.Status.BOM = nil
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
		"isolation":            "10.200.200.10/32, 10.200.200.20/32, 10.200.200.30/31, 10.200.200.32/28, 10.200.200.48/31 are not in the provider's blockedCidrs. Saving the settings adds them",
		"credentials":          pve.CSITokenID + " is not stored: run the root command of step 2",
		"images":               "is on the blocklist: install a newer ProxCloud release",
	} {
		if c := got[id]; c.Status != CheckFail || !strings.Contains(c.Message, want) {
			t.Errorf("%s: want a failure naming %q, got %+v", id, want, c)
		}
	}
	// A single store reads apart from /storage, and the row says, once, why the grant on /storage does not reach it.
	// Two such lines, to see that it is once.
	delete(f.perms[pve.DefaultPlatformTokenID], "/storage/nvme-u2")
	if m := checkMap(pf.Run(ctx))["privileges:kubemint"].Message; strings.Count(m, pve.KubeMintShadowed) != 1 || strings.Count(m, " lacks ") != 2 {
		t.Errorf("privileges:kubemint with two stores of their own: %s", m)
	}
	f.perms[pve.DefaultPlatformTokenID]["/storage/nvme-u2"] = pve.ManagedRoles[2].Privs
	if m := got["privileges:kubemint"].Message; !strings.Contains(m, pve.KubeMintShadowed) || strings.Contains(m, "on /storage (") {
		t.Errorf("privileges:kubemint does not explain the store's own row: %s", m)
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

// MintScopeOf: the stores of every profile a boot disk may sit on, on every node, with the ISO and import stores, and
// the provider's zones. A capacity profile holds no boot disk, so no cluster token gets a row on its store.
func TestMintScopeOf(t *testing.T) {
	es := &externalservice.ExternalService{Config: map[string]any{
		"importStorage": "nas-templates",
		"storageProfiles": map[string]any{
			"fast":    map[string]any{"storages": map[string]any{"*": "nvme-u2", "pve2": "nvme"}},
			"archive": map[string]any{"bootDisk": false, "storages": map[string]any{"*": "truenas"}},
		},
		"network": map[string]any{"zones": map[string]any{"pc": map[string]any{}, "lan": map[string]any{"kind": "simple"}}},
	}}
	stores, zones := MintScopeOf(es)
	if want := []string{"local", "nas-templates", "nvme", "nvme-u2"}; !slices.Equal(stores, want) {
		t.Errorf("stores %v, want %v", stores, want)
	}
	if want := []string{"lan", "pc"}; !slices.Equal(zones, want) {
		t.Errorf("zones %v, want %v", zones, want)
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
	if s := SiteOf(es(map[string]any{"zones": map[string]any{"pc": map[string]any{"kind": "simple", "dhcp": false}}})); !s.DefaultZoneNoDHCP {
		t.Errorf("a simple zone with dhcp false: %+v", s)
	}
	if s := SiteOf(es(nil)); s.DefaultZone != "" || s.DefaultZoneKind != "" {
		t.Errorf("no zone: %+v", s)
	}
}

// The boot store row: a store Proxmox does not have and one without disk images fail with the command; disks of VMs
// that are neither the platform's machines nor ProxCloud templates nor CSI volumes warn and name the VMs; the rule row
// never fails and says who writes the rule.
func TestPreflightBootStoreAndAPIRule(t *testing.T) {
	in := validInstallation(t)
	in.Spec.Storage.Import = "local"
	for i := range in.Spec.Clusters[0].NodeGroups {
		in.Spec.Clusters[0].NodeGroups[i].Nodes = []string{"pve1"}
	}
	tags := `"tags":"pc-platform-` + in.ID + `"`
	f := &preflightPVE{seed: 106, machines: map[int][2]string{106: {`{"name":"seed","onboot":1,` + tags + `}`, "running"}}, extra: map[string]string{
		"/pools/" + pve.TemplatePool: `{"members":[{"id":"qemu/9100","type":"qemu","vmid":9100,"node":"pve1"}]}`,
		"/nodes/pve1/storage/local-lvm/content": `[{"volid":"local-lvm:vm-106-disk-0","vmid":106},{"volid":"local-lvm:base-9100-disk-0","vmid":9100},
			{"volid":"local-lvm:vm-9999-pvc-1f","vmid":9999}]`,
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	pf := Preflight{Installation: in, Site: Site{CIDRPool: "10.100.0.0/16"},
		Portal: &pve.Service{C: f.client(t, srv.URL, pve.DefaultPlatformTokenID)},
		CAPI:   f.client(t, srv.URL, pve.CAPITokenID), CSI: f.client(t, srv.URL, pve.CSITokenID)}
	ctx := context.Background()
	row := func(id string) Check { return checkMap(pf.Run(ctx))[id] }

	if c := row("boot-store"); c.Status != CheckOK || !strings.Contains(c.Message, "no disk of a VM that is not the platform's") {
		t.Errorf("the platform's own disks: %+v", c)
	}
	f.extra["/nodes/pve1/storage/local-lvm/content"] = `[{"volid":"local-lvm:vm-106-disk-0","vmid":106},{"volid":"local-lvm:vm-101-disk-0","vmid":101},
		{"volid":"local-lvm:vm-101-disk-1","vmid":101},{"volid":"local-lvm:vm-300-disk-0","vmid":300}]`
	if c := row("boot-store"); c.Status != CheckWarn || !strings.Contains(c.Message, "2 other VMs (VM 101, 300)") || !strings.Contains(c.Message, "Datastore.Allocate") ||
		!strings.Contains(c.Message, "pvesm add zfspool") {
		t.Errorf("foreign disks: %+v", c)
	}
	f.extra["/nodes/pve1/storage/local-lvm/content"] = "!"
	if c := row("boot-store"); c.Status != CheckWarn || !strings.Contains(c.Message, "could not be listed") {
		t.Errorf("a list that is refused: %+v", c)
	}
	// Without either token nothing is listed: without the CAPI token the platform's own machines would be "others".
	for _, without := range []**pve.Client{&pf.CAPI, &pf.CSI} {
		keep := *without
		*without = nil
		if c := row("boot-store"); c.Status != CheckOK || !strings.Contains(c.Message, "once the tokens "+pve.CAPITokenID+" and "+pve.CSITokenID+" are stored") {
			t.Errorf("a token is not stored: %+v", c)
		}
		*without = keep
	}
	pf.CSI = nil
	f.extra["/storage"] = `[{"storage":"local","type":"dir","content":"iso,import"},{"storage":"local-lvm","type":"lvmthin","content":"rootdir"}]`
	if c := row("boot-store"); c.Status != CheckFail || !strings.Contains(c.Message, "pvesm set local-lvm --content rootdir,images") {
		t.Errorf("no images content: %+v", c)
	}
	f.extra["/storage"] = `[{"storage":"local","type":"dir","content":"iso,import"}]`
	if c := row("boot-store"); c.Status != CheckFail || !strings.Contains(c.Message, "Proxmox has no storage local-lvm") {
		t.Errorf("a store that does not exist: %+v", c)
	}

	if c := row("pve-api-rule"); c.Status != CheckOK || !strings.Contains(c.Message, "Install writes the cluster firewall rule") || !strings.Contains(c.Message, "Nothing to do by hand") {
		t.Errorf("before the install: %+v", c)
	}
	in.Status.BOM = &BOMRef{Release: "x"}
	if c := row("pve-api-rule"); c.Status != CheckWarn || !strings.Contains(c.Message, "Press Apply") {
		t.Errorf("installed, rule missing: %+v", c)
	}
	src, dest, port := in.APIAccess()
	b, _ := json.Marshal([]pve.FirewallRule{pve.APIAccessRule(in.ID, src, dest, port)})
	f.extra["/cluster/firewall/rules"] = string(b)
	f.extra["/cluster/firewall/options"] = `{"enable":0}`
	if c := row("pve-api-rule"); c.Status != CheckOK || !strings.Contains(c.Message, "lets the seed and the node range reach") || !strings.Contains(c.Message, "datacenter firewall is off") {
		t.Errorf("rule present: %+v", c)
	}
	if len(f.writes) != 0 {
		t.Fatalf("the preflight wrote to PVE: %v", f.writes)
	}
}
