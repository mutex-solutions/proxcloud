package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
)

// memHolderStore is a deterministic DB-clock model for cancellation/renewal
// tests. Real PostgreSQL races and crash durability are tested separately.
type memHolderStore struct {
	mu                       sync.Mutex
	lease                    *lock.Lease
	now, until               time.Time
	pending                  *HolderOperation
	epoch                    int64
	renews, releases, begins int
	loseRenew, loseSave      bool
	renewed                  chan struct{}
}

func newMemHolder() *memHolderStore {
	return &memHolderStore{now: time.Unix(1, 0), renewed: make(chan struct{}, 100)}
}
func (m *memHolderStore) valid(x lock.Lease) bool {
	return m.lease != nil && *m.lease == x && m.now.Before(m.until)
}
func (m *memHolderStore) Acquire(_ context.Context, n string, ttl time.Duration) (*lock.Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lease != nil && m.now.Before(m.until) {
		return nil, nil
	}
	m.epoch++
	x := lock.Lease{Name: n, Owner: fmt.Sprint(m.epoch), Epoch: m.epoch}
	m.lease = &x
	m.until = m.now.Add(ttl)
	return &x, nil
}
func (m *memHolderStore) Renew(_ context.Context, x lock.Lease, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loseRenew || !m.valid(x) {
		return lock.ErrLeaseLost
	}
	m.renews++
	m.now = m.now.Add(20 * time.Second)
	m.until = m.now.Add(ttl)
	m.renewed <- struct{}{}
	return nil
}
func (m *memHolderStore) Check(_ context.Context, x lock.Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.valid(x) {
		return lock.ErrLeaseLost
	}
	if m.pending != nil {
		return holderPending(m.pending)
	}
	return nil
}
func (m *memHolderStore) Release(_ context.Context, x lock.Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.valid(x) {
		return lock.ErrLeaseLost
	}
	m.releases++
	m.until = m.now
	return nil
}
func (m *memHolderStore) Pending(_ context.Context, _ string) (*HolderOperation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		return nil, nil
	}
	p := *m.pending
	return &p, nil
}
func (m *memHolderStore) Begin(_ context.Context, x lock.Lease, p holderProof) (*HolderOperation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.valid(x) {
		return nil, lock.ErrLeaseLost
	}
	if m.pending != nil {
		return nil, holderPending(m.pending)
	}
	m.begins++
	op := &HolderOperation{ID: fmt.Sprint(m.begins), Scope: x.Name, Owner: x.Owner, Epoch: x.Epoch, State: "DISPATCHING", Proof: p}
	m.pending = op
	copy := *op
	return &copy, nil
}
func (m *memHolderStore) RecordUPID(_ context.Context, x lock.Lease, id string, up UPID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loseSave || !m.valid(x) {
		return errors.New("database partition after reply")
	}
	if m.pending == nil || m.pending.ID != id {
		return lock.ErrLeaseLost
	}
	m.pending.UPID = up
	m.pending.State = "OBSERVING"
	return nil
}
func (m *memHolderStore) Settle(_ context.Context, x lock.Lease, op *HolderOperation, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.valid(x) || m.pending == nil || m.pending.ID != op.ID {
		return lock.ErrLeaseLost
	}
	m.pending = nil
	return nil
}
func smallHolderBudgets() HolderBudgets {
	return HolderBudgets{Config: 30 * time.Millisecond, Migration: 40 * time.Millisecond, Settlement: 30 * time.Millisecond, Acquire: 10 * time.Millisecond, DB: time.Second, Renew: time.Hour}
}

func TestBoundedTaskObservation(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		want       TaskOutcome
	}{
		{"success", `{"data":{"status":"stopped","exitstatus":"OK"}}`, 200, TaskSucceeded},
		{"failed", `{"data":{"status":"stopped","exitstatus":"no"}}`, 200, TaskFailed},
		{"malformed stopped", `{"data":{"status":"stopped"}}`, 200, TaskUncertain},
		{"running", `{"data":{"status":"running"}}`, 200, TaskUncertain},
		{"forbidden", `{}`, 403, TaskUncertain}, {"missing", `{}`, 404, TaskUncertain},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, c := fakePVE(t, fakeRoutes{moveStatus: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tt.status); fmt.Fprint(w, tt.body) }})
			r := c.WaitTaskBounded(context.Background(), UPID(moveUPID), 20*time.Millisecond)
			if r.Outcome != tt.want || r.UPID != UPID(moveUPID) {
				t.Fatalf("%+v", r)
			}
		})
	}
}
func TestHolderUnknownNeverCompensates(t *testing.T) {
	for _, kind := range []string{"reply-lost", "upid-save-lost", "status403", "status404", "running", "wrong-identity"} {
		t.Run(kind, func(t *testing.T) {
			store := newMemHolder()
			store.loseSave = kind == "upid-save-lost"
			calls := 0
			routes := fakeRoutes{
				"GET /api2/json/pools/tp-p1":                 say(`{"data":{"poolid":"tp-p1","members":[{"id":"qemu/101","type":"qemu","node":"pve1","vmid":101},{"id":"qemu/900","type":"qemu","node":"pve1","vmid":900}]}}`),
				"GET " + carrierPath + "/config":             say(`{"data":{"smbios1":"uuid=carrier","tags":"pc-holder;proxcloud"}}`),
				"GET " + carrierPath + "/pending":            say(`{"data":[]}`),
				"GET /api2/json/nodes/pve1/qemu/101/pending": say(`{"data":[]}`),
				"GET /api2/json/nodes/pve1/qemu/101/config":  say(`{"data":{"smbios1":"uuid=expected"}}`),
				"DELETE " + carrierPath:                      func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprintf(w, `{"data":%q}`, moveUPID) },
				moveStatus:                                   say(`{"data":{"status":"running"}}`),
			}
			if kind == "status403" || kind == "status404" {
				status := 403
				if kind == "status404" {
					status = 404
				}
				routes[moveStatus] = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }
			}
			if kind == "wrong-identity" {
				routes[moveStatus] = say(`{"data":{"status":"stopped","exitstatus":"OK"}}`)
				routes["GET /api2/json/nodes/pve1/qemu/101/config"] = say(`{"data":{"smbios1":"uuid=foreign"}}`)
			}
			s := volCluster(t, cfgLocalUnused, map[string][]url.Values{}, routes)
			s.HolderOperations = store
			s.HolderBudgets = smallHolderBudgets()
			ctx, release, err := s.beginHolders(context.Background(), "p1")
			if err != nil {
				t.Fatal(err)
			}
			p := holderProof{Kind: "config", Pool: "tp-p1", Source: holderVMIdentity{Node: "pve1", VMID: 101, UUID: "uuid=expected"}}
			dispatches := 0
			err = s.holderTask(ctx, p, time.Second, func(q context.Context) (UPID, error) {
				dispatches++
				if store.pending == nil {
					t.Error("POST before durable intent")
				}
				if kind == "reply-lost" {
					return "", errors.New("reply lost")
				}
				return UPID(moveUPID), nil
			})
			if err == nil {
				t.Fatal("uncertain operation succeeded")
			}
			// Models ALL cleanup defers, including ones using WithoutCancel.
			s.dropEmptyHolder(context.WithoutCancel(ctx), &ClusterResource{Node: "pve1", VMID: 900})
			release()
			if calls != 0 || store.releases != 0 || dispatches != 1 || store.pending == nil {
				t.Fatalf("destructive=%d releases=%d dispatch=%d pending=%+v", calls, store.releases, dispatches, store.pending)
			}
			store.mu.Lock()
			store.now = store.now.Add(24 * time.Hour)
			store.mu.Unlock()
			// A new owner can poll known work but cannot resubmit or destroy, even
			// well past the OLD ten-minute ceiling.
			_, _, err = s.beginHolders(context.Background(), "p1")
			if err == nil {
				t.Fatal("TTL cleared pending")
			}
			if calls != 0 || dispatches != 1 {
				t.Fatal("recovery mutated PVE")
			}
		})
	}
}
func TestRenewalBeyondTenMinutesAndWithoutCancel(t *testing.T) {
	m := newMemHolder()
	s := &Service{HolderOperations: m, HolderBudgets: HolderBudgets{Renew: time.Millisecond}}
	ctx, release, err := s.beginHolders(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 35; i++ {
		select {
		case <-m.renewed:
		case <-time.After(time.Second):
			t.Fatal("renew goroutine stalled")
		}
	}
	if err = s.holderCheck(ctx); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	if m.now.Before(time.Unix(1, 0).Add(10 * time.Minute)) {
		t.Fatal("fake clock did not pass ten minutes")
	}
	m.loseRenew = true
	m.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("renew loss did not cancel work")
	}
	if s.holderCheck(context.WithoutCancel(ctx)) == nil {
		t.Fatal("WithoutCancel revived a retired owner")
	}
	release()
	if m.releases != 0 {
		t.Fatal("lost owner released a lease")
	}
	select {
	case <-holderFrom(ctx).done:
	default:
		t.Fatal("renew goroutine leaked")
	}
}
func TestKnownHolderRecoveryPollOnly(t *testing.T) {
	m := newMemHolder()
	p := holderProof{Kind: "config", Pool: "tp-p1", Source: holderVMIdentity{Node: "pve1", VMID: 900, UUID: "uuid=owned"}, Params: map[string]string{"name": "pc-holder-pve1"}}
	m.pending = &HolderOperation{ID: "existing", Scope: "pve-holders/svc/tp-p1", UPID: UPID(moveUPID), Proof: p}
	polls := 0
	_, c := fakePVE(t, fakeRoutes{
		moveStatus: func(w http.ResponseWriter, r *http.Request) {
			polls++
			fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
		},
		"GET /api2/json/pools/tp-p1":      say(`{"data":{"poolid":"tp-p1","members":[{"id":"qemu/900","type":"qemu","node":"pve1","vmid":900}]}}`),
		"GET " + carrierPath + "/config":  say(`{"data":{"smbios1":"uuid=owned","name":"pc-holder-pve1"}}`),
		"GET " + carrierPath + "/pending": say(`{"data":[]}`),
	})
	s := &Service{C: c, PoolPrefix: "tp-", ServiceID: "svc", HolderOperations: m, HolderBudgets: smallHolderBudgets()}
	if err := s.PollHolderOperation(context.Background(), "p1", "existing"); err != nil {
		t.Fatal(err)
	}
	if m.pending != nil || polls != 1 || m.begins != 0 {
		t.Fatalf("pending=%v polls=%d dispatch=%d", m.pending, polls, m.begins)
	}
}
func TestFencedEntrypointsRefusePending(t *testing.T) {
	for name, run := range map[string]func(*Service) error{
		"new holder": func(s *Service) error { _, e := s.newHolder(context.Background(), "p1", "pve1", tagHolder); return e },
		"create": func(s *Service) error {
			_, e := s.CreateDetachedVolume(context.Background(), "p1", "pve1", "nas", 1, 0)
			return e
		},
		"add":             func(s *Service) error { _, e := s.AddVolume(context.Background(), "p1", "101", 1, "nas", 0); return e },
		"extend":          func(s *Service) error { return s.ExtendVolume(context.Background(), "p1", 101, "disk", 2, 10) },
		"detach":          func(s *Service) error { return s.DetachVolume(context.Background(), "p1", 101, "disk") },
		"attach":          func(s *Service) error { return s.AttachVolume(context.Background(), "p1", 101, "disk", "102") },
		"delete volume":   func(s *Service) error { return s.DeleteVolume(context.Background(), "p1", 101, "disk") },
		"retype":          func(s *Service) error { return s.RetypeVolume(context.Background(), "p1", 101, "disk", "nas", 0) },
		"delete server":   func(s *Service) error { return s.DeleteServer(context.Background(), "p1", "101") },
		"retire server":   func(s *Service) error { return s.RetireServer(context.Background(), "p1", "101", "pbs", true) },
		"project backups": func(s *Service) error { return s.DeleteProjectBackups(context.Background(), "p1", "pbs") },
		"teardown":        func(s *Service) error { return s.TeardownProjectPool(context.Background(), "tp-p1") },
		"blank":           func(s *Service) error { _, e := s.RemoveBlankVMs(context.Background(), "p1"); return e },
		"keep":            func(s *Service) error { return s.KeepServerVolumes(context.Background(), "p1", "101") },
	} {
		t.Run(name, func(t *testing.T) {
			m := newMemHolder()
			m.pending = &HolderOperation{ID: "unknown", State: "DISPATCHING"}
			writes := map[string][]url.Values{}
			s := volCluster(t, cfgLocalUnused, writes)
			s.HolderOperations = m
			s.HolderBudgets = smallHolderBudgets()
			if e := run(s); e == nil {
				t.Fatal("pending operation bypassed")
			}
			if len(writes) != 0 {
				t.Fatalf("writes=%v", writes)
			}
		})
	}
}
