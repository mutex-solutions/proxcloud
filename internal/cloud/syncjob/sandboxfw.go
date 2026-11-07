package syncjob

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// SandboxNodes is the part of the sandbox cluster's API the pass uses (kamajik8s.Client).
type SandboxNodes interface {
	ListNodes(ctx context.Context) ([]map[string]any, error)
	SetNodeTaints(ctx context.Context, name, resourceVersion string, taints []any) error
	Server() string
}

// reconcileSandboxWorkers is layer 1 of the sandbox network (docs/managed-sandbox.md §2): the VM firewall of every
// worker in the provider's sandbox pool, then the unsecured taint off each worker that is fenced, pinned to its own
// address and older than the join token on its disk. A worker that fails any step keeps its taint, and one already
// cleared that fails a later pass gets it back.
func (j *Job) reconcileSandboxWorkers(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService) error {
	cfg, ok := es.PVESandboxConfig()
	if !ok {
		return nil
	}
	if cfg.Kubeconfig == "" {
		return errors.New("sandbox: secret.sandboxKubeconfig is missing; the workers stay as they are (tainted)")
	}
	kc, err := kamajik8s.New(cfg.Kubeconfig)
	if err != nil {
		return fmt.Errorf("sandbox: kubeconfig: %w", err)
	}
	err = j.fenceSandbox(ctx, ps, es, cfg, kc)
	// The entry appliance passes traffic only where the workers' rule admits it, so it follows the fence either way.
	if cfg.Proxy.EntryVMID > 0 {
		if eerr := ps.SandboxEntry(ctx, cfg.Proxy.EntryVMID, cfg.Proxy.EntryPool, cfg.Pool, cfg.Proxy.Address, cfg.Proxy.Port); eerr != nil {
			err = errors.Join(err, fmt.Errorf("sandbox entry: %w", eerr))
		}
	}
	return err
}

func (j *Job) fenceSandbox(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService, cfg externalservice.PVESandbox, kc SandboxNodes) error {
	f, err := sandboxFirewall(ctx, ps, es.PVENetworkConfig(), cfg, kc.Server())
	if err != nil {
		return err
	}
	pass, err := ps.StartSandboxPass(ctx, f)
	if err != nil {
		return err
	}
	nodes, err := kc.ListNodes(ctx)
	if err != nil { // the firewall still goes on; without the nodes no address is pinned and no taint comes off
		j.log.Warn("syncjob: sandbox nodes", "serviceId", es.ID, "err", err)
	}
	byName := map[string]map[string]any{}
	for _, n := range nodes {
		byName[strAt(n, "metadata", "name")] = n
	}
	for _, w := range pass.Workers {
		n := byName[w.Name]
		addr := internalIP(n)
		st, err := pass.Secure(ctx, w, trustedAddr(n))
		if lostFence(n, st, err) {
			j.log.Error("syncjob: sandbox worker no longer fenced, tainting it again", "serviceId", es.ID, "vmid", w.VMID,
				"node", w.Name, "address", addr, "pinned", st.Pinned, "secured", st.Secured, "err", err)
			if err := kc.SetNodeTaints(ctx, w.Name, strAt(n, "metadata", "resourceVersion"), withTaint(n, pve.SandboxUnsecuredTaint)); err != nil {
				j.log.Error("syncjob: sandbox worker taint", "serviceId", es.ID, "node", w.Name, "err", err)
			}
			continue
		}
		if err != nil {
			j.log.Error("syncjob: sandbox worker firewall", "serviceId", es.ID, "vmid", w.VMID, "err", err)
			continue
		}
		if ok, why := clearForTenants(st, n, j.now(), cfg.UnsecuredMinAge); !ok {
			if why != "" {
				j.log.Warn("syncjob: sandbox worker keeps its taint", "serviceId", es.ID, "vmid", w.VMID, "node", w.Name, "why", why)
			}
			continue
		}
		if err := kc.SetNodeTaints(ctx, w.Name, strAt(n, "metadata", "resourceVersion"), withoutTaint(n, pve.SandboxUnsecuredTaint)); err != nil {
			j.log.Error("syncjob: sandbox worker untaint", "serviceId", es.ID, "node", w.Name, "err", err)
			continue
		}
		j.log.Info("syncjob: sandbox worker fenced, open to tenant pods", "serviceId", es.ID, "node", w.Name, "address", addr, "group", pass.Group())
	}
	return pass.Sweep(ctx)
}

// sandboxFirewall is the firewall the pass writes for a provider's sandbox pool (and the fence gate reads back).
func sandboxFirewall(ctx context.Context, ps *pve.Service, netCfg externalservice.PVENetwork, cfg externalservice.PVESandbox, server string) (pve.SandboxFirewall, error) {
	if len(netCfg.SandboxCIDRs) != 1 {
		return pve.SandboxFirewall{}, fmt.Errorf("sandbox: network.sandboxCidrs must hold the sandbox subnet, has %d entries", len(netCfg.SandboxCIDRs))
	}
	host, port, err := apiEndpoint(server)
	if err != nil {
		return pve.SandboxFirewall{}, err
	}
	deny, err := sandboxDeny(ctx, ps, netCfg, cfg)
	if err != nil {
		return pve.SandboxFirewall{}, err
	}
	return pve.SandboxFirewall{
		Pool: cfg.Pool, Subnet: netCfg.SandboxCIDRs[0], Nodes: cfg.Nodes, LoadBalancers: cfg.LoadBalancers,
		ControlPlane: host, APIPort: port, DNS: cfg.DNS, KubeletFrom: cfg.KubeletSources, Deny: deny,
		ProxyAddress: cfg.Proxy.Address, ProxyPort: cfg.Proxy.Port, EntryFrom: cfg.Proxy.EntryAddress,
	}, nil
}

// sandboxDeny is what a worker's internet set leaves out on top of the private ranges: the nodes' management
// networks and every address a node holds (its PVE API and ssh, on whatever interface), the sandbox's own SNAT address
// (on the exit node, often public, and not on an interface PVE lists), the blocked networks and the tenant pool. A node
// that does not answer stops the pass: a new generation must never deny less than the one the workers hold.
func sandboxDeny(ctx context.Context, ps *pve.Service, netCfg externalservice.PVENetwork, cfg externalservice.PVESandbox) ([]string, error) {
	mgmt, complete, err := ps.C.ManagementCIDRs(ctx)
	if err != nil {
		return nil, err
	}
	hosts, hostsComplete, err := ps.C.NodeAddresses(ctx)
	if err != nil {
		return nil, err
	}
	if !complete || !hostsComplete {
		// ponytail: a down node pauses the whole pass (new workers stay tainted); keep the last complete list if that hurts.
		return nil, errors.New("sandbox: a PVE node did not report its networks; the workers keep their current firewall this pass")
	}
	deny := append(append(append(mgmt, hosts...), netCfg.BlockedCIDRs...), netCfg.CIDRPool)
	if ip := net.ParseIP(cfg.SNAT); ip != nil {
		deny = append(deny, cfg.SNAT)
	}
	return deny, nil
}

// trustedAddr is the Node's address while it still has the unsecured taint, "" after: the kubelet writes its Node, and
// once tenant pods run there it may name another worker's address.
func trustedAddr(n map[string]any) string {
	if hasTaint(n, pve.SandboxUnsecuredTaint) {
		return internalIP(n)
	}
	return ""
}

// lostFence says a worker already cleared for tenants is not fenced as it was: the pass failed on it, its firewall
// did not read back in place, or its filter no longer admits exactly its Node's address. It gets the taint back.
func lostFence(n map[string]any, st pve.SandboxWorkerState, err error) bool {
	if n == nil || hasTaint(n, pve.SandboxUnsecuredTaint) {
		return false
	}
	return err != nil || !st.Secured || st.Pinned == "" || st.Pinned != internalIP(n)
}

// clearForTenants says whether a worker's unsecured taint may come off: its firewall read back in place, its filter
// pinned to the address its Node reports, and the Node older than minAge. why is set when something looks wrong
// (rather than just early).
func clearForTenants(st pve.SandboxWorkerState, n map[string]any, now time.Time, minAge time.Duration) (ok bool, why string) {
	switch {
	case n == nil || !hasTaint(n, pve.SandboxUnsecuredTaint) || !st.Secured:
		return false, ""
	case st.Pinned == "" || st.Pinned != internalIP(n):
		return false, fmt.Sprintf("its Node reports %q, its firewall admits %q", internalIP(n), st.Pinned)
	}
	created, err := time.Parse(time.RFC3339, strAt(n, "metadata", "creationTimestamp"))
	if err != nil {
		return false, "no creation time on its Node"
	}
	return now.Sub(created) >= minAge, ""
}

func apiEndpoint(server string) (string, int, error) {
	u, err := url.Parse(server)
	if err != nil || u.Hostname() == "" {
		return "", 0, fmt.Errorf("sandbox: kubeconfig server %q", server)
	}
	if net.ParseIP(u.Hostname()) == nil {
		return "", 0, fmt.Errorf("sandbox: kubeconfig server %q must be an IPv4 address: the firewall rule names it", server)
	}
	port := 443
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	return u.Hostname(), port, nil
}

func strAt(m map[string]any, keys ...string) string {
	for i, k := range keys {
		if i == len(keys)-1 {
			s, _ := m[k].(string)
			return s
		}
		m, _ = m[k].(map[string]any)
	}
	return ""
}

func internalIP(n map[string]any) string {
	status, _ := n["status"].(map[string]any)
	addrs, _ := status["addresses"].([]any)
	for _, a := range addrs {
		if m, ok := a.(map[string]any); ok && m["type"] == "InternalIP" {
			s, _ := m["address"].(string)
			return s
		}
	}
	return ""
}

func taints(n map[string]any) []any {
	spec, _ := n["spec"].(map[string]any)
	t, _ := spec["taints"].([]any)
	return t
}

func hasTaint(n map[string]any, key string) bool {
	for _, t := range taints(n) {
		if m, ok := t.(map[string]any); ok && m["key"] == key {
			return true
		}
	}
	return false
}

// withTaint is the Node's taints plus key as NoSchedule, the way the node group sets it at join.
func withTaint(n map[string]any, key string) []any {
	return append(append([]any{}, taints(n)...), map[string]any{"key": key, "value": "true", "effect": "NoSchedule"})
}

func withoutTaint(n map[string]any, key string) []any {
	out := []any{}
	for _, t := range taints(n) {
		if m, ok := t.(map[string]any); !ok || m["key"] != key {
			out = append(out, t)
		}
	}
	return out
}
