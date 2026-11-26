package platformlife

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

var testArgo = &ArgoCDAccess{Address: "10.200.200.210", Sources: []string{"192.168.33.0/24", "10.8.0.0/24"}}

func workloadArgoValues(t *testing.T, in RenderInput) map[string]any {
	t.Helper()
	apps, err := Applications(plain(t), in)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(apps, func(a map[string]any) bool { return str(a, "metadata", "name") == ClusterName+"-argocd" })
	return dig(apps[i], "spec", "source", "helm", "valuesObject").(map[string]any)
}

// With spec.access.argocd the workload ArgoCD's Service is MetalLB's with the address and the source ranges, and
// server.insecure is gone; without the block the values are today's (TestRenderGolden holds them byte for byte).
func TestArgoCDAccessRender(t *testing.T) {
	in := renderInput()
	if v := workloadArgoValues(t, in); v["server"] != nil || dig(v, "configs", "params", "server.insecure") != true {
		t.Fatalf("without the block: %v", v)
	}
	in.ArgoCD = testArgo
	v := workloadArgoValues(t, in)
	if dig(v, "configs", "params") != nil || dig(v, "configs", "cm") == nil {
		t.Fatalf("configs with the block: %v", v["configs"])
	}
	golden(t, "render-argocd-access.golden.yaml", yamlDoc(t, map[string]any{"server": v["server"], "configs.params": dig(v, "configs", "params")}))
}

func TestArgoCDAccessValidate(t *testing.T) {
	set := func(addr string, sources ...string) func(*Installation) {
		return func(in *Installation) {
			in.Spec.Access.ArgoCD = &ArgoCDAccess{Address: addr, Sources: sources}
			in.Normalize()
		}
	}
	for name, tc := range map[string]struct {
		mut  func(*Installation)
		want string // "" = valid
	}{
		"set":                   {set("10.200.200.210", "192.168.33.0/24"), ""},
		"an empty block":        {set(""), ""},
		"outside the range":     {set("10.200.200.50", "192.168.33.0/24"), "spec.access.argocd.address 10.200.200.50 is outside spec.clusters[0].metallbRange"},
		"everyone":              {set("10.200.200.210", "0.0.0.0/0"), `spec.access.argocd.sources "0.0.0.0/0" is not an IPv4 CIDR narrower than /0`},
		"no sources":            {set("10.200.200.210"), "spec.access.argocd.sources: one to 16 CIDRs"},
		"no address":            {set("", "192.168.33.0/24"), `spec.access.argocd.address "" is not an IPv4 address`},
		"IPv6 address":          {set("fd00::10", "192.168.33.0/24"), `spec.access.argocd.address "fd00::10" is not an IPv4 address`},
		"IPv6 source":           {set("10.200.200.210", "fd00::/64"), `spec.access.argocd.sources "fd00::/64" is not an IPv4 CIDR`},
		"a source not a CIDR":   {set("10.200.200.210", "192.168.33.7"), `spec.access.argocd.sources "192.168.33.7"`},
		"a name for an address": {set("argocd.example", "192.168.33.0/24"), "spec.access.argocd.address"},
	} {
		in := validInstallation(t)
		tc.mut(in)
		err := in.Validate()
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	// The block is not fixed: set, changed and cleared on an installed platform.
	old, next := validInstallation(t), validInstallation(t)
	cur, _ := Current()
	old.Status.BOM = &BOMRef{Release: cur.Release, SHA256: cur.SHA256}
	next.Spec.Access.ArgoCD = testArgo
	if f := append(FixedChanges(old, next), FixedChanges(next, old)...); len(f) != 0 {
		t.Errorf("fixed: %v", f)
	}
}

// The plan row access.argocd: for a set, a change (of the address, of the sources alone) and a clear on an installed
// platform; not when the settings are what a run built, and not before the first install.
func TestArgoCDAccessPlan(t *testing.T) {
	cur, _ := Current()
	row := func(spec *ArgoCDAccess, url string, sources []string, installed bool) *PlanChange {
		in := validInstallation(t)
		in.Spec.Access.ArgoCD = spec
		if installed {
			in.Status.BOM = &BOMRef{Release: cur.Release, SHA256: cur.SHA256}
			in.Status.Clusters = []ClusterStatus{{Name: ClusterName}}
		}
		in.Status.ArgoCDURL, in.Status.ArgoCDSources = url, sources
		plan, err := PlanFor(in, "")
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(plan.Changes, func(c PlanChange) bool { return c.Name == "access.argocd" })
		if i < 0 {
			return nil
		}
		return &plan.Changes[i]
	}
	if c := row(nil, "", nil, true); c != nil {
		t.Errorf("no block, nothing built: %+v", c)
	}
	if c := row(testArgo, "", nil, false); c != nil {
		t.Errorf("a new installation lists components only: %+v", c)
	}
	if c := row(testArgo, testArgo.URL(), testArgo.Sources, true); c != nil {
		t.Errorf("as built: %+v", c)
	}
	c := row(testArgo, "", nil, true)
	if c == nil || c.From != "" || c.To != "https://10.200.200.210" || c.Class != ClassChart || c.Stage != stageWorkloadPlatform || c.Cluster != ClusterName ||
		!strings.Contains(c.Impact, "ArgoCD restarts") || !strings.Contains(c.Impact, "Only 192.168.33.0/24, 10.8.0.0/24 are let in") ||
		!strings.Contains(c.Impact, "self-signed") || !strings.Contains(c.Impact, CodeArgoCDAddressUnavailable) {
		t.Errorf("set: %+v", c)
	}
	if c := row(testArgo, "https://10.200.200.211", testArgo.Sources, true); c == nil || c.From != "https://10.200.200.211" || c.To != "https://10.200.200.210" ||
		!strings.HasPrefix(c.Impact, "ArgoCD does not restart.") {
		t.Errorf("another address: %+v", c)
	}
	if c := row(testArgo, testArgo.URL(), []string{"192.168.33.0/24"}, true); c == nil || c.From != c.To || !strings.Contains(c.Impact, "10.8.0.0/24") {
		t.Errorf("other sources: %+v", c)
	}
	if c := row(nil, testArgo.URL(), testArgo.Sources, true); c == nil || c.From != testArgo.URL() || c.To != "" || !strings.Contains(c.Impact, "ArgoCD restarts") {
		t.Errorf("clear: %+v", c)
	}
	// Rendered by a run that did not get the address (sources without a URL): a clear still restarts ArgoCD.
	if c := row(nil, "", testArgo.Sources, true); c == nil || c.To != "" {
		t.Errorf("clear after argocd_address_unavailable: %+v", c)
	}
}

const argoServicePath = "/api/v1/namespaces/argocd/services/argocd-server"

func argoService(ingress ...string) map[string]any {
	svc := map[string]any{"metadata": map[string]any{"name": argoServer, "namespace": argoNamespace, "annotations": map[string]any{argoAddressAnnotation: testArgo.Address}},
		"spec": map[string]any{"type": "LoadBalancer"}}
	if len(ingress) > 0 {
		svc["status"] = map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": ingress[0]}}}}
	}
	return svc
}

// Stage 9 with the block: a Service without the address waits with argocd_address_unavailable, names the Service that
// holds it, and fails with that code at the deadline, also while ArgoCD still calls the Application Progressing.
func TestArgoCDAddressUnavailable(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageWorkloadPlatform)
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Access.ArgoCD = testArgo })
	// Not synced yet (the Service is still a ClusterIP): the wave's own wait.
	p.seed.unsynced["pc-platform-argocd"] = true
	p.wl.put(argoServicePath, map[string]any{"metadata": map[string]any{"name": argoServer}, "spec": map[string]any{"type": "ClusterIP"}})
	if end := p.once(); end != "defer:waiting_workload_platform" || p.stage(stageWorkloadPlatform).Code != CodeComponentUnhealthy {
		t.Fatalf("before the sync: %s %+v", end, p.stage(stageWorkloadPlatform))
	}
	// Synced, no address, and ArgoCD keeps the Application Progressing for it.
	p.wl.put(argoServicePath, argoService())
	p.wl.put("/api/v1/services/tenant-lb", map[string]any{"metadata": map[string]any{"name": "api", "namespace": "tenant-a"},
		"status": map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": testArgo.Address}}}}})
	check := func(when string) {
		t.Helper()
		s := p.stage(stageWorkloadPlatform)
		if s.Code != CodeArgoCDAddressUnavailable || !strings.Contains(s.Message, "10.200.200.210 was not given to the platform's ArgoCD: Service tenant-a/api holds it. Pick a free address") {
			t.Fatalf("%s: %+v", when, s)
		}
		if _, in, _ := p.state.snapshot(); in.Status.ArgoCDURL != "" || !slices.Equal(in.Status.ArgoCDSources, testArgo.Sources) {
			t.Fatalf("%s: status %q %v", when, in.Status.ArgoCDURL, in.Status.ArgoCDSources)
		}
	}
	if end := p.once(); end != "defer:waiting_workload_platform" {
		t.Fatalf("end %s", end)
	}
	check("the wave pending")
	delete(p.seed.unsynced, "pc-platform-argocd")
	if end := p.once(); end != "defer:waiting_workload_platform" {
		t.Fatalf("end %s", end)
	}
	check("the wave passed")
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		for i := range run.Stages {
			if past := time.Now().Add(-time.Second); run.Stages[i].ID == stageWorkloadPlatform {
				run.Stages[i].Deadline = &past
			}
		}
	})
	if end := p.once(); end != "fail:"+CodeArgoCDAddressUnavailable {
		t.Fatalf("after the deadline: %s", end)
	}
}

// With the address on the Service the run records status.argocdUrl; an apply with the block cleared clears it; a
// platform that never set the block reads no Service and records nothing.
func TestArgoCDAddressRecorded(t *testing.T) {
	p := newPlatform(t, RunApply)
	p.only(stageWorkloadPlatform)
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Access.ArgoCD = testArgo })
	p.wl.put(argoServicePath, argoService(testArgo.Address))
	if end := p.until(10); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
	if _, in, _ := p.state.snapshot(); in.Status.ArgoCDURL != "https://10.200.200.210" || !slices.Equal(in.Status.ArgoCDSources, testArgo.Sources) {
		t.Fatalf("status %q %v", in.Status.ArgoCDURL, in.Status.ArgoCDSources)
	}
	app := p.seed.obj(appsPath + "pc-platform-argocd")
	if dig(app, "spec", "source", "helm", "valuesObject", "server", "service", "type") != "LoadBalancer" {
		t.Fatalf("the Application does not carry the address: %v", dig(app, "spec", "source", "helm", "valuesObject"))
	}

	// Other networks, the Service still holds the address and the Application is not synced yet: the record keeps the
	// networks that are in force, so the plan keeps its row if the run ends here.
	p = newPlatform(t, RunApply)
	p.only(stageWorkloadPlatform)
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) {
		in.Spec.Access.ArgoCD = &ArgoCDAccess{Address: testArgo.Address, Sources: []string{"10.8.0.0/24"}}
		in.Status.ArgoCDURL, in.Status.ArgoCDSources = testArgo.URL(), testArgo.Sources
	})
	p.wl.put(argoServicePath, argoService(testArgo.Address))
	p.seed.unsynced["pc-platform-argocd"] = true
	if end := p.once(); end != "defer:waiting_workload_platform" || p.stage(stageWorkloadPlatform).Code != CodeComponentUnhealthy {
		t.Fatalf("not synced: %s %+v", end, p.stage(stageWorkloadPlatform))
	}
	if _, in, _ := p.state.snapshot(); !slices.Equal(in.Status.ArgoCDSources, testArgo.Sources) {
		t.Fatalf("networks recorded before the sync: %v", in.Status.ArgoCDSources)
	}
	delete(p.seed.unsynced, "pc-platform-argocd")
	if end := p.until(10); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
	if _, in, _ := p.state.snapshot(); !slices.Equal(in.Status.ArgoCDSources, []string{"10.8.0.0/24"}) {
		t.Fatalf("networks after the sync: %v", in.Status.ArgoCDSources)
	}

	p = newPlatform(t, RunApply)
	p.only(stageWorkloadPlatform)
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) {
		in.Status.ArgoCDURL, in.Status.ArgoCDSources = testArgo.URL(), testArgo.Sources
	})
	if end := p.until(10); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
	if _, in, _ := p.state.snapshot(); in.Status.ArgoCDURL != "" || in.Status.ArgoCDSources != nil {
		t.Fatalf("status after the clear: %q %v", in.Status.ArgoCDURL, in.Status.ArgoCDSources)
	}
	if v := dig(p.seed.obj(appsPath+"pc-platform-argocd"), "spec", "source", "helm", "valuesObject").(map[string]any); v["server"] != nil ||
		dig(v, "configs", "params", "server.insecure") != true {
		t.Fatalf("values after the clear: %v", v)
	}
}

// The row argocd-access: absent without the block, a failure on what Validate refuses, a warning when an operator
// network contains a Proxmox node's address (the SNAT case) or the nodes were not read, ok otherwise.
func TestArgoCDAccessCheck(t *testing.T) {
	in := validInstallation(t)
	nodes := []string{"192.0.2.11/24", "192.168.33.5/32"}
	if _, has := in.argoAccessCheck(nodes, false); has {
		t.Fatal("a row without the block")
	}
	in.Spec.Access.ArgoCD = &ArgoCDAccess{Address: "10.200.200.210", Sources: []string{"10.8.0.0/24"}}
	if c, has := in.argoAccessCheck(nodes, false); !has || c.ID != "argocd-access" || c.Status != CheckOK ||
		!strings.Contains(c.Message, "https://10.200.200.210 for 10.8.0.0/24 only") {
		t.Fatalf("ok: %+v", c)
	}
	if c, _ := in.argoAccessCheck(nil, true); c.Status != CheckWarn || !strings.Contains(c.Message, "could not be read") {
		t.Fatalf("unread: %+v", c)
	}
	in.Spec.Access.ArgoCD.Sources = []string{"192.168.33.0/24"}
	if c, _ := in.argoAccessCheck(nodes, false); c.Status != CheckWarn || !strings.Contains(c.Message, "192.168.33.5 (in 192.168.33.0/24)") ||
		!strings.Contains(c.Message, "SNAT") || strings.Contains(c.Message, "192.0.2.11") {
		t.Fatalf("a node inside the operator network: %+v", c)
	}
	// A source stored with host bits before the save refused it: Validate still takes the spec (the platform must
	// plan and upgrade), the row warns with the form to write, and the node warning stays beside it.
	in.Spec.Access.ArgoCD.Sources = []string{"10.8.0.5/24"}
	if err := in.Validate(); err != nil {
		t.Fatalf("a stored source with host bits fails Validate: %v", err)
	}
	if c, _ := in.argoAccessCheck(nodes, false); c.Status != CheckWarn || !strings.Contains(c.Message, "10.8.0.5/24 has host bits set: use 10.8.0.0/24") ||
		!strings.Contains(c.Message, "Kubernetes 1.36") {
		t.Fatalf("a stored source with host bits: %+v", c)
	}
	in.Spec.Access.ArgoCD.Sources = []string{"192.168.33.5/24"}
	if c, _ := in.argoAccessCheck(nodes, false); c.Status != CheckWarn || !strings.Contains(c.Message, "SNAT") || !strings.Contains(c.Message, "use 192.168.33.0/24") {
		t.Fatalf("host bits and a node inside: %+v", c)
	}
	in.Spec.Access.ArgoCD.Sources = []string{"192.168.33.0/24"}
	in.Spec.Access.ArgoCD.Address = "10.200.200.50"
	if c, _ := in.argoAccessCheck(nodes, false); c.Status != CheckFail || !strings.Contains(c.Message, "outside spec.clusters[0].metallbRange") {
		t.Fatalf("an address Validate refuses: %+v", c)
	}
}

// The row through Preflight.Run on a bridge-mode site, whose nodes' addresses no other row reads: an operator network
// that is the node's own address warns, though it contains the network address of no management network.
func TestArgoCDAccessCheckReadsNodeAddresses(t *testing.T) {
	in := validInstallation(t)
	for i := range in.Spec.Clusters[0].NodeGroups {
		in.Spec.Clusters[0].NodeGroups[i].Nodes = []string{"pve1"}
	}
	in.Spec.Access.ArgoCD = &ArgoCDAccess{Address: "10.200.200.210", Sources: []string{"192.0.2.11/32"}}
	f := &preflightPVE{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	pf := Preflight{Installation: in, Site: Site{CIDRPool: "10.100.0.0/16"}, Portal: &pve.Service{C: f.client(t, srv.URL, pve.DefaultPlatformTokenID)}}
	if c := checkMap(pf.Run(context.Background()))["argocd-access"]; c.Status != CheckWarn || !strings.Contains(c.Message, "192.0.2.11 (in 192.0.2.11/32)") {
		t.Fatalf("the node's own address as an operator network: %+v", c)
	}
}

// The save's rule for spec.access.argocd.sources: a new entry with host bits is refused with the form to write, an
// entry the stored spec already has passes, alone or beside a new canonical one.
func TestArgoSourceProblems(t *testing.T) {
	in := validInstallation(t)
	if p := in.ArgoSourceProblems(nil); p != nil {
		t.Fatalf("without the block: %v", p)
	}
	in.Spec.Access.ArgoCD = &ArgoCDAccess{Address: "10.200.200.210", Sources: []string{"10.8.0.0/24", "192.168.33.5/24"}}
	if p := in.ArgoSourceProblems(nil); len(p) != 1 || p[0] != "spec.access.argocd.sources: 192.168.33.5/24 has host bits set: use 192.168.33.0/24" {
		t.Fatalf("a create with host bits: %v", p)
	}
	stored := &Spec{}
	if p := in.ArgoSourceProblems(stored); len(p) != 1 {
		t.Fatalf("a stored spec without the block: %v", p)
	}
	stored.Access.ArgoCD = &ArgoCDAccess{Address: "10.200.200.210", Sources: []string{"192.168.33.5/24"}}
	if p := in.ArgoSourceProblems(stored); p != nil {
		t.Fatalf("the stored entry beside a new canonical one: %v", p)
	}
	in.Spec.Access.ArgoCD.Sources = append(in.Spec.Access.ArgoCD.Sources, "10.9.0.1/24")
	if p := in.ArgoSourceProblems(stored); len(p) != 1 || !strings.Contains(p[0], "use 10.9.0.0/24") {
		t.Fatalf("a new entry with host bits beside the stored one: %v", p)
	}
}
