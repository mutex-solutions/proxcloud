package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBuilderOneGiBDiskRequestsNeverExceedLimits(t *testing.T) {
	result, _ := RenderDockerfile("python:3.12", nil)
	for _, cfg := range []Config{{}, {Build: BuildConfig{MaxDiskGiB: 1}}} {
		_, pod := BuildObjects(cfg, BuildRequest{Result: result, DiskGiB: 1})
		for _, key := range []string{"initContainers", "containers"} {
			for _, raw := range dig(pod, "spec", key).([]any) {
				c := raw.(map[string]any)
				if digStr(c, "resources", "requests", "ephemeral-storage") != "1Gi" || digStr(c, "resources", "limits", "ephemeral-storage") != "1Gi" {
					t.Fatalf("invalid disk1 resources %s: %v", key, c)
				}
			}
		}
	}
}

func TestWarmingServiceNamesAndNodeFanoutAreBounded(t *testing.T) {
	f := &warmFake{fakeAPI: newFake()}
	cfg := warmTestConfig(f)
	cfg.Warming = WarmConfig{Enabled: true, MaxNodes: 1, MaxParallel: 1}
	f.nodes = []map[string]any{warmNode("n1", ""), warmNode("n2", "")}
	a := NewWithAPI(f, cfg, "service-a")
	b := NewWithAPI(f, cfg, "service-b")
	image := warmRegistry + "/" + warmProject + "/" + importTemplateID + "@" + importDigest
	for _, s := range []*Service{a, b} {
		if _, err := s.PrepareImage(context.Background(), warmProject, Template{Image: image}, 0); err != nil {
			t.Fatal(err)
		}
	}
	pods, _ := f.ListPods(context.Background(), PrepullNamespace, "")
	if len(pods) != 2 || digStr(pods[0], "metadata", "name") == digStr(pods[1], "metadata", "name") {
		t.Fatal("services shared worker identity", pods)
	}
	c, err := a.ImageWarmCoverage(context.Background(), warmProject, Template{Image: image})
	if err != nil || c.Total != 1 {
		t.Fatal("maxNodes not reflected by coverage", c, err)
	}
	now := time.Now().UTC().Add(48 * time.Hour)
	a.now = func() time.Time { return now }
	if err := a.ReconcileWarming(context.Background()); err != nil {
		t.Fatal(err)
	}
	pods, _ = f.ListPods(context.Background(), PrepullNamespace, "")
	if len(pods) != 1 || digStr(pods[0], "metadata", "labels", LabelService) != "service-b" {
		t.Fatal("cross-service cleanup", pods)
	}
}

type phaseDeadlineAPI struct {
	*fakeAPI
	remaining time.Duration
}

func (f *phaseDeadlineAPI) GetCR(ctx context.Context, g, v, p, ns, name string) (map[string]any, error) {
	if d, ok := ctx.Deadline(); ok {
		f.remaining = time.Until(d)
	}
	return f.fakeAPI.GetCR(ctx, g, v, p, ns, name)
}
func TestWaitReadyReadContextUsesCurrentPhaseBudget(t *testing.T) {
	f := &phaseDeadlineAPI{fakeAPI: newFake()}
	cfg := testConfig()
	cfg.Startup = StartupConfig{TotalSeconds: 900, SchedulingSeconds: 2}
	s := NewWithAPI(f, cfg, "svc")
	sp := Spec{ProjectID: "p1", Template: "python"}
	_, _ = s.CreateSandbox(context.Background(), &sp)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.WaitReady(ctx, "p1", sp.ID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	cr := f.crs["sb-p1/"+sp.ID]
	setReadyAt(cr, time.Now().UTC().Add(-time.Second))
	if _, err := s.WaitReady(context.Background(), "p1", sp.ID); err != nil {
		t.Fatal(err)
	}
	if f.remaining <= 0 || f.remaining > 2*time.Second {
		t.Fatal("GET inherited total instead of phase cap", f.remaining)
	}
}

func TestWarmingReconcilesLoweredLimitsSelectorAndDisable(t *testing.T) {
	f := &warmFake{fakeAPI: newFake()}
	cfg := warmTestConfig(f)
	cfg.Warming = WarmConfig{Enabled: true, MaxImages: 3, MaxImageGiB: 15, MaxTotalGiB: 45, MaxParallel: 3}
	f.nodes = []map[string]any{warmNode("n1", "")}
	s := NewWithAPI(f, cfg, "current-policy")
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for _, letter := range []string{"a", "b", "c"} {
		image := warmRegistry + "/" + warmProject + "/" + importTemplateID + "@sha256:" + strings.Repeat(letter, 64)
		if _, err := s.PrepareImage(context.Background(), warmProject, Template{Image: image}, 0); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	es, _ := s.warmEntries(context.Background())
	if len(es) != 3 {
		t.Fatal("admission fixture", es)
	}
	s.cfg.Warming.MaxImages = 1
	s.cfg.Warming.MaxTotalGiB = 15
	if err := s.ReconcileWarming(context.Background()); err != nil {
		t.Fatal(err)
	}
	es, _ = s.warmEntries(context.Background())
	pods, _ := f.ListPods(context.Background(), PrepullNamespace, "")
	if len(es) != 1 || len(pods) != 1 || !strings.HasSuffix(es[0].Image, strings.Repeat("c", 64)) {
		t.Fatal("lowered limits not enforced", es, pods)
	}
	// Removing node eligibility retires only owned workers, not another service/runtime pod.
	foreign := s.warmPod("foreign", "n1", es[0])
	foreign["metadata"].(map[string]any)["labels"].(map[string]any)[LabelService] = "other-service"
	_ = f.ApplyCore(context.Background(), "pods", foreign)
	s.cfg.Warming.NodeSelector = map[string]string{"operator-pool": "other"}
	if err := s.ReconcileWarming(context.Background()); err != nil {
		t.Fatal(err)
	}
	pods, _ = f.ListPods(context.Background(), PrepullNamespace, "")
	if len(pods) != 1 || digStr(pods[0], "metadata", "name") != "foreign" {
		t.Fatal("selector cleanup escaped ownership", pods)
	}
	s.cfg.Warming.Enabled = false
	if err := s.ReconcileWarming(context.Background()); err != nil {
		t.Fatal(err)
	}
	es, _ = s.warmEntries(context.Background())
	if len(es) != 0 {
		t.Fatal("disable retained intents", es)
	}
}

func TestWarmingQueuedAdmissionHonorsCancellation(t *testing.T) {
	f := &warmFake{fakeAPI: newFake()}
	cfg := warmTestConfig(f)
	cfg.Warming.Enabled = true
	s := NewWithAPI(f, cfg, "queued-cancel")
	release, err := s.acquireWarm(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := time.Now()
	if _, err := s.PrepareImage(ctx, warmProject, Template{Image: warmRegistry + "/" + warmProject + "/" + importTemplateID + "@" + importDigest}, 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.ReconcileWarming(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if time.Since(before) > time.Second || len(f.core) != 0 {
		t.Fatal("queued request ignored cancellation or wrote state")
	}
}
