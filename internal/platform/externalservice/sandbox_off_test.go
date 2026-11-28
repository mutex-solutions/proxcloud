package externalservice

import (
	"testing"
	"time"
)

// A sandbox provider has no placement only with both halves: the mark the off writes, and no kubeconfig. Everything
// else is a provider whose cluster is asked.
func TestSandboxOffSince(t *testing.T) {
	at := "2026-11-28T01:12:00Z"
	row := func(provider string, offSince any, kubeconfig string) *ExternalService {
		sb := map[string]any{"mode": "shared"}
		if offSince != nil {
			sb[SandboxOffSinceKey] = offSince
		}
		secret := map[string]any{}
		if kubeconfig != "" {
			secret["kubeconfig"] = kubeconfig
		}
		return &ExternalService{Config: map[string]any{"provider": provider, "sandbox": sb}, Secret: secret}
	}
	if since, off := row("sandbox", at, "").SandboxOffSince(); !off || since.Format(time.RFC3339) != at {
		t.Fatalf("the mark and no kubeconfig: %s %v", since, off)
	}
	for name, es := range map[string]*ExternalService{
		"a kubeconfig beside the mark":   row("sandbox", at, "apiVersion: v1"),
		"no kubeconfig and no mark":      row("sandbox", nil, ""),
		"a mark that is no time":         row("sandbox", "yes", ""),
		"a mark that is not a string":    row("sandbox", true, ""),
		"another provider with the mark": row("kamaji", at, ""),
		"no row":                         nil,
	} {
		if since, off := es.SandboxOffSince(); off || !since.IsZero() {
			t.Errorf("%s reads as a provider with no placement since %s", name, since)
		}
	}
}
