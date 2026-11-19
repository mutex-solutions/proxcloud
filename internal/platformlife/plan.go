package platformlife

// plan.go — GET …/plan (design §7): what an apply does to an installation, read-only, and for an installed platform
// on another BOM than this build's the plan of the move to it: every difference of the two BOMs with its class, where
// the run applies it, what it does to tenants, and the reason when this release refuses it. StartRun decides with the
// same function (move), so a plan and the 409 of a run never disagree. The live reason (an Application not Healthy)
// comes with the live read.

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
)

// Classes of a change in an upgrade plan.
const (
	ClassChart      = "chart"       // the run syncs a new chart revision
	ClassRecordOnly = "record only" // nothing of the platform is synced for it: the record and labels, or what new tenants get
	ClassRefused    = "refused"     // this release does not make the change: the whole upgrade is refused
	ClassSite       = "site"        // the run writes it on the Proxmox site, outside the platform's clusters
	ClassSeed       = "seed"        // the run replaces k3s on the running seed VM (stage seed-k3s)
)

// PlanChange is one difference: a component (Cluster and Name), a top-level pin (Name only), or a control plane the
// settings make larger than the one a run built (Cluster is the platform cluster's name, Name controlPlane.replicas,
// From and To the counts, with Stage and Impact), or the cluster firewall rule a run writes because Proxmox does not
// hold it (Name pve-api-rule, class site: Plan.ListAPIAccess). From "" is an install, To "" a removal. Class and Wave
// are set in an upgrade plan only, Stage and Impact there, on a count and on the rule.
type PlanChange struct {
	Cluster  string `json:"cluster,omitempty"`
	Name     string `json:"name"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Class    string `json:"class,omitempty"`
	Stage    string `json:"stage,omitempty"`  // the stage of the run that applies it
	Wave     *int   `json:"wave,omitempty"`   // the component's wave in that stage
	Impact   string `json:"impact,omitempty"` // what it restarts or rolls
	Blocking string `json:"blocking,omitempty"`
}

// Upgrade is the move of an installed platform from one BOM to another.
type Upgrade struct {
	From BOMRef `json:"from"`
	To   BOMRef `json:"to"`
	// Allowed: an apply that names To as its target is accepted. False with the reasons in Blocking.
	Allowed bool `json:"allowed"`
	// InProgress: a run toward To has started and has not succeeded yet (status.targetBom). Every apply continues to
	// To, without a new confirmation.
	InProgress bool `json:"inProgress,omitempty"`
	// Confirm is what the operator types to start it ("" when it is not allowed or already in progress).
	Confirm  string       `json:"confirm,omitempty"`
	Changes  []PlanChange `json:"changes"`
	Blocking []string     `json:"blocking"`
	// SeedBackup is the seed-backup row, read from Proxmox by the route when the move replaces the seed's k3s and has
	// not started. When it is not ok, Blocking carries seed_backup_missing: the one reason the operator can answer
	// (seedBackupTaken on the apply), so Allowed and Confirm stay as the BOMs decide them.
	SeedBackup *Check `json:"seedBackup,omitempty"`
}

// MovesSeed reports a move that replaces the seed's k3s.
func (u *Upgrade) MovesSeed() bool {
	return u != nil && slices.ContainsFunc(u.Changes, func(c PlanChange) bool { return c.Class == ClassSeed })
}

// SeedBackupMissing is the 409 seed_backup_missing of an apply that starts an upgrade which replaces the seed's k3s:
// the portal saw no backup of the seed VM younger than SeedBackupAge, and the apply does not say one was taken.
type SeedBackupMissing struct{ VMID int }

func (e *SeedBackupMissing) Error() string {
	return fmt.Sprintf(CodeSeedBackupMissing+": no backup of seed VM %d younger than 24 hours is visible to the portal. "+
		"Take one (vzdump %d), or confirm that you took one the portal cannot see.", e.VMID, e.VMID)
}

// NeedSeedBackup adds the seed-backup row to the moves of p that replace the seed's k3s and have not started, and
// seed_backup_missing to their Blocking when the row is not ok. read is called once, and only when a move needs it.
func (p *Plan) NeedSeedBackup(in *Installation, read func() Check) {
	var row *Check
	for _, u := range []*Upgrade{p.Upgrade, p.Replacement} {
		if !u.MovesSeed() || u.InProgress || in.Status.Seed == nil {
			continue
		}
		if row == nil {
			c := read()
			row = &c
		}
		if u.SeedBackup = row; row.Status != CheckOK {
			u.Blocking = append(u.Blocking, (&SeedBackupMissing{VMID: in.Status.Seed.VMID}).Error())
		}
	}
}

// Plan is the answer of GET …/plan.
type Plan struct {
	Installed *BOMRef      `json:"installed"`       // nil: nothing installed yet
	Target    BOMRef       `json:"target"`          // the BOM an apply without a target runs with
	Newer     *BOMRef      `json:"newer,omitempty"` // this build's own BOM, when the installed platform is on another one
	Changes   []PlanChange `json:"changes"`         // what an apply without a target changes: every component of a new installation; of an installed one, a control plane count above the one a run built
	Blocking  []string     `json:"blocking"`
	// Upgrade is the move to this build's BOM (or to ?target=), or the one in progress. nil: there is none to make.
	Upgrade *Upgrade `json:"upgrade,omitempty"`
	// Replacement is set next to an Upgrade in progress whose target is not this build's BOM: the move to this build's
	// BOM in its place (the way out when the unfinished target cannot be reached). An apply that names its To starts it.
	Replacement *Upgrade `json:"replacement,omitempty"`
}

// UpgradeRefused is the 409 of an apply that names a target this release does not move the platform to.
type UpgradeRefused struct{ Reasons []string }

func (e *UpgradeRefused) Error() string {
	return CodeUpgradeRefused + ": " + strings.Join(e.Reasons, "; ")
}

func refOf(b Embedded) BOMRef { return BOMRef{Release: b.Release, SHA256: b.SHA256} }

// PlanFor is what an apply does to in. A new installation gets this build's BOM. An installed platform stays on its
// BOM unless the apply names a target: Target is the installed BOM, Newer this build's when it is another one, and
// Upgrade the plan of the move to it (or to target, the sha256 of a BOM this build embeds). While an upgrade is in
// progress Target is its target and Upgrade that move: no apply goes back. When that target is the BOM of an earlier
// build, Replacement is the move to this build's. An installed BOM this build does not embed blocks with
// bom_unsupported. Changes of an installed platform are the control plane counts above what a run built; whether the
// new machines fit needs a read of Proxmox, which the route adds to Blocking (Preflight.GrowthBlocking).
func PlanFor(in *Installation, target string) (Plan, error) {
	cur, err := Current()
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Installed: in.Status.BOM, Target: refOf(cur), Changes: []PlanChange{}, Blocking: []string{}}
	if in.Status.BOM == nil {
		if target != "" && target != cur.SHA256 {
			return p, ErrBadTarget
		}
		for _, c := range cur.Components {
			p.Changes = append(p.Changes, PlanChange{Cluster: c.Cluster, Name: c.Name, To: c.Chart.Version})
		}
		return p, nil
	}
	// The settings of an installed platform against what a run built: the BOMs say nothing about the machines.
	for i, c := range in.Spec.Clusters {
		if from, to := in.controlPlaneGrowth(i); to > 0 {
			p.Changes = append(p.Changes, PlanChange{Cluster: in.Status.Clusters[i].Name, Name: "controlPlane.replicas", From: strconv.Itoa(from), To: strconv.Itoa(to),
				Stage: stageCluster, Impact: fmt.Sprintf("The apply adds %d control plane machines on node %s.", to-from, c.ControlPlane.Node)})
		}
	}
	if _, ok := Lookup(in.Status.BOM.SHA256); !ok {
		p.Blocking = append(p.Blocking, ErrBOMNotHere.Error())
		return p, nil
	}
	if in.Status.BOM.SHA256 != cur.SHA256 {
		newer := refOf(cur)
		p.Newer = &newer
	}
	// The move an apply would make by default, then the one asked for or on offer.
	run, _, err := move(in, "")
	if err != nil {
		p.Target = *in.Status.BOM
		p.Blocking = append(p.Blocking, err.Error())
		return p, nil
	}
	p.Target = refOf(run)
	if target == "" && in.Status.TargetBOM == nil {
		target = cur.SHA256
	}
	if _, p.Upgrade, err = move(in, target); err != nil {
		return p, err
	}
	if pending := in.Status.TargetBOM; target == "" && pending != nil && pending.SHA256 != cur.SHA256 && in.Status.BOM.SHA256 != cur.SHA256 {
		if _, p.Replacement, err = move(in, cur.SHA256); err != nil {
			return p, err
		}
	}
	return p, nil
}

// move decides the BOM an apply of in runs with. target is the sha256 the apply names ("" = none): without one an
// apply stays on the installed BOM, or continues to status.targetBom when an upgrade has started. up is nil when the
// run stays on the installed BOM; otherwise it is the plan of the move, which the caller starts only when up.Allowed.
//
// A platform moves only to this build's own BOM. After a run toward a target has started, an apply goes to that target
// (Retry), or to this build's BOM when a later release replaces a target that cannot be reached: then both the
// installed BOM and the unfinished target must be allowed to move to it, because each component stands at one of the two.
func move(in *Installation, target string) (run Embedded, up *Upgrade, err error) {
	cur, err := Current()
	if err != nil {
		return Embedded{}, nil, err
	}
	if in.Status.BOM == nil {
		if target != "" && target != cur.SHA256 {
			return Embedded{}, nil, ErrBadTarget
		}
		return cur, nil, nil
	}
	from, ok := Lookup(in.Status.BOM.SHA256)
	if !ok {
		return Embedded{}, nil, ErrBOMNotHere
	}
	pending := in.Status.TargetBOM
	want := target
	switch {
	case want == "" && pending != nil:
		want = pending.SHA256
	case want == "":
		return from, nil, nil
	case want == from.SHA256 && pending == nil:
		return from, nil, nil
	case want == from.SHA256:
		return Embedded{}, nil, &UpgradeRefused{Reasons: []string{fmt.Sprintf("an upgrade to release %s has started and an apply continues to it: "+
			"a platform does not go back to the BOM it left", pending.Release)}}
	}
	to, ok := Lookup(want)
	switch {
	case !ok && target == "":
		return Embedded{}, nil, ErrBOMNotHere
	case !ok || (want != cur.SHA256 && (pending == nil || want != pending.SHA256)):
		return Embedded{}, nil, ErrBadTarget
	}
	u := &Upgrade{From: refOf(from), To: refOf(to), Changes: Classify(from.BOM, to.BOM), Blocking: []string{}}
	if strings.HasSuffix(to.Release, "-dev") {
		u.Blocking = append(u.Blocking, fmt.Sprintf("release %s is a development BOM: no later build embeds it, so a platform moved to it could be applied by "+
			"this build alone. Only the BOM of a release is a target (docs/ci-release.md, \"Upgrade drill builds\")", to.Release))
	}
	for _, c := range u.Changes {
		if c.Blocking != "" {
			u.Blocking = append(u.Blocking, strings.TrimPrefix(c.Cluster+"/", "/")+c.Name+": "+c.Blocking)
		}
	}
	switch {
	case pending != nil && pending.SHA256 == to.SHA256:
		// The build that started it allowed it, and going on is the only way: nothing refuses a Retry.
		u.InProgress, u.Blocking = true, []string{}
	case pending != nil:
		mid, ok := Lookup(pending.SHA256)
		if !ok {
			u.Blocking = append(u.Blocking, fmt.Sprintf("the unfinished upgrade's target %s is not a BOM this build embeds", pending.Release))
			break
		}
		for _, c := range Classify(mid.BOM, to.BOM) {
			if c.Blocking != "" {
				u.Blocking = append(u.Blocking, fmt.Sprintf("from the unfinished target %s: %s%s: %s", pending.Release, strings.TrimPrefix(c.Cluster+"/", "/"), c.Name, c.Blocking))
			}
		}
	}
	if u.Allowed = len(u.Blocking) == 0; u.Allowed && !u.InProgress {
		u.Confirm = "upgrade " + in.Name + " to " + to.Release
	}
	return to, u, nil
}

// later ends the reason of every change this release does not make.
const later = "not supported by this release: "

// k3sVersion splits "v1.35.10+k3s1" into major, minor, patch and the k3s build.
func k3sVersion(v string) (n [4]int, err error) {
	core, build, ok := strings.Cut(v, "+k3s")
	if !ok {
		return n, fmt.Errorf("k3s version %q: want vMAJOR.MINOR.PATCH+k3sN", v)
	}
	if n[0], n[1], n[2], err = kamaji.ParseVersion(core); err == nil {
		n[3], err = strconv.Atoi(build)
	}
	return n, err
}

// k3sStep is why k3s does not move from version from to version to ("" when it does): never back, and at most one
// minor up. The same version is no step; Classify and the stage decide that case before they ask.
func k3sStep(from, to string) string {
	f, err := k3sVersion(from)
	if err != nil {
		return err.Error()
	}
	t, err := k3sVersion(to)
	switch {
	case err != nil:
		return err.Error()
	case slices.Compare(t[:], f[:]) < 0:
		return "k3s never goes back"
	case t[0] != f[0] || t[1] > f[1]+1:
		return fmt.Sprintf("one minor at a time: go through %d.%d first", f[0], f[1]+1)
	}
	return ""
}

// slug writes a version for a name: "v1.35.10+k3s1" as "v1.35.10-k3s1".
func slug(v string) string { return strings.ReplaceAll(v, "+", "-") }

// Classify lists every difference between two BOMs for a move from from to to, each with its class. This release moves
// a platform only when the BOMs differ in the product's own charts: the release line, the tenant charts, and the charts
// of our own repository (proxcloud-platform as capi, capi-tenant and platform; proxmox-kamaji-cluster as pc-platform)
// at a version whose entry lists the old one in upgradeFrom; and in the seed's k3s, by a patch, a k3s build or one
// minor up (class seed, with what the run writes on the seed as class site). Everything else is refused with its
// reason: the Kubernetes version, the node and seed images, kube-vip, clusterctl and the providers, every third-party
// chart, and a component that is added, removed or changed in anything but its chart version.
func Classify(from, to *BOM) []PlanChange {
	out := []PlanChange{}
	pin := func(name, f, t, class, text string) {
		if f == t {
			return
		}
		c := PlanChange{Name: name, From: f, To: t, Class: class, Impact: text}
		if class == ClassRefused {
			c.Impact, c.Blocking = "", later+text
		}
		out = append(out, c)
	}
	image := func(im VMImage) string { return im.Package + ":" + im.Tag + "@" + im.Digest }
	chart := func(c Chart) string { return c.Repo + "/" + c.Name + ":" + c.Version + "@" + c.Digest }
	k3s := func(k K3s) string {
		return k.Version + " (binary " + k.Binary.SHA256 + " " + k.Binary.URL + ", install script " + k.InstallScript.SHA256 + " " + k.InstallScript.URL + ")"
	}
	pin("release", from.Release, to.Release, ClassRecordOnly,
		"Nothing is synced for it: the platform's record and the labels of its objects name the new release.")
	pin("kubernetes.version", from.Kubernetes.Version, to.Kubernetes.Version, ClassRefused,
		"needs the Kubernetes upgrade of a later release (every machine of the platform cluster is replaced)")
	pin("kubernetes.nodeImage", image(from.Kubernetes.NodeImage), image(to.Kubernetes.NodeImage), ClassRefused,
		"a new node image replaces every machine of the platform cluster, which needs the Kubernetes upgrade of a later release")
	pin("seed.image", image(from.Seed.Image), image(to.Seed.Image), ClassRefused,
		"only a rebuilt seed would boot it, and a ready seed is never rebuilt")
	switch fk, tk := from.Seed.K3s, to.Seed.K3s; {
	case fk.Version == tk.Version:
		pin("seed.k3s", k3s(fk), k3s(tk), ClassRefused, "the same k3s version with another sha256 or URL: a published k3s version does not change")
	case k3sStep(fk.Version, tk.Version) != "":
		pin("seed.k3s", k3s(fk), k3s(tk), ClassRefused, k3sStep(fk.Version, tk.Version))
	default:
		out = append(out, PlanChange{Name: "seed.k3s", From: fk.Version, To: tk.Version, Class: ClassSeed, Stage: stageSeedK3s,
			Impact: "The k3s binary on the seed VM is replaced and k3s is restarted. The seed's API is away for about a minute. Meanwhile only " +
				"reconciliation stops: Cluster API replaces or repairs no machine, the seed's ArgoCD syncs none of the platform's own components, and no " +
				"platform run goes on. The platform cluster, every tenant cluster, the tenant API servers and the databases keep serving. ArgoCD and " +
				"Cluster API on the seed keep their state: etcd on the seed's disk is not touched."})
		for _, w := range [][3]string{
			{"seed-k3s-job", "Job " + SeedPortalNamespace + "/" + k3sJobName(tk.Version) + " on the seed",
				"A privileged Job on the seed downloads the binary, checks its sha256 and restarts k3s. The run deletes the Job after it has read its result."},
			{"seed-etcd-snapshot", k3sSnapshots + "/" + k3sSnapshotName(fk.Version) + "-<node>-<time>",
				"The Job first saves a snapshot of the seed's etcd (k3s etcd-snapshot save). It is kept: k3s's own snapshot retention applies."},
			{"seed-k3s-binary", "/usr/local/bin/k3s, the previous binary kept as /usr/local/bin/k3s.prev",
				"The binary is replaced only after the download passed its sha256. The install script is not run again: the unit and /etc/rancher/k3s/config.yaml stay."},
		} {
			out = append(out, PlanChange{Name: w[0], To: w[1], Class: ClassSite, Stage: stageSeedK3s, Impact: w[2]})
		}
	}
	pin("images.kubeVip", from.Images.KubeVip, to.Images.KubeVip, ClassRefused,
		"kube-vip is in the control plane's machine spec, so the control plane machine would be replaced; it needs the Kubernetes upgrade of a later release")
	pin("images.clusterctl", from.Images.Clusterctl, to.Images.Clusterctl, ClassRefused,
		"the Cluster API providers are upgraded (clusterctl upgrade) only by a later release")
	for _, side := range []struct {
		name string
		f, t Providers
	}{{"capi.seed", from.CAPI.Seed, to.CAPI.Seed}, {"capi.workload", from.CAPI.Workload, to.CAPI.Workload}} {
		for _, p := range [][3]string{{"core", side.f.Core, side.t.Core}, {"bootstrap", side.f.Bootstrap, side.t.Bootstrap},
			{"controlPlane", side.f.ControlPlane, side.t.ControlPlane}, {"infrastructure", side.f.Infrastructure, side.t.Infrastructure},
			{"ipam", side.f.IPAM, side.t.IPAM}, {"addon", side.f.Addon, side.t.Addon}} {
			pin(side.name+"."+p[0], p[1], p[2], ClassRefused, "the Cluster API providers are upgraded (clusterctl upgrade) only by a later release")
		}
	}
	for _, tc := range [][3]string{{"tenantCharts.kubernetes", chart(from.TenantCharts.Kubernetes), chart(to.TenantCharts.Kubernetes)},
		{"tenantCharts.database", chart(from.TenantCharts.Database), chart(to.TenantCharts.Database)}} {
		pin(tc[0], tc[1], tc[2], ClassRecordOnly,
			"The run writes the chart on the provider: new tenants get it. An existing tenant keeps its chart until its own chart upgrade.")
	}

	find := func(b *BOM, cluster, name string) *Component {
		for i := range b.Components {
			if c := &b.Components[i]; c.Cluster == cluster && c.Name == name {
				return c
			}
		}
		return nil
	}
	refuse := func(c *Component, fromV, toV, why string) {
		out = append(out, PlanChange{Cluster: c.Cluster, Name: c.Name, From: fromV, To: toV, Class: ClassRefused, Blocking: later + why})
	}
	for i := range to.Components {
		t := &to.Components[i]
		f := find(from, t.Cluster, t.Name)
		switch {
		case f == nil:
			refuse(t, "", t.Chart.Version, "a component is added to an installed platform only by a later release")
		case f.Wave != t.Wave || f.Install != t.Install || f.Namespace != t.Namespace || f.Group != t.Group || f.Stateful != t.Stateful ||
			f.Chart.Repo != t.Chart.Repo || f.Chart.Name != t.Chart.Name || !reflect.DeepEqual(f.Values, t.Values):
			refuse(t, f.Chart.Version, t.Chart.Version, "the component's wave, install mode, namespace, node group, chart source or values changed, and a run moves only a chart version")
		case f.Chart.Version == t.Chart.Version && f.Chart.Digest == t.Chart.Digest:
		case !ownPackage(t.Chart.Repo):
			refuse(t, f.Chart.Version+"@"+f.Chart.Digest, t.Chart.Version+"@"+t.Chart.Digest,
				"a third-party chart is upgraded only by a later release (this one moves the product's own charts)")
		case f.Chart.Version == t.Chart.Version:
			refuse(t, f.Chart.Digest, t.Chart.Digest, "the same chart version with another digest: a published chart version does not change")
		case !slices.Contains(t.UpgradeFrom, f.Chart.Version):
			refuse(t, f.Chart.Version, t.Chart.Version, fmt.Sprintf("version %s is not in upgradeFrom of %s: the release did not check this move", f.Chart.Version, t.Chart.Version))
		default:
			wave := t.Wave
			c := PlanChange{Cluster: t.Cluster, Name: t.Name, From: f.Chart.Version, To: t.Chart.Version, Class: ClassChart, Stage: stageSeedPlatform, Wave: &wave,
				Impact: "The Application moves to the new chart version. ArgoCD syncs only the objects the new chart renders differently, and starts no sync when " +
					"there is none. No pod of a tenant restarts."}
			switch {
			case t.Cluster == ClusterSeed && t.Name == ClusterName:
				c.Stage = stageCluster
				c.Impact = "The Application moves to the new chart version. ArgoCD syncs only the Cluster API objects of the platform cluster that the new chart " +
					"renders differently. Before any sync the run checks that no machine or bootstrap template is new " +
					"and that the control plane is unchanged, and fails with " + CodeUnexpectedRollout + " otherwise: no machine is replaced."
			case t.Cluster == ClusterWorkload:
				c.Stage = stageWorkloadPlatform
			}
			if _, hook := clusterctlProviders(to, *t); hook {
				c.Impact = "The Application moves to the new chart version. ArgoCD syncs, and runs the chart's clusterctl hook again, only when the new chart renders " +
					"an object differently; the hook then finds every provider at its version and changes nothing. No pod of a tenant restarts."
			}
			out = append(out, c)
		}
	}
	for i := range from.Components {
		if f := &from.Components[i]; find(to, f.Cluster, f.Name) == nil {
			refuse(f, f.Chart.Version, "", "a component is removed from an installed platform only by a later release")
		}
	}
	return out
}
