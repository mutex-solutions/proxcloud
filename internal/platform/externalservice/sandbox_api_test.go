package externalservice

import "testing"

func TestSandboxAPIExtensionConfigDoesNotReplaceExistingProviderPolicy(t *testing.T) {
	es := &ExternalService{Config: map[string]any{"provider": "sandbox", "sandbox": map[string]any{
		"templateRegistry": "registry.example.org:5443/t",
		"startup":          map[string]any{"totalSeconds": 1000, "schedulingSeconds": 100, "pullSeconds": 800, "runtimeSeconds": 300},
		"build":            map[string]any{"cpu": 4, "memoryGiB": 8, "diskGiB": 40, "maxConcurrent": 3},
		"warming":          map[string]any{"enabled": true, "maxImages": 2, "nodeSelector": map[string]any{"pool": "sandbox"}},
		"import":           map[string]any{"sourceRepositories": map[string]any{"https://source.example.org": []any{"approved/app", "project/{project}/app"}}, "allowAnonymous": false},
		"rotation":         map[string]any{"enabled": true},
		"egressMbps":       75,
	}}, Secret: map[string]any{"kubeconfig": "sandbox-only", "rotationKubeconfig": "rotation-only", "registryGcUser": "gc"}}
	cfg := es.SandboxConfig()
	if cfg.Startup.TotalSeconds != 1000 || cfg.Startup.PullSeconds != 800 || cfg.Build.CPU != 4 || cfg.Build.DiskGiB != 40 {
		t.Fatalf("startup/build lost: %+v %+v", cfg.Startup, cfg.Build)
	}
	if !cfg.Warming.Enabled || cfg.Warming.NodeSelector["pool"] != "sandbox" || len(cfg.Import.SourceRepositories["https://source.example.org"]) != 2 || cfg.Import.AllowAnonymous {
		t.Fatal("typed warming/import policy not decoded")
	}
	if cfg.EgressMbps != 75 || cfg.Kubeconfig != "sandbox-only" || cfg.Rotation.Kubeconfig != "rotation-only" || cfg.RegistryGC.User != "gc" {
		t.Fatal("existing scoped provider policy or secrets replaced")
	}
}
