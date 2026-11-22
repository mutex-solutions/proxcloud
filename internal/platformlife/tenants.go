package platformlife

// tenants.go — what a green install still owes a tenant (design decision 51): a Kubernetes version on the Kubernetes
// provider, and the two providers attached to projects. Both were hand steps on the first production install.
//
// The version. The run never publishes a node image: stage 2 imports it staged, and Template updates publishes only a
// copy whose smoke test passed. So the register stage, every apply and POST …/kubernetes-version offer the BOM's
// Kubernetes version only once a node image of it is published on every node the platform cluster uses, with the line
// "Publish on all nodes" shows for copying. A version the provider already names is the operator's and is never
// written again. A run offers a version once (status.kubernetesOffered): when the operator removed it from the provider
// since, an apply leaves it removed and only the route offers it again.
//
// The attachment. spec.tenants.attach says who gets the two providers; the register stage applies a choice once, when
// it differs from the one it applied last (status.tenantsAttach).

import (
	"slices"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
)

// The values of spec.tenants.attach. A spec without one means AttachNone, as every install before v0.9.0 was.
const (
	AttachAll  = "all"  // every project that exists, and every new one
	AttachNew  = "new"  // new projects only
	AttachNone = "none" // no project: the operator attaches the providers later
)

// Tenants is what the install does for tenants beyond building the platform.
//
// Room for the sandbox choice (owner decision 2, a later release): where sandboxes run, on a node group of this
// cluster or on a cluster of their own, becomes one more optional key of the spec. A spec stored without it reads as
// "no sandbox", as today's do, so adding it changes no stored record and no request an older console sends.
type Tenants struct {
	// Attach: AttachAll, AttachNew or AttachNone ("" = AttachNone). Changeable after install.
	Attach string `json:"attach,omitempty"`
}

// AttachChoice is the attach choice with the default spelled out.
func (t Tenants) AttachChoice() string {
	if t.Attach == "" {
		return AttachNone
	}
	return t.Attach
}

// ClusterNodes are the Proxmox nodes the installation's clusters put a machine on, sorted, each once: the nodes stage 2
// imports the node image on.
func (in *Installation) ClusterNodes() []string {
	var nodes []string
	for _, c := range in.Spec.Clusters {
		nodes = append(nodes, c.ControlPlane.Node)
		for _, g := range c.NodeGroups {
			nodes = append(nodes, g.Nodes...)
		}
	}
	for _, g := range in.Spec.sandboxGroups() { // of either mode: they boot the same node image
		nodes = append(nodes, g.Nodes...)
	}
	return slices.Compact(slices.Sorted(slices.Values(nodes)))
}

// NodeImageLine is the Kubernetes provider's config.cluster.versions value for b's Kubernetes version, read from the
// cluster's VM rows: the newest published node image of that version per node, "pve1=9212,pve2=9213". missing are the
// nodes of the installation's clusters that publish none; with any, line is "" and the version is not offered: a
// tenant cluster's workers are cloned from the image of their own node.
func NodeImageLine(b *BOM, in *Installation, rows []pve.ClusterResource) (line string, missing []string) {
	tag, _ := templateupdates.ParseTag(b.Kubernetes.NodeImage.Tag)
	line = templateupdates.KubernetesVersionsValue(rows, tag.OS, b.Kubernetes.Version)
	has := map[string]bool{}
	for _, part := range strings.Split(line, ",") {
		node, _, _ := strings.Cut(part, "=")
		has[node] = true
	}
	missing = []string{}
	for _, n := range in.ClusterNodes() {
		if !has[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return "", missing
	}
	return line, missing
}

// NodesText names the nodes NodeImageLine found missing for a message; none (an installation with no machine and no
// published image anywhere) reads "any node".
func NodesText(missing []string) string {
	if len(missing) == 0 {
		return "any node"
	}
	return strings.Join(missing, ", ")
}

// OfferVersion writes version with its node image line into a Kubernetes provider row's config.cluster.versions and
// reports whether it did. A version the row already names stays as it is, whatever it names.
func OfferVersion(row map[string]any, version, line string) bool {
	if version == "" || line == "" {
		return false
	}
	at := row
	for _, k := range []string{"config", "cluster", "versions"} {
		next, ok := at[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			at[k] = next
		}
		at = next
	}
	if _, has := at[version]; has {
		return false
	}
	at[version] = line
	return true
}

// SchedulerOffNote is the one row the Check and the Ready card show while the job scheduler is off: what a tenant then
// misses, and the command that turns it on. Nothing turns it on by itself: it also starts charging.
const SchedulerOffNote = "The job scheduler is off. Until it is on, a tenant cannot delete a project, the state of clusters, databases and " +
	"servers is refreshed only when an operator runs a provider sync, and no usage is charged or billed. Turn it on where ProxCloud is " +
	"installed: ./proxcloud-up.sh install --scheduler on a Compose install, PROXCLOUD_JOBS_SCHEDULER_ENABLED=true for the API elsewhere. " +
	"It starts charging usage on a timer, so set the prices first."

// SchedulerOffSandboxNote is what the same row adds while the settings ask for sandboxes. The jobs that keep sandboxes
// safe run on the scheduler: the provider sync that fences a sandbox machine, takes its unsecured taint off and puts it
// back on a machine that lost its fence, and the minutely read and abuse check. One operator sync fences once and
// nothing watches afterwards, so a sandbox provider of a managed platform is refused "live" while the scheduler is off.
const SchedulerOffSandboxNote = " Sandboxes need it: the provider sync fences each sandbox machine and keeps watching its firewall, and the minutely " +
	"read ends sandboxes that are gone. While the scheduler is off the sandbox provider cannot be set live. The same switch starts charging " +
	"sandbox minutes, and the price rule for sandboxes holds a placeholder rate until you set one (or 0)."
