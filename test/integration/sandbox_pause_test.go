//go:build integration

package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/billingjob"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

// pauseCluster is the in-memory cluster with what a snapshot needs: exec streaming the changed files, and assembly
// pods in sbx-build that finish at once with a digest.
type pauseCluster struct {
	*sbxCluster
	pmu  sync.Mutex
	pods map[string]map[string]any
	// cut: the capture stream ends early although the exec reports success.
	cut bool
	// hold: assembly pods stay Pending (a slow site) until release.
	hold bool
	// captures: snapshot captures taken (exec of sandbox-init -snapshot).
	captures int
}

func succeeded() map[string]any {
	return map[string]any{"phase": "Succeeded", "containerStatuses": []any{map[string]any{"name": "push",
		"state": map[string]any{"terminated": map[string]any{"exitCode": 0, "message": "192.0.2.226/t/x@sha256:" + strings.Repeat("a", 64),
			"finishedAt": time.Now().UTC().Format(time.RFC3339)}}}}}
}

// release lets every held assembly pod finish, and the next ones finish at once.
func (c *pauseCluster) release() {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	c.hold = false
	for _, p := range c.pods {
		p["status"] = succeeded()
	}
}

func (c *pauseCluster) ExecStream(_ context.Context, _, _, _ string, cmd []string, w io.Writer, _ int) (int, string, error) {
	if len(cmd) != 2 || cmd[1] != "-snapshot" {
		return 1, "unexpected command", nil
	}
	c.pmu.Lock()
	c.captures++
	c.pmu.Unlock()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := strings.Repeat("layer-bytes ", 1000)
	_ = tw.WriteHeader(&tar.Header{Name: "home/user/notes.txt", Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()
	b := buf.Bytes()
	if c.cut {
		b = b[:len(b)/2]
	}
	_, err := w.Write(b)
	return 0, "", err
}
func (c *pauseCluster) ApplyCore(_ context.Context, plural string, obj map[string]any) error {
	if plural != "pods" {
		return nil
	}
	c.pmu.Lock()
	defer c.pmu.Unlock()
	md := obj["metadata"].(map[string]any)
	obj["status"] = succeeded()
	if c.hold {
		obj["status"] = map[string]any{"phase": "Pending", "startTime": time.Now().UTC().Format(time.RFC3339)}
	}
	c.pods[md["name"].(string)] = obj
	return nil
}
func (c *pauseCluster) GetCore(_ context.Context, plural, _, name string) (map[string]any, error) {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	if plural == "pods" {
		return maps.Clone(c.pods[name]), nil // release rewrites a pod's status under pmu: hand out a copy
	}
	return nil, nil
}

// ListCRsAllNamespaces: no sandbox is due for autoPause (the job's listing).
func (c *pauseCluster) ListCRsAllNamespaces(context.Context, string, string, string, string) ([]map[string]any, error) {
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

// pausedState waits up to 10 s for the sandbox's paused record to reach state ("" = paused; "gone" = no record).
func (f *sbxFixture) pausedState(id, want string) {
	f.t.Helper()
	got := ""
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		var rec struct{ State string }
		found, err := f.db.C("sandbox_paused").Get(context.Background(), id, &rec)
		if err != nil {
			f.t.Fatal(err)
		}
		if got = rec.State; !found {
			got = "gone"
		}
		if got == want {
			return
		}
	}
	f.t.Fatalf("paused record of %s: %q, want %q", id, got, want)
}

// shutdownWithin waits up to 10 s for the sandbox's cluster deadline (spec.shutdownTime) to fall in [lo, hi] from now.
func (f *sbxFixture) shutdownWithin(pc *pauseCluster, id string, lo, hi time.Duration) {
	f.t.Helper()
	left := time.Duration(0)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		cr, _ := pc.GetCR(context.Background(), "", "", "", sandbox.NamespaceFor("pa"), id)
		spec, _ := cr["spec"].(map[string]any)
		end, err := time.Parse(time.RFC3339, fmt.Sprint(spec["shutdownTime"]))
		if left = time.Until(end); err == nil && left >= lo && left <= hi {
			return
		}
	}
	f.t.Fatalf("sandbox %s ends in %v, want %v..%v", id, left, lo, hi)
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
	// A capture cut short fails the pause while the sandbox still runs: nothing paused, nothing billed.
	pc.cut = true
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code < 500 {
		t.Fatalf("pause on a cut capture: %d %s", code, body)
	}
	pc.cut = false
	if n, _ := f.db.C("sandbox_paused").Count(ctx, pgdoc.M{}); f.cluster.count() != 1 || n != 0 || len(f.snapshotRows("pa")) != 0 {
		t.Fatalf("a failed capture paused the sandbox: running %d, paused %d", f.cluster.count(), n)
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause: %d %s", code, body)
	}
	f.pausedState(id, "")
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
	// The profile suspender's sandbox leg (D9) deletes running sandboxes only: the paused one keeps its snapshot.
	proj, err := f.projs.FindByID(ctx, "pa")
	if err != nil || proj == nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := f.h.DeleteProjectSandboxes(ctx, proj); err != nil {
		t.Fatalf("suspender leg: %v", err)
	}
	if n, _ := f.db.C("sandbox_paused").Count(ctx, pgdoc.M{"projectId": "pa"}); n != 1 {
		t.Fatalf("the suspension dropped the paused sandbox: %d records", n)
	}
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
	f.pausedState(id, "")
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
	f.pausedState(id, "")
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

// A paused sandbox's snapshot bills per stored GB-hour on the real charge driver, unit by unit across charges, and
// stops once the resume archives its row.
func TestSandboxSnapshotBilledPerGBHour(t *testing.T) {
	f, _, keyA, _ := newPauseFixture(t)
	ctx := context.Background()
	code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base","timeout":300}`)
	var sb struct{ SandboxID string }
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &sb) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+sb.SandboxID+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause: %d %s", code, body)
	}
	f.pausedState(sb.SandboxID, "")
	deadline := time.Now().Add(10 * time.Second)
	for len(f.snapshotRows("pa")) == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	rows := f.snapshotRows("pa")
	if len(rows) != 1 {
		t.Fatalf("snapshot rows: %+v", rows)
	}
	// The test layer is a few KB: price a 2 GB snapshot stored since three whole hours ago.
	now := time.Now().UTC().Truncate(time.Hour)
	stored := now.Add(-3 * time.Hour)
	row := rows[0]
	row.Data["sizeGB"], row.Data["createdAt"], row.CreatedAt = 2.0, stored.Format(time.RFC3339), &stored
	if _, err := f.rows.Update(ctx, &row); err != nil {
		t.Fatal(err)
	}

	mustInsert(t, f.db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	if _, err := f.db.C("billingProfile").SetByID(ctx, "bpa", pgdoc.M{"currency": "USD",
		"pricePlanConfig": pgdoc.M{"includePublicPricePlans": true}}, nil); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, f.db, "pricePlan", pricing.PricePlan{ID: "pp-public", Enabled: true, AccessMode: pricing.AccessPublic})
	mustInsert(t, f.db, "pricePlanRule", pricing.PricePlanRule{PricePlanID: "pp-public", TimeUnit: pricing.TimeUnitHour,
		ResourceType: "sandbox_snapshot", Prices: []pricing.PricePlanRulePrice{{AttributeName: "size_gb",
			Tiers: []pricing.PriceTier{{From: dptr("0"), Value: dptr("0.05")}}}}})
	at := now
	driver := billingjob.New(billingjob.Deps{
		Billing: billing.NewRepo(f.db), ExternalServices: f.es, Projects: f.projs, Orgs: org.NewRepo(f.db),
		Pricing: pricing.NewRepo(f.db), Engine: pricing.NewEngine(pricing.SystemClock()), Cloud: f.rows,
		Registry: map[string]billingresource.Provider{cloud.TypeSandboxSnapshot: billingresource.NewSandboxSnapshotProvider()},
		Now:      func() time.Time { return at },
	})
	billed := func() decimal.Decimal {
		t.Helper()
		var bill pricing.Bill
		if found, err := f.db.C("bill").FindOne(ctx, pgdoc.M{"billingProfileId": "bpa"}, &bill); err != nil || !found {
			t.Fatalf("no bill: %v %v", found, err)
		}
		sum := decimal.Zero
		for _, it := range bill.Items {
			if it.ResourceID == row.ID {
				sum = sum.Add(it.NetAmount)
			}
		}
		return sum
	}
	// 2 GB × 3 h × 0.05 = 0.30; one more hour = 0.40 (no hour counted twice).
	if err := driver.Charge(ctx, pricing.TimeUnitHour, at); err != nil {
		t.Fatal(err)
	}
	if got := billed(); !got.Equal(decimal.RequireFromString("0.3")) {
		t.Fatalf("after 3 h: %s, want 0.30", got)
	}
	at = now.Add(time.Hour)
	if err := driver.Charge(ctx, pricing.TimeUnitHour, at); err != nil {
		t.Fatal(err)
	}
	if got := billed(); !got.Equal(decimal.RequireFromString("0.4")) {
		t.Fatalf("after 4 h: %s, want 0.40", got)
	}
	// The resume (wall clock, inside the last charged hour) archives the row: the next hour adds nothing.
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+sb.SandboxID+"/resume", `{}`); code != http.StatusCreated {
		t.Fatalf("resume: %d %s", code, body)
	}
	if n := len(f.snapshotRows("pa")); n != 0 {
		t.Fatalf("the resumed snapshot's row is still open: %d", n)
	}
	at = now.Add(2 * time.Hour)
	if err := driver.Charge(ctx, pricing.TimeUnitHour, at); err != nil {
		t.Fatal(err)
	}
	if got := billed(); !got.Equal(decimal.RequireFromString("0.4")) {
		t.Fatalf("after the resume: %s, want 0.40", got)
	}
}

// failingAssembly: every snapshot assembly pod fails (crane could not read the layer, the registry is full, ...).
type failingAssembly struct{ *pauseCluster }

func (c failingAssembly) ApplyCore(ctx context.Context, plural string, obj map[string]any) error {
	if err := c.pauseCluster.ApplyCore(ctx, plural, obj); err != nil || plural != "pods" {
		return err
	}
	obj["status"] = map[string]any{"phase": "Failed", "message": "crane: unexpected EOF"}
	return nil
}

// A pause whose snapshot cannot be assembled ends while the sandbox still runs: nothing is deleted, paused or billed.
// The pause answers once the capture is stored, so the failure shows as the sandbox running again.
func TestE2BPauseFailedAssemblyKeepsSandbox(t *testing.T) {
	f, pc, keyA, _ := newPauseFixture(t)
	ctx := context.Background()
	f.h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
		return sandbox.NewWithAPI(failingAssembly{pc}, es.SandboxConfig(), es.ID), nil
	})
	code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base","timeout":300}`)
	var sb struct{ SandboxID string }
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &sb) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+sb.SandboxID+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause with a failing assembly: %d %s", code, body)
	}
	f.pausedState(sb.SandboxID, "gone")
	f.shutdownWithin(pc, sb.SandboxID, 0, 300*time.Second) // the pause's hold is gone: its own end again
	n, _ := f.db.C("sandbox_paused").Count(ctx, pgdoc.M{})
	tpls, _ := f.db.C("sandbox_templates").Count(ctx, pgdoc.M{})
	if f.cluster.count() != 1 || n != 0 || tpls != 0 || len(f.snapshotRows("pa")) != 0 {
		t.Fatalf("running %d, paused %d, snapshots %d, rows %d", f.cluster.count(), n, tpls, len(f.snapshotRows("pa")))
	}
	if code, body := f.e2b(keyA, http.MethodGet, "/sandboxes/"+sb.SandboxID, ""); code != http.StatusOK || !strings.Contains(body, `"state":"running"`) {
		t.Fatalf("the sandbox after the failed pause: %d %s", code, body)
	}
	// One past its end (an autoPause sandbox the job pauses after its end) is not given that end back, which would
	// remove it at once: it runs a few minutes, for the next tick to pause it again.
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := pc.MergePatchCR(ctx, "", "", "", sandbox.NamespaceFor("pa"), sb.SandboxID, map[string]any{"spec": map[string]any{"shutdownTime": past}}); err != nil {
		t.Fatal(err)
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+sb.SandboxID+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause past its end: %d %s", code, body)
	}
	f.pausedState(sb.SandboxID, "gone")
	f.shutdownWithin(pc, sb.SandboxID, 4*time.Minute, 5*time.Minute)
}

// A portal that stops mid-pause leaves its lease to lapse: the autoPause tick takes the pause over and finishes it
// (the sandbox deleted, the record paused, resumable from its image); the old driver finds the pause taken.
func TestE2BPauseRedrivenAfterRestart(t *testing.T) {
	f, pc, keyA, _ := newPauseFixture(t)
	ctx := context.Background()
	pc.pmu.Lock()
	pc.hold = true
	pc.pmu.Unlock()
	id := f.e2bCreate(keyA)
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause: %d %s", code, body)
	}
	f.pausedState(id, "pausing")
	past := time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
	if _, err := f.db.C("sandbox_paused").SetByID(ctx, id, pgdoc.M{"leaseUntil": past}, nil); err != nil {
		t.Fatal(err)
	}
	f.h.AutoPauseSandboxes(ctx)
	var rec struct{ LeaseUntil time.Time }
	if _, err := f.db.C("sandbox_paused").Get(ctx, id, &rec); err != nil || time.Until(rec.LeaseUntil) <= 0 {
		t.Fatalf("not taken over: lease %v, %v", rec.LeaseUntil, err)
	}
	pc.release()
	f.pausedState(id, "")
	if f.cluster.count() != 0 {
		t.Fatalf("paused, still running: %d", f.cluster.count())
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":60}`); code != http.StatusCreated {
		t.Fatalf("connect of the re-driven pause: %d %s", code, body)
	}
}

// A pause re-driven long after it started (the portal down past snapshotWait, or past the pause's hold, when the
// cluster has already removed the sandbox) still follows its image: a late build ends paused and resumable, and only a
// failed build gives the pause up, leaving the sandbox running if it still exists. The pause's start is set back in
// its record, which is the only clock the drive reads.
func TestE2BPauseRedrivenLate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		age        time.Duration
		gone, fail bool
	}{
		{"past snapshotWait", 16 * time.Minute, false, false},
		{"past the hold, sandbox removed", 26 * time.Minute, true, false},
		{"past snapshotWait, build failed", 16 * time.Minute, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, pc, keyA, _ := newPauseFixture(t)
			ctx := context.Background()
			pc.pmu.Lock()
			pc.hold = true
			pc.pmu.Unlock()
			id := f.e2bCreate(keyA)
			if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusNoContent {
				t.Fatalf("pause: %d %s", code, body)
			}
			f.pausedState(id, "pausing")
			now := time.Now().UTC()
			if _, err := f.db.C("sandbox_paused").SetByID(ctx, id, pgdoc.M{"pausedAt": now.Add(-tc.age).Format(time.RFC3339),
				"leaseUntil": now.Add(-time.Second).Format(time.RFC3339)}, nil); err != nil {
				t.Fatal(err)
			}
			if tc.gone { // removed by the cluster at the pause's hold while nobody drove the pause
				if err := pc.DeleteCR(ctx, "", "", "", sandbox.NamespaceFor("pa"), id); err != nil {
					t.Fatal(err)
				}
			}
			f.h.AutoPauseSandboxes(ctx)
			time.Sleep(3 * time.Second) // past the re-drive's first reads of the build
			f.pausedState(id, "pausing")
			if n, _ := f.db.C("sandbox_templates").Count(ctx, pgdoc.M{"projectId": "pa"}); n != 1 {
				t.Fatalf("the re-drive dropped the snapshot of a build still running: %d", n)
			}
			if tc.fail {
				pc.pmu.Lock()
				for _, p := range pc.pods {
					p["status"] = map[string]any{"phase": "Failed", "message": "crane: unexpected EOF"}
				}
				pc.pmu.Unlock()
				f.pausedState(id, "gone")
				if f.cluster.count() != 1 {
					t.Fatalf("the failed build took the sandbox: running %d", f.cluster.count())
				}
				return
			}
			pc.release()
			f.pausedState(id, "")
			if f.cluster.count() != 0 {
				t.Fatalf("paused, still running: %d", f.cluster.count())
			}
			if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":60}`); code != http.StatusCreated {
				t.Fatalf("connect of the late pause: %d %s", code, body)
			}
			if cr, _ := pc.GetCR(ctx, "", "", "", sandbox.NamespaceFor("pa"), id); cr == nil || !strings.Contains(fmtJSON(cr), "192.0.2.226/t/pa/") {
				t.Fatalf("not resumed from the snapshot image: %v", cr)
			}
		})
	}
}

// A native delete of a sandbox whose pause is under way ends the pause like the E2B kill: the sandbox is gone and
// stays gone once the image is ready (no paused record, no snapshot).
func TestSandboxNativeDeleteDuringPause(t *testing.T) {
	f, pc, keyA, _ := newPauseFixture(t)
	ctx := context.Background()
	pc.pmu.Lock()
	pc.hold = true
	pc.pmu.Unlock()
	id := f.e2bCreate(keyA)
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause: %d %s", code, body)
	}
	f.pausedState(id, "pausing")
	rows, err := f.rows.FindByServiceProjectAndType(ctx, "sbx", "pa", cloud.TypeSandbox)
	if err != nil || len(rows) != 1 || rows[0].ExternalID != id {
		t.Fatalf("rows: %v %+v", err, rows)
	}
	if code, body := f.do(sbxA, http.MethodDelete, "/project/pa/cloud/"+rows[0].ID, ""); code != http.StatusAccepted {
		t.Fatalf("native delete while pausing: %d %s", code, body)
	}
	pc.release()
	time.Sleep(3 * time.Second) // the pause's driver sees the image ready
	f.pausedState(id, "gone")
	if n, _ := f.db.C("sandbox_templates").Count(ctx, pgdoc.M{"projectId": "pa"}); n != 0 || f.cluster.count() != 0 {
		t.Fatalf("after the delete: snapshots %d, running %d", n, f.cluster.count())
	}
}

// Repeated successful snapshots retain their history, but two build records naming
// the same physical immutable image must charge that stored digest only once.
func TestE2BSnapshotRetakenBillsKeptImage(t *testing.T) {
	f, _, keyA, _ := newPauseFixture(t)
	ctx := context.Background()
	code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base","timeout":300}`)
	var sb struct{ SandboxID string }
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &sb) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	type rec struct {
		Builds []struct {
			Image     string `json:"image"`
			SizeBytes int64  `json:"sizeBytes"`
		} `json:"builds"`
	}
	var tpl rec
	for i := range 2 {
		if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+sb.SandboxID+"/snapshots", `{"name":"again"}`); code != http.StatusCreated {
			t.Fatalf("snapshot %d: %d %s", i, code, body)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			tpl = rec{}
			found, err := f.db.C("sandbox_templates").FindOne(ctx, pgdoc.M{"projectId": "pa", "name": "again"}, &tpl)
			if err != nil || !found {
				t.Fatalf("template: %v %v", found, err)
			}
			if len(tpl.Builds) == i+1 && tpl.Builds[len(tpl.Builds)-1].Image != "" || time.Now().After(deadline) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if len(tpl.Builds) != 2 || tpl.Builds[0].Image == "" || tpl.Builds[0].Image != tpl.Builds[1].Image || tpl.Builds[0].SizeBytes <= 0 {
		t.Fatalf("builds kept: %+v", tpl.Builds)
	}
	rows := f.snapshotRows("pa")
	if len(rows) != 1 {
		t.Fatalf("rows: %d", len(rows))
	}
	if got, want := rows[0].Data["sizeGB"].(float64), float64(tpl.Builds[0].SizeBytes)/(1<<30); got != want {
		t.Fatalf("billed %v GB, kept %v GB", got, want)
	}
}

// On a slow site (assembly pods Pending) every call answers within the E2B SDK's 120 s: the pause once the capture is
// stored (the sandbox lists once, as paused, and still runs), a connect after PauseJoinWait with the running sandbox
// (the pause cancelled, nothing left of it), a kill at once, and a fork or a create from a snapshot with 503 after
// SnapshotRequestWait (the fork's snapshot kept for a retry, the named one still assembling). Once the image is ready the pause
// deletes the sandbox and a connect resumes it from the image.
func TestE2BPauseOnASlowSite(t *testing.T) {
	f, pc, keyA, _ := newPauseFixture(t)
	ctx := context.Background()
	defer func(j, w time.Duration) { project.PauseJoinWait, project.SnapshotRequestWait = j, w }(project.PauseJoinWait, project.SnapshotRequestWait)
	project.PauseJoinWait, project.SnapshotRequestWait = time.Second, time.Second
	pc.pmu.Lock()
	pc.hold = true
	pc.pmu.Unlock()
	id := f.e2bCreate(keyA)
	templates := func() int64 {
		n, _ := f.db.C("sandbox_templates").Count(ctx, pgdoc.M{"projectId": "pa"})
		return n
	}

	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause: %d %s", code, body)
	}
	f.pausedState(id, "pausing")
	if f.cluster.count() != 1 {
		t.Fatal("the sandbox was deleted before its image was ready")
	}
	// Created for 300 s: held past that while its image is assembled, so its end cannot remove it first.
	f.shutdownWithin(pc, id, 20*time.Minute, 26*time.Minute)
	if code, body := f.e2b(keyA, http.MethodGet, "/sandboxes/"+id, ""); code != http.StatusOK || !strings.Contains(body, `"state":"paused"`) {
		t.Fatalf("get while pausing: %d %s", code, body)
	}
	if code, body := f.e2b(keyA, http.MethodGet, "/v2/sandboxes", ""); code != http.StatusOK || strings.Count(body, id) != 1 || !strings.Contains(body, `"state":"paused"`) {
		t.Fatalf("list while pausing (once, paused): %d %s", code, body)
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusConflict {
		t.Fatalf("pause while pausing: %d %s", code, body)
	}
	// The connect waits PauseJoinWait, then cancels the pause and answers with the sandbox as it runs.
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":60}`); code != http.StatusCreated || !strings.Contains(body, id) {
		t.Fatalf("connect while pausing: %d %s", code, body)
	}
	f.pausedState(id, "gone")
	f.shutdownWithin(pc, id, 50*time.Second, 61*time.Second) // the connect's timeout, not the pause's hold
	if f.cluster.count() != 1 || templates() != 0 || len(f.snapshotRows("pa")) != 0 {
		t.Fatalf("after the cancelled pause: running %d, snapshots %d, rows %d", f.cluster.count(), templates(), len(f.snapshotRows("pa")))
	}
	if code, body := f.e2b(keyA, http.MethodGet, "/sandboxes/"+id, ""); code != http.StatusOK || !strings.Contains(body, `"state":"running"`) {
		t.Fatalf("get after the cancelled pause: %d %s", code, body)
	}

	// A kill while pausing ends the pause and the sandbox.
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause: %d %s", code, body)
	}
	if code, body := f.e2b(keyA, http.MethodDelete, "/sandboxes/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("kill while pausing: %d %s", code, body)
	}
	f.pausedState(id, "gone")
	if f.cluster.count() != 0 || templates() != 0 {
		t.Fatalf("after the kill: running %d, snapshots %d", f.cluster.count(), templates())
	}

	// Fork and create from a snapshot answer 503 when the image is late.
	id = f.e2bCreate(keyA)
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/fork", `{"count":1,"timeout":60}`); code != http.StatusServiceUnavailable {
		t.Fatalf("fork on a slow site: %d %s", code, body)
	}
	if templates() != 1 || f.cluster.count() != 1 {
		t.Fatalf("the late fork left %d snapshots (want its own, for a retry), %d sandboxes", templates(), f.cluster.count())
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/snapshots", `{"name":"late"}`); code != http.StatusCreated {
		t.Fatalf("named snapshot: %d %s", code, body)
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"late","timeout":60}`); code != http.StatusServiceUnavailable {
		t.Fatalf("create from a late snapshot: %d %s", code, body)
	}

	// The pause completes once the image is ready; then a connect resumes from it.
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause: %d %s", code, body)
	}
	f.pausedState(id, "pausing")
	pc.release()
	f.pausedState(id, "")
	if f.cluster.count() != 0 {
		t.Fatalf("paused, still running: %d", f.cluster.count())
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":60}`); code != http.StatusCreated {
		t.Fatalf("connect of the paused sandbox: %d %s", code, body)
	}
	if cr, _ := pc.GetCR(ctx, "", "", "", sandbox.NamespaceFor("pa"), id); cr == nil || !strings.Contains(fmtJSON(cr), "192.0.2.226/t/pa/") {
		t.Fatalf("not resumed from the snapshot image: %v", cr)
	}
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"late","timeout":60}`); code != http.StatusCreated {
		t.Fatalf("create from the snapshot once ready: %d %s", code, body)
	}
}

// A connect on a pause whose sandbox the cluster already removed (the hold passed while the build was still running)
// must not cancel the pause: the snapshot is the only copy. It waits and resumes from the image once it is ready.
func TestE2BConnectWaitsWhenPausedSandboxIsGone(t *testing.T) {
	f, pc, keyA, _ := newPauseFixture(t)
	ctx := context.Background()
	old := project.PauseJoinWait
	project.PauseJoinWait = 0
	t.Cleanup(func() { project.PauseJoinWait = old })
	pc.pmu.Lock()
	pc.hold = true
	pc.pmu.Unlock()
	id := f.e2bCreate(keyA)
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); code != http.StatusNoContent {
		t.Fatalf("pause: %d %s", code, body)
	}
	f.pausedState(id, "pausing")
	if err := pc.DeleteCR(ctx, "", "", "", sandbox.NamespaceFor("pa"), id); err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(3 * time.Second); pc.release() }()
	if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":60}`); code != http.StatusCreated {
		t.Fatalf("connect while the build still runs and the sandbox is gone: %d %s", code, body)
	}
	if cr, _ := pc.GetCR(ctx, "", "", "", sandbox.NamespaceFor("pa"), id); cr == nil || !strings.Contains(fmtJSON(cr), "192.0.2.226/t/pa/") {
		t.Fatalf("not resumed from the snapshot image: %v", cr)
	}
}

// On a site where the assembly takes longer than SnapshotRequestWait, a fork answers 503 and keeps its snapshot: a
// retry within ForkReuseWindow waits for that same snapshot instead of capturing again, and gets the fork once the
// image is ready. Another project's fork of the sandbox stays refused and never sees it; one no retry takes is dropped
// by job sandboxBuildSync past the window.
func TestE2BForkRetryReusesSnapshot(t *testing.T) {
	f, pc, keyA, keyB := newPauseFixture(t)
	ctx := context.Background()
	defer func(w, r time.Duration) { project.SnapshotRequestWait, project.ForkReuseWindow = w, r }(project.SnapshotRequestWait, project.ForkReuseWindow)
	project.SnapshotRequestWait = time.Second
	pc.pmu.Lock()
	pc.hold = true
	pc.pmu.Unlock()
	id := f.e2bCreate(keyA)
	snapshots := func(proj string) []map[string]any {
		var ts []map[string]any
		if err := f.db.C("sandbox_templates").Find(ctx, pgdoc.M{"projectId": proj}, &ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}
	captures := func() int {
		pc.pmu.Lock()
		defer pc.pmu.Unlock()
		return pc.captures
	}
	fork := func(key string) (int, string) {
		return f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/fork", `{"count":1,"timeout":60}`)
	}

	if code, body := fork(keyA); code != http.StatusServiceUnavailable || !strings.Contains(body, "retry") {
		t.Fatalf("fork on a slow site: %d %s", code, body)
	}
	first := snapshots("pa")
	if len(first) != 1 || captures() != 1 || f.cluster.count() != 1 {
		t.Fatalf("after the late fork: %d snapshots, %d captures, %d sandboxes", len(first), captures(), f.cluster.count())
	}
	snapID := fmt.Sprint(first[0]["id"])

	// Another project cannot fork the sandbox, and takes nothing from its pending snapshot.
	if code, body := fork(keyB); code != http.StatusNotFound {
		t.Fatalf("tenant B forked: %d %s", code, body)
	}
	if len(snapshots("pb")) != 0 || captures() != 1 {
		t.Fatalf("tenant B's fork: %d snapshots, %d captures", len(snapshots("pb")), captures())
	}

	// A retry while the image is still assembled: 503 again, no second capture.
	if code, body := fork(keyA); code != http.StatusServiceUnavailable {
		t.Fatalf("retry while assembling: %d %s", code, body)
	}
	if ts := snapshots("pa"); len(ts) != 1 || fmt.Sprint(ts[0]["id"]) != snapID || captures() != 1 {
		t.Fatalf("the retry took a new snapshot: %d snapshots, %d captures", len(ts), captures())
	}

	// Once the image is ready the retry forks from that snapshot, and the snapshot goes.
	pc.release()
	code, body := fork(keyA)
	var forks []struct {
		Sandbox *struct{ SandboxID string } `json:"sandbox"`
	}
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &forks) != nil || len(forks) != 1 || forks[0].Sandbox == nil ||
		forks[0].Sandbox.SandboxID == id {
		t.Fatalf("retry once ready: %d %s", code, body)
	}
	if captures() != 1 || len(snapshots("pa")) != 0 || f.cluster.count() != 2 {
		t.Fatalf("after the fork: %d captures, %d snapshots, %d sandboxes", captures(), len(snapshots("pa")), f.cluster.count())
	}
	if cr, _ := pc.GetCR(ctx, "", "", "", sandbox.NamespaceFor("pa"), forks[0].Sandbox.SandboxID); cr == nil ||
		!strings.Contains(fmtJSON(cr), "192.0.2.226/t/pa/"+snapID+"@") {
		t.Fatalf("the fork does not run from the first snapshot %s: %v", snapID, cr)
	}

	// The next fork captures afresh (the files may have changed since).
	if code, body := fork(keyA); code != http.StatusCreated || captures() != 2 {
		t.Fatalf("a later fork: %d %s, %d captures", code, body, captures())
	}

	// A late fork's snapshot no retry takes is dropped by the build sync past its window.
	pc.pmu.Lock()
	pc.hold = true
	pc.pmu.Unlock()
	project.ForkReuseWindow = 0
	if code, body := fork(keyA); code != http.StatusServiceUnavailable || len(snapshots("pa")) != 1 {
		t.Fatalf("late fork: %d %s, %d snapshots", code, body, len(snapshots("pa")))
	}
	f.h.SyncSandboxBuilds(ctx)
	if n := len(snapshots("pa")); n != 0 || len(f.snapshotRows("pa")) != 0 {
		t.Fatalf("the unused fork snapshot stayed: %d snapshots, %d rows", n, len(f.snapshotRows("pa")))
	}
}
