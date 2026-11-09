package externalservice

import "testing"

func TestSandboxConfig(t *testing.T) {
	es := &ExternalService{
		ID: "svc-sbx", Type: TypeCloud,
		Config: map[string]any{
			"provider": "sandbox",
			"regions":  map[string]any{"az1": map[string]any{}},
			"services": map[string]any{"sandbox": map[string]any{"az1": true}},
			"sandbox": map[string]any{
				"templates": map[string]any{
					"python": map[string]any{"image": "python:3.12-slim", "cpu": float64(2), "memoryGiB": float64(4), "command": []any{"bash", "-c", "sleep infinity"}},
					"code-interpreter-v1": map[string]any{"image": "ci@sha256:1", "startCmd": "/root/start-up.sh", "readyCmd": "curl -fs localhost:49999/health",
						"user": "user", "workdir": "/home/user", "startAsRoot": true},
					"junk": "not-a-map",
				},
				"gpuModels":        []any{"nvidia-3090", "nvidia-a6000"},
				"limits":           map[string]any{"maxCpu": float64(16), "maxConcurrentGpus": float64(2)},
				"templateRegistry": "registry.example.org/sandbox-templates",
			},
		},
		Secret: map[string]any{"kubeconfig": "apiVersion: v1"},
	}
	if !es.IsSandbox() || es.IsDbaas() {
		t.Fatal("discriminator")
	}
	c := es.SandboxConfig()
	if c.Kubeconfig != "apiVersion: v1" || c.Region != "az1" {
		t.Errorf("kubeconfig/region: %+v", c)
	}
	if ci := c.Templates["code-interpreter-v1"]; ci.StartCmd != "/root/start-up.sh" || ci.ReadyCmd == "" || ci.User != "user" || ci.Workdir != "/home/user" || !ci.StartAsRoot {
		t.Errorf("built-in runtime fields: %+v", ci)
	}
	delete(c.Templates, "code-interpreter-v1")
	if len(c.Templates) != 1 || c.Templates["python"].CPU != 2 || c.Templates["python"].MemoryGiB != 4 || len(c.Templates["python"].Command) != 3 {
		t.Errorf("templates: %+v", c.Templates)
	}
	if c.TemplateRegistry != "registry.example.org/sandbox-templates" {
		t.Errorf("templateRegistry: %q", c.TemplateRegistry)
	}
	if len(c.GPUModels) != 2 || c.Limits.MaxCPU != 16 || c.Limits.MaxConcurrentGPUs != 2 {
		t.Errorf("gpu/limits: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}
