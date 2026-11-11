// Package sandboxenv defines sandbox environment budgets shared by admission and PID 1.
package sandboxenv

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	SandboxID     = "E2B_SANDBOX_ID"
	MaxVars       = 256
	MaxValueBytes = 32 << 10
	MaxBytes      = 128 << 10
	// MaxRuntimeBytes includes image ENV, pod env and pointer overhead. Admission cannot
	// see image ENV. Keep room for it without imposing the caller quotas on system vars.
	MaxRuntimeBytes  = 256 << 10
	ExecReserveBytes = 32 << 10 // argv, per-user defaults and envd's own variables
)

var keyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// Validate checks caller variables only; errors never contain environment values.
// Byte accounting matches execve's NAME=value\0 strings, not JSON encoding size.
func Validate(env map[string]string) error {
	if len(env) > MaxVars {
		return fmt.Errorf("at most %d environment variables", MaxVars)
	}
	total := 0
	for k, v := range env {
		if !keyRE.MatchString(k) {
			return fmt.Errorf("invalid environment variable name %q", k)
		}
		if strings.HasPrefix(k, "PROXCLOUD_") || strings.HasPrefix(k, "KUBERNETES_") || k == SandboxID {
			return fmt.Errorf("environment variable %s is reserved", k)
		}
		if strings.IndexByte(v, 0) >= 0 {
			return fmt.Errorf("environment variable %s contains NUL", k)
		}
		if len(v) > MaxValueBytes {
			return fmt.Errorf("environment variable %s is longer than %d bytes", k, MaxValueBytes)
		}
		total += len(k) + len(v) + 2
	}
	if total > MaxBytes {
		return fmt.Errorf("environment exceeds %d bytes total (names, values, '=' and NUL terminators)", MaxBytes)
	}
	return nil
}

// ValidateRuntime checks the actual merged image/pod environment before envd or user
// commands start. Image names need not obey the caller's shell identifier grammar.
// Linux MAX_ARG_STRLEN is 32 pages, including NUL. argMax is the current execve
// argv+env limit (RLIMIT_STACK dependent), not an assumed fixed 2 MiB.
func ValidateRuntime(environ []string, pageSize, argMax int) error {
	total := 0
	for _, kv := range environ {
		if strings.IndexByte(kv, 0) >= 0 {
			return fmt.Errorf("merged image and sandbox environment contains NUL")
		}
		if len(kv)+1 > 32*pageSize {
			return fmt.Errorf("merged image and sandbox environment entry exceeds Linux per-string limit (%d bytes)", 32*pageSize)
		}
		total += len(kv) + 1 + 8 // conservative 64-bit envp pointer accounting
	}
	if total > MaxRuntimeBytes {
		return fmt.Errorf("merged image and sandbox environment exceeds %d bytes including pointer overhead", MaxRuntimeBytes)
	}
	if total+ExecReserveBytes > argMax {
		return fmt.Errorf("merged image and sandbox environment exceeds Linux exec budget (%d bytes with %d reserved)", argMax, ExecReserveBytes)
	}
	return nil
}
