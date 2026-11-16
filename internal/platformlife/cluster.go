package platformlife

// cluster.go — stages 6 to 11 of an apply (design §4.2): the seed's ArgoCD and Cluster API, the workload cluster, the
// seed ArgoCD's access to it, the workload's components wave by wave, the two providers. Each observes first: every
// write is a server-side apply, and what a stage waits for is read again at every acquisition. The workload's admin
// kubeconfig, the argocd-manager token and the providers' token are held in memory only.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

const (
	capiGroup, capiVersion   = "cluster.x-k8s.io", "v1beta2"
	infraGroup, infraVersion = "infrastructure.cluster.x-k8s.io", "v1alpha2"
	argoGroup, argoVersion   = "argoproj.io", "v1alpha1"
	rbacGroup                = "rbac.authorization.k8s.io"
	clusterSelector          = "cluster.x-k8s.io/cluster-name=" + ClusterName
	kubeconfigSecret         = ClusterName + "-kubeconfig" // Cluster API's admin kubeconfig, on the seed only
	argoManager              = "argocd-manager"            // the seed ArgoCD's service account on the workload
	argoClusterSecret        = "cluster-" + ClusterName    // the seed ArgoCD's cluster Secret: the one place the token is
	managerUIDAnnotation     = "proxcloud.io/argocd-manager-uid"
	tokenExpiryAnnotation    = "proxcloud.io/token-expires-at"
	managerTokenTTL          = 365 * 24 * time.Hour
	managerTokenRenew        = 90 * 24 * time.Hour
	csiDriver                = "csi.proxmox.sinextra.dev"
	providerNamespace        = "proxcloud-system" // the providers' service account and its token Secret on the workload
	providerTokenSecret      = "proxcloud-token"
	waveStall                = 20 * time.Minute // stage 9 fails when a wave does not finish within this
	// capmoxSilence is how long stage 7 waits for CAPMOX's first condition on the ProxmoxCluster. CAPMOX opens the
	// Proxmox API before it writes anything, so a URL the seed cannot reach or a token Proxmox refuses shows as no
	// condition at all (pkg/scope/cluster.go at v0.9.2: "Unable to initialize ProxmoxClient", nothing patched).
	capmoxSilence = 10 * time.Minute
	capmoxLog     = "kubectl -n capmox-system logs deploy/capmox-controller-manager"
	// wokenAnnotation is what a run writes on a ProxmoxCluster that CAPMOX left without a condition before the run
	// began (a Retry): CAPMOX v0.9.2 tries a ProxmoxCluster whose Proxmox API it could not open again only on
	// controller-runtime's backoff, up to 1000 s apart, longer than capmoxSilence, and watches nothing a fix touches.
	// Any update of the object makes it look at once (its only predicates are not paused, not externally managed).
	wokenAnnotation = "proxcloud.io/retried-by"
	// syncAttempts is how often one sync operation must have failed before a stage fails on it while ArgoCD still
	// tries: 30s, 1m, 2m and 4m of backoff, about 8 minutes, which a slow image pull survives. The Applications'
	// retry policy (ten attempts, about 40 minutes) outlasts the time limit of stage 6 and of a wave of stage 9.
	syncAttempts = 4
	kcpGroup     = "controlplane.cluster.x-k8s.io"
)

// oneLine is a condition message on one line: Cluster API's v1beta2 summaries are bullet lists.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// CAPMOX's condition on a ProxmoxMachine (api/v1alpha2/conditions_consts.go at v0.9.2). It is False with the step as its
// reason while the VM is built. Two reasons end the machine: with VMProvisionFailed the controller skips it from then
// on (MachineScope.HasFailed), and after TaskFailed (a Proxmox task but qmstart failed) every step of its state machine
// is skipped, because each runs only at its own reason. CloningFailed and PoweringOnFailed are tried again.
const vmProvisioned = "VirtualMachineProvisioned"

func capmoxGaveUp(c map[string]any) bool {
	return str(c, "status") == "False" && (str(c, "reason") == "VMProvisionFailed" || str(c, "reason") == "TaskFailed")
}

// dig reads the value at path in a decoded Kubernetes object (nil when absent); str reads a string there.
func dig(obj any, path ...string) any {
	v := obj
	for _, k := range path {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}

func str(obj map[string]any, path ...string) string {
	s, _ := dig(obj, path...).(string)
	return s
}

// cond is obj's condition of type typ (nil when it has none); condition is its status.
func cond(obj map[string]any, typ string) map[string]any {
	conds, _ := dig(obj, "status", "conditions").([]any)
	for _, c := range conds {
		if m, _ := c.(map[string]any); str(m, "type") == typ {
			return m
		}
	}
	return nil
}

func condition(obj map[string]any, typ string) string { return str(cond(obj, typ), "status") }

// seed is the seed's API with the stored service-account credential, TLS pinned to the seed's CA.
func (x *acq) seed() (*kamajik8s.Client, error) {
	if x.in.Status.Seed == nil || x.sec.SeedKubeconfig == nil {
		return nil, permanent(CodeSeedIdentityLost, "The installation holds no credential for its seed.")
	}
	return seedClient(x.in.Status.Seed, x.sec.SeedKubeconfig.Token)
}

// workload is the workload cluster's API with the admin kubeconfig Cluster API keeps on the seed; nil while the seed
// does not hold one. Nothing stores the kubeconfig.
func (x *acq) workload(ctx context.Context, seed *kamajik8s.Client) (*kamajik8s.Client, error) {
	data, err := seed.GetSecretData(ctx, ClusterName, kubeconfigSecret)
	if err != nil || len(data["value"]) == 0 {
		return nil, classify(CodeSeedAPIUnreachable, err)
	}
	c, err := kamajik8s.New(string(data["value"]))
	if err != nil { // the parse error could quote the kubeconfig
		return nil, permanent(CodeWorkloadUnreachable, "The kubeconfig Cluster API wrote for %s is not usable.", ClusterName)
	}
	return c, nil
}

// clusters is seed with workload, for the stages that need the workload to answer.
func (x *acq) clusters(ctx context.Context) (seed, wl *kamajik8s.Client, err error) {
	if seed, err = x.seed(); err != nil {
		return nil, nil, err
	}
	if wl, err = x.workload(ctx, seed); err == nil && wl == nil {
		err = permanent(CodeWorkloadUnreachable, "The seed holds no %s/%s: stage %s builds it.", ClusterName, kubeconfigSecret, stageCluster)
	}
	return seed, wl, err
}

// hide adds a value a later message must not carry.
func (x *acq) hide(v string) {
	if len(v) >= 6 {
		x.hidden = append(x.hidden, v)
	}
}

// renderInput is what the renderers read of the installation, its Proxmox provider and stage 2.
func (x *acq) renderInput() RenderInput {
	sp, c := x.in.Spec, x.in.Spec.Clusters[0]
	prefix, _ := netip.ParsePrefix(sp.Network.Prefix)
	region := x.es.PVERegion()
	templates := sp.NodeImage.Templates
	if sp.NodeImage.Mode == ImageBOM {
		templates = map[string]int{}
		for _, s := range x.run.Stages {
			for k, vmid := range receiptTemplates(s) {
				if node, ok := strings.CutPrefix(k, "node:"); ok {
					templates[node] = vmid
				}
			}
		}
	}
	mtu := 0 // the smallest MTU of the tenant zones, which a worker's interface must fit
	for _, z := range x.es.PVENetworkConfig().Zones {
		if z.MTU > 0 && (mtu == 0 || z.MTU < mtu) {
			mtu = z.MTU
		}
	}
	in := RenderInput{ID: x.in.ID, Name: x.in.Name, Region: region, RegionName: str(x.es.Config, "regions", region, "displayName"),
		PVEAPIURL: sp.PVEAPIURL, BootStorage: sp.Storage.Boot, SeedAddress: sp.Seed.Address, SSHKeys: sp.Seed.SSHKeys, Templates: templates, WorkerMTU: mtu,
		Network: RenderNetwork{Bridge: sp.Network.Bridge, Gateway: sp.Network.Gateway, Prefix: prefix.Bits(), DNS: sp.Network.DNS, MTU: sp.Network.MTU},
		Cluster: RenderCluster{Endpoint: c.Endpoint, NodeRange: []string{c.NodeRange}, MetalLBRange: c.MetalLBRange,
			ControlPlane: RenderMachine{Node: c.ControlPlane.Node, Cores: c.ControlPlane.Cores, MemoryMiB: c.ControlPlane.MemoryMiB, DiskGiB: c.ControlPlane.DiskGiB}}}
	for _, g := range c.NodeGroups {
		in.Cluster.NodeGroups = append(in.Cluster.NodeGroups, RenderNodeGroup{Name: g.Name, Nodes: g.Nodes, Count: g.Count, Cores: g.Cores, MemoryMiB: g.MemoryMiB, DiskGiB: g.DiskGiB})
	}
	return in
}

func receiptTemplates(s Stage) map[string]int {
	if s.ID != StageImages || s.Receipt == nil {
		return nil
	}
	return s.Receipt.Templates
}

// appOf is a BOM component with the Application rendered for it.
type appOf struct {
	c   Component
	obj map[string]any
}

func (a appOf) name() string { return str(a.obj, "metadata", "name") }

// apps are the Applications of one cluster's components, in BOM order.
func (x *acq) apps(cluster string) ([]appOf, error) {
	objs, err := Applications(x.bom, x.renderInput())
	if err != nil {
		return nil, permanent(CodeComponentUnhealthy, "The Applications could not be rendered: %s", clip(err.Error()))
	}
	var out []appOf
	i := 0
	for _, c := range x.bom.Components {
		if c.Install != InstallApplication {
			continue
		}
		if c.Cluster == cluster {
			out = append(out, appOf{c, objs[i]})
		}
		i++
	}
	return out, nil
}

// appState is an Application as ArgoCD reports it: ok when it is Synced and Healthy, that was compared against the
// source the Application has now (both read in one GET), so the status of another version or of other values does
// not pass, and no sync is asked for, runs or ended Failed or Error: ArgoCD reports Synced and Healthy from the
// resources alone, while a Sync or PostSync hook of the same operation still runs or keeps failing. An Application
// without any operation passes: nothing ran a hook on it. The detail names the resources and hooks the last operation
// failed on, with their messages.
func appState(app map[string]any) (state, detail string, ok bool) {
	sync, health := str(app, "status", "sync", "status"), str(app, "status", "health", "status")
	phase := str(app, "status", "operationState", "phase")
	var msgs []string
	conds, _ := dig(app, "status", "conditions").([]any)
	for _, c := range conds {
		m, _ := c.(map[string]any)
		msgs = append(msgs, str(m, "type")+": "+str(m, "message"))
	}
	if m := str(app, "status", "operationState", "message"); m != "" {
		msgs = append(msgs, m)
	}
	msgs = append(msgs, syncFailures(app)...)
	if sync == "" {
		sync, health = "Unknown", "Unknown"
	}
	state = sync + "/" + health
	if phase != "" && phase != "Succeeded" {
		state += ", sync " + phase
	}
	source := dig(app, "spec", "source")
	ok = sync == "Synced" && health == "Healthy" && source != nil && reflect.DeepEqual(source, dig(app, "status", "sync", "comparedTo", "source")) &&
		app["operation"] == nil && (phase == "" || phase == "Succeeded")
	return state, strings.Join(msgs, "; "), ok
}

// syncFailures are the resources and hooks the Application's last sync attempt failed on, each with its message.
func syncFailures(app map[string]any) []string {
	var out []string
	results, _ := dig(app, "status", "operationState", "syncResult", "resources").([]any)
	for _, r := range results {
		m, _ := r.(map[string]any)
		if hp := str(m, "hookPhase"); hp == "Failed" || hp == "Error" || str(m, "status") == "SyncFailed" {
			out = append(out, str(m, "kind")+" "+str(m, "namespace")+"/"+str(m, "name")+": "+str(m, "message"))
		}
	}
	return out
}

// clusterctlProviders are the providers a component's clusterctl Sync hook installs (false: it has no such hook).
func clusterctlProviders(b *BOM, c Component) (Providers, bool) {
	switch c.Cluster + "/" + c.Name {
	case "seed/capi":
		return b.CAPI.Seed, true
	case "workload/capi-tenant":
		return b.CAPI.Workload, true
	}
	return Providers{}, false
}

// inventory gates a clusterctl component on what its hook installed, which shows in neither sync nor health: the
// clusterctl inventory of target must equal the BOM. "" = equal; otherwise the hook has to run again, and only a sync
// runs it (resync). A provider at another version is permanent: the hook refuses it, and provider upgrades are not
// part of this release.
func inventory(ctx context.Context, target *kamajik8s.Client, want Providers) (string, error) {
	items, err := target.ListCRsAllNamespaces(ctx, "clusterctl.cluster.x-k8s.io", "v1alpha3", "providers", "")
	if err != nil && !kamajik8s.NotFound(err) { // 404: clusterctl has not created its CRD yet
		return "", classify(CodeCAPIInitFailed, err)
	}
	have := map[string]string{}
	for _, p := range items {
		have[str(p, "type")+" "+str(p, "providerName")] = str(p, "version")
	}
	var missing []string
	for typ, ref := range map[string]string{"CoreProvider": want.Core, "BootstrapProvider": want.Bootstrap, "ControlPlaneProvider": want.ControlPlane,
		"InfrastructureProvider": want.Infrastructure, "IPAMProvider": want.IPAM, "AddonProvider": want.Addon} {
		name, version, _ := strings.Cut(ref, ":")
		switch got := have[typ+" "+name]; {
		case ref == "" || got == version:
		case got == "": // the type too: bootstrap and control plane are both kubeadm at one version
			missing = append(missing, strings.TrimSuffix(typ, "Provider")+" "+ref)
		default:
			return "", permanent(CodeCAPIInitFailed, "%s %s is installed at %s and the BOM names %s: provider upgrades come with a later release.", typ, name, got, version)
		}
	}
	if len(missing) == 0 {
		return "", nil
	}
	slices.Sort(missing)
	return "clusterctl has not installed " + strings.Join(missing, ", ") + " yet", nil
}

// resync starts the sync of an Application that will not sync by itself: ArgoCD never syncs a revision again whose
// last operation ended Failed or Error (its retries are spent, and self-heal runs only after a success), and a
// clusterctl hook that ended Succeeded without its providers (hook) runs again only in a new sync. Nothing while an
// operation is requested or runs. The operation carries the Application's retry policy, so a sync that keeps failing
// backs off instead of starting at every acquisition. "" = nothing started. A run starts the sync of a failed
// Application once (the stage receipt names it): when that one ends Failed or Error too, its retries are spent a
// second time and the stage fails with code and detail, what the operation failed on, instead of waiting for its
// deadline. The next run (Retry) starts it once more.
//
// An operation that fails is not waited out either. Between two attempts ArgoCD (v3.6.1, controller/appcontroller.go)
// keeps the phase Running with finishedAt set, the retry count raised and the failed attempt's resources: when that
// shows syncAttempts failures, the last of them after this stage began (so a Retry gives the operation one attempt
// with the cause fixed), the stage fails with what the attempt failed on.
func (x *acq) resync(ctx context.Context, seed *kamajik8s.Client, a appOf, live map[string]any, hook bool, code, detail string) (string, error) {
	phase := str(live, "status", "operationState", "phase")
	if n, _ := dig(live, "status", "operationState", "retryCount").(float64); phase == "Running" && n >= syncAttempts {
		at, err := time.Parse(time.RFC3339, str(live, "status", "operationState", "finishedAt"))
		if fails := syncFailures(live); err == nil && len(fails) > 0 && at.After(*x.run.Stages[x.si].StartedAt) {
			return "", permanent(code, "The sync of %s failed %d times and ArgoCD still tries it: %s", a.name(), int(n), clip(strings.Join(fails, "; ")))
		}
	}
	if live["operation"] != nil || (phase != "Failed" && phase != "Error" && (phase != "Succeeded" || !hook)) {
		return "", nil
	}
	failed := phase != "Succeeded"
	if failed && slices.Contains(x.receipt().Resynced, a.name()) {
		return "", permanent(code, "The sync of %s ended %s again after this run had started it once more: %s", a.name(), phase, clip(detail))
	}
	if err := x.check(ctx); err != nil {
		return "", err
	}
	op := map[string]any{"initiatedBy": map[string]any{"username": ManagedBy}, "retry": dig(a.obj, "spec", "syncPolicy", "retry"),
		"sync": map[string]any{"revision": a.c.Chart.Version, "syncOptions": dig(a.obj, "spec", "syncPolicy", "syncOptions")}}
	if _, err := seed.MergePatchCR(ctx, argoGroup, argoVersion, "applications", argoNamespace, a.name(), map[string]any{"operation": op}); err != nil {
		return "", classify(code, err)
	}
	if failed { // after the patch: a crash between the two starts one sync more, never a failure for a sync not started
		if err := x.recordReceipt(ctx, func(r *StageReceipt, _ *Installation, _ *Secrets) { r.Resynced = append(r.Resynced, a.name()) }); err != nil {
			return "", err
		}
	}
	return "its sync ended " + phase + "; the run started it again", nil
}

// waves applies apps wave by wave on the seed's ArgoCD, each wave only once the one before is Synced and Healthy
// (and, for a clusterctl component, its inventory on target equals the BOM); after runs when a wave is. With stall, the stage's deadline starts again when the run reaches a wave for the first time: every
// acquisition walks the waves from the first, and the ones the stored progress already names leave the deadline alone.
func (x *acq) waves(ctx context.Context, seed, target *kamajik8s.Client, apps []appOf, stall time.Duration, after func(wave int) error) error {
	var order []int
	for _, a := range apps {
		order = append(order, a.c.Wave)
	}
	slices.Sort(order)
	order = slices.Compact(order)
	var items []StageItem
	seen := 0 // the wave the stored progress names
	_, _ = fmt.Sscanf(x.run.Stages[x.si].Progress, "wave %d", &seen)
	for i, w := range order {
		if err := x.check(ctx); err != nil {
			return err
		}
		code, pending, why := CodeComponentUnhealthy, []string(nil), ""
		for _, a := range apps {
			if a.c.Wave != w {
				continue
			}
			if err := seed.ApplyApplication(ctx, a.obj); err != nil {
				return classify(CodeComponentUnhealthy, err)
			}
			live, err := seed.GetApplication(ctx, argoNamespace, a.name())
			if err != nil {
				return classify(CodeComponentUnhealthy, err)
			}
			state, detail, ok := appState(live)
			hook, appCode := false, CodeComponentUnhealthy
			if want, has := clusterctlProviders(x.bom, a.c); has {
				msg, err := inventory(ctx, target, want)
				if err != nil {
					return err
				}
				if msg != "" {
					hook, appCode = true, CodeCAPIInitFailed
					ok, code, detail = false, CodeCAPIInitFailed, msg+"; "+detail
				}
			}
			detail = strings.TrimSuffix(detail, "; ")
			if ok {
				detail = "" // what ArgoCD still shows of an earlier attempt (a condition it has not cleared) is no news
			}
			if !ok {
				pending = append(pending, a.name()+" ("+state+")")
				msg, err := x.resync(ctx, seed, a, live, hook, appCode, detail)
				if err != nil { // the item first: it names what the sync failed on
					items = append(items, StageItem{ID: a.name(), State: state, Message: clip(detail)})
					if serr := x.saveStage(ctx, func(s *Stage) { s.Items = items }); serr != nil {
						return serr
					}
					return err
				}
				if msg != "" {
					detail = strings.TrimSuffix(msg+"; "+detail, "; ")
				}
				if why == "" && detail != "" {
					why = " " + a.name() + ": " + detail
				}
			}
			items = append(items, StageItem{ID: a.name(), State: state, Message: clip(detail)})
		}
		progress := fmt.Sprintf("wave %d of %d", i+1, len(order))
		// Written where the run stands: a new wave, the stored one, or an earlier one that is pending again.
		if reached := i+1 > seen; reached || ((len(pending) > 0 || i+1 == seen) && !slices.Equal(x.run.Stages[x.si].Items, items)) {
			if err := x.saveStage(ctx, func(s *Stage) {
				if reached {
					if s.Progress = progress; stall > 0 {
						now := time.Now().UTC()
						d := now.Add(stall)
						s.LastProgressAt, s.Deadline = &now, &d
					}
				}
				s.Items = items
			}); err != nil {
				return err
			}
		}
		if len(pending) > 0 {
			return waiting(code, "Waiting for %s to be Synced and Healthy with its sync Succeeded (%s).%s", strings.Join(pending, ", "), progress, clip(why))
		}
		if after != nil {
			if err := after(w); err != nil {
				return err
			}
		}
	}
	return nil
}

// seedPlatform is stage 6: the seed's ArgoCD (a k3s HelmChart created through the API), its project and repositories,
// then the seed's components but the workload cluster, done when the clusterctl inventory equals the BOM.
func (x *acq) seedPlatform(ctx context.Context) error {
	seed, err := x.seed()
	if err != nil {
		return err
	}
	in := x.renderInput()
	chart, err := SeedArgoCD(x.bom, in)
	if err != nil {
		return permanent(CodeComponentUnhealthy, "The seed's ArgoCD could not be rendered: %s", clip(err.Error()))
	}
	if err := x.check(ctx); err != nil {
		return err
	}
	// A rotation that died between storing the new credential and deleting the old Secrets left a token Secret that
	// nothing stores: every one but the stored credential's goes.
	if stored := x.in.Status.Seed.TokenSecret; stored != "" {
		all, err := seed.ListSecrets(ctx, SeedPortalNamespace, "")
		if err != nil {
			return classify(CodeSeedAPIUnreachable, err)
		}
		for _, s := range all {
			if name := str(s, "metadata", "name"); strings.HasPrefix(name, strings.TrimSuffix(SeedPortalTokenSecret, "1")) && name != stored {
				if err := seed.DeleteSecret(ctx, SeedPortalNamespace, name); err != nil {
					return classify(CodeSeedAPIUnreachable, err)
				}
			}
		}
	}
	if err := seed.ApplyCR(ctx, "helm.cattle.io", "v1", "helmcharts", chart); err != nil {
		return classify(CodeComponentUnhealthy, err)
	}
	dep, err := seed.GetCR(ctx, "apps", "v1", "deployments", argoNamespace, "argocd-repo-server")
	if err != nil {
		return classify(CodeComponentUnhealthy, err)
	}
	if n, _ := dig(dep, "status", "availableReplicas").(float64); n < 1 {
		return waiting(CodeComponentUnhealthy, "The seed's ArgoCD is not up yet: %s/argocd-repo-server has no available replica.", argoNamespace)
	}
	for _, obj := range SeedProjects(x.bom, in) {
		if obj["kind"] == "Secret" {
			err = seed.ApplyCore(ctx, "secrets", obj)
		} else {
			err = seed.ApplyCR(ctx, argoGroup, argoVersion, "appprojects", obj)
		}
		if err != nil {
			return classify(CodeComponentUnhealthy, err)
		}
	}
	apps, err := x.apps(ClusterSeed)
	if err != nil {
		return err
	}
	apps = slices.DeleteFunc(apps, func(a appOf) bool { return a.c.Name == ClusterName })
	return x.waves(ctx, seed, seed, apps, 0, nil)
}

// machineDeployments are the MachineDeployments the cluster chart renders: one per node group and PVE node.
func machineDeployments(c ClusterSpec) []string {
	var out []string
	for _, g := range c.NodeGroups {
		for _, n := range g.Nodes {
			out = append(out, ClusterName+"-"+g.Name+"-"+n)
		}
	}
	return out
}

// cluster is stage 7: the CAPI token's Secret and the workload cluster's Application on the seed, the
// MachineDeployments of nodes no longer in a group deleted (the Application is stateful, so ArgoCD prunes nothing),
// then the wait for the control plane, the admin kubeconfig and every Machine Running. Cluster API v1.14 no longer
// sets a Machine Failed and CAPMOX has no failure fields: what went wrong is on the ProxmoxMachine's condition, which
// the items and the waiting message carry, and which fails the stage when CAPMOX gave the machine up.
func (x *acq) cluster(ctx context.Context) error {
	if err := x.needCAPI(); err != nil {
		return err
	}
	templates, err := x.nodeTemplates(ctx)
	if err != nil {
		return err
	}
	seed, err := x.seed()
	if err != nil {
		return err
	}
	in, c := x.renderInput(), x.in.Spec.Clusters[0]
	apps, err := x.apps(ClusterSeed)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(apps, func(a appOf) bool { return a.c.Name == ClusterName })
	if i < 0 {
		return permanent(CodeComponentUnhealthy, "The BOM has no seed component %s.", ClusterName)
	}
	// The status names the cluster before anything builds it, so a Remove after a failed run knows to delete it.
	want := ClusterStatus{Name: ClusterName, Namespace: ClusterName, Endpoint: c.Endpoint, KubernetesVersion: x.bom.Kubernetes.Version,
		KubeconfigSecret: kubeconfigSecret, ManagedBy: "application", Templates: in.Templates}
	if cur := x.in.Status.Clusters; len(cur) == 1 {
		want.ArgoCDTokenExpiresAt = cur[0].ArgoCDTokenExpiresAt
	}
	if !reflect.DeepEqual(x.in.Status.Clusters, []ClusterStatus{want}) {
		if err := x.save(ctx, func(_ *Run, in *Installation, _ *Secrets) { in.Status.Clusters = []ClusterStatus{want} }); err != nil {
			return err
		}
	} else if err := x.check(ctx); err != nil {
		return err
	}
	if err := seed.EnsureNamespace(ctx, ClusterName, map[string]string{"app.kubernetes.io/managed-by": ManagedBy}, nil); err != nil {
		return classify(CodeSeedAPIUnreachable, err)
	}
	if err := seed.ApplyCore(ctx, "secrets", PVECredentialsSecret(x.bom, in, PVEToken{ID: x.sec.CAPI.TokenID, Secret: x.sec.CAPI.Secret})); err != nil {
		return classify(CodeSeedAPIUnreachable, err)
	}
	if err := seed.ApplyApplication(ctx, apps[i].obj); err != nil {
		return classify(CodeComponentUnhealthy, err)
	}
	// The stage reads the cluster's own objects, not this Application; a sync of it that failed is started again, and
	// what it reports goes into the waiting message: without a sync there is no machine to report anything.
	// The Application is Progressing for as long as the machines are built, so its health alone is no news: only a
	// sync that is not done is. It goes last, after what CAPMOX and Cluster API say: the message is clipped.
	note, appNote := "", ""
	if live, err := seed.GetApplication(ctx, argoNamespace, apps[i].name()); err != nil {
		return classify(CodeComponentUnhealthy, err)
	} else if state, detail, ok := appState(live); !ok {
		if _, err := x.resync(ctx, seed, apps[i], live, false, CodeComponentUnhealthy, detail); err != nil {
			return err
		}
		if phase := str(live, "status", "operationState", "phase"); str(live, "status", "sync", "status") != "Synced" || (phase != "" && phase != "Succeeded") {
			appNote = strings.TrimSuffix(fmt.Sprintf(" Application %s is %s: %s", apps[i].name(), state, detail), ": ")
		}
	}
	mds, err := seed.ListMachineDeployments(ctx, ClusterName, clusterSelector+",proxcloud.io/node-group")
	if err != nil && !kamajik8s.NotFound(err) {
		return classify(CodeSeedAPIUnreachable, err)
	}
	rendered := machineDeployments(c)
	for _, md := range mds {
		name := str(md, "metadata", "name")
		if slices.Contains(rendered, name) {
			continue
		}
		if err := seed.DeleteCR(ctx, capiGroup, capiVersion, "machinedeployments", ClusterName, name); err != nil {
			return classify(CodeSeedAPIUnreachable, err)
		}
		if err := x.recordReceipt(ctx, func(r *StageReceipt, _ *Installation, _ *Secrets) {
			if !slices.Contains(r.MachineDeploymentsDeleted, name) {
				r.MachineDeploymentsDeleted = append(r.MachineDeploymentsDeleted, name)
			}
		}); err != nil {
			return err
		}
	}
	machines, vmids, infra, err := x.machines(ctx, seed)
	if err != nil {
		return err
	}
	// CAPMOX reports a refused token on its own objects; no Machine gets a failure message for it.
	pc, err := seed.GetCR(ctx, infraGroup, infraVersion, "proxmoxclusters", ClusterName, ClusterName)
	if err != nil && !kamajik8s.NotFound(err) {
		return classify(CodeSeedAPIUnreachable, err)
	}
	for _, obj := range append(infra, pc) {
		conds, _ := dig(obj, "status", "conditions").([]any)
		for _, c := range conds {
			if m, _ := c.(map[string]any); str(m, "status") != "True" && pveAuthFailure(str(m, "message")) {
				return permanent(CodeCAPMOXAuthFailed, "Proxmox refuses the token of Cluster API (%s): %s", str(obj, "metadata", "name"), clip(str(m, "message")))
			}
		}
	}
	// CAPMOX reports on the ProxmoxCluster only once it has opened the Proxmox API from the seed.
	if c := cond(pc, "ProxmoxAvailable"); pc != nil && c == nil {
		start := *x.run.Stages[x.si].StartedAt
		since := time.Since(start) // the object's age when that is less: ArgoCD may have made it late
		if made, err := time.Parse(time.RFC3339, str(pc, "metadata", "creationTimestamp")); err == nil {
			since = min(since, time.Since(made))
			// Left silent by an earlier run: CAPMOX is deep in its backoff and would not see a fix in time (wokenAnnotation).
			if made.Before(start) && x.receipt().CAPMOXWokenAt == nil {
				if err := x.check(ctx); err != nil {
					return err
				}
				patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{wokenAnnotation: x.runID}}}
				if _, err := seed.MergePatchCR(ctx, infraGroup, infraVersion, "proxmoxclusters", ClusterName, ClusterName, patch); err != nil {
					return classify(CodeSeedAPIUnreachable, err)
				}
				now := time.Now().UTC()
				if err := x.recordReceipt(ctx, func(r *StageReceipt, _ *Installation, _ *Secrets) { r.CAPMOXWokenAt = &now }); err != nil {
					return err
				}
			}
		}
		if at := x.receipt().CAPMOXWokenAt; at != nil {
			since = min(since, time.Since(*at))
		}
		if since > capmoxSilence {
			return permanent(CodeCAPMOXAuthFailed, "CAPMOX has set no condition on ProxmoxCluster %s in %d minutes. It reports only after it opened the Proxmox API "+
				"at %s from the platform network with the token of Cluster API: check that the seed reaches that URL (route, the nodes' firewall) and the token. "+
				"Its log says what stops it: %s", ClusterName, int(since.Minutes()), in.PVEAPIURL, capmoxLog)
		}
		note += fmt.Sprintf(" CAPMOX has set no condition on the ProxmoxCluster yet: it opens %s from the seed first (%s).", in.PVEAPIURL, capmoxLog)
	} else if str(c, "status") == "False" {
		note += fmt.Sprintf(" ProxmoxCluster: %s: %s", str(c, "reason"), str(c, "message"))
	}
	conds := map[string]map[string]any{} // a ProxmoxMachine's name → its VirtualMachineProvisioned condition
	for _, pm := range infra {
		conds[str(pm, "metadata", "name")] = cond(pm, vmProvisioned)
	}
	items := templates
	var failed error
	running, machineNote := 0, ""
	for _, m := range machines {
		name, phase := str(m, "metadata", "name"), str(m, "status", "phase")
		msg := str(m, "status", "failureMessage")
		if msg == "" {
			msg = str(m, "status", "deprecated", "v1beta1", "failureMessage")
		}
		c := conds[str(m, "spec", "infrastructureRef", "name")]
		step := strings.TrimSuffix(str(c, "reason")+": "+str(c, "message"), ": ")
		if msg == "" && capmoxGaveUp(c) {
			msg = step
		}
		item := StageItem{ID: name, State: phase, Message: vmText(vmids[name])}
		if phase == "Running" {
			running++
		} else if msg == "" {
			text := "" // the step CAPMOX is at, with its error when it has one
			if str(c, "status") == "False" {
				text = step
			}
			// CAPMOX calls a VM provisioned once it runs and has its addresses (the chart sets skipCloudInitStatus): what
			// keeps it from joining after that (kubeadm, the API address, etcd) is on the Machine's Ready condition,
			// which sums up BootstrapConfigReady, InfrastructureReady and NodeHealthy.
			if r := cond(m, "Ready"); str(c, "message") == "" && str(r, "status") != "True" && str(r, "message") != "" {
				text = strings.TrimPrefix(text+"; Cluster API: "+str(r, "reason")+": "+oneLine(str(r, "message")), "; ")
			}
			if text != "" {
				item.Message = clip(item.Message + ": " + text)
				if text != str(c, "reason") { // a step alone is in the item only
					machineNote += fmt.Sprintf(" %s (%s): %s", name, vmText(vmids[name]), text)
				}
			}
		}
		if phase == "Failed" || msg != "" {
			item.State, item.Message = "Failed", clip(item.Message+": "+msg)
			code := CodeMachineFailed
			if pveAuthFailure(msg) {
				code = CodeCAPMOXAuthFailed
			}
			advice := fmt.Sprintf("correct the cause, delete the Machine on the seed (kubectl -n %s delete machine %s; Cluster API makes a new one), then Retry.", ClusterName, name)
			if strings.Contains(msg, "expected VM name to match") { // internal/service/vmservice/find.go at v0.9.2
				advice = fmt.Sprintf("VM %d is another VM, not this machine's. Do not delete the Machine as it is: CAPMOX then deletes the VM at that VMID. "+
					"Look in Proxmox whose VM %d is; ProxmoxMachine %s must no longer name it (spec.virtualMachineID) before its Machine is deleted.",
					vmids[name], vmids[name], str(m, "spec", "infrastructureRef", "name"))
			}
			if failed == nil {
				failed = permanent(code, "Machine %s (%s) failed: %s. CAPMOX does not try this machine again: %s", name, vmText(vmids[name]), clip(msg), advice)
			}
		}
		items = append(items, item)
	}
	if !slices.Equal(items, x.run.Stages[x.si].Items) {
		if err := x.saveStage(ctx, func(s *Stage) { s.Items = items }); err != nil {
			return err
		}
	}
	if failed != nil {
		return failed
	}
	cl, err := seed.GetCR(ctx, capiGroup, capiVersion, "clusters", ClusterName, ClusterName)
	if err != nil {
		return classify(CodeSeedAPIUnreachable, err)
	}
	initialized := dig(cl, "status", "initialization", "controlPlaneInitialized") == true || condition(cl, "ControlPlaneInitialized") == "True"
	kc, err := seed.GetSecretData(ctx, ClusterName, kubeconfigSecret)
	if err != nil {
		return classify(CodeSeedAPIUnreachable, err)
	}
	total := 1
	for _, g := range c.NodeGroups {
		total += g.Count
	}
	if !initialized || len(kc["value"]) == 0 || running != total || len(machines) != total {
		// Shown only: a read that fails leaves the control plane's own word out.
		kcps, _ := seed.ListCRs(ctx, kcpGroup, capiVersion, "kubeadmcontrolplanes", ClusterName, "")
		for _, k := range kcps {
			if a := cond(k, "Available"); a != nil && str(a, "status") != "True" {
				note += strings.TrimSuffix(fmt.Sprintf(" Control plane: %s: %s", str(a, "reason"), oneLine(str(a, "message"))), ": ")
			}
		}
		note += machineNote + appNote
		return waiting(CodeControlPlaneTimeout, "%d of %d machines are Running (%d exist); control plane initialized: %t.%s", running, total, len(machines), initialized, clip(note))
	}
	return nil
}

// nodeTemplates refuses a node template that CAPMOX cannot boot a machine from: one with a Proxmox cloud-init drive on
// another slot than ide0. CAPMOX v0.9.2 puts its NoCloud ISO on ide0 (internal/inject: CloudInitISODevice) and touches
// no other drive, so a clone of such a template starts with two media labelled cidata, and cloud-init's NoCloud source
// takes the first of the devices sorted downwards, the later drive: Proxmox's own, with no kubeadm data and no
// address. The machine then never joins and the stage would wait out its hour. A drive on ide0 is replaced by CAPMOX's.
//
// A template the portal token may not read (an operator's own, in nodeImage mode templates) is not a reason to stop:
// the stage goes on and lists it as an item, not checked. A template that passed is an item too (ok), so the stage
// shows that it looked; one that Proxmox no longer knows has none.
func (x *acq) nodeTemplates(ctx context.Context) (items []StageItem, err error) {
	templates := x.renderInput().Templates
	for _, node := range slices.Sorted(maps.Keys(templates)) {
		cfg, err := x.portal.GetVMConfig(ctx, node, templates[node])
		var ae *pve.APIError
		if pve.IsNotFound(err) {
			continue // CAPMOX reports a template that is gone on the machine
		} else if errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403) {
			items = append(items, StageItem{ID: fmt.Sprintf("template-%d", templates[node]), State: "not checked", Message: fmt.Sprintf("The portal token may not read "+
				"the config of node template %d on %s, so its cloud-init drive was not checked: a Proxmox cloud-init drive must be on ide0 or absent.", templates[node], node)})
			continue
		} else if err != nil {
			return nil, classify(CodeMachineFailed, err)
		}
		for _, slot := range slices.Sorted(maps.Keys(cfg)) {
			if s := cfg.Str(slot); slot != "ide0" && strings.Contains(s, "cloudinit") && strings.Contains(s, "media=cdrom") {
				return nil, permanent(CodeMachineFailed, "Node template %d on %s has a Proxmox cloud-init drive on %s. Cluster API puts its own on ide0, and a machine "+
					"with both boots from the wrong one and never joins. On %s run: qm set %d --delete %s (templates imported by this release have the drive "+
					"on ide0), then Retry.", templates[node], node, slot, node, templates[node], slot)
			}
		}
		items = append(items, StageItem{ID: fmt.Sprintf("template-%d", templates[node]), State: CheckOK,
			Message: fmt.Sprintf("Node template %d on %s has no Proxmox cloud-init drive on another slot than ide0.", templates[node], node)})
	}
	return items, nil
}

// pveAuthFailure tells a message that carries Proxmox's refusal of a token: "401 authentication failure" from the
// API, "not authorized to access endpoint" from the client CAPMOX uses. A bare 401 is not enough: VMIDs are numbers.
func pveAuthFailure(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "authentication failure") || strings.Contains(low, "not authorized") || strings.Contains(low, "401 unauthorized")
}

// vmText names a machine's VM: a Machine has no VMID until CAPMOX has cloned it.
func vmText(vmid int) string {
	if vmid == 0 {
		return "no VM"
	}
	return fmt.Sprintf("VM %d", vmid)
}

// machines lists the workload cluster's Machines on the seed with the VMID of each, read from its ProxmoxMachine, and
// the ProxmoxMachines.
func (x *acq) machines(ctx context.Context, seed *kamajik8s.Client) (machines []map[string]any, vmids map[string]int, infra []map[string]any, err error) {
	if machines, err = seed.ListCRs(ctx, capiGroup, capiVersion, "machines", ClusterName, clusterSelector); err != nil {
		return nil, nil, nil, classify(CodeSeedAPIUnreachable, err)
	}
	if infra, err = seed.ListCRs(ctx, infraGroup, infraVersion, "proxmoxmachines", ClusterName, ""); err != nil {
		return nil, nil, nil, classify(CodeSeedAPIUnreachable, err)
	}
	vmids = map[string]int{}
	for _, m := range machines {
		for _, pm := range infra {
			if str(pm, "metadata", "name") == str(m, "spec", "infrastructureRef", "name") {
				id, _ := dig(pm, "spec", "virtualMachineID").(float64)
				vmids[str(m, "metadata", "name")] = int(id)
			}
		}
	}
	return machines, vmids, infra, nil
}

// workloadAccess is stage 8: the seed ArgoCD's service account on the workload, cluster-admin and without a token
// Secret; a TokenRequest token for it, written only to the seed's cluster Secret and renewed only when under 90 days
// are left or the account was recreated; and the CSI token's Secret for proxmox-csi.
func (x *acq) workloadAccess(ctx context.Context) error {
	if x.sec.CSI == nil {
		return permanent(CodeCredentialsMissing, "The token of the workload's proxmox-csi is not stored: paste it (PUT …/credentials), then Retry.")
	}
	seed, wl, err := x.clusters(ctx)
	if err != nil {
		return err
	}
	in := x.renderInput()
	if err := x.check(ctx); err != nil {
		return err
	}
	if err := wl.ApplyCore(ctx, "serviceaccounts", object("v1", "ServiceAccount", "kube-system", argoManager)); err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	if err := wl.ApplyClusterScoped(ctx, rbacGroup, "v1", "clusterrolebindings", with(object(rbacGroup+"/v1", "ClusterRoleBinding", "", argoManager), map[string]any{
		"roleRef":  map[string]any{"apiGroup": rbacGroup, "kind": "ClusterRole", "name": "cluster-admin"},
		"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": argoManager, "namespace": "kube-system"}},
	})); err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	sa, err := wl.GetCore(ctx, "serviceaccounts", "kube-system", argoManager)
	if err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	uid := str(sa, "metadata", "uid")
	held, err := seed.GetCore(ctx, "secrets", argoNamespace, argoClusterSecret)
	if err != nil {
		return classify(CodeSeedAPIUnreachable, err)
	}
	expires, _ := time.Parse(time.RFC3339, str(held, "metadata", "annotations", tokenExpiryAnnotation))
	if held == nil || uid == "" || str(held, "metadata", "annotations", managerUIDAnnotation) != uid || time.Until(expires) < managerTokenRenew {
		ca, err := seed.GetSecretData(ctx, ClusterName, ClusterName+"-ca")
		if err != nil {
			return classify(CodeSeedAPIUnreachable, err)
		}
		if len(ca["tls.crt"]) == 0 || uid == "" {
			return waiting(CodeWorkloadUnreachable, "The seed holds no %s/%s-ca yet, or the workload has not created %s.", ClusterName, ClusterName, argoManager)
		}
		if err := x.check(ctx); err != nil {
			return err
		}
		var token string
		if token, expires, err = wl.CreateServiceAccountToken(ctx, "kube-system", argoManager, managerTokenTTL); err != nil {
			return classify(CodeWorkloadUnreachable, err)
		}
		x.hide(token)
		config, err := jsonText(map[string]any{"bearerToken": token,
			"tlsClientConfig": map[string]any{"insecure": false, "caData": base64.StdEncoding.EncodeToString(ca["tls.crt"])}})
		if err != nil {
			return permanent(CodeComponentUnhealthy, "The seed ArgoCD's cluster Secret could not be rendered.")
		}
		if err := seed.ApplySecret(ctx, argoNamespace, argoClusterSecret,
			map[string]string{"name": ClusterName, "server": WorkloadServer(in.Cluster.Endpoint), "config": config},
			map[string]string{"argocd.argoproj.io/secret-type": "cluster", "app.kubernetes.io/managed-by": ManagedBy},
			map[string]string{managerUIDAnnotation: uid, tokenExpiryAnnotation: expires.UTC().Format(time.RFC3339)}); err != nil {
			return classify(CodeSeedAPIUnreachable, err)
		}
	}
	if cur := x.in.Status.Clusters; len(cur) == 1 && (cur[0].ArgoCDTokenExpiresAt == nil || !cur[0].ArgoCDTokenExpiresAt.Equal(expires)) {
		if err := x.record(ctx, func(_ *Run, in *Installation, _ *Secrets) {
			if len(in.Status.Clusters) == 1 {
				e := expires.UTC()
				in.Status.Clusters[0].ArgoCDTokenExpiresAt = &e
			}
		}); err != nil {
			return err
		}
	}
	// As services-platform.sh: the namespace privileged for the node plugin, then the config the chart names.
	if err := wl.EnsureNamespace(ctx, csiNamespace, map[string]string{"pod-security.kubernetes.io/enforce": "privileged"}, nil); err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	csi, err := CSIConfigSecret(x.bom, in, PVEToken{ID: x.sec.CSI.TokenID, Secret: x.sec.CSI.Secret})
	if err != nil {
		return permanent(CodeComponentUnhealthy, "The CSI configuration could not be rendered.")
	}
	return classify(CodeWorkloadUnreachable, wl.ApplyCore(ctx, "secrets", csi))
}

// workloadPlatform is stage 9: the workload's components wave by wave. Once the CNI's wave is in, every node must be
// Ready with its group label; once the last stateful component's is, the platform's own volumes are recorded.
func (x *acq) workloadPlatform(ctx context.Context) error {
	seed, wl, err := x.clusters(ctx)
	if err != nil {
		return err
	}
	apps, err := x.apps(ClusterWorkload)
	if err != nil {
		return err
	}
	stateful, first := -1, apps[0].c.Wave // the CNI's wave is the lowest, wherever the BOM lists it
	for _, a := range apps {
		first = min(first, a.c.Wave)
		if a.c.Stateful {
			stateful = max(stateful, a.c.Wave)
		}
	}
	// Also at every look, not only once the stateful wave is Healthy: a run that fails inside that wave has made the
	// volume already, and a Remove that cannot ask the cluster deletes only what is recorded.
	if vols, err := platformVolumes(ctx, x.bom, wl); err != nil {
		return classify(CodeWorkloadUnreachable, err)
	} else if len(vols) > 0 && !slices.Equal(vols, x.in.Status.Volumes) {
		if err := x.record(ctx, func(_ *Run, in *Installation, _ *Secrets) { in.Status.Volumes = vols }); err != nil {
			return err
		}
	}
	return x.waves(ctx, seed, wl, apps, waveStall, func(wave int) error {
		if wave == first {
			if err := x.nodes(ctx, wl); err != nil {
				return err
			}
		}
		if wave != stateful {
			return nil
		}
		vols, err := platformVolumes(ctx, x.bom, wl)
		if err != nil {
			return classify(CodeWorkloadUnreachable, err)
		}
		if slices.Equal(vols, x.in.Status.Volumes) {
			return nil
		}
		return x.record(ctx, func(_ *Run, in *Installation, _ *Secrets) { in.Status.Volumes = vols })
	})
}

// nodes waits until every node of the workload is Ready and each group has its count of nodes carrying its label.
func (x *acq) nodes(ctx context.Context, wl *kamajik8s.Client) error {
	nodes, err := wl.ListNodes(ctx)
	if err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	ready, total := map[string]int{}, 1
	for _, n := range nodes {
		if condition(n, "Ready") == "True" {
			ready[str(n, "metadata", "labels", GroupLabel)]++
		} else {
			ready["not ready"]++
		}
	}
	for _, g := range x.in.Spec.Clusters[0].NodeGroups {
		total += g.Count
		if ready[g.Name] != g.Count {
			return waiting(CodeNodesTimeout, "%d of %d nodes of group %s are Ready with the label %s.", ready[g.Name], g.Count, g.Name, GroupLabel)
		}
	}
	if ready["not ready"] > 0 || len(nodes) != total {
		return waiting(CodeNodesTimeout, "%d nodes exist, %d of them not Ready; the cluster has %d.", len(nodes), ready["not ready"], total)
	}
	return nil
}

// platformVolumes are the proxmox-csi volumes bound to claims in the namespaces of the workload's own components
// (tenant claims live in tenant namespaces), each with the component that owns its namespace, the stateful one first.
func platformVolumes(ctx context.Context, b *BOM, wl *kamajik8s.Client) ([]Volume, error) {
	pvs, err := wl.ListPersistentVolumes(ctx)
	if err != nil {
		return nil, err
	}
	owner := map[string]string{}
	for _, c := range b.Components {
		if _, has := owner[c.Namespace]; c.Cluster == ClusterWorkload && (!has || c.Stateful) {
			owner[c.Namespace] = c.Name
		}
	}
	var out []Volume
	for _, pv := range pvs {
		ns := str(pv, "spec", "claimRef", "namespace")
		if handle := str(pv, "spec", "csi", "volumeHandle"); owner[ns] != "" && handle != "" && str(pv, "spec", "csi", "driver") == csiDriver {
			out = append(out, Volume{Handle: handle, Component: owner[ns], PVC: ns + "/" + str(pv, "spec", "claimRef", "name")})
		}
	}
	slices.SortFunc(out, func(a, b Volume) int { return strings.Compare(a.Handle, b.Handle) })
	return out, nil
}

// register is stage 10: the two providers, created or adopted with the workload's proxcloud service account as their
// kubeconfig (the token goes to the provider rows' encrypted secret only).
func (x *acq) register(ctx context.Context) error {
	_, wl, err := x.clusters(ctx)
	if err != nil {
		return err
	}
	data, err := wl.GetSecretData(ctx, providerNamespace, providerTokenSecret)
	if err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	if len(data["token"]) == 0 || len(data["ca.crt"]) == 0 {
		return waiting(CodeWorkloadUnreachable, "The workload has not filled %s/%s with a token yet.", providerNamespace, providerTokenSecret)
	}
	x.hide(string(data["token"]))
	in := x.renderInput()
	kubeconfig, err := kamajik8s.BuildKubeconfig(ClusterName, WorkloadServer(in.Cluster.Endpoint), data["ca.crt"], string(data["token"]))
	if err != nil {
		return permanent(CodeWorkloadUnreachable, "The workload's CA is not usable for the providers' kubeconfig.")
	}
	if err := x.check(ctx); err != nil {
		return err
	}
	var ids ProviderIDs
	k8s, db := ProviderBodies(x.bom, in, kubeconfig)
	for _, p := range []struct {
		body map[string]any
		id   *string
	}{{k8s, &ids.Kubernetes}, {db, &ids.Database}} {
		id, err := x.r.Register(ctx, p.body)
		if errors.Is(err, ErrProviderConflict) {
			return permanent(CodeProviderConflict, "%s", clip(err.Error()))
		} else if err != nil {
			return err
		}
		*p.id = id
	}
	return x.record(ctx, func(_ *Run, in *Installation, _ *Secrets) { in.Status.Providers = &ids })
}

// done is stage 11: what was applied, and the BOM it came from.
func (x *acq) done(ctx context.Context) error {
	return x.save(ctx, func(run *Run, in *Installation, _ *Secrets) {
		now := time.Now().UTC()
		in.Status.Components = nil
		for _, c := range x.bom.Components {
			in.Status.Components = append(in.Status.Components, ComponentStatus{Name: c.Name, Cluster: c.Cluster, Version: c.Chart.Version,
				Digest: c.Chart.Digest, AppliedAt: &now, Managed: true})
		}
		ref := run.BOM
		in.Status.BOM = &ref
	})
}

// ErrProviderConflict is what a registration answers when a provider row that is not this installation's is in the way.
var ErrProviderConflict = errors.New(CodeProviderConflict)

// ProviderMarker is the config key that makes a provider row an installation's.
const ProviderMarker = "platformInstallationId"

// providerStatusBefore is the config key a Remove leaves on a provider row it disables: the status the row had. Stage
// 10 gives a row that carries it that status back, so an install after a Remove does not end ready with both
// providers disabled; a row the operator disabled has no such key and stays as it is.
const providerStatusBefore = "platformStatusBeforeRemove"

// DisableProvider sets a provider row DISABLED for a Remove and notes the status it had; false = it was disabled already.
func DisableProvider(row map[string]any) bool {
	cfg, _ := row["config"].(map[string]any)
	was := str(row, "status")
	if cfg == nil || was == "DISABLED" {
		return false
	}
	cfg[providerStatusBefore], row["status"] = was, "DISABLED"
	return true
}

// providerOwned are the config keys stage 10 rewrites on a row it adopts. Everything else is written when the row is
// created and is the operator's from then on: its name, its status, the node image versions, the attach policy.
var providerOwned = [][]string{{"argocd"}, {"namespacePlacement"}, {"cluster", "pveApiUrl"},
	{"database", "egressFence"}, {"database", "lbAddressPool"}, {"database", "storageClasses"},
	{"database", "engines", "postgresql", "replicas"}} // [1] again when the dbaas group drops below three PVE nodes

// AdoptProvider finds among the stored provider rows the one a stage 10 body adopts, the row of the body's
// config.provider that carries its marker, and rewrites the owned keys on it. nil = there is none: create it. Two rows
// with the marker, or a row without it under the body's name, are ErrProviderConflict. A row a Remove disabled gets
// the status it had before. The caller stores the row and the body's secret.
func AdoptProvider(rows []map[string]any, body map[string]any) (map[string]any, error) {
	var found map[string]any
	for _, row := range rows {
		switch {
		case str(row, "config", "provider") == str(body, "config", "provider") && str(row, "config", ProviderMarker) == str(body, "config", ProviderMarker):
			if found != nil {
				return nil, fmt.Errorf("%w: two %s providers carry the marker of installation %s", ErrProviderConflict,
					str(body, "config", "provider"), str(body, "config", ProviderMarker))
			}
			found = row
		case str(row, "name") == str(body, "name"):
			return nil, fmt.Errorf("%w: a provider named %q exists and is not this installation's: rename it or the installation",
				ErrProviderConflict, str(body, "name"))
		}
	}
	if found == nil {
		return nil, nil
	}
	if was := str(found, "config", providerStatusBefore); was != "" { // disabled by a Remove, not by the operator
		found["status"] = was
		delete(found["config"].(map[string]any), providerStatusBefore)
	}
	for _, path := range providerOwned {
		v := dig(body, append([]string{"config"}, path...)...)
		if v == nil {
			continue
		}
		at := found
		for _, k := range append([]string{"config"}, path[:len(path)-1]...) {
			next, ok := at[k].(map[string]any)
			if !ok {
				next = map[string]any{}
				at[k] = next
			}
			at = next
		}
		at[path[len(path)-1]] = v
	}
	return found, nil
}
