package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/ciliumcni"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

// CheckNetworkGuard requires, on every schedulable node, that Cilium leaves the CNI config to
// netguard (cluster-wide, or by a CiliumNodeConfig selecting the node) and a fresh host-installed
// chain heartbeat. Old installations fail closed until stage 55 is reapplied. Read-only.
func (s *Service) CheckNetworkGuard(ctx context.Context) error {
	cm, ce := s.api.GetCore(ctx, "configmaps", "kube-system", "cilium-config")
	if ce != nil {
		return ce
	}
	conf := map[string]string{}
	if d, ok := dig(cm, "data").(map[string]any); ok {
		for k, v := range d {
			conf[k], _ = v.(string)
		}
	}
	ncs, err := s.ciliumNodeConfigs(ctx)
	if err != nil {
		return err
	}
	a, ok := s.api.(telemetryAPI)
	if !ok {
		return fmt.Errorf("sandbox network guard reader unavailable")
	}
	cms, err := a.ListConfigMaps(ctx, TelemetryNamespace, "app.kubernetes.io/name=sandbox-netguard")
	if err != nil {
		return err
	}
	fresh := map[string]bool{}
	for _, cm := range cms {
		at, e := time.Parse(time.RFC3339, digStr(cm, "data", "at"))
		if e == nil && !at.After(time.Now().Add(5*time.Second)) && time.Since(at) < 90*time.Second && digStr(cm, "data", "version") == "1" {
			fresh[digStr(cm, "data", "node")] = true
		}
	}
	nodes, err := s.api.ListNodes(ctx)
	if err != nil {
		return err
	}
	count := 0
	for _, n := range nodes {
		if dig(n, "spec", "unschedulable") == true {
			continue
		}
		name := digStr(n, "metadata", "name")
		labels := map[string]string{}
		if l, ok := dig(n, "metadata", "labels").(map[string]any); ok {
			for k, v := range l {
				labels[k], _ = v.(string)
			}
		}
		if !ciliumcni.CustomConf(conf, ncs, labels) {
			return fmt.Errorf("Cilium still owns the CNI config on node %s", name)
		}
		if !fresh[name] {
			return fmt.Errorf("sandbox host CNI guard missing/stale; reapply installer stage 55 before enabling provider")
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("no network-guarded sandbox nodes")
	}
	return nil
}

// ciliumNodeConfigs lists the CiliumNodeConfigs of kube-system. A 403 (RBAC from before the
// per-node check) counts as none: the ConfigMap alone decides, as it did then.
func (s *Service) ciliumNodeConfigs(ctx context.Context) ([]ciliumcni.NodeConfig, error) {
	items, err := s.api.ListCRs(ctx, "cilium.io", "v2", "ciliumnodeconfigs", "kube-system", "")
	var ae *kamajik8s.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusForbidden {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	var out []ciliumcni.NodeConfig
	return out, json.Unmarshal(b, &out)
}
