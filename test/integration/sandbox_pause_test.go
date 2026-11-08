//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// pauseCluster is the in-memory cluster with what a snapshot needs: exec streaming the changed files, and assembly
// pods in sbx-build that finish at once with a digest.
type pauseCluster struct {
	*sbxCluster
	pmu  sync.Mutex
	pods map[string]map[string]any
}

func (c *pauseCluster) ExecStream(_ context.Context, _, _, _ string, cmd []string, w io.Writer, _ int) (int, string, error) {
	if len(cmd) != 2 || cmd[1] != "-snapshot" {
		return 1, "unexpected command", nil
	}
	_, err := w.Write([]byte(strings.Repeat("layer-bytes ", 1000)))
	return 0, "", err
}
func (c *pauseCluster) ApplyCore(_ context.Context, plural string, obj map[string]any) error {
	if plural != "pods" {
		return nil
	}
	c.pmu.Lock()
	defer c.pmu.Unlock()
	md := obj["metadata"].(map[string]any)
	obj["status"] = map[string]any{"phase": "Succeeded", "containerStatuses": []any{map[string]any{"name": "push",
		"state": map[string]any{"terminated": map[string]any{"exitCode": 0, "message": "192.0.2.226/t/x@sha256:" + strings.Repeat("a", 64),
			"finishedAt": time.Now().UTC().Format(time.RFC3339)}}}}}
	c.pods[md["name"].(string)] = obj
	return nil
}
func (c *pauseCluster) GetCore(_ context.Context, plural, _, name string) (map[string]any, error) {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	if plural == "pods" {
		return c.pods[name], nil
	}
	return nil, nil
}
func (c *pauseCluster) DeleteCore(_ context.Context, plural, _, name string) error {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	if plural == "pods" {
		delete(c.pods, name)
	}
	return nil
}

// s3Bucket is a bucket that keeps what is put and answers HEAD, DELETE and an empty listing.
type s3Bucket struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (b *s3Bucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodPut:
		b.objects[r.URL.Path] = body
	case r.Method == http.MethodHead:
		o, ok := b.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o)))
	case r.Method == http.MethodDelete:
		delete(b.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Query().Has("list-type"):
		_, _ = w.Write([]byte(`<ListBucketResult></ListBucketResult>`))
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func newPauseFixture(t *testing.T) (*sbxFixture, *pauseCluster, string, string) {
	t.Helper()
	f, keyA, keyB := newE2BFixture(t, gatesHold)
	pc := &pauseCluster{sbxCluster: f.cluster, pods: map[string]map[string]any{}}
	f.h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
		return sandbox.NewWithAPI(pc, es.SandboxConfig(), es.ID), nil
	})
	s3 := httptest.NewServer(&s3Bucket{objects: map[string][]byte{}})
	t.Cleanup(s3.Close)
	if _, err := f.db.C("externalService").SetByID(context.Background(), "sbx", pgdoc.M{
		"config": pgdoc.M{"provider": "sandbox", "region": "az1", "services": pgdoc.M{"sandbox": pgdoc.M{"az1": true}},
			"sandbox": pgdoc.M{"envd": true, "domain": "sbx.example.org", "templateRegistry": "192.0.2.226/t",
				"buildStorage": pgdoc.M{"endpoint": s3.URL, "bucket": "b", "region": "us-east-1"},
				"templates":    pgdoc.M{"base": pgdoc.M{"image": "base:1", "cpu": 1, "memoryGiB": 1}}}},
		"secret": pgdoc.M{"buildStorageAccessKey": "a", "buildStorageSecretKey": "s", "proxyWakeToken": "wake-secret"}}, nil); err != nil {
		t.Fatal(err)
	}
	f.h.SetSandboxTemplates(f.db.C("sandbox_templates"))
	f.h.SetSandboxPaused(f.db.C("sandbox_paused"))
	return f, pc, keyA, keyB
}

func (f *sbxFixture) snapshotRows(proj string) []cloud.CloudResource {
	f.t.Helper()
	rows, err := f.rows.FindByServiceProjectAndType(context.Background(), "sbx", proj, cloud.TypeSandboxSnapshot)
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

func (f *sbxFixture) wake(id, body string) (int, string) {
	req := httptest.NewRequest(http.MethodPost, "/internal/sandboxes/"+id+"/wake", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer wake-secret")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// Pause keeps the sandbox's files as a billed snapshot and removes the pod; another project can neither resume, fork
// nor wake it; a suspended profile keeps the snapshot but is refused resume and wake; the wake with the sandbox's own
// token brings it back under its id and ends the snapshot's billing; fork and named snapshots work from a running one;
// the project's teardown removes paused records, snapshots and their rows.
func TestE2BPauseResumeForkWake(t *testing.T) {
	f, pc, keyA, keyB := newPauseFixture(t)
	ctx := context.Background()
	ns := sandbox.NamespaceFor("pa")

	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base","autoPause":true,"autoPauseMemory":true}`); code != http.StatusBadRequest {
		t.Fatalf("autoPauseMemory: %d %s", code, body)
	}
	code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base","timeout":300,"autoPause":true,"autoResume":{"enabled":true}}`)
	var sb struct{ SandboxID, EnvdAccessToken string }
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &sb) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	id := sb.SandboxID

	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{"memory":true}`); code != http.StatusBadRequest || !strings.Contains(body, "filesystem") {
		t.Fatalf("pause with memory must be refused, not downgraded: %d %s", code, body)
	}
	if f.cluster.count() != 1 {
		t.Fatal("a refused pause touched the sandbox")
	}
	if code, body := f.e2b(keyB, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusNotFound {
		t.Fatalf("another project paused it: %d %s", code, body)
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause: %d %s", code, body)
	}
	if _, err := pc.GetCR(ctx, "", "", "", ns, id); err != nil || f.cluster.count() != 0 {
		t.Fatalf("the paused sandbox still runs: %d", f.cluster.count())
	}
	if code, body := f.e2b(keyA, http.MethodGet, "/sandboxes/"+id, ""); code != http.StatusOK || !strings.Contains(body, `"state":"paused"`) {
		t.Fatalf("get paused: %d %s", code, body)
	}
	if code, body := f.e2b(keyA, http.MethodGet, "/v2/sandboxes?state=paused", ""); code != http.StatusOK || !strings.Contains(body, id) {
		t.Fatalf("list paused: %d %s", code, body)
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusConflict {
		t.Fatalf("pause twice: %d %s", code, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(f.snapshotRows("pa")) == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	rows := f.snapshotRows("pa")
	if len(rows) != 1 || rows[0].Data["kind"] != "pause" || rows[0].Data["sizeGB"].(float64) <= 0 {
		t.Fatalf("the stored snapshot is not billed: %+v", rows)
	}

	// Another project: not found everywhere; a wake with the wrong token is refused.
	for _, p := range []string{"/resume", "/connect", "/fork"} {
		if code, body := f.e2b(keyB, http.MethodPost, "/sandboxes/"+id+p, `{}`); code != http.StatusNotFound {
			t.Errorf("tenant B %s: %d %s", p, code, body)
		}
	}
	if code, _ := f.e2b(keyB, http.MethodGet, "/sandboxes/"+id, ""); code != http.StatusNotFound {
		t.Errorf("tenant B sees the paused sandbox: %d", code)
	}
	if code, body := f.wake(id, `{"port":49983,"envdAccessToken":"wrong"}`); code != http.StatusForbidden {
		t.Fatalf("wake with another token: %d %s", code, body)
	}

	// Suspended profile: the snapshot stays, resume and wake are refused.
	setProfile := func(status string) {
		if _, err := f.db.C("billingProfile").SetByID(ctx, "bpa", pgdoc.M{"status": status}, nil); err != nil {
			t.Fatal(err)
		}
	}
	setProfile(billing.StatusSuspended)
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/resume", `{}`); code != http.StatusForbidden {
		t.Fatalf("resume while suspended: %d %s", code, body)
	}
	if code, body := f.wake(id, `{"port":49983,"envdAccessToken":"`+sb.EnvdAccessToken+`"}`); code != http.StatusForbidden {
		t.Fatalf("wake while suspended: %d %s", code, body)
	}
	if f.cluster.count() != 0 || len(f.snapshotRows("pa")) != 1 {
		t.Fatal("the suspension must keep the snapshot and start nothing")
	}
	setProfile(billing.StatusActive)

	// The wake with the sandbox's own token resumes it under its id.
	if code, body := f.wake(id, `{"port":49983,"envdAccessToken":"`+sb.EnvdAccessToken+`"}`); code != http.StatusOK {
		t.Fatalf("wake: %d %s", code, body)
	}
	if cr, _ := pc.GetCR(ctx, "", "", "", ns, id); cr == nil || !strings.Contains(fmtJSON(cr), "192.0.2.226/t/pa/") {
		t.Fatalf("not resumed from the snapshot image: %v", cr)
	}
	if code, body := f.e2b(keyA, http.MethodGet, "/sandboxes/"+id, ""); code != http.StatusOK || !strings.Contains(body, `"state":"running"`) {
		t.Fatalf("after wake: %d %s", code, body)
	}
	if n := len(f.snapshotRows("pa")); n != 0 {
		t.Fatalf("the resumed snapshot is still billed: %d rows", n)
	}
	if code, _ := f.wake(id, `{"port":49983,"envdAccessToken":"`+sb.EnvdAccessToken+`"}`); code != http.StatusNotFound {
		t.Fatalf("a running sandbox is not woken: %d", code)
	}

	// Fork: new id, new token, same files.
	code, body = f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/fork", `{"count":1,"timeout":120}`)
	var forks []struct {
		Sandbox *struct{ SandboxID, EnvdAccessToken string } `json:"sandbox"`
	}
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &forks) != nil || len(forks) != 1 || forks[0].Sandbox == nil ||
		forks[0].Sandbox.SandboxID == id || forks[0].Sandbox.EnvdAccessToken == sb.EnvdAccessToken {
		t.Fatalf("fork: %d %s", code, body)
	}
	if f.cluster.count() != 2 {
		t.Fatalf("after fork: %d sandboxes", f.cluster.count())
	}

	// A named snapshot is a template of the project: listed, startable, not another project's.
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/snapshots", `{"name":"keep1"}`); code != http.StatusCreated {
		t.Fatalf("snapshot: %d %s", code, body)
	}
	if code, body := f.e2b(keyA, http.MethodGet, "/snapshots", ""); code != http.StatusOK || !strings.Contains(body, "keep1") {
		t.Fatalf("snapshots: %d %s", code, body)
	}
	if code, body := f.e2b(keyB, http.MethodPost, "/sandboxes", `{"templateID":"keep1"}`); code == http.StatusCreated {
		t.Fatalf("tenant B started tenant A's snapshot: %s", body)
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"keep1","timeout":60}`); code != http.StatusCreated {
		t.Fatalf("start from the snapshot: %d %s", code, body)
	}

	// The lease: concurrent connects of a paused sandbox bring it back once and all answer with it.
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause again: %d %s", code, body)
	}
	before := f.cluster.count()
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Go(func() { codes[i], _ = f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":60}`) })
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusCreated && c != http.StatusOK {
			t.Errorf("connect %d: %d", i, c)
		}
	}
	if f.cluster.count() != before+1 {
		t.Fatalf("concurrent resumes started %d sandboxes", f.cluster.count()-before)
	}

	// Pause one more, then tear the project down: nothing of it is left.
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause again: %d %s", code, body)
	}
	if err := f.h.TeardownProject(ctx, "pa"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if n, _ := f.db.C("sandbox_paused").Count(ctx, pgdoc.M{"projectId": "pa"}); n != 0 {
		t.Errorf("%d paused records left", n)
	}
	if n, _ := f.db.C("sandbox_templates").Count(ctx, pgdoc.M{"projectId": "pa"}); n != 0 {
		t.Errorf("%d snapshot templates left", n)
	}
	if rows := f.snapshotRows("pa"); len(rows) != 0 {
		t.Errorf("snapshot rows left: %+v", rows)
	}
}

func fmtJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
