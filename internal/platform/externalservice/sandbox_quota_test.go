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

// A project's function quota, key by key: the project's own document, then the provider's functions.projectQuota,
// then the built-in default. Never unlimited: an absent or negative key falls back. Its keys are not those of the
// project's quota document, and nothing of that document is read.
func TestFunctionProjectQuota(t *testing.T) {
	es := &ExternalService{Config: map[string]any{"provider": "sandbox", "sandbox": map[string]any{
		"projectQuota": map[string]any{"sandboxVcpus": float64(32)},
		"functions":    map[string]any{"enabled": true, "projectQuota": map[string]any{"vcpus": float64(4)}},
	}}}
	if got := es.FunctionProjectQuota(nil); got != (sandbox.FunctionQuota{Functions: 20, VCPUs: 4, RAMMb: 16384}) {
		t.Errorf("the provider's default over the built-in one: %+v", got)
	}
	own := map[string]any{"functions": float64(0), "ramMb": float64(2048), "vcpus": float64(-1), "servers": float64(3)}
	if got := es.FunctionProjectQuota(own); got != (sandbox.FunctionQuota{Functions: 0, VCPUs: 4, RAMMb: 2048}) {
		t.Errorf("the project's own over the provider's: %+v", got)
	}
	bare := &ExternalService{Config: map[string]any{"provider": "sandbox"}}
	if got := bare.FunctionProjectQuota(map[string]any{"sandboxes": float64(1), "functionVcpus": float64(99)}); got != (sandbox.FunctionQuota{Functions: 20, VCPUs: 8, RAMMb: 16384}) {
		t.Errorf("a document without function quota keys falls back to the built-in default, not unlimited: %+v", got)
	}
	// The sandbox quota does not see the function keys, and the other way round.
	if got := es.SandboxProjectQuota(own); got.VCPUs != 32 || got.RAMMb != sandbox.DefaultProjectQuota.RAMMb {
		t.Errorf("the sandbox quota read a function quota document: %+v", got)
	}
}

// The functions block is read from the provider document as it is written: the switch, the idle bounds, the caps of
// one function, the floor (minInstances, maxMinInstances) and the default quota of a project. The token functions
// are woken with is the secret's functionWakeToken and nothing else: never the sandbox wake token, and a provider
// that holds only the sandbox one has no function token.
func TestSandboxConfigReadsFunctions(t *testing.T) {
	es := &ExternalService{Config: map[string]any{"provider": "sandbox", "sandbox": map[string]any{
		"domain": "sbx.example.org",
		"functions": map[string]any{"enabled": true, "minInstances": float64(1), "maxMinInstances": float64(1), "idleSeconds": float64(120),
			"minIdleSeconds": float64(60), "maxIdleSeconds": float64(600), "maxCpuMillis": float64(1000), "maxMemoryMi": float64(2048),
			"maxFunctions": float64(7), "maxInstances": float64(3), "projectQuota": map[string]any{"functions": float64(2), "vcpus": float64(1), "ramMb": float64(512)}},
	}}, Secret: map[string]any{"functionWakeToken": "t0ken", "proxyWakeToken": ""}}
	cfg := es.SandboxConfig()
	want := sandbox.FunctionsConfig{Enabled: true, MinInstances: 1, MaxMinInstances: 1, IdleSeconds: 120, MinIdleSeconds: 60, MaxIdleSeconds: 600,
		MaxCPUMillis: 1000, MaxMemoryMi: 2048, MaxFunctions: 7, MaxInstances: 3, ProjectQuota: sandbox.FunctionQuota{Functions: 2, VCPUs: 1, RAMMb: 512}}
	if cfg.Functions != want || cfg.ValidateFunctions() != nil {
		t.Errorf("functions block: %+v (%v), want %+v", cfg.Functions, cfg.ValidateFunctions(), want)
	}
	if cfg.FunctionWakeToken != "t0ken" || cfg.ProxyWakeToken != "" {
		t.Errorf("the wake tokens: function %q, sandbox %q, want the function token alone", cfg.FunctionWakeToken, cfg.ProxyWakeToken)
	}
	other := &ExternalService{Config: es.Config, Secret: map[string]any{"proxyWakeToken": "sandbox-t0ken"}}
	if cfg := other.SandboxConfig(); cfg.FunctionWakeToken != "" || cfg.ProxyWakeToken != "sandbox-t0ken" {
		t.Errorf("a provider with the sandbox wake token alone: function %q, sandbox %q", cfg.FunctionWakeToken, cfg.ProxyWakeToken)
	}
	if off := (&ExternalService{Config: map[string]any{"provider": "sandbox", "sandbox": map[string]any{"domain": "sbx.example.org"}}}).SandboxConfig(); off.Functions.Enabled {
		t.Error("a provider without a functions block has functions on")
	}
}
