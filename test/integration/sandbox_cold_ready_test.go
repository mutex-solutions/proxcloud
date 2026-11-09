//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// Cold reads may ignore their context just like a slow transport. The handler must
// check its own immutable lifetime after both readiness and the final access read.
type coldReadyCluster struct {
	*sbxCluster
	muCold     sync.Mutex
	mode       string
	gets       int
	id         string
	end        string
	cancel     context.CancelFunc
	deletes    int
	cleanupErr error
}

func (c *coldReadyCluster) GetCR(ctx context.Context, g, v, p, ns, name string) (map[string]any, error) {
	obj, err := c.sbxCluster.GetCR(ctx, g, v, p, ns, name)
	if obj == nil || err != nil || p != sandbox.CRPlural {
		return obj, err
	}
	c.muCold.Lock()
	defer c.muCold.Unlock()
	c.gets++
	c.id = name
	// Get1 is create's egress-policy lookup; Get2 is the readiness poll;
	// Get3 is AccessFor. Mutations stay in this serial fake API call.
	if c.gets == 1 {
		spec := obj["spec"].(map[string]any)
		if c.mode == "expired-ready" {
			c.end = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			spec["shutdownTime"] = c.end
			c.sbxCluster.mu.Lock()
			c.sbxCluster.crs[ns+"/"+name]["spec"].(map[string]any)["shutdownTime"] = c.end
			c.sbxCluster.mu.Unlock()
		}
		if c.mode == "expired-ready" {
			obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": time.Now().UTC().Format(time.RFC3339)}}}
		}
	}
	if c.gets == 2 && c.mode == "cancel-ready" {
		c.cancel()
	}
	if c.gets == 3 && c.mode == "cancel-access" {
		c.cancel()
	}
	return obj, nil
}

func (c *coldReadyCluster) DeleteCR(ctx context.Context, g, v, p, ns, name string) error {
	if p == sandbox.CRPlural {
		c.muCold.Lock()
		c.deletes++
		c.cleanupErr = ctx.Err()
		c.muCold.Unlock()
	}
	return c.sbxCluster.DeleteCR(ctx, g, v, p, ns, name)
}

func TestE2BCustomColdReadyCleanup(t *testing.T) {
	for _, mode := range []string{"cancel-ready", "cancel-access", "expired-ready"} {
		t.Run(mode, func(t *testing.T) {
			f, _, key := newTemplateFixture(t)
			// Runtime fingerprints must see the same provider startup budget as
			// the low-level service, not an out-of-band fake-only override.
			if _, err := f.db.C("externalService").SetByID(context.Background(), "sbx", pgdoc.M{"config.sandbox.startup": pgdoc.M{"totalSeconds": 1, "schedulingSeconds": 1, "pullSeconds": 1, "runtimeSeconds": 1}}, nil); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cluster := &coldReadyCluster{sbxCluster: f.cluster, mode: mode, cancel: cancel}
			f.h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
				cfg := es.SandboxConfig()
				cfg.Startup = sandbox.StartupConfig{TotalSeconds: 1, SchedulingSeconds: 1, PullSeconds: 1, RuntimeSeconds: 1}
				return sandbox.NewWithAPI(cluster, cfg, es.ID), nil
			})
			mustInsert(t, f.db, "sandbox_templates", pgdoc.M{
				"_id": "cold-build", "projectId": "pa", "name": "cold-build", "cpu": 1, "memoryGiB": 1,
				"builds": []any{pgdoc.M{"buildID": "b", "status": "ready", "image": "192.0.2.226/t/pa/0123456789abcdef0123@sha256:" + strings.Repeat("a", 64)}},
			})
			req := httptest.NewRequest(http.MethodPost, "/e2b/sandboxes", strings.NewReader(`{"templateID":"cold-build","timeout":300}`)).WithContext(ctx)
			req.Header.Set("X-API-Key", key)
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable || cluster.deletes != 1 || cluster.cleanupErr != nil || cluster.count() != 0 {
				t.Fatalf("code=%d deletes=%d cleanupCtx=%v live=%d body=%s", rec.Code, cluster.deletes, cluster.cleanupErr, cluster.count(), rec.Body.String())
			}
			row, err := f.rows.FindByServiceIDAndExternalID(context.Background(), "sbx", cluster.id)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "expired-ready" {
				if row != nil && row.Data["runningSince"] != nil && row.Data["runningSince"] != "" {
					t.Fatalf("expired provisional startup must not become billable: %+v", row)
				}
			}
			if ctx.Err() != nil && !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestE2BLegacyShortLifetimeUnchanged(t *testing.T) {
	for _, kind := range []string{"builtin", "snapshot", "future"} {
		t.Run(kind, func(t *testing.T) {
			f, _, key := newTemplateFixture(t)
			name := "base"
			if kind != "builtin" {
				name = kind
				mustInsert(t, f.db, "sandbox_templates", pgdoc.M{
					"_id": name, "projectId": "pa", "name": name, "kind": kind, "cpu": 1, "memoryGiB": 1,
					"builds": []any{pgdoc.M{"buildID": "b", "status": "ready", "image": "192.0.2.226/t/pa/0123456789abcdef0123@sha256:" + strings.Repeat("a", 64)}},
				})
			}
			code, body := f.e2b(key, http.MethodPost, "/sandboxes", `{"templateID":"`+name+`","timeout":1}`)
			if code != http.StatusCreated {
				t.Fatalf("legacy auto-ready short lifetime must still succeed: %d %s", code, body)
			}
		})
	}
}
