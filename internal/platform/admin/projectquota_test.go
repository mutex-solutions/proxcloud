package admin

import (
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func TestValidateProjectQuota(t *testing.T) {
	cases := []struct {
		name string
		body pgdoc.M
		ok   bool
	}{
		{"empty", pgdoc.M{}, true},
		{"no gpu key", pgdoc.M{"cores": float64(10)}, true},
		{"valid gpu", pgdoc.M{"gpu": map[string]any{"a100-80gb": float64(4), "*": float64(8)}}, true},
		{"gpu not object", pgdoc.M{"gpu": "x"}, false},
		{"negative", pgdoc.M{"gpu": map[string]any{"h100": float64(-1)}}, false},
		{"non integer", pgdoc.M{"gpu": map[string]any{"h100": 1.5}}, false},
		{"non numeric", pgdoc.M{"gpu": map[string]any{"h100": "two"}}, false},
		{"empty model", pgdoc.M{"gpu": map[string]any{" ": float64(1)}}, false},
		{"canonical collision", pgdoc.M{"gpu": map[string]any{"NVIDIA_A100": float64(1), "nvidia-a100": float64(2)}}, false},
		{"resource keys", pgdoc.M{"servers": float64(5), "vcpus": float64(0), "ramMb": float64(8192), "floatingIps": float64(1)}, true},
		{"resource negative", pgdoc.M{"vcpus": float64(-1)}, false},
		{"resource non integer", pgdoc.M{"diskGb": 10.5}, false},
		{"resource non numeric", pgdoc.M{"networks": "3"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateProjectQuota(c.body)
			if (err == nil) != c.ok {
				t.Fatalf("validateProjectQuota(%v) err=%v, want ok=%v", c.body, err, c.ok)
			}
		})
	}
}

func TestNormalizeProjectQuotaCanonicalizesGPUModels(t *testing.T) {
	got, err := normalizeProjectQuota(pgdoc.M{
		"gpu": map[string]any{" NVIDIA_A100_80GB ": float64(4), "*": float64(8)},
	})
	if err != nil {
		t.Fatalf("normalizeProjectQuota() error = %v", err)
	}
	gpu := got["gpu"].(map[string]any)
	if gpu["nvidia-a100-80gb"] != float64(4) || gpu["*"] != float64(8) || len(gpu) != 2 {
		t.Fatalf("normalized GPU quota = %#v", gpu)
	}
}

// A function limit is no key of the project's quota document. Stored there it would limit no function, and on a
// project that held only sandbox keys it would turn the document into a Proxmox quota whose absent keys are
// unlimited: every spelling it could take is refused, with where it belongs.
func TestProjectQuotaRefusesFunctionLimits(t *testing.T) {
	for _, key := range []string{"functions", "functionVcpus", "functionRamMb", "functionQuota"} {
		_, err := normalizeProjectQuota(pgdoc.M{"sandboxes": float64(2), key: float64(5)})
		if err == nil || err.Status != 400 || !strings.Contains(err.Msg, "/function-quota") {
			t.Errorf("quota.%s: %v, want 400 naming the function quota route", key, err)
		}
	}
	if _, err := normalizeProjectQuota(pgdoc.M{"sandboxes": float64(2), "vcpus": float64(4), "ramMb": float64(1024)}); err != nil {
		t.Errorf("the Proxmox keys vcpus and ramMb are still the project's: %v", err)
	}
}

// The function quota document: its three keys, each a non-negative integer and optional; nothing else.
func TestNormalizeFunctionQuota(t *testing.T) {
	for name, c := range map[string]struct {
		body pgdoc.M
		ok   bool
	}{
		"empty clears":           {pgdoc.M{}, true},
		"all three":              {pgdoc.M{"functions": float64(5), "vcpus": float64(2), "ramMb": float64(4096)}, true},
		"one key":                {pgdoc.M{"vcpus": float64(0)}, true},
		"negative":               {pgdoc.M{"vcpus": float64(-1)}, false},
		"not an integer":         {pgdoc.M{"ramMb": 512.5}, false},
		"not a number":           {pgdoc.M{"functions": "5"}, false},
		"a Proxmox key":          {pgdoc.M{"servers": float64(1)}, false},
		"a sandbox key":          {pgdoc.M{"sandboxVcpus": float64(1)}, false},
		"the withdrawn spelling": {pgdoc.M{"functionVcpus": float64(1)}, false},
	} {
		got, err := normalizeFunctionQuota(c.body)
		if (err == nil) != c.ok || (c.ok && len(got) != len(c.body)) {
			t.Errorf("%s: %v %v, want ok=%v", name, got, err, c.ok)
		}
	}
}
