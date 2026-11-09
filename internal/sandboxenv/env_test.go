package sandboxenv

import (
	"fmt"
	"strings"
	"testing"
)

func TestCallerEnvironmentBudgets(t *testing.T) {
	vars := map[string]string{}
	for i := range MaxVars {
		vars[fmt.Sprintf("V%d", i)] = "value"
	}
	if err := Validate(vars); err != nil {
		t.Fatalf("256 vars: %v", err)
	}
	vars["OVER"] = "secret"
	if err := Validate(vars); err == nil || !strings.Contains(err.Error(), "256") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("count limit: %v", err)
	}
	for _, size := range []int{16 << 10, MaxValueBytes} {
		if err := Validate(map[string]string{"TOKEN": strings.Repeat("x", size)}); err != nil {
			t.Fatalf("value size %d: %v", size, err)
		}
	}
	for name, env := range map[string]map[string]string{
		"value":            {"TOKEN": strings.Repeat("secret", MaxValueBytes/6+1)},
		"nul":              {"TOKEN": "secret\x00value"},
		"name":             {"NO=NAME": "secret"},
		"reserved control": {"PROXCLOUD_TOKEN": "secret"},
		"reserved id":      {SandboxID: "secret"},
	} {
		t.Run(name, func(t *testing.T) {
			err := Validate(env)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("must reject without values: %v", err)
			}
		})
	}
	// Exactly 128 KiB with NAME=value\0 overhead; the fourth value stays below 32 KiB.
	env := map[string]string{"A": strings.Repeat("x", MaxValueBytes), "B": strings.Repeat("x", MaxValueBytes), "C": strings.Repeat("x", MaxValueBytes), "D": strings.Repeat("x", MaxValueBytes-12)}
	if err := Validate(env); err != nil {
		t.Fatalf("aggregate boundary: %v", err)
	}
	env["D"] += "x"
	if err := Validate(env); err == nil || !strings.Contains(err.Error(), "131072 bytes total") {
		t.Fatalf("aggregate +1: %v", err)
	}
	// UTF-8 bytes, not characters.
	if err := Validate(map[string]string{"UNICODE": strings.Repeat("界", MaxValueBytes/3+1)}); err == nil {
		t.Fatal("multibyte values must count bytes")
	}
}

func TestMergedEnvironmentExecBudgets(t *testing.T) {
	// 256 caller vars plus ordinary image ENV is fine, without applying caller count quotas.
	var env []string
	for i := range 300 {
		env = append(env, fmt.Sprintf("IMAGE_%d=value", i))
	}
	if err := ValidateRuntime(env, 4096, 2<<20); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		env   []string
		limit int
		want  string
	}{
		{"single image ENV", []string{"A=" + strings.Repeat("secret", (128<<10)/6+1)}, 2 << 20, "per-string"},
		{"aggregate image ENV", []string{strings.Repeat("x", 100<<10), strings.Repeat("x", 100<<10), strings.Repeat("x", 100<<10)}, 2 << 20, "including pointer"},
		{"small stack budget", []string{"A=" + strings.Repeat("x", 100<<10)}, 128 << 10, "exec budget"},
		{"NUL", []string{"TOKEN=secret\x00value"}, 2 << 20, "NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRuntime(tc.env, 4096, tc.limit)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("got %v, want %q and no values", err, tc.want)
			}
		})
	}
}
