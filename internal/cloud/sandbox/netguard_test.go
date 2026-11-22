package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

type forbiddenCRs struct{ *sampleAPI }

func (forbiddenCRs) ListCRs(context.Context, string, string, string, string, string) ([]map[string]any, error) {
	return nil, &kamajik8s.APIError{Status: 403}
}

func TestNetworkGuardGateClusterWideOrPerNode(t *testing.T) {
	platformCM := map[string]any{"custom-cni-conf": "false", "write-cni-conf-when-ready": "/host/etc/cni/net.d/05-cilium.conflist"}
	cnc := func(defaults map[string]any) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": "sandbox-nodes", "namespace": "kube-system"}, "spec": map[string]any{
			"nodeSelector": map[string]any{"matchLabels": map[string]any{"proxcloud.io/node-group": "sandbox"}}, "defaults": defaults}}
	}
	node := func(name, group string) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{"proxcloud.io/node-group": group}}}
	}
	heartbeat := func(n string) map[string]any {
		return map[string]any{"data": map[string]any{"node": n, "at": time.Now().UTC().Format(time.RFC3339), "version": "1"}}
	}
	for _, c := range []struct {
		name      string
		cm        map[string]any
		cnc       map[string]any
		forbidden bool
		nodes     []map[string]any
		want      string // "" = passes
	}{
		{"cluster-wide", map[string]any{"custom-cni-conf": "true"}, nil, false, []map[string]any{node("s1", "sandbox")}, ""},
		{"cluster-wide, RBAC without ciliumnodeconfigs", map[string]any{"custom-cni-conf": "true"}, nil, true, []map[string]any{node("s1", "sandbox")}, ""},
		{"per-node", platformCM, cnc(map[string]any{"custom-cni-conf": "true", "write-cni-conf-when-ready": ""}), false, []map[string]any{node("s1", "sandbox")}, ""},
		{"neither", platformCM, nil, false, []map[string]any{node("s1", "sandbox")}, "still owns the CNI config on node s1"},
		{"per-node without write-cni-conf-when-ready", platformCM, cnc(map[string]any{"custom-cni-conf": "true"}), false, []map[string]any{node("s1", "sandbox")}, "still owns the CNI config on node s1"},
		{"per-node, a schedulable node it does not select", platformCM, cnc(map[string]any{"custom-cni-conf": "true", "write-cni-conf-when-ready": ""}), false, []map[string]any{node("s1", "sandbox"), node("p1", "system")}, "still owns the CNI config on node p1"},
		{"config right, heartbeat missing", map[string]any{"custom-cni-conf": "true"}, nil, false, []map[string]any{node("s1", "sandbox"), node("s2", "sandbox")}, "guard missing/stale"},
	} {
		f := newFake()
		f.core["configmaps/kube-system/cilium-config"] = map[string]any{"data": c.cm}
		if c.cnc != nil {
			f.crs["kube-system/sandbox-nodes"] = c.cnc
		}
		f.nodes = c.nodes
		a := &sampleAPI{fakeAPI: f, cms: []map[string]any{heartbeat("s1"), heartbeat("p1")}}
		var api K8sAPI = a
		if c.forbidden {
			api = forbiddenCRs{a}
		}
		err := NewWithAPI(api, testConfig(), "svc").CheckNetworkGuard(context.Background())
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err=%v, want %q", c.name, err, c.want)
		}
	}
}
