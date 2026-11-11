package project

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

func TestSavedLifecycleDriversHoldBeforeAnyStateOrClusterAccess(t *testing.T) {
	cfg := sandbox.Config{TemplateRegistry: "registry.example/t", Templates: map[string]sandbox.Template{"external": {Image: "external.example/library/python:3.12", CPU: 1, MemoryGiB: 1}}}
	c := &e2bCaller{proj: &Project{ID: "p1", Status: StatusEnabled}, es: &externalservice.ExternalService{ID: "svc"}, ss: sandbox.NewWithAPI(nil, cfg, "svc")}
	h := &Handler{sandboxGates: func(context.Context, *externalservice.ExternalService) []sandbox.Gate {
		return []sandbox.Gate{{Name: "fixture", OK: true}}
	}}
	// Stores and cluster are deliberately nil, not to bypass admission but to prove
	// that every existing unfinished driver returns before touching either.
	d := &pauseDrive{id: "sbx-0123456789abcdef", templateID: "0123456789abcdef0123", lease: time.Now().Add(-time.Hour)}
	before := d.lease
	h.finishPause(c, d)
	h.redrivePauses(context.Background(), c.es, c.ss)
	h.abandonPause(context.Background(), c, d, errors.New("old driver error"))
	if h.cancelPause(context.Background(), c, d.id) {
		t.Fatal("held cancel removed state")
	}
	if e := h.stopPausedSource(context.Background(), c, d); !errors.Is(e, sandbox.ErrManagedSavedStartHeld) {
		t.Fatal(e)
	}
	if e := h.driveStep(context.Background(), c, d, func(*pausedSandbox) bool { t.Fatal("held drive callback"); return false }); !errors.Is(e, sandbox.ErrManagedSavedStartHeld) {
		t.Fatal(e)
	}
	if !d.lease.Equal(before) {
		t.Fatal("held driver renewed lease")
	}
	p := &pausedSandbox{ID: d.id, ProjectID: "p1", ServiceID: "svc", State: pausedResuming, ResumeUncertain: true, LeaseUntil: &before, Saved: map[string]any{"PRIVATE_ENV": "PRIVATE_VALUE"}}
	raw, _ := json.Marshal(p)
	if _, e := h.resumePaused(context.Background(), c, p, 300); !errors.Is(e, sandbox.ErrManagedSavedStartHeld) {
		t.Fatal(e)
	}
	after, _ := json.Marshal(p)
	if string(raw) != string(after) {
		t.Fatal("held resume mutated paused state")
	}
	if status, msg, ok := sandboxHTTPError(sandbox.ErrManagedSavedStartHeld); !ok || status != http.StatusForbidden || msg != sandbox.ErrManagedSavedStartHeld.Error() {
		t.Fatal("held mapping", status, msg)
	}
	if status, msg := e2bError(sandbox.ErrManagedSavedStartHeld); status != http.StatusForbidden || msg != sandbox.ErrManagedSavedStartHeld.Error() {
		t.Fatal("E2B mapping")
	}
}
func TestSavedStartRefusesBeforeArmingOrChangingSavedTokens(t *testing.T) {
	cfg := sandbox.Config{TemplateRegistry: "registry.example/t", Templates: map[string]sandbox.Template{"base": {Image: "external.example/library/python:3.12"}}}
	c := &e2bCaller{proj: &Project{ID: "p1"}, es: &externalservice.ExternalService{ID: "svc"}, ss: sandbox.NewWithAPI(nil, cfg, "svc")}
	saved := map[string]any{"metadata": map[string]any{"name": "sbx-0123456789abcdef", "namespace": "sb-p1", "labels": map[string]any{sandbox.LabelProject: "p1"}}, "spec": map[string]any{"PRIVATE_ENV": "PRIVATE_VALUE", "shutdownTime": "2099-01-01T00:00:00Z"}}
	before, _ := json.Marshal(saved)
	h := &Handler{}
	if _, _, e := h.startSavedReady(context.Background(), c, saved, "registry.example/t/p1/0123456789abcdef0123:tag", 300); !errors.Is(e, sandbox.ErrManagedSavedStartHeld) {
		t.Fatal(e)
	}
	after, _ := json.Marshal(saved)
	if string(before) != string(after) {
		t.Fatal("held start armed/changed saved map")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, e := h.startSavedReady(ctx, c, saved, "ignored", 300); !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation precedence", e)
	}
	if _, _, e := h.startSavedReady(context.Background(), c, saved, "ignored", 0); e == nil {
		t.Fatal("invalid timeout must be rejected")
	}
}
