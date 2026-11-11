package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/sandboxenv"
)

func assertSandboxID(t *testing.T, cr map[string]any, want string) {
	t.Helper()
	count := 0
	for _, e := range firstContainer(cr)["env"].([]any) {
		m := e.(map[string]any)
		if m["name"] == sandboxenv.SandboxID {
			count++
			if m["value"] != want || m["valueFrom"] != nil {
				t.Errorf("sandbox ID not authoritative: %v", m)
			}
		}
	}
	if count != 1 {
		t.Errorf("ID env count = %d, want 1", count)
	}
}

func TestEnvironmentCreateAndSavedIDs(t *testing.T) {
	f := newFake()
	cfg := testConfig()
	cfg.TemplateRegistry = "registry.example/t"
	python := cfg.Templates["python"]
	python.Image = "external.example/library/python:3.12"
	cfg.Templates["python"] = python
	cfg.Envd = true
	s := NewWithAPI(f, cfg, "svc-sbx")
	sp := Spec{ProjectID: "p1", Template: "python", Env: map[string]string{"LARGE": strings.Repeat("secret", 3000)}}
	if _, err := s.CreateSandbox(context.Background(), &sp); err != nil {
		t.Fatal(err)
	}
	cr := f.crs[NamespaceFor("p1")+"/"+sp.ID]
	assertSandboxID(t, cr, sp.ID)
	b, _ := json.Marshal(sandboxData(cr))
	if strings.Contains(string(b), "secret") {
		t.Fatal("env value leaked into cache/API payload")
	}
	// A snapshot-create uses BuildCR with its new server ID, overriding image ENV.
	snapshot := cfg.Templates["python"]
	created := BuildCR(cfg, "svc-sbx", Spec{ID: "sbx-00000002", ProjectID: "p1", Custom: &snapshot, Env: map[string]string{sandboxenv.SandboxID: "spoof"}}, time.Now())
	assertSandboxID(t, created, "sbx-00000002")
	args, ok := firstContainer(created)["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "--serve" {
		t.Fatalf("image CMD must not select sandbox-init utility modes: %v", firstContainer(created)["args"])
	}
	// Old saved snapshots may have no ID, duplicates, or a stale valueFrom.
	for _, old := range []any{nil, []any{map[string]any{"name": sandboxenv.SandboxID, "value": "old"}, map[string]any{"name": sandboxenv.SandboxID, "valueFrom": map[string]any{}}}} {
		firstContainer(cr)["env"] = old
		clone, id := CloneSaved(cr)
		assertSandboxID(t, clone, id)
		if err := s.ResumeSandbox(context.Background(), "p1", cr, python.Image, time.Now().Add(time.Hour), nil); err != nil {
			t.Fatal(err)
		}
		assertSandboxID(t, cr, sp.ID)
		if err := s.ResumeSandbox(context.Background(), "p2", cr, python.Image, time.Now().Add(time.Hour), nil); err != ErrNotFound {
			t.Fatalf("cross-project resume: %v", err)
		}
	}
}

func TestSpecEnvironmentAdmission(t *testing.T) {
	sp := Spec{Template: "python", Env: map[string]string{sandboxenv.SandboxID: "spoof"}}
	if err := sp.Validate(testConfig()); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("caller ID must be refused: %v", err)
	}
}
