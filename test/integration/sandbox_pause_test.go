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
func (c *pauseCluster) GetCore(ctx context.Context, plural, ns, name string) (map[string]any, error) {
	if plural == "pods" && strings.HasPrefix(ns, "sb-") {
		return c.sbxCluster.GetCore(ctx, plural, ns, name)
	}
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
	wireRuntimeCreateFixture(t, f)
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

// Current saved lifecycle is held before capture/restore. Historical copies retain
// auth, tenant and suspension checks; explicit authorized kill/teardown still works.
func TestE2BSavedLifecycleAdmissionAuthAndNamedCapture(t *testing.T) {
	f, pc, keyA, keyB := newPauseFixture(t)
	ctx := context.Background()
	id := f.e2bCreate(keyA)
	if c, b := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base","autoPause":true,"autoPauseMemory":true}`); c != 400 {
		t.Fatal(c, b)
	}
	if c, b := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{"memory":true}`); c != 400 || !strings.Contains(b, "filesystem") {
		t.Fatal(c, b)
	}
	if c, _ := f.e2b(keyB, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`); c != 404 {
		t.Fatal("foreign pause", c)
	}
	before := heldStateDigest(t, f)
	c, b := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`)
	requireHeld(t, c, b)
	if captureCount(pc) != 0 || heldStateDigest(t, f) != before || f.cluster.count() != 1 {
		t.Fatal("held pause effects")
	}
	seedHeldPause(t, f, pc, id, "", time.Time{}, false)
	saved := historicalSaved(t, f, id)
	token := sandbox.AccessOf(saved).EnvdAccessToken
	before = heldStateDigest(t, f)
	for _, path := range []string{"/resume", "/connect", "/fork"} {
		c, b = f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+path, `{}`)
		requireHeld(t, c, b)
		if c, _ := f.e2b(keyB, http.MethodPost, "/sandboxes/"+id+path, `{}`); c != 404 {
			t.Fatal("foreign", path, c)
		}
	}
	if c, _ := f.wake(id, `{"port":49983,"envdAccessToken":"wrong"}`); c != 403 {
		t.Fatal("token", c)
	}
	if _, e := f.db.C("billingProfile").SetByID(ctx, "bpa", pgdoc.M{"status": billing.StatusSuspended}, nil); e != nil {
		t.Fatal(e)
	}
	if c, b := f.wake(id, `{"port":49983,"envdAccessToken":"`+token+`"}`); c != 403 || !strings.Contains(b, "suspended") {
		t.Fatal("suspension precedence", c, b)
	}
	if _, e := f.db.C("billingProfile").SetByID(ctx, "bpa", pgdoc.M{"status": billing.StatusActive}, nil); e != nil {
		t.Fatal(e)
	}
	c, b = f.wake(id, `{"port":49983,"envdAccessToken":"`+token+`"}`)
	requireHeld(t, c, b)
	if heldStateDigest(t, f) != before {
		t.Fatal("refused resume dropped copy")
	}
	if c, b := f.e2b(keyA, http.MethodDelete, "/sandboxes/"+id, ""); c != 204 {
		t.Fatal(c, b)
	}
	f.pausedState(id, "gone")
	if f.cluster.count() != 0 || len(f.snapshotRows("pa")) != 0 {
		t.Fatal("explicit kill cleanup")
	}
	if e := f.h.TeardownProject(ctx, "pa"); e != nil {
		t.Fatal(e)
	}
}

func fmtJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// A paused sandbox's snapshot bills per stored GB-hour on the real charge driver, unit by unit across charges, and
// remains billed during a held resume and stops at explicit authorized discard.
func TestSandboxHeldSnapshotBilledUntilAuthorizedDiscard(t *testing.T) {
	f, _, keyA, _ := newPauseFixture(t)
	ctx := context.Background()
	code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base","timeout":300}`)
	var sb struct{ SandboxID string }
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &sb) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	// Seed an existing billed pause; a current pause is intentionally unavailable.
	pc := &pauseCluster{sbxCluster: f.cluster, pods: map[string]map[string]any{}}
	seedHeldPause(t, f, pc, sb.SandboxID, "", time.Time{}, false)
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
	// A held resume keeps the row unchanged. Explicit discard archives it in the
	// last charged hour, so the next hour adds nothing.
	beforeHeld := heldStateDigest(t, f)
	code, body = f.e2b(keyA, http.MethodPost, "/sandboxes/"+sb.SandboxID+"/resume", `{}`)
	requireHeld(t, code, body)
	if heldStateDigest(t, f) != beforeHeld {
		t.Fatal("held resume changed storage/billing row")
	}
	if code, body = f.e2b(keyA, http.MethodDelete, "/sandboxes/"+sb.SandboxID, ""); code != http.StatusNoContent {
		t.Fatalf("authorized discard: %d %s", code, body)
	}
	if n := len(f.snapshotRows("pa")); n != 0 {
		t.Fatalf("the explicitly discarded snapshot row is still open: %d", n)
	}
	at = now.Add(2 * time.Hour)
	if err := driver.Charge(ctx, pricing.TimeUnitHour, at); err != nil {
		t.Fatal(err)
	}
	if got := billed(); !got.Equal(decimal.RequireFromString("0.4")) {
		t.Fatalf("after the discard: %s, want 0.40", got)
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
func TestE2BPauseAdmissionRetainsHistoricalFailedAssembly(t *testing.T) {
	f, pc, key, _ := newPauseFixture(t)
	id := f.e2bCreate(key)
	tpl := seedHeldPause(t, f, pc, id, "pausing", time.Now().Add(-time.Hour), false)
	var doc pgdoc.M
	if ok, e := f.db.C("sandbox_templates").Get(context.Background(), tpl, &doc); e != nil || !ok {
		t.Fatal(e)
	}
	builds := doc["builds"].([]any)
	builds[0].(map[string]any)["status"] = sandbox.BuildError
	if e := f.db.C("sandbox_templates").Upsert(context.Background(), tpl, doc); e != nil {
		t.Fatal(e)
	}
	before := heldStateDigest(t, f)
	cr, _ := pc.GetCR(context.Background(), "", "", "", "sb-pa", id)
	raw := fmtJSON(cr)
	c, b := f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`)
	requireHeld(t, c, b)
	f.h.AutoPauseSandboxes(context.Background())
	if heldStateDigest(t, f) != before || captureCount(pc) != 0 {
		t.Fatal("failed historical copy changed")
	}
	after, _ := pc.GetCR(context.Background(), "", "", "", "sb-pa", id)
	if fmtJSON(after) != raw {
		t.Fatal("TTL/source changed")
	}
}

// A portal that stops mid-pause leaves its lease to lapse: the autoPause tick takes the pause over and finishes it
// (the sandbox deleted, the record paused, resumable from its image); the old driver finds the pause taken.
func TestE2BPauseAdmissionDoesNotRedriveAfterRestart(t *testing.T) {
	f, pc, key, _ := newPauseFixture(t)
	id := f.e2bCreate(key)
	seedHeldPause(t, f, pc, id, "pausing", time.Now().Add(-time.Hour), false)
	before := heldStateDigest(t, f)
	f.h.AutoPauseSandboxes(context.Background())
	c, b := f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":60}`)
	requireHeld(t, c, b)
	if heldStateDigest(t, f) != before || captureCount(pc) != 0 || f.cluster.count() != 1 {
		t.Fatal("redrive changed old state")
	}
}

// A pause re-driven long after it started (the portal down past snapshotWait, or past the pause's hold, when the
// cluster has already removed the sandbox) still follows its image: a late build ends paused and resumable, and only a
// failed build gives the pause up, leaving the sandbox running if it still exists. The pause's start is set back in
// its record, which is the only clock the drive reads.
func TestE2BPauseAdmissionRetainsLateSourceOrOnlyCopy(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(fmt.Sprint(gone), func(t *testing.T) {
			f, pc, key, _ := newPauseFixture(t)
			id := f.e2bCreate(key)
			seedHeldPause(t, f, pc, id, "stopping", time.Now().Add(-24*time.Hour), false)
			if gone {
				if e := pc.DeleteCR(context.Background(), "", "", "", "sb-pa", id); e != nil {
					t.Fatal(e)
				}
			}
			before := heldStateDigest(t, f)
			f.h.AutoPauseSandboxes(context.Background())
			c, b := f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":60}`)
			requireHeld(t, c, b)
			if heldStateDigest(t, f) != before {
				t.Fatal("late state/copy removed")
			}
		})
	}
}

// A native delete of a sandbox whose pause is under way ends the pause like the E2B kill: the sandbox is gone and
// stays gone once the image is ready (no paused record, no snapshot).
func TestSandboxNativeDeleteOfHistoricalPauseDoesNotResurrect(t *testing.T) {
	f, pc, key, _ := newPauseFixture(t)
	ctx := context.Background()
	id := f.e2bCreate(key)
	seedHeldPause(t, f, pc, id, "pausing", time.Now().Add(-time.Hour), false)
	rows, e := f.rows.FindByServiceProjectAndType(ctx, "sbx", "pa", cloud.TypeSandbox)
	if e != nil || len(rows) != 1 {
		t.Fatal(e)
	}
	c, b := f.do(sbxA, http.MethodDelete, "/project/pa/cloud/"+rows[0].ID, "")
	if c != 202 {
		t.Fatal(c, b)
	}
	f.h.AutoPauseSandboxes(ctx)
	f.pausedState(id, "gone")
	if n, _ := f.db.C("sandbox_templates").Count(ctx, pgdoc.M{"projectId": "pa"}); n != 0 || f.cluster.count() != 0 {
		t.Fatal("explicit deletion resurrected state")
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
func TestE2BSlowSiteHeldLifecycleKeepsNamedCaptureAndPinnedCreate(t *testing.T) {
	f, pc, key, _ := newPauseFixture(t)
	id := f.e2bCreate(key)
	pc.pmu.Lock()
	pc.hold = true
	pc.pmu.Unlock()
	old := project.SnapshotRequestWait
	project.SnapshotRequestWait = time.Second
	t.Cleanup(func() { project.SnapshotRequestWait = old })
	for _, path := range []string{"/pause", "/fork"} {
		before := captureCount(pc)
		c, b := f.e2b(key, http.MethodPost, "/sandboxes/"+id+path, `{"count":1,"timeout":60}`)
		requireHeld(t, c, b)
		if captureCount(pc) != before {
			t.Fatal("held path captured")
		}
	}
	if c, b := f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/snapshots", `{"name":"late"}`); c != 201 {
		t.Fatal(c, b)
	}
	if c, b := f.e2b(key, http.MethodPost, "/sandboxes", `{"templateID":"late","timeout":60}`); c != 503 {
		t.Fatal("named pending", c, b)
	}
	pc.release()
	if c, b := f.e2b(key, http.MethodPost, "/sandboxes", `{"templateID":"late","timeout":60}`); c != 201 {
		t.Fatal("named ready pinnedCREATE", c, b)
	}
}

// A connect on a pause whose sandbox the cluster already removed (the hold passed while the build was still running)
// must not cancel the pause: the snapshot is the only copy. It waits and resumes from the image once it is ready.
func TestE2BHeldConnectKeepsOnlyCopyWhenSourceGone(t *testing.T) {
	f, pc, key, _ := newPauseFixture(t)
	id := f.e2bCreate(key)
	seedHeldPause(t, f, pc, id, "pausing", time.Now().Add(-time.Hour), false)
	if e := pc.DeleteCR(context.Background(), "", "", "", "sb-pa", id); e != nil {
		t.Fatal(e)
	}
	before := heldStateDigest(t, f)
	c, b := f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":60}`)
	requireHeld(t, c, b)
	if heldStateDigest(t, f) != before || f.cluster.count() != 0 {
		t.Fatal("held connect discarded only copy or applied")
	}
}

// On a site where the assembly takes longer than SnapshotRequestWait, a fork answers 503 and keeps its snapshot: a
// retry within ForkReuseWindow waits for that same snapshot instead of capturing again, and gets the fork once the
// image is ready. Another project's fork of the sandbox stays refused and never sees it; one no retry takes is dropped
// by job sandboxBuildSync past the window.
func TestE2BHeldForkDoesNotCaptureOrExpireInternalCopy(t *testing.T) {
	f, pc, key, keyB := newPauseFixture(t)
	ctx := context.Background()
	id := f.e2bCreate(key)
	tpl := seedHeldPause(t, f, pc, id, "", time.Time{}, false)
	// Deliberately dirty historical fork: TTL retention must precede diagnostics
	// and retired-build cleanup, not pass only because the fixture was cleaned.
	var oldFork pgdoc.M
	if ok, e := f.db.C("sandbox_templates").Get(ctx, tpl, &oldFork); e != nil || !ok {
		t.Fatal(e)
	}
	oldFork["forkUntil"] = time.Now().Add(-time.Hour)
	builds := oldFork["builds"].([]any)
	delete(builds[0].(map[string]any), "diagnostics")
	oldFork["retiredBuilds"] = []any{pgdoc.M{"buildID": "retired-held", "status": sandbox.BuildError, "reason": "historical"}}
	if e := f.db.C("sandbox_templates").Upsert(ctx, tpl, oldFork); e != nil {
		t.Fatal(e)
	}
	before := heldStateDigest(t, f)
	for range 2 {
		c, b := f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/fork", `{"count":1,"timeout":60}`)
		requireHeld(t, c, b)
	}
	if c, _ := f.e2b(keyB, http.MethodPost, "/sandboxes/"+id+"/fork", `{}`); c != 404 {
		t.Fatal("foreign fork", c)
	}
	f.h.SyncSandboxBuilds(ctx)
	if heldStateDigest(t, f) != before || captureCount(pc) != 0 || f.cluster.count() != 1 {
		t.Fatal("held fork captured/dropped old TTL copy")
	}
}
