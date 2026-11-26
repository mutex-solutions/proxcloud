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
	ClassMachines   = "machines"    // the run replaces machines of the platform cluster, one unit at a time (stage cluster, roll.go)
	ClassAdded      = "added"       // a component the target BOM adds, rendered only for settings that ask for its feature (Component.Feature)
)

// PlanChange is one difference: a component (Cluster and Name), a top-level pin (Name only), or a control plane the
// settings make larger than the one a run built (Cluster is the platform cluster's name, Name controlPlane.replicas,
// From and To the counts, with Stage and Impact), or the cluster firewall rule a run writes because Proxmox does not
// hold it (Name pve-api-rule, class site: Plan.ListAPIAccess), or the address of the platform cluster's ArgoCD (Name
// access.argocd, class chart: From and To the URLs). From "" is an install, To "" a removal. Class and Wave
// are set in an upgrade plan only (Class also on the address), Stage and Impact there, on a count, on the rule and on
// the address.
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
	// Carried is set with InProgress when To took the place of the unfinished target of an earlier build (move,
	// carries): the BOM the upgrade was started for. The next apply records To as the target.
	Carried *BOMRef `json:"carried,omitempty"`
	// Confirm is what the operator types to start it ("" when it is not allowed or already in progress).
	Confirm  string       `json:"confirm,omitempty"`
	Changes  []PlanChange `json:"changes"`
	Blocking []string     `json:"blocking"`
	// SeedBackup is the seed-backup row, read from Proxmox by the route when the move replaces the seed's k3s and has
	// not started. When it is not ok, Blocking carries seed_backup_missing: the one reason the operator can answer
	// (seedBackupTaken on the apply), so Allowed and Confirm stay as the BOMs decide them.
	SeedBackup *Check `json:"seedBackup,omitempty"`
	// Parts are set on a move that replaces the platform cluster's machines: its two operator actions, "seed" then
	// "platform". Allowed, Confirm and Blocking above are then those of the platform part (the apply without a part).
	Parts []UpgradePart `json:"parts,omitempty"`
}

// MovesSeed reports a move that replaces the seed's k3s.
func (u *Upgrade) MovesSeed() bool {
	return u != nil && slices.ContainsFunc(u.Changes, func(c PlanChange) bool { return c.Class == ClassSeed })
}

// SeedBackupMissing is the 409 seed_backup_missing of an apply that starts an upgrade which replaces the seed's k3s:
// the portal saw no backup of the seed VM younger than SeedBackupAge, and the apply does not say one was taken. On
// most sites the portal cannot see one (SeedBackupCheck), so the operator's word is the usual answer: the sentence
// says what to check first.
type SeedBackupMissing struct{ VMID int }

func (e *SeedBackupMissing) Error() string {
	return fmt.Sprintf(CodeSeedBackupMissing+": the portal saw no backup of seed VM %d younger than 24 hours, and on most stores it cannot see one (the seed-backup row says where). "+
		"Check in Proxmox that there is an archive of this seed VM, made after the seed VM was created and younger than 24 hours (vzdump %d makes one), then confirm it (seedBackupTaken).", e.VMID, e.VMID)
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
			missing := (&SeedBackupMissing{VMID: in.Status.Seed.VMID}).Error()
			u.Blocking = append(u.Blocking, missing)
			for i := range u.Parts { // either part starts the upgrade
				u.Parts[i].Blocking = append(u.Parts[i].Blocking, missing)
			}
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
// bom_unsupported. Changes of an installed platform are the control plane counts above what a run built (whether the
// new machines fit needs a read of Proxmox, which the route adds to Blocking: Preflight.GrowthBlocking) and the
// address of the platform cluster's ArgoCD where the settings differ from what a run built (argoAccessChange).
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
		for _, c := range cur.BOM.For(in.Spec).Components {
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
	if c := in.argoAccessChange(); c != nil {
		p.Changes = append(p.Changes, *c)
	}
	for _, c := range in.sandboxChanges() {
		if p.Changes = append(p.Changes, c); c.Blocking != "" {
			p.Blocking = append(p.Blocking, c.Name+": "+c.Blocking)
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
	// Not when this build's BOM already took the unfinished target's place (Upgrade.Carried): that is the same move.
	if pending := in.Status.TargetBOM; target == "" && pending != nil && pending.SHA256 != cur.SHA256 && in.Status.BOM.SHA256 != cur.SHA256 &&
		(p.Upgrade == nil || p.Upgrade.Carried == nil) {
		if _, p.Replacement, err = move(in, cur.SHA256); err != nil {
			return p, err
		}
	}
	return p, nil
}

// argoAccessChange is the plan row access.argocd of an installed platform: spec.access.argocd against what a run built
// (status.argocdUrl and status.argocdSources). nil when they agree.
func (in *Installation) argoAccessChange() *PlanChange {
	a, st := in.Spec.Access.ArgoCD, in.Status
	c := &PlanChange{Name: "access.argocd", From: st.ArgoCDURL, Class: ClassChart, Stage: stageWorkloadPlatform}
	if len(in.Status.Clusters) > 0 {
		c.Cluster = in.Status.Clusters[0].Name
	}
	const restart = "The platform cluster's ArgoCD restarts: tenant clusters and databases keep running, their status in the consoles is stale for the restart. "
	built := st.ArgoCDURL != "" || len(st.ArgoCDSources) > 0 // a run rendered the block, with or without getting the address
	switch {
	case a == nil && !built:
		return nil
	case a == nil:
		c.Impact = restart + "MetalLB no longer announces the address and ArgoCD answers inside the cluster only, over plain HTTP, as before the address was set."
	case st.ArgoCDURL == a.URL() && slices.Equal(st.ArgoCDSources, a.Sources):
		return nil
	default:
		c.To = a.URL()
		c.Impact = fmt.Sprintf("MetalLB announces %s on the platform network. Only %s are let in. The certificate is self-signed (ArgoCD's own): the browser warns once. "+
			"No password is set or shown: sign in as admin with the password read through the platform kubeconfig.", a.Address, strings.Join(a.Sources, ", "))
		if !built {
			c.Impact = restart + c.Impact + " If the address is taken the run fails with " + CodeArgoCDAddressUnavailable + " and the platform keeps serving."
		} else {
			c.Impact = "ArgoCD does not restart. " + c.Impact
		}
	}
	return c
}

// move decides the BOM an apply of in runs with. target is the sha256 the apply names ("" = none): without one an
// apply stays on the installed BOM, or continues to status.targetBom when an upgrade has started. up is nil when the
// run stays on the installed BOM; otherwise it is the plan of the move, which the caller starts only when up.Allowed.
//
// A platform moves only to this build's own BOM. After a run toward a target has started, an apply goes to that target
// (Retry), or to this build's BOM when a later release replaces a target that cannot be reached: then both the
// installed BOM and the unfinished target must be allowed to move to it, because each component stands at one of the two.
//
// While the platform cluster's machines are being replaced (status.clusters[0].roll at a step that still names a
// unit), a later build whose BOM replaces no machine and no k3s against the unfinished target (carries) takes the
// target's place by itself: an apply without a target, and one that names this build's BOM, continue under this
// build's BOM as the upgrade in progress (Upgrade.Carried names the target it replaced), with nothing to confirm, and
// the installation's target becomes this build's. The roll's step is kept: the units already replaced render as they
// are (same version, same node image), the units not reached render with this build's cluster chart. That is how a
// site whose roll stands at a defect of the target's own chart or of the build that started it gets out with a Retry
// from the corrected build. A later build that would replace machines or the seed's k3s again is not a target while
// the roll runs: an apply without a target goes on to the unfinished target, and one that names the build's BOM is
// refused. Before the roll has begun, and once its last unit is recorded (only the record is left to write, and the
// Application must carry exactly the render the roll was made with), nothing is carried: an apply without a target
// goes on to the unfinished target, and this build's BOM takes its place only when named and confirmed.
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
	// In a roll that still has a unit to replace, a build whose BOM differs from the unfinished target only in the
	// product's own charts and the release line takes the target's place (carries).
	carried := false
	if roll := rollOf(in); roll != nil && len(in.Spec.Clusters) > 0 && roll.Step <= len(rollUnits(in.Spec.platformCluster())) && pending != nil && pending.SHA256 != cur.SHA256 && (target == "" || target == cur.SHA256) {
		if mid, ok := Lookup(pending.SHA256); ok && carries(from, mid, cur) {
			want, carried = cur.SHA256, true
		}
	}
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
	u := &Upgrade{From: refOf(from), To: refOf(to), Changes: in.featureRows(Classify(from.BOM, to.BOM), to.BOM), Blocking: []string{}}
	if strings.HasSuffix(to.Release, "-dev") {
		u.Blocking = append(u.Blocking, fmt.Sprintf("release %s is a development BOM: no later build embeds it, so a platform moved to it could be applied by "+
			"this build alone. Only the BOM of a release is a target (docs/ci-release.md, \"Upgrade drill builds\")", to.Release))
	}
	for _, c := range u.Changes {
		if c.Blocking != "" {
			u.Blocking = append(u.Blocking, strings.TrimPrefix(c.Cluster+"/", "/")+c.Name+": "+c.Blocking)
		}
	}
	machines := hasMachines(u.Changes)
	if machines && in.Spec.NodeImage.Mode != ImageBOM {
		u.Blocking = append(u.Blocking, "kubernetes.nodeImage: "+later+"this installation names its own node templates; the roll needs the BOM's image")
	}
	// The separate sandbox cluster is a tenant-style cluster, and no run upgrades one in place. A move that replaces the
	// platform's machines is refused while it exists or is asked for: it would leave pc-sandbox on the version it left.
	if built := in.Status.Sandbox; machines && (in.Spec.sandboxMode() == SandboxSeparate || (built != nil && built.Placement == SandboxSeparate)) {
		u.Blocking = append(u.Blocking, "spec.sandbox: "+later+"the sandbox cluster "+SandboxClusterName+" is not upgraded in place, and this upgrade changes the Kubernetes version or the node image. "+
			"Set spec.sandbox.placement off and apply (every sandbox ends, the cluster and its machines are deleted), make this upgrade, then set placement separate again: the cluster is built new on the new version."+
			in.functionsStop())
	}
	// A roll renders every unit it has not reached at the version and the templates the platform left: a group that
	// does not exist yet has neither. The group is added by an apply before the upgrade or after it.
	if machines && in.Spec.sandboxMode() == SandboxShared && (in.Status.Sandbox == nil || in.Status.Sandbox.BuiltAt == nil) {
		u.Blocking = append(u.Blocking, "spec.sandbox: "+later+"the node group sandbox is not built yet, and this upgrade replaces the platform cluster's machines. "+
			"Apply the settings on the installed release first (that builds the group), or leave spec.sandbox out until the upgrade is done")
	}
	switch {
	case pending != nil && (pending.SHA256 == to.SHA256 || carried):
		// The build that started it allowed it, and going on is the only way: nothing refuses a Retry.
		u.InProgress, u.Blocking = true, []string{}
		if carried {
			u.Carried = pending
		}
	case pending != nil:
		mid, ok := Lookup(pending.SHA256)
		if !ok {
			u.Blocking = append(u.Blocking, fmt.Sprintf("the unfinished upgrade's target %s is not a BOM this build embeds", pending.Release))
			break
		}
		// A roll that has begun was ordered for one Kubernetes version and node image: the record names one step and
		// one target, so a BOM that replaces machines again cannot take the unfinished target's place.
		if roll := rollOf(in); roll != nil {
			var again []string
			for _, c := range Classify(mid.BOM, to.BOM) {
				if c.Class == ClassMachines || c.Class == ClassSeed {
					again = append(again, c.Name)
				}
			}
			if len(again) > 0 {
				u.Blocking = append(u.Blocking, fmt.Sprintf("the replacement of the platform cluster's machines for release %s has begun (step %d) and release %s "+
					"changes %s again: the machines already replaced would be replaced a second time. Finish the upgrade to %s first: an apply without a target "+
					"(Retry) goes on with it, and this build embeds its BOM. Then start the upgrade to %s",
					pending.Release, roll.Step, to.Release, strings.Join(again, ", "), pending.Release, to.Release))
			}
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
	if machines && len(in.Spec.Clusters) > 0 {
		u.setParts(in, from.BOM, to.BOM)
	}
	return to, u, nil
}

// carries reports that this build's BOM cur takes the place of the unfinished target mid of an upgrade from from: the
// two name the same Kubernetes version, node image, kube-vip, k3s, seed image, providers and third-party charts but
// cert-manager, and differ only in what a run moves without replacing a machine (the release line, the tenant charts,
// the product's own charts and cert-manager at a version that lists the other in upgradeFrom), and the installed BOM
// may move to cur as well, because each component stands at one of the two. A cur that moves cert-manager carries no
// unfinished roll or k3s move: the move from the installed BOM then has machines or seed rows, and Classify's pair gate
// refuses cert-manager's. Then nothing a run of the unfinished upgrade has reached is rendered otherwise under cur: the
// seed's k3s, the node image's templates and the roll's step keep their meaning, and the units the roll has not reached
// render with cur's cluster chart. A development BOM is never a target, so it carries nothing.
func carries(from, mid, cur Embedded) bool {
	if strings.HasSuffix(cur.Release, "-dev") {
		return false
	}
	for _, c := range Classify(from.BOM, cur.BOM) {
		if c.Blocking != "" {
			return false
		}
	}
	for _, c := range Classify(mid.BOM, cur.BOM) {
		if c.Blocking != "" || (c.Class != ClassRecordOnly && c.Class != ClassChart && c.Class != ClassAdded) {
			return false
		}
	}
	return true
}

// later ends the reason of every change this release does not make.
const later = "not supported by this release: "

// thirdPartyMove reports a third-party component an upgrade may move by its chart version, under the rules of the
// product's own charts (a version whose entry lists the installed one in upgradeFrom: the release read that pair's
// upgrade notes and compared the render, TestCertManagerMoveRenders) and only in a pair with no seed or machines row.
// cert-manager alone, by its component and its chart name: the case before it in Classify holds repo, chart name,
// values, wave and namespace equal to the installed entry.
func thirdPartyMove(c *Component) bool {
	return c.Name == "cert-manager" && c.Chart.Name == "cert-manager"
}

// certManagerImpact is the impact of a cert-manager row.
const certManagerImpact = "cert-manager moves to the new chart version. ArgoCD applies its CRDs in place (v1 stays the only version) and removes what " +
	"the new chart no longer renders. Each of the controller, cainjector and webhook Deployments starts its new pod and removes the old one only " +
	"when the new one is Ready; the three roll together. The wave waits until ArgoCD reports the three Deployments rolled (Healthy against the " +
	"new chart) and the sync, the chart's startupapicheck Job included, Succeeded. Certificates and their Secrets stay. On a lab (one sample) " +
	"the run took 3 minutes 8 seconds, about a minute and a half per cluster, no Certificate was issued again and no webhook refused a request; " +
	"a one-node seed's API was slow for about half a minute while it pulled the new images. The webhooks of Cluster API and Kamaji keep their " +
	"certificates. No pod of a tenant restarts, no machine is replaced and nothing " +
	"is written on Proxmox, so no seed backup is needed. There is no way back: a platform does not go back to a BOM, and cert-manager documents no " +
	"downgrade; its upgrade guide advises a backup of the cert-manager resources first (kubectl get certificates,issuers,clusterissuers -A -o yaml " +
	"on the seed and on the platform cluster)."

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
// at a version whose entry lists the old one in upgradeFrom; in cert-manager (thirdPartyMove) by the same rule, but
// only in a pair that changes no k3s, Kubernetes version or node image; and in the seed's k3s, by a patch, a k3s build
// or one minor up (class seed, with what the run writes on the seed as class site). Everything else is refused with its
// reason: the seed image, clusterctl and the providers, every other third-party chart, and a component that is added,
// removed or changed in anything but its chart version. The Kubernetes version (never back, at most one minor up), the
// node image and, together with a new node image, kube-vip are class machines when the target's cluster chart can
// order a rollout: the run then replaces every machine of the platform cluster (roll.go).
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
	// The three pins in every machine's spec. A new node image replaces every machine of the platform cluster, and so
	// does a new Kubernetes version (which always comes with its image: BOM.Validate ties the image's tag to the
	// version). The run orders it (roll.go), which needs the cluster chart that can hold a MachineDeployment back.
	const rolls = "Every machine of the platform cluster is replaced by Cluster API, the control plane first, then one node group and node at a time. " +
		"The rows roll/… say per unit what stops serving meanwhile."
	newImage := image(from.Kubernetes.NodeImage) != image(to.Kubernetes.NodeImage)
	machines := func(name, f, t, refusal string) {
		chartV := ClusterChartVersion(to)
		switch {
		case refusal != "":
			pin(name, f, t, ClassRefused, refusal)
		case !kamaji.ChartHasRollOrder(chartV):
			pin(name, f, t, ClassRefused, fmt.Sprintf("the BOM names cluster chart %s, which cannot order a rollout (it needs %s or newer)", chartV, kamaji.RollOrderChartVersion))
		case f != t:
			out = append(out, PlanChange{Name: name, From: f, To: t, Class: ClassMachines, Stage: stageCluster, Impact: rolls})
		}
	}
	step := ""
	if from.Kubernetes.Version != to.Kubernetes.Version {
		step = k8sStep(from.Kubernetes.Version, to.Kubernetes.Version)
	}
	machines("kubernetes.version", from.Kubernetes.Version, to.Kubernetes.Version, step)
	machines("kubernetes.nodeImage", image(from.Kubernetes.NodeImage), image(to.Kubernetes.NodeImage), "")
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
				"The Job first saves a snapshot of the seed's etcd (k3s etcd-snapshot save). It is kept: k3s prunes no snapshot saved on demand."},
			{"seed-k3s-config", k3sLeaderElectFile,
				"Before it replaces the binary the Job writes this k3s config drop-in: leader-elect=false for kube-controller-manager, kube-scheduler and " +
					"k3s's cloud-controller-manager, which a seed of one server does not need and which ended k3s when a slow disk made one miss its lease. " +
					"It takes effect with the restart. /etc/rancher/k3s/config.yaml stays as installed. To undo it by hand: delete the file and restart k3s."},
			{"seed-k3s-binary", "/usr/local/bin/k3s, the previous binary kept as /usr/local/bin/k3s.prev",
				"The binary is replaced only after the download passed its sha256. The install script is not run again: the unit stays. " +
					"The way back if the seed's API does not return, by hand over SSH: " + seedK3sWayBack(from, to) + ". Not tried on a lab."},
		} {
			out = append(out, PlanChange{Name: w[0], To: w[1], Class: ClassSite, Stage: stageSeedK3s, Impact: w[2]})
		}
	}
	kubeVipAlone := ""
	if !newImage {
		kubeVipAlone = "kube-vip is in the control plane's machine spec, so the control plane machine would be replaced; kube-vip moves only together with a new node image, when the control plane is replaced anyway"
	}
	machines("images.kubeVip", from.Images.KubeVip, to.Images.KubeVip, kubeVipAlone)
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
	var thirdParty []int // the rows of thirdPartyMove components, which the pair gate below may refuse
	for i := range to.Components {
		t := &to.Components[i]
		f := find(from, t.Cluster, t.Name)
		switch {
		case f == nil && t.Feature != "" && ownPackage(t.Chart.Repo):
			// The one addition an upgrade makes: a chart of our own that only an installation whose settings ask for
			// its feature renders (BOM.For). For every other installation the move changes nothing of it.
			wave := t.Wave
			out = append(out, PlanChange{Cluster: t.Cluster, Name: t.Name, To: t.Chart.Version, Class: ClassAdded, Stage: stageWorkloadPlatform, Wave: &wave,
				Impact: "New in this release, and rendered only when the settings ask for it (feature " + t.Feature + ", spec." + t.Feature + "): nothing is synced for it on a " +
					"platform whose settings do not. No pod of a tenant restarts and no machine is replaced."})
		case f == nil:
			refuse(t, "", t.Chart.Version, "a component is added to an installed platform only by a later release")
		case f.Wave != t.Wave || f.Install != t.Install || f.Namespace != t.Namespace || f.Group != t.Group || f.Stateful != t.Stateful ||
			f.Chart.Repo != t.Chart.Repo || f.Chart.Name != t.Chart.Name || !reflect.DeepEqual(f.Values, t.Values):
			refuse(t, f.Chart.Version, t.Chart.Version, "the component's wave, install mode, namespace, node group, chart source or values changed, and a run moves only a chart version")
		case f.Chart.Version == t.Chart.Version && f.Chart.Digest == t.Chart.Digest:
		case !ownPackage(t.Chart.Repo) && !thirdPartyMove(t):
			refuse(t, f.Chart.Version+"@"+f.Chart.Digest, t.Chart.Version+"@"+t.Chart.Digest,
				"a third-party chart is upgraded only by a later release (this one moves the product's own charts and cert-manager)")
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
			if !ownPackage(t.Chart.Repo) {
				c.Impact = certManagerImpact
				thirdParty = append(thirdParty, len(out))
			}
			out = append(out, c)
		}
	}
	// A third-party chart moves in a release of its own: the seed's charts sync after its k3s (stage 5a), the platform
	// cluster's after the roll, so in one run the old chart would meet the new Kubernetes first.
	var also []string
	for _, c := range out {
		if c.Class == ClassSeed || c.Class == ClassMachines {
			also = append(also, c.Name)
		}
	}
	for _, i := range thirdParty {
		if len(also) == 0 {
			break
		}
		out[i] = PlanChange{Cluster: out[i].Cluster, Name: out[i].Name, From: out[i].From, To: out[i].To, Class: ClassRefused, Blocking: later +
			"a third-party chart moves only in an upgrade that changes no k3s, Kubernetes version or node image, and this one also changes " +
			strings.Join(also, ", ") + ". First finish an upgrade to a release with this one's Kubernetes version (" + to.Kubernetes.Version +
			"), k3s (" + to.Seed.K3s.Version + ") and node image (" + to.Kubernetes.NodeImage.Tag + "), with that release's build, then start this one"}
	}
	for i := range from.Components {
		if f := &from.Components[i]; find(to, f.Cluster, f.Name) == nil {
			refuse(f, f.Chart.Version, "", "a component is removed from an installed platform only by a later release")
		}
	}
	return out
}
