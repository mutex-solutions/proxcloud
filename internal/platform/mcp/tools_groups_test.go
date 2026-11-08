package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
)

type sent struct {
	method, uri string
	body        map[string]any
}

// dispatchTo runs one tool through dispatch against a router that records the request.
func dispatchTo(t *testing.T, defs []toolDef, name string, args map[string]any) sent {
	t.Helper()
	var d *toolDef
	for i := range defs {
		if defs[i].name == name {
			d = &defs[i]
		}
	}
	if d == nil {
		t.Fatalf("no tool %q", name)
	}
	var got sent
	root := chi.NewRouter()
	root.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		got = sent{method: r.Method, uri: r.URL.RequestURI()}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			if err := json.Unmarshal(b, &got.body); err != nil {
				t.Fatalf("%s: body %q: %v", name, b, err)
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	h := testHandler()
	h.SetRoot(root)
	ctx := context.WithValue(context.Background(), credKey{}, cred{jwt: "x.y.z"})
	if _, _, err := h.dispatch(ctx, *d, args); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

type toolCase struct {
	tool string
	args map[string]any
	want sent
}

func runToolCases(t *testing.T, defs []toolDef, cases []toolCase) {
	t.Helper()
	for _, c := range cases {
		got := dispatchTo(t, defs, c.tool, c.args)
		if got.method != c.want.method || got.uri != c.want.uri || !reflect.DeepEqual(got.body, c.want.body) {
			t.Errorf("%s:\n got  %s %s %v\n want %s %s %v", c.tool, got.method, got.uri, got.body, c.want.method, c.want.uri, c.want.body)
		}
	}
}

const actionURI = "/api/v1/project/p1/cloud/r1/action"

var pr = map[string]any{"id": "p1", "resourceId": "r1"}

func with(m map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func TestClientComputeTools(t *testing.T) {
	spec := map[string]any{"name": "web", "flavorId": "f1", "imageId": "ubuntu-24", "networkIds": []any{"n1"}}
	runToolCases(t, clientTools, []toolCase{
		{"list_project_services", map[string]any{"id": "p1"}, sent{"GET", "/api/v1/project/p1/service", nil}},
		{"list_public_images", map[string]any{"id": "p1"}, sent{"POST", "/api/v1/project/p1/cloud/action", map[string]any{"action": "PUBLIC_IMAGES"}}},
		{"create_server", map[string]any{"id": "p1", "data": spec}, sent{"POST", "/api/v1/project/p1/cloud", map[string]any{"type": "SERVER", "data": spec}}},
		{"list_backups", pr, sent{"POST", actionURI, map[string]any{"action": "LIST_BACKUPS"}}},
		// A fixed action cannot be overridden by an argument of the same name.
		{"create_backup", with(pr, "action", "DELETE_BACKUP"), sent{"POST", actionURI, map[string]any{"action": "CREATE_BACKUP"}}},
		{"restore_server_backup", with(pr, "data", map[string]any{"id": "b1"}), sent{"POST", actionURI, map[string]any{"action": "RESTORE_BACKUP", "data": map[string]any{"id": "b1"}}}},
		{"delete_server_backup", with(pr, "data", map[string]any{"id": "b1"}), sent{"POST", actionURI, map[string]any{"action": "DELETE_BACKUP", "data": map[string]any{"id": "b1"}}}},
		{"create_server_snapshot", with(pr, "data", map[string]any{"name": "s1"}), sent{"POST", actionURI, map[string]any{"action": "CREATE_SNAPSHOT", "data": map[string]any{"name": "s1"}}}},
		{"rollback_server_snapshot", with(pr, "data", map[string]any{"name": "s1"}), sent{"POST", actionURI, map[string]any{"action": "ROLLBACK_SNAPSHOT", "data": map[string]any{"name": "s1"}}}},
		{"enable_server_protection", pr, sent{"POST", actionURI, map[string]any{"action": "ENABLE_PROTECTION"}}},
		{"disable_server_protection", pr, sent{"POST", actionURI, map[string]any{"action": "DISABLE_PROTECTION"}}},
	})
}

func TestClientManagedTools(t *testing.T) {
	groups := map[string]any{"nodeGroups": []any{map[string]any{"name": "workers", "flavorId": "f1", "count": float64(2)}}}
	record := map[string]any{"name": "www", "type": "A", "records": []any{"192.0.2.10"}}
	runToolCases(t, clientTools, []toolCase{
		{"list_kubernetes_clusters", map[string]any{"id": "p1"}, sent{"POST", "/api/v1/project/p1/resource?type=KUBERNETES_CLUSTER", nil}},
		// The create names its provider, or the API would pick the project's Proxmox service.
		{"create_kubernetes_cluster", map[string]any{"id": "p1", "serviceId": "k8s1", "data": groups}, sent{"POST", "/api/v1/project/p1/cloud?serviceId=k8s1", map[string]any{"type": "KUBERNETES_CLUSTER", "data": groups}}},
		{"get_kubeconfig", pr, sent{"POST", actionURI, map[string]any{"action": "GET_KUBECONFIG"}}},
		{"set_kubernetes_node_groups", with(pr, "data", groups), sent{"POST", actionURI, map[string]any{"action": "SET_NODE_GROUPS", "data": groups}}},
		{"upgrade_kubernetes_cluster", with(pr, "data", map[string]any{"version": "1.31"}), sent{"POST", actionURI, map[string]any{"action": "UPGRADE", "data": map[string]any{"version": "1.31"}}}},
		{"list_databases", map[string]any{"id": "p1"}, sent{"POST", "/api/v1/project/p1/resource?type=DATABASE_CLUSTER", nil}},
		{"create_database", map[string]any{"id": "p1", "serviceId": "db1", "data": map[string]any{"name": "d", "engine": "postgres"}}, sent{"POST", "/api/v1/project/p1/cloud?serviceId=db1", map[string]any{"type": "DATABASE_CLUSTER", "data": map[string]any{"name": "d", "engine": "postgres"}}}},
		{"list_dns_zones", map[string]any{"id": "p1"}, sent{"POST", "/api/v1/project/p1/resource?type=DNS_ZONE", nil}},
		{"list_dns_records", pr, sent{"POST", actionURI, map[string]any{"action": "LIST_RECORDS"}}},
		{"set_dns_record", with(pr, "data", record), sent{"POST", actionURI, map[string]any{"action": "SET_RECORD", "data": record}}},
		{"delete_dns_record", with(pr, "data", map[string]any{"name": "www", "type": "A"}), sent{"POST", actionURI, map[string]any{"action": "DELETE_RECORD", "data": map[string]any{"name": "www", "type": "A"}}}},
	})
	for _, d := range clientTools {
		if (d.name == "create_kubernetes_cluster" || d.name == "create_database") && !d.params[1].required {
			t.Errorf("%s: serviceId must be required", d.name)
		}
	}
}

func TestClientSandboxTools(t *testing.T) {
	runToolCases(t, clientTools, []toolCase{
		{"list_sandbox_templates", map[string]any{"id": "p1"}, sent{"GET", "/api/v1/project/p1/sandbox-templates", nil}},
		{"get_sandbox_access", pr, sent{"POST", actionURI, map[string]any{"action": "GET_ACCESS"}}},
		{"exec_in_sandbox", with(pr, "data", map[string]any{"cmd": "id"}), sent{"POST", actionURI, map[string]any{"action": "EXEC", "data": map[string]any{"cmd": "id"}}}},
		{"create_sandbox", map[string]any{"id": "p1", "data": map[string]any{"template": "base"}}, sent{"POST", "/api/v1/project/p1/cloud", map[string]any{"type": "SANDBOX", "data": map[string]any{"template": "base"}}}},
	})
}

func TestAdminOperatorCheckTools(t *testing.T) {
	runToolCases(t, adminAllTools(), []toolCase{
		// The handler decodes a JSON body, so the tool sends {} and the stored token is used.
		{"test_proxmox_connection", map[string]any{"externalServiceId": "pve1"}, sent{"POST", "/api/v1/admin/service/proxmox/auth?externalServiceId=pve1", map[string]any{}}},
		{"test_object_store", map[string]any{"id": "s3"}, sent{"POST", "/api/v1/admin/service/s3/object-store/test", nil}},
		{"get_sandbox_release_gates", map[string]any{"id": "sb"}, sent{"POST", "/api/v1/admin/service/sb/sandbox/release-check", nil}},
		{"list_billing_profile_suspensions", map[string]any{"billingProfileId": "bp1"}, sent{"GET", "/api/v1/admin/suspensions/bp1", nil}},
	})
	for _, name := range []string{"test_proxmox_connection", "test_object_store", "get_sandbox_release_gates"} {
		for _, d := range adminAllTools() {
			if d.name == name && !toolAnnotations(d).ReadOnlyHint {
				t.Errorf("%s: a check that stores nothing must be annotated read-only", name)
			}
		}
	}
}
