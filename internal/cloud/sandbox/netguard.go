package sandbox

import (
	"context"
	"fmt"
	"time"
)

// CheckNetworkGuard requires fresh host-installed CNI chaining on every schedulable
// node. Old installations fail closed until stage 55 is reapplied. Read-only.
func (s *Service) CheckNetworkGuard(ctx context.Context) error {
	a, ok := s.api.(telemetryAPI)
	if !ok {
		return fmt.Errorf("sandbox network guard reader unavailable")
	}
	napi, ok := s.api.(interface {
		ListNodes(context.Context) ([]map[string]any, error)
	})
	if !ok {
		return fmt.Errorf("sandbox node reader unavailable")
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
	nodes, err := napi.ListNodes(ctx)
	if err != nil {
		return err
	}
	count := 0
	for _, n := range nodes {
		if dig(n, "spec", "unschedulable") == true {
			continue
		}
		if !fresh[digStr(n, "metadata", "name")] {
			return fmt.Errorf("sandbox host CNI guard missing/stale; reapply installer stage 55 before enabling provider")
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("no network-guarded sandbox nodes")
	}
	return nil
}
