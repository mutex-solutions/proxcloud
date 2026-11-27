package platformlife

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

const (
	wakeSecretPath = seedNS + "sandbox-system/secrets/sandbox-proxy-wake"
	portalURL      = "https://portal.example.org:8443"
)

// functionsBOM is sandboxBOM with the sandbox chart at the first version that has functions, as the release that
// offers them will pin it: no BOM this build embeds does. Call it before sandboxPlatform.
func functionsBOM(t *testing.T) Embedded {
	t.Helper()
	cur := sandboxBOM(t)
	c := cur.Feature(FeatureSandbox)
	c.Chart.Version, c.Chart.Digest, c.UpgradeFrom = FunctionsChartVersion, "sha256:"+strings.Repeat("f", 64), []string{"0.1.0"}
	return cur
}

func withFunctions(sb *SandboxSpec, url string) *SandboxSpec {
	sb.Functions = &SandboxFunctions{Enabled: true, PortalURL: url}
	return sb
}

// fakeFunctions is the portal's side of functions as a run calls it: whether the provider holds a function record,
// the functions it records as running, and the provider's wake token.
type fakeFunctions struct {
	exist   bool
	running map[string]int // function id: instances with an open interval
	calls   []string       // every call, in order
	token   string         // the provider's secret.functionWakeToken
}

func (p *platform) functions() *fakeFunctions {
	f := &fakeFunctions{running: map[string]int{}}
	p.r.FunctionsExist = func(_ context.Context, id string) (bool, error) {
		f.calls = append(f.calls, "exist "+id)
		return f.exist, nil
	}
	p.r.EndFunctions = func(_ context.Context, id string) error {
		f.calls = append(f.calls, "end "+id)
		clear(f.running)
		return nil
	}
	p.r.FunctionWakeToken = func(_ context.Context, _ *Installation, id, token string) error {
		f.calls = append(f.calls, "token "+id)
		f.token = token
		return nil
	}
	return f
}

// newApply makes the next acquisition another apply of the same installation: a run of its own, from stage cluster.
func (p *platform) newApply(id string) {
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.ID = id })
	for _, s := range []string{stageCluster, stageWorkloadAccess, stageWorkloadPlatform, stageRegister, stageDone} {
		p.again(s)
	}
}

// The sandbox chart has functions from 0.2.0 on, and every BOM this build embeds pins an older one or none: under each
// of them the save refuses the block, so nothing of functions can be built by this build. With the chart that has
// them the block is taken.
func TestFunctionsAreRefusedUnderEveryBOMOfThisBuild(t *testing.T) {
	for v, want := range map[string]bool{"0.1.0": false, "0.1.99": false, "0.2.0": true, "0.2.1": true, "0.10.0": true, "1.0.0": true, "": false, "0.2": false, "v": false, "0.2.0-rc.1": false} {
		if SandboxChartHasFunctions(v) != want {
			t.Errorf("chart %q has functions: %v, want %v", v, !want, want)
		}
	}
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	in := validInstallation(t)
	in.Spec.Sandbox = withFunctions(sharedSandbox(), portalURL)
	refs := []*BOMRef{nil, {Release: cur.Release, SHA256: cur.SHA256}}
	for release, sha := range previousSHA256 {
		refs = append(refs, &BOMRef{Release: release, SHA256: sha})
	}
	for _, ref := range refs {
		in.Status.BOM = ref
		if probs := in.SandboxProblems(); len(probs) == 0 {
			t.Errorf("BOM %+v takes spec.sandbox.functions", ref)
		}
	}
	in.Status.BOM = nil
	if probs := in.SandboxProblems(); len(probs) != 1 || !strings.Contains(probs[0], "pins sandbox chart 0.1.0, which has no functions") {
		t.Fatalf("under this build's BOM: %v", probs)
	}
	// Without the tick the block changes nothing of what the BOM takes.
	in.Spec.Sandbox.Functions.Enabled = false
	if probs := in.SandboxProblems(); probs != nil {
		t.Fatalf("functions not enabled: %v", probs)
	}
	in.Spec.Sandbox.Functions.Enabled = true
	functionsBOM(t)
	if probs := in.SandboxProblems(); probs != nil {
		t.Fatalf("under a BOM with chart %s: %v", FunctionsChartVersion, probs)
	}
}

func TestValidateFunctions(t *testing.T) {
	ca, key, err := selfSignedWildcard("portal.example.org", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want string
		sb         *SandboxSpec
		fn         *SandboxFunctions
	}{
		{"no block", "", sharedSandbox(), nil},
		{"not enabled: the URL is not read", "", sharedSandbox(), &SandboxFunctions{PortalURL: "nonsense"}},
		{"a name", "", sharedSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "https://portal.example.org"}},
		{"a port and a path", "", sharedSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "https://portal.example.org:8443/portal/"}},
		{"with its certificate", "", sharedSandbox(), &SandboxFunctions{Enabled: true, PortalURL: portalURL, PortalCA: ca + ca}},
		{"no URL", "portalURL \"\" is empty", sharedSandbox(), &SandboxFunctions{Enabled: true}},
		{"no scheme", "is not an https URL", sharedSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "portal.example.org"}},
		{"plain http: the token would cross in the clear", "is not an https URL", sharedSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "http://portal.example.org"}},
		{"a query", "a login, a query or a fragment", sharedSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "https://portal.example.org/?a=b"}},
		{"a login", "a login, a query or a fragment", sharedSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "https://u:p@portal.example.org"}},
		{"port 0", "names no port", sharedSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "https://portal.example.org:0"}},
		{"not a certificate", "portalCA is not PEM certificates", sharedSandbox(), &SandboxFunctions{Enabled: true, PortalURL: portalURL, PortalCA: "nonsense"}},
		{"a key beside the certificate", "portalCA is not PEM certificates", sharedSandbox(), &SandboxFunctions{Enabled: true, PortalURL: portalURL, PortalCA: ca + key}},
		{"a certificate that is not read while off is still checked", "portalCA is not PEM certificates", sharedSandbox(), &SandboxFunctions{PortalCA: "nonsense"}},
		{"too large", "portalCA is not PEM certificates", sharedSandbox(), &SandboxFunctions{Enabled: true, PortalURL: portalURL, PortalCA: strings.Repeat(ca, 200)}},
		{"shared takes an address too", "", sharedSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "https://10.200.200.70"}},
		{"separate: an address", "", separateSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "https://192.168.33.10:8443"}},
		{"separate: a name", "its host is an IPv4 address", separateSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "https://portal.example.org"}},
		{"separate: IPv6", "its host is an IPv4 address", separateSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "https://[2001:db8::1]"}},
		{"separate: inside the sandbox range", "the portal answers at an address of its own", separateSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "https://10.200.200.70"}},
		{"separate: the proxy address", "the portal answers at an address of its own", separateSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "https://10.200.200.150"}},
		{"separate: inside the MetalLB range", "the portal answers at an address of its own", separateSandbox(), &SandboxFunctions{Enabled: true, PortalURL: "https://10.200.200.205"}},
		{"placement off: nothing is read", "", &SandboxSpec{Placement: SandboxOff}, &SandboxFunctions{Enabled: true, PortalURL: "nonsense", PortalCA: "nonsense"}},
	} {
		in := validInstallation(t)
		in.Spec.Sandbox = tc.sb
		in.Spec.Sandbox.Functions = tc.fn
		err := in.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || strings.Count(err.Error(), "spec.sandbox.functions") != 1):
			t.Errorf("%s: %v, want one problem with %q", tc.name, err, tc.want)
		}
		// A login someone put into the URL is refused, and the refusal does not repeat its password.
		if err != nil && strings.Contains(err.Error(), "u:p@") {
			t.Errorf("%s: the problem repeats the login of the URL: %v", tc.name, err)
		}
	}
}

// A PUT whose sandbox block names no functions keeps the stored ones; enabled false is the explicit off, and it is
// "functions switched off" only for functions a run built on sandboxes that stay.
func TestFunctionsKeepAndSwitchedOff(t *testing.T) {
	old := validInstallation(t)
	old.Spec.Sandbox = withFunctions(sharedSandbox(), portalURL)
	next := validInstallation(t)
	next.Spec.Sandbox = sharedSandbox()
	next.Spec.Sandbox.Count = 2
	if KeepSandbox(&old.Spec, &next.Spec); next.Spec.functions() == nil || next.Spec.Sandbox.Functions.PortalURL != portalURL || next.Spec.Sandbox.Count != 2 {
		t.Fatalf("a block without functions: %+v", next.Spec.Sandbox)
	}
	next.Spec.Sandbox.Functions = &SandboxFunctions{}
	if KeepSandbox(&old.Spec, &next.Spec); next.Spec.functions() != nil {
		t.Fatal("enabled false is explicit and stays")
	}
	off := &Spec{Sandbox: &SandboxSpec{Placement: SandboxOff}}
	if KeepSandbox(&old.Spec, off); off.Sandbox.Functions != nil {
		t.Fatal("placement off reads nothing else, and keeps nothing")
	}
	plain := validInstallation(t)
	plain.Spec.Sandbox = sharedSandbox()
	keep := validInstallation(t)
	keep.Spec.Sandbox = sharedSandbox()
	if KeepSandbox(&plain.Spec, &keep.Spec); keep.Spec.Sandbox.Functions != nil {
		t.Fatal("nothing stored, nothing kept")
	}

	old.Status.Providers = &ProviderIDs{Sandbox: "svc-sandbox"}
	if _, off := FunctionsSwitchedOff(old, next); off {
		t.Error("saved and never built: nothing is switched off")
	}
	old.Status.Sandbox = &SandboxStatus{Placement: SandboxShared, Functions: &FunctionsStatus{PortalURL: portalURL, TokenVersion: "pr-1"}}
	if id, off := FunctionsSwitchedOff(old, next); !off || id != "svc-sandbox" {
		t.Errorf("built, then enabled false: %q %v", id, off)
	}
	if _, off := FunctionsSwitchedOff(old, old); off {
		t.Error("functions that stay")
	}
	gone := validInstallation(t)
	gone.Spec.Sandbox = &SandboxSpec{Placement: SandboxOff}
	if _, off := FunctionsSwitchedOff(old, gone); off {
		t.Error("sandboxes switched off as a whole are SandboxSwitchedOff's")
	}
	if id, off := SandboxSwitchedOff(old, gone); !off || id != "svc-sandbox" {
		t.Errorf("sandboxes off: %q %v", id, off)
	}
}

// What the renders give for functions, as files to review. The block is in the chart's values of both placements only
// with the version of a token a run wrote; before that, and without the tick, every render is the one without
// functions. The fence input names the portal only with placement separate. The provider body carries no wake token
// of either kind. `go test ./internal/platformlife -run TestFunctionsRenderGolden -update` rewrites the files.
func TestFunctionsRenderGolden(t *testing.T) {
	fixedSandboxPins(t)
	b := embedded(t)
	addSandbox(b)
	b.Feature(FeatureSandbox).Chart.Version = FunctionsChartVersion
	b.Release = "0.0.0-synthetic"
	sandboxApp := func(in RenderInput) map[string]any {
		t.Helper()
		spec := validInstallation(t).Spec
		spec.Sandbox = sharedSandbox()
		for _, a := range renderAll(t, b.For(spec), in).apps {
			if str(a, "metadata", "name") == "pc-platform-sandbox" {
				return a
			}
		}
		t.Fatal("no Application of the sandbox chart")
		return nil
	}
	without := func(app map[string]any, path ...string) map[string]any {
		var out map[string]any
		raw, _ := json.Marshal(app)
		_ = json.Unmarshal(raw, &out)
		delete(dig(out, path...).(map[string]any), "functions")
		return out
	}
	values := []string{"spec", "source", "helm", "valuesObject"}

	// Shared.
	plain := sandboxRender(sharedSandbox())
	in := sandboxRender(sharedSandbox())
	in.Sandbox.Functions = &RenderFunctions{WakeURL: "https://portal.example.org/api/v1/internal/functions", PortalCA: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"}
	if !sameJSON(sandboxApp(plain), sandboxApp(in)) {
		t.Error("shared: functions are rendered before a token's Secret is on the cluster")
	}
	in.Sandbox.Functions.TokenVersion = "pr-0123456789abcdef"
	app := sandboxApp(in)
	golden(t, "render-sandbox-shared-functions.golden.yaml", yamlDoc(t, map[string]any{"application": app, "site": SandboxSiteConfig(in)}))
	fn, _ := dig(app, append(values, "functions")...).(map[string]any)
	if len(fn) != 4 || fn["enabled"] != true || fn["wakeURL"] != in.Sandbox.Functions.WakeURL || fn["wakeTokenVersion"] != "pr-0123456789abcdef" || fn["portalCA"] != in.Sandbox.Functions.PortalCA {
		t.Errorf("the functions block: %v", fn)
	}
	if !sameJSON(without(app, values...), sandboxApp(plain)) {
		t.Error("shared: functions change more of the Application than its functions block")
	}
	if dig(app, append(values, "proxy", "wakeURL")...) != nil {
		t.Error("the sandbox wake URL (autoResume) is rendered")
	}
	if !sameJSON(SandboxSiteConfig(in), SandboxSiteConfig(plain)) {
		t.Errorf("shared: the fence input changes with functions: %v", SandboxSiteConfig(in))
	}

	// Separate.
	plain = sandboxRender(separateSandbox())
	in = sandboxRender(separateSandbox())
	in.Sandbox.Functions = &RenderFunctions{WakeURL: "https://198.51.100.10:8443/api/v1/internal/functions", PortalAddress: "198.51.100.10", PortalPort: 8443}
	base, err := SandboxApplication(b, plain)
	if err != nil {
		t.Fatal(err)
	}
	if early, _ := SandboxApplication(b, in); !sameJSON(base, early) {
		t.Error("separate: functions are rendered before a token's Secret is in pc-sandbox")
	}
	in.Sandbox.Functions.TokenVersion = "pr-0123456789abcdef"
	sapp, err := SandboxApplication(b, in)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "render-sandbox-separate-functions.golden.yaml", yamlDoc(t, map[string]any{"application": sapp, "site": SandboxSiteConfig(in)}))
	addon := append(values, "addons", "sandbox", "values")
	if fn, _ := dig(sapp, append(addon, "functions")...).(map[string]any); fn["enabled"] != true || fn["wakeTokenVersion"] != "pr-0123456789abcdef" {
		t.Errorf("separate: the add-on's functions block: %v", fn)
	}
	if !sameJSON(without(sapp, addon...), base) {
		t.Error("separate: functions change more of the Application than the add-on's functions block")
	}
	site := SandboxSiteConfig(in)
	if p, _ := site["portal"].(map[string]any); p["address"] != "198.51.100.10" || p["port"] != 8443 {
		t.Errorf("separate: the fence input's portal: %v", site["portal"])
	}
	// As the provider's sync reads it for the fence and the isolation rules: one more endpoint, with separate only.
	read := func(in RenderInput) []string {
		return (&externalservice.ExternalService{Config: map[string]any{"sandbox": SandboxSiteConfig(in)}}).PVESandboxEndpoints()
	}
	if got := read(in); !slices.Equal(got, []string{"198.51.100.10:8443"}) || len(read(plain)) != 0 {
		t.Errorf("the endpoints the fence opens: %v with functions, %v without", got, read(plain))
	}
	shared := sandboxRender(sharedSandbox())
	shared.Sandbox.Functions = &RenderFunctions{WakeURL: "https://198.51.100.10/api/v1/internal/functions", TokenVersion: "pr-1", PortalAddress: "198.51.100.10", PortalPort: 443}
	if got := read(shared); len(got) != 0 {
		t.Errorf("shared: the fence opens %v", got)
	}
	delete(site, "portal")
	if !sameJSON(site, SandboxSiteConfig(plain)) {
		t.Error("separate: functions change more of the fence input than the portal")
	}
	// The body a run registers the sandbox provider with: the kubeconfig and no wake token. The functions' token
	// goes through Runner.FunctionWakeToken alone, and the sandbox wake token is never written.
	body, err := SandboxProviderBody(in, "svc-pve", "KUBECONFIG")
	if err != nil {
		t.Fatal(err)
	}
	if secret, _ := body["secret"].(map[string]any); len(secret) != 1 || secret["kubeconfig"] != "KUBECONFIG" || dig(body, "config", "sandbox", "functions") != nil {
		t.Errorf("the provider body: secret keys %v, functions %v", keysOf(body["secret"]), dig(body, "config", "sandbox", "functions"))
	}
}

// The token is sealed at rest like every other secret of an installation, and opens again; its version is not secret.
func TestFunctionWakeTokenIsSealed(t *testing.T) {
	enc := textcrypt.New("test-key")
	s := &Store{enc: enc}
	plain := Secrets{ID: "pi-1", FunctionWake: &FunctionWake{Token: strings.Repeat("ab", 32), Version: "pr-1"}}
	sealed := s.seal(plain, enc.Encrypt)
	raw, _ := json.Marshal(sealed)
	if strings.Contains(string(raw), plain.FunctionWake.Token) || sealed.FunctionWake.Version != "pr-1" || plain.FunctionWake.Token != strings.Repeat("ab", 32) {
		t.Fatalf("stored: %s", raw)
	}
	if open := s.seal(sealed, enc.Decrypt); *open.FunctionWake != *plain.FunctionWake {
		t.Fatalf("opened: %+v", open.FunctionWake)
	}
	if none := s.seal(Secrets{ID: "pi-1"}, enc.Encrypt); none.FunctionWake != nil {
		t.Fatal("a token appeared")
	}
	// And no message of a run may carry it: an error of a cluster that quotes the Secret it refused is redacted.
	x := &acq{sec: &plain}
	if got := x.redact("secrets \"sandbox-proxy-wake\" is invalid: " + plain.FunctionWake.Token); strings.Contains(got, plain.FunctionWake.Token) || !strings.Contains(got, "[redacted]") {
		t.Fatalf("a message carries the token: %s", got)
	}
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// An apply with functions, placement shared: the token is 32 random bytes, sealed in the installation's secrets,
// written as its own key of Secret sandbox-proxy-wake on the platform cluster, in the chart's values only as the
// version, and given to the provider in stage register. It is in no Application, no message and no record. A look
// again in the same run keeps token and version; the next apply replaces both, and the version changes when and only
// when the token does. Functions are switched off only when the provider holds no function.
func TestApplyBuildsTheFunctionSide(t *testing.T) {
	functionsBOM(t)
	p := sandboxPlatform(t, RunApply, withFunctions(sharedSandbox(), portalURL))
	fns := p.functions()
	p.upTo(stageSeedPlatform)
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v\n%+v\n%s", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform), p.logs)
	}
	built := func() (token, version string) {
		t.Helper()
		_, in, sec := p.state.snapshot()
		wake, st := sec.FunctionWake, in.Status.Sandbox
		if wake == nil || !hex64.MatchString(wake.Token) || wake.Version == "" || strings.Contains(wake.Token, wake.Version) {
			t.Fatalf("the sealed token: %+v", wake)
		}
		secret := p.wl.obj(wakeSecretPath)
		if data, _ := secret["data"].(map[string]any); secret == nil || secret["type"] != "Opaque" || len(data) != 1 || data["PROXCLOUD_FUNCTION_WAKE_TOKEN"] != b64(wake.Token) {
			t.Fatalf("Secret sandbox-proxy-wake holds the functions' token under its own key and nothing else: %v", keysOf(secret["data"]))
		}
		app := p.seed.obj(sandboxApp)
		fn, _ := dig(app, "spec", "source", "helm", "valuesObject", "functions").(map[string]any)
		if len(fn) != 4 || fn["enabled"] != true || fn["wakeURL"] != portalURL+"/api/v1/internal/functions" || fn["portalCA"] != "" || fn["wakeTokenVersion"] != wake.Version {
			t.Fatalf("the chart's functions block: %v (version %s)", fn, wake.Version)
		}
		if st.Functions == nil || *st.Functions != (FunctionsStatus{PortalURL: portalURL, TokenVersion: wake.Version}) {
			t.Fatalf("status %+v", st.Functions)
		}
		if fns.token != wake.Token {
			t.Fatal("the provider does not hold the token of the Secret")
		}
		// In no object of the seed (the Applications), no message, no record, no log.
		p.seed.mu.Lock()
		objs, _ := json.Marshal(p.seed.objs)
		p.seed.mu.Unlock()
		if strings.Contains(string(objs), wake.Token) {
			t.Fatal("the token is in an object of the seed")
		}
		p.leaks(t, wake.Token)
		return wake.Token, wake.Version
	}
	token, version := built()
	// The provider got it after the chart's wave, in stage register, and the registration body carries no token:
	// secret.proxyWakeToken stays unset, which is what keeps autoResume refused.
	if !slices.Equal(fns.calls, []string{"token svc-sandbox"}) {
		t.Errorf("calls %v", fns.calls)
	}
	for _, body := range p.registers {
		if raw, _ := json.Marshal(body); strings.Contains(string(raw), "WakeToken") || strings.Contains(string(raw), token) {
			t.Errorf("a registration body carries a wake token: %v", keysOf(body["secret"]))
		}
	}
	// With placement shared no sandbox machine may reach the portal: the fence input names none.
	for _, s := range p.sites {
		if s.Config["portal"] != nil {
			t.Fatalf("shared: the fence input names the portal: %v", s.Config["portal"])
		}
	}

	// The same run again (a Retry of a stage, a second look): nothing is minted.
	for _, id := range []string{stageCluster, stageWorkloadPlatform, stageRegister} {
		p.again(id)
	}
	if end := p.until(30); end != "complete" {
		t.Fatalf("the same run again: %s", end)
	}
	if tok, ver := built(); tok != token || ver != version {
		t.Fatal("a second look of the same run changed the token or its version")
	}
	// The next apply: a new token and a new version, and the provider follows in stage register.
	p.newApply("pr-2")
	if end := p.until(30); end != "complete" {
		t.Fatalf("the next apply: %s: %+v", end, p.stage(stageWorkloadPlatform))
	}
	token2, version2 := built()
	if token2 == token || version2 == version || version2 != "pr-2" {
		t.Fatalf("the next apply: token changed %v, version %s then %s", token2 != token, version, version2)
	}

	// Off while the provider holds a function: refused in stage cluster, and nothing is taken away.
	fns.exist, fns.running["fn-0123456789abcdef"] = true, 1
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Sandbox.Functions.Enabled = false })
	p.newApply("pr-3")
	if end := p.until(30); end != "fail:"+CodeSandboxRefused || !strings.Contains(p.stage(stageCluster).Message, "still holds functions") {
		t.Fatalf("off with a function: %s %+v", end, p.stage(stageCluster))
	}
	if tok, _ := built(); tok != token2 || len(fns.running) != 1 {
		t.Fatal("a refused off changed something")
	}
	// A build that cannot read the records does not switch functions off either.
	p.r.FunctionsExist = nil
	p.newApply("pr-4")
	if end := p.until(30); end != "fail:"+CodeSandboxRefused || p.wl.obj(wakeSecretPath) == nil {
		t.Fatalf("off without a read of the records: %s", end)
	}
	// No function left: the intervals are ended (none is open), the chart loses the block, and the token goes from
	// the provider, the cluster, the secrets and the record.
	fns = p.functions()
	p.newApply("pr-5")
	if end := p.until(30); end != "complete" {
		t.Fatalf("off: %s: %+v\n%+v", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform))
	}
	_, in, sec := p.state.snapshot()
	if sec.FunctionWake != nil || in.Status.Sandbox == nil || in.Status.Sandbox.Functions != nil || p.wl.obj(wakeSecretPath) != nil || fns.token != "" ||
		dig(p.seed.obj(sandboxApp), "spec", "source", "helm", "valuesObject", "functions") != nil {
		t.Fatalf("off: sealed %v, status %+v, Secret %v, provider token set %v", sec.FunctionWake != nil, in.Status.Sandbox, p.wl.obj(wakeSecretPath) != nil, fns.token != "")
	}
	if !slices.Equal(fns.calls, []string{"exist svc-sandbox", "end svc-sandbox", "token svc-sandbox"}) {
		t.Errorf("off: calls %v", fns.calls)
	}
	// And with nothing built, an apply asks nothing about functions.
	fns = p.functions()
	p.newApply("pr-6")
	if end := p.until(30); end != "complete" || len(fns.calls) != 0 {
		t.Fatalf("without functions: %s, calls %v", end, fns.calls)
	}
}

// A record that asks for functions under a BOM whose sandbox chart has none (the save refuses it, so only a record
// written past the save can): the run stops in stage cluster before it writes anything.
func TestFunctionsRunRefusedUnderAChartWithoutThem(t *testing.T) {
	p := sandboxPlatform(t, RunApply, withFunctions(sharedSandbox(), portalURL))
	fns := p.functions()
	p.upTo(stageCluster)
	if end := p.until(5); end != "fail:"+CodeSandboxRefused || len(p.pve.writes) != 0 || len(fns.calls) != 0 || len(p.sites) != 0 {
		t.Fatalf("end %s, Proxmox writes %v, calls %v", end, p.pve.writes, fns.calls)
	}
	if s := p.stage(stageCluster); !strings.Contains(s.Message, "which has no functions") {
		t.Errorf("stage %+v", s)
	}
	_, _, sec := p.state.snapshot()
	if sec.FunctionWake != nil || p.wl.obj(wakeSecretPath) != nil {
		t.Error("a token was minted")
	}
}

// Placement separate: the token's Secret is written inside pc-sandbox, the cluster's Application carries the functions
// block (in addons.sandbox.values) only after it, and the fence input names the portal's address and port.
func TestApplyBuildsTheFunctionSideOfTheSeparateCluster(t *testing.T) {
	functionsBOM(t)
	sb := withFunctions(separateSandbox(), "https://192.168.33.10:8443")
	sb.System.Nodes = []string{"pve1"}
	p := sandboxPlatform(t, RunApply, sb)
	fns := p.functions()
	p.mutate(func(_ *Run, in *Installation, _ *Secrets) { routed(in) })
	for _, name := range []string{"pc-sandbox-sandbox-pve1-x", "pc-sandbox-sandbox-system-pve1-x"} {
		m := meta(name, map[string]any{"cluster.x-k8s.io/cluster-name": SandboxClusterName})
		m["status"] = map[string]any{"phase": "Running"}
		p.wl.put("/apis/cluster.x-k8s.io/v1beta2/namespaces/pc-sandbox/machines/"+name, m)
	}
	p.upTo(stageSeedPlatform)
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v\n%+v\n%s", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform), p.logs)
	}
	_, in, sec := p.state.snapshot()
	wake := sec.FunctionWake
	if wake == nil || !hex64.MatchString(wake.Token) {
		t.Fatalf("the sealed token: %+v", wake)
	}
	if data, _ := p.sbx.obj(wakeSecretPath)["data"].(map[string]any); len(data) != 1 || data["PROXCLOUD_FUNCTION_WAKE_TOKEN"] != b64(wake.Token) || p.wl.obj(wakeSecretPath) != nil {
		t.Fatal("the Secret is inside pc-sandbox, and not on the platform cluster")
	}
	app := p.seed.obj(appsPath + SandboxClusterName)
	fn, _ := dig(app, "spec", "source", "helm", "valuesObject", "addons", "sandbox", "values", "functions").(map[string]any)
	if fn["enabled"] != true || fn["wakeURL"] != "https://192.168.33.10:8443/api/v1/internal/functions" || fn["wakeTokenVersion"] != wake.Version || in.Status.Sandbox.Functions == nil {
		t.Fatalf("the add-on's functions block: %v, status %+v", fn, in.Status.Sandbox.Functions)
	}
	// Applied once without the block, before pc-sandbox answered, and again with it once the Secret was there.
	if n := len(p.seed.callsOfApply(SandboxClusterName)); n < 2 {
		t.Errorf("the cluster's Application was applied %d times", n)
	}
	if len(p.sites) == 0 {
		t.Fatal("no fence input was written")
	}
	for _, s := range p.sites {
		if portal, _ := s.Config["portal"].(map[string]any); portal["address"] != "192.168.33.10" || portal["port"] != 8443 {
			t.Fatalf("the fence input's portal: %v", s.Config["portal"])
		}
	}
	if fns.token != wake.Token {
		t.Error("the provider does not hold the token")
	}
	raw, _ := json.Marshal(app)
	if strings.Contains(string(raw), wake.Token) {
		t.Fatal("the Application carries the token")
	}
	p.leaks(t, wake.Token)
}

// Sandboxes switched off on a platform whose separate cluster runs functions, which is also how that cluster is
// rebuilt for another Kubernetes version: before the cluster's Application is deleted the run ends every running
// interval in the portal's records, so none is left open while the cluster is gone, and it stops when it cannot.
// The provider's token goes with the sandbox access.
func TestSandboxOffEndsTheFunctionsFirst(t *testing.T) {
	p := sandboxPlatform(t, RunApply, &SandboxSpec{Placement: SandboxOff})
	fns := p.functions()
	fns.exist, fns.running, fns.token = true, map[string]int{"fn-0123456789abcdef": 2, "fn-fedcba9876543210": 1}, "token-of-the-old-cluster"
	p.r.FunctionsExist, p.r.EndFunctions = nil, nil
	now := time.Now().UTC()
	p.mutate(func(_ *Run, in *Installation, sec *Secrets) {
		in.Status.Sandbox = &SandboxStatus{Placement: SandboxSeparate, NodeRange: "10.200.200.64/28", ProxyAddress: "10.200.200.150", Count: 1, BuiltAt: &now,
			Functions: &FunctionsStatus{PortalURL: "https://192.168.33.10", TokenVersion: "pr-0"}}
		in.Status.Providers = &ProviderIDs{Kubernetes: "svc-kamaji", Database: "svc-dbaas", Sandbox: "svc-sandbox"}
		sec.Sandbox = &TokenCredential{TokenID: "capmox@pve!pc-sandbox", Secret: "minted-0123456789"}
		sec.FunctionWake = &FunctionWake{Token: "token-of-the-old-cluster", Version: "pr-0"}
	})
	p.pve.pools, p.pve.tokens = []string{pve.SandboxPool}, []string{"capmox@pve!pc-sandbox"}
	app := meta(SandboxClusterName, nil)
	app["metadata"].(map[string]any)["finalizers"] = []any{resourcesFinalizer}
	p.seed.put(appsPath+SandboxClusterName, app)
	p.upTo(stageCluster)
	// No way to end the intervals: nothing is deleted.
	if end := p.until(5); end != "fail:"+CodeSandboxRefused || p.seed.obj(appsPath+SandboxClusterName) == nil || len(fns.running) != 2 {
		t.Fatalf("without EndFunctions: %s %+v", end, p.stage(stageCluster))
	}
	fns = p.functions()
	fns.running, fns.token = map[string]int{"fn-0123456789abcdef": 2, "fn-fedcba9876543210": 1}, "token-of-the-old-cluster"
	// The intervals are ended while the cluster's Application is still there, untouched.
	end, before := p.r.EndFunctions, []bool(nil)
	p.r.EndFunctions = func(ctx context.Context, id string) error {
		before = append(before, p.seed.obj(appsPath+SandboxClusterName) != nil && p.seed.deletes() == 0)
		return end(ctx, id)
	}
	p.again(stageCluster)
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v\n%s", end, p.stage(stageCluster), p.logs)
	}
	if len(before) == 0 || !before[0] || len(fns.running) != 0 || p.seed.obj(appsPath+SandboxClusterName) != nil {
		t.Fatalf("the intervals were ended before the cluster's Application was deleted: %v; running intervals left %d", before, len(fns.running))
	}
	_, in, sec := p.state.snapshot()
	if in.Status.Sandbox != nil || sec.FunctionWake != nil || fns.token != "" {
		t.Fatalf("status %+v, sealed token left %v, provider token left %v", in.Status.Sandbox, sec.FunctionWake != nil, fns.token != "")
	}
	// The function records are not asked about: off as a whole keeps them, for the sandboxes built again.
	if slices.Contains(fns.calls, "exist svc-sandbox") || fns.calls[0] != "end svc-sandbox" {
		t.Errorf("calls %v", fns.calls)
	}
	p.leaks(t, "token-of-the-old-cluster")
}

// Remove on a platform with functions: once its providers are disabled, and before anything of a cluster is deleted,
// every running interval is ended in the records. The token goes from the provider with the sandbox access.
func TestRemoveEndsTheFunctionsFirst(t *testing.T) {
	sandboxBOM(t)
	p := removal(t)
	fns := p.functions()
	fns.running, fns.token = map[string]int{"fn-0123456789abcdef": 1}, "token-of-the-removed-platform"
	p.mutate(func(_ *Run, in *Installation, sec *Secrets) {
		in.Status.Sandbox = &SandboxStatus{Placement: SandboxShared, Count: 1, Functions: &FunctionsStatus{PortalURL: portalURL, TokenVersion: "pr-0"}}
		in.Status.Providers.Sandbox = "svc-sandbox"
		sec.FunctionWake = &FunctionWake{Token: "token-of-the-removed-platform", Version: "pr-0"}
	})
	p.pve.pools = []string{pve.SandboxPool}
	end, before := p.r.EndFunctions, []bool(nil)
	p.r.EndFunctions = func(ctx context.Context, id string) error {
		before = append(before, p.disabled > 0 && p.seed.deletes() == 0)
		return end(ctx, id)
	}
	if end := p.once(); end != "defer:waiting_remove_cluster" {
		t.Fatalf("end %s: %+v", end, p.stage(StageRemoveCluster))
	}
	if len(before) == 0 || !before[0] || len(fns.running) != 0 || p.seed.deletes() == 0 {
		t.Fatalf("the intervals were ended after the providers were disabled and before any delete on the seed: %v; left %d; deletes %d", before, len(fns.running), p.seed.deletes())
	}
	p.seed.mu.Lock()
	for k := range p.seed.objs {
		if strings.HasPrefix(k, capiNS+"machines/") {
			delete(p.seed.objs, k)
		}
	}
	p.seed.mu.Unlock()
	if end := p.until(8); end != "complete" {
		t.Fatalf("end %s: %+v\n%s", end, p.stage(StageRemoveCluster), p.logs)
	}
	_, in, sec := p.state.snapshot()
	if in.Status.Sandbox != nil || sec.FunctionWake != nil || fns.token != "" {
		t.Fatalf("status %+v, sealed token left %v, provider token left %v", in.Status.Sandbox, sec.FunctionWake != nil, fns.token != "")
	}
	p.leaks(t, "token-of-the-removed-platform")
}

// The plan rows of functions on an installed platform: on (what is rendered, the token, where the proxy calls, and
// with separate the one address the fence opens), the token every later apply replaces, off, and what the row that
// deletes the sandboxes says of the functions on them.
func TestFunctionsPlanRows(t *testing.T) {
	b := functionsBOM(t)
	in := validInstallation(t)
	in.Status.BOM = &BOMRef{Release: b.Release, SHA256: b.SHA256}
	in.Status.Clusters = []ClusterStatus{{Name: ClusterName}}
	rows := func() map[string]PlanChange {
		t.Helper()
		p, err := PlanFor(in, "")
		if err != nil || len(p.Blocking) != 0 {
			t.Fatalf("plan: %+v %v", p.Blocking, err)
		}
		out := map[string]PlanChange{}
		for _, c := range p.Changes {
			out[c.Name] = c
		}
		return out
	}
	now := in.CreatedAt
	shared := &SandboxStatus{Placement: SandboxShared, NodeRange: "10.200.200.64/28", ProxyAddress: "10.200.200.210", Count: 1, Nodes: []string{"pve1"}, BuiltAt: &now}
	// Sandboxes built, no functions asked: nothing.
	in.Spec.Sandbox, in.Status.Sandbox = sharedSandbox(), shared
	if r := rows(); len(r) != 0 {
		t.Fatalf("no functions: %+v", r)
	}
	in.Spec.Sandbox.Functions = &SandboxFunctions{PortalURL: portalURL}
	if r := rows(); len(r) != 0 {
		t.Fatalf("functions not enabled: %+v", r)
	}
	// On, with sandboxes that stand.
	in.Spec.Sandbox = withFunctions(sharedSandbox(), portalURL)
	r := rows()
	c := r["sandbox-functions"]
	if len(r) != 1 || c.Class != ClassChart || c.Stage != stageWorkloadPlatform || c.From != "" || c.To != "on ("+portalURL+")" {
		t.Fatalf("on: %+v", r)
	}
	for _, want := range []string{"sandbox-system/sandbox-proxy-wake", "32 random bytes", "it is in no Application", portalURL + "/api/v1/internal/functions", "is cut once",
		"reads the Secrets", "A machine rotation does not wait for function instances", "<name>-<6 hex>.sbx.example.org", "Every later apply replaces the token"} {
		if !strings.Contains(c.Impact, want) {
			t.Errorf("on, shared: the row lacks %q: %s", want, c.Impact)
		}
	}
	if strings.Contains(c.Impact, "fence") {
		t.Errorf("shared opens nothing in the fence: %s", c.Impact)
	}
	// On, in the same apply that builds the sandboxes: the sandbox rows and the functions row.
	in.Status.Sandbox = nil
	if r := rows(); len(r) != 4 || r["sandbox-functions"].To == "" || r["sandbox"].Class != ClassMachines {
		t.Fatalf("sandboxes and functions in one apply: %+v", r)
	}
	// Separate: the fence opens the portal's address and port; a rotation waits; a rebuild stops functions.
	in.Spec.Sandbox = withFunctions(separateSandbox(), "https://192.168.33.10:8443")
	in.Status.Sandbox = &SandboxStatus{Placement: SandboxSeparate, NodeRange: "10.200.200.64/28", ProxyAddress: "10.200.200.150", Count: 1, Nodes: []string{"pve1"},
		SystemCount: 1, SystemNodes: []string{"pve2"}, BuiltAt: &now}
	c = rows()["sandbox-functions"]
	for _, want := range []string{"the fence of the sandbox machines opens 192.168.33.10 port 8443 and nothing else new", "holds a machine rotation back", "A rebuild of pc-sandbox", "The sandbox chart on pc-sandbox"} {
		if !strings.Contains(c.Impact, want) {
			t.Errorf("on, separate: the row lacks %q: %s", want, c.Impact)
		}
	}
	// Built: every apply replaces the token, and the row says so and names a new URL.
	in.Status.Sandbox.Functions = &FunctionsStatus{PortalURL: "https://192.168.33.10:8443", TokenVersion: "pr-1"}
	c = rows()["sandbox-functions"]
	if !strings.Contains(c.To, "a new wake token") || !strings.Contains(c.Impact, "may be answered 503") || strings.Contains(c.Impact, "then calls the portal") {
		t.Errorf("built: %+v", c)
	}
	in.Spec.Sandbox.Functions.PortalURL = "https://192.168.33.11"
	if c = rows()["sandbox-functions"]; !strings.Contains(c.Impact, "then calls the portal at https://192.168.33.11/api/v1/internal/functions") || !strings.Contains(c.Impact, "opens 192.168.33.11 port 443") {
		t.Errorf("another URL: %+v", c)
	}
	// Off, sandboxes stay.
	in.Spec.Sandbox.Functions.Enabled = false
	if c = rows()["sandbox-functions"]; c.From != "on" || c.To != "off" || !strings.Contains(c.Impact, "Refused while the sandbox provider holds a function") {
		t.Errorf("off: %+v", c)
	}
	// Sandboxes off: the row that deletes the cluster says what happens to the functions on it, and only then.
	in.Spec.Sandbox = &SandboxSpec{Placement: SandboxOff}
	r = rows()
	if c = r["sandbox"]; !strings.Contains(c.Impact, "ends every running interval of every function") || !strings.Contains(c.Impact, "The function records stay") || r["sandbox-functions"].Name != "" {
		t.Errorf("sandboxes off with functions: %+v", r)
	}
	in.Status.Sandbox.Functions = nil
	if c = rows()["sandbox"]; strings.Contains(c.Impact, "unction") {
		t.Errorf("sandboxes off without functions names them: %s", c.Impact)
	}
}

// The move of the sandbox chart from the version without functions to the one with them, from the BOM of the release
// that first pinned the chart. The two BOMs differ in that chart for every installation; what the move does differs:
// nothing without sandboxes (no row), the Application's sync with placement shared, the add-on's upgrade inside
// pc-sandbox with separate. No machine is replaced in any of them, so the separate cluster does not refuse the move.
func TestSandboxChartMovePlansByPlacement(t *testing.T) {
	old, ok := Lookup(previousSHA256["0.12.0-rc.1"])
	if !ok || old.Feature(FeatureSandbox) == nil || old.Feature(FeatureSandbox).Chart.Version != "0.1.0" {
		t.Fatal("this build does not embed the 0.12.0-rc.1 BOM with sandbox chart 0.1.0")
	}
	cur := functionsBOM(t)
	cur.Release = "0.13.0"
	from := refOf(old)
	now := time.Now()
	row := func(edit func(*Installation)) (*PlanChange, *Upgrade) {
		t.Helper()
		in := validInstallation(t)
		in.Status.BOM = &from
		in.Status.Clusters = []ClusterStatus{{Name: ClusterName}}
		edit(in)
		_, up, err := move(in, cur.SHA256)
		if err != nil || up == nil || !up.Allowed || hasMachines(up.Changes) {
			t.Fatalf("the move: %+v %v", up, err)
		}
		for i, c := range up.Changes {
			if c.Cluster == ClusterWorkload && c.Name == "sandbox" {
				return &up.Changes[i], up
			}
		}
		return nil, up
	}
	if c, up := row(func(*Installation) {}); c != nil {
		t.Errorf("no sandboxes: the move has a row for a chart nothing renders: %+v (of %d rows)", c, len(up.Changes))
	}
	// Asked for and not built: this apply installs the chart; nothing is moved.
	if c, _ := row(func(in *Installation) { in.Spec.Sandbox = sharedSandbox() }); c != nil {
		t.Errorf("sandboxes not built yet: %+v", c)
	}
	c, _ := row(func(in *Installation) {
		in.Spec.Sandbox, in.Status.Sandbox = sharedSandbox(), &SandboxStatus{Placement: SandboxShared, Count: 1, BuiltAt: &now}
	})
	if c == nil || c.Class != ClassChart || c.From != "0.1.0" || c.To != "0.2.0" || c.Stage != stageWorkloadPlatform || c.Wave == nil ||
		!strings.Contains(c.Impact, "The Application pc-platform-sandbox moves") || !strings.Contains(c.Impact, "its replicas are replaced one at a time") || !strings.Contains(c.Impact, "no machine is replaced") {
		t.Errorf("shared: %+v", c)
	}
	c, _ = row(func(in *Installation) {
		in.Spec.Sandbox, in.Status.Sandbox = separateSandbox(), &SandboxStatus{Placement: SandboxSeparate, Count: 1, SystemCount: 1, BuiltAt: &now}
	})
	if c == nil || c.Class != ClassChart || c.From != "0.1.0" || c.To != "0.2.0" || c.Stage != stageWorkloadPlatform || c.Wave != nil ||
		!strings.Contains(c.Impact, "add-on of the cluster pc-sandbox") || !strings.Contains(c.Impact, "addonCharts.sandbox.version") || !strings.Contains(c.Impact, "in place") ||
		strings.Contains(c.Impact, "The Application pc-platform-sandbox") {
		t.Errorf("separate: %+v", c)
	}
	// Built, and the settings switch them off in this apply: the chart is deleted, not moved.
	if c, _ := row(func(in *Installation) {
		in.Spec.Sandbox, in.Status.Sandbox = &SandboxSpec{Placement: SandboxOff}, &SandboxStatus{Placement: SandboxShared, Count: 1, BuiltAt: &now}
	}); c != nil {
		t.Errorf("sandboxes switched off: %+v", c)
	}
}

// The upgrade run of that move reaches what it must change: with placement shared the sandbox chart's Application is
// applied at the new version; with separate the cluster's Application is applied again with the add-on's new version
// (the run's last act of stage workload-platform), which is the only way the new chart reaches pc-sandbox.
func TestUpgradeMovesTheSandboxChart(t *testing.T) {
	old, ok := Lookup(previousSHA256["0.12.0-rc.1"])
	if !ok {
		t.Fatal("this build does not embed the 0.12.0-rc.1 BOM")
	}
	for _, placement := range []string{SandboxShared, SandboxSeparate} {
		t.Run(placement, func(t *testing.T) {
			cur := functionsBOM(t)
			cur.Release = "0.13.0"
			from, to := refOf(old), refOf(cur)
			sb := sharedSandbox()
			if placement == SandboxSeparate {
				sb = separateSandbox()
				sb.System.Nodes = []string{"pve1"}
			}
			p := sandboxPlatform(t, RunApply, sb)
			p.mutate(func(run *Run, in *Installation, _ *Secrets) {
				run.BOM, in.Status.BOM = from, &from
				if placement == SandboxSeparate {
					routed(in)
				}
			})
			for _, name := range []string{"pc-sandbox-sandbox-pve1-x", "pc-sandbox-sandbox-system-pve1-x"} {
				m := meta(name, map[string]any{"cluster.x-k8s.io/cluster-name": SandboxClusterName})
				m["status"] = map[string]any{"phase": "Running"}
				p.wl.put("/apis/cluster.x-k8s.io/v1beta2/namespaces/pc-sandbox/machines/"+name, m)
			}
			p.upTo(stageSeedPlatform)
			if end := p.until(30); end != "complete" {
				t.Fatalf("the installing run: %s: %+v\n%+v", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform))
			}
			installed := func() string {
				if placement == SandboxShared {
					return str(p.seed.obj(sandboxApp), "spec", "source", "targetRevision")
				}
				return str(p.seed.obj(appsPath+SandboxClusterName), "spec", "source", "helm", "valuesObject", "addonCharts", "sandbox", "version")
			}
			if installed() != "0.1.0" {
				t.Fatalf("installed with sandbox chart %q", installed())
			}
			p.start(t, to.SHA256, "upgrade lab to "+cur.Release)
			if end := p.until(30); end != "complete" {
				t.Fatalf("the upgrade: %s: %+v\n%+v", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform))
			}
			_, in, sec := p.state.snapshot()
			if installed() != FunctionsChartVersion || *in.Status.BOM != to {
				t.Fatalf("after the upgrade the sandbox chart is %q, the BOM %+v", installed(), in.Status.BOM)
			}
			// The move alone builds nothing of functions: the tick comes after it.
			if sec.FunctionWake != nil || in.Status.Sandbox.Functions != nil || p.wl.obj(wakeSecretPath) != nil {
				t.Error("the upgrade built something of functions")
			}
		})
	}
}

// deletes counts the DELETE requests the seed has taken.
func (f *fakeSeed) deletes() (n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.writes {
		if strings.HasPrefix(w, "DELETE ") {
			n++
		}
	}
	return n
}

// callsOfApply are the applies of the Application name, as <name>@<revision>.
func (f *fakeSeed) callsOfApply(name string) (out []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.applied {
		if strings.HasPrefix(a, name+"@") {
			out = append(out, a)
		}
	}
	return out
}
