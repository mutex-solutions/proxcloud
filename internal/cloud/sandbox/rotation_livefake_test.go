package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type rotateAPI struct {
	*fakeAPI
	nodes, pods, crsAll []map[string]any
	cordons             []string
	failNode            string
}

func (a *rotateAPI) ListNodes(context.Context) ([]map[string]any, error) { return a.nodes, nil }
func (a *rotateAPI) ListPodsAllNamespaces(context.Context, string) ([]map[string]any, error) {
	return a.pods, nil
}
func (a *rotateAPI) ListCRsAllNamespaces(context.Context, string, string, string, string) ([]map[string]any, error) {
	return a.crsAll, nil
}
func (a *rotateAPI) SetNodeUnschedulable(_ context.Context, n string, b bool) error {
	a.cordons = append(a.cordons, fmt.Sprintf("%s:%t", n, b))
	if n == a.failNode {
		return fmt.Errorf("failed")
	}
	return nil
}
func TestRotationDryRunWaitsAndOnlyPatchesExactBeta2Scope(t *testing.T) {
	for _, scenario := range []string{"empty", "sandbox", "build", "pending-build", "foreign-role", "system", "wrong-uid", "cordoned"} {
		t.Run(scenario, func(t *testing.T) {
			writes := 0
			md := map[string]any{"metadata": map[string]any{"labels": map[string]any{"cluster.x-k8s.io/cluster-name": "sandbox"}}, "spec": map[string]any{"clusterName": "sandbox", "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{WorkerRoleLabel: "true"}}}}}
			machine := map[string]any{"metadata": map[string]any{"labels": map[string]any{WorkerRoleLabel: "true"}}, "spec": map[string]any{"clusterName": "sandbox"}, "status": map[string]any{"nodeRef": map[string]any{"name": "worker", "uid": "node-uid"}}}
			node := map[string]any{"metadata": map[string]any{"name": "worker", "uid": "node-uid", "labels": map[string]any{WorkerRoleLabel: "true"}}}
			api := &rotateAPI{fakeAPI: &fakeAPI{}, nodes: []map[string]any{node}}
			switch scenario {
			case "sandbox":
				api.crsAll = []map[string]any{{"metadata": map[string]any{"name": "customer"}}}
			case "build", "pending-build":
				p := map[string]any{"metadata": map[string]any{"namespace": BuildNamespace}, "spec": map[string]any{"nodeName": "worker"}, "status": map[string]any{"phase": "Running"}}
				if scenario == "pending-build" {
					p["spec"] = map[string]any{}
				}
				api.pods = []map[string]any{p}
			case "foreign-role":
				dig(node, "metadata", "labels").(map[string]any)[WorkerRoleLabel] = "false"
			case "system":
				dig(node, "metadata", "labels").(map[string]any)["node-restriction.kubernetes.io/proxcloud-system"] = "true"
			case "wrong-uid":
				dig(node, "metadata").(map[string]any)["uid"] = "other"
			case "cordoned":
				node["spec"] = map[string]any{"unschedulable": true}
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
					t.Error("dry run wrote", r.URL.Path)
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/machinedeployments/md-worker"):
					_ = json.NewEncoder(w).Encode(md)
				case strings.HasSuffix(r.URL.Path, "/machines"):
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{machine}})
				default:
					t.Error("escaped scope", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
			kc := fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: x\nclusters:\n- name: x\n  cluster: {server: %s, certificate-authority-data: %s}\ncontexts:\n- name: x\n  context: {cluster: x, user: x}\nusers:\n- name: x\n  user: {token: token}\n", server.URL, ca)
			s := NewWithAPI(api, Config{Rotation: RotationPolicy{Enabled: true, IntervalHours: 24, Namespace: "pc-sandbox", Cluster: "sandbox", MachineDeployments: []string{"md-worker"}, Kubeconfig: kc}}, "svc")
			plan, err := s.RotationInspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if plan.Ready != (scenario == "empty") {
				t.Fatalf("%s: %+v", scenario, plan)
			}
			if writes != 0 || len(api.cordons) != 0 {
				t.Fatal("dryrun mutated")
			}
		})
	}
}
func TestCordonFailureRollsBackOnlyChangedNodes(t *testing.T) {
	a := &rotateAPI{fakeAPI: &fakeAPI{}, failNode: "b"}
	s := NewWithAPI(a, Config{}, "s")
	if s.CordonRotationNodes(context.Background(), []string{"a", "b", "c"}, true) == nil {
		t.Fatal("expected error")
	}
	if strings.Join(a.cordons, ",") != "a:true,b:true,a:false" {
		t.Fatal(a.cordons)
	}
}
