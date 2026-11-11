package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The project quota (D10) is checked against the live sandboxes on top of the provider limits, and the
// namespace ResourceQuota is lowered to it.
func TestCreateEnforcesProjectQuota(t *testing.T) {
	f := newFake()
	cfg := testConfig()
	cfg.Limits = Limits{MaxConcurrent: 10, MaxConcurrentGPUs: 1}
	s := NewWithAPI(f, cfg, "svc-sbx")
	ctx := context.Background()
	q := &ProjectQuota{Sandboxes: 3, VCPUs: 5, RAMMb: 6 * 1024, GPUs: 0}
	mk := func(cpu, mem int) Spec {
		return Spec{ProjectID: "p1", Template: "python", CPU: cpu, MemoryGiB: mem, Quota: q}
	}
	a := mk(2, 2)
	if _, err := s.CreateSandbox(ctx, &a); err != nil {
		t.Fatal(err)
	}
	hard := f.quotas["sb-p1"]["spec"].(map[string]any)["hard"].(map[string]any)
	if hard["pods"] != "3" || hard["requests.cpu"] != "5" || hard["requests.memory"] != "6144Mi" || hard["requests."+ResourceGPU] != "0" {
		t.Errorf("namespace quota = %v", hard)
	}
	for name, sp := range map[string]Spec{
		"vcpus":  mk(4, 1), // 2 + 4 > 5
		"ram_mb": mk(1, 5), // 2 + 5 GiB > 6 GiB
		"gpus":   {ProjectID: "p1", Template: "pytorch", GPU: &GPURequest{Model: "nvidia-3090"}, Quota: q},
	} {
		if _, err := s.CreateSandbox(ctx, &sp); !errors.Is(err, ErrQuotaExceeded) {
			t.Errorf("%s: err = %v, want quota", name, err)
		}
	}
	b, c := mk(1, 1), mk(1, 1)
	if _, err := s.CreateSandbox(ctx, &b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSandbox(ctx, &c); err != nil {
		t.Fatal(err)
	}
	d := mk(1, 1)
	if _, err := s.CreateSandbox(ctx, &d); !errors.Is(err, ErrQuotaExceeded) || !strings.Contains(err.Error(), "sandboxes: 3 in use") {
		t.Errorf("fourth sandbox: err = %v", err)
	}
}
