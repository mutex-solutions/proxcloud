package platformlife

// cluster.go — stages 6 to 11 of an apply (design §4.2): the seed's ArgoCD and Cluster API, the workload cluster, the
// seed ArgoCD's access to it, the workload's components wave by wave, the two providers. Each observes first: every
// write is a server-side apply, and what a stage waits for is read again at every acquisition. The workload's admin
// kubeconfig, the argocd-manager token and the providers' token are held in memory only.

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
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
	in := RenderInput{ArgoCD: sp.Access.ArgoCD, ID: x.in.ID, Name: x.in.Name, Region: region, RegionName: str(x.es.Config, "regions", region, "displayName"),
		PVEAPIURL: sp.PVEAPIURL, BootStorage: sp.Storage.Boot, SeedAddress: sp.Seed.Address, SSHKeys: sp.Seed.SSHKeys, Templates: templates, WorkerMTU: mtu,
		Network: RenderNetwork{Bridge: sp.Network.Bridge, VLAN: sp.Network.VLAN, Gateway: sp.Network.Gateway, Prefix: prefix.Bits(), DNS: sp.Network.DNS, MTU: sp.Network.MTU},
		Cluster: RenderCluster{Endpoint: c.Endpoint, NodeRange: []string{c.NodeRange}, MetalLBRange: c.MetalLBRange,
			ControlPlane:         RenderMachine{Node: c.ControlPlane.Node, Cores: c.ControlPlane.Cores, MemoryMiB: c.ControlPlane.MemoryMiB, DiskGiB: c.ControlPlane.DiskGiB},
			ControlPlaneReplicas: c.ControlPlane.Count()}}
	for _, g := range c.NodeGroups {
		in.Cluster.NodeGroups = append(in.Cluster.NodeGroups, RenderNodeGroup{Name: g.Name, Nodes: g.Nodes, Count: g.Count, Cores: g.Cores, MemoryMiB: g.MemoryMiB, DiskGiB: g.DiskGiB})
	}
	// A move that replaces machines renders from the recorded step, in every stage: 0 until stage 7 begins the roll
	// (everything as it runs), and never lower than a step a run has reached.
	if x.rolls {
		r := &Roll{FromVersion: x.from.Kubernetes.Version, FromKubeVip: x.from.Images.KubeVip}
		if len(x.in.Status.Clusters) > 0 {
			r.FromTemplates = x.in.Status.Clusters[0].Templates
		}
		if st := rollOf(x.in); st != nil {
			r.Step, r.DeleteFirst = st.Step, st.DeleteFirst
		}
		in.Cluster.Roll = r
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

// lastSyncIsCurrent: the Application's last sync operation succeeded and was of the source the Application has now
// (ArgoCD records the source of a sync in its result). It is what an upgrade run takes for "the new chart is in" where
// Synced cannot be asked for: the platform cluster's Application never prunes, so the machine templates of an earlier
// size stay and keep it OutOfSync.
func lastSyncIsCurrent(app map[string]any) bool {
	source := dig(app, "spec", "source")
	return source != nil && app["operation"] == nil && str(app, "status", "operationState", "phase") == "Succeeded" &&
		reflect.DeepEqual(source, dig(app, "status", "operationState", "syncResult", "source"))
}

// onlyPruneLeft: ArgoCD has compared the cluster with the source the Application has now, and all that is out of sync
// is what it would prune. Automated sync without prune starts no sync for that alone (ArgoCD's controller skips it:
// "need to prune extra resources only but automated prune is disabled"), so a sync result of the new source never
// comes, and none is needed: every object the chart renders is on the cluster as rendered. Read from ArgoCD's
// controller source, not seen on a lab.
func onlyPruneLeft(app map[string]any) bool {
	source := dig(app, "spec", "source")
	resources, _ := dig(app, "status", "resources").([]any)
	if source == nil || len(resources) == 0 || app["operation"] != nil || !reflect.DeepEqual(source, dig(app, "status", "sync", "comparedTo", "source")) {
		return false
	}
	for _, r := range resources {
		if m, _ := r.(map[string]any); str(m, "status") != "Synced" && m["requiresPruning"] != true {
			return false
		}
	}
	return true
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
	if n, _ := fmt.Sscanf(x.run.Stages[x.si].Progress, "step %d", &seen); n == 0 {
		_, _ = fmt.Sscanf(x.run.Stages[x.si].Progress, "wave %d", &seen) // a run stored before the BOM's wave was named
	}
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
					all := append(deletedNodes(x.run.Stages[x.si].Items), items...)
					if serr := x.saveStage(ctx, func(s *Stage) { s.Items = all }); serr != nil {
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
		// The step counts the waves present; the wave is the BOM's number, the one the plan and the guide name.
		progress := fmt.Sprintf("step %d of %d, wave %d", i+1, len(order), w)
		// Written where the run stands: a new wave, the stored one, or an earlier one that is pending again.
		// The Nodes the run deleted (nodes, called between two waves) stay on the stage whatever is written after.
		all := append(deletedNodes(x.run.Stages[x.si].Items), items...)
		if reached := i+1 > seen; reached || ((len(pending) > 0 || i+1 == seen) && !slices.Equal(x.run.Stages[x.si].Items, all)) {
			if err := x.saveStage(ctx, func(s *Stage) {
				if reached {
					if s.Progress = progress; stall > 0 {
						now := time.Now().UTC()
						d := now.Add(stall)
						s.LastProgressAt, s.Deadline = &now, &d
					}
				}
				s.Items = all
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
		var passed []Component
		for _, a := range apps {
			if a.c.Wave == w {
				passed = append(passed, a.c)
			}
		}
		if err := x.moved(ctx, passed...); err != nil {
			return err
		}
	}
	return nil
}

// moved records, on an upgrade run, components that passed their gate at the target BOM's version. status.bom names
// the BOM the platform left until the whole run has succeeded (stage 11), so between the two the components say what
// runs. Nothing on a run that stays on its BOM: stage 11 writes its record.
func (x *acq) moved(ctx context.Context, comps ...Component) error {
	if x.from == nil {
		return nil
	}
	at := func(in *Installation, c Component) int {
		return slices.IndexFunc(in.Status.Components, func(s ComponentStatus) bool { return s.Cluster == c.Cluster && s.Name == c.Name })
	}
	comps = slices.DeleteFunc(slices.Clone(comps), func(c Component) bool {
		i := at(x.in, c)
		return i >= 0 && x.in.Status.Components[i].Version == c.Chart.Version && x.in.Status.Components[i].Digest == c.Chart.Digest
	})
	if len(comps) == 0 {
		return nil
	}
	return x.record(ctx, func(_ *Run, in *Installation, _ *Secrets) {
		now := time.Now().UTC()
		for _, c := range comps {
			s := ComponentStatus{Name: c.Name, Cluster: c.Cluster, Version: c.Chart.Version, Digest: c.Chart.Digest, AppliedAt: &now, Managed: true}
			if i := at(in, c); i >= 0 {
				in.Status.Components[i] = s
			} else {
				in.Status.Components = append(in.Status.Components, s)
			}
		}
	})
}

// sameJSON compares a rendered object with one read from a cluster, whose numbers are all float64.
func sameJSON(a, b any) bool {
	x, errA := jsonText(a)
	y, errB := jsonText(b)
	return errA == nil && errB == nil && x == y
}

// rolloutGate runs on an upgrade run before stage 7 lets the cluster chart of the target BOM sync. A chart whose
// rendered machine or bootstrap template differs gives the template a new name (the name carries a hash of its spec),
// and Cluster API then replaces every machine that used the old one; a changed KubeadmControlPlane replaces the
// control plane machine. The Application syncs by itself, so a look after the sync is too late. The gate therefore
// applies the Application with the new source and without automated sync, waits until ArgoCD has compared the cluster
// with that source, and reads the comparison: a ProxmoxMachineTemplate or KubeadmConfigTemplate that is not Synced
// (and is not one ArgoCD would only prune: an old template no longer rendered), or a KubeadmControlPlane that is not
// Synced, fails the run with unexpected_rollout, with nothing synced. Otherwise the caller applies the Application as
// it always is, automated, and the sync runs.
//
// A release must not ship such a chart to an installed platform: CI renders both sides and compares them
// (TestUpgradeRollsNoMachine), and the classifier refuses a version the check did not pass. This is the same check on
// the real cluster, which also sees an installation whose machine sizes were edited together with the upgrade.
// Not verified on a lab: Runner.SkipRolloutGate turns it off.
//
// On a roll (step above 0) the same read lets through what the step is there to change and nothing else: at step 1
// the KubeadmControlPlane and the control plane's machine template, at a later step the templates of that step's
// MachineDeployment (unit). A template of another unit that is new, or a control plane that would change after its
// step, is a machine size, node or group edited together with the upgrade: unexpected_rollout, with the step not synced.
func (x *acq) rolloutGate(ctx context.Context, seed *kamajik8s.Client, a appOf, step int, unit rollUnit) error {
	if rc := x.receipt(); x.from == nil || x.r.SkipRolloutGate || (step == 0 && rc.RolloutChecked) || (step > 0 && rc.RollChecked >= step) {
		return nil
	}
	own := func(kind, name string) bool { // what this step replaces
		base := name[:max(strings.LastIndex(name, "-"), 0)] // a template's name ends in a hash of its spec
		switch {
		case step == 1:
			return kind == "KubeadmControlPlane" || (kind == "ProxmoxMachineTemplate" && base == ClusterName+"-cp-"+unit.Node)
		case step > 1:
			return kind != "KubeadmControlPlane" && base == unit.Owner
		}
		return false
	}
	live, err := seed.GetApplication(ctx, argoNamespace, a.name())
	if err != nil {
		return classify(CodeComponentUnhealthy, err)
	}
	// No Application: no cluster to roll. The target's source with automated sync: an earlier run passed the gate, or
	// the two BOMs render the same source.
	if live == nil || (sameJSON(dig(live, "spec", "source"), dig(a.obj, "spec", "source")) && dig(live, "spec", "syncPolicy", "automated") != nil) {
		return nil
	}
	held, err := gateRender(a, live)
	if err != nil {
		return permanent(CodeComponentUnhealthy, "The Application of the platform cluster could not be rendered for the comparison: %s", clip(err.Error()))
	}
	if err := x.check(ctx); err != nil {
		return err
	}
	if err := seed.ApplyApplication(ctx, held); err != nil {
		return classify(CodeComponentUnhealthy, err)
	}
	if live, err = seed.GetApplication(ctx, argoNamespace, a.name()); err != nil {
		return classify(CodeComponentUnhealthy, err)
	}
	resources, _ := dig(live, "status", "resources").([]any)
	var rolls []string
	controlPlane := false
	for _, r := range resources {
		m, _ := r.(map[string]any)
		kind, synced := str(m, "kind"), str(m, "status") == "Synced"
		switch {
		case kind == "KubeadmControlPlane":
			if controlPlane = true; !synced && !own(kind, str(m, "name")) {
				rolls = append(rolls, "KubeadmControlPlane "+str(m, "name")+" would change")
			}
		case (kind == "ProxmoxMachineTemplate" || kind == "KubeadmConfigTemplate") && !synced && m["requiresPruning"] != true && !own(kind, str(m, "name")):
			rolls = append(rolls, kind+" "+str(m, "name")+" is new or would change")
		}
	}
	// The comparison must be of the source the Application has now, and list the control plane: an empty or an old
	// list says nothing.
	if source := dig(live, "spec", "source"); source == nil || !reflect.DeepEqual(source, dig(live, "status", "sync", "comparedTo", "source")) || !controlPlane {
		_, detail, _ := appState(live)
		return waiting(CodeComponentUnhealthy, "Waiting for ArgoCD to compare the platform cluster with chart %s %s before anything of it is synced. %s",
			a.c.Chart.Name, a.c.Chart.Version, clip(detail))
	}
	if len(rolls) > 0 && step > 0 {
		slices.Sort(rolls)
		return permanent(CodeUnexpectedRollout, "Step %d of the roll replaces %s, and its render would also replace other machines of the platform cluster: %s. Nothing of "+
			"this step was synced and every machine runs as before; Application %s is left without automated sync. If the installation's machine sizes, nodes or "+
			"node groups were changed together with the upgrade, put them back and Retry: a roll changes the Kubernetes version and the node image, nothing else.",
			step, unit.Name, clip(strings.Join(rolls, "; ")), a.name())
	}
	if len(rolls) > 0 {
		slices.Sort(rolls)
		return permanent(CodeUnexpectedRollout, "Chart %s %s would replace machines of the platform cluster: %s. Nothing of it was synced and the cluster runs as before; "+
			"Application %s is left without automated sync. An upgrade replaces no machine in this release. If the installation's machine sizes, nodes or node groups "+
			"were changed together with the upgrade, put them back and Retry. Otherwise the release is at fault: install the ProxCloud release that corrects it and "+
			"start the upgrade to its BOM.", a.c.Chart.Name, a.c.Chart.Version, clip(strings.Join(rolls, "; ")), a.name())
	}
	return x.recordReceipt(ctx, func(r *StageReceipt, _ *Installation, _ *Secrets) {
		if step > 0 {
			r.RollChecked = step
		} else {
			r.RolloutChecked = true
		}
	})
}

// gateRender is the Application rolloutGate lets ArgoCD compare before anything syncs: a's, without automated sync, and
// with the cluster chart's nodeDeletionTimeoutSeconds (chart 0.1.16 and later) as the live Application has it: when
// live is of a chart before 0.1.16, or renders no timeout, the comparison renders none either. That value is on the
// KubeadmControlPlane and on every MachineDeployment, and Cluster API puts it on the Machines in place: no machine is
// replaced for it (Cluster API v1.14.4: the control plane's syncMachines and matchesMachineSpec, the MachineSet's
// syncMachines, mdutil.MachineTemplateDeepCopyRolloutFields; the chart's templates cite the lines). ArgoCD's
// comparison says only Synced or OutOfSync per object, so with the value new in the render, a control plane that
// would be replaced could not be told from one that only gets the timeout. Rendered as live has it, the comparison is
// the strict one it always was, and what the caller applies next differs from what was compared in that one value:
// TestNodeDeletionTimeoutChangesNoMachine holds the chart to it.
func gateRender(a appOf, live map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(a.obj)
	if err != nil {
		return nil, err
	}
	var held map[string]any
	if err := json.Unmarshal(raw, &held); err != nil {
		return nil, err
	}
	policy, _ := dig(held, "spec", "syncPolicy").(map[string]any)
	delete(policy, "automated")
	liveSeconds, set := dig(live, "spec", "source", "helm", "valuesObject", "nodeDeletionTimeoutSeconds").(float64)
	liveHas := kamaji.ChartHasNodeDeletionTimeout(str(live, "spec", "source", "targetRevision")) && (!set || liveSeconds >= 0)
	if kamaji.ChartHasNodeDeletionTimeout(a.c.Chart.Version) && !liveHas {
		helm, _ := dig(held, "spec", "source", "helm").(map[string]any)
		if helm == nil {
			return nil, errors.New("the Application has no helm source")
		}
		values, _ := helm["valuesObject"].(map[string]any)
		if values == nil {
			values = map[string]any{}
			helm["valuesObject"] = values
		}
		values["nodeDeletionTimeoutSeconds"] = kamaji.NodeDeletionTimeoutUnset
	}
	return held, nil
}

// tenantsDown is what the tenants run on and is not there, and how many things were read in all. The drill of
// 0.10.1-rc.2 showed what may not be asked alone: the health ArgoCD shows for a tenant's Application and the status of
// a TenantControlPlane are written by controllers, and a drain that evicts the tenant's pods evicts those controllers
// with them (Kamaji runs beside the control planes), so both went on saying Healthy and Ready for five minutes while
// the only tenant API server was away. Read as well, and written by the platform cluster's own controller manager
// and kubelets, which a unit of the roll does not take away with the tenants:
//   - every Deployment in the namespaces of the Kamaji and CloudNativePG operators (operators): an operator that is
//     not available has stopped writing the status the first two reads rely on;
//   - per TenantControlPlane the Deployment Kamaji runs it as (same namespace and name): its available replicas
//     against its replicas, with its change observed;
//   - per CloudNativePG Cluster its instance pods that are Ready, against spec.instances.
//
// ponytail: a Deployment's status is the controller manager's. One that stopped while every node still reads Ready
// would leave it stale; the caller counts the nodes first, and the pods' own conditions are the upgrade path.
func tenantsDown(ctx context.Context, wl *kamajik8s.Client, operators []string) (down []string, total int, err error) {
	apps, err := wl.ListApplications(ctx, argoNamespace, tenantSelector)
	if err != nil && !kamajik8s.NotFound(err) {
		return nil, 0, classify(CodeWorkloadUnreachable, err)
	}
	for _, a := range apps {
		if h := str(a, "status", "health", "status"); h != "Healthy" {
			down = append(down, "Application "+str(a, "metadata", "name")+" ("+h+")")
		}
	}
	list, err := wl.ListCRsAllNamespaces(ctx, "apps", "v1", "deployments", "")
	if err != nil {
		return nil, 0, classify(CodeWorkloadUnreachable, err)
	}
	deployments := map[string]map[string]any{}
	for _, d := range list {
		key := str(d, "metadata", "namespace") + "/" + str(d, "metadata", "name")
		deployments[key] = d
		if slices.Contains(operators, str(d, "metadata", "namespace")) {
			total++
			if why := unavailable(d); why != "" {
				down = append(down, "Operator "+key+" ("+why+")")
			}
		}
	}
	tcps, err := wl.ListCRsAllNamespaces(ctx, "kamaji.clastix.io", "v1alpha1", "tenantcontrolplanes", "")
	if err != nil && !kamajik8s.NotFound(err) {
		return nil, 0, classify(CodeWorkloadUnreachable, err)
	}
	for _, t := range tcps {
		key := str(t, "metadata", "namespace") + "/" + str(t, "metadata", "name")
		if s := str(t, "status", "kubernetesResources", "version", "status"); s != "Ready" {
			down = append(down, "TenantControlPlane "+key+" ("+s+")")
		}
		if why := unavailable(deployments[key]); why != "" {
			down = append(down, "Deployment "+key+" ("+why+")")
		}
	}
	dbs, err := wl.ListCRsAllNamespaces(ctx, "postgresql.cnpg.io", "v1", "clusters", "")
	if err != nil && !kamajik8s.NotFound(err) {
		return nil, 0, classify(CodeWorkloadUnreachable, err)
	}
	if len(dbs) > 0 {
		pods, err := wl.ListPodsAllNamespaces(ctx, "cnpg.io/cluster")
		if err != nil {
			return nil, 0, classify(CodeWorkloadUnreachable, err)
		}
		ready := map[string]int{}
		for _, p := range pods {
			labels, _ := dig(p, "metadata", "labels").(map[string]any)
			role, _ := labels["cnpg.io/podRole"].(string)
			// Instance pods only: not the initdb and join Jobs' pods, not a pooler's, not one that is being deleted.
			if labels["cnpg.io/jobRole"] != nil || (role != "" && role != "instance") || dig(p, "metadata", "deletionTimestamp") != nil || condition(p, "Ready") != "True" {
				continue
			}
			name, _ := labels["cnpg.io/cluster"].(string)
			ready[str(p, "metadata", "namespace")+"/"+name]++
		}
		for _, d := range dbs {
			key := str(d, "metadata", "namespace") + "/" + str(d, "metadata", "name")
			if want, _ := dig(d, "spec", "instances").(float64); ready[key] < int(want) {
				down = append(down, fmt.Sprintf("Database %s (%d of %d instances Ready)", key, ready[key], int(want)))
			}
		}
	}
	slices.Sort(down)
	return down, total + len(apps) + 2*len(tcps) + len(dbs), nil
}

// unavailable says why a Deployment does not serve ("" when it does): it is absent, the controller manager has not
// seen its last change, or fewer of its replicas are available than it asks for.
func unavailable(d map[string]any) string {
	if d == nil {
		return "no Deployment"
	}
	want := 1.0 // the API server's default
	if n, ok := dig(d, "spec", "replicas").(float64); ok {
		want = n
	}
	gen, _ := dig(d, "metadata", "generation").(float64)
	seen, _ := dig(d, "status", "observedGeneration").(float64)
	have, _ := dig(d, "status", "availableReplicas").(float64)
	switch {
	case seen < gen:
		return "its last change is not observed yet"
	case have < want:
		return fmt.Sprintf("%d of %d replicas available", int(have), int(want))
	}
	return ""
}

// downBefore reports whether d (an entry of tenantsDown) is of a tenant the note says was down before the upgrade.
// An Application and a TenantControlPlane are compared with their state, so one that was Progressing and is Degraded
// now holds the run. A Deployment and a Database carry counts, which move: they are compared by name, and also go
// with their tenant's own entry (the TenantControlPlane of the same name, the Application named as the database), so
// a note taken by a release that did not read them yet still covers the same tenants.
func downBefore(before []string, d string) bool {
	if slices.Contains(before, d) {
		return true
	}
	kind, rest, _ := strings.Cut(d, " ")
	name, _, _ := strings.Cut(rest, " (")
	noted := func(prefix string) bool {
		return slices.ContainsFunc(before, func(b string) bool { return strings.HasPrefix(b, prefix+" (") })
	}
	switch kind {
	case "Operator":
		return noted("Operator " + name)
	case "Deployment":
		return noted("Deployment "+name) || noted("TenantControlPlane "+name)
	case "Database":
		return noted("Database "+name) || noted("Application "+name[strings.LastIndex(name, "/")+1:])
	}
	return false
}

// tenantGate holds an upgrade run after a wave of the workload, and between two units of a roll, until everything the
// tenants run on that was there when the upgrade first reached the workload is there again (tenantsDown). What was
// down before is noted once, on the installation next to the target (status.tenantsDownBefore, cleared in stage 11),
// and does not hold the run: an upgrade must not wait for a customer's broken database. The note is the upgrade's, not
// the run's: a Retry after a wave broke a tenant must still wait for it, and would not if it read the tenants again.
// The wait ends with the stage's deadline, under tenants_unhealthy and with the names still down.
func (x *acq) tenantGate(ctx context.Context, wl *kamajik8s.Client, baseline bool) error {
	if x.from == nil {
		return nil
	}
	var operators []string
	for _, c := range x.bom.Components {
		if c.Cluster == ClusterWorkload && (c.Name == "kamaji" || c.Name == "cnpg") {
			operators = append(operators, c.Namespace)
		}
	}
	down, total, err := tenantsDown(ctx, wl, operators)
	if err != nil {
		return err
	}
	if baseline {
		if x.in.Status.TenantsDownBefore != nil {
			return nil
		}
		down = append([]string{}, down...) // never nil: an empty note is a note
		return x.record(ctx, func(_ *Run, in *Installation, _ *Secrets) { in.Status.TenantsDownBefore = &down })
	}
	var before []string
	if b := x.in.Status.TenantsDownBefore; b != nil {
		before = *b
	}
	down = slices.DeleteFunc(down, func(d string) bool { return downBefore(before, d) })
	if len(down) > 0 {
		return waiting(CodeTenantsUnhealthy, "Waiting for the tenants: %d of the %d things read that they run on are not there, and were before the upgrade: %s. "+
			"The run goes on by itself when they are back.", len(down), total, clip(strings.Join(down, ", ")))
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
	if n := c.ControlPlane.Count(); n > 1 {
		want.ControlPlaneReplicas = n // before the sync: a control plane half way to three is one of three
	}
	if cur := x.in.Status.Clusters; len(cur) == 1 {
		want.ArgoCDTokenExpiresAt = cur[0].ArgoCDTokenExpiresAt
		want.ControlPlaneReplicas = max(want.ControlPlaneReplicas, cur[0].ControlPlaneReplicas)
		if want.Roll = cur[0].Roll; x.rolls {
			// The record keeps the version and the templates the unreached units run until the last unit is done (roll).
			want.KubernetesVersion, want.Templates = cur[0].KubernetesVersion, cur[0].Templates
		}
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
	if x.rolls {
		return x.roll(ctx, seed, templates)
	}
	if err := x.rolloutGate(ctx, seed, apps[i], 0, rollUnit{}); err != nil {
		return err
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
	} else if state, detail, ok := appState(live); x.from != nil && !ok && !lastSyncIsCurrent(live) && !onlyPruneLeft(live) {
		// An upgrade run waits for the sync of the target's chart: the machines are Running all along, so nothing below
		// would wait for it, and the component would be recorded as moved while ArgoCD still syncs or has failed. A
		// chart that renders every object as it is has nothing to sync (onlyPruneLeft): there is no sync to wait for.
		if _, err := x.resync(ctx, seed, apps[i], live, false, CodeComponentUnhealthy, detail); err != nil {
			return err
		}
		return waiting(CodeComponentUnhealthy, "Waiting for ArgoCD to sync %s at chart %s %s (%s). %s", apps[i].name(), apps[i].c.Chart.Name, apps[i].c.Chart.Version, state, clip(detail))
	} else if !ok {
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
	total := c.ControlPlane.Count()
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
	if err := x.moved(ctx, apps[i].c); err != nil {
		return err
	}
	return x.startAtBoot(ctx)
}

// platformVM is one VM of an installation as the CAPI token reads it. err: its config or status could not be read,
// so nothing else about it is known (not even whether it is this installation's).
type platformVM struct {
	node, name      string
	vmid            int
	running, onboot bool
	err             error
}

// platformVMs are the installation's VMs: the members of the seed's pool and of proxcloud-system that carry its tag
// (stage 3 tags the seed, CAPMOX the machines: machineTags), in VMID order. A member that cannot be read is in the
// answer with its err and does not hide the others.
func platformVMs(ctx context.Context, capi *pve.Client, id string) ([]platformVM, error) {
	var out []platformVM
	for _, name := range []string{pve.SeedPool, pve.SystemPool} {
		pool, err := capi.GetPool(ctx, name)
		if err != nil {
			return nil, err
		}
		for i := 0; pool != nil && i < len(pool.Members); i++ {
			m := pool.Members[i]
			if m.Type != "qemu" {
				continue
			}
			cfg, err := capi.GetVMConfig(ctx, m.Node, m.VMID)
			if err != nil {
				out = append(out, platformVM{node: m.Node, vmid: m.VMID, err: err})
				continue
			}
			if !slices.Contains(vmTags(cfg), "pc-platform-"+id) {
				continue
			}
			st, err := capi.GetVMStatus(ctx, m.Node, m.VMID)
			if err != nil {
				out = append(out, platformVM{node: m.Node, name: cfg.Str("name"), vmid: m.VMID, err: err})
				continue
			}
			out = append(out, platformVM{node: m.Node, name: cfg.Str("name"), vmid: m.VMID, running: st.Status == "running", onboot: cfg.Int("onboot") == 1})
		}
	}
	slices.SortFunc(out, func(a, b platformVM) int { return a.vmid - b.vmid })
	return out, nil
}

// startAtBoot is the end of stage 7: onboot on every machine of the platform that lacks it, so that a node that
// reboots starts them again (stage 3 sets it on the seed; the cluster chart sets none, and CAPMOX has no field for it).
// It runs at every apply, so a machine that Cluster API replaced gets it at the next one. The CAPI token holds
// VM.Config.Options on both pools. No start order: Proxmox takes Sys.Modify on / for the startup option, which the
// token does not hold (docs/managed-services.md has the command). A machine that cannot be set does not fail the
// stage: the platform is built, and the Check's machines row names it.
func (x *acq) startAtBoot(ctx context.Context) error {
	vms, err := platformVMs(ctx, x.capi, x.in.ID)
	if err != nil {
		x.r.Log.Warn("platform: start at boot not checked", "installation", x.in.ID, "err", err)
		return nil
	}
	for _, vm := range vms {
		if vm.err != nil {
			x.r.Log.Warn("platform: start at boot not checked", "installation", x.in.ID, "vmid", vm.vmid, "err", vm.err)
			continue
		}
		if vm.onboot {
			continue
		}
		if err := x.check(ctx); err != nil {
			return err
		}
		up, err := x.capi.SetVMConfig(ctx, vm.node, vm.vmid, map[string]string{"onboot": "1"})
		if res := x.capi.WaitTaskBounded(ctx, up, taskBudget); err == nil && res.Outcome != pve.TaskSucceeded {
			err = fmt.Errorf("task %s: %s %v", up, res.ExitStatus, res.Err)
		}
		if err != nil {
			x.r.Log.Warn("platform: start at boot not set", "installation", x.in.ID, "vmid", vm.vmid, "err", err)
		}
	}
	return nil
}

// nodeTemplates refuses a node template that CAPMOX cannot boot a machine from: one with a Proxmox cloud-init drive on
// another slot than ide0. CAPMOX v0.9.2 puts its NoCloud ISO on ide0 (internal/inject: CloudInitISODevice) and touches
// no other drive, so a clone of such a template starts with two media labelled cidata, and cloud-init's NoCloud source
// takes the first of the devices sorted downwards, the later drive: Proxmox's own, with no kubeadm data and no
// address. The machine then never joins and the stage would wait out its hour. A drive on ide0 is replaced by CAPMOX's.
// The advice is to delete the drive, not to move it: a template without one is what CAPMOX needs, and the other reader
// of a node template, the template-updates smoke test, gives its clone a drive when the template has none
// (templateupdates.SmokeExecutor.test), so the corrected template can still be tested and published.
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
					"on ide0), then Retry. The template needs no drive of its own: its smoke test in Template updates adds one to its test VM.",
					templates[node], node, slot, node, templates[node], slot)
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
	stateful, first, argo := -1, apps[0].c.Wave, -1 // the CNI's wave is the lowest, wherever the BOM lists it
	for _, a := range apps {
		first = min(first, a.c.Wave)
		if a.c.Name == argoNamespace {
			argo = a.c.Wave
		}
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
	if err := x.tenantGate(ctx, wl, true); err != nil {
		return err
	}
	err = x.waves(ctx, seed, wl, apps, waveStall, func(wave int) error {
		if wave == first {
			if err := x.nodes(ctx, seed, wl); err != nil {
				return err
			}
		}
		if wave == argo {
			if err := x.argoAddress(ctx, wl, true); err != nil {
				return err
			}
		}
		if err := x.tenantGate(ctx, wl, false); err != nil {
			return err
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
	// ArgoCD calls a LoadBalancer Service without an address Progressing, so a wave that waits may wait for the
	// address: then the stage says so, and fails with that code at its deadline.
	var se *stageErr
	if errors.As(err, &se) && se.kind == kindWait && se.code == CodeComponentUnhealthy && x.in.Spec.Access.ArgoCD != nil {
		if aerr := x.argoAddress(ctx, wl, false); aerr != nil {
			return aerr
		}
	}
	return err
}

// argoAddress records where the platform cluster's ArgoCD answers (spec.access.argocd): status.argocdUrl once the
// Service argocd-server holds the address, nothing when the block is absent. A Service that asked for the address and
// has none waits with argocd_address_unavailable, naming the Service that holds it when one does. passed: the
// component's wave is Synced and Healthy; before that a Service that was not synced yet is the wave's own wait, and a
// cleared block still has its address.
func (x *acq) argoAddress(ctx context.Context, wl *kamajik8s.Client, passed bool) error {
	a, st := x.in.Spec.Access.ArgoCD, &x.in.Status
	set := func(url string, sources []string) error {
		if st.ArgoCDURL == url && slices.Equal(st.ArgoCDSources, sources) {
			return nil
		}
		return x.record(ctx, func(_ *Run, in *Installation, _ *Secrets) {
			in.Status.ArgoCDURL, in.Status.ArgoCDSources = url, sources
		})
	}
	if a == nil {
		if !passed {
			return nil
		}
		return set("", nil)
	}
	holds := func(svc map[string]any) bool {
		ingress, _ := dig(svc, "status", "loadBalancer", "ingress").([]any)
		return slices.ContainsFunc(ingress, func(i any) bool { m, _ := i.(map[string]any); return str(m, "ip") == a.Address })
	}
	svc, err := wl.GetService(ctx, argoNamespace, argoServer)
	if err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	if holds(svc) {
		if !passed { // the address is there; the networks let in are the rendered ones only once the wave is Synced
			return nil
		}
		return set(a.URL(), slices.Clone(a.Sources))
	}
	if !passed && (str(svc, "spec", "type") != "LoadBalancer" || str(svc, "metadata", "annotations", argoAddressAnnotation) != a.Address) {
		return nil
	}
	holder := "no Service of the platform cluster holds it, so it is outside MetalLB's pool or MetalLB is not answering"
	if all, err := wl.ListServicesAllNamespaces(ctx); err == nil { // best effort: the refusal stands without the name
		for _, o := range all {
			if holds(o) {
				holder = "Service " + str(o, "metadata", "namespace") + "/" + str(o, "metadata", "name") + " holds it"
			}
		}
	}
	// Sources without a URL: the block was rendered and the address not given, which the plan of a later clear needs.
	if err := set("", slices.Clone(a.Sources)); err != nil {
		return err
	}
	return waiting(CodeArgoCDAddressUnavailable, "%s was not given to the platform's ArgoCD: %s. Pick a free address of the load balancer range (%s), "+
		"save and apply again. The platform keeps serving: ArgoCD answers inside the cluster.", a.Address, holder, x.in.Spec.Clusters[0].MetalLBRange)
}

// Cluster API writes these on every Node it joins for a Machine (v1.14 api/core/v1beta2: ClusterNameAnnotation,
// MachineAnnotation).
const (
	nodeClusterAnnotation = "cluster.x-k8s.io/cluster-name"
	nodeMachineAnnotation = "cluster.x-k8s.io/machine"
)

// orphanNodes are the Nodes of the platform cluster that are not Ready and that no Machine owns: Cluster API joined
// the Node for a Machine of this cluster (its annotations), that Machine is gone, and no Machine or ProxmoxMachine
// names the Node by status.nodeRef, by the provider id of its VM or by its own name (a machine that is still joining
// has no nodeRef yet, and CAPMOX names the Node after the Machine). Such a Node is what Cluster API leaves when it
// gives up deleting it (spec.deletion.nodeDeletionTimeoutSeconds, 10 s by default): with one control plane machine
// that moment falls into the API gap the same deletion opens. Nothing else removes a Node here: the platform cluster
// has no cloud controller manager. An empty Machine list proves nothing and owns every Node.
func orphanNodes(nodes, machines, infra []map[string]any) (orphans []map[string]any) {
	if len(machines) == 0 {
		return nil
	}
	names, refs, ids := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, m := range machines {
		names[str(m, "metadata", "name")] = true
		refs[str(m, "status", "nodeRef", "name")] = true
		ids[str(m, "spec", "providerID")] = true
	}
	for _, pm := range infra {
		ids[str(pm, "spec", "providerID")] = true
	}
	for _, n := range nodes {
		name, a := str(n, "metadata", "name"), func(k string) string { return str(n, "metadata", "annotations", k) }
		id := str(n, "spec", "providerID")
		if condition(n, "Ready") == "True" || a(nodeClusterAnnotation) != ClusterName || a(nodeMachineAnnotation) == "" ||
			names[a(nodeMachineAnnotation)] || names[name] || refs[name] || (id != "" && ids[id]) {
			continue
		}
		orphans = append(orphans, n)
	}
	return orphans
}

// NodesCheck is the Check row nodes of an installed platform: ok when every node of the platform cluster is Ready, a
// warning (never a failure: a run must be able to start and repair) that names each node that is not, and says of a
// Node no Machine owns (orphanNodes) that the next apply deletes the Node object. nil when the seed or the platform
// cluster cannot be read: other rows and the run say that.
func NodesCheck(ctx context.Context, seed, wl *kamajik8s.Client) *Check {
	if seed == nil || wl == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	nodes, err := wl.ListNodes(ctx)
	if err != nil || len(nodes) == 0 {
		return nil
	}
	var unready []map[string]any
	for _, n := range nodes {
		if condition(n, "Ready") != "True" {
			unready = append(unready, n)
		}
	}
	if len(unready) == 0 {
		return &Check{ID: "nodes", Status: CheckOK, Message: fmt.Sprintf("The platform cluster's %d nodes are Ready.", len(nodes))}
	}
	machines, err := seed.ListCRs(ctx, capiGroup, capiVersion, "machines", ClusterName, clusterSelector)
	if err != nil {
		return nil
	}
	infra, err := seed.ListCRs(ctx, infraGroup, infraVersion, "proxmoxmachines", ClusterName, "")
	if err != nil {
		return nil
	}
	orphans := orphanNodes(unready, machines, infra)
	var lines []string
	for _, n := range unready {
		name, ready := str(n, "metadata", "name"), cmp.Or(condition(n, "Ready"), "not reported")
		if slices.ContainsFunc(orphans, func(o map[string]any) bool { return str(o, "metadata", "name") == name }) {
			lines = append(lines, fmt.Sprintf("Node %s is not Ready (%s) and no Machine owns it: Cluster API removed its machine %s and left the Node object, "+
				"which holds back DaemonSet rollouts and the replacement of the cluster's machines. The next Apply deletes the Node object, a Retry too.",
				name, ready, str(n, "metadata", "annotations", nodeMachineAnnotation)))
			continue
		}
		lines = append(lines, fmt.Sprintf("Node %s is not Ready (%s).", name, ready))
	}
	slices.Sort(lines)
	return &Check{ID: "nodes", Status: CheckWarn, Message: strings.Join(lines, "\n")}
}

// CertificatesCheck is the Check row certificates of an installed platform, the view before and after a move of
// cert-manager: ok when the controller, cainjector and webhook Deployments of cert-manager are Available on the seed and
// on the platform cluster and every Certificate there is Ready; otherwise a warning that names each one that is not.
// Never a failure, and no run waits on it: a Certificate that is not Ready keeps the certificate in its Secret, and one
// broken before an upgrade (an Issuer the operator removed) must not hold the platform. The run's own gate of a
// cert-manager move is its wave: the Application Healthy and its sync, with the chart's startupapicheck, Succeeded.
// nil when the seed or the platform cluster cannot be read: other rows and the run say that.
func CertificatesCheck(ctx context.Context, seed, wl *kamajik8s.Client) *Check {
	if seed == nil || wl == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var lines []string
	n := 0
	for _, side := range []struct {
		name string
		c    *kamajik8s.Client
	}{{"the seed", seed}, {"the platform cluster", wl}} {
		// cert-manager's namespace in both BOM entries, which a run does not change (Classify).
		deps, err := side.c.ListCRs(ctx, "apps", "v1", "deployments", "cert-manager", "")
		if err != nil {
			return nil
		}
		for _, name := range []string{"cert-manager", "cert-manager-cainjector", "cert-manager-webhook"} {
			i := slices.IndexFunc(deps, func(d map[string]any) bool { return str(d, "metadata", "name") == name })
			var d map[string]any
			if i >= 0 {
				d = deps[i]
			}
			if why := unavailable(d); why != "" {
				lines = append(lines, fmt.Sprintf("On %s, Deployment cert-manager/%s is not Available (%s).", side.name, name, why))
			}
		}
		certs, err := side.c.ListCRsAllNamespaces(ctx, "cert-manager.io", "v1", "certificates", "")
		if err != nil {
			return nil
		}
		n += len(certs)
		for _, c := range certs {
			if condition(c, "Ready") == "True" {
				continue
			}
			ready := cond(c, "Ready")
			lines = append(lines, fmt.Sprintf("On %s, Certificate %s/%s is not Ready (%s).", side.name, str(c, "metadata", "namespace"), str(c, "metadata", "name"),
				cmp.Or(strings.Trim(str(ready, "reason")+": "+str(ready, "message"), ": "), "Ready "+cmp.Or(str(ready, "status"), "not reported"))))
		}
	}
	if len(lines) == 0 {
		return &Check{ID: "certificates", Status: CheckOK, Message: fmt.Sprintf("cert-manager's Deployments are Available on the seed and on the platform cluster, "+
			"and their %d Certificates are Ready.", n)}
	}
	slices.Sort(lines)
	return &Check{ID: "certificates", Status: CheckWarn, Message: strings.Join(lines, "\n") + "\nA Certificate that is not Ready keeps the certificate its Secret " +
		"holds until a new one is issued: read why with kubectl describe certificate, and fix it before an upgrade that moves cert-manager. No run waits on this row."}
}

// nodeNames lists nodes by name, each with what its Ready condition says.
func nodeNames(nodes []map[string]any) string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, str(n, "metadata", "name")+" (Ready "+cmp.Or(condition(n, "Ready"), "not reported")+")")
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

// nodes waits until every node of the workload is Ready and each group has its count of nodes carrying its label.
// A Node that is not Ready and that no Machine owns (orphanNodes) is deleted first, through the platform cluster's API
// with the admin kubeconfig Cluster API keeps on the seed and only while its uid is the one that was read: the count
// could never come true with it, and a run that stood in this wait gets out with one Retry. The stage's items and
// its message name every Node deleted. A wait names the nodes it waits for.
func (x *acq) nodes(ctx context.Context, seed, wl *kamajik8s.Client) error {
	nodes, err := wl.ListNodes(ctx)
	if err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	var unready []map[string]any
	for _, n := range nodes {
		if condition(n, "Ready") != "True" {
			unready = append(unready, n)
		}
	}
	ownerless := ""
	if len(unready) > 0 { // the seed is read only when a node is not Ready
		machines, _, infra, err := x.machines(ctx, seed)
		if err != nil {
			return err
		}
		if orphans := orphanNodes(unready, machines, infra); len(orphans) > 0 {
			if err := x.check(ctx); err != nil {
				return err
			}
			items := slices.Clone(x.run.Stages[x.si].Items)
			var gone []string
			for _, n := range orphans {
				name := str(n, "metadata", "name")
				if err := wl.DeleteNode(ctx, name, str(n, "metadata", "uid")); err != nil {
					return classify(CodeWorkloadUnreachable, err)
				}
				gone = append(gone, name)
				item := StageItem{ID: "node/" + name, State: "deleted", Message: clip(fmt.Sprintf("Node %s was not Ready (%s) and no Machine owned it: Cluster API "+
					"joined it for Machine %s, which is gone, and gave up deleting the Node. The run deleted the Node object.", name,
					cmp.Or(condition(n, "Ready"), "not reported"), str(n, "metadata", "annotations", nodeMachineAnnotation)))}
				items = append(slices.DeleteFunc(items, func(i StageItem) bool { return i.ID == item.ID }), item)
				if x.r.OnNodeDeleted != nil {
					x.r.OnNodeDeleted(x.in, name, item.Message, templateupdates.PlatformRunActor+x.run.ID)
				}
			}
			x.r.Log.Info("platform run: deleted nodes no machine owns", "run", x.runID, "nodes", strings.Join(gone, ","))
			if err := x.saveStage(ctx, func(s *Stage) { s.Items = items }); err != nil {
				return err
			}
			return waiting(CodeNodesTimeout, "Deleted %d Node no Machine owned (%s): left by a machine Cluster API removed while the platform cluster's API "+
				"did not answer. The nodes are counted again at the next look.", len(gone), strings.Join(gone, ", "))
		}
		// Not Ready and not deleted: say which have no Machine by nodeRef, so the reader sees what the run will not touch.
		refs := map[string]bool{}
		for _, m := range machines {
			refs[str(m, "status", "nodeRef", "name")] = true
		}
		var names []string
		for _, n := range unready {
			if name := str(n, "metadata", "name"); !refs[name] {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			ownerless = " No Machine names " + strings.Join(names, ", ") + " as its node yet."
		}
	}
	ready, total := map[string]int{}, x.in.Spec.Clusters[0].ControlPlane.Count()
	for _, n := range nodes {
		if condition(n, "Ready") == "True" {
			ready[str(n, "metadata", "labels", GroupLabel)]++
		}
	}
	for _, g := range x.in.Spec.Clusters[0].NodeGroups {
		total += g.Count
		if ready[g.Name] != g.Count {
			return waiting(CodeNodesTimeout, "%d of %d nodes of group %s are Ready with the label %s. Not Ready: %s.%s", ready[g.Name], g.Count, g.Name, GroupLabel,
				cmp.Or(nodeNames(unready), "none"), ownerless)
		}
	}
	switch {
	case len(unready) > 0:
		return waiting(CodeNodesTimeout, "%d nodes exist, %d of them not Ready: %s; the cluster has %d.%s", len(nodes), len(unready), nodeNames(unready), total, ownerless)
	case len(nodes) != total:
		return waiting(CodeNodesTimeout, "%d nodes exist and the cluster has %d machines: %s.", len(nodes), total, nodeNames(nodes))
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
	var items []StageItem
	// A node image is offered only where it is published; a cluster that cannot be read offers none in this run. A
	// version is offered once (status.kubernetesOffered): an apply does not add again one the operator removed.
	version := StageItem{ID: "kubernetes-version", State: "warn"}
	offered := slices.Contains(x.in.Status.KubernetesOffered, x.bom.Kubernetes.Version)
	if offered {
		version.State, version.Message = "done", fmt.Sprintf("Kubernetes %s was offered before; a run offers a version once, so the provider's versions are left as they are. "+
			"POST …/kubernetes-version offers it again when the provider no longer names it.", x.bom.Kubernetes.Version)
	} else if rows, err := x.portal.ClusterResources(ctx, "vm"); err != nil {
		version.Message = "Proxmox did not list its templates, so no Kubernetes version was offered in this run: " + clip(err.Error())
	} else if line, missing := NodeImageLine(x.bom, x.in, rows); line == "" {
		version.Message = fmt.Sprintf("Kubernetes %s is not offered to tenants: no node image of it is published on %s. Publish it from the Ready card.",
			x.bom.Kubernetes.Version, NodesText(missing))
	} else {
		in.NodeImageLine, offered = line, true
		version.State, version.Message = "done", fmt.Sprintf("Kubernetes %s is published on %s and is offered unless the provider already names that version.", x.bom.Kubernetes.Version, line)
	}
	items = append(items, version)
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
	// The attach choice is applied once, when it differs from the one applied last: an apply must not attach again
	// projects the operator detached since, or turn a policy back on. It is recorded only when both providers took it:
	// until then the next apply applies it to both again, the one that took it included.
	choice, applied := x.in.Spec.Tenants.AttachChoice(), x.in.Status.TenantsAttach
	if x.r.Attach != nil && choice != (Tenants{Attach: applied}).AttachChoice() {
		actor := x.run.RequestedBy
		if actor == "" {
			actor = templateupdates.PlatformRunActor + x.run.ID
		}
		all := true
		for _, id := range []string{ids.Kubernetes, ids.Database} {
			done, note, err := x.r.Attach(ctx, x.in.ID, id, choice, actor)
			if err != nil { // the providers are registered: an attach problem is a warning, never a failed install
				done, note = false, "The attach choice was not applied: "+clip(err.Error())+" The next apply tries again."
			}
			it := StageItem{ID: "attach:" + id, State: "done", Message: note}
			if !done { // the next apply tries again
				it.State, all = "warn", false
			}
			items = append(items, it)
		}
		if all {
			applied = choice
		}
	}
	if err := x.saveStage(ctx, func(s *Stage) { s.Items = items }); err != nil {
		return err
	}
	return x.record(ctx, func(_ *Run, in *Installation, _ *Secrets) {
		in.Status.Providers, in.Status.TenantsAttach = &ids, applied
		if v := x.bom.Kubernetes.Version; offered && !slices.Contains(in.Status.KubernetesOffered, v) {
			in.Status.KubernetesOffered = append(in.Status.KubernetesOffered, v)
		}
	})
}

// done is stage 11: what was applied, and the BOM it came from. An upgrade ends here: only now, with every stage
// passed, does status.bom name the target, and status.targetBom, status.tenantsDownBefore and the cluster's roll
// record are cleared.
func (x *acq) done(ctx context.Context) error {
	return x.save(ctx, func(run *Run, in *Installation, _ *Secrets) {
		now := time.Now().UTC()
		in.Status.Components = nil
		for _, c := range x.bom.Components {
			in.Status.Components = append(in.Status.Components, ComponentStatus{Name: c.Name, Cluster: c.Cluster, Version: c.Chart.Version,
				Digest: c.Chart.Digest, AppliedAt: &now, Managed: true})
		}
		ref := run.BOM
		in.Status.BOM, in.Status.TargetBOM, in.Status.TenantsDownBefore = &ref, nil, nil
		for i := range in.Status.Clusters {
			in.Status.Clusters[i].Roll = nil
		}
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
// created and is the operator's from then on: its name, its status, the node image versions, the attach policy. Two
// things a run still does there: it adds a Kubernetes version the row does not name (OfferVersion), and it applies a
// changed spec.tenants.attach (Runner.Attach).
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
	offered, _ := dig(body, "config", "cluster", "versions").(map[string]any)
	for version, line := range offered {
		if l, ok := line.(string); ok {
			OfferVersion(found, version, l)
		}
	}
	return found, nil
}
