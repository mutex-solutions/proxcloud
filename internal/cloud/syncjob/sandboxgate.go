package syncjob

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

// The sandbox provider's release gates (docs/managed-sandbox.md §3), checked through the Proxmox provider that owns the
// workers (config.sandbox.proxmoxServiceId): that provider's sync fences them, and its sandbox kubeconfig (the
// proxcloud-firewall service account) and PVE token are what the gates read with. Everything here only reads, except
// the API server's dry run, which stores nothing.

// nodeRestrictionProbe is the node identity the NodeRestriction gate impersonates. The firewall service account may
// impersonate this user and the system:nodes group and nothing else (deploy/sandbox-cluster/rbac.yaml).
const nodeRestrictionProbe = "proxcloud-noderestriction-probe"

// nodeProber is the part of the sandbox cluster's API the NodeRestriction gate uses (kamajik8s.Client).
type nodeProber interface {
	CreateNodeDryRunAs(ctx context.Context, user string, groups []string, node map[string]any) error
}

// probeNodeRestriction asks the API server, as a node, to create its own Node twice in a dry run: plain, which the Node
// authorizer allows (so the identity works), and with a node-restriction.kubernetes.io/ label, which only the
// NodeRestriction admission plugin refuses. The gate holds when the first passes and the second is refused by that
// plugin; a refusal for any other reason proves nothing.
func probeNodeRestriction(ctx context.Context, kc nodeProber) error {
	user, groups := "system:node:"+nodeRestrictionProbe, []string{"system:nodes"}
	node := func(labels map[string]any) map[string]any {
		meta := map[string]any{"name": nodeRestrictionProbe}
		if labels != nil {
			meta["labels"] = labels
		}
		return map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": meta}
	}
	if err := kc.CreateNodeDryRunAs(ctx, user, groups, node(nil)); err != nil {
		return fmt.Errorf("a node identity could not register its own Node in a dry run, so the probe proves nothing (impersonation rights of the firewall service account, or no Node authorizer): %w", err)
	}
	err := kc.CreateNodeDryRunAs(ctx, user, groups, node(map[string]any{"node-restriction.kubernetes.io/proxcloud-probe": "x"}))
	if err == nil {
		return errors.New("NodeRestriction is off: a node could label itself node-restriction.kubernetes.io/ and drop its own taints")
	}
	var ae *kamajik8s.APIError
	if errors.As(err, &ae) && ae.Status == 403 && strings.Contains(ae.Message, "is not allowed to set the following labels") {
		return nil
	}
	return fmt.Errorf("the labelled dry run was refused, but not by NodeRestriction: %w", err)
}

// SandboxReleaseGates checks the three release gates of a sandbox provider; each Gate says whether it holds and why.
func (j *Job) SandboxReleaseGates(ctx context.Context, es *externalservice.ExternalService) []sandbox.Gate {
	gates := []sandbox.Gate{{Name: sandbox.GateNodeRestriction}, {Name: sandbox.GateIsolation}, {Name: sandbox.GateFence}}
	set := func(i int, err error, ok string) {
		gates[i].OK, gates[i].Detail = err == nil, ok
		if err != nil {
			gates[i].Detail = err.Error()
		}
	}
	pes, cfg, kc, ps, err := j.sandboxSite(ctx, es)
	if err != nil {
		for i := range gates {
			set(i, err, "")
		}
		return gates
	}
	set(0, probeNodeRestriction(ctx, kc), "a node identity is refused a node-restriction.kubernetes.io/ label on its own Node")
	netCfg := pes.PVENetworkConfig()
	set(1, ps.CheckSandboxIsolation(ctx, netCfg.CIDRPool, netCfg.BlockedCIDRs, netCfg.SandboxCIDRs, pes.PVESandboxEndpoints()...),
		"the cluster firewall drops "+strings.Join(netCfg.SandboxCIDRs, ",")+" to and from the tenant pool and the blocked networks")
	open, ferr := openWorkers(ctx, kc)
	if ferr == nil {
		var f pve.SandboxFirewall
		if f, ferr = sandboxFirewall(ctx, ps, netCfg, cfg, kc.Server()); ferr == nil {
			ferr = ps.CheckSandboxFence(ctx, f, open)
		}
	}
	set(2, ferr, fmt.Sprintf("%d worker(s) open to sandboxes, each with the VM firewall of pool %s pinned to its address", len(open), cfg.Pool))
	ss, nerr := sandbox.New(es.SandboxConfig(), es.ID)
	if nerr == nil {
		nerr = ss.CheckNetworkGuard(ctx)
	}
	ng := sandbox.Gate{Name: "network-guard", OK: nerr == nil, Detail: "host CNI guard installed on every schedulable node"}
	if nerr != nil {
		ng.Detail = nerr.Error()
	}
	gates = append(gates, ng)
	return gates
}

// sandboxSite resolves what the gates read through: the Proxmox provider of the workers, its sandbox config, the
// firewall service account's client and the PVE service. Both kubeconfigs must name the same API server, or the gates
// would vouch for another cluster than the one sandboxes are created on.
func (j *Job) sandboxSite(ctx context.Context, es *externalservice.ExternalService) (*externalservice.ExternalService, externalservice.PVESandbox, *kamajik8s.Client, *pve.Service, error) {
	var none externalservice.PVESandbox
	id := es.SandboxProxmoxServiceID()
	if id == "" {
		return nil, none, nil, nil, errors.New("config.sandbox.proxmoxServiceId is not set: no Proxmox provider fences this provider's workers")
	}
	pes, err := j.services.Get(ctx, id)
	if err != nil {
		return nil, none, nil, nil, err
	}
	if pes == nil || !pes.IsProxmox() || pes.IsDisabled() {
		return nil, none, nil, nil, fmt.Errorf("config.sandbox.proxmoxServiceId %s is no enabled Proxmox provider", id)
	}
	cfg, ok := pes.PVESandboxConfig()
	if !ok || cfg.Kubeconfig == "" {
		return nil, none, nil, nil, fmt.Errorf("Proxmox provider %s has no sandbox pool (config.sandbox) or no secret.sandboxKubeconfig", id)
	}
	kc, err := kamajik8s.New(cfg.Kubeconfig)
	if err != nil {
		return nil, none, nil, nil, fmt.Errorf("Proxmox provider %s: sandbox kubeconfig: %w", id, err)
	}
	own, err := kamajik8s.New(es.SandboxConfig().Kubeconfig)
	if err != nil {
		return nil, none, nil, nil, fmt.Errorf("secret.kubeconfig: %w", err)
	}
	if own.Server() != kc.Server() {
		return nil, none, nil, nil, fmt.Errorf("this provider's kubeconfig names %s, Proxmox provider %s fences %s: they must be one cluster", own.Server(), id, kc.Server())
	}
	ps, err := j.pveFor(pes)
	if err != nil {
		return nil, none, nil, nil, err
	}
	return pes, cfg, kc, ps, nil
}

// openWorkers is every Node without the unsecured taint (the scheduler may put tenant pods there) → the address it
// reports.
func openWorkers(ctx context.Context, kc SandboxNodes) (map[string]string, error) {
	nodes, err := kc.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	open := map[string]string{}
	for _, n := range nodes {
		if !hasTaint(n, pve.SandboxUnsecuredTaint) {
			open[strAt(n, "metadata", "name")] = internalIP(n)
		}
	}
	return open, nil
}

// syncSandboxService reconciles the project's SANDBOX rows off the sandbox cluster. A sandbox the controller removed at
// its shutdownTime vanishes from the list; its row is ended (sandbox.MarkVanished) and archived, and the charge rates it to its end.
func (j *Job) syncSandboxService(ctx context.Context, p *project.Project, es *externalservice.ExternalService) int {
	if j.sandboxFor == nil {
		return 0
	}
	region := es.SandboxRegion()
	ss, err := j.sandboxFor(es)
	if err != nil {
		j.log.Error("syncjob: build sandbox service", "project", p.ID, "serviceId", es.ID, "err", err)
		return 0
	}
	st, err := providers.Reconcile(ctx, ss.SyncProvider(region, p.ID), j.cloud, es.ID, j.now())
	if err != nil {
		j.log.Error("syncjob: reconcile sandboxes", "project", p.ID, "serviceId", es.ID, "err", err)
	}
	return st.Created + st.Updated
}

// RunSandboxState is the minutely sandbox read, ahead of the minutely charge: every enabled project on every sandbox
// provider (a DISABLED provider's sandboxes still run until they end). A sandbox lives for seconds to hours, so the
// 15-minute sync alone would see many start and end unread: this pins each start (runningSince) within a minute and
// ends each vanished row (sandbox.MarkVanished). Returns the rows changed.
// ponytail: one list per project per minute; one cluster-wide list per provider if the project count grows.
func (j *Job) RunSandboxState(ctx context.Context) (int, error) {
	services, err := j.services.ListByType(ctx, externalservice.TypeCloud)
	if err != nil {
		return 0, err
	}
	projects, err := j.projects.AllEnabled(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for i := range services {
		es := &services[i]
		if !es.IsSandbox() {
			continue
		}
		for k := range projects {
			if projects[k].HasService(es.ID) {
				total += j.syncSandboxService(ctx, &projects[k], es)
			}
		}
	}
	return total, nil
}
