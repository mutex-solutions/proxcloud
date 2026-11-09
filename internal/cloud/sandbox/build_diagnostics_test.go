package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type diagnosticAPI struct {
	*fakeAPI
	text  map[string]string
	fail  map[string]bool
	calls []string
}

func (f *diagnosticAPI) PodLogPrefix(_ context.Context, _, _, c string, cap int) (string, bool, error) {
	f.calls = append(f.calls, c)
	if f.fail[c] {
		return "", false, errors.New("private-node/token=must-not-leak")
	}
	x := f.text[c]
	if len(x) > cap {
		return x[:cap], true, nil
	}
	return x, false, nil
}

func diagnosticPod(phase string, states ...map[string]any) map[string]any {
	inits, main := []any{}, []any{}
	for _, st := range states {
		if st["name"] == "push" {
			main = append(main, st)
		} else {
			inits = append(inits, st)
		}
	}
	return map[string]any{
		"metadata": map[string]any{"name": "build-b1", "namespace": BuildNamespace, "labels": map[string]any{LabelManagedBy: ManagedByValue, LabelProject: "p1", LabelBuild: "b1"}},
		"status":   map[string]any{"phase": phase, "initContainerStatuses": inits, "containerStatuses": main},
	}
}

func terminated(c string, code int, reason, message string) map[string]any {
	return map[string]any{"name": c, "state": map[string]any{"terminated": map[string]any{"exitCode": code, "signal": 9, "reason": reason, "message": message, "finishedAt": "2026-11-09T00:00:00Z"}}}
}

func TestBuildFailureEvidence(t *testing.T) {
	for _, tt := range []struct {
		stage, reason, message string
		code                   int
	}{
		{"kaniko", "OOMKilled", "", 137},
		{"kaniko", "Error", "RUN command exited 2", 2},
		{"base", "Error", "registry returned 401 Unauthorized", 1},
		{"push", "Error", "registry returned 403 Forbidden", 1},
	} {
		t.Run(tt.stage+tt.reason, func(t *testing.T) {
			s := buildStateOf(diagnosticPod("Failed", terminated(tt.stage, tt.code, tt.reason, tt.message)))
			if s.Status != BuildError || s.Step != tt.stage || len(s.Stages) != 1 || *s.Stages[0].ExitCode != tt.code || s.Stages[0].Reason != tt.reason || s.Stages[0].Signal != 9 || !strings.Contains(s.Reason, tt.reason) || !strings.Contains(s.Reason, tt.message) {
				t.Fatalf("evidence lost: %+v", s)
			}
		})
	}
	p := diagnosticPod("Failed", terminated("kaniko", 137, "Error", ""))
	if s := buildStateOf(p); strings.Contains(s.Reason, "OOM") {
		t.Fatalf("exit137 is not proof of OOM: %+v", s)
	}
	for _, reason := range []string{"Evicted", "DeadlineExceeded"} {
		p["status"].(map[string]any)["reason"] = reason
		p["status"].(map[string]any)["message"] = "private-node internal-address"
		if s := buildStateOf(p); !strings.Contains(s.Reason, reason) || strings.Contains(s.Reason, "private-node") {
			t.Fatalf("unsafe/missing pod reason: %+v", s)
		}
	}
	w := map[string]any{"name": "base", "state": map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff", "message": "private-node"}}}
	if s := buildStateOf(diagnosticPod("Pending", w)); s.Stages[0].Reason != "ImagePullBackOff" || strings.Contains(s.Stages[0].Message, "private-node") {
		t.Fatalf("waiting state: %+v", s)
	}
}

func TestCaptureBuildLogsRecoveryOwnershipAndBounds(t *testing.T) {
	f := &diagnosticAPI{fakeAPI: newFake(), text: map[string]string{"fetch": "f\n", "base": "b\n", "kaniko": "RUN exit 2\nstderr\n", "push": "p\n"}, fail: map[string]bool{"base": true}}
	s := NewWithAPI(f, Config{}, "s1")
	pod := diagnosticPod("Failed", terminated("fetch", 0, "Completed", ""), terminated("base", 0, "Completed", ""), terminated("kaniko", 2, "Error", ""), terminated("push", 0, "Completed", ""))
	_ = f.ApplyCore(context.Background(), "pods", pod)
	got, err := s.CaptureBuildLogs(context.Background(), "p1", "b1")
	if err != nil || len(got) != 4 || !got[1].Unavailable || got[2].Lines[1] != "stderr" {
		t.Fatalf("capture: %+v %v", got, err)
	}
	f.fail["base"] = false
	got, _ = s.CaptureBuildLogs(context.Background(), "p1", "b1")
	if got[1].Unavailable || got[1].Lines[0] != "b" {
		t.Fatal("transient log read did not recover")
	}
	f.calls = nil
	if _, err := s.CaptureBuildLogs(context.Background(), "p2", "b1"); !errors.Is(err, ErrNotFound) || len(f.calls) != 0 {
		t.Fatal("cross-project log access")
	}
	if err := s.CleanupBuild(context.Background(), "p2", "b1"); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.GetCore(context.Background(), "pods", BuildNamespace, "build-b1"); p == nil {
		t.Fatal("cross-project cleanup")
	}
	f.text["kaniko"] = strings.Repeat("x", BuildStageLogBytes+99)
	got, _ = s.CaptureBuildLogs(context.Background(), "p1", "b1")
	if !got[2].Truncated || len(got[2].Tail) == 0 || len(got[2].Lines[0]) > BuildStageLogBytes {
		t.Fatal("unbounded log line")
	}
	f.text["kaniko"] = "complete\npartial"
	pod["status"].(map[string]any)["phase"] = "Running"
	got, _ = s.CaptureBuildLogs(context.Background(), "p1", "b1")
	if len(got[2].Lines) != 1 || got[2].Lines[0] != "complete" {
		t.Fatal("growing partial line committed")
	}
	pod["status"].(map[string]any)["phase"] = "Failed"
	if err := s.CleanupBuild(context.Background(), "p1", "b1"); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.GetCore(context.Background(), "pods", BuildNamespace, "build-b1"); p != nil {
		t.Fatal("authorized cleanup failed")
	}
}

func TestBuildLogRedaction(t *testing.T) {
	for _, in := range []string{
		"Authorization: Bearer token-value", "authorization=Basic dXNlcjpwYXNz", "https://u:p@example.com/a?token=secret-value&x=1", `{"auth":"encoded-secret","password":"password-value"}`, "?X-Amz-Signature=signature-value",
	} {
		out := RedactBuildText(in)
		if out == in || strings.Contains(out, "secret-value") || strings.Contains(out, "token-value") || strings.Contains(out, "encoded-secret") || strings.Contains(out, "signature-value") {
			t.Fatalf("not redacted: %s", out)
		}
	}
}
