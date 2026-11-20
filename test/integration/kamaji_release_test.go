//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// cascadeKube is a management cluster holding what a delete cascade leaves: the cluster's marker Secret and its
// Application, which a delete marks as deleting (ArgoCD's finalizer) and the test removes when the cascade is over.
type cascadeKube struct {
	kamaji.K8sAPI
	mu       sync.Mutex
	down     bool
	apps     map[string]bool // cluster id → deleting
	markers  map[string]bool
	machines map[string]bool
}

func (k *cascadeKube) GetApplication(_ context.Context, _, name string) (map[string]any, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	deleting, ok := k.apps[name]
	if !ok {
		return nil, nil
	}
	meta := map[string]any{"name": name, "labels": map[string]any{kamaji.LabelManagedBy: kamaji.ManagedByValue}}
	if deleting {
		meta["deletionTimestamp"] = time.Now().UTC().Format(time.RFC3339)
	}
	return map[string]any{"metadata": meta}, nil
}
func (k *cascadeKube) DeleteApplication(_ context.Context, _, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.apps[name] = true
	return nil
}
func (k *cascadeKube) ListSecrets(context.Context, string, string) ([]map[string]any, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.down {
		return nil, errors.New("dial tcp 10.0.0.1:6443: connect: connection refused")
	}
	var out []map[string]any
	for id := range k.markers {
		// Made a moment ago: the sweep would not look at this cluster for another 30 minutes.
		out = append(out, map[string]any{"metadata": map[string]any{
			"name": kamaji.MarkerSecretName(id), "creationTimestamp": time.Now().UTC().Format(time.RFC3339),
			"annotations": map[string]any{"proxcloud.io/compute-service": "pve"},
		}})
	}
	return out, nil
}
func (k *cascadeKube) DeleteSecret(_ context.Context, _, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for id := range k.markers {
		if name == kamaji.MarkerSecretName(id) {
			delete(k.markers, id)
		}
	}
	return nil
}
func (k *cascadeKube) GetTenantControlPlane(context.Context, string, string) (map[string]any, error) {
	return nil, nil
}
func (k *cascadeKube) ListTenantControlPlanes(context.Context, string, string) ([]map[string]any, error) {
	return nil, nil
}
func (k *cascadeKube) ListMachineDeployments(context.Context, string, string) ([]map[string]any, error) {
	return nil, nil
}
func (k *cascadeKube) ListCRs(_ context.Context, _, _, _, _, selector string) ([]map[string]any, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for id := range k.machines {
		if strings.HasSuffix(selector, "="+id) {
			return []map[string]any{{"metadata": map[string]any{"name": id + "-workers-pve1-x7k2p"}}}, nil
		}
	}
	return nil, nil
}
func (k *cascadeKube) ListApplications(context.Context, string, string) ([]map[string]any, error) {
	return nil, nil
}
func (k *cascadeKube) GetNamespace(context.Context, string) (map[string]any, error) { return nil, nil }

// tokenACL is the ACL of a fake Proxmox, kept as PVE keeps it: the rows of a deleted token stay in user.cfg (dead:
// PVE lists them nowhere and warns about each at every CLI call) until the file is written again. The lab's release
// of 2026-11-21 left six such lines of the last tenant cluster token deleted.
type tokenACL struct {
	rows []pve.ACLEntry
	gone []string // the tokens deleted
}

// clusterACL is what a mint leaves for the tenant cluster tokens ids, next to rows that are not theirs.
func clusterACL(ids ...string) *tokenACL {
	a := &tokenACL{rows: []pve.ACLEntry{
		{Path: "/pool", RoleID: "ProxCloudPlatform", Type: "token", UGID: pve.DefaultPlatformTokenID, Propagate: 1},
		{Path: "/storage/local", RoleID: "PVEDatastoreAdmin", Type: "user", UGID: "capmox@pve"},
	}}
	for _, id := range ids {
		for _, path := range []string{"/pool/pc-p1", "/storage/local-zfs", "/pool/pc-templates", "/nodes", "/sdn/zones/localnetwork", "/storage/local"} {
			a.rows = append(a.rows, pve.ACLEntry{Path: path, RoleID: "ProxCloudCAPI", Type: "token", UGID: "capmox@pve!" + id, Propagate: 1})
		}
	}
	return a
}

func (a *tokenACL) of(token string) (n int) {
	for _, r := range a.rows {
		if r.UGID == token {
			n++
		}
	}
	return n
}

// serve answers the ACL list, a row's removal and a token's delete; false for any other request.
func (a *tokenACL) serve(w http.ResponseWriter, r *http.Request, path string) bool {
	dead := func(row pve.ACLEntry) bool { return row.Type == "token" && slices.Contains(a.gone, row.UGID) }
	switch {
	case r.Method == http.MethodGet && path == "/access/acl":
		rows := []pve.ACLEntry{}
		for _, row := range a.rows {
			if !dead(row) {
				rows = append(rows, row)
			}
		}
		b, _ := json.Marshal(map[string]any{"data": rows})
		_, _ = w.Write(b)
	case r.Method == http.MethodPut && path == "/access/acl":
		_ = r.ParseForm()
		a.rows = slices.DeleteFunc(a.rows, dead) // a write of user.cfg drops the dead rows
		if r.Form.Get("delete") == "1" {
			a.rows = slices.DeleteFunc(a.rows, func(row pve.ACLEntry) bool {
				return row.Path == r.Form.Get("path") && row.RoleID == r.Form.Get("roles") && row.UGID == r.Form.Get("tokens")
			})
		}
		fmt.Fprint(w, `{"data":null}`)
	case r.Method == http.MethodDelete && strings.Contains(path, "/token/"):
		seg := strings.Split(path, "/") // /access/users/<user>/token/<name>
		a.rows = slices.DeleteFunc(a.rows, dead)
		a.gone = append(a.gone, seg[3]+"!"+seg[5])
		fmt.Fprint(w, `{"data":null}`)
	default:
		return false
	}
	return true
}

// Lab finding F57 (2026-11-17, a host without the job scheduler): a deleted tenant cluster's token capmox@pve!<id> and
// its ACL rows stayed in Proxmox until an operator started a provider sync, and for a cluster deleted in its first 30
// minutes until a sync after that age. The delete now queues a durable job that needs no scheduler: it waits for the
// cascade, keeps the token while anything may still need it, and then revokes it.
func TestKubernetesDeleteReleasesTheTokenWithoutASync(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var revoked []string
	workerLeft := false
	acl := clusterACL("pc-c-a", "pc-c-ab") // pc-c-ab: another cluster's token, whose name starts with this one's
	rowsBefore := len(acl.rows)
	proxmox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		if r.Method == http.MethodDelete && strings.Contains(path, "/token/") {
			revoked = append(revoked, path)
		}
		switch {
		case acl.serve(w, r, path):
		case r.Method == http.MethodGet && path == "/cluster/resources" && r.URL.Query().Get("type") == "node":
			fmt.Fprint(w, `{"data":[{"type":"node","node":"pve1","status":"online"}]}`)
		case r.Method == http.MethodGet && path == "/cluster/resources" && workerLeft:
			fmt.Fprint(w, `{"data":[{"type":"qemu","vmid":124,"node":"pve1","name":"pc-c-a-workers-pve1-x7k2p","pool":"pc-p1","status":"running"}]}`)
		case r.Method == http.MethodGet && path == "/cluster/resources":
			fmt.Fprint(w, `{"data":[{"type":"qemu","vmid":125,"node":"pve1","name":"web","pool":"pc-p1","status":"running"}]}`)
		case r.Method == http.MethodGet && path == "/cluster/firewall/groups": // the release looks for the workers' security group
			fmt.Fprint(w, `{"data":[]}`)
		case r.Method == http.MethodGet && path == "/access/users/csi@pve/token": // no token of this cluster: no delete for one
			fmt.Fprint(w, `{"data":[{"tokenid":"gateway"}]}`)
		default:
			t.Errorf("the fake Proxmox has no answer for %s %s", r.Method, path)
			http.Error(w, "no fake", http.StatusNotImplemented)
		}
	}))
	t.Cleanup(proxmox.Close)
	revokes := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(revoked)
	}

	kube := &cascadeKube{apps: map[string]bool{"pc-c-a": false}, markers: map[string]bool{"pc-c-a": true}, machines: map[string]bool{}}
	enc := textcrypt.New("test-key")
	orgRepo := org.NewRepo(db)
	billingRepo := billing.NewRepo(db)
	users := user.NewRepo(db)
	cloudRepo := cloud.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), enc)
	h := project.NewHandler(project.NewService(project.NewRepo(db), orgRepo, billingRepo, users, nil), project.NewPolicy(org.NewPolicy(orgRepo)),
		org.NewService(orgRepo, billingRepo, nil), users, audit.NewService(audit.NewRepo(db), slog.Default()), billingRepo, cloudRepo, esSvc, nil, nil, "r1")
	pveFor := func(*externalservice.ExternalService) (*pve.Service, error) {
		return pve.NewService(pve.Config{APIURL: proxmox.URL, TokenID: pve.DefaultPlatformTokenID, Secret: "s"}, "pve", "pc-")
	}
	h.SetKamaji(func(es *externalservice.ExternalService) (*kamaji.Service, error) {
		ks := kamaji.NewWithAPI(kube, es.KamajiConfig(), es.ID)
		ks.OnClusterGone(project.KamajiReleaseCluster(esSvc, cloudRepo, pveFor, project.NewPublicIPRepo(db), false, nil))
		return ks, nil
	})
	h.SetKamajiReleaseJobs(jobs)
	router := chi.NewRouter()
	h.Routes(router)
	runner := &project.KamajiReleaseRunner{Jobs: jobs, Finalize: h.KamajiFinalizeCluster, Log: slog.Default()}

	const sub = "owner-sub"
	for coll, docs := range map[string][]pgdoc.M{
		"users":                {{"sub": sub, "email": "o@example.org"}},
		"organization":         {{"_id": "o1", "name": "o", "billingProfileId": "bp1"}},
		"organization_members": {{"organizationId": "o1", "sub": sub, "roles": []string{"OWNER"}}},
		"externalService": {
			{"_id": "pve", "name": "pve", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
				"config": pgdoc.M{"provider": "proxmox", "regions": pgdoc.M{"r1": pgdoc.M{}}}},
			{"_id": "k8s", "name": "k8s", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": pgdoc.M{
				"provider": "kamaji", "region": "r1", "cluster": pgdoc.M{"versions": pgdoc.M{"1.35.10": "pve1=9200"}}}},
		},
		// A project on the Kubernetes provider alone, for the teardown below.
		"project": {{"_id": "p1", "name": "p", "status": project.StatusEnabled, "organizationId": "o1",
			"memberships": []pgdoc.M{{"sub": sub, "role": project.RoleOwner}},
			"services": []any{
				pgdoc.M{"serviceId": "pve", "provider": "proxmox", "region": "r1", "pool": "pc-p1"},
				pgdoc.M{"serviceId": "k8s", "provider": "kamaji", "region": "r1"},
			}}, {"_id": "p2", "name": "q", "status": project.StatusEnabled, "organizationId": "o1",
			"services": []any{pgdoc.M{"serviceId": "k8s", "provider": "kamaji", "region": "r1"}}}},
		"cloudResource": {{"_id": "row-d", "serviceId": "k8s", "projectId": "p2", "region": "r1", "type": cloud.TypeKubernetesCluster,
			"externalId": "pc-c-d", "data": pgdoc.M{"name": "demo"}}, {"_id": "row-a", "serviceId": "k8s", "projectId": "p1", "region": "r1", "type": cloud.TypeKubernetesCluster,
			"externalId": "pc-c-a", "data": pgdoc.M{"name": "demo"}}},
	} {
		for _, d := range docs {
			mustInsert(t, db, coll, d)
		}
	}

	req := httptest.NewRequest(http.MethodDelete, "/project/p1/cloud/row-a", nil)
	req.Header.Set("x-service-id", "k8s")
	req.Header.Set("x-region-id", "r1")
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("delete answered %d %s", w.Code, w.Body.String())
	}

	// job reads the one job of a cluster.
	job := func(cluster string) (state, code string, attempts int) {
		t.Helper()
		if err := db.Pool.QueryRow(ctx, `SELECT state,last_error,attempts FROM durable_job WHERE kind=$1 AND key=$2`,
			durablejobs.KamajiRelease, "k8s/"+cluster).Scan(&state, &code, &attempts); err != nil {
			t.Fatalf("the release job of %s: %v", cluster, err)
		}
		return
	}
	// look runs the runner once on a job that is due and returns what the job reads after it.
	look := func(cluster string) (string, string) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET next_at=clock_timestamp() WHERE state='pending'`); err != nil {
			t.Fatal(err)
		}
		if did, err := runner.RunOnce(ctx); !did || err != nil {
			t.Fatalf("look: did %v, err %v", did, err)
		}
		state, code, attempts := job(cluster)
		if state == "pending" && attempts != 0 {
			t.Fatalf("a wait spent an attempt: %d", attempts)
		}
		return state, code
	}
	if state, _, _ := job("pc-c-a"); state != "pending" {
		t.Fatalf("the delete queued a job in state %s", state)
	}

	// ArgoCD's cascade is running: CAPMOX needs the token.
	if state, code := look("pc-c-a"); state != "pending" || code != "cascade_running" || revokes() != 0 {
		t.Fatalf("cascade running: %s %s, %d tokens revoked", state, code, revokes())
	}
	// A job that waits is not due at once.
	if did, err := runner.RunOnce(ctx); did || err != nil {
		t.Fatalf("a waiting job was claimed again at once: %v %v", did, err)
	}
	// The Application is gone, a Machine is not.
	kube.mu.Lock()
	delete(kube.apps, "pc-c-a")
	kube.machines["pc-c-a"] = true
	kube.mu.Unlock()
	if state, code := look("pc-c-a"); state != "pending" || code != "cascade_running" || revokes() != 0 {
		t.Fatalf("a machine left: %s %s, %d tokens revoked", state, code, revokes())
	}
	// Nothing on the management cluster, and a worker VM still in the project's pool.
	kube.mu.Lock()
	delete(kube.machines, "pc-c-a")
	kube.mu.Unlock()
	mu.Lock()
	workerLeft = true
	mu.Unlock()
	if state, code := look("pc-c-a"); state != "pending" || code != "release_failed" || revokes() != 0 {
		t.Fatalf("a worker VM left: %s %s, %d tokens revoked", state, code, revokes())
	}
	mu.Lock()
	workerLeft = false
	mu.Unlock()
	// The management cluster does not answer: nothing says the workers are gone.
	kube.mu.Lock()
	kube.down = true
	kube.mu.Unlock()
	if state, code := look("pc-c-a"); state != "pending" || code != "release_failed" || revokes() != 0 {
		t.Fatalf("management cluster down: %s %s, %d tokens revoked", state, code, revokes())
	}
	kube.mu.Lock()
	kube.down = false
	kube.mu.Unlock()

	// The cascade is over: the token goes, with no provider sync and inside the marker's 30 minutes.
	if state, code := look("pc-c-a"); state != "succeeded" || code != "" {
		t.Fatalf("cascade finished: %s %s", state, code)
	}
	mu.Lock()
	if len(revoked) != 1 || !strings.HasSuffix(revoked[0], "/access/users/capmox@pve/token/pc-c-a") {
		t.Fatalf("revoked = %v, want the cluster's token alone: csi@pve lists no token of this cluster, so no delete is sent for one", revoked)
	}
	// The token's six rows went before the token did: none is left as a dead line of user.cfg, and no other row went.
	if acl.of("capmox@pve!pc-c-a") != 0 || acl.of("capmox@pve!pc-c-ab") != 6 || len(acl.rows) != rowsBefore-6 {
		t.Fatalf("after the release: %d rows of the deleted token, %d of the other cluster's, %d of %d in all",
			acl.of("capmox@pve!pc-c-a"), acl.of("capmox@pve!pc-c-ab"), len(acl.rows), rowsBefore)
	}
	mu.Unlock()
	if len(kube.markers) != 0 {
		t.Fatal("the marker is left")
	}

	// The sweep was first (no marker left): the job ends without asking Proxmox for anything.
	if _, err := jobs.Enqueue(ctx, durablejobs.KamajiRelease, "k8s/pc-c-b", []byte(`{"serviceId":"k8s","projectId":"p1","clusterId":"pc-c-b"}`), 20); err != nil {
		t.Fatal(err)
	}
	if state, _ := look("pc-c-b"); state != "succeeded" || revokes() != 1 {
		t.Fatalf("already released: %s, %d tokens revoked", state, revokes())
	}

	// The provider was removed: no management cluster is left to ask, and the job ends.
	if _, err := jobs.Enqueue(ctx, durablejobs.KamajiRelease, "k8s/pc-c-e", []byte(`{"serviceId":"removed","projectId":"p1","clusterId":"pc-c-e"}`), 20); err != nil {
		t.Fatal(err)
	}
	if state, _ := look("pc-c-e"); state != "succeeded" || revokes() != 1 {
		t.Fatalf("provider removed: %s, %d tokens revoked", state, revokes())
	}

	// A project teardown deletes its clusters the same way, and queues the same job.
	kube.mu.Lock()
	kube.apps["pc-c-d"], kube.markers["pc-c-d"] = false, true
	kube.mu.Unlock()
	_ = h.TeardownProject(ctx, "p2") // its outcome (the cascade is pending) is not this test's subject
	if state, _, _ := job("pc-c-d"); state != "pending" {
		t.Fatalf("the teardown queued a job in state %s", state)
	}
	kube.mu.Lock()
	delete(kube.apps, "pc-c-d")
	kube.mu.Unlock()
	if state, _ := look("pc-c-d"); state != "succeeded" {
		t.Fatalf("after the teardown's cascade: %s", state)
	}

	// A cascade that never ends: after its lifetime the job is dead, the token stays, and the sweep is what is left.
	before := revokes()
	kube.mu.Lock()
	kube.apps["pc-c-c"], kube.markers["pc-c-c"] = true, true
	kube.mu.Unlock()
	if _, err := jobs.Enqueue(ctx, durablejobs.KamajiRelease, "k8s/pc-c-c", []byte(`{"serviceId":"k8s","projectId":"p1","clusterId":"pc-c-c"}`), 20); err != nil {
		t.Fatal(err)
	}
	if state, code := look("pc-c-c"); state != "pending" || code != "cascade_running" {
		t.Fatalf("a young job: %s %s", state, code)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET created_at=clock_timestamp()-interval '25 hours' WHERE key='k8s/pc-c-c'`); err != nil {
		t.Fatal(err)
	}
	if state, code := look("pc-c-c"); state != "dead" || code != "cascade_not_finished" || revokes() != before {
		t.Fatalf("past its lifetime: %s %s, %d tokens revoked", state, code, revokes())
	}
}

// A failed create releases what it took at once (kamaji.Service.AbandonCluster), through the same release as a
// finished delete. A node that is down, holding some VM of the project, must not keep the new token and addresses.
func TestFailedCreateReleasesWithANodeDown(t *testing.T) {
	ctx := context.Background()
	f := newShareAccessFixture(t)
	f.reserve(t, "vmbrw", "10.89.0.99", "pc-c-a")
	var revoked []string
	acl := clusterACL("pc-c-a")
	proxmox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		if r.Method == http.MethodDelete && strings.Contains(path, "/token/") {
			revoked = append(revoked, path)
		}
		switch {
		case acl.serve(w, r, path):
		case r.Method == http.MethodGet && path == "/cluster/resources" && r.URL.Query().Get("type") == "node":
			fmt.Fprint(w, `{"data":[{"type":"node","node":"pve1","status":"online"},{"type":"node","node":"pve2","status":"offline"}]}`)
		case r.Method == http.MethodGet && path == "/cluster/resources":
			fmt.Fprintf(w, `{"data":[{"type":"qemu","vmid":130,"node":"pve2","pool":"pc-%s","status":"unknown"}]}`, f.projectID)
		case r.Method == http.MethodGet && path == "/cluster/firewall/groups": // the release looks for the workers' security group
			fmt.Fprint(w, `{"data":[]}`)
		case r.Method == http.MethodGet && path == "/access/users/csi@pve/token": // a cluster from before the CSI gateway
			fmt.Fprint(w, `{"data":[{"tokenid":"gateway"},{"tokenid":"pc-c-a"}]}`)
		default:
			t.Errorf("the fake Proxmox has no answer for %s %s", r.Method, path)
			http.Error(w, "no fake", http.StatusNotImplemented)
		}
	}))
	t.Cleanup(proxmox.Close)
	kube := &cascadeKube{markers: map[string]bool{"pc-c-a": true}}
	ks := kamaji.NewWithAPI(kube, kamaji.Config{}, "svc-k8s")
	ks.OnClusterGone(project.KamajiReleaseCluster(externalservice.NewService(externalservice.NewRepo(f.db), textcrypt.New("k")), cloud.NewRepo(f.db),
		func(*externalservice.ExternalService) (*pve.Service, error) {
			return pve.NewService(pve.Config{APIURL: proxmox.URL, TokenID: pve.DefaultPlatformTokenID, Secret: "s"}, "svc-pve", "pc-")
		}, f.ips, false, nil))
	if err := ks.AbandonCluster(ctx, f.projectID, "pc-c-a", "svc-pve"); err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 2 || !strings.HasSuffix(revoked[0], "/access/users/capmox@pve/token/pc-c-a") {
		t.Fatalf("revoked = %v, want the cluster's tokens: csi@pve lists one for it, as for a cluster from before the CSI gateway", revoked)
	}
	if acl.of("capmox@pve!pc-c-a") != 0 || len(acl.rows) != 2 {
		t.Fatalf("rows left after the release: %+v", acl.rows)
	}
	if n := f.clusterRows(t, "vmbrw"); n != 0 || len(kube.markers) != 0 {
		t.Fatalf("%d address rows and %d markers left", n, len(kube.markers))
	}
}
