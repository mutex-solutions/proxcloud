//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// repairKube is a management cluster with the worker Machines of cluster pc-c-a (project p1) and one Machine of
// another cluster in the same namespace. forbid: the service account has no delete on Machines (an older ClusterRole).
type repairKube struct {
	kamaji.K8sAPI
	mu       sync.Mutex
	machines []map[string]any
	infra    []map[string]any
	deleted  []string
	forbid   bool
}

func (k *repairKube) GetApplication(_ context.Context, _, name string) (map[string]any, error) {
	if name != "pc-c-a" {
		return nil, nil
	}
	return map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{
		kamaji.LabelManagedBy: kamaji.ManagedByValue, kamaji.LabelService: "k8s", kamaji.LabelProject: "p1"}}}, nil
}
func (k *repairKube) ListCRs(_ context.Context, _, _, plural, ns, _ string) ([]map[string]any, error) {
	switch {
	case ns != "pc-p1":
		return nil, nil
	case plural == "machines":
		return k.machines, nil
	case plural == "proxmoxmachines":
		return k.infra, nil
	}
	return nil, nil
}
func (k *repairKube) ListSecrets(context.Context, string, string) ([]map[string]any, error) {
	return []map[string]any{{"metadata": map[string]any{"name": kamaji.CloudSecretName("pc-c-a"),
		"annotations": map[string]any{"proxcloud.io/compute-service": "pve"}}}}, nil
}
func (k *repairKube) GetTenantControlPlane(context.Context, string, string) (map[string]any, error) {
	return nil, nil
}
func (k *repairKube) ListTenantControlPlanes(context.Context, string, string) ([]map[string]any, error) {
	return nil, nil
}
func (k *repairKube) DeleteCR(_ context.Context, _, _, plural, ns, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.forbid {
		return &kamajik8s.APIError{Status: http.StatusForbidden, Message: `machines.cluster.x-k8s.io "` + name + `" is forbidden`}
	}
	k.deleted = append(k.deleted, plural+":"+ns+"/"+name)
	return nil
}
func (k *repairKube) gone() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return strings.Join(k.deleted, ",")
}

// The two ways to replace one worker and the operator's join error, through the routes: who may call them, which
// machine they may touch, and when a guest is read.
func TestWorkerRepairRoutes(t *testing.T) {
	db := freshPG(t)
	ago := func(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }
	machine := func(cluster, name, phase string, age time.Duration, ready bool) map[string]any {
		m := map[string]any{
			"metadata": map[string]any{"name": name, "creationTimestamp": ago(age),
				"labels": map[string]any{"cluster.x-k8s.io/cluster-name": cluster, "proxcloud.io/node-group": "workers"}},
			"spec":   map[string]any{"infrastructureRef": map[string]any{"name": name}},
			"status": map[string]any{"phase": phase, "lastUpdated": ago(age)},
		}
		if ready {
			m["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "NodeReady", "status": "True"}}
		}
		return m
	}
	vm := func(name string, vmid int) map[string]any {
		pm := map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{},
			"status": map[string]any{"proxmoxNode": "pve1", "conditions": []any{map[string]any{"type": "VirtualMachineProvisioned", "status": "True"}}}}
		if vmid != 0 {
			pm["spec"].(map[string]any)["virtualMachineID"] = float64(vmid)
		}
		return pm
	}
	kube := &repairKube{
		machines: []map[string]any{
			machine("pc-c-a", "old", "Provisioned", 3*time.Hour, false),     // never joined, past its bound
			machine("pc-c-a", "new", "Provisioned", time.Minute, false),     // not joined yet, inside its bound
			machine("pc-c-a", "novm", "Provisioned", 3*time.Hour, false),    // past its bound, CAPMOX names no VM
			machine("pc-c-a", "ready", "Running", 3*time.Hour, true),        // the only Ready node of the group
			machine("pc-c-b", "foreign", "Provisioned", 3*time.Hour, false), // another cluster of the namespace
		},
		infra: []map[string]any{vm("old", 141), vm("new", 142), vm("novm", 0), vm("ready", 143), vm("foreign", 144)},
	}

	var mu sync.Mutex
	reads := map[string]int{}
	proxmox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		reads[path]++
		if r.Method != http.MethodGet || path != "/nodes/pve1/qemu/141/agent/file-read" {
			t.Errorf("the guest of a machine that is not past its bound was read: %s %s", r.Method, path)
			http.Error(w, "no fake", http.StatusNotImplemented)
			return
		}
		if r.URL.Query().Get("file") == "/run/cloud-init/result.json" {
			fmt.Fprint(w, `{"data":{"content":"{\"v1\":{\"errors\":[\"runcmd failed\"]}}"}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"content":"kubeadm join --token abcdef.0123456789abcdef\ndial tcp 192.168.140.101:6443: i/o timeout\n"}}`)
	}))
	t.Cleanup(proxmox.Close)

	orgRepo, billingRepo, users, cloudRepo := org.NewRepo(db), billing.NewRepo(db), user.NewRepo(db), cloud.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key"))
	kamajiFor := func(es *externalservice.ExternalService) (*kamaji.Service, error) {
		return kamaji.NewWithAPI(kube, es.KamajiConfig(), es.ID), nil
	}
	ph := project.NewHandler(project.NewService(project.NewRepo(db), orgRepo, billingRepo, users, nil), project.NewPolicy(org.NewPolicy(orgRepo)),
		org.NewService(orgRepo, billingRepo, nil), users, audit.NewService(audit.NewRepo(db), slog.Default()), billingRepo, cloudRepo, esSvc, nil, nil, "r1")
	ph.SetKamaji(kamajiFor)
	// LOCAL_IDP posture: the role comes from the adminPermission doc of the sub.
	ah := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, esSvc, "r1", "", "")
	ah.SetKamaji(kamajiFor)
	ah.SetProxmox(func(*externalservice.ExternalService) (*pve.Service, error) {
		return pve.NewService(pve.Config{APIURL: proxmox.URL, TokenID: pve.DefaultPlatformTokenID, Secret: "s"}, "pve", "pc-")
	})
	router := chi.NewRouter()
	ph.Routes(router)
	ah.Routes(router)

	const owner = "owner-sub"
	k8sSvc := pgdoc.M{"serviceId": "k8s", "provider": "kamaji", "region": "r1"}
	cluster := func(row, projectID, id string) pgdoc.M {
		return pgdoc.M{"_id": row, "serviceId": "k8s", "projectId": projectID, "region": "r1", "type": cloud.TypeKubernetesCluster,
			"externalId": id, "data": pgdoc.M{"name": "demo"}}
	}
	for coll, docs := range map[string][]pgdoc.M{
		"users":                {{"sub": owner, "email": "o@example.org"}},
		"organization":         {{"_id": "o1", "name": "o", "billingProfileId": "bp1"}},
		"organization_members": {{"organizationId": "o1", "sub": owner, "roles": []string{"OWNER"}}},
		"adminPermission":      {{"_id": "support-sub", "role": "SUPPORT"}, {"_id": "admin-sub", "role": "ADMIN"}},
		"externalService": {
			{"_id": "pve", "name": "pve", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
				"config": pgdoc.M{"provider": "proxmox", "regions": pgdoc.M{"r1": pgdoc.M{}}}},
			{"_id": "k8s", "name": "k8s", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": pgdoc.M{
				"provider": "kamaji", "region": "r1", "cluster": pgdoc.M{"versions": pgdoc.M{"1.35.10": "pve1=9200"}}}},
		},
		"project": {
			{"_id": "p1", "name": "p", "status": project.StatusEnabled, "organizationId": "o1",
				"memberships": []pgdoc.M{{"sub": owner, "role": project.RoleOwner}}, "services": []any{k8sSvc}},
			{"_id": "p2", "name": "q", "status": project.StatusEnabled, "organizationId": "o1", "services": []any{k8sSvc}},
			{"_id": "p3", "name": "off", "status": project.StatusDisabled, "organizationId": "o1",
				"memberships": []pgdoc.M{{"sub": owner, "role": project.RoleOwner}}, "services": []any{k8sSvc}},
		},
		// row-other is a cluster of p2 whose id is the one the fake knows: reaching it through p1 would delete.
		"cloudResource": {cluster("row-a", "p1", "pc-c-a"), cluster("row-other", "p2", "pc-c-a"), cluster("row-off", "p3", "pc-c-a")},
	} {
		for _, d := range docs {
			mustInsert(t, db, coll, d)
		}
	}
	call := func(sub, method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("x-service-id", "k8s")
		req.Header.Set("x-region-id", "r1")
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	want := func(what string, code int, body string, status int, deleted string) {
		t.Helper()
		if code != status || kube.gone() != deleted {
			t.Fatalf("%s: %d %s, want %d; deleted %q, want %q", what, code, body, status, kube.gone(), deleted)
		}
	}

	// The tenant action.
	act := func(projectID, row, data string) (int, string) {
		return call(owner, http.MethodPost, "/project/"+projectID+"/cloud/"+row+"/action", `{"action":"REPLACE_MACHINE","data":`+data+`}`)
	}
	code, body := act("p1", "row-a", `{}`)
	want("no machine named", code, body, http.StatusBadRequest, "")
	code, body = act("p1", "row-other", `{"machine":"old"}`)
	want("another project's cluster", code, body, http.StatusNotFound, "")
	code, body = act("p3", "row-off", `{"machine":"old"}`)
	want("a suspended project", code, body, http.StatusConflict, "")
	code, body = act("p1", "row-a", `{"machine":"foreign"}`)
	want("a machine of another cluster", code, body, http.StatusNotFound, "")
	code, body = act("p1", "row-a", `{"machine":"ready"}`)
	want("the last Ready node, not confirmed", code, body, http.StatusConflict, "")
	if !strings.Contains(body, "last_ready_node") {
		t.Errorf("the refusal carries no reason: %s", body)
	}
	code, body = act("p1", "row-a", `{"machine":"ready","confirm":true}`)
	want("the last Ready node, confirmed", code, body, http.StatusOK, "machines:pc-p1/ready")

	// The operator's routes: reading shows the machines and the join error, replacing needs admin:service:manage.
	const machinesPath = "/admin/service/k8s/k8s-clusters/pc-c-a/machines"
	for _, sub := range []string{owner, "nobody"} {
		if code, _ := call(sub, http.MethodGet, machinesPath, ""); code != http.StatusForbidden {
			t.Fatalf("%s read the machines: %d", sub, code)
		}
		code, body = call(sub, http.MethodPost, machinesPath+"/old/replace", "")
		want(sub+" replaces", code, body, http.StatusForbidden, "machines:pc-p1/ready")
	}
	code, body = call("support-sub", http.MethodPost, machinesPath+"/old/replace", "")
	want("admin:service:read replaces", code, body, http.StatusForbidden, "machines:pc-p1/ready")

	code, body = call("support-sub", http.MethodGet, machinesPath, "")
	if code != http.StatusOK {
		t.Fatalf("admin:service:read reads the machines: %d %s", code, body)
	}
	var view struct {
		Data kamaji.ClusterMachines `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatal(err)
	}
	joinErrors := map[string]*kamaji.JoinError{}
	for _, m := range view.Data.Machines {
		joinErrors[m.Name] = m.JoinError
	}
	if len(joinErrors) != 4 {
		t.Fatalf("machines of the cluster: %s", body)
	}
	// Past its bound: cloud-init's result and the end of its log, read through the Proxmox provider the cluster's
	// secret names, with the join token masked.
	if je := joinErrors["old"]; je == nil || je.ReadError != "" || len(je.Errors) != 1 || !strings.Contains(je.LogTail, "i/o timeout") || strings.Contains(body, "abcdef.0123456789abcdef") {
		t.Errorf("the machine past its bound: %+v\n%s", je, body)
	}
	// Inside its bound, or Ready: nothing is read from the guest (the fake fails the test on any other VM).
	if joinErrors["new"] != nil || joinErrors["ready"] != nil {
		t.Errorf("a machine that needs no attention carries a join error: %s", body)
	}
	if je := joinErrors["novm"]; je == nil || !strings.Contains(je.ReadError, "no VM") {
		t.Errorf("a machine without a VM: %+v", je)
	}
	mu.Lock()
	if n := reads["/nodes/pve1/qemu/141/agent/file-read"]; n != 2 || len(reads) != 1 {
		t.Errorf("guest reads: %v, want the two files of VM 141 only", reads)
	}
	mu.Unlock()

	// A ClusterRole older than the action: the operator reads what to apply, the tenant does not.
	kube.forbid = true
	code, body = call("admin-sub", http.MethodPost, machinesPath+"/old/replace", "")
	want("an old ClusterRole, operator", code, body, http.StatusBadGateway, "machines:pc-p1/ready")
	if !strings.Contains(body, "deploy/mgmt-cluster/rbac.yaml") || !strings.Contains(body, "0.1.3") || strings.Contains(body, "Contact the operator") {
		t.Errorf("the operator is not told what to apply: %s", body)
	}
	code, body = act("p1", "row-a", `{"machine":"old"}`)
	want("an old ClusterRole, tenant", code, body, http.StatusBadGateway, "machines:pc-p1/ready")
	if strings.Contains(body, "rbac.yaml") || !strings.Contains(body, "Contact the operator") {
		t.Errorf("the tenant's answer: %s", body)
	}
	kube.forbid = false
	code, body = call("admin-sub", http.MethodPost, machinesPath+"/old/replace", "")
	want("admin:service:manage replaces", code, body, http.StatusOK, "machines:pc-p1/ready,machines:pc-p1/old")
}
