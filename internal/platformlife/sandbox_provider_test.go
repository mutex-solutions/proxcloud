package platformlife

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// The placement names the run writes are the ones the portal's sandbox code reads.
func TestSandboxModeNames(t *testing.T) {
	if SandboxShared != sandbox.ModeShared || GroupLabel != sandbox.LabelNodeGroup || SandboxGroup != sandbox.NodeGroupSandbox || SandboxTaintKey != sandbox.TaintSandbox ||
		sandboxNamespace != sandbox.SharedAccountNS || UnsecuredTaintKey != pve.SandboxUnsecuredTaint {
		t.Fatal("platformlife and the sandbox packages name the shared mode, the sandbox group or its taints differently")
	}
}

// installerTask finds a task of installer stage 60 by name.
func installerTask(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../installer/playbooks/60-register.yml")
	if err != nil {
		t.Fatal(err)
	}
	var plays []map[string]any
	if err := yaml.Unmarshal(raw, &plays); err != nil {
		t.Fatal(err)
	}
	var find func(tasks []any) map[string]any
	find = func(tasks []any) map[string]any {
		for _, raw := range tasks {
			task, _ := raw.(map[string]any)
			if task["name"] == name {
				return task
			}
			if block, ok := task["block"].([]any); ok {
				if got := find(block); got != nil {
					return got
				}
			}
		}
		return nil
	}
	for _, p := range plays {
		if tasks, ok := p["tasks"].([]any); ok {
			if got := find(tasks); got != nil {
				return got
			}
		}
	}
	t.Fatalf("installer stage 60 has no task %q", name)
	return nil
}

func keysOf(m any) []string {
	var out []string
	mm, _ := m.(map[string]any)
	for k := range mm {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// What the run writes follows installer stage 60 key for key: each set below is the installer's keys, less the ones of
// what a managed platform does not have (named with the reason), plus the ones the platform network adds. A key the
// installer gains fails this test until it is given a place here.
func TestSandboxRegistrationFollowsTheInstaller(t *testing.T) {
	fixedSandboxPins(t)
	less := func(all []string, drop ...string) []string {
		return slices.DeleteFunc(slices.Clone(all), func(k string) bool { return slices.Contains(drop, k) })
	}
	// config.sandbox of the Proxmox provider.
	installer := keysOf(dig(installerTask(t, "Give the provider the sandbox workers"), "vars", "sandbox"))
	added := []string{ProviderMarker, "mode", "bridge", "vlan", "deny"}
	site := func(sb *SandboxSpec) []string {
		return less(keysOf(SandboxSiteConfig(sandboxRender(sb))), added...)
	}
	// snat: no egress address of its own (the site's). registry, buildStorage: no sandbox registry. portal: no wake.
	if got, want := site(separateSandbox()), less(installer, "snat", "registry", "buildStorage", "portal", "kubeletSources"); !slices.Equal(got, want) {
		t.Errorf("separate writes config.sandbox keys %v, the installer's are %v", got, want)
	}
	// Shared has no proxy and no load balancer on a sandbox machine, and names the cluster's other machines.
	if got, want := site(sharedSandbox()), slices.Sorted(slices.Values(append(less(installer, "snat", "registry", "buildStorage", "portal", "proxy", "loadBalancers"), "peers"))); !slices.Equal(got, want) {
		t.Errorf("shared writes config.sandbox keys %v, want %v", got, want)
	}
	shared := SandboxSiteConfig(sandboxRender(sharedSandbox()))
	want := map[string]any{ProviderMarker: "pi-0123abcd", "mode": "shared", "pool": "proxcloud-sbx", "nodes": []any{"192.0.2.64/28"}, "dns": []any{"192.0.2.53"},
		"unsecuredMinAgeMinutes": 20, "bridge": "vmbr1", "vlan": 0, "deny": []any{"192.0.2.0/24"},
		"peers": []any{"192.0.2.20-192.0.2.39"}, "kubeletSources": []any{"192.0.2.20-192.0.2.39"}}
	if !reflect.DeepEqual(shared, want) {
		t.Errorf("shared config.sandbox:\n got %v\nwant %v", shared, want)
	}
	sep := SandboxSiteConfig(sandboxRender(separateSandbox()))
	if !reflect.DeepEqual(sep["proxy"], map[string]any{"address": "192.0.2.150", "port": 443}) || !reflect.DeepEqual(sep["loadBalancers"], []any{"192.0.2.150"}) || sep["peers"] != nil {
		t.Errorf("separate config.sandbox: %v", sep)
	}

	// The sandbox provider.
	body, err := SandboxProviderBody(sandboxRender(sharedSandbox()), "svc-pve", "KUBECONFIG")
	if err != nil {
		t.Fatal(err)
	}
	task := installerTask(t, "Register the sandbox provider")
	ibody, _ := dig(task, "proxcloud_api", "body").(map[string]any)
	if got, want := less(keysOf(body["config"]), ProviderMarker, "attachToNewProjects"), keysOf(ibody["config"]); !slices.Equal(got, want) {
		t.Errorf("provider config keys %v, the installer's %v", got, want)
	}
	// templateRegistry, imageAdoption, registryCa, registryPrune, buildStorage: no sandbox registry on the platform.
	if got, want := less(keysOf(dig(body, "config", "sandbox")), "mode"), less(keysOf(dig(ibody, "config", "sandbox")), "templateRegistry", "imageAdoption", "registryCa", "registryPrune", "buildStorage"); !slices.Equal(got, want) {
		t.Errorf("provider config.sandbox keys %v, the installer's %v", got, want)
	}
	if body["status"] != "DISABLED" || ibody["status"] != "DISABLED" || body["name"] != "Lab Sandboxes" || str(body, "config", "provider") != "sandbox" ||
		str(body, "config", "sandbox", "proxmoxServiceId") != "svc-pve" || str(body, "config", ProviderMarker) != "pi-0123abcd" || str(body, "secret", "kubeconfig") != "KUBECONFIG" ||
		dig(body, "config", "sandbox", "envd") != true || str(body, "config", "sandbox", "domain") != "sbx.example.org" || str(body, "config", "sandbox", "mode") != "shared" {
		t.Errorf("the provider body: %v", body)
	}
	// The body is a provider the portal accepts, and reads as a shared one.
	es := &externalservice.ExternalService{Config: body["config"].(map[string]any), Secret: body["secret"]}
	cfg := es.SandboxConfig()
	if !es.IsSandbox() || !cfg.Shared || !cfg.Envd || cfg.EgressMbps != 100 || cfg.Rotation.Enabled || len(cfg.Templates) != 4 {
		t.Fatalf("the portal reads %+v", cfg)
	}
	cfg.Kubeconfig = ""
	if err := cfg.ValidateAutomaticActions(); err != nil {
		t.Errorf("the abuse and rotation defaults: %v", err)
	}
	for name, tmpl := range cfg.Templates {
		if !strings.Contains(tmpl.Image, "@sha256:") || strings.HasPrefix(tmpl.Image, "pin:") || tmpl.CPU < 1 || tmpl.MemoryGiB < 1 {
			t.Errorf("template %s: %+v", name, tmpl)
		}
	}
	if ci := cfg.Templates["code-interpreter-v1"]; ci.ReadyCmd == "" || !ci.StartAsRoot || ci.User != "user" {
		t.Errorf("code-interpreter-v1: %+v", ci)
	}
	if sepBody, _ := SandboxProviderBody(sandboxRender(separateSandbox()), "svc-pve", "K"); (&externalservice.ExternalService{Config: sepBody["config"].(map[string]any)}).SandboxConfig().Shared {
		t.Error("a separate cluster's provider reads as shared")
	}
}

// The release's own images.yaml pins every image a run names, by digest: a template registered with a tag would be
// pulled as whatever the tag names that day.
func TestSandboxTemplatesArePinned(t *testing.T) {
	templates, err := SandboxTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(templates); !slices.Equal(got, []string{"base", "code-interpreter-v1", "mcp-gateway", "python"}) {
		t.Fatalf("templates %v", got)
	}
	if refs := sandboxPrepullRefs(); len(refs) != len(sandboxPrepull) {
		t.Fatalf("prepull %v: an image of %v is not pinned", refs, sandboxPrepull)
	}
	if _, err := sandboxPin("no-such-image"); err == nil {
		t.Fatal("an image the file does not pin resolved")
	}
}

// A sandbox provider never gets its status back from a run: after a Remove it stays disabled, where the two other
// providers return to what they were.
func TestAdoptProviderKeepsASandboxProviderDisabled(t *testing.T) {
	row := func(provider string) map[string]any {
		return map[string]any{"_id": "svc-" + provider, "name": "Lab " + provider, "status": "DISABLED",
			"config": map[string]any{"provider": provider, ProviderMarker: "pi-1", providerStatusBefore: "PUBLIC", "sandbox": map[string]any{"templates": map[string]any{"mine": map[string]any{"image": "x"}}}}}
	}
	body := func(provider string) map[string]any {
		return map[string]any{"name": "Lab " + provider, "status": "DISABLED", "config": map[string]any{"provider": provider, ProviderMarker: "pi-1",
			"sandbox": map[string]any{"templates": map[string]any{"base": map[string]any{"image": "y"}}}}}
	}
	sbx, err := AdoptProvider([]map[string]any{row("sandbox")}, body("sandbox"))
	if err != nil || sbx["status"] != "DISABLED" || dig(sbx, "config", providerStatusBefore) != nil {
		t.Fatalf("the sandbox provider after a Remove: %v %v", sbx, err)
	}
	// What the operator owns is kept: the run's template list does not replace an edited one.
	if dig(sbx, "config", "sandbox", "templates", "mine") == nil || dig(sbx, "config", "sandbox", "templates", "base") != nil {
		t.Fatalf("an adopted sandbox row was rewritten: %v", sbx)
	}
	if k8s, _ := AdoptProvider([]map[string]any{row("kamaji")}, body("kamaji")); k8s["status"] != "PUBLIC" {
		t.Fatalf("the Kubernetes provider after a Remove: %v", k8s)
	}
}

// The register stage, both placements: the firewall identity's kubeconfig goes to the Proxmox provider, the portal
// identity's to a sandbox provider registered DISABLED after the two others, and neither token is in a message.
func TestRegisterAddsTheSandboxProvider(t *testing.T) {
	for _, placement := range []string{SandboxShared, SandboxSeparate} {
		t.Run(placement, func(t *testing.T) {
			sb := sharedSandbox()
			if placement == SandboxSeparate {
				sb = separateSandbox()
				sb.System.Nodes = []string{"pve1"}
			}
			p := sandboxPlatform(t, RunApply, sb)
			target := p.wl
			if placement == SandboxSeparate {
				p.mutate(func(_ *Run, in *Installation, _ *Secrets) { routed(in) })
				for _, name := range []string{"pc-sandbox-sandbox-pve1-x", "pc-sandbox-sandbox-system-pve1-x"} {
					m := meta(name, map[string]any{"cluster.x-k8s.io/cluster-name": SandboxClusterName})
					m["status"] = map[string]any{"phase": "Running"}
					p.wl.put("/apis/cluster.x-k8s.io/v1beta2/namespaces/pc-sandbox/machines/"+name, m)
				}
				target = p.sbx
			}
			var calls []string
			p.r.Attach = func(_ context.Context, _, service, choice, _ string) (bool, string, error) {
				calls = append(calls, service+"="+choice)
				return service != "svc-sandbox", "note", nil // the sandbox provider is disabled: every project waits
			}
			p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Tenants.Attach = AttachAll })
			p.upTo(stageSeedPlatform)
			// The provider's sandbox network and pool are written before the Application that makes the machines.
			record, firstSite := p.r.SandboxSite, 0
			p.r.SandboxSite = func(ctx context.Context, in *Installation, s *SandboxSite) error {
				if len(p.sites) == 0 && p.seed.obj(appsPath+"pc-platform") == nil {
					firstSite = 1
				}
				return record(ctx, in, s)
			}
			if end := p.until(40); end != "complete" {
				t.Fatalf("end %s: %+v\n%+v", end, p.stage(stageWorkloadPlatform), p.stage(stageRegister))
			}
			_, in, sec := p.state.snapshot()
			if firstSite == 0 || p.sites[0].Kubeconfig != "" || p.sites[0].Range != "10.200.200.64/28" || p.sites[0].Config["pool"] != pve.SandboxPool || p.sites[0].Config["mode"] != placement {
				t.Fatalf("stage cluster wrote %d site settings before the cluster's Application: %+v", firstSite, p.sites)
			}
			// Shared: the platform cluster's endpoint, which the fence's rule names. Separate: pc-sandbox's own API.
			server := WorkloadServer(in.Spec.Clusters[0].Endpoint)
			if placement == SandboxSeparate {
				server = target.url
			}
			last := p.sites[len(p.sites)-1]
			fw, err := kamajik8s.New(last.Kubeconfig)
			if err != nil || fw.Server() != server || !strings.Contains(last.Kubeconfig, sandboxFirewallSecret) || strings.Contains(last.Kubeconfig, sandboxPortalSecret) {
				t.Fatalf("the Proxmox provider's sandbox kubeconfig names %s, want %s (%v)", fw.Server(), server, err)
			}
			if len(p.registers) != 3 || str(p.registers[2], "config", "provider") != "sandbox" || p.registers[2]["status"] != "DISABLED" ||
				str(p.registers[2], "config", "sandbox", "mode") != placement || str(p.registers[2], "config", "sandbox", "proxmoxServiceId") != in.ServiceID {
				t.Fatalf("registered %d providers; the last: %v", len(p.registers), p.registers[len(p.registers)-1]["config"])
			}
			own, err := kamajik8s.New(str(p.registers[2], "secret", "kubeconfig"))
			if err != nil || own.Server() != fw.Server() || !strings.Contains(str(p.registers[2], "secret", "kubeconfig"), sandboxPortalSecret) {
				t.Fatalf("the sandbox provider's kubeconfig must name the cluster the Proxmox provider fences: %v %v", own, err)
			}
			if in.Status.Providers == nil || in.Status.Providers.Sandbox != "svc-sandbox" || in.Status.Providers.Kubernetes != "svc-kamaji" {
				t.Fatalf("providers %+v", in.Status.Providers)
			}
			// The attach choice is recorded for the two providers that took it, and apart for the one that waits.
			if !slices.Equal(calls, []string{"svc-sandbox=all", "svc-kamaji=all", "svc-dbaas=all"}) || in.Status.TenantsAttach != AttachAll || in.Status.SandboxAttach != "" {
				t.Fatalf("attach calls %v, recorded %q and %q", calls, in.Status.TenantsAttach, in.Status.SandboxAttach)
			}
			// The proxy's certificate is where the sandbox chart runs.
			if tls := target.obj(seedNS + "sandbox-system/secrets/sandbox-proxy-tls"); tls == nil || tls["type"] != "kubernetes.io/tls" {
				t.Fatalf("no certificate Secret on %s", target.url)
			}
			if placement == SandboxSeparate && p.wl.obj(seedNS+"sandbox-system/secrets/sandbox-proxy-tls") != nil {
				t.Fatal("separate: the certificate was written to the platform cluster")
			}
			p.leaks(t, sandboxPortalSecret, sandboxFirewallSecret, sec.SandboxTLSKey, "sbx-admin-0123456789")

			// The next apply: the two other providers are not attached again, the sandbox provider is tried again.
			calls = nil
			p.again(stageRegister)
			if end := p.until(10); end != "complete" || !slices.Equal(calls, []string{"svc-sandbox=all"}) || len(p.registers) != 6 {
				t.Fatalf("second apply: %s, attach calls %v, %d registrations", end, calls, len(p.registers))
			}
		})
	}
}

// The identities are not there yet (the sandbox chart is still on its way): the two tenant providers are registered,
// the stage waits, and nothing is written to the Proxmox provider with half a kubeconfig.
func TestRegisterWaitsForTheSandboxIdentities(t *testing.T) {
	p := sandboxPlatform(t, RunApply, sharedSandbox())
	delete(p.wl.objs, seedNS+"sandbox-system/secrets/"+sandboxFirewallToken)
	p.only(stageRegister)
	if end := p.once(); !strings.HasPrefix(end, "defer:") {
		t.Fatalf("end %s: %+v", end, p.stage(stageRegister))
	}
	if s := p.stage(stageRegister); !strings.Contains(s.Message, sandboxFirewallToken) || len(p.registers) != 2 || len(p.sites) != 0 {
		t.Fatalf("stage %+v, %d registrations, %d site writes", s, len(p.registers), len(p.sites))
	}
	p.sandboxIdentities(p.wl)
	if end := p.until(5); end != "complete" || len(p.sites) != 1 {
		t.Fatalf("with the identities: %s, %d site writes", end, len(p.sites))
	}
}

// A provider that already fences another sandbox pool: the run stops before it makes a machine.
func TestSandboxSiteOfAnotherOwnerStopsTheRun(t *testing.T) {
	p := sandboxPlatform(t, RunApply, sharedSandbox())
	p.siteErr = fmt.Errorf("%w: the provider's config.sandbox fences pool proxcloud-sandbox, which is not this installation's", ErrProviderConflict)
	p.upTo(stageCluster)
	if end := p.until(5); end != "fail:"+CodeSandboxRefused {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
	if p.seed.obj(appsPath+"pc-platform") != nil && slices.Contains(p.seed.applied, "pc-platform@"+ClusterChartVersion(in2bom(t))) {
		t.Fatalf("the cluster's Application was applied after the refusal: %v", p.seed.applied)
	}
}

// Sandboxes switched off: refused while their provider is live; with it disabled the provider's sandbox settings are
// taken away before the access and the pool, and the pool's firewall generations go.
func TestSandboxOffNeedsADisabledProviderAndClearsTheSite(t *testing.T) {
	p := sandboxPlatform(t, RunApply, &SandboxSpec{Placement: SandboxOff})
	p.mutate(func(_ *Run, in *Installation, sec *Secrets) {
		in.Status.Sandbox = &SandboxStatus{Placement: SandboxShared, NodeRange: "10.200.200.64/28", ProxyAddress: "10.200.200.210", Count: 1}
		in.Status.Providers = &ProviderIDs{Kubernetes: "svc-kamaji", Database: "svc-dbaas", Sandbox: "svc-sandbox"}
	})
	status := "PUBLIC"
	pveES := p.r.Provider
	p.r.Provider = func(ctx context.Context, id string) (*externalservice.ExternalService, error) {
		if id == "svc-sandbox" {
			return &externalservice.ExternalService{ID: id, Name: "Lab Sandboxes", Status: status, Config: map[string]any{"provider": "sandbox"}}, nil
		}
		return pveES(ctx, id)
	}
	p.pve.pools = []string{pve.SandboxPool}
	// A generation of this pool, named as pve names it ("sbx", four hex digits of the pool, the content's hash), and a
	// group of another's.
	sum := sha256.Sum256([]byte(pve.SandboxPool))
	gen := "sbx" + hex.EncodeToString(sum[:2]) + "deadbeef"
	p.pve.fwGroups, p.pve.fwSets = []string{gen, "other"}, []string{gen + "-n", gen + "-o"}
	p.seed.put(sandboxApp, meta("pc-platform-sandbox", nil))
	p.upTo(stageCluster)
	if end := p.until(5); end != "fail:"+CodeSandboxRefused || p.seed.obj(sandboxApp) == nil || len(p.pve.writes) != 0 {
		t.Fatalf("off with a live provider: %s, Proxmox writes %v", end, p.pve.writes)
	}
	if s := p.stage(stageCluster); !strings.Contains(s.Message, "Lab Sandboxes") || !strings.Contains(s.Message, "DISABLED") {
		t.Fatalf("the refusal: %+v", s)
	}
	status = externalservice.StatusDisabled
	p.again(stageCluster)
	if end := p.until(30); end != "complete" {
		t.Fatalf("off with the provider disabled: %s %+v", end, p.stage(stageCluster))
	}
	_, in, _ := p.state.snapshot()
	if len(p.sites) != 1 || p.sites[0].Config != nil || p.sites[0].Range != "10.200.200.64/28" {
		t.Fatalf("the provider's sandbox settings were not taken away: %+v", p.sites)
	}
	if !slices.Equal(p.pve.fwGroups, []string{"other"}) || len(p.pve.fwSets) != 0 || len(p.pve.pools) != 0 || in.Status.Sandbox != nil {
		t.Fatalf("left on Proxmox: groups %v, sets %v, pools %v; status %+v", p.pve.fwGroups, p.pve.fwSets, p.pve.pools, in.Status.Sandbox)
	}
	// The provider row is kept, disabled, for the day sandboxes come back.
	if in.Status.Providers.Sandbox != "svc-sandbox" {
		t.Fatalf("providers %+v", in.Status.Providers)
	}
	if SandboxOffBlocked(nil) != "" || SandboxOffBlocked(&externalservice.ExternalService{Status: externalservice.StatusDisabled}) != "" {
		t.Fatal("a provider that is gone or disabled blocks off")
	}
}

// Once a run wrote the range into the provider's sandboxCidrs, a spec that switches sandboxes off must still save: the
// range is this installation's until the apply has taken it away.
func TestSiteProblemsKnowTheBuiltSandboxRange(t *testing.T) {
	in := validInstallation(t)
	in.ID = "pi-0a1b2c3d"
	site := Site{CIDRPool: "10.100.0.0/16", Sandbox: []string{"10.200.200.64/28"}, SandboxPool: pve.SandboxPool, SandboxOwner: in.ID}
	in.Spec.Sandbox = &SandboxSpec{Placement: SandboxOff}
	if p := strings.Join(in.SiteProblems(site), "\n"); !strings.Contains(p, "overlaps sandboxCidrs") {
		t.Fatalf("a sandbox network inside the platform network that no run of this installation built: %q", p)
	}
	in.Status.Sandbox = &SandboxStatus{Placement: SandboxShared, NodeRange: "10.200.200.64/28"}
	if p := in.SiteProblems(site); len(p) != 0 {
		t.Fatalf("off after a build is refused: %v", p)
	}
	in.Spec.Sandbox = sharedSandbox()
	if p := in.SiteProblems(site); len(p) != 0 {
		t.Fatalf("the installation's own block and range: %v", p)
	}
	// The installer's sandbox cluster, or another installation's, on the same provider.
	for owner, want := range map[string]string{"": "the installer's sandbox cluster", "pi-ffffffff": "installation pi-ffffffff"} {
		s := site
		s.SandboxPool, s.SandboxOwner, s.Sandbox = "proxcloud-sandbox", owner, nil
		if p := strings.Join(in.SiteProblems(s), "\n"); !strings.Contains(p, "already fences a sandbox pool, proxcloud-sandbox") || !strings.Contains(p, want) {
			t.Errorf("owner %q: %q", owner, p)
		}
	}
}

// The Check rows that read what a run writes: the datacenter firewall, a taken proxy address, the scheduler.
func TestSandboxCheckReadsTheSite(t *testing.T) {
	sandboxBOM(t)
	in := validInstallation(t)
	in.ID = "pi-0a1b2c3d"
	in.Spec.Sandbox = sharedSandbox()
	f := &preflightPVE{perms: map[string]map[string][]string{pve.DefaultPlatformTokenID: {"/pool/" + pve.SandboxPool: pve.SandboxPoolPrivileges().Privs}}, extra: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	wl := newFakeSeed(t, staticCA(t), func() string { return "" })
	wl.sa = wlAdmin
	kc, err := kamajik8s.BuildKubeconfig("wl", wl.url, []byte(wl.caPEM()), wlAdmin)
	if err != nil {
		t.Fatal(err)
	}
	client, err := kamajik8s.New(kc)
	if err != nil {
		t.Fatal(err)
	}
	p := Preflight{Installation: in, Portal: &pve.Service{C: f.client(t, srv.URL, pve.DefaultPlatformTokenID), ServiceID: "svc-pve"}, Workload: client}
	ctx := context.Background()
	svc := func(ns, name, ip string) {
		m := meta(name, nil)
		m["metadata"].(map[string]any)["namespace"] = ns
		m["status"] = map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": ip}}}}
		wl.put("/api/v1/services/"+ns+"-"+name, m)
	}
	svc("sandbox-system", "sandbox-proxy", "10.200.200.210") // its own Service holds the address: not taken
	svc("argocd", "argocd-server", "10.200.200.211")
	if c := p.sandboxCheck(ctx); c == nil || c.Status != CheckOK || !strings.Contains(c.Message, "datacenter firewall is on") {
		t.Fatalf("a site ready for sandboxes: %+v", c)
	}
	svc("pc-db-p1", "pg-rw", "10.200.200.210")
	if c := p.sandboxCheck(ctx); c.Status != CheckFail || !strings.Contains(c.Message, "spec.sandbox.proxyAddress 10.200.200.210 is taken: Service pc-db-p1/pg-rw") {
		t.Errorf("a tenant database holds the proxy address: %+v", c)
	}
	delete(wl.objs, "/api/v1/services/pc-db-p1-pg-rw")
	f.extra["/cluster/firewall/options"] = `{"enable":0}`
	if c := p.sandboxCheck(ctx); c.Status != CheckFail || !strings.Contains(c.Message, "The datacenter firewall is off") || !strings.Contains(c.Message, "unsecured taint would never come off") {
		t.Errorf("the datacenter firewall is off: %+v", c)
	}
	if len(f.writes) != 0 || wl.writeCount() != 0 {
		t.Fatalf("the Check wrote: %v", f.writes)
	}
	// The scheduler row says what sandboxes lose without it, and only when sandboxes are asked for.
	row := func() string {
		pf := Preflight{Installation: in, Portal: p.Portal, SchedulerOff: true}
		for _, c := range pf.Run(ctx) {
			if c.ID == "scheduler" {
				return c.Message
			}
		}
		return ""
	}
	if m := row(); !strings.Contains(m, "the sandbox provider cannot be set live") || !strings.Contains(m, "placeholder rate") {
		t.Errorf("scheduler row with sandboxes: %q", m)
	}
	in.Spec.Sandbox = nil
	if m := row(); m != SchedulerOffNote {
		t.Errorf("scheduler row without sandboxes: %q", m)
	}
}

// The capacity row names the memory the control plane of a separate sandbox cluster takes inside the kamaji group's
// machines, while those are small and the cluster is not built.
func TestCapacityNamesTheSandboxControlPlane(t *testing.T) {
	in := validInstallation(t)
	in.Spec.Sandbox = separateSandbox()
	kamajiGroup := func() *NodeGroup {
		for i := range in.Spec.Clusters[0].NodeGroups {
			if in.Spec.Clusters[0].NodeGroups[i].Name == "kamaji" {
				return &in.Spec.Clusters[0].NodeGroups[i]
			}
		}
		t.Fatal("no kamaji group")
		return nil
	}
	kamajiGroup().MemoryMiB = 4096
	if n := in.sandboxControlPlaneNote(); !strings.Contains(n, "control plane of pc-sandbox") || !strings.Contains(n, "4 GiB each") {
		t.Fatalf("small kamaji machines: %q", n)
	}
	f := &preflightPVE{extra: map[string]string{"/nodes": `[{"node":"pve1","status":"online","maxmem":274877906944},{"node":"pve2","status":"online","maxmem":274877906944},{"node":"pve3","status":"online","maxmem":274877906944}]`, "/cluster/resources": `[]`}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	pf := Preflight{Installation: in, CAPI: f.client(t, srv.URL, pve.CAPITokenID)}
	if c := pf.capacity(context.Background()); c.Status != CheckWarn || !strings.Contains(c.Message, "control plane of pc-sandbox") {
		t.Fatalf("the capacity row with room on every node: %+v", c)
	}
	kamajiGroup().MemoryMiB = 8192
	if n := in.sandboxControlPlaneNote(); n != "" {
		t.Fatalf("8 GiB machines: %q", n)
	}
	kamajiGroup().MemoryMiB = 4096
	now := in.CreatedAt
	in.Status.Sandbox = &SandboxStatus{Placement: SandboxSeparate, BuiltAt: &now}
	if n := in.sandboxControlPlaneNote(); n != "" {
		t.Fatalf("built: %q", n)
	}
	in.Status.Sandbox, in.Spec.Sandbox = nil, sharedSandbox()
	if n := in.sandboxControlPlaneNote(); n != "" {
		t.Fatalf("shared has no control plane of its own: %q", n)
	}
}
