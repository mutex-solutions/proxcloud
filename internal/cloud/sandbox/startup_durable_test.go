package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

type readyCASAPI struct {
	*fakeAPI
	failFinal bool
	afterRead func()
}

func cloneCR(obj map[string]any) map[string]any {
	b, _ := json.Marshal(obj)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}
func (f *readyCASAPI) GetCR(ctx context.Context, g, v, p, ns, name string) (map[string]any, error) {
	obj, err := f.fakeAPI.GetCR(ctx, g, v, p, ns, name)
	out := cloneCR(obj)
	if f.afterRead != nil {
		f.afterRead()
	}
	return out, err
}
func (f *readyCASAPI) MergePatchCR(ctx context.Context, g, v, p, ns, name string, patch map[string]any) (map[string]any, error) {
	obj := f.crs[ns+"/"+name]
	if digStr(patch, "metadata", "uid") != digStr(obj, "metadata", "uid") || digStr(patch, "metadata", "resourceVersion") != digStr(obj, "metadata", "resourceVersion") {
		return nil, errors.New("conflict")
	}
	if f.failFinal && digStr(patch, "metadata", "annotations", AnnotationRunningSince) != "" {
		return nil, errors.New("temporary lifetime patch failure")
	}
	_, err := f.fakeAPI.MergePatchCR(ctx, g, v, p, ns, name, patch)
	obj["metadata"].(map[string]any)["resourceVersion"] = digStr(obj, "metadata", "resourceVersion") + "1"
	return cloneCR(obj), err
}
func setReadyAt(obj map[string]any, at time.Time) {
	obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": at.UTC().Format(time.RFC3339)}}}
}

func TestReadyCandidateSurvivesFinalPatchFailureAndRestartFlap(t *testing.T) {
	f := &readyCASAPI{fakeAPI: newFake(), failFinal: true}
	s := NewWithAPI(f, testConfig(), "svc")
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	sp := Spec{ProjectID: "p1", Template: "python", TimeoutSeconds: 60}
	if _, err := s.CreateSandbox(context.Background(), &sp); err != nil {
		t.Fatal(err)
	}
	cr := f.crs["sb-p1/"+sp.ID]
	first := now.Add(5 * time.Second)
	now = first
	setReadyAt(cr, first)
	if _, err := s.GetSandbox(context.Background(), "p1", sp.ID); err == nil {
		t.Fatal("uncommitted lifetime returned ready")
	}
	if got := digStr(cr, "metadata", "annotations", AnnotationReadyCandidate); got != first.Format(time.RFC3339) {
		t.Fatal("candidate not durable", got)
	}
	if d := sandboxData(cr); d["status"] != StatusPending || d["runningSince"] != nil || d["ready"] != false {
		t.Fatal("candidate prematurely billed/usable", d)
	}
	now = now.Add(10 * time.Second)
	setReadyAt(cr, now)
	f.failFinal = false
	d, err := s.GetSandbox(context.Background(), "p1", sp.ID)
	if err != nil || d["runningSince"] != first.Format(time.RFC3339) || d["shutdownTime"] != first.Add(time.Minute).Format(time.RFC3339) {
		t.Fatal("flap moved candidate origin", d, err)
	}
	// Subsequent Ready flaps cannot move committed billing or TTL.
	now = now.Add(time.Second)
	setReadyAt(cr, now)
	d, err = s.GetSandbox(context.Background(), "p1", sp.ID)
	if err != nil || d["runningSince"] != first.Format(time.RFC3339) {
		t.Fatal(d, err)
	}
}

func TestReadyCandidateRefusesStaleUIDAndExpiredProvisional(t *testing.T) {
	f := &readyCASAPI{fakeAPI: newFake(), failFinal: true}
	s := NewWithAPI(f, testConfig(), "svc")
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	sp := Spec{ProjectID: "p1", Template: "python", TimeoutSeconds: 60}
	_, _ = s.CreateSandbox(context.Background(), &sp)
	cr := f.crs["sb-p1/"+sp.ID]
	setReadyAt(cr, now)
	old := cloneCR(cr)
	if _, err := s.GetSandbox(context.Background(), "p1", sp.ID); err == nil {
		t.Fatal("final should fail")
	}
	cr["metadata"].(map[string]any)["uid"] = "replacement"
	if _, err := s.GetSandbox(context.Background(), "p1", sp.ID); err == nil {
		t.Fatal("replacement adopted old UID candidate")
	}
	if err := s.pinRunningSince(context.Background(), old); err == nil {
		t.Fatal("stale candidate CAS accepted")
	}
	cr["metadata"].(map[string]any)["uid"] = digStr(cr, "metadata", "annotations", AnnotationReadyCandidateUID)
	original := digStr(cr, "spec", "shutdownTime")
	now = now.Add(2 * time.Hour)
	f.failFinal = false
	if _, err := s.GetSandbox(context.Background(), "p1", sp.ID); err == nil {
		t.Fatal("expired provisional was resurrected")
	}
	if digStr(cr, "spec", "shutdownTime") != original {
		t.Fatal("expired lifetime extended")
	}
}

func TestWaitReadyRejectsLateGETCancellationPhaseAndRuntimeExpiry(t *testing.T) {
	for _, kind := range []string{"context", "phase", "runtime"} {
		t.Run(kind, func(t *testing.T) {
			f := &readyCASAPI{fakeAPI: newFake()}
			cfg := testConfig()
			cfg.Startup.SchedulingSeconds = 1
			s := NewWithAPI(f, cfg, "svc")
			now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
			s.now = func() time.Time { return now }
			sp := Spec{ProjectID: "p1", Template: "python", TimeoutSeconds: 60}
			_, _ = s.CreateSandbox(context.Background(), &sp)
			cr := f.crs["sb-p1/"+sp.ID]
			setReadyAt(cr, now)
			if _, err := s.GetSandbox(context.Background(), "p1", sp.ID); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.afterRead = func() {
				switch kind {
				case "context":
					cancel()
				case "phase":
					now = now.Add(2 * time.Second)
				case "runtime":
					now = now.Add(61 * time.Second)
				}
			}
			if d, err := s.WaitReady(ctx, "p1", sp.ID); err == nil || d != nil {
				t.Fatal("late readiness accepted", d, err)
			}
		})
	}
}

func TestReadyClockPendingVanishNeverCreatesBillingResource(t *testing.T) {
	cfg := testConfig()
	cr := BuildCR(cfg, "svc", Spec{ID: "sbx-00000001", ProjectID: "p1", Template: "python", CPU: 1, MemoryGiB: 2}, time.Now().Add(time.Hour))
	armStartup(cr, cfg, 60, time.Now())
	row := &cloud.CloudResource{ID: "r", Data: sandboxData(cr)}
	MarkVanished(row, time.Now().Add(time.Minute))
	if row.Data["runningSince"] != nil {
		t.Fatal("pending vanished assumed start", row.Data)
	}
	got, err := billingresource.NewSandboxProvider().GetBillingInformation(context.Background(), billingapi.BillingContext{}, row)
	if err != nil || len(got) != 0 {
		t.Fatal("pending billed", got, err)
	}
	legacy := &cloud.CloudResource{Data: map[string]any{"status": StatusPending, "startedAt": "2026-11-09T00:00:00Z"}}
	MarkVanished(legacy, time.Date(2026, 11, 9, 0, 1, 0, 0, time.UTC))
	if legacy.Data["runningSince"] != "2026-11-09T00:00:00Z" {
		t.Fatal("legacy billing behavior changed")
	}
}

func TestArmedSavedResumePreservesProvisionalShutdown(t *testing.T) {
	f := newFake()
	s := NewWithAPI(f, testConfig(), "svc")
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	saved := BuildCR(s.Config(), "svc", Spec{ID: "sbx-00000001", ProjectID: "p1", Template: "python", CPU: 1, MemoryGiB: 2}, now)
	ArmSavedStartup(saved, s.Config(), 60, now)
	armed := digStr(saved, "spec", "shutdownTime")
	if err := s.ResumeSandbox(context.Background(), "p1", saved, "python:3.12", now.Add(time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if digStr(f.crs["sb-p1/sbx-00000001"], "spec", "shutdownTime") != armed {
		t.Fatal("resume overwrote provisional startup lifetime")
	}
}
