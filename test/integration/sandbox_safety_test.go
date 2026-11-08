//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// Persistence + audit are atomic: parallel replicas flag the same identity once;
// restarting the process (reload observation) does not forget sustained evidence.
func TestSandboxSafetyDurableConsecutiveWindowAndAudit(t *testing.T) {
	db := freshPG(t)
	ctx := context.Background()
	store := db.C("sandbox_safety")
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.C("auditEvent").Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	ar := audit.NewService(audit.NewRepo(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()
	v := uint64(0)
	rx := uint64(0)
	s := sandbox.Sample{ProjectID: "p", SandboxID: "sbx-1234567890123456", CRUID: "cr", PodUID: "pod", Node: "n", At: now, CPUAt: now, NetworkAt: now, CPU: &v, RX: &rx, TX: &rx, Cores: 1}
	o, _ := sandbox.Observation{}.Next(s, sandbox.AbusePolicy{WindowSeconds: 60}, now)
	if err := store.Upsert(ctx, "key", o); err != nil {
		t.Fatal(err)
	}
	s.At = now.Add(time.Minute)
	s.CPUAt = s.At
	s.NetworkAt = s.At
	v = 60e9
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			err := db.WithTx(ctx, func(tc context.Context) error {
				if e := db.LockXact(tc, "safety:key"); e != nil {
					return e
				}
				var cur sandbox.Observation
				if _, e := store.GetForUpdate(tc, "key", &cur); e != nil {
					return e
				}
				next, flag := cur.Next(s, sandbox.AbusePolicy{WindowSeconds: 60}, s.At)
				if e := store.Upsert(tc, "key", next); e != nil {
					return e
				}
				if !flag {
					return nil
				}
				ev := audit.SystemEvent()
				ev.ResourceType = "SANDBOX_ABUSE"
				ev.ProjectID = "p"
				ev.Action = "DETECTED"
				return ar.Log(tc, ev)
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	n, e := ar.Count(ctx, audit.Filter{ResourceType: "SANDBOX_ABUSE", ProjectID: "p"})
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
	var persisted sandbox.Observation
	if _, e := store.Get(ctx, "key", &persisted); e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(persisted)
	if len(b) == 0 {
		t.Fatal("no state")
	}
	if !persisted.Flagged {
		t.Fatal("parallel sample reset flagged observation")
	}
	// Advisory capture locks skip automation across connections without wrapping capture progress in an SQL tx.
	_, e = db.WithAdvisoryLock(ctx, "capture", false, func() error {
		ok, err := db.WithAdvisoryLock(ctx, "capture", true, func() error { t.Error("busy capture entered"); return nil })
		if ok || err != nil {
			t.Errorf("try lock %v %v", ok, err)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	_ = pgdoc.M{}
}
