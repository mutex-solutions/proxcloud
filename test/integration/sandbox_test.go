//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/cloud/syncjob"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// sbxCluster is an in-memory sandbox cluster: namespaces and Sandbox CRs. The embedded interface is nil, so a call the
// test did not expect panics.
type sbxCluster struct {
	sandbox.K8sAPI
	mu  sync.Mutex
	ns  map[string]map[string]any
	crs map[string]map[string]any // "ns/name"
	// autoReady makes every new sandbox's pod Ready at once (the E2B create waits for it).
	autoReady bool
}

func newSbxCluster() *sbxCluster {
	return &sbxCluster{ns: map[string]map[string]any{}, crs: map[string]map[string]any{}}
}

func (c *sbxCluster) EnsureNamespace(_ context.Context, name string, labels map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := map[string]any{}
	for k, v := range labels {
		l[k] = v
	}
	c.ns[name] = map[string]any{"metadata": map[string]any{"name": name, "labels": l}}
	return nil
}
func (c *sbxCluster) GetNamespace(_ context.Context, name string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ns[name], nil
}
func (c *sbxCluster) DeleteNamespace(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.ns, name)
	for k := range c.crs {
		if strings.HasPrefix(k, name+"/") {
			delete(c.crs, k)
		}
	}
	return nil
}
func (c *sbxCluster) ApplyResourceQuota(context.Context, map[string]any) error { return nil }
func (c *sbxCluster) GetSecretData(context.Context, string, string) (map[string][]byte, error) {
	return nil, nil
}
func (c *sbxCluster) ListPods(context.Context, string, string) ([]map[string]any, error) {
	return nil, nil
}
func (c *sbxCluster) ApplyCR(_ context.Context, _, _, plural string, obj map[string]any) error {
	if plural != sandbox.CRPlural {
		return nil // the egress policy
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := obj["metadata"].(map[string]any)
	m["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
	c.crs[m["namespace"].(string)+"/"+m["name"].(string)] = obj
	if c.autoReady {
		obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": m["creationTimestamp"]}}}
	}
	return nil
}
func (c *sbxCluster) GetCR(_ context.Context, _, _, _, ns, name string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.crs[ns+"/"+name], nil
}
func (c *sbxCluster) DeleteCR(_ context.Context, _, _, _, ns, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.crs, ns+"/"+name)
	return nil
}
func (c *sbxCluster) MergePatchCR(_ context.Context, _, _, _, ns, name string, patch map[string]any) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj := c.crs[ns+"/"+name]
	for _, top := range []string{"spec", "metadata"} {
		p, _ := patch[top].(map[string]any)
		dst := obj[top].(map[string]any)
		for k, v := range p {
			if sub, ok := v.(map[string]any); ok {
				d, _ := dst[k].(map[string]any)
				if d == nil {
					d = map[string]any{}
					dst[k] = d
				}
				for sk, sv := range sub {
					d[sk] = sv
				}
				continue
			}
			dst[k] = v
		}
	}
	return obj, nil
}
func (c *sbxCluster) ListCRs(_ context.Context, _, _, _, ns, _ string) ([]map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for k, v := range c.crs {
		if strings.HasPrefix(k, ns+"/") {
			out = append(out, v)
		}
	}
	return out, nil
}
func (c *sbxCluster) Exec(context.Context, string, string, string, []string, []byte, int) (*kamajik8s.ExecResult, error) {
	return &kamajik8s.ExecResult{Stdout: "ok"}, nil
}
func (c *sbxCluster) PodLogs(context.Context, string, string, string, int) (string, error) {
	return "hello\n", nil
}

// ready makes the sandbox's pod Ready since at.
func (c *sbxCluster) ready(ns, id string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.crs[ns+"/"+id]["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": at.UTC().Format(time.RFC3339)}}}
}

func (c *sbxCluster) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.crs)
}

const (
	sbxA = "tenant-a"
	sbxB = "tenant-b"
)

type sbxFixture struct {
	t       *testing.T
	db      *pgdoc.DB
	h       *project.Handler
	router  chi.Router
	cluster *sbxCluster
	rows    *cloud.Repo
	projs   *project.Repo
	es      *externalservice.Service
}

// newSbxFixture is the real client handler on Postgres with one sandbox provider "sbx" on the in-memory cluster:
// tenant A owns project pa, tenant B project pb, each in its own org on an ACTIVE billing profile. gates answers the
// release check (nil = none wired).
func newSbxFixture(t *testing.T, gates func(context.Context, *externalservice.ExternalService) []sandbox.Gate) *sbxFixture {
	t.Helper()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	repo := project.NewRepo(db)
	orgRepo := org.NewRepo(db)
	billingRepo := billing.NewRepo(db)
	users := user.NewRepo(db)
	svc := project.NewService(repo, orgRepo, billingRepo, users, nil)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), enc)
	rows := cloud.NewRepo(db)
	h := project.NewHandler(svc, project.NewPolicy(org.NewPolicy(orgRepo)), org.NewService(orgRepo, billingRepo, nil), users,
		audit.NewService(audit.NewRepo(db), slog.Default()), billingRepo, rows, esSvc, nil, nil, "az1")
	cluster := newSbxCluster()
	h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
		return sandbox.NewWithAPI(cluster, es.SandboxConfig(), es.ID), nil
	})
	if gates != nil {
		h.SetSandboxGates(gates)
	}
	router := chi.NewRouter()
	h.Routes(router)
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "sbx", "name": "sbx", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
		"config": pgdoc.M{"provider": "sandbox", "region": "az1", "services": pgdoc.M{"sandbox": pgdoc.M{"az1": true}},
			"sandbox": pgdoc.M{"templates": pgdoc.M{"python": pgdoc.M{"image": "python:3.12-slim", "cpu": 1, "memoryGiB": 2}}}}})
	for _, tn := range []struct{ sub, org, bp, proj string }{{sbxA, "oa", "bpa", "pa"}, {sbxB, "ob", "bpb", "pb"}} {
		mustInsert(t, db, "users", pgdoc.M{"sub": tn.sub, "email": tn.sub + "@example.org"})
		mustInsert(t, db, "organization", pgdoc.M{"_id": tn.org, "name": tn.org, "billingProfileId": tn.bp})
		mustInsert(t, db, "organization_members", pgdoc.M{"organizationId": tn.org, "sub": tn.sub, "roles": []string{"OWNER"}})
		mustInsert(t, db, "billingProfile", pgdoc.M{"_id": tn.bp, "organizationId": tn.org, "status": billing.StatusActive})
		mustInsert(t, db, "project", pgdoc.M{"_id": tn.proj, "name": tn.proj, "organizationId": tn.org, "status": project.StatusEnabled,
			"memberships": []pgdoc.M{{"sub": tn.sub, "role": project.RoleOwner}},
			"services":    []any{pgdoc.M{"serviceId": "sbx", "provider": "sandbox", "region": "az1"}}})
	}
	return &sbxFixture{t: t, db: db, h: h, router: router, cluster: cluster, rows: rows, projs: repo, es: esSvc}
}

func gatesHold(context.Context, *externalservice.ExternalService) []sandbox.Gate {
	return []sandbox.Gate{{Name: sandbox.GateNodeRestriction, OK: true}, {Name: sandbox.GateIsolation, OK: true}, {Name: sandbox.GateFence, OK: true}}
}

func (f *sbxFixture) do(sub, method, path, body string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// create starts a python sandbox in project pa as tenant A and returns its row.
func (f *sbxFixture) create() *cloud.CloudResource {
	f.t.Helper()
	code, body := f.do(sbxA, http.MethodPost, "/project/pa/cloud", `{"type":"SANDBOX","data":{"template":"python","timeoutSeconds":600}}`)
	if code != http.StatusOK {
		f.t.Fatalf("create: %d %s", code, body)
	}
	var env struct {
		Data cloud.CloudResource `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Data.ID == "" {
		f.t.Fatalf("create body %s: %v", body, err)
	}
	return f.row(env.Data.ID)
}

func (f *sbxFixture) row(id string) *cloud.CloudResource {
	f.t.Helper()
	r, err := f.rows.FindByID(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *sbxFixture) setProvider(status string) {
	f.t.Helper()
	if _, err := f.db.C("externalService").SetByID(context.Background(), "sbx", pgdoc.M{"status": status}, nil); err != nil {
		f.t.Fatal(err)
	}
}

// archivedBill is what the charge rates for project pa's archived rows (the minute rules' final interval).
func (f *sbxFixture) archivedBill() []*billingapi.BillingResource {
	f.t.Helper()
	brs, err := billingresource.GetArchivedBillingResources(context.Background(), f.rows,
		map[string]billingresource.Provider{cloud.TypeSandbox: billingresource.NewSandboxProvider()}, "pa", "sbx",
		time.Now().Add(-time.Hour), billingapi.BillingContext{})
	if err != nil {
		f.t.Fatal(err)
	}
	return brs
}

// Fail closed: no gate checker, a failing gate, or a DISABLED provider refuses what adds usage and puts nothing on the
// cluster; what lowers usage stays open.
func TestSandboxUsageGatesFailClosed(t *testing.T) {
	for _, c := range []struct {
		name  string
		gates func(context.Context, *externalservice.ExternalService) []sandbox.Gate
	}{
		{"no checker", nil},
		{"a gate fails", func(context.Context, *externalservice.ExternalService) []sandbox.Gate {
			return []sandbox.Gate{{Name: sandbox.GateNodeRestriction, OK: false, Detail: "admitted a node label"}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newSbxFixture(t, c.gates)
			code, body := f.do(sbxA, http.MethodPost, "/project/pa/cloud", `{"type":"SANDBOX","data":{"template":"python"}}`)
			if code != http.StatusServiceUnavailable || f.cluster.count() != 0 {
				t.Fatalf("create: %d %s, %d on the cluster", code, body, f.cluster.count())
			}
		})
	}

	t.Run("provider disabled", func(t *testing.T) {
		f := newSbxFixture(t, gatesHold)
		sb := f.create()
		f.cluster.ready(sandbox.NamespaceFor("pa"), sb.ExternalID, time.Now())
		f.setProvider(externalservice.StatusDisabled)
		for _, c := range []struct{ name, method, path, body string }{
			{"create", http.MethodPost, "/project/pa/cloud", `{"type":"SANDBOX","data":{"template":"python"}}`},
			{"create naming the service", http.MethodPost, "/project/pa/cloud?serviceId=sbx", `{"type":"SANDBOX","data":{"template":"python"}}`},
			{"exec", http.MethodPost, "/project/pa/cloud/" + sb.ID + "/action", `{"action":"EXEC","data":{"cmd":"true"}}`},
			{"timeout", http.MethodPost, "/project/pa/cloud/" + sb.ID + "/action", `{"action":"SET_TIMEOUT","data":{"timeoutSeconds":3600}}`},
		} {
			if code, body := f.do(sbxA, c.method, c.path, c.body); code != http.StatusServiceUnavailable {
				t.Errorf("%s on a DISABLED provider: %d %s", c.name, code, body)
			}
		}
		if f.cluster.count() != 1 {
			t.Fatalf("a create got through: %d sandboxes", f.cluster.count())
		}
		if code, body := f.do(sbxA, http.MethodPost, "/project/pa/cloud/"+sb.ID+"/action", `{"action":"GET_LOGS"}`); code != http.StatusOK {
			t.Errorf("logs: %d %s", code, body)
		}
		if code, body := f.do(sbxA, http.MethodDelete, "/project/pa/cloud/"+sb.ID, ""); code != http.StatusAccepted || f.cluster.count() != 0 {
			t.Errorf("delete: %d %s", code, body)
		}
	})

	// The usual project holds a Proxmox provider too, which the default resolution picks: a create naming no service
	// must still reach the DISABLED sandbox provider and answer 503, not Proxmox's "SANDBOX is not supported" 400.
	t.Run("provider disabled, project on Proxmox too", func(t *testing.T) {
		f := newSbxFixture(t, gatesHold)
		mustInsert(t, f.db, "externalService", pgdoc.M{"_id": "pve", "name": "pve", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
			"config": pgdoc.M{"provider": "proxmox", "region": "az1", "services": pgdoc.M{"compute": pgdoc.M{"az1": true}}}})
		if _, err := f.db.C("project").SetByID(context.Background(), "pa", pgdoc.M{"services": []any{
			pgdoc.M{"serviceId": "pve", "provider": "proxmox", "region": "az1"},
			pgdoc.M{"serviceId": "sbx", "provider": "sandbox", "region": "az1"}}}, nil); err != nil {
			t.Fatal(err)
		}
		f.create() // a live sandbox provider is found without naming it
		f.setProvider(externalservice.StatusDisabled)
		if code, body := f.do(sbxA, http.MethodPost, "/project/pa/cloud", `{"type":"SANDBOX","data":{"template":"python"}}`); code != http.StatusServiceUnavailable {
			t.Errorf("create naming no service: %d %s", code, body)
		}
	})

	t.Run("project disabled, profile suspended", func(t *testing.T) {
		f := newSbxFixture(t, gatesHold)
		sb := f.create()
		if _, err := f.db.C("billingProfile").SetByID(context.Background(), "bpa", pgdoc.M{"status": billing.StatusSuspended}, nil); err != nil {
			t.Fatal(err)
		}
		if code, body := f.do(sbxA, http.MethodPost, "/project/pa/cloud", `{"type":"SANDBOX","data":{"template":"python"}}`); code != http.StatusForbidden {
			t.Errorf("create on a suspended profile: %d %s", code, body)
		}
		if _, err := f.db.C("project").SetByID(context.Background(), "pa", pgdoc.M{"status": project.StatusDisabled}, nil); err != nil {
			t.Fatal(err)
		}
		for _, a := range []string{`{"action":"EXEC","data":{"cmd":"true"}}`, `{"action":"SET_TIMEOUT","data":{"timeoutSeconds":60}}`} {
			if code, body := f.do(sbxA, http.MethodPost, "/project/pa/cloud/"+sb.ID+"/action", a); code != http.StatusConflict {
				t.Errorf("%s on a DISABLED project: %d %s", a, code, body)
			}
		}
	})
}

// Every sandbox that ran is billed, also one that nothing read while it ran: an exec records the start, and a
// sandbox gone before any read is ended and billed from its create.
func TestSandboxRunsAreBilled(t *testing.T) {
	f := newSbxFixture(t, gatesHold)
	ns := sandbox.NamespaceFor("pa")
	started := time.Now().Add(-2 * time.Minute).UTC().Truncate(time.Second)

	execd := f.create()
	if rs := execd.Data["runningSince"]; rs != nil {
		t.Fatalf("a new sandbox has not run yet: %v", rs)
	}
	f.cluster.ready(ns, execd.ExternalID, started)
	if code, body := f.do(sbxA, http.MethodPost, "/project/pa/cloud/"+execd.ID+"/action", `{"action":"EXEC","data":{"cmd":"echo hi"}}`); code != http.StatusOK {
		t.Fatalf("exec: %d %s", code, body)
	}
	if got := f.row(execd.ID).Data["runningSince"]; got != started.Format(time.RFC3339) {
		t.Fatalf("exec must record that the sandbox ran: runningSince %v", got)
	}

	// The second one runs and ends before anything reads it: no list, no exec, no delete.
	unread := f.create()
	f.cluster.ready(ns, unread.ExternalID, time.Now())
	if err := f.cluster.DeleteCR(context.Background(), "", "", "", ns, unread.ExternalID); err != nil {
		t.Fatal(err)
	}
	job := syncjob.New(f.projs, f.es, f.rows, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithSandboxFactory(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
			return sandbox.NewWithAPI(f.cluster, es.SandboxConfig(), es.ID), nil
		})
	if _, err := job.RunSandboxState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.row(unread.ID); r != nil {
		t.Fatalf("a vanished sandbox's row is archived: %v", r.Data)
	}
	brs := f.archivedBill()
	if len(brs) != 1 || brs[0].ResourceType != "sandbox" || brs[0].DeletedAt == nil || brs[0].ResourceID != unread.ID {
		t.Fatalf("the unread sandbox must bill: %+v", brs)
	}
	if got := f.row(execd.ID).Data["status"]; got != sandbox.StatusRunning {
		t.Fatalf("the running one stays RUNNING: %v", got)
	}

	// Quota (D10): one sandbox at a time for this project.
	if _, err := f.db.C("project").SetByID(context.Background(), "pa", pgdoc.M{"quota": pgdoc.M{"sandboxes": 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if code, body := f.do(sbxA, http.MethodPost, "/project/pa/cloud", `{"type":"SANDBOX","data":{"template":"python"}}`); code != http.StatusTooManyRequests {
		t.Fatalf("over quota: %d %s", code, body)
	}

	// A delete ends the row at once; billing stops there.
	if code, body := f.do(sbxA, http.MethodDelete, "/project/pa/cloud/"+execd.ID, ""); code != http.StatusAccepted {
		t.Fatalf("delete: %d %s", code, body)
	}
	if r := f.row(execd.ID); r == nil || r.Data["status"] != sandbox.StatusDeleted || r.Data["endedAt"] == nil {
		t.Fatalf("deleted row: %+v", r)
	}
}

// One tenant can neither see, exec into nor delete another tenant's sandbox by id.
func TestSandboxCrossTenant(t *testing.T) {
	f := newSbxFixture(t, gatesHold)
	sb := f.create()
	f.cluster.ready(sandbox.NamespaceFor("pa"), sb.ExternalID, time.Now())
	for _, c := range []struct{ name, method, path, body string }{
		{"exec through own project", http.MethodPost, "/project/pb/cloud/" + sb.ID + "/action", `{"action":"EXEC","data":{"cmd":"id"}}`},
		{"logs through own project", http.MethodPost, "/project/pb/cloud/" + sb.ID + "/action", `{"action":"GET_LOGS"}`},
		{"access through own project", http.MethodPost, "/project/pb/cloud/" + sb.ID + "/action", `{"action":"GET_ACCESS"}`},
		{"access on the other project", http.MethodPost, "/project/pa/cloud/" + sb.ID + "/action", `{"action":"GET_ACCESS"}`},
		{"delete through own project", http.MethodDelete, "/project/pb/cloud/" + sb.ID, ""},
		{"exec on the other project", http.MethodPost, "/project/pa/cloud/" + sb.ID + "/action", `{"action":"EXEC","data":{"cmd":"id"}}`},
		{"delete on the other project", http.MethodDelete, "/project/pa/cloud/" + sb.ID, ""},
		{"list the other project", http.MethodPost, "/project/pa/resource?type=SANDBOX", ""},
	} {
		if code, body := f.do(sbxB, c.method, c.path, c.body); code < 400 || strings.Contains(body, sb.ExternalID) {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	if code, body := f.do(sbxB, http.MethodPost, "/project/pb/resource?type=SANDBOX", ""); code != http.StatusOK || strings.Contains(body, sb.ExternalID) {
		t.Errorf("own list shows the other tenant's sandbox: %d %s", code, body)
	}
	if f.cluster.count() != 1 || f.row(sb.ID) == nil || f.row(sb.ID).Data["status"] == sandbox.StatusDeleted {
		t.Fatal("the sandbox was touched")
	}
}

// GET_ACCESS hands the tenant its sandbox's host and traffic token (read from the CR, kept off the row), and the wake
// endpoint answers only a sandbox provider's proxyWakeToken.
func TestSandboxAccessAndWake(t *testing.T) {
	f := newSbxFixture(t, gatesHold)
	if code, body := f.do(sbxA, http.MethodPost, "/project/pa/cloud/"+f.create().ID+"/action", `{"action":"GET_ACCESS"}`); code != http.StatusConflict {
		t.Fatalf("a provider without a domain publishes no ports: %d %s", code, body)
	}
	if _, err := f.db.C("externalService").SetByID(context.Background(), "sbx", pgdoc.M{
		"config": pgdoc.M{"provider": "sandbox", "region": "az1", "services": pgdoc.M{"sandbox": pgdoc.M{"az1": true}},
			"sandbox": pgdoc.M{"domain": "sbx.example.org",
				"templates": pgdoc.M{"python": pgdoc.M{"image": "python:3.12-slim", "cpu": 1, "memoryGiB": 2}}}},
		"secret": pgdoc.M{"proxyWakeToken": "wake-secret"}}, nil); err != nil {
		t.Fatal(err)
	}
	code, body := f.do(sbxA, http.MethodPost, "/project/pa/cloud", `{"type":"SANDBOX","data":{"template":"python","allowPublicTraffic":false}}`)
	if code != http.StatusOK {
		t.Fatalf("create: %d %s", code, body)
	}
	var created struct {
		Data cloud.CloudResource `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &created)
	sb := f.row(created.Data.ID)
	a, err := sandbox.NewWithAPI(f.cluster, sandbox.Config{}, "sbx").AccessFor(context.Background(), "pa", sb.ExternalID)
	if err != nil || a.TrafficAccessToken == "" {
		t.Fatalf("no traffic token on the CR: %v", err)
	}
	if strings.Contains(body, a.TrafficAccessToken) {
		t.Fatalf("the create answer carries the token: %s", body)
	}
	if raw, _ := json.Marshal(sb.Data); strings.Contains(string(raw), a.TrafficAccessToken) {
		t.Fatal("the token is cached on the row")
	}
	code, body = f.do(sbxA, http.MethodPost, "/project/pa/cloud/"+sb.ID+"/action", `{"action":"GET_ACCESS"}`)
	var acc struct {
		Data struct {
			Result map[string]string `json:"result"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &acc)
	if code != http.StatusOK || acc.Data.Result["host"] != "{port}-"+sb.ExternalID+".sbx.example.org" ||
		acc.Data.Result["trafficAccessToken"] != a.TrafficAccessToken {
		t.Fatalf("access: %d %s", code, body)
	}

	wake := func(auth, id string) int {
		req := httptest.NewRequest(http.MethodPost, "/internal/sandboxes/"+id+"/wake", strings.NewReader(`{"port":8000}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, req)
		return w.Code
	}
	for _, c := range []struct {
		auth, id string
		want     int
	}{
		{"", sb.ExternalID, http.StatusUnauthorized},
		{"Bearer wrong", sb.ExternalID, http.StatusUnauthorized},
		{"wake-secret", sb.ExternalID, http.StatusUnauthorized}, // not a bearer
		{"Bearer wake-secret", "not-an-id", http.StatusNotFound},
		{"Bearer wake-secret", sb.ExternalID, http.StatusNotFound}, // running, not paused
	} {
		if got := wake(c.auth, c.id); got != c.want {
			t.Errorf("wake %q %s: %d, want %d", c.auth, c.id, got, c.want)
		}
	}
}

// A suspension's delete ends every row, also one whose sandbox is already gone; a project deletion takes the rows and
// the namespace.
func TestSandboxSuspendAndTeardown(t *testing.T) {
	f := newSbxFixture(t, gatesHold)
	ns := sandbox.NamespaceFor("pa")
	running := f.create()
	f.cluster.ready(ns, running.ExternalID, time.Now().Add(-time.Minute))
	gone := f.create()
	_ = f.cluster.DeleteCR(context.Background(), "", "", "", ns, gone.ExternalID)

	p, err := f.projs.FindByID(context.Background(), "pa")
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.h.DeleteProjectSandboxes(context.Background(), p)
	if err != nil || n != 1 || f.cluster.count() != 0 {
		t.Fatalf("deleted %d, err %v, %d left", n, err, f.cluster.count())
	}
	if r := f.row(running.ID); r == nil || r.Data["status"] != sandbox.StatusDeleted || r.Data["runningSince"] == nil {
		t.Fatalf("the running sandbox's row must end with its start: %+v", r)
	}
	if r := f.row(gone.ID); r == nil || r.Data["status"] != sandbox.StatusDeleted || r.Data["runningSince"] == nil {
		t.Fatalf("a row whose sandbox was already gone must end too: %+v", r)
	}

	again := f.create()
	if _, ok := f.cluster.ns[ns]; !ok {
		t.Fatal("no namespace")
	}
	if err := f.h.TeardownProject(context.Background(), "pa"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if _, ok := f.cluster.ns[ns]; ok || f.cluster.count() != 0 {
		t.Fatalf("namespace or sandboxes left: %v %d", f.cluster.ns, f.cluster.count())
	}
	for _, id := range []string{running.ID, gone.ID, again.ID} {
		if r := f.row(id); r != nil {
			t.Errorf("row %s left after teardown: %v", id, r.Data)
		}
	}
}
