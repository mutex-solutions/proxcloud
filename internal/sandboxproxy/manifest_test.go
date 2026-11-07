package sandboxproxy

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// The proxy holds the wildcard key and the wake token, and a node's kubelet can read the Secrets of the pods bound to
// it: the Deployment must be able to land on a system node only (a label the kubelet cannot set), never on a worker
// that runs tenant code, even when no system node exists.
func TestProxyRunsOnSystemNodesOnly(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/sandbox-cluster/sandbox-proxy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	type expr struct{ Key, Operator string }
	var dep struct {
		Kind     string
		Metadata struct{ Name string }
		Spec     struct {
			Template struct {
				Spec struct {
					NodeSelector map[string]string `yaml:"nodeSelector"`
					Tolerations  []struct{ Key, Operator, Effect string }
					Affinity     struct {
						NodeAffinity struct {
							Required *struct {
								NodeSelectorTerms []struct {
									MatchExpressions []expr `yaml:"matchExpressions"`
								} `yaml:"nodeSelectorTerms"`
							} `yaml:"requiredDuringSchedulingIgnoredDuringExecution"`
							Preferred []any `yaml:"preferredDuringSchedulingIgnoredDuringExecution"`
						} `yaml:"nodeAffinity"`
					}
				}
			}
		}
	}
	found := false
	for d := yaml.NewDecoder(bytes.NewReader(raw)); ; {
		var doc yaml.Node
		if err := d.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if err := doc.Decode(&dep); err != nil {
			t.Fatal(err)
		}
		if dep.Kind == "Deployment" && dep.Metadata.Name == "sandbox-proxy" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no sandbox-proxy Deployment")
	}
	ps := dep.Spec.Template.Spec
	req := ps.Affinity.NodeAffinity.Required
	if req == nil || len(req.NodeSelectorTerms) == 0 {
		t.Fatal("no required node affinity: the proxy could land next to tenant code")
	}
	// Terms are ORed: every one of them must demand the system label.
	for i, term := range req.NodeSelectorTerms {
		ok := false
		for _, e := range term.MatchExpressions {
			ok = ok || (e.Key == "node-restriction.kubernetes.io/proxcloud-system" && e.Operator == "Exists")
		}
		if !ok {
			t.Errorf("node selector term %d does not require the system label: %+v", i, term)
		}
	}
	if len(ps.Affinity.NodeAffinity.Preferred) > 0 {
		t.Error("a preferred term means a fallback onto other nodes")
	}
	for _, tol := range ps.Tolerations {
		if tol.Key != "proxcloud.io/sandbox-system" {
			t.Errorf("toleration %+v: the proxy tolerates the system taint alone (not unsecured, not everything)", tol)
		}
	}
}
