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
// address and older than the join token on its disk. A worker that fails any step keeps its taint.
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
	return j.fenceSandbox(ctx, ps, es, cfg, kc)
}

func (j *Job) fenceSandbox(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService, cfg externalservice.PVESandbox, kc SandboxNodes) error {
	netCfg := es.PVENetworkConfig()
	if len(netCfg.SandboxCIDRs) != 1 {
		return fmt.Errorf("sandbox: network.sandboxCidrs must hold the sandbox subnet, has %d entries", len(netCfg.SandboxCIDRs))
	}
	host, port, err := apiEndpoint(kc.Server())
	if err != nil {
		return err
	}
	mgmt, _, err := ps.C.ManagementCIDRs(ctx)
	if err != nil {
		return err
	}
	deny := append(append(mgmt, netCfg.BlockedCIDRs...), netCfg.CIDRPool)
	pass, err := ps.StartSandboxPass(ctx, pve.SandboxFirewall{
		Pool: cfg.Pool, Subnet: netCfg.SandboxCIDRs[0], Nodes: cfg.Nodes, LoadBalancers: cfg.LoadBalancers,
		ControlPlane: host, APIPort: port, DNS: cfg.DNS, KubeletFrom: cfg.KubeletSources, Deny: deny,
	})
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
		st, err := pass.Secure(ctx, w, addr)
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

func withoutTaint(n map[string]any, key string) []any {
	out := []any{}
	for _, t := range taints(n) {
		if m, ok := t.(map[string]any); !ok || m["key"] != key {
			out = append(out, t)
		}
	}
	return out
}
