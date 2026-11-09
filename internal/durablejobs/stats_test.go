package durablejobs

import (
	"context"
	"strings"
	"testing"
)

func TestHistoryInvalidDSNDoesNotLeakCredentials(t *testing.T) {
	canary := "never-log-password-canary"
	_, err := OpenHistory(context.Background(), "postgres://alice:"+canary+"@host:invalid/db")
	if err == nil || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "postgres://") {
		t.Fatalf("unsafe history error: %v", err)
	}
}
func TestBackoffBounded(t *testing.T) {
	for _, n := range []int{-1, 0, 1, 8, 20, 100} {
		for i := 0; i < 10; i++ {
			d := Backoff(n)
			if d <= 0 || d > 15*60*1e9 {
				t.Fatalf("backoff(%d)=%v", n, d)
			}
		}
	}
}
