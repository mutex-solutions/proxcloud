package ciliumcni

import (
	"encoding/json"
	"testing"
)

// platformCM is cilium-config as the platform cluster renders it (cni.customConf off), read on the lab.
var platformCM = map[string]string{"custom-cni-conf": "false", "write-cni-conf-when-ready": "/host/etc/cni/net.d/05-cilium.conflist", "cni-exclusive": "true"}

func ncs(t *testing.T, s string) []NodeConfig {
	t.Helper()
	var out []NodeConfig
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCustomConf(t *testing.T) {
	sandbox := map[string]string{"proxcloud.io/node-group": "sandbox"}
	system := map[string]string{"proxcloud.io/node-group": "system"}
	perNode := `[{"metadata":{"name":"sandbox-nodes"},"spec":{"nodeSelector":{"matchLabels":{"proxcloud.io/node-group":"sandbox"}},"defaults":{"custom-cni-conf":"true","write-cni-conf-when-ready":"","enable-bandwidth-manager":"true"}}}]`
	for _, c := range []struct {
		name   string
		cm     map[string]string
		ncs    string
		labels map[string]string
		want   bool
	}{
		{"cluster-wide", map[string]string{"custom-cni-conf": "true"}, `[]`, system, true},
		{"cluster-wide, empty writer", map[string]string{"custom-cni-conf": "true", "write-cni-conf-when-ready": ""}, `[]`, system, true},
		{"per-node on a sandbox node", platformCM, perNode, sandbox, true},
		{"per-node, other node", platformCM, perNode, system, false},
		{"neither", platformCM, `[]`, sandbox, false},
		{"per-node without write-cni-conf-when-ready", platformCM, `[{"metadata":{"name":"s"},"spec":{"nodeSelector":{"matchLabels":{"proxcloud.io/node-group":"sandbox"}},"defaults":{"custom-cni-conf":"true"}}}]`, sandbox, false},
		{"per-node without custom-cni-conf", platformCM, `[{"metadata":{"name":"s"},"spec":{"nodeSelector":{},"defaults":{"write-cni-conf-when-ready":""}}}]`, sandbox, false},
		{"nil selector matches nothing", platformCM, `[{"metadata":{"name":"s"},"spec":{"defaults":{"custom-cni-conf":"true","write-cni-conf-when-ready":""}}}]`, sandbox, false},
		{"empty selector matches every node", platformCM, `[{"metadata":{"name":"s"},"spec":{"nodeSelector":{},"defaults":{"custom-cni-conf":"true","write-cni-conf-when-ready":""}}}]`, system, true},
		{"a later name turns it off again", platformCM, perNode[:len(perNode)-1] + `,{"metadata":{"name":"zz"},"spec":{"nodeSelector":{},"defaults":{"custom-cni-conf":"false"}}}]`, sandbox, false},
		{"an earlier name is overridden", platformCM, perNode[:len(perNode)-1] + `,{"metadata":{"name":"aa"},"spec":{"nodeSelector":{},"defaults":{"custom-cni-conf":"false"}}}]`, sandbox, true},
		{"cluster-wide undone for the node", map[string]string{"custom-cni-conf": "true"}, `[{"metadata":{"name":"s"},"spec":{"nodeSelector":{},"defaults":{"custom-cni-conf":"false"}}}]`, system, false},
	} {
		if got := CustomConf(c.cm, ncs(t, c.ncs), c.labels); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestSelectorExpressions(t *testing.T) {
	l := map[string]string{"g": "sandbox", "z": "a"}
	for s, want := range map[string]bool{
		`{"matchExpressions":[{"key":"g","operator":"In","values":["sandbox","x"]}]}`:                     true,
		`{"matchExpressions":[{"key":"g","operator":"In","values":["x"]}]}`:                               false,
		`{"matchExpressions":[{"key":"g","operator":"NotIn","values":["x"]}]}`:                            true,
		`{"matchExpressions":[{"key":"q","operator":"NotIn","values":["x"]}]}`:                            true,
		`{"matchExpressions":[{"key":"g","operator":"NotIn","values":["sandbox"]}]}`:                      false,
		`{"matchExpressions":[{"key":"z","operator":"Exists"}]}`:                                          true,
		`{"matchExpressions":[{"key":"q","operator":"Exists"}]}`:                                          false,
		`{"matchExpressions":[{"key":"q","operator":"DoesNotExist"}]}`:                                    true,
		`{"matchExpressions":[{"key":"z","operator":"DoesNotExist"}]}`:                                    false,
		`{"matchExpressions":[{"key":"z","operator":"Gt","values":["1"]}]}`:                               false,
		`{"matchLabels":{"g":"sandbox"},"matchExpressions":[{"key":"z","operator":"In","values":["b"]}]}`: false,
	} {
		var sel Selector
		if err := json.Unmarshal([]byte(s), &sel); err != nil {
			t.Fatal(err)
		}
		if sel.Matches(l) != want {
			t.Errorf("%s: want %v", s, want)
		}
	}
}
