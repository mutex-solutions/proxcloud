package registrypins

import (
	"errors"
	"strings"
	"testing"
)

func TestCanonicalHostAndExactRefs(t *testing.T) {
	for raw, want := range map[string]string{"REGISTRY.internal:443": "registry.internal", "192.0.2.3:5000": "192.0.2.3:5000", "[2001:db8::1]:443": "[2001:db8::1]", "registry.internal:05000": "registry.internal:5000"} {
		got, err := CanonicalHost(raw)
		if err != nil || got != want {
			t.Fatalf("host %q = %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "https://r.internal", "user@r.internal", "r.internal/t", "r.internal?x", "r.internal#x", "r.internal.", "r.internal:", "r.internal:0", "r.internal:65536", "r internal", "-r.internal", "r.internal\n", "r%2einternal"} {
		if _, err := CanonicalHost(raw); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid host %q accepted: %v", raw, err)
		}
	}
	repo := "registry.internal/t/project1/0123456789abcdef0123"
	digest := "sha256:" + strings.Repeat("ab", 32)
	for _, raw := range []string{repo + ":build-1", repo + "@" + digest} {
		r, err := ParseRef("REGISTRY.internal:443", raw)
		if err != nil || r.String() != raw || r.Project != "project1" || r.Template != "0123456789abcdef0123" {
			t.Fatalf("ref %q = %+v %v", raw, r, err)
		}
	}
	for _, raw := range []string{repo, repo + ":", repo + "@sha256:bad", repo + ":tag@" + digest, "other.internal/t/project1/0123456789abcdef0123@" + digest, "registry.internal/library/base@" + digest, repo + ":../x", repo + ":x?q", strings.Replace(repo, "project1", "../project1", 1) + ":x", strings.Replace(repo, "0123456789abcdef0123", "0123456789ABCDEF0123", 1) + ":x"} {
		if _, err := ParseRef("registry.internal", raw); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid ref %q accepted: %v", raw, err)
		}
	}
}

func TestRealDeleteHoldHasNoOverride(t *testing.T) {
	err := RealDeleteHold()
	var typed *OperatorHoldError
	if !errors.Is(err, ErrOperatorHold) || !errors.As(err, &typed) || typed.Reason != "writer_coverage_and_legacy_adoption_required" {
		t.Fatal(err)
	}
}
