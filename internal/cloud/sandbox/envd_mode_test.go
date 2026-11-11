package sandbox

import (
	"encoding/json"
	"testing"
	"time"
)

// envdModePod models the API-server JSON boundary, rather than passing BuildCR's
// in-memory container representation directly to the classifier.
func envdModePod(t *testing.T, envd bool, command []string) map[string]any {
	t.Helper()
	cfg := Config{Envd: envd, Templates: map[string]Template{
		"mode-test": {Image: "example.invalid/test:local", Command: command},
	}}
	spec := Spec{ID: "sbx-12345678", ProjectID: "mode-test", Template: "mode-test"}
	if err := spec.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	cr := BuildCR(cfg, "mode-service", spec, time.Unix(1800000000, 0).UTC())
	pod := cr["spec"].(map[string]any)["podTemplate"]
	b, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestPodRunsEnvdBuildCRRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		envd    bool
		command []string
	}{
		{name: "native default"},
		{name: "native application", command: []string{"sleep", "infinity"}},
		{name: "native template naming helper", command: []string{RuntimeMountPath + "/sandbox-init"}},
		{name: "envd default", envd: true},
		{name: "envd template start command", envd: true, command: []string{"sleep", "infinity"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := envdModePod(t, tc.envd, tc.command)
			if got := PodRunsEnvd(pod); got != tc.envd {
				t.Fatalf("PodRunsEnvd(BuildCR JSON roundtrip) = %v, want %v; pod=%v", got, tc.envd, pod)
			}
			main := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			if tc.envd {
				args, ok := main["args"].([]any)
				if !ok || len(args) != 1 || args[0] != "--serve" {
					t.Fatalf("envd BuildCR must explicitly select serving mode: args=%v", main["args"])
				}
			}
		})
	}
}

func TestPodRunsEnvdServingArgsOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		args any
		want bool
	}{
		{name: "legacy omitted args", want: true},
		{name: "legacy empty args", args: []any{}, want: true},
		{name: "explicit serve", args: []any{"--serve"}, want: true},
		{name: "ready utility", args: []any{"-ready"}},
		{name: "snapshot utility", args: []any{"-snapshot"}},
		{name: "snapshot utility with extra args", args: []any{"-snapshot", "/tmp/test-snapshot"}},
		{name: "malformed string args", args: "--serve"},
		{name: "malformed args entry", args: []any{1}},
		{name: "native utility", args: []any{"-native", "sleep", "infinity"}},
		{name: "unknown utility", args: []any{"--not-a-serving-mode"}},
		{name: "serve plus utility", args: []any{"--serve", "-ready"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := envdModePod(t, true, nil)
			main := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			if tc.args == nil {
				delete(main, "args")
			} else {
				main["args"] = tc.args
			}
			if got := PodRunsEnvd(pod); got != tc.want {
				t.Fatalf("PodRunsEnvd with args %v = %v, want %v: utility modes must not get envd's traffic-token exemption", tc.args, got, tc.want)
			}
		})
	}
}

func TestPodRunsEnvdRequiresMainServingCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		pod  map[string]any
	}{
		{name: "missing spec", pod: map[string]any{}},
		{name: "missing containers", pod: map[string]any{"spec": map[string]any{}}},
		{name: "sidecar only", pod: map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "sidecar", "command": []any{RuntimeMountPath + "/sandbox-init"}},
		}}}},
		{name: "main without command", pod: map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "main"},
		}}}},
		{name: "different binary", pod: map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "main", "command": []any{"/tmp/sandbox-init"}},
		}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if PodRunsEnvd(tc.pod) {
				t.Fatal("pod without the main serving command was classified as envd")
			}
		})
	}
}
