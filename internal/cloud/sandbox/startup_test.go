package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStartupBudgetAndReadyTTL(t *testing.T) {
	cfg := testConfig()
	cfg.Startup = StartupConfig{TotalSeconds: 600}
	cfg.Envd = true
	f := newFake()
	s := NewWithAPI(f, cfg, "svc")
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	sp := Spec{ProjectID: "p1", Template: "python", TimeoutSeconds: 1, AutoPause: true}
	d, err := s.CreateSandbox(context.Background(), &sp)
	if err != nil {
		t.Fatal(err)
	}
	if d["status"] != StatusPending || d["runningSince"] != nil {
		t.Fatal("pending must never be billed", d)
	}
	cr := f.crs["sb-p1/"+sp.ID]
	if got := digStr(cr, "metadata", "annotations", AnnotationEndAt); got != now.Add(601*time.Second).Format(time.RFC3339) {
		t.Fatal("TTL can expire during pull", got)
	}
	now = now.Add(5 * time.Minute)
	if due, _ := s.DueForAutoPause(context.Background()); len(due) != 0 {
		t.Fatal("pending cannot autopause")
	}
	cr["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": now.Format(time.RFC3339)}}}
	d, err = s.GetSandbox(context.Background(), "p1", sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d["shutdownTime"] != now.Add(time.Second).Format(time.RFC3339) || d["runningSince"] != now.Format(time.RFC3339) {
		t.Fatal("ready must start TTL and billing", d)
	}
	old := d["runningSince"]
	cr["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": now.Add(time.Minute).Format(time.RFC3339)}}}
	d, err = s.GetSandbox(context.Background(), "p1", sp.ID)
	if err != nil || d["runningSince"] != old {
		t.Fatal("restart moved billing", err, d)
	}
}
func TestStartupProgressReasons(t *testing.T) {
	for _, tc := range []struct {
		reason, phase string
		terminal      bool
	}{{"ImagePullBackOff", "image-pull", false}, {"InvalidImageName", "image-pull", true}, {"CreateContainerConfigError", "runtime", true}} {
		pod := map[string]any{"spec": map[string]any{"nodeName": "secret-node"}, "status": map[string]any{"containerStatuses": []any{map[string]any{"state": map[string]any{"waiting": map[string]any{"reason": tc.reason, "message": "secret-password"}}}}}}
		p := startupProgress(pod)
		if p.Phase != tc.phase || p.Terminal != tc.terminal || strings.Contains(p.Reason, "secret") {
			t.Fatal(p)
		}
	}
}
func TestWaitReadyCancellationAndTerminal(t *testing.T) {
	f := newFake()
	s := newTestService(f)
	sp := Spec{ProjectID: "p1", Template: "python"}
	_, err := s.CreateSandbox(context.Background(), &sp)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.WaitReady(ctx, "p1", sp.ID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.core["pods/sb-p1/"+sp.ID] = map[string]any{"spec": map[string]any{"nodeName": "n1"}, "status": map[string]any{"phase": "Failed"}}
	if _, err = s.WaitReady(context.Background(), "p1", sp.ID); err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Fatal(err)
	}
	cfg := Config{Startup: StartupConfig{TotalSeconds: 999999, PullSeconds: -1}}.WithDefaults()
	if cfg.Startup.TotalSeconds != MaxStartupSeconds || cfg.Startup.PullSeconds != 1 {
		t.Fatal(cfg.Startup)
	}
}

type startupUncertainAPI struct{ *fakeAPI }

func (f *startupUncertainAPI) ApplyCR(ctx context.Context, g, v, p string, obj map[string]any) error {
	if p == CRPlural {
		_ = f.fakeAPI.ApplyCR(ctx, g, v, p, obj)
		return context.Canceled
	}
	return f.fakeAPI.ApplyCR(ctx, g, v, p, obj)
}
func TestCreateUncertainApplyCleansMintedID(t *testing.T) {
	f := &startupUncertainAPI{newFake()}
	s := NewWithAPI(f, testConfig(), "svc")
	sp := Spec{ProjectID: "p1", Template: "python"}
	if _, err := s.CreateSandbox(context.Background(), &sp); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.crs["sb-p1/"+sp.ID] != nil {
		t.Fatal("uncertain create stranded pending sandbox")
	}
}

func TestSavedStartupArmsNewTTL(t *testing.T) {
	cfg := testConfig().WithDefaults()
	cr := BuildCR(cfg, "svc", Spec{ID: "sbx-00000001", ProjectID: "p1", Template: "python", CPU: 1, MemoryGiB: 2}, time.Unix(1, 0))
	cr["metadata"].(map[string]any)["annotations"].(map[string]any)[AnnotationRunningSince] = "old"
	clone, _ := CloneSaved(cr)
	ArmSavedStartup(clone, cfg, 15, time.Unix(100, 0))
	if digStr(clone, "metadata", "annotations", AnnotationRunningSince) != "" || digStr(clone, "metadata", "annotations", AnnotationStartupPending) != "true" {
		t.Fatal(clone)
	}
	if got := digStr(clone, "spec", "shutdownTime"); got != time.Unix(1015, 0).UTC().Format(time.RFC3339) {
		t.Fatal(got)
	}
}
