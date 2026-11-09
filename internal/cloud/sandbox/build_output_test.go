package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildOutputHandoff(t *testing.T) {
	res, _ := RenderDockerfile("python:3.12", nil)
	_, pod := BuildObjects(Config{}, BuildRequest{Result: res})
	prepare := firstInitContainer(pod, "prepare-output")
	if prepare["image"] != craneDebugImage || !reflect.DeepEqual(prepare["command"], []any{"/busybox/sh", "-c", buildOutputScript}) {
		t.Fatalf("handoff must reuse the pinned shell and fixed script: %v", prepare)
	}
	if !reflect.DeepEqual(prepare["volumeMounts"], []any{mount("out", "/out")}) || prepare["env"] != nil || prepare["envFrom"] != nil {
		t.Errorf("handoff may see only the isolated output volume, no credentials/config/cache: %v", prepare)
	}
	wantSecurity := map[string]any{
		"runAsUser": int64(0), "runAsGroup": int64(0),
		"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
		"capabilities": map[string]any{"drop": []any{"ALL"}},
	}
	if !reflect.DeepEqual(prepare["securityContext"], wantSecurity) {
		t.Errorf("handoff security = %v, want %v", prepare["securityContext"], wantSecurity)
	}
	push := dig(pod, "spec", "containers").([]any)[0].(map[string]any)
	if push["image"] != craneImage || push["securityContext"] != nil {
		t.Error("push must retain its existing pinned image/nonroot default")
	}
	for _, raw := range push["volumeMounts"].([]any) {
		m := raw.(map[string]any)
		if m["name"] == "out" && m["readOnly"] != true {
			t.Error("push must read output without write access")
		}
	}
}

type outputLogsAPI struct {
	*fakeAPI
}

func (f *outputLogsAPI) PodLogs(_ context.Context, _, _, container string, _ int) (string, error) {
	if container == "prepare-output" {
		return "invalid build output entry\n", nil
	}
	return "", nil
}

func TestBuildOutputDiagnostics(t *testing.T) {
	s := NewWithAPI(&outputLogsAPI{newFake()}, Config{}, "svc")
	if logs := s.BuildLogs(context.Background(), "b1", 10); !reflect.DeepEqual(logs, []string{"invalid build output entry"}) {
		t.Errorf("handoff failure logs missing: %v", logs)
	}
	pod := map[string]any{"status": map[string]any{
		"phase": "Failed", "initContainerStatuses": []any{map[string]any{
			"name": "prepare-output", "state": map[string]any{"terminated": map[string]any{"exitCode": 1}},
		}},
	}}
	if st := buildStateOf(pod); st.Status != BuildError || st.Step != "prepare-output" || st.Reason != "preparing the built image failed (exit 1)" {
		t.Errorf("handoff error = %+v", st)
	}
}

func TestBuildOutputScript(t *testing.T) {
	for _, tool := range []string{"sh", "find", "chmod"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("script test needs %s: %v", tool, err)
		}
	}
	for _, name := range []string{"valid", "top-symlink", "nested-symlink", "hardlink", "special-file"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			output := filepath.Join(root, "image")
			outside := filepath.Join(root, "outside")
			if err := os.WriteFile(outside, []byte("outside canary"), 0600); err != nil {
				t.Fatal(err)
			}
			if name == "top-symlink" {
				if err := os.Symlink(root, output); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			} else {
				if err := os.Mkdir(output, 0755); err != nil {
					t.Fatal(err)
				}
				switch name {
				case "nested-symlink":
					if err := os.Symlink(outside, filepath.Join(output, "link")); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				case "hardlink":
					if err := os.Link(outside, filepath.Join(output, "alias")); err != nil {
						t.Skipf("hardlink unavailable: %v", err)
					}
				case "special-file":
					if _, err := exec.LookPath("mkfifo"); err != nil {
						t.Skipf("mkfifo unavailable: %v", err)
					}
					if err := exec.Command("mkfifo", filepath.Join(output, "pipe")).Run(); err != nil {
						t.Fatal(err)
					}
				}
			}
			inside := filepath.Join(output, "blob")
			if name != "top-symlink" {
				if err := os.WriteFile(inside, []byte("unchanged OCI blob bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Only adapt the fixed mount path to an isolated test directory. The shell
			// logic/flags are identical; the exact BusyBox image is also checked offline.
			quoted := "'" + strings.ReplaceAll(output, "'", "'\\''") + "'"
			script := strings.ReplaceAll(buildOutputScript, "/out/image", quoted)
			text, err := exec.Command("sh", "-c", script).CombinedOutput()
			if (err == nil) != (name == "valid") {
				t.Fatalf("script error = %v, output = %s", err, text)
			}
			check := func(path string, mode os.FileMode, contents string) {
				t.Helper()
				st, err := os.Stat(path)
				if err != nil || st.Mode().Perm() != mode {
					t.Fatalf("%s: stat=%v err=%v, want mode %o", path, st, err, mode)
				}
				if contents != "" {
					b, err := os.ReadFile(path)
					if err != nil || string(b) != contents {
						t.Errorf("%s: bytes=%q err=%v", path, b, err)
					}
				}
			}
			check(outside, 0600, "outside canary")
			if name == "valid" {
				check(inside, 0444, "unchanged OCI blob bytes")
				check(output, 0555, "")
				// Let t.TempDir remove its own now-readonly fixture.
				t.Cleanup(func() { _ = os.Chmod(output, 0755) })
			} else if name != "top-symlink" {
				check(inside, 0600, "unchanged OCI blob bytes")
				check(output, 0755, "")
			}
		})
	}
}
