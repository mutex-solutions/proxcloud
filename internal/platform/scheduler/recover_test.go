package scheduler

import (
	"context"
	"testing"
)

// TestRunRecoveredSwallowsPanic: a panicking job must be contained — an unguarded panic in a
// cron fn crashes the whole API pod (robfig/cron does not recover), which is how one bad
// billing doc froze every midnight job in prod.
func TestRunRecoveredSwallowsPanic(t *testing.T) {
	ran := false
	runRecovered(context.Background(), "boom", func(context.Context) {
		ran = true
		panic("job blew up")
	})
	if !ran {
		t.Fatal("fn did not run")
	}
}
