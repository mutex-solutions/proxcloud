package project

import (
	"testing"
	"time"
)

func TestLiveCreatedAt(t *testing.T) {
	if ts := liveCreatedAt(map[string]any{"created_at": "2026-07-02T03:33:34Z"}); ts == nil || ts.UTC() != time.Date(2026, 7, 2, 3, 33, 34, 0, time.UTC) {
		t.Fatalf("rfc3339: %v", ts)
	}
	if ts := liveCreatedAt(map[string]any{"created_at": "2026-07-02T03:33:34"}); ts == nil {
		t.Fatalf("neutron no-Z form must parse")
	}
	if ts := liveCreatedAt(map[string]any{}); ts != nil {
		t.Fatalf("missing → nil, got %v", ts)
	}
	if ts := liveCreatedAt(map[string]any{"created_at": "garbage"}); ts != nil {
		t.Fatalf("garbage → nil, got %v", ts)
	}
}
