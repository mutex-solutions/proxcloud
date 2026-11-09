//go:build integration

package integration

import (
	"context"
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
func TestReviewHeldPauseDoesNotCaptureAndExplicitKillDoesNotResurrect(t *testing.T) {
	f, pc, key, _ := newPauseFixture(t)
	ctx := context.Background()
	id := f.e2bCreate(key)
	killed := 0
	f.h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
		return sandbox.NewWithAPI(killDuringCapture{pc, func() { killed, _ = f.e2b(key, http.MethodDelete, "/sandboxes/"+id, "") }}, es.SandboxConfig(), es.ID), nil
	})
	code, body := f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/pause", `{}`)
	requireHeld(t, code, body)
	if killed != 0 || captureCount(pc) != 0 || f.cluster.count() != 1 {
		t.Fatal("held pause invoked capture callback")
	}
	if code, body = f.e2b(key, http.MethodDelete, "/sandboxes/"+id, ""); code != 204 {
		t.Fatal(code, body)
	}
	f.h.AutoPauseSandboxes(ctx)
	n, _ := f.db.C("sandbox_paused").Count(ctx, pgdoc.M{})
	tpls, _ := f.db.C("sandbox_templates").Count(ctx, pgdoc.M{})
	if n != 0 || tpls != 0 || f.cluster.count() != 0 {
		t.Fatal("explicit kill resurrected state")
	}
}

// Retaining state does not extend an already-expired source or promise it lives
// indefinitely. Capability refusal must keep the copy and leave its TTL untouched.
func TestReviewHeldStalePauseKeepsOnlyCopyWithoutRearmingTTL(t *testing.T) {
	f, pc, key, _ := newPauseFixture(t)
	ctx := context.Background()
	id := f.e2bCreate(key)
	seedHeldPause(t, f, pc, id, "pausing", time.Now().Add(-time.Hour), false)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if _, e := pc.MergePatchCR(ctx, "", "", "", "sb-pa", id, map[string]any{"spec": map[string]any{"shutdownTime": past}}); e != nil {
		t.Fatal(e)
	}
	before := heldStateDigest(t, f)
	cr, _ := pc.GetCR(ctx, "", "", "", "sb-pa", id)
	wire := fmtJSON(cr)
	code, body := f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":60}`)
	requireHeld(t, code, body)
	after, _ := pc.GetCR(ctx, "", "", "", "sb-pa", id)
	if heldStateDigest(t, f) != before || fmtJSON(after) != wire {
		t.Fatal("held stale pause removed copy or rearmedTTL")
	}
	if e := pc.DeleteCR(ctx, "", "", "", "sb-pa", id); e != nil {
		t.Fatal(e)
	}
	code, body = f.e2b(key, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":60}`)
	requireHeld(t, code, body)
	if heldStateDigest(t, f) != before || f.cluster.count() != 0 {
		t.Fatal("absent source adopted or copy lost")
	}
}
