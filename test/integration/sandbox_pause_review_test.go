//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

type killDuringCapture struct {
	*pauseCluster
	kill func()
}

func (c killDuringCapture) ExecStream(ctx context.Context, a, b, d string, cmd []string, w io.Writer, n int) (int, string, error) {
	return c.pauseCluster.ExecStream(ctx, a, b, d, cmd, w, n)
}

// ApplyCore of the assembly pod: the capture is stored, the paused record not written yet.
func (c killDuringCapture) ApplyCore(ctx context.Context, plural string, obj map[string]any) error {
	if plural == "pods" {
		c.kill()
	}
	return c.pauseCluster.ApplyCore(ctx, plural, obj)
}

// A kill that lands while the pause streams its capture (no paused record yet) answers 204 and deletes the sandbox;
// the pause must not then bring it back as paused (a snapshot of a killed sandbox, billed as storage).
func TestReviewKillDuringCaptureNotResurrected(t *testing.T) {
	f, pc, keyA, _ := newPauseFixture(t)
	ctx := context.Background()
	// A long timeout (past the pause's hold), so the pause never patches the sandbox's end.
	code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base","timeout":3600}`)
	var sb struct{ SandboxID string }
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &sb) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	id := sb.SandboxID
	killed := 0
	f.h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
		return sandbox.NewWithAPI(killDuringCapture{pc, func() {
			if killed == 0 {
				killed, _ = f.e2b(keyA, http.MethodDelete, "/sandboxes/"+id, "")
			}
		}}, es.SandboxConfig(), es.ID), nil
	})
	code, body = f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`)
	if killed != http.StatusNoContent || f.cluster.count() != 0 {
		t.Fatalf("kill during capture: %d, running %d", killed, f.cluster.count())
	}
	t.Logf("pause answered %d %s", code, body)
	time.Sleep(3 * time.Second)
	n, _ := f.db.C("sandbox_paused").Count(ctx, pgdoc.M{})
	tpls, _ := f.db.C("sandbox_templates").Count(ctx, pgdoc.M{"projectId": "pa"})
	if n != 0 || tpls != 0 {
		t.Fatalf("killed sandbox came back: pause answered %d %s; paused records %d, snapshots %d", code, body, n, tpls)
	}
}

// A pause left behind by a crashed portal (lease expired) whose sandbox still runs on the pause's hold: a connect
// drops the pause and its snapshot, and must give the sandbox a live end, or the cluster removes it at the dead hold.
func TestReviewStalePauseKeepsSandboxOrSnapshot(t *testing.T) {
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
	// the crashed pause's hold, which equals its lease
	if _, err := pc.MergePatchCR(ctx, "", "", "", sandbox.NamespaceFor("pa"), id, map[string]any{"spec": map[string]any{"shutdownTime": past}}); err != nil {
		t.Fatal(err)
	}
	code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":60}`)
	tpls, _ := f.db.C("sandbox_templates").Count(ctx, pgdoc.M{"projectId": "pa"})
	cr, _ := pc.GetCR(ctx, "", "", "", sandbox.NamespaceFor("pa"), id)
	spec, _ := cr["spec"].(map[string]any)
	end, _ := time.Parse(time.RFC3339, fmt.Sprint(spec["shutdownTime"]))
	if tpls == 0 && time.Until(end) <= 0 {
		t.Fatalf("connect %d %s: snapshot dropped and the sandbox still ends at the dead hold (%v)", code, body, end)
	}
}
