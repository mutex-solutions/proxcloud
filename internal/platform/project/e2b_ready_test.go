package project

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

func TestE2BCreateReadyBudget(t *testing.T) {
	now := time.Date(2026, 11, 9, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, kind, end string
		custom          bool
		want            time.Duration
		knownEnd        bool
	}{
		{"normal build", "", now.Add(300 * time.Second).Format(time.RFC3339), true, 120 * time.Second, true},
		{"bounded lifetime", "", now.Add(70 * time.Second).Format(time.RFC3339), true, 65 * time.Second, true},
		{"expired", "", now.Add(-time.Second).Format(time.RFC3339), true, -6 * time.Second, true},
		{"missing end", "", "", true, 50 * time.Second, false},
		{"malformed end", "", "invalid", true, 50 * time.Second, false},
		{"builtin short lifetime", "", now.Add(time.Second).Format(time.RFC3339), false, 50 * time.Second, false},
		{"snapshot", "snapshot", now.Add(time.Second).Format(time.RFC3339), true, 50 * time.Second, false},
		{"future kind", "future", now.Add(time.Second).Format(time.RFC3339), true, 50 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget, end := e2bCreateReadyBudget(tc.custom, tc.kind, map[string]any{"shutdownTime": tc.end}, now)
			if budget != tc.want || !end.IsZero() != tc.knownEnd {
				t.Fatalf("budget=%s end=%s, want %s known=%v", budget, end, tc.want, tc.knownEnd)
			}
		})
	}
}

func TestE2BPollReadyDeadline(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		budget, readAt, storeAt  time.Duration
		expireAt                 time.Duration
		cancelRead, realDeadline bool
		wantSuccess, wantStore   bool
	}{
		{"custom after legacy cutoff", 120 * time.Second, 70 * time.Second, 70 * time.Second, 300 * time.Second, false, false, true, true},
		{"legacy cutoff", 50 * time.Second, 70 * time.Second, 70 * time.Second, 300 * time.Second, false, false, false, false},
		{"expiry after uncooperative read", 120 * time.Second, 80 * time.Second, 80 * time.Second, 75 * time.Second, false, false, false, false},
		{"expiry during cache store", 120 * time.Second, 70 * time.Second, 80 * time.Second, 75 * time.Second, false, false, false, true},
		{"caller cancels in read", 120 * time.Second, time.Second, time.Second, 300 * time.Second, true, false, false, false},
		{"uncooperative read after real deadline", time.Millisecond, time.Second, time.Second, 300 * time.Second, false, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Date(2026, 11, 9, 14, 0, 0, 0, time.UTC)
			at := start
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stored := false
			err := e2bPollReady(ctx, tc.budget, start.Add(tc.expireAt), func() time.Time { return at },
				func(ctx context.Context) (map[string]any, error) {
					if tc.realDeadline {
						<-ctx.Done()
					}
					at = start.Add(tc.readAt)
					if tc.cancelRead {
						cancel()
					}
					// A live reader cannot extend the captured authoritative expiry.
					return map[string]any{"status": sandbox.StatusRunning, "shutdownTime": start.Add(time.Hour).Format(time.RFC3339)}, nil
				}, func(context.Context, map[string]any) { stored = true; at = start.Add(tc.storeAt) })
			if (err == nil) != tc.wantSuccess || stored != tc.wantStore {
				t.Fatalf("err=%v stored=%v, want success=%v store=%v", err, stored, tc.wantSuccess, tc.wantStore)
			}
		})
	}
}

func TestE2BColdAccessDeadline(t *testing.T) {
	for _, name := range []string{"ready", "late access", "expired access", "canceled access", "real deadline"} {
		t.Run(name, func(t *testing.T) {
			start := time.Date(2026, 11, 9, 14, 0, 0, 0, time.UTC)
			at := start
			budget, expiry := 120*time.Second, start.Add(300*time.Second)
			if name == "real deadline" {
				budget = time.Millisecond
			}
			if name == "expired access" {
				expiry = start.Add(75 * time.Second)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a, err := e2bColdAccess(ctx, budget, expiry, func() time.Time { return at },
				func(ctx context.Context) error { at = start.Add(70 * time.Second); return nil },
				func(ctx context.Context) (*sandbox.Access, error) {
					switch name {
					case "late access":
						at = start.Add(121 * time.Second)
					case "expired access":
						at = start.Add(80 * time.Second)
					case "canceled access":
						cancel()
					case "real deadline":
						<-ctx.Done()
					}
					return &sandbox.Access{}, nil
				})
			if name == "ready" {
				if err != nil || a == nil {
					t.Fatalf("ready: access=%v err=%v", a, err)
				}
			} else if err == nil || a != nil || (!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) {
				t.Fatalf("late access must not yield a 201 payload: access=%v err=%v", a, err)
			}
		})
	}
}
