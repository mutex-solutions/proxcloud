package syncjob

import (
	"context"
	"errors"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// fakeProber answers the two dry runs: plain (the Node authorizer) and labelled (NodeRestriction).
type fakeProber struct {
	plain, labelled error
	calls           []string
}

func (f *fakeProber) CreateNodeDryRunAs(_ context.Context, user string, groups []string, node map[string]any) error {
	meta := node["metadata"].(map[string]any)
	f.calls = append(f.calls, user+" "+groups[0]+" "+meta["name"].(string))
	if meta["labels"] != nil {
		return f.labelled
	}
	return f.plain
}

func TestProbeNodeRestriction(t *testing.T) {
	refused := &kamajik8s.APIError{Status: 403, Message: `nodes "proxcloud-noderestriction-probe" is forbidden: node "proxcloud-noderestriction-probe" is not allowed to set the following labels: node-restriction.kubernetes.io/proxcloud-probe`}
	authz := &kamajik8s.APIError{Status: 403, Message: `nodes is forbidden: User "system:node:proxcloud-noderestriction-probe" cannot create resource "nodes"`}
	for name, c := range map[string]struct {
		plain, labelled error
		ok              bool
	}{
		"NodeRestriction on":            {nil, refused, true},
		"NodeRestriction off":           {nil, nil, false},
		"no right to impersonate":       {authz, authz, false},
		"refused, not by the admission": {nil, authz, false},
		"cluster unreachable":           {kamajik8s.ErrUnreachable, kamajik8s.ErrUnreachable, false},
	} {
		f := &fakeProber{plain: c.plain, labelled: c.labelled}
		err := probeNodeRestriction(context.Background(), f)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", name, err)
		}
		if f.calls[0] != "system:node:proxcloud-noderestriction-probe system:nodes proxcloud-noderestriction-probe" {
			t.Errorf("%s: probe identity %v", name, f.calls)
		}
	}
}

type fakeNodes struct {
	nodes []map[string]any
	class map[string]any // RuntimeClass gvisor; nil = not there
}

func (f fakeNodes) GetClusterScoped(_ context.Context, group, _, plural, name string) (map[string]any, error) {
	if group != "node.k8s.io" || plural != "runtimeclasses" || name != "gvisor" || f.class == nil {
		return nil, &kamajik8s.APIError{Status: 404, Message: plural + " " + name + " not found"}
	}
	return f.class, nil
}

func (f fakeNodes) ListNodes(context.Context) ([]map[string]any, error) { return f.nodes, nil }
func (f fakeNodes) SetNodeTaints(context.Context, string, string, []any) error {
	return errors.New("the gate never writes")
}
func (f fakeNodes) Server() string { return "https://10.20.0.214:6443" }

func TestOpenWorkers(t *testing.T) {
	node := func(name, ip string, taints ...any) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{"taints": taints},
			"status": map[string]any{"addresses": []any{map[string]any{"type": "InternalIP", "address": ip}}}}
	}
	open, err := openWorkers(context.Background(), fakeNodes{nodes: []map[string]any{
		node("w1", "10.110.0.10"),
		node("w2", "10.110.0.11", map[string]any{"key": pve.SandboxUnsecuredTaint, "effect": "NoSchedule"}),
		node("w3", "10.110.0.12", map[string]any{"key": "other", "effect": "NoSchedule"}),
	}}, false)
	if err != nil || len(open) != 2 || open["w1"] != "10.110.0.10" || open["w3"] != "10.110.0.12" {
		t.Fatalf("open = %v %v: every Node without the unsecured taint", open, err)
	}
}

// A shared cluster: the platform's own machines carry no unsecured taint and are no sandbox machines. The gate asks
// for the sandbox group's Nodes alone, and only while the RuntimeClass still keeps every sandbox on that group.
func TestOpenWorkersOnASharedCluster(t *testing.T) {
	node := func(name, group, ip string, taints ...any) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{sandbox.LabelNodeGroup: group, "proxcloud.io/node-group": "sandbox"}},
			"spec": map[string]any{"taints": taints}, "status": map[string]any{"addresses": []any{map[string]any{"type": "InternalIP", "address": ip}}}}
	}
	taint := func(key string) map[string]any {
		return map[string]any{"key": key, "value": "true", "effect": "NoSchedule"}
	}
	class := func(sel map[string]any, tolerate ...any) map[string]any {
		return map[string]any{"handler": "gvisor", "scheduling": map[string]any{"nodeSelector": sel, "tolerations": tolerate}}
	}
	group := map[string]any{sandbox.LabelNodeGroup: "sandbox"}
	good := class(group, map[string]any{"key": "proxcloud.io/sandbox", "operator": "Exists", "effect": "NoSchedule"})
	nodes := []map[string]any{
		// Every platform machine claims the kubelet label of the sandbox group: that label is not what the gate reads.
		node("pc-platform-cp-a", "", "192.168.140.20"), node("pc-platform-system-a", "system", "192.168.140.21"), node("pc-platform-kamaji-a", "kamaji", "192.168.140.22"),
		node("pc-platform-sandbox-a", "sandbox", "192.168.140.66", taint("proxcloud.io/sandbox")),
		node("pc-platform-sandbox-b", "sandbox", "192.168.140.67", taint("proxcloud.io/sandbox"), taint(pve.SandboxUnsecuredTaint)),
	}
	open, err := openWorkers(context.Background(), fakeNodes{nodes: nodes, class: good}, true)
	if err != nil || len(open) != 1 || open["pc-platform-sandbox-a"] != "192.168.140.66" {
		t.Fatalf("open = %v %v: the fenced sandbox machine and no platform machine", open, err)
	}
	// Read as the installer's cluster, the same Nodes fail the fence gate: every platform machine would be asked for.
	if open, _ := openWorkers(context.Background(), fakeNodes{nodes: nodes}, false); len(open) != 4 {
		t.Fatalf("without the mode every untainted Node is open: %v", open)
	}
	for name, rc := range map[string]map[string]any{
		"no class":                      nil,
		"no scheduling":                 {"handler": "gvisor"},
		"an empty selector":             class(map[string]any{}),
		"the kubelet's label":           class(map[string]any{"proxcloud.io/node-group": "sandbox"}),
		"another group":                 class(map[string]any{sandbox.LabelNodeGroup: "kamaji"}),
		"a second selector key":         class(map[string]any{sandbox.LabelNodeGroup: "sandbox", "kubernetes.io/os": "linux"}),
		"tolerates the unsecured taint": class(group, map[string]any{"key": "proxcloud.io/sandbox"}, map[string]any{"key": pve.SandboxUnsecuredTaint}),
		"tolerates every taint":         class(group, map[string]any{"operator": "Exists"}),
	} {
		if open, err := openWorkers(context.Background(), fakeNodes{nodes: nodes, class: rc}, true); err == nil {
			t.Errorf("%s: the gate went on with %v", name, open)
		}
	}
}
