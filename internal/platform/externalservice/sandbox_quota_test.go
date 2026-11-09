package externalservice

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// Key by key: the project, then the provider default, then D10's built-in default. Never unlimited.
func TestSandboxProjectQuota(t *testing.T) {
	es := &ExternalService{Config: map[string]any{"provider": "sandbox", "sandbox": map[string]any{
		"proxmoxServiceId": "pve-1",
		"projectQuota":     map[string]any{"sandboxVcpus": float64(32), "sandboxGpus": float64(1)},
	}}}
	if es.SandboxProxmoxServiceID() != "pve-1" {
		t.Fatal("proxmoxServiceId")
	}
	if got := es.SandboxProjectQuota(nil); got != (sandbox.ProjectQuota{Sandboxes: 5, VCPUs: 32, RAMMb: 32768, GPUs: 1}) {
		t.Errorf("provider default over D10: %+v", got)
	}
	project := map[string]any{"servers": float64(3), "sandboxes": float64(0), "sandboxRamMb": float64(4096), "sandboxGpus": float64(-1)}
	if got := es.SandboxProjectQuota(project); got != (sandbox.ProjectQuota{Sandboxes: 0, VCPUs: 32, RAMMb: 4096, GPUs: 1}) {
		t.Errorf("project over provider: %+v", got)
	}
	bare := &ExternalService{Config: map[string]any{"provider": "sandbox"}}
	if got := bare.SandboxProjectQuota(map[string]any{"servers": float64(1)}); got != sandbox.DefaultProjectQuota {
		t.Errorf("a quota without sandbox keys falls back to D10, not unlimited: %+v", got)
	}
}
