package syncjob

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// A refusal repeats on every cycle while the rest of the sync works: a warning. A failed PVE call is an error.
func TestProviderIsolationLevel(t *testing.T) {
	if got := providerIsolationLevel(fmt.Errorf("%w: cidrPool overlaps", pve.ErrIsolationRefused)); got != slog.LevelWarn {
		t.Errorf("a refusal logs at %v", got)
	}
	if got := providerIsolationLevel(errors.New("pve: 500")); got != slog.LevelError {
		t.Errorf("a failed call logs at %v", got)
	}
}
