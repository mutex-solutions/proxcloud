package platformlife

// sandbox_functions.go — spec.sandbox.functions: the function side of a platform's sandboxes.
//
// Functions run on the machines sandboxes run on, behind the same sandbox-proxy, on its address and under its
// certificate (docs/managed-services.md, "Functions on the sandbox machines"). A run adds three things to what it
// builds for sandboxes, and nothing when the settings do not ask:
//
//   - the functions block of the sandbox chart's values (sandboxChartValues): the chart then renders the function
//     role, the admission policies and the egress policy, and sandbox-proxy serves function hosts and calls the portal;
//   - a wake token of 32 random bytes, a new one at every apply: sealed in the installation's secrets, written as
//     Secret sandbox-system/sandbox-proxy-wake (key PROXCLOUD_FUNCTION_WAKE_TOKEN) on the cluster that runs the chart,
//     and stored as secret.functionWakeToken of the sandbox provider. It is in no Application and in no message;
//   - with placement separate, the portal's address and port in the fence of the sandbox machines (SandboxSiteConfig):
//     there sandbox-proxy runs on fenced machines.
//
// The sandbox provider's own wake token (secret.proxyWakeToken, E2B autoResume) is never written: it staying empty is
// one of the two refusals of autoResume, and the sandbox wake route takes it as its bearer.
//
// What the portal knows of functions is behind three calls of the Runner (FunctionsExist, EndFunctions,
// FunctionWakeToken): this package holds no function record.
//
// The sandbox chart has functions from FunctionsChartVersion on. Under a BOM that pins an older one the save refuses
// the block (SandboxProblems) and so does a run (sandboxAccess).

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

const (
	// FunctionsChartVersion is the first sandbox chart that renders the function objects (functions.enabled).
	FunctionsChartVersion = "0.2.0"
	// sandboxWakeSecret is the Secret sandbox-proxy reads its wake tokens from (envFrom, optional) and
	// functionWakeTokenKey the key of the functions' token in it: the proxy's variable of that name.
	sandboxWakeSecret    = "sandbox-proxy-wake"
	functionWakeTokenKey = "PROXCLOUD_FUNCTION_WAKE_TOKEN"
	// functionRoutes is where the portal's API answers sandbox-proxy for functions (wake and activity), below portalURL.
	functionRoutes = "/api/v1/internal/functions"
	maxPortalCA    = 64 << 10
)

// SandboxFunctions is spec.sandbox.functions: enabled, portalURL, portalCA.
type SandboxFunctions struct {
	Enabled bool `json:"enabled"`
	// PortalURL is the portal's API as sandbox-proxy reaches it, https://host[:port]: the proxy posts wakes and activity
	// to <portalURL>/api/v1/internal/functions, with the wake token as its bearer, which is why plain http is refused.
	// The save fills in the portal's own PROXCLOUD_SELF_API_BASE_URL when it is left empty. With placement separate
	// its host is an IPv4 address: the proxy's machines are fenced, and the fence opens one address and port.
	PortalURL string `json:"portalURL,omitempty"`
	// PortalCA is the PEM certificate sandbox-proxy trusts the portal by, for a portal whose certificate no public
	// authority signed. Public data; empty = the system's trust store.
	PortalCA string `json:"portalCA,omitempty"`
}

// FunctionsStatus is what a run built for spec.sandbox.functions: recorded once the wake token's Secret is in the
// cluster, and cleared by the apply that took functions away.
type FunctionsStatus struct {
	PortalURL string `json:"portalURL"`
	// TokenVersion names the wake token whose Secret the run wrote into the cluster (not the token, and not derived
	// from it): the chart's functions.wakeTokenVersion, an annotation of sandbox-proxy's pods, so a new token replaces
	// them.
	TokenVersion string `json:"tokenVersion"`
}

// FunctionWake is the wake token of functions in the installation's secrets. Version is the id of the run that
// minted Token, and both are saved in one write: the version changes when, and only when, the token does.
type FunctionWake struct {
	Token   string `json:"token"`
	Version string `json:"version"`
}

// RenderFunctions is spec.sandbox.functions as the renderers read it.
type RenderFunctions struct {
	WakeURL  string // <portalURL>/api/v1/internal/functions
	PortalCA string
	// TokenVersion is the version of the token whose Secret is in the cluster; "" = not written yet, and the sandbox
	// chart is still rendered without functions.
	TokenVersion string
	// PortalAddress and PortalPort are the portal as the fence of the sandbox machines names it: placement separate only.
	PortalAddress string
	PortalPort    int
}

// SandboxChartHasFunctions reports a sandbox chart that renders the function objects.
func SandboxChartHasFunctions(v string) bool {
	maj, min, patch, err := kamaji.ParseVersion(v)
	wMaj, wMin, wPatch, _ := kamaji.ParseVersion(FunctionsChartVersion)
	if err != nil || strings.Count(v, ".") != 2 {
		return false
	}
	for _, p := range [][2]int{{maj, wMaj}, {min, wMin}, {patch, wPatch}} {
		if p[0] != p[1] {
			return p[0] > p[1]
		}
	}
	return true
}

// functions is the functions block the settings ask a run to build, nil when there is none: no sandboxes, no block,
// or enabled false.
func (s Spec) functions() *SandboxFunctions {
	if s.sandboxMode() == "" || s.Sandbox.Functions == nil || !s.Sandbox.Functions.Enabled {
		return nil
	}
	return s.Sandbox.Functions
}

// portal is a portalURL taken apart.
type portal struct {
	wakeURL string
	addr    netip.Addr // the host when it is an IPv4 address, else the zero Addr
	port    int
}

func parsePortal(raw string) (portal, error) {
	u, err := url.Parse(raw)
	switch {
	case raw == "":
		return portal{}, fmt.Errorf("is empty")
	case err != nil || u.Scheme != "https" || u.Hostname() == "":
		return portal{}, fmt.Errorf("is not an https URL with a host")
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return portal{}, fmt.Errorf("carries a login, a query or a fragment")
	}
	p := portal{wakeURL: strings.TrimRight("https://"+u.Host+u.Path, "/") + functionRoutes, port: 443}
	if u.Port() != "" {
		if p.port, err = strconv.Atoi(u.Port()); err != nil || p.port < 1 || p.port > 65535 {
			return portal{}, fmt.Errorf("names no port from 1 to 65535")
		}
	}
	if a, err := netip.ParseAddr(u.Hostname()); err == nil && a.Is4() {
		p.addr = a
	}
	return p, nil
}

// shownURL is a portalURL as a message repeats it. One that carries a login is not repeated: its password would be in
// the answer of the save and in the message of a run.
func shownURL(raw string) string {
	if strings.Contains(raw, "@") {
		return "(not repeated: it carries a login)"
	}
	return raw
}

// validateFunctions checks spec.sandbox.functions of an active sandbox block. With placement separate the portal's
// address goes into the fence of the sandbox machines, which takes an IPv4 address outside the sandbox range that is
// not the sandbox cluster's own API (an address of the platform cluster's MetalLB range): one it refused would stop
// the fence of every sandbox machine, so it is refused here.
func (sb *SandboxSpec) validateFunctions(bad func(string, ...any), nodeRange netip.Prefix, rangeOK bool, lbs addrRange, rangesOK bool) {
	f := sb.Functions
	if f == nil {
		return
	}
	if f.PortalCA != "" {
		rest, certs := []byte(f.PortalCA), 0
		for len(f.PortalCA) <= maxPortalCA {
			var block *pem.Block
			if block, rest = pem.Decode(rest); block == nil {
				break
			}
			if _, err := x509.ParseCertificate(block.Bytes); block.Type != "CERTIFICATE" || err != nil {
				certs = -1
				break
			}
			certs++
		}
		if certs < 1 || strings.TrimSpace(string(rest)) != "" {
			bad("spec.sandbox.functions.portalCA is not PEM certificates and nothing else (at most %d KiB): give the certificate that signed the portal's, or leave it empty for a publicly trusted one", maxPortalCA>>10)
		}
	}
	if !f.Enabled {
		return
	}
	p, err := parsePortal(f.PortalURL)
	if err != nil {
		bad("spec.sandbox.functions.portalURL %q %s: it is the portal's API as sandbox-proxy reaches it, https://host[:port] (the save fills in PROXCLOUD_SELF_API_BASE_URL when that is set)", shownURL(f.PortalURL), err)
		return
	}
	if sb.Placement != SandboxSeparate {
		return
	}
	proxy, _ := netip.ParseAddr(sb.ProxyAddress)
	switch {
	case !p.addr.IsValid():
		bad("spec.sandbox.functions.portalURL %q: with placement separate its host is an IPv4 address: sandbox-proxy runs on fenced machines, and their fence opens one address and port for the portal", f.PortalURL)
	case (rangeOK && nodeRange.Contains(p.addr)) || p.addr == proxy || (rangesOK && lbs.contains(p.addr)):
		bad("spec.sandbox.functions.portalURL %q: %s is inside spec.sandbox.nodeRange or the cluster's metallbRange, or is the proxy address: the portal answers at an address of its own", f.PortalURL, p.addr)
	}
}

// functionsChartProblem is why bom cannot build the functions the settings ask for, "" when it can or they ask for none.
func functionsChartProblem(s Spec, bom *BOM) string {
	c := bom.Feature(FeatureSandbox)
	if s.functions() == nil || c == nil || SandboxChartHasFunctions(c.Chart.Version) {
		return ""
	}
	why := fmt.Sprintf("spec.sandbox.functions: the platform's BOM (release %s) pins sandbox chart %s, which has no functions: they need chart %s or newer. ",
		bom.Release, c.Chart.Version, FunctionsChartVersion)
	if cur, err := Current(); err == nil && cur.Release != bom.Release && cur.Feature(FeatureSandbox) != nil && SandboxChartHasFunctions(cur.Feature(FeatureSandbox).Chart.Version) {
		return why + "Upgrade the platform to release " + cur.Release + " first; until then leave spec.sandbox.functions out, or set enabled false"
	}
	return why + "Leave spec.sandbox.functions out, or set enabled false"
}

// FunctionsSwitchedOff reports that next takes away the functions a run built for old while its sandboxes stay
// (functions.enabled false), and names the sandbox provider a run registered ("" = none). Sandboxes switched off as a
// whole are SandboxSwitchedOff's: that keeps the function records, and the sandboxes built again run them again.
func FunctionsSwitchedOff(old, next *Installation) (providerID string, off bool) {
	if st := old.Status.Sandbox; st == nil || st.Functions == nil || next.Spec.sandboxMode() == "" || next.Spec.functions() != nil {
		return "", false
	}
	return sandboxProviderID(old), true
}

func sandboxProviderID(in *Installation) string {
	if p := in.Status.Providers; p != nil {
		return p.Sandbox
	}
	return ""
}

// FunctionsOffBlocked is why functions are not taken away from sandboxes that stay: the provider still holds function
// records, and without the function objects of the chart nothing could run, stop or delete them on the cluster.
const FunctionsOffBlocked = "The sandbox provider still holds functions: switching functions off takes the role and the policies their Deployments need off the cluster. " +
	"Have every function deleted first (a project deletes its own; they stay deletable with the provider's functions block off), then set spec.sandbox.functions.enabled false."

// functionsUnknown is the refusal of a build that cannot ask the portal about function records.
const functionsUnknown = "This build has no way to read or end the function records of the sandbox provider, so it does not take functions or their cluster away."

// functionsOff runs in stage cluster for settings that take functions away from sandboxes that stay: refused while the
// provider holds a function record; then every running interval is ended in the records (there is none left to end
// unless a function was made since the save).
func (x *acq) functionsOff(ctx context.Context) error {
	id, off := FunctionsSwitchedOff(x.in, x.in)
	if !off {
		return nil
	}
	if id != "" {
		if x.r.FunctionsExist == nil {
			return permanent(CodeSandboxRefused, "%s", functionsUnknown)
		}
		exist, err := x.r.FunctionsExist(ctx, id)
		if err != nil {
			return classify(CodeSandboxRefused, err)
		}
		if exist {
			return permanent(CodeSandboxRefused, "%s Nothing was changed.", FunctionsOffBlocked)
		}
	}
	return x.endFunctions(ctx)
}

// endFunctions ends, in the portal's records, every running interval of the provider's functions, before a run takes
// their cluster or their objects away: the sandboxes switched off (which is also how the separate cluster is rebuilt
// for another Kubernetes version), functions switched off, a Remove. The charge reads only the record, so a function
// recorded as running would be billed for as long as its cluster is gone. The records themselves stay.
func (x *acq) endFunctions(ctx context.Context) error {
	st, id := x.in.Status.Sandbox, sandboxProviderID(x.in)
	if st == nil || st.Functions == nil || id == "" {
		return nil
	}
	if x.r.EndFunctions == nil {
		return permanent(CodeSandboxRefused, "%s", functionsUnknown)
	}
	if err := x.check(ctx); err != nil {
		return err
	}
	return classify(CodeSandboxRefused, x.r.EndFunctions(ctx, id))
}

// providerWakeToken stores token on the installation's sandbox provider ("" takes it away). Before the provider is
// registered there is no row: stage register stores it then.
func (x *acq) providerWakeToken(ctx context.Context, id, token string) error {
	if x.r.FunctionWakeToken == nil || id == "" {
		return nil
	}
	err := x.r.FunctionWakeToken(ctx, x.in, id, token)
	if err != nil && strings.Contains(err.Error(), CodeProviderConflict) {
		return permanent(CodeProviderConflict, "%s", clip(err.Error()))
	}
	return classify(CodeSandboxRefused, err)
}

// builtWakeToken is the token stage register stores on the provider: the one whose Secret is in the cluster and whose
// version the chart was rendered with, "" when the settings ask for no functions or the Secret is not written yet.
func (x *acq) builtWakeToken() string {
	st, wake := x.in.Status.Sandbox, x.sec.FunctionWake
	if x.in.Spec.functions() == nil || st == nil || st.Functions == nil || wake == nil || st.Functions.TokenVersion != wake.Version {
		return ""
	}
	return wake.Token
}

// functionWake keeps the wake token of functions on target, the cluster that runs the sandbox chart: the platform
// cluster before the chart's wave (shared), pc-sandbox once its control plane answers (separate).
//
// Settings that ask for functions: every apply mints a token of its own, once (a leaked token is replaced by an
// apply), writes its Secret and records the token's version. Only that record makes the next render carry the
// functions block with the version, so sandbox-proxy never starts for functions before its token is there, and is
// replaced when the token is: it reads the token only when it starts. The provider takes the new token, and no other,
// in stage register (registerSandbox), after the chart's wave: until then the replicas not yet replaced go on with
// the token the provider still holds, so a run that fails before the proxy is replaced leaves functions as they
// were. Between a replica's start with the new token and stage register that replica's wakes and reports are
// refused: a request it holds for a sleeping function is answered 503 for that time.
//
// Settings that ask for none, after a run built them: the provider's token, the Secret, the sealed token and the
// record go. Nothing is read or written when no run ever built functions.
func (x *acq) functionWake(ctx context.Context, target *kamajik8s.Client) error {
	st, fn, id := x.in.Status.Sandbox, x.in.Spec.functions(), sandboxProviderID(x.in)
	if st == nil {
		return nil
	}
	if fn == nil {
		if st.Functions == nil && x.sec.FunctionWake == nil {
			return nil
		}
		if err := x.providerWakeToken(ctx, id, ""); err != nil {
			return err
		}
		if err := x.check(ctx); err != nil {
			return err
		}
		if err := target.DeleteSecret(ctx, sandboxNamespace, sandboxWakeSecret); err != nil && !kamajik8s.NotFound(err) {
			return classify(CodeWorkloadUnreachable, err)
		}
		return x.save(ctx, func(_ *Run, in *Installation, sec *Secrets) {
			if sec.FunctionWake = nil; in.Status.Sandbox != nil {
				in.Status.Sandbox.Functions = nil
			}
		})
	}
	if wake := x.sec.FunctionWake; wake == nil || wake.Version != x.run.ID {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return permanent(CodeSandboxRefused, "The wake token of functions could not be made: no random bytes.")
		}
		next := &FunctionWake{Token: hex.EncodeToString(raw), Version: x.run.ID}
		if err := x.save(ctx, func(_ *Run, _ *Installation, sec *Secrets) { sec.FunctionWake = next }); err != nil {
			return err
		}
	} else if err := x.check(ctx); err != nil {
		return err
	}
	wake := x.sec.FunctionWake
	if err := target.EnsureNamespace(ctx, sandboxNamespace, map[string]string{"app.kubernetes.io/managed-by": ManagedBy}, nil); err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	s := object("v1", "Secret", sandboxNamespace, sandboxWakeSecret)
	s["type"], s["stringData"] = "Opaque", map[string]any{functionWakeTokenKey: wake.Token}
	if err := target.ApplyCore(ctx, "secrets", s); err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	built := FunctionsStatus{PortalURL: fn.PortalURL, TokenVersion: wake.Version}
	if st.Functions != nil && *st.Functions == built {
		return nil
	}
	return x.save(ctx, func(_ *Run, in *Installation, _ *Secrets) {
		if in.Status.Sandbox != nil {
			in.Status.Sandbox.Functions = &built
		}
	})
}

// functionsChanges are the plan rows of spec.sandbox.functions on an installed platform whose sandboxes stay or are
// being added, against what a run built: on, off, and for functions that stay the token every apply replaces.
func (in *Installation) functionsChanges() []PlanChange {
	fn, sb, built := in.Spec.functions(), in.Spec.Sandbox, (*FunctionsStatus)(nil)
	if st := in.Status.Sandbox; st != nil {
		built = st.Functions
	}
	row := PlanChange{Cluster: ClusterName, Name: "sandbox-functions", Class: ClassChart, Stage: stageWorkloadPlatform}
	const roll = "sandbox-proxy is replaced one replica at a time: a connection open through the replaced replica, to a sandbox or to a function, is cut once. No sandbox ends and no machine is replaced."
	switch {
	case fn == nil && built == nil:
		return nil
	case fn == nil:
		row.From, row.To = "on", "off"
		row.Impact = "Functions are switched off. Refused while the sandbox provider holds a function: every function is deleted first. The run then renders the sandbox chart without " +
			"the function objects (the role, the policies, the function hosts of sandbox-proxy) and deletes the wake token: its Secret on the cluster, the sealed copy and the provider's. " + roll
		return []PlanChange{row}
	}
	where, cluster := "", ClusterName
	if sb.Placement == SandboxSeparate {
		cluster = SandboxClusterName
		if p, err := parsePortal(fn.PortalURL); err == nil && p.addr.IsValid() {
			where = fmt.Sprintf(" sandbox-proxy runs on fenced machines there, so the fence of the sandbox machines opens %s port %d and nothing else new.", p.addr, p.port)
		}
	}
	if built != nil {
		row.From, row.To = "on ("+built.PortalURL+")", "on ("+fn.PortalURL+", a new wake token)"
		row.Impact = "Every apply replaces the wake token of functions: the run mints a new one, the sandbox provider takes only the new one from then on, and until the last replica of " +
			"sandbox-proxy is replaced a request to a sleeping function may be answered 503. " + roll
		if built.PortalURL != fn.PortalURL {
			row.Impact += " sandbox-proxy then calls the portal at " + fn.PortalURL + functionRoutes + "." + where
		}
		return []PlanChange{row}
	}
	row.To = "on (" + fn.PortalURL + ")"
	row.Impact = "The sandbox chart on " + cluster + " is rendered with functions: a second role for the portal's sandbox identity (Deployments, their scale and write-only Secrets, in fn-* " +
		"namespaces only), the admission policies that hold a function pod to gVisor without a volume, a policy that opens the internet to function pods, and sandbox-proxy serving " +
		"https://<name>-<6 hex>." + sb.Domain + " on the sandboxes' address and certificate. The run mints a wake token of 32 random bytes, keeps it sealed, and writes it as Secret " +
		sandboxNamespace + "/" + sandboxWakeSecret + " on that cluster and on the sandbox provider; it is in no Application. sandbox-proxy posts wakes and activity to " + fn.PortalURL + functionRoutes + "." + where +
		" " + roll + " Every later apply replaces the token and sandbox-proxy with it. Tenants get functions only once the sandbox provider's functions block is enabled on its page. "
	if sb.Placement == SandboxShared {
		row.Impact += "Function instances run on the sandbox machines of " + ClusterName + ": code that escapes gVisor there also reads the Secrets (environment secrets, registry logins) of every " +
			"function with an instance on that machine. A machine rotation does not wait for function instances: they end with the machine and their Deployment starts them again."
	} else {
		row.Impact += "Code that escapes gVisor on a sandbox machine reads the Secrets (environment secrets, registry logins) of every function with an instance on that machine. " +
			"A running function instance holds a machine rotation back as a sandbox does. A rebuild of " + SandboxClusterName + " (placement off, then separate again) stops every function until the " +
			"portal has deployed them again from its records."
	}
	return []PlanChange{row}
}

// functionsStop is the sentence a row that deletes the sandboxes' cluster adds when a run built functions on it.
func (in *Installation) functionsStop() string {
	if st := in.Status.Sandbox; st == nil || st.Functions == nil {
		return ""
	}
	return " Functions stop with it: before anything is deleted the run ends every running interval of every function in the portal's records, so none is billed while its cluster " +
		"is gone. The function records stay, and the portal deploys them again when sandboxes are built again with functions."
}

// featureRows makes the rows Classify gave for the components of a feature say what the move does to this
// installation. Classify compares the two BOMs as they are, and a feature's component is rendered for some
// installations only (BOM.For): the sandbox chart is an Application of the platform cluster with placement shared, an
// add-on of pc-sandbox with separate, and nothing without sandboxes.
func (in *Installation) featureRows(changes []PlanChange, to *BOM) []PlanChange {
	c := to.Feature(FeatureSandbox)
	mode := "" // what a run built and the settings keep: only that is moved (a chart installed by this apply is the sandbox row's)
	if st := in.Status.Sandbox; st != nil && in.Spec.sandboxMode() == st.Placement {
		mode = st.Placement
	}
	out := changes[:0:0]
	for _, row := range changes {
		if c == nil || row.Cluster != c.Cluster || row.Name != c.Name || row.Class != ClassChart {
			out = append(out, row)
			continue
		}
		const proxy = " Where the new chart changes sandbox-proxy (its image or its settings), its replicas are replaced one at a time, and a connection open through the " +
			"replaced replica, to a sandbox or to a function, is cut once. Running sandboxes stay, and no machine is replaced."
		switch mode {
		case SandboxShared:
			row.Impact = "The Application " + ClusterName + "-" + c.Name + " moves to the new chart version. ArgoCD syncs only the objects the new chart renders differently." + proxy
		case SandboxSeparate:
			row.Wave = nil
			row.Impact = "The sandbox chart is an add-on of the cluster " + SandboxClusterName + ": the run writes the new version into that cluster's Application on the seed " +
				"(addonCharts.sandbox.version), and the add-on controller of the platform cluster upgrades the release inside " + SandboxClusterName + " in place." + proxy
		default:
			continue // no sandboxes built: nothing of the chart runs for this installation, so the move changes nothing of it
		}
		out = append(out, row)
	}
	return out
}
