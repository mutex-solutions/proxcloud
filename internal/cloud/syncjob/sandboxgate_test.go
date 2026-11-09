package syncjob

import (
	"context"
	"errors"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
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

type fakeNodes struct{ nodes []map[string]any }

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
	}})
	if err != nil || len(open) != 2 || open["w1"] != "10.110.0.10" || open["w3"] != "10.110.0.12" {
		t.Fatalf("open = %v %v: every Node without the unsecured taint", open, err)
	}
}
