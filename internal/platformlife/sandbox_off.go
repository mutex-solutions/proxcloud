package platformlife

// sandbox_off.go — what an off of placement shared takes away that neither ArgoCD nor Cluster API does (the machines,
// the Applications and the access on Proxmox are sandbox_run.go):
//
//   - On the platform cluster the CustomResourceDefinitions of agent-sandbox. The sandbox chart installs them, and
//     the cascade of the Application's deletion left all four on the lab, with no finalizer and no owner
//     (v0120lab/z3/c/crds-left.txt): ArgoCD's controller is understood to leave definitions out of a cascade. A
//     definition goes only when no object of its kind is left in any namespace: deleting it deletes them all.
//   - On the seed the sandbox group's address pool and its machine and bootstrap templates. The cluster's Application
//     never prunes, so the pool stays when the group leaves the render and the Application reads OutOfSync from then
//     on; the templates carry helm.sh/resource-policy keep, as every group's do, so that no template is deleted under a
//     machine. They go once no MachineDeployment, MachineSet or Machine of the group is left, the templates first: the
//     pool is what they name.
//
//   - On the platform cluster the projects' namespaces sb-<project> of the sandbox provider, before the sandbox chart's
//     Application goes (sandboxProjectNamespacesGone).
//
// Separate has none of the three: its definitions and the projects' namespaces are inside pc-sandbox and its pool and
// templates in the namespace pc-sandbox on the platform cluster, and all go with the cluster.
//
// And for both placements, when the last sandbox machine is gone: the sandbox provider's identity is taken away and
// the row marked as having no placement (SandboxProviderOff).

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

const (
	bootstrapGroup         = "bootstrap.cluster.x-k8s.io"
	ipamGroup, ipamVersion = "ipam.cluster.x-k8s.io", "v1alpha2"
	crdGroup, crdVersion   = "apiextensions.k8s.io", "v1"
	crdPlural              = "customresourcedefinitions"
)

// sandboxCRDs are the CustomResourceDefinitions the sandbox chart installs (agent-sandbox, vendored in the chart's
// files). builtSharedSandbox (sandbox_off_test.go) holds the list equal to the chart's render.
var sandboxCRDs = []string{
	"sandboxclaims.extensions.agents.x-k8s.io",
	"sandboxes.agents.x-k8s.io",
	"sandboxtemplates.extensions.agents.x-k8s.io",
	"sandboxwarmpools.extensions.agents.x-k8s.io",
}

// sandboxCRDsGone deletes, once the sandbox chart's Application is gone from the platform cluster, each definition of
// sandboxCRDs that has no object left, and waits until the API server has taken it away. A definition with objects is
// kept and named in the log: the objects are a project's, and the portal ends them (docs/managed-services.md,
// "Switching sandboxes off", has the commands for what is then left).
func (x *acq) sandboxCRDsGone(ctx context.Context, wl *kamajik8s.Client) error {
	var going []string
	for _, name := range sandboxCRDs {
		crd, err := wl.GetClusterScoped(ctx, crdGroup, crdVersion, crdPlural, name)
		if err != nil {
			return classify(CodeWorkloadUnreachable, err)
		}
		if crd == nil {
			continue
		}
		if str(crd, "metadata", "deletionTimestamp") != "" {
			going = append(going, name)
			continue
		}
		versions, _ := dig(crd, "spec", "versions").([]any)
		left := 0
		for _, v := range versions {
			if m, _ := v.(map[string]any); m["served"] == true {
				objs, err := wl.ListCRsAllNamespaces(ctx, str(crd, "spec", "group"), str(m, "name"), str(crd, "spec", "names", "plural"), "")
				if err != nil && !kamajik8s.NotFound(err) {
					return classify(CodeWorkloadUnreachable, err)
				}
				left = len(objs)
				break // every served version lists the same objects
			}
		}
		if left > 0 {
			x.r.Log.Warn("platform run: a sandbox definition is kept, objects of it are left", "run", x.runID, "crd", name, "objects", left)
			continue
		}
		if err := x.check(ctx); err != nil {
			return err
		}
		if err := wl.DeleteClusterScoped(ctx, crdGroup, crdVersion, crdPlural, name); err != nil {
			return classify(CodeWorkloadUnreachable, err)
		}
		going = append(going, name)
	}
	if len(going) > 0 {
		return waiting(CodeMachineDeleteStuck, "Sandboxes are switched off: the platform cluster is deleting the definitions of agent-sandbox (%s), and the run goes on when they are gone.", strings.Join(going, ", "))
	}
	return nil
}

// sandboxGroupGone runs at the end of stage 7 of an off of placement shared, when no machine of the group is left: it
// deletes the group's templates and then its address pool from the seed, and waits until the pool is gone. After it
// the cluster's Application has nothing left that its render does not name.
func (x *acq) sandboxGroupGone(ctx context.Context, seed *kamajik8s.Client) error {
	built := x.in.Status.Sandbox
	if built == nil || x.in.Spec.sandboxMode() != "" || built.Placement != SandboxShared {
		return nil
	}
	// A template is deleted under no machine: not while a MachineDeployment or a MachineSet names it, nor while a
	// Machine of the group exists (its ProxmoxMachine was cloned from one).
	named := map[string]bool{}
	for _, plural := range []string{"machinedeployments", "machinesets"} {
		owners, err := seed.ListCRs(ctx, capiGroup, capiVersion, plural, ClusterName, "")
		if err != nil {
			return classify(CodeSeedAPIUnreachable, err)
		}
		for _, o := range owners {
			named[str(o, "spec", "template", "spec", "infrastructureRef", "name")] = true
			named[str(o, "spec", "template", "spec", "bootstrap", "configRef", "name")] = true
		}
	}
	machines, err := seed.ListCRs(ctx, capiGroup, capiVersion, "machines", ClusterName, "proxcloud.io/node-group="+SandboxGroup)
	if err != nil {
		return classify(CodeSeedAPIUnreachable, err)
	}
	prefix, held := ClusterName+"-"+SandboxGroup+"-", len(machines)
	for _, kind := range [][3]string{{infraGroup, infraVersion, "proxmoxmachinetemplates"}, {bootstrapGroup, capiVersion, "kubeadmconfigtemplates"}} {
		templates, err := seed.ListCRs(ctx, kind[0], kind[1], kind[2], ClusterName, "")
		if err != nil {
			return classify(CodeSeedAPIUnreachable, err)
		}
		for _, t := range templates {
			name := str(t, "metadata", "name")
			switch {
			case !strings.HasPrefix(name, prefix):
			case named[name] || len(machines) > 0:
				held++
			default:
				if err := x.check(ctx); err != nil {
					return err
				}
				if err := seed.DeleteCR(ctx, kind[0], kind[1], kind[2], ClusterName, name); err != nil {
					return classify(CodeSeedAPIUnreachable, err)
				}
			}
		}
	}
	if held > 0 {
		return waiting(CodeMachineDeleteStuck, "Sandboxes are switched off: Cluster API still holds a machine or a MachineSet of group %s; its templates and its address pool are deleted when they are gone.", SandboxGroup)
	}
	name := ClusterName + "-" + SandboxGroup
	pool, err := seed.GetCR(ctx, ipamGroup, ipamVersion, "inclusterippools", ClusterName, name)
	if err != nil {
		return classify(CodeSeedAPIUnreachable, err)
	}
	if pool == nil {
		return nil
	}
	if str(pool, "metadata", "deletionTimestamp") == "" {
		if err := x.check(ctx); err != nil {
			return err
		}
		err := seed.DeleteCR(ctx, ipamGroup, ipamVersion, "inclusterippools", ClusterName, name)
		// The address provider refuses the delete of a pool while an address of it is claimed, and a claim outlives its
		// machine by a moment: a refusal is waited out, and the stage's deadline ends a wait that never ends.
		var ae *kamajik8s.APIError
		if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
			return waiting(CodeMachineDeleteStuck, "Sandboxes are switched off: the seed refuses to delete the address pool %s of group %s yet: %s", name, SandboxGroup, clip(ae.Message))
		}
		if err != nil {
			return classify(CodeSeedAPIUnreachable, err)
		}
	}
	return waiting(CodeMachineDeleteStuck, "Sandboxes are switched off: the seed is deleting the address pool %s of group %s, and the run goes on when it is gone.", name, SandboxGroup)
}

// sandboxIdentityKeys are the leaves of a sandbox provider's secret that are an identity on, or of, its cluster: the
// portal's kubeconfig, the worker rotation's, and the bearer its sandbox-proxy shows the portal.
var sandboxIdentityKeys = []string{"kubeconfig", "rotationKubeconfig", "proxyWakeToken"}

// SandboxProviderOff makes a sandbox provider row one with no placement: it takes the identities away (they name a
// cluster, or a node group, that is gone) and writes config.sandbox.offSince, which externalservice.SandboxOffSince
// reads. From then on the provider's rows are ended without a call and a project attached to it can be deleted
// (v0.12.0-rc.2, drill E5-A: the off left the row its dead kubeconfig, every teardown answered 401 and no attached
// project could be deleted). false = the row is not a sandbox provider's, or says so already: the time of the first
// off stays. The next build adopts the row and gives it a kubeconfig again (AdoptProvider takes the mark off).
func SandboxProviderOff(row map[string]any, at time.Time) bool {
	cfg, _ := row["config"].(map[string]any)
	if str(cfg, "provider") != "sandbox" {
		return false
	}
	secret, _ := row["secret"].(map[string]any)
	held := slices.ContainsFunc(sandboxIdentityKeys, func(k string) bool { _, ok := secret[k]; return ok })
	sb, _ := cfg["sandbox"].(map[string]any)
	if !held && str(sb, externalservice.SandboxOffSinceKey) != "" {
		return false
	}
	for _, k := range sandboxIdentityKeys {
		delete(secret, k)
	}
	if sb == nil {
		sb = map[string]any{}
		cfg["sandbox"] = sb
	}
	if str(sb, externalservice.SandboxOffSinceKey) == "" {
		sb[externalservice.SandboxOffSinceKey] = at.UTC().Format(time.RFC3339)
	}
	return true
}

// projectNamespaceWait is how long an off of placement shared waits for one project's namespace to go before it goes
// on without it. A namespace ends when its last pod is confirmed gone, and a pod on a sandbox machine that no longer
// answers is confirmed only once that machine is deleted, which this very off does further on.
const projectNamespaceWait = 5 * time.Minute

// sandboxProjectNamespacesGone deletes, in an off of placement shared, the namespaces sb-<project> of the sandbox
// provider on the platform cluster, and waits for them, before the sandbox chart's Application is deleted: its
// controller still takes its objects away, and the portal's identity, which could have deleted them at a project's
// teardown, goes with that chart (drill E3-B: both namespaces of the lab stayed). With separate they go with
// pc-sandbox.
//
// Only a namespace that is the product's own for that provider goes: managed by the product, labelled as a tenant's,
// with the provider's id, and named sb-<the project its label names>. Any other namespace is never deleted, whatever
// its name.
func (x *acq) sandboxProjectNamespacesGone(ctx context.Context, wl *kamajik8s.Client) error {
	var providerID string
	if p := x.in.Status.Providers; p != nil {
		providerID = p.Sandbox
	}
	if providerID == "" { // never registered: no project was ever attached
		return nil
	}
	items, err := wl.ListNamespaces(ctx, sandbox.LabelManagedBy+"="+sandbox.ManagedByValue+","+sandbox.LabelTenant+"=true,"+sandbox.LabelService+"="+providerID)
	if err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	var going []string
	for _, ns := range items {
		name := str(ns, "metadata", "name")
		labels, _ := dig(ns, "metadata", "labels").(map[string]any)
		if project := str(labels, sandbox.LabelProject); project == "" || name != sandbox.NamespaceFor(project) || str(labels, sandbox.LabelService) != providerID ||
			str(labels, sandbox.LabelManagedBy) != sandbox.ManagedByValue || str(labels, sandbox.LabelTenant) != "true" {
			continue
		}
		if since := str(ns, "metadata", "deletionTimestamp"); since != "" {
			if t, err := time.Parse(time.RFC3339, since); err == nil && time.Since(t) > projectNamespaceWait {
				x.r.Log.Warn("platform run: a project's sandbox namespace is still being deleted, the off goes on", "run", x.runID, "namespace", name, "since", since)
				continue
			}
			going = append(going, name)
			continue
		}
		if err := x.check(ctx); err != nil {
			return err
		}
		if err := wl.DeleteNamespace(ctx, name); err != nil && !kamajik8s.NotFound(err) {
			return classify(CodeWorkloadUnreachable, err)
		}
		going = append(going, name)
	}
	if len(going) > 0 {
		return waiting(CodeMachineDeleteStuck, "Sandboxes are switched off: the platform cluster is deleting the sandbox namespaces of %d projects (%s), and the run goes on when they are gone.", len(going), clip(strings.Join(going, ", ")))
	}
	return nil
}
