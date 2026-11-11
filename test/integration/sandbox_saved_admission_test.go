//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// This seeds HISTORICAL durable state, not a fake successful current pause.
func seedHeldPause(t *testing.T, f *sbxFixture, pc *pauseCluster, id, state string, lease time.Time, uncertain bool) string {
	t.Helper()
	ctx := context.Background()
	es, e := f.es.Get(ctx, "sbx")
	if e != nil {
		t.Fatal(e)
	}
	saved, e := sandbox.NewWithAPI(pc, es.SandboxConfig(), es.ID).SavedSandbox(ctx, "pa", id)
	if e != nil {
		t.Fatal(e)
	}
	md := saved["metadata"].(map[string]any)
	ann, _ := md["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
		md["annotations"] = ann
	}
	ann[sandbox.AnnotationAutoResume] = "true"
	tpl := "0123456789abcdef0123"
	build := uuid.NewString()
	now := time.Now().UTC()
	image := "192.0.2.226/t/pa/" + tpl + "@sha256:" + strings.Repeat("a", 64)
	doc := pgdoc.M{"_id": tpl, "projectId": "pa", "name": "historical-held-copy", "kind": "snapshot", "internal": true, "sourceSandbox": id, "cpu": 1, "memoryGiB": 1, "tags": pgdoc.M{"default": build}, "uploadToken": "historical-secret", "createdAt": now, "updatedAt": now, "builds": []any{pgdoc.M{"buildID": build, "status": sandbox.BuildReady, "image": image, "sizeBytes": int64(1 << 20), "createdAt": now, "finishedAt": now, "tags": []string{"default"}, "diagnostics": pgdoc.M{"archivedAt": now, "cleanedAt": now}}}}
	if e = f.db.C("sandbox_templates").Upsert(ctx, tpl, doc); e != nil {
		t.Fatal(e)
	}
	pause := pgdoc.M{"id": id, "projectId": "pa", "serviceId": "sbx", "templateId": tpl, "saved": saved, "state": state, "pausedAt": now.Add(-time.Hour), "resumeUncertain": uncertain}
	if !lease.IsZero() {
		pause["leaseUntil"] = lease
	}
	if e = f.db.C("sandbox_paused").Upsert(ctx, id, pause); e != nil {
		t.Fatal(e)
	}
	if _, e = f.rows.Insert(ctx, &cloud.CloudResource{ProjectID: "pa", ServiceID: "sbx", Region: "az1", Type: cloud.TypeSandboxSnapshot, ExternalID: tpl, CreatedAt: &now, UpdatedAt: &now, Data: map[string]any{"kind": "pause", "sizeGB": float64(1.0 / 1024), "createdAt": now.Format(time.RFC3339)}}); e != nil {
		t.Fatal(e)
	}
	return tpl
}
func heldStateDigest(t *testing.T, f *sbxFixture) string {
	t.Helper()
	out := ""
	for _, table := range []string{"sandbox_paused", "sandbox_templates", "cloudResource", "cloudResourceHistory", "registry_pin", "registry_runtime_dispatch", "registry_snapshot_dispatch"} {
		var exists bool
		if e := f.db.Pool.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, `public."`+table+`"`).Scan(&exists); e != nil {
			t.Fatal(e)
		}
		if !exists {
			continue
		}
		var digest string
		if e := f.db.Pool.QueryRow(context.Background(), `SELECT md5(coalesce(string_agg(to_jsonb(t)::text,'' ORDER BY to_jsonb(t)::text),'')) FROM "`+table+`" t`).Scan(&digest); e != nil {
			t.Fatal(e)
		}
		out += table + ":" + digest + ";"
	}
	return out
}
func requireHeld(t *testing.T, code int, body string) {
	t.Helper()
	if code != http.StatusForbidden || !strings.Contains(body, "Managed snapshot resume/fork is held") || strings.Contains(body, "historical-secret") {
		t.Fatalf("want static held403, got %d %s", code, body)
	}
}
func captureCount(pc *pauseCluster) int { pc.pmu.Lock(); defer pc.pmu.Unlock(); return pc.captures }
func historicalSaved(t *testing.T, f *sbxFixture, id string) map[string]any {
	t.Helper()
	var p struct{ Saved map[string]any }
	if ok, e := f.db.C("sandbox_paused").Get(context.Background(), id, &p); e != nil || !ok {
		t.Fatal(e)
	}
	return p.Saved
}
func TestSavedAdmissionHistoricalStatesDoNotMutateAnyReferenceOrTTL(t *testing.T) {
	for _, state := range []string{"", "pausing", "stopping", "resuming", "resuming-uncertain", "expired-pausing", "expired-stopping", "expired-resuming"} {
		t.Run(state, func(t *testing.T) {
			f, pc, key, _ := newPauseFixture(t)
			id := f.e2bCreate(key)
			lease := time.Now().Add(time.Hour)
			actual := state
			uncertain := false
			if strings.HasPrefix(state, "expired-") {
				actual = strings.TrimPrefix(state, "expired-")
				lease = time.Now().Add(-time.Hour)
			}
			if state == "resuming-uncertain" {
				actual = "resuming"
				uncertain = true
			}
			if state == "" {
				lease = time.Time{}
			}
			seedHeldPause(t, f, pc, id, actual, lease, uncertain)
			before := heldStateDigest(t, f)
			cr, _ := pc.GetCR(context.Background(), "", "", "", "sb-pa", id)
			wire, _ := json.Marshal(cr)
			for _, path := range []string{"/resume", "/connect", "/pause", "/fork"} {
				code, body := f.e2b(key, http.MethodPost, "/sandboxes/"+id+path, `{"timeout":60}`)
				requireHeld(t, code, body)
			}
			token := sandbox.AccessOf(historicalSaved(t, f, id)).EnvdAccessToken
			code, body := f.wake(id, `{"port":49983,"envdAccessToken":"`+token+`"}`)
			requireHeld(t, code, body)
			f.h.AutoPauseSandboxes(context.Background())
			f.h.SyncSandboxBuilds(context.Background())
			if heldStateDigest(t, f) != before || captureCount(pc) != 0 {
				t.Fatal("held path changed durable state/captured")
			}
			after, _ := pc.GetCR(context.Background(), "", "", "", "sb-pa", id)
			afterWire, _ := json.Marshal(after)
			if string(wire) != string(afterWire) {
				t.Fatal("held path changed source/TTL")
			}
		})
	}
}
func TestSavedAdmissionWakeAuthorizationAndDiscardFences(t *testing.T) {
	for _, mode := range []string{"uncertain", "leased-stopping", "leased-resuming", "safe-paused"} {
		t.Run(mode, func(t *testing.T) {
			f, pc, key, keyB := newPauseFixture(t)
			id := f.e2bCreate(key)
			state := ""
			lease := time.Time{}
			if mode == "leased-stopping" {
				state = "stopping"
				lease = time.Now().Add(time.Hour)
			}
			if mode == "leased-resuming" {
				state = "resuming"
				lease = time.Now().Add(time.Hour)
			}
			seedHeldPause(t, f, pc, id, state, lease, mode == "uncertain")
			before := heldStateDigest(t, f)
			for _, path := range []string{"/resume", "/connect", "/fork", "/pause"} {
				c, _ := f.e2b(keyB, http.MethodPost, "/sandboxes/"+id+path, `{}`)
				if c != 404 {
					t.Fatal("foreign capability oracle", c)
				}
			}
			if c, _ := f.e2b("", http.MethodPost, "/sandboxes/"+id+"/resume", `{}`); c != 401 {
				t.Fatal("anonymous", c)
			}
			if c, _ := f.wake(id, `{"port":49983,"envdAccessToken":"wrong"}`); c != 403 {
				t.Fatal("wrongenvd", c)
			}
			req := httptest.NewRequest(http.MethodPost, "/internal/sandboxes/"+id+"/wake", strings.NewReader(`{"port":49983,"envdAccessToken":"wrong"}`))
			req.Header.Set("Authorization", "Bearer wrong-provider")
			w := httptest.NewRecorder()
			f.router.ServeHTTP(w, req)
			if w.Code != 401 {
				t.Fatal("wake location bearer", w.Code)
			}
			if c, _ := f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/resume", `{"timeout":0}`); c != 400 {
				t.Fatal("timeoutprecedence", c)
			}
			if heldStateDigest(t, f) != before {
				t.Fatal("auth/validation changed state")
			}
			c, body := f.do(sbxA, http.MethodGet, "/project/pa/sandbox-paused", "")
			if c != 200 || !strings.Contains(body, `"resumeAvailable":false`) || strings.Contains(body, "historical-secret") || strings.Contains(body, "saved") || strings.Contains(body, "envdAccessToken") {
				t.Fatal("unsafe list", c, body)
			}
			c, body = f.do(sbxA, http.MethodDelete, "/project/pa/sandbox-paused/"+id, "")
			if mode == "safe-paused" {
				if c != 204 {
					t.Fatal(c, body)
				}
				f.pausedState(id, "gone")
			} else {
				if c != 409 || heldStateDigest(t, f) != before {
					t.Fatal("discard bypassed uncertainty/lease", c, body)
				}
			}
		})
	}
}
func TestSavedAdmissionOrdinaryLiveAccessRemainsSupported(t *testing.T) {
	f, _, key, _ := newPauseFixture(t)
	id := f.e2bCreate(key)
	for _, call := range []struct {
		method, path, body string
		want               int
	}{{"GET", "/sandboxes/" + id, "", 200}, {"POST", "/sandboxes/" + id + "/connect", `{"timeout":60}`, 200}, {"POST", "/sandboxes/" + id + "/timeout", `{"timeout":600}`, 204}} {
		c, b := f.e2b(key, call.method, call.path, call.body)
		if c != call.want {
			t.Fatal(call.path, c, b)
		}
	}
	c, b := f.do(sbxA, http.MethodPost, "/project/pa/cloud", `{"type":"SANDBOX","data":{"template":"base","timeoutSeconds":60}}`)
	if c != 200 {
		t.Fatal("native ordinary create", c, b)
	}
	if c, b := f.e2b(key, http.MethodPost, "/sandboxes", `{"templateID":"base","autoPause":true}`); c != 403 {
		t.Fatal("new autopause promise", c, b)
	}
}
