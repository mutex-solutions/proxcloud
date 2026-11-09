//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// Named capture/export remains a real executed positive path. It is not a
// successful managed pause/resume test and does not bypass production admission.
func TestNamedSnapshotCapturePinnedCreateAndTenantIsolationRemain(t *testing.T) {
	f, pc, key, keyB := newPauseFixture(t)
	id := f.e2bCreate(key)
	if c, b := f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/snapshots", `{"name":"keep1"}`); c != 201 {
		t.Fatal(c, b)
	}
	if c, b := f.e2b(key, http.MethodGet, "/snapshots", ""); c != 200 || !contains(b, "keep1") {
		t.Fatal(c, b)
	}
	if c, _ := f.e2b(keyB, http.MethodPost, "/sandboxes", `{"templateID":"keep1"}`); c == 201 {
		t.Fatal("foreign snapshot started")
	}
	if c, b := f.e2b(key, http.MethodPost, "/sandboxes", `{"templateID":"keep1","timeout":60}`); c != 201 {
		t.Fatal("pinned snapshot CREATE", c, b)
	}
	if captureCount(pc) != 1 || f.cluster.count() != 2 {
		t.Fatal("capture/create positive not exercised")
	}
}
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
func TestNamedSnapshotTruncatedCaptureAndFailedAssemblyKeepSource(t *testing.T) {
	for _, mode := range []string{"cut", "failed-assembly"} {
		t.Run(mode, func(t *testing.T) {
			f, pc, key, _ := newPauseFixture(t)
			ctx := context.Background()
			id := f.e2bCreate(key)
			if mode == "cut" {
				pc.cut = true
			} else {
				f.h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
					return sandbox.NewWithAPI(failingAssembly{pc}, es.SandboxConfig(), es.ID), nil
				})
			}
			c, b := f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/snapshots", `{"name":"broken"}`)
			if mode == "cut" {
				if c != http.StatusInternalServerError || !contains(b, "internal error") {
					t.Fatal("truncated gzip must return the fixed capture failure", c, b)
				}
				if n, _ := f.db.C("sandbox_templates").Count(ctx, pgdoc.M{}); n != 0 {
					t.Fatal("cut capture retained partial template")
				}
			} else {
				if c != 201 {
					t.Fatal(c, b)
				}
				var doc struct{ Builds []struct{ Status string } }
				deadline := time.Now().Add(5 * time.Second)
				for {
					f.h.SyncSandboxBuilds(ctx)
					ok, e := f.db.C("sandbox_templates").FindOne(ctx, pgdoc.M{"name": "broken"}, &doc)
					if e != nil || !ok {
						t.Fatal(e)
					}
					if len(doc.Builds) > 0 && doc.Builds[0].Status == sandbox.BuildError {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("failed assembly not recorded")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if f.cluster.count() != 1 || len(f.snapshotRows("pa")) != 0 {
				t.Fatal("bad named capture/assembly deleted source or billed failed output")
			}
		})
	}
}
