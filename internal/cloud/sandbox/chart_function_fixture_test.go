package sandbox_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/function"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// The pods of the functions BuildDeployment makes (plain; with secrets, registry credentials, a command and
// environment), as the Deployment's controller creates them, written for test/policy, which runs them through the
// sandbox chart's pod policy: with functions on it must admit every one in an fn-* namespace. -update rewrites the
// fixture. An external test package, because internal/cloud/function imports this one.
func TestPolicyFixtureIsBuildDeployment(t *testing.T) {
	const fixture = "../../../test/policy/testdata/function-pods.json"
	cfg := sandbox.Config{Domain: "sbx.example.org", Functions: sandbox.FunctionsConfig{Enabled: true}}
	f := function.Function{ID: "fn-0123456789abcdef", ProjectID: "p1", Host: "api-a1b2c3.sbx.example.org"}
	pods := map[string]any{}
	for name, snap := range map[string]function.Snapshot{
		"plain": {Image: "ghcr.io/acme/api:1", Port: 8080, CPUMillis: 250, MemoryMi: 256},
		"full": {Image: "ghcr.io/acme/api@sha256:1111111111111111111111111111111111111111111111111111111111111111", Port: 3000, CPUMillis: 1000, MemoryMi: 1024,
			Env: map[string]string{"A": "1"}, Command: []string{"/app"}, Args: []string{"--serve"},
			SecretNames: []string{"TOKEN"}, SecretsVersion: "v1",
			RegistryAuth: &function.RegistryAuth{Server: "ghcr.io", Username: "bot", Version: "v1"}},
	} {
		tpl := function.BuildDeployment(cfg, "svc", f, snap)["spec"].(map[string]any)["template"].(map[string]any)
		meta := tpl["metadata"].(map[string]any)
		pods[name] = map[string]any{"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": f.ID + "-5d4f8c7b9-abcde", "namespace": function.NamespaceFor(f.ProjectID), "labels": meta["labels"], "annotations": meta["annotations"]},
			"spec":     tpl["spec"]}
	}
	got, err := json.MarshalIndent(pods, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if u := flag.Lookup("update"); u != nil && u.Value.String() == "true" {
		if err := os.WriteFile(fixture, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read %s (run with -update to create): %v", fixture, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is not what BuildDeployment makes now: run with -update, then test/policy", fixture)
	}
}
