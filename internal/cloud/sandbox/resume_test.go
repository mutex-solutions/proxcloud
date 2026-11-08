package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Build and snapshot pods ask the scheduler for a little disk and may use up to the limit: a container with a limit
// and no request requests the limit, and a 30Gi request schedules on no 30 GB worker (seen on the lab: Pending,
// "Insufficient ephemeral-storage").
func TestBuildPodsRequestLessDiskThanTheLimit(t *testing.T) {
	res, _ := RenderDockerfile("python:3.12", nil)
	_, build := BuildObjects(Config{}, BuildRequest{ProjectID: "p1", TemplateID: "t1", BuildID: "b1", Image: testRegistry + "/p1/t1:b1",
		Result: res, CPU: 1, MemoryGiB: 1, FetchURL: func(h string) string { return h }})
	snap := SnapshotObjects(Config{}, SnapshotRequest{ProjectID: "p1", TemplateID: "t1", BuildID: "b2", Base: "b", Image: "i", LayerURL: "u"})
	for name, pod := range map[string]map[string]any{"build": build, "snapshot": snap} {
		checked := 0
		cs, _ := dig(pod, "spec", "containers").([]any)
		ics, _ := dig(pod, "spec", "initContainers").([]any)
		for _, c := range append(cs, ics...) {
			c := c.(map[string]any)
			lim := digStr(c, "resources", "limits", "ephemeral-storage")
			if lim == "" {
				continue
			}
			checked++
			if got := digStr(c, "resources", "requests", "ephemeral-storage"); got != buildDiskRequest || lim != buildDiskLimit {
				t.Errorf("%s/%s: ephemeral-storage request %q limit %q", name, c["name"], got, lim)
			}
		}
		if checked == 0 {
			t.Errorf("%s: no container with a disk limit was checked", name)
		}
	}
}

// A resume or fork counts against the project quota like a create: with two 1 vCPU sandboxes live and a 2 vCPU cap,
// bringing a third back is refused, and with room it is not.
func TestResumeChecksProjectQuota(t *testing.T) {
	f := newFake()
	cfg := Config{Templates: map[string]Template{"py": {Image: "python:3.12", CPU: 1, MemoryGiB: 1}}}
	s := NewWithAPI(f, cfg, "svc")
	for range 2 {
		if _, err := s.CreateSandbox(context.Background(), &Spec{ProjectID: "p1", Template: "py", TimeoutSeconds: 600}); err != nil {
			t.Fatal(err)
		}
	}
	saved := BuildCR(cfg, "svc", Spec{ID: "sbx-00000009", ProjectID: "p1", Template: "py", CPU: 1, MemoryGiB: 1}, time.Unix(0, 0))
	q := &ProjectQuota{Sandboxes: 5, VCPUs: 2, RAMMb: 8192}
	if err := s.ResumeSandbox(context.Background(), "p1", saved, "python:3.12", time.Unix(3600, 0), q); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("resume over the vCPU quota: %v", err)
	}
	if _, ok := f.crs[NamespaceFor("p1")+"/sbx-00000009"]; ok {
		t.Fatal("a refused resume still created the sandbox")
	}
	if err := s.ResumeSandbox(context.Background(), "p1", saved, "python:3.12", time.Unix(3600, 0), &ProjectQuota{Sandboxes: 5, VCPUs: 3, RAMMb: 8192}); err != nil {
		t.Fatalf("resume within quota: %v", err)
	}
}
