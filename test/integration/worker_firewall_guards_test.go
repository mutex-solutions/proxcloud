//go:build integration

package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
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

// wfwRig is the project handler over a fresh database and a Proxmox that holds three security groups of project p1:
// the group of its cluster pc-c-a, a group the tenant named k8s-ingress before those names were reserved, and a group
// named after pc-c-z, a cluster of another project.
type wfwRig struct {
	t      *testing.T
	db     *pgdoc.DB
	h      *project.Handler
	ps     *pve.Service
	router http.Handler

	mu     sync.Mutex
	vms    string   // the qemu rows of /cluster/resources
	writes []string // every call that is not a GET
}

func newWFWRig(t *testing.T) *wfwRig {
	t.Helper()
	g := &wfwRig{t: t, db: freshPG(t)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		switch {
		case r.Method != http.MethodGet:
			g.writes = append(g.writes, r.Method+" "+path)
			fmt.Fprint(w, `{"data":null}`)
		case path == "/cluster/firewall/options":
			fmt.Fprint(w, `{"data":{"enable":1}}`)
		case path == "/cluster/resources" && r.URL.Query().Get("type") == "node":
			fmt.Fprint(w, `{"data":[{"type":"node","node":"pve1","status":"online"}]}`)
		case path == "/cluster/resources":
			fmt.Fprintf(w, `{"data":[%s]}`, g.vms)
		case path == "/cluster/firewall/groups":
			fmt.Fprint(w, `{"data":[{"group":"gcluster","comment":"st:p1:k8s-pc-c-a"},{"group":"glegacy","comment":"st:p1:k8s-ingress"},{"group":"gz","comment":"st:p1:k8s-pc-c-z"}]}`)
		case strings.HasSuffix(path, "/config"):
			fmt.Fprint(w, `{"data":{"net0":"virtio=BC:24:11:00:00:01,bridge=vtenant"}}`)
		case strings.HasSuffix(path, "/firewall/options"):
			fmt.Fprint(w, `{"data":{}}`)
		default: // rules of a group or a VM, IP sets
			fmt.Fprint(w, `{"data":[]}`)
		}
	}))
	t.Cleanup(srv.Close)
	var err error
	if g.ps, err = pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "pve", "pc-"); err != nil {
		t.Fatal(err)
	}
	orgRepo := org.NewRepo(g.db)
	billingRepo := billing.NewRepo(g.db)
	users := user.NewRepo(g.db)
	esSvc := externalservice.NewService(externalservice.NewRepo(g.db), textcrypt.New("test-key"))
	g.h = project.NewHandler(project.NewService(project.NewRepo(g.db), orgRepo, billingRepo, users, nil), project.NewPolicy(org.NewPolicy(orgRepo)),
		org.NewService(orgRepo, billingRepo, nil), users, audit.NewService(audit.NewRepo(g.db), slog.Default()), billingRepo,
		cloud.NewRepo(g.db), esSvc, nil, nil, "r1")
	g.h.SetProxmox(func(*externalservice.ExternalService) (*pve.Service, error) { return g.ps, nil })
	g.h.SetPublicIPs(project.NewPublicIPRepo(g.db))
	router := chi.NewRouter()
	g.h.Routes(router)
	g.router = router
	sg := func(id, ext, name string) pgdoc.M {
		return pgdoc.M{"_id": id, "serviceId": "pve", "projectId": "p1", "region": "r1", "type": cloud.TypeSecurityGroup,
			"externalId": ext, "data": pgdoc.M{"securityGroup": pgdoc.M{"id": ext, "name": name}}}
	}
	cluster := func(id, projectID string) pgdoc.M {
		return pgdoc.M{"_id": "row-" + id, "serviceId": "k8s", "projectId": projectID, "region": "r1", "type": cloud.TypeKubernetesCluster,
			"externalId": id, "data": pgdoc.M{"name": "demo"}}
	}
	reserved := func(clusterID, ip string) pgdoc.M {
		return pgdoc.M{"_id": "pve/vtenant/" + ip, "serviceId": "pve", "networkId": "vtenant", "ip": ip, "prefix": 24,
			"projectId": "p1", "serverId": "k8s:" + clusterID, "nic": project.ReservedNIC}
	}
	for coll, docs := range map[string][]pgdoc.M{
		"users":                {{"sub": "owner-sub", "email": "o@example.org"}},
		"organization":         {{"_id": "o1", "name": "o"}},
		"organization_members": {{"organizationId": "o1", "sub": "owner-sub", "roles": []string{"OWNER"}}},
		"externalService": {
			{"_id": "pve", "name": "pve", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
				"config": pgdoc.M{"provider": "proxmox", "regions": pgdoc.M{"r1": pgdoc.M{}}}},
			{"_id": "k8s", "name": "k8s", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
				"config": pgdoc.M{"provider": "kamaji", "region": "r1"}},
		},
		"project": {{"_id": "p1", "name": "p", "status": project.StatusEnabled, "organizationId": "o1",
			"memberships": []pgdoc.M{{"sub": "owner-sub", "role": project.RoleOwner}},
			"services": []any{
				pgdoc.M{"serviceId": "pve", "provider": "proxmox", "region": "r1", "pool": "pc-p1"},
				pgdoc.M{"serviceId": "k8s", "provider": "kamaji", "region": "r1"},
			}}},
		"cloudResource": {cluster("pc-c-a", "p1"), cluster("pc-c-z", "p2"),
			sg("sg-cluster", "gcluster", "k8s-pc-c-a"), sg("sg-legacy", "glegacy", "k8s-ingress")},
		"pvePublicIp": {reserved("pc-c-a", "10.100.0.232"), reserved("pc-c-z", "10.100.0.240")},
	} {
		for _, doc := range docs {
			mustInsert(t, g.db, coll, doc)
		}
	}
	return g
}

func (g *wfwRig) do(method, path, body string) (int, string) {
	g.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "owner-sub"}))
	w := httptest.NewRecorder()
	g.router.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func (g *wfwRig) wrote() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := g.writes
	g.writes = nil
	return out
}

// A group named k8s-<cluster> is a cluster's switch, so a tenant cannot make one, nor delete the one of a cluster the
// project has. A group of that form that is no cluster's (made under 0.8.x, when the names were free) stays the
// tenant's: it can be deleted.
func TestWorkerGroupNamesAreTheClusters(t *testing.T) {
	g := newWFWRig(t)
	code, body := g.do(http.MethodPost, "/project/p1/cloud", `{"type":"SECURITY_GROUP","serviceId":"pve","region":"r1","data":{"name":"k8s-mine"}}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "reserved") {
		t.Fatalf("create of a k8s- group: %d %s", code, body)
	}
	code, body = g.do(http.MethodDelete, "/project/p1/cloud/sg-cluster", "")
	if code != http.StatusConflict || !strings.Contains(body, "belongs to a Kubernetes cluster") {
		t.Fatalf("delete of the cluster's group: %d %s", code, body)
	}
	if w := g.wrote(); len(w) != 0 {
		t.Fatalf("refused requests reached Proxmox: %v", w)
	}
	code, body = g.do(http.MethodDelete, "/project/p1/cloud/sg-legacy", "")
	if w := g.wrote(); code >= 300 || len(w) != 1 || w[0] != "DELETE /cluster/firewall/groups/glegacy" {
		t.Fatalf("delete of the tenant's own k8s-ingress: %d %s, wrote %v", code, body, w)
	}
}

// The provider sync acts on a group only for a cluster the group's project has. Here project p1 holds a group named
// after pc-c-z, a cluster of project p2, and a VM in p1's pool carries that cluster's tag: the pass leaves it alone.
func TestWorkerFirewallSyncNeedsTheProjectsCluster(t *testing.T) {
	g := newWFWRig(t)
	es := &externalservice.ExternalService{ID: "pve"}
	vm := func(tag string) string {
		return `{"type":"qemu","vmid":301,"node":"pve1","name":"w","pool":"pc-p1","status":"running","tags":"proxcloud;pc-k8s-` + tag + `"}`
	}
	g.vms = vm("pc-c-z")
	if err := g.h.ReconcileWorkerFirewalls(context.Background(), g.ps, es); err != nil {
		t.Fatal(err)
	}
	if w := g.wrote(); len(w) != 0 {
		t.Fatalf("a group that is no cluster of its project switched a firewall on: %v", w)
	}
	// The project's own cluster: the pass writes.
	g.vms = vm("pc-c-a")
	_ = g.h.ReconcileWorkerFirewalls(context.Background(), g.ps, es)
	if w := g.wrote(); len(w) == 0 {
		t.Fatal("the worker of the project's protected cluster was not written")
	}
}

// wfwKube is a management cluster with the Application of cluster pc-c-a.
type wfwKube struct {
	kamaji.K8sAPI
	service, pool string
}

func (k *wfwKube) GetApplication(_ context.Context, _, name string) (map[string]any, error) {
	if name != "pc-c-a" {
		return nil, nil
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "labels": map[string]any{
			kamaji.LabelManagedBy: kamaji.ManagedByValue, kamaji.LabelService: k.service, kamaji.LabelProject: "p1"}},
		"spec": map[string]any{"source": map[string]any{"helm": map[string]any{"valuesObject": map[string]any{
			"proxmox": map[string]any{"pool": k.pool, "metallb": map[string]any{"addresses": []any{"10.100.0.248/32"}}}}}}},
	}, nil
}

// The worker firewall acts on a cluster of this provider whose workers are in its project's pool, and on no other.
func TestWorkerFirewallTarget(t *testing.T) {
	g := newWFWRig(t)
	ctx := context.Background()
	kube := &wfwKube{service: "k8s", pool: "pc-p1"}
	ks := kamaji.NewWithAPI(kube, kamaji.Config{}, "k8s")
	status := func(err error) int {
		if he, ok := err.(*httpx.HTTPError); ok {
			return he.Status
		}
		return 0
	}
	rep, err := g.h.ClusterWorkerFirewall(ctx, ks, "pc-c-a")
	if err != nil || !rep.Protected || rep.Group != "gcluster" || rep.GroupName != "k8s-pc-c-a" || !rep.Enforced {
		t.Fatalf("report %+v, %v", rep, err)
	}
	kube.pool = "pc-other"
	if _, err := g.h.ClusterWorkerFirewall(ctx, ks, "pc-c-a"); status(err) != http.StatusConflict || !strings.Contains(err.Error(), "not the project's pool") {
		t.Fatalf("workers outside the project's pool: %v", err)
	}
	if _, err := g.h.UnprotectClusterWorkers(ctx, ks, "pc-c-a"); status(err) != http.StatusConflict {
		t.Fatalf("remove with workers outside the project's pool: %v", err)
	}
	kube.pool, kube.service = "pc-p1", "another-provider"
	if _, err := g.h.ClusterWorkerFirewall(ctx, ks, "pc-c-a"); status(err) != http.StatusNotFound {
		t.Fatalf("a cluster of another provider: %v", err)
	}
	if _, err := g.h.ClusterWorkerFirewall(ctx, ks, "pc-c-none"); status(err) != http.StatusNotFound {
		t.Fatalf("no such cluster: %v", err)
	}
	if w := g.wrote(); len(w) != 0 {
		t.Fatalf("refused or read-only calls wrote %v", w)
	}
}

// Reading a cluster's worker firewall needs admin:service:read; switching it on or off needs the manage permission.
// The routes act on a Managed Kubernetes provider only.
func TestAdminWorkerFirewallPermissions(t *testing.T) {
	db := freshPG(t)
	es := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key"))
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, es, "", "", "")
	h.SetKamaji(func(es *externalservice.ExternalService) (*kamaji.Service, error) {
		return kamaji.NewWithAPI(nil, es.KamajiConfig(), es.ID), nil
	})
	var ran []string
	h.SetWorkerFirewall(func(_ context.Context, _ *kamaji.Service, clusterID, method string) (any, error) {
		ran = append(ran, method+" "+clusterID)
		return map[string]any{"clusterId": clusterID}, nil
	})
	r := chi.NewRouter()
	h.Routes(r)
	for _, doc := range []pgdoc.M{
		{"_id": "k8s", "name": "k8s", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": pgdoc.M{"provider": "kamaji", "region": "r1"}},
		{"_id": "pve", "name": "pve", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": pgdoc.M{"provider": "proxmox"}},
	} {
		mustInsert(t, db, "externalService", doc)
	}
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "support-sub", "role": "SUPPORT"}) // admin:service:read
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})     // admin:service:*
	do := func(sub, method, service string) int {
		req := httptest.NewRequest(method, "/admin/service/"+service+"/k8s-clusters/pc-c-a/worker-firewall", nil)
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	for _, tc := range []struct {
		sub, method, service string
		want                 int
	}{
		{"nobody", http.MethodGet, "k8s", http.StatusForbidden},
		{"nobody", http.MethodPost, "k8s", http.StatusForbidden},
		{"nobody", http.MethodDelete, "k8s", http.StatusForbidden},
		{"support-sub", http.MethodPost, "k8s", http.StatusForbidden},
		{"support-sub", http.MethodDelete, "k8s", http.StatusForbidden},
		{"admin-sub", http.MethodGet, "pve", http.StatusBadRequest},
		{"admin-sub", http.MethodPost, "pve", http.StatusBadRequest},
		{"admin-sub", http.MethodDelete, "none", http.StatusNotFound},
	} {
		if got := do(tc.sub, tc.method, tc.service); got != tc.want || len(ran) != 0 {
			t.Errorf("%s %s on %s: %d, want %d; ran %v", tc.sub, tc.method, tc.service, got, tc.want, ran)
		}
	}
	for _, tc := range []struct{ sub, method string }{
		{"support-sub", http.MethodGet}, {"admin-sub", http.MethodGet}, {"admin-sub", http.MethodPost}, {"admin-sub", http.MethodDelete},
	} {
		ran = nil
		if got := do(tc.sub, tc.method, "k8s"); got != http.StatusOK || len(ran) != 1 || ran[0] != tc.method+" pc-c-a" {
			t.Errorf("%s %s: %d, ran %v", tc.sub, tc.method, got, ran)
		}
	}
}
