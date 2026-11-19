package platformlife

// roll.go — the replacement of the platform cluster's machines on an upgrade whose BOM names another Kubernetes
// version or another node image (class machines). Cluster API replaces the machines; the run orders it: the control
// plane first, then one MachineDeployment at a time, each step a render of the cluster's values (Roll in render.go)
// that ArgoCD syncs. Nothing is patched on the live cluster: ArgoCD would put a patch back.
//
// The step is stored on the installation before its Application is applied and only ever raised (RollStatus), so no
// apply renders a replaced unit at the old version. Nothing here restores or goes back: once the new control plane
// machine has joined, etcd has moved on and kubeadm has no downgrade.
//
// None of this has run on a lab. What is expected of each unit is in the plan's rows and marked "not measured".

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
)

// rollStall fails stage 7 of a roll when no machine moved for this long. The timer starts again whenever the machines'
// items change or a unit is done.
const rollStall = 30 * time.Minute

// noWayBack is the first line of the plan of every move that replaces machines.
const noWayBack = "There is no way back once the new control plane machine has joined the platform cluster: the run restores nothing and has no rollback, " +
	"and neither Cancel nor Remove brings the old machines or the old Kubernetes version back. A step that fails leaves the machines that still run " +
	"serving, and Retry goes on at that step. Tenant backups are the only net for a database whose volume does not follow it to the new machine."

const notMeasured = " Not measured on a lab yet."

// rollUnit is what a roll replaces in one step: the control plane, or the MachineDeployment of one node group on one
// Proxmox node.
type rollUnit struct {
	Name        string // "controlPlane" or "<group>@<node>"
	Key         string // "" for the control plane, else "group/node" (Roll.DeleteFirst, rollOrder)
	Group, Node string
	Owner       string // the KubeadmControlPlane's or the MachineDeployment's name
	Machines    int
	MemoryMiB   int
	DiskGiB     int
}

// rollUnits are the units of a cluster in the order a roll takes them; a step n of RollStatus is rollUnits[n-1].
func rollUnits(c ClusterSpec) []rollUnit {
	cp := c.ControlPlane
	out := []rollUnit{{Name: "controlPlane", Node: cp.Node, Owner: ClusterName + "-cp", Machines: cp.Count(), MemoryMiB: cp.MemoryMiB, DiskGiB: cp.DiskGiB}}
	nodes, groups := map[string][]string{}, map[string]NodeGroup{}
	for _, g := range c.NodeGroups {
		nodes[g.Name], groups[g.Name] = g.Nodes, g
	}
	for _, key := range rollOrder(nodes) {
		group, node, _ := strings.Cut(key, "/")
		g := groups[group]
		n := g.Count / len(g.Nodes) // as the chart splits machineCount: the first nodes take the remainder
		if slices.Index(g.Nodes, node) < g.Count%len(g.Nodes) {
			n++
		}
		out = append(out, rollUnit{Name: group + "@" + node, Key: key, Group: group, Node: node, Owner: ClusterName + "-" + group + "-" + node,
			Machines: n, MemoryMiB: g.MemoryMiB, DiskGiB: g.DiskGiB})
	}
	return out
}

// rollOf is the installation's roll record (nil: no roll has begun).
func rollOf(in *Installation) *RollStatus {
	if len(in.Status.Clusters) == 0 {
		return nil
	}
	return in.Status.Clusters[0].Roll
}

// hasMachines reports a move that replaces the platform cluster's machines.
func hasMachines(changes []PlanChange) bool {
	return slices.ContainsFunc(changes, func(c PlanChange) bool { return c.Class == ClassMachines && !strings.HasPrefix(c.Name, "roll") })
}

// k8sStep is why Kubernetes does not move from version from to version to ("" when it does): never back, and at most
// one minor up. The same version is a step: a rebuilt node image of it.
func k8sStep(from, to string) string {
	fa, fi, fp, err := kamaji.ParseVersion(from)
	if err != nil {
		return err.Error()
	}
	ta, ti, tp, err := kamaji.ParseVersion(to)
	switch {
	case err != nil:
		return err.Error()
	case slices.Compare([]int{ta, ti, tp}, []int{fa, fi, fp}) < 0:
		return "Kubernetes never goes back"
	case ta != fa || ti > fi+1:
		return fmt.Sprintf("one minor at a time: go through %d.%d first", fa, fi+1)
	}
	return ""
}

// rollRows are the rows of a plan that say what a roll does, in its order: the line that there is no way back, one row
// per unit with what stops serving while it is replaced, then per unit what the run makes and deletes on the Proxmox
// site. deleteFirst are the units replaced by deleting the machine first; templates the target image's template VMID
// per node when the site was read; databases the number of one-instance databases on the cluster, -1 when not read.
func rollRows(in *Installation, from, to *BOM, deleteFirst []string, templates map[string]int, databases int) []PlanChange {
	c := in.Spec.Clusters[0]
	units := rollUnits(c)
	fromV, toV := from.Kubernetes.Version, to.Kubernetes.Version
	if fromV == toV { // a rebuilt image of the same version
		fromV, toV = from.Kubernetes.NodeImage.Tag, to.Kubernetes.NodeImage.Tag
	}
	out := []PlanChange{{Name: "roll", Class: ClassMachines, Stage: stageCluster, Impact: noWayBack}}
	var site []PlanChange
	for _, u := range units {
		first := slices.Contains(deleteFirst, u.Key)
		var text string
		switch {
		case u.Machines == 0:
			text = "No machine of this group is on this node: nothing is replaced."
		case u.Key == "" && u.Machines == 1:
			text = "The one control plane machine is replaced: a second machine is cloned and joins, then the old one is removed. From the removal of the old " +
				"etcd member until the new machine holds the API address (the kube-vip lease, up to 60 seconds) the platform cluster's API does not answer: " +
				"expected one to a few minutes. Meanwhile nothing on the platform can be created, changed, repaired or failed over: no tenant cluster or " +
				"database operation, no restart of a failed tenant API server pod, no database failover, no ArgoCD sync, and the consoles show stale status. " +
				"Running tenant API servers, tenant workloads and databases are expected to keep answering: they are pods on the node groups, not on the " +
				"control plane. That expectation comes from reading the manifests; whether tenant API servers answer through the gap is for the lab to measure."
		case u.Key == "":
			text = fmt.Sprintf("The %d control plane machines are replaced one at a time, one more machine first. No gap of the platform cluster's API is expected: "+
				"the other machines keep etcd and the API address.", u.Machines)
		case u.Group == GroupSystem:
			text = "The platform's operators (ArgoCD, Cluster API for tenants, CloudNativePG, MetalLB, cert-manager) restart on the new machine. A MetalLB " +
				"address may move to another node (seconds). Tenant API servers, tenant workloads and databases keep running."
		case u.Group == "kamaji" && c.groupCount("kamaji") == 1:
			text = "The one kamaji machine is replaced: ALL tenant API servers are away while their pods restart on the new machine, and kamaji-etcd moves " +
				"with its disk. Tenant workloads keep running; nothing in a tenant cluster can be created or changed meanwhile."
		case u.Group == "kamaji":
			text = "Each tenant API server with a pod on this machine restarts on another one; a tenant control plane with one replica is away while it does. " +
				"Tenant workloads keep running."
		case u.Group == "dbaas":
			text = "Every database with one instance on this machine is down while its pod moves to the new machine and its disk is attached there. A " +
				"database with three instances fails over."
			if databases >= 0 {
				text += fmt.Sprintf(" The platform cluster holds %d databases with one instance.", databases)
			}
		default:
			text = "Every pod on this machine restarts on the new one."
		}
		if first && u.Machines > 0 {
			text += fmt.Sprintf(" Node %s has no room for one more machine of this group, so the old machine is deleted BEFORE the new one exists: everything on it "+
				"is away for the whole clone and join, and stays away if the new machine fails to join, until a Retry succeeds.", u.Node)
		}
		if u.Machines > 0 {
			text += notMeasured
		}
		out = append(out, PlanChange{Cluster: ClusterName, Name: "roll/" + u.Name, From: fromV, To: toV, Class: ClassMachines, Stage: stageCluster, Impact: text})
		if u.Machines == 0 {
			continue
		}
		tmpl := "the template of node image " + to.Kubernetes.NodeImage.Tag
		if vmid := templates[u.Node]; vmid > 0 {
			tmpl = fmt.Sprintf("template %d", vmid)
		}
		how := "one more VM than today exists on the node while a machine is replaced"
		if first {
			how = "the old VM is deleted first"
		}
		site = append(site, PlanChange{Cluster: ClusterName, Name: "roll-vm/" + u.Name, Class: ClassSite, Stage: stageCluster,
			To: fmt.Sprintf("%d new VM on %s cloned from %s, %d old VM deleted with its boot disk (%s)", u.Machines, u.Node, tmpl, u.Machines, how),
			Impact: "Cluster API clones the new VM and deletes the old one with its boot disk. Volumes of proxmox-csi are separate disks and are kept. Only VMs that " +
				"carry this installation's tag (pc-platform-" + in.ID + ") are deleted: no other VM on the node is touched."})
	}
	return append(out, site...)
}

// groupCount is the machine count of a node group (0 when the cluster has none of that name).
func (c ClusterSpec) groupCount(name string) int {
	for _, g := range c.NodeGroups {
		if g.Name == name {
			return g.Count
		}
	}
	return 0
}

// UpgradePart is one of the two operator actions of a move that replaces machines: "seed" (the node image on every
// node, the seed's k3s, the seed's own charts; no tenant is touched) and "platform" (everything else, the roll
// included). An apply names the seed part with part "seed"; without a part it is the platform part.
type UpgradePart struct {
	ID       string   `json:"id"`
	Done     bool     `json:"done"`
	Allowed  bool     `json:"allowed"`
	Confirm  string   `json:"confirm,omitempty"`
	Blocking []string `json:"blocking"`
}

// setParts gives a move that replaces machines its two parts and its roll rows. The move's own Allowed, Blocking and
// Confirm become those of the platform part: the apply without a part.
func (u *Upgrade) setParts(in *Installation, from, to *BOM) {
	roll := rollOf(in)
	phrase := "upgrade " + in.Name + " to " + to.Release
	seed := UpgradePart{ID: PartSeed, Blocking: slices.Clone(u.Blocking)}
	platform := UpgradePart{ID: "platform", Blocking: slices.Clone(u.Blocking)}
	behind := from.Seed.K3s.Version != to.Seed.K3s.Version && (in.Status.Seed == nil || in.Status.Seed.K3sVersion != to.Seed.K3s.Version)
	// The node image is read from Proxmox by the route: NeedRoll withdraws Done when a node lacks its template.
	seed.Done = u.InProgress && !behind
	if roll == nil && behind {
		platform.Blocking = append(platform.Blocking, CodeSeedNotUpgraded+": the seed's k3s is not at "+to.Seed.K3s.Version+` yet; run "Seed and node image" first`)
	}
	seed.Allowed = len(seed.Blocking) == 0 && roll == nil // once the roll has begun only the platform part goes on
	platform.Allowed = len(platform.Blocking) == 0
	if seed.Allowed && !u.InProgress {
		seed.Confirm = phrase
	}
	if platform.Allowed && roll == nil { // the run that begins the roll, whether it starts the upgrade or continues it
		platform.Confirm = phrase + " and replace its machines"
	}
	u.Parts = []UpgradePart{seed, platform}
	u.Allowed, u.Blocking, u.Confirm = platform.Allowed, platform.Blocking, platform.Confirm
	var first []string
	if roll != nil {
		first = roll.DeleteFirst
	}
	for i, c := range u.Changes { // the cluster chart's row promises that no machine is replaced: not on this move
		if c.Class == ClassChart && c.Cluster == ClusterSeed && c.Name == ClusterName {
			u.Changes[i].Impact = "The Application moves to the new chart version with the first step of the roll (rows roll/…)."
		}
	}
	u.setRollRows(rollRows(in, from, to, first, nil, -1))
}

// setRollRows puts the roll's rows into the plan: the line that there is no way back first of all, the others last.
func (u *Upgrade) setRollRows(rows []PlanChange) {
	rest := slices.DeleteFunc(u.Changes, func(c PlanChange) bool { return strings.HasPrefix(c.Name, "roll") })
	u.Changes = append(append([]PlanChange{rows[0]}, rest...), rows[1:]...)
}

// part is what an apply of part id ("" or "seed") is decided by: the move's own answer when it has no parts.
func (u *Upgrade) part(id string) (allowed bool, blocking []string, confirm string) {
	if u == nil {
		return true, nil, ""
	}
	for _, p := range u.Parts {
		if p.ID == id {
			return p.Allowed, p.Blocking, p.Confirm
		}
	}
	return u.Allowed, u.Blocking, u.Confirm
}

// NeedsRollRead reports a move whose platform part must read the site before it starts: it replaces machines and
// the roll has not begun.
func (u *Upgrade) NeedsRollRead(in *Installation) bool {
	return u != nil && len(u.Parts) > 0 && rollOf(in) == nil
}

// RollRead is what was read of the site for the platform part of a move that replaces machines: the plan route reads
// it, POST …/runs reads it again and hands it to StartRun, and stage 1 of the run reads it a third time.
type RollRead struct {
	// Read is false on the zero value: nothing was read, and StartRun then refuses the platform part.
	Read bool
	// Blocking are the pre-checks that failed, each "<code>: <message with its fix>".
	Blocking []string
	// DeleteFirst are the units ("group/node") whose node has no room for one more machine.
	DeleteFirst []string
	// Templates is the target node image's template per node, where one was found.
	Templates map[string]int
	// Databases is the number of databases with one instance on the platform cluster (-1: not read).
	Databases int
}

// RollRefused is the 409 of an apply whose platform part a pre-check of the site refuses; Code is the first one's.
type RollRefused struct {
	Code    string
	Reasons []string
}

func (e *RollRefused) Error() string { return strings.Join(e.Reasons, "; ") }

func rollRefused(reasons []string) *RollRefused {
	code, _, _ := strings.Cut(reasons[0], ":")
	return &RollRefused{Code: code, Reasons: reasons}
}

// NeedRoll adds the site's read to the moves of p that replace machines and have not begun their roll: the roll rows
// with the surge or delete-first decision, the templates and the database count, and on the platform part every
// pre-check that failed. read is called once, and only when a move needs it.
func (p *Plan) NeedRoll(in *Installation, read func(to BOMRef) RollRead) {
	for _, u := range []*Upgrade{p.Upgrade, p.Replacement} {
		if !u.NeedsRollRead(in) {
			continue
		}
		from, okF := Lookup(u.From.SHA256)
		to, okT := Lookup(u.To.SHA256)
		if !okF || !okT {
			continue
		}
		r := read(u.To)
		u.setRollRows(rollRows(in, from.BOM, to.BOM, r.DeleteFirst, r.Templates, r.Databases))
		seed, platform := &u.Parts[0], &u.Parts[1]
		if slices.ContainsFunc(r.Blocking, func(b string) bool { return strings.HasPrefix(b, CodeNodeImageMissing) }) {
			seed.Done = false
		}
		if len(r.Blocking) > 0 {
			platform.Blocking = append(platform.Blocking, r.Blocking...)
			platform.Allowed, platform.Confirm = false, ""
		}
		u.Allowed, u.Blocking, u.Confirm = platform.Allowed, platform.Blocking, platform.Confirm
	}
}

// RollSite is what the pre-checks of a roll read: Proxmox with the portal token and with Cluster API's, the import
// receipts of Template updates, the seed's API and the platform cluster's. A nil member fails the checks that need it.
type RollSite struct {
	Portal, CAPI *pve.Client
	Imports      interface {
		ByKey(ctx context.Context, serviceID, node, digest string) ([]templateupdates.Receipt, error)
	}
	Seed, Workload *kamajik8s.Client
}

// RollClients opens the seed's API with the stored credential and the platform cluster's with the kubeconfig Cluster
// API keeps on the seed, for the plan's read. Either is nil when it cannot be opened: the checks that need it then fail.
func RollClients(ctx context.Context, in *Installation, sec *Secrets) (seed, workload *kamajik8s.Client) {
	seed, err := storedSeed(in, sec)
	if err != nil {
		return nil, nil
	}
	if data, err := seed.GetSecretData(ctx, ClusterName, kubeconfigSecret); err == nil && len(data["value"]) > 0 {
		workload, _ = kamajik8s.New(string(data["value"]))
	}
	return seed, workload
}

// nodeImageTemplates finds the template of image img on every node the cluster uses, as stage 2 does: an import
// receipt of this provider, node and digest that finished and still has its template in the template pool.
func nodeImageTemplates(ctx context.Context, s RollSite, in *Installation, img VMImage) (found map[string]int, missing []string, err error) {
	if s.Portal == nil || s.Imports == nil {
		return nil, nil, errors.New("the installation's Proxmox provider or Template updates is not available")
	}
	rows, err := s.Portal.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, nil, err
	}
	found = map[string]int{}
	for _, node := range in.ClusterNodes() {
		recs, err := s.Imports.ByKey(ctx, in.ServiceID, node, img.Digest)
		if err != nil {
			return nil, nil, err
		}
		for _, rec := range recs {
			if rec.TemplateGone || (rec.Step != templateupdates.StepTemplated && rec.Step != templateupdates.StepSucceeded) {
				continue
			}
			if slices.ContainsFunc(rows, func(r pve.ClusterResource) bool {
				return r.VMID == rec.VMID && r.Node == node && r.Template == 1 && r.Pool == pve.TemplatePool
			}) {
				found[node] = rec.VMID
			}
		}
		if found[node] == 0 {
			missing = append(missing, node)
		}
	}
	return found, missing, nil
}

// ReadRoll makes the pre-checks of the platform part of a move to to that replaces machines. Each failure is a
// blocking reason with its code and its fix; a read that fails blocks under the code of its check, because a roll must
// not start on what could not be seen. Tenants that are unhealthy do not block: the run notes them before its first
// step and does not wait for them (tenantGate).
func ReadRoll(ctx context.Context, in *Installation, to *BOM, s RollSite) RollRead {
	out := RollRead{Read: true, Databases: -1}
	block := func(code, format string, a ...any) {
		out.Blocking = append(out.Blocking, code+": "+fmt.Sprintf(format, a...))
	}

	img := to.Kubernetes.NodeImage
	found, missing, err := nodeImageTemplates(ctx, s, in, img)
	if err != nil {
		block(CodeNodeImageMissing, "whether the nodes hold a template of node image %s could not be read (%s); read the plan again", img.Tag, clip(err.Error()))
	}
	for _, node := range missing {
		block(CodeNodeImageMissing, `node %s has no template of node image %s; run "Seed and node image" first`, node, img.Tag)
	}
	out.Templates = found

	short, first, err := rollRoom(ctx, s.CAPI, in)
	switch {
	case err != nil:
		block(CodeCapacityShort, "whether the nodes have room for one more machine could not be read (%s); read the plan again", clip(err.Error()))
	case short != "":
		block(CodeCapacityShort, "%s", short)
	}
	out.DeleteFirst = first

	if s.Seed == nil {
		block(CodePlatformUnhealthy, "the seed's API could not be opened, so the machines and the platform's components were not read; see the platform page for the seed's state")
	} else {
		var bad []string
		machines, err := s.Seed.ListCRs(ctx, capiGroup, capiVersion, "machines", ClusterName, clusterSelector)
		if err != nil {
			bad = append(bad, "the Machines could not be read ("+clip(err.Error())+")")
		}
		want := 0
		for _, u := range rollUnits(in.Spec.Clusters[0]) {
			want += u.Machines
		}
		for _, m := range machines {
			if phase := str(m, "status", "phase"); phase != "Running" {
				bad = append(bad, "Machine "+str(m, "metadata", "name")+" is "+phase)
			}
		}
		if err == nil && len(machines) != want {
			bad = append(bad, fmt.Sprintf("%d Machines exist and the settings name %d", len(machines), want))
		}
		apps, err := s.Seed.ListApplications(ctx, argoNamespace, "proxcloud.io/platform-installation="+in.ID)
		if err != nil {
			bad = append(bad, "the platform's Applications could not be read ("+clip(err.Error())+")")
		}
		for _, a := range apps {
			if h := str(a, "status", "health", "status"); h != "Healthy" {
				bad = append(bad, "component "+str(a, "metadata", "name")+" is "+h)
			}
		}
		if len(bad) > 0 {
			block(CodePlatformUnhealthy, "%s. A roll starts only on a platform whose machines all run and whose components are all Healthy: repair it "+
				"(an apply of the installed release, Retry), then read the plan again", strings.Join(bad, "; "))
		}
	}

	if s.Workload == nil {
		block(CodeDrainBlocked, "the platform cluster's API could not be opened, so its PodDisruptionBudgets were not read; read the plan again")
		return out
	}
	pdbs, err := s.Workload.ListCRsAllNamespaces(ctx, "policy", "v1", "poddisruptionbudgets", "")
	if err != nil {
		block(CodeDrainBlocked, "the PodDisruptionBudgets of the platform cluster could not be read (%s); read the plan again", clip(err.Error()))
	}
	for _, p := range pdbs {
		allowed, _ := dig(p, "status", "disruptionsAllowed").(float64)
		pods, _ := dig(p, "status", "expectedPods").(float64)
		if allowed > 0 || pods == 0 {
			continue
		}
		ns, name := str(p, "metadata", "namespace"), str(p, "metadata", "name")
		fix := "give its workload a second replica or delete the budget"
		if dig(p, "metadata", "labels", "cnpg.io/cluster") != nil || dig(p, "spec", "selector", "matchLabels", "cnpg.io/cluster") != nil {
			fix = "it is a database's: bump the database's chart to 0.5.2 or newer (docs/managed-dbaas.md, \"Bump a database's chart\"), which sets no budget for one instance"
		}
		block(CodeDrainBlocked, "PodDisruptionBudget %s/%s allows no disruption, so Cluster API could never drain the machine its pod is on and the roll "+
			"would wait for ever; %s", ns, name, fix)
	}
	if dbs, err := s.Workload.ListCRsAllNamespaces(ctx, "postgresql.cnpg.io", "v1", "clusters", ""); err == nil {
		out.Databases = 0
		for _, d := range dbs {
			if n, _ := dig(d, "spec", "instances").(float64); n <= 1 {
				out.Databases++
			}
		}
	}
	return out
}

// rollSite is the site as a run reads it.
func (x *acq) rollSite(ctx context.Context) RollSite {
	s := RollSite{Portal: x.portal, CAPI: x.capi, Imports: x.r.Imports}
	if seed, err := x.seed(); err == nil {
		s.Seed = seed
		s.Workload, _ = x.workload(ctx, seed)
	}
	return s
}

// rollChecks is the end of stage 1 on the run that begins a roll: the pre-checks the plan showed, read again.
func (x *acq) rollChecks(ctx context.Context) error {
	if !x.rolls || x.run.Part != "" || rollOf(x.in) != nil {
		return nil
	}
	if r := ReadRoll(ctx, x.in, x.bom, x.rollSite(ctx)); len(r.Blocking) > 0 {
		e := rollRefused(r.Blocking)
		return permanent(e.Code, "%s. Nothing was changed.", clip(e.Error()))
	}
	return nil
}

// apiAway makes a platform API that does not answer a wait: during the control plane's step that is expected.
func apiAway(err error) error {
	var se *stageErr
	if errors.As(err, &se) && se.kind == kindRetry && se.code == CodeWorkloadUnreachable {
		return waiting(CodeWorkloadUnreachable, "The platform cluster's API does not answer, as while its control plane machine is replaced: %s", se.msg)
	}
	return err
}

// roll is stage 7 of a run that replaces the cluster's machines. Per step: the gate reads ArgoCD's comparison of the
// step's render before anything of it syncs (rolloutGate: only the step's own unit may differ), the Application is
// applied, ArgoCD's sync of exactly this source is waited for, then the unit: its owner settled at its count with
// every machine at the target version and Running, and no machine of it left over. Between two units every node must
// be Ready and every tenant that was healthy before the first step healthy again. A Cancel is honoured between
// looks; a machine in flight is finished by Cluster API on its own.
func (x *acq) roll(ctx context.Context, seed *kamajik8s.Client, templates []StageItem) error {
	units := rollUnits(x.in.Spec.Clusters[0])
	wl, err := x.workload(ctx, seed)
	if err != nil {
		return err
	}
	if wl == nil {
		return permanent(CodeWorkloadUnreachable, "The seed holds no %s/%s: there is no platform cluster whose machines could be replaced.", ClusterName, kubeconfigSecret)
	}
	if rollOf(x.in) == nil {
		if err := apiAway(x.tenantGate(ctx, wl, true)); err != nil {
			return err
		}
		first := slices.Clone(x.run.DeleteFirst)
		if err := x.save(ctx, func(_ *Run, in *Installation, _ *Secrets) {
			in.Status.Clusters[0].Roll = &RollStatus{Step: 1, DeleteFirst: first}
		}); err != nil {
			return err
		}
	}
	// A run that continues a roll starts the stall timer at its first look, in place of the stage's hour.
	if !strings.HasPrefix(x.run.Stages[x.si].Progress, "step ") {
		if err := x.progress(ctx, nil); err != nil {
			return err
		}
	}
	target := strings.TrimPrefix(x.bom.Kubernetes.Version, "v")
	for {
		st := rollOf(x.in)
		if st.Step < 1 {
			return permanent(CodeComponentUnhealthy, "The installation's record names step %d of a roll, which no run writes. Nothing was applied: the record was changed by hand.", st.Step)
		}
		if st.Step > len(units) {
			break
		}
		u := units[st.Step-1]
		apps, err := x.apps(ClusterSeed) // rendered at this step
		if err != nil {
			return err
		}
		a := apps[slices.IndexFunc(apps, func(a appOf) bool { return a.c.Name == ClusterName })]
		if err := x.rollApply(ctx, seed, a, st, u); err != nil {
			return err
		}
		items, done, note, failed, err := x.unitState(ctx, seed, units, u, target)
		if err != nil {
			return err
		}
		if items = append(slices.Clone(templates), items...); !slices.Equal(items, x.run.Stages[x.si].Items) {
			if err := x.progress(ctx, items); err != nil {
				return err
			}
		}
		if failed != nil {
			return failed
		}
		if !done {
			return waiting(CodeRolloutStalled, "Step %d of %d, %s: %s The stage fails when no machine moves for %d minutes.", st.Step, len(units), u.Name, note, int(rollStall.Minutes()))
		}
		// ponytail: the tenants are waited for between two units, not between two machines of one MachineDeployment
		// (Cluster API waits for the new node only). minReadySeconds on the MachineDeployment is the upgrade path.
		if err := apiAway(x.nodes(ctx, wl)); err != nil {
			return err
		}
		if err := apiAway(x.tenantGate(ctx, wl, false)); err != nil {
			return err
		}
		if err := x.save(ctx, func(_ *Run, in *Installation, _ *Secrets) {
			if r := in.Status.Clusters[0].Roll; r.Step == st.Step { // only ever raised
				r.Step++
			}
		}); err != nil {
			return err
		}
		if err := x.progress(ctx, items); err != nil {
			return err
		}
	}
	// Every unit runs the target: only now does the record say so.
	in := x.renderInput()
	if cur := x.in.Status.Clusters[0]; cur.KubernetesVersion != x.bom.Kubernetes.Version || !maps.Equal(cur.Templates, in.Templates) {
		if err := x.save(ctx, func(_ *Run, inst *Installation, _ *Secrets) {
			inst.Status.Clusters[0].KubernetesVersion, inst.Status.Clusters[0].Templates = x.bom.Kubernetes.Version, in.Templates
		}); err != nil {
			return err
		}
	}
	for _, c := range x.bom.Components {
		if c.Cluster == ClusterSeed && c.Name == ClusterName {
			if err := x.moved(ctx, c); err != nil {
				return err
			}
		}
	}
	return x.startAtBoot(ctx)
}

// progress writes the stage's items (nil: leave them) and starts the stall timer again.
func (x *acq) progress(ctx context.Context, items []StageItem) error {
	return x.saveStage(ctx, func(s *Stage) {
		now := time.Now().UTC()
		d := now.Add(rollStall)
		if items != nil {
			s.Items = items
		}
		if r := rollOf(x.in); r != nil {
			s.Progress = fmt.Sprintf("step %d of %d", min(r.Step, len(rollUnits(x.in.Spec.Clusters[0]))), len(rollUnits(x.in.Spec.Clusters[0])))
		}
		s.LastProgressAt, s.Deadline = &now, &d
	})
}

// rollApply puts the Application at the step's render and waits until ArgoCD has synced exactly that source: each
// step's source differs from the one before, so a sync of an earlier step cannot pass. Before a step that makes one
// more machine first, the node is counted again: short now is a refusal with nothing of the step applied.
func (x *acq) rollApply(ctx context.Context, seed *kamajik8s.Client, a appOf, st *RollStatus, u rollUnit) error {
	live, err := seed.GetApplication(ctx, argoNamespace, a.name())
	if err != nil {
		return classify(CodeComponentUnhealthy, err)
	}
	if live == nil || !sameJSON(dig(live, "spec", "source"), dig(a.obj, "spec", "source")) || dig(live, "spec", "syncPolicy", "automated") == nil {
		if u.Machines > 0 && !slices.Contains(st.DeleteFirst, u.Key) {
			mem, what, err := readMemory(ctx, x.capi, x.in.ID)
			if err != nil {
				return transient(CodeCapacityShort, fmt.Errorf("could not read %s with %s: %w", what, pve.CAPITokenID, err))
			}
			if m := mem[u.Node]; m.free() < u.MemoryMiB {
				return permanent(CodeCapacityShort, "Step %d, %s: node %s can reserve %s more now (%s in the node, less %s configured for this platform's machines and %s for "+
					"the other VMs and containers), and the one more machine this step makes first needs %s. The plan found the room; it is gone since. Nothing of "+
					"this step was applied and every machine runs as before: free memory on the node, then Retry.",
					st.Step, u.Name, u.Node, gib(m.free()), gib(int(m.total>>20)), gib(int(m.own>>20)), gib(int(m.others>>20)), gib(u.MemoryMiB))
			}
		}
		if err := x.rolloutGate(ctx, seed, a, st.Step, u); err != nil {
			return err
		}
		if err := x.check(ctx); err != nil {
			return err
		}
		if err := seed.ApplyApplication(ctx, a.obj); err != nil {
			return classify(CodeComponentUnhealthy, err)
		}
		if live, err = seed.GetApplication(ctx, argoNamespace, a.name()); err != nil {
			return classify(CodeComponentUnhealthy, err)
		}
	}
	if state, detail, ok := appState(live); !ok && !lastSyncIsCurrent(live) && !onlyPruneLeft(live) {
		if _, err := x.resync(ctx, seed, a, live, false, CodeComponentUnhealthy, detail); err != nil {
			return err
		}
		return waiting(CodeComponentUnhealthy, "Step %d, %s: waiting for ArgoCD to sync %s at this step (%s). %s", st.Step, u.Name, a.name(), state, clip(detail))
	}
	return nil
}

// unitState reads the machines for the stage's items and says whether unit cur is done: its owner (the
// KubeadmControlPlane or the MachineDeployment) has seen its last change and counts as many replicas, up-to-date,
// ready and available as it asks for, every Machine of the unit is at the target version and Running, and the unit
// has no Machine more. A unit whose machines cannot be found is not done: the count must match. failed is a machine
// CAPMOX gave up on.
func (x *acq) unitState(ctx context.Context, seed *kamajik8s.Client, units []rollUnit, cur rollUnit, target string) (items []StageItem, done bool, note string, failed, err error) {
	machines, vmids, infra, err := x.machines(ctx, seed)
	if err != nil {
		return nil, false, "", nil, err
	}
	group, plural, kind := capiGroup, "machinedeployments", "MachineDeployment"
	if cur.Key == "" {
		group, plural, kind = kcpGroup, "kubeadmcontrolplanes", "KubeadmControlPlane"
	}
	owner, err := seed.GetCR(ctx, group, capiVersion, plural, ClusterName, cur.Owner)
	if err != nil && !kamajik8s.NotFound(err) {
		return nil, false, "", nil, classify(CodeSeedAPIUnreachable, err)
	}
	num := func(path ...string) int {
		if f, ok := dig(owner, path...).(float64); ok {
			return int(f)
		}
		return -1
	}
	want := num("spec", "replicas")
	settled := owner != nil && want >= 0 && num("metadata", "generation") >= 0 && num("metadata", "generation") == num("status", "observedGeneration")
	counts := ""
	for _, f := range []string{"replicas", "upToDateReplicas", "readyReplicas", "availableReplicas"} {
		n := max(num("status", f), 0) // Cluster API leaves a zero count out
		settled = settled && n == want
		counts += fmt.Sprintf(", %d %s", n, strings.TrimSuffix(f, "Replicas"))
	}
	conds := map[string]map[string]any{}
	for _, pm := range infra {
		conds[str(pm, "metadata", "name")] = cond(pm, vmProvisioned)
	}
	unitOf := func(m map[string]any) string {
		labels, _ := dig(m, "metadata", "labels").(map[string]any)
		if _, cp := labels["cluster.x-k8s.io/control-plane"]; cp {
			return units[0].Name
		}
		for _, u := range units[1:] {
			if labels["proxcloud.io/machine-deployment"] == u.Owner || labels["cluster.x-k8s.io/deployment-name"] == u.Owner {
				return u.Name
			}
		}
		return ""
	}
	mine, atTarget := 0, 0
	for _, m := range machines {
		name, phase, unit := str(m, "metadata", "name"), str(m, "status", "phase"), unitOf(m)
		at := strings.TrimPrefix(str(m, "spec", "version"), "v") == target
		deleting := dig(m, "metadata", "deletionTimestamp") != nil
		c := conds[str(m, "spec", "infrastructureRef", "name")]
		var step string
		switch d := cond(m, "Deleting"); {
		case deleting:
			step = strings.TrimSuffix("old machine being removed: "+str(d, "reason")+": "+oneLine(str(d, "message")), ": ")
			step = strings.TrimSuffix(step, ": ")
		case at && phase == "Running":
			step = "joined at " + target
		case at && str(c, "status") == "True":
			step = "cloned, joining"
		case at:
			step = strings.TrimSuffix("being cloned: "+str(c, "reason")+": "+str(c, "message"), ": ")
			step = strings.TrimSuffix(step, ": ")
		default:
			step = "at " + strings.TrimPrefix(str(m, "spec", "version"), "v") + ", not replaced yet"
		}
		items = append(items, StageItem{ID: name, State: phase, Message: clip(vmText(vmids[name]) + ", " + cmp.Or(unit, "no unit") + ": " + step)})
		if unit != cur.Name {
			continue
		}
		mine++
		if at && phase == "Running" && !deleting {
			atTarget++
		} else if note == "" || deleting { // the draining machine, with the pod that blocks it, says most
			note = name + " (" + vmText(vmids[name]) + "): " + step + "."
		}
		msg := str(m, "status", "failureMessage")
		if msg == "" && capmoxGaveUp(c) {
			msg = strings.TrimSuffix(str(c, "reason")+": "+str(c, "message"), ": ")
		}
		if (phase == "Failed" || msg != "") && failed == nil {
			items[len(items)-1].State = "Failed"
			still := "The machine it replaces still runs."
			if slices.Contains(rollOf(x.in).DeleteFirst, cur.Key) {
				still = "Its unit deletes the old machine first, so what ran on it stays away until a Retry succeeds."
			}
			failed = permanent(CodeMachineFailed, "Machine %s (%s) of %s failed: %s. CAPMOX does not try this machine again: correct the cause, delete the Machine on "+
				"the seed (kubectl -n %s delete machine %s; Cluster API makes a new one), then Retry. %s", name, vmText(vmids[name]), cur.Name, clip(msg), ClusterName, name, still)
		}
	}
	done = settled && mine == want && atTarget == want
	if owner == nil {
		counts = ", which the seed does not hold yet"
	}
	note = strings.TrimSpace(fmt.Sprintf("%d of %d machines are at %s and Running (%s %s asks for %d%s). %s", atTarget, max(want, 0), target, kind, cur.Owner, max(want, 0), counts, note))
	return items, done, note, failed, nil
}
