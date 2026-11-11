package mailoutbox

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// FakeTransport is an explicit local-test capability, not a feature checkbox.
// The production SMTP adapter does NOT assert LoopbackTestOnly. This PR has no
// production activation API; enabling live delivery needs a separately reviewed
// business-coupling/rollout change, not the presence of a configured SMTP relay.
type FakeTransport interface {
	Transport
	LoopbackTestOnly() bool
}
type Dispatcher struct {
	store     *Store
	transport Transport
	enabled   bool
	ttl       time.Duration
}

func NewDispatcher(s *Store) *Dispatcher { return &Dispatcher{store: s, ttl: time.Minute} }
func (d *Dispatcher) Enabled() bool      { return d != nil && d.enabled }

// StartWithFakeTransport opts into dispatch ONLY for deterministic local tests.
// No background worker is registered by construction; call RunOnce explicitly.
func (d *Dispatcher) StartWithFakeTransport(t FakeTransport) error {
	if t == nil || !t.LoopbackTestOnly() {
		return ErrDisabled
	}
	d.transport = t
	d.enabled = true
	return nil
}

// RunOnce keeps T1/T2/T3 short, with NO SQL transaction around network I/O. If
// receipt persistence fails, the intent remains DISPATCHING then UNKNOWN: never
// turn an ACK error into a fresh send. A panic/process death is equally safe.
func (d *Dispatcher) RunOnce(ctx context.Context) (bool, error) {
	if !d.Enabled() {
		return false, ErrDisabled
	}
	c, err := d.store.Claim(ctx, d.ttl)
	if err != nil || c == nil {
		return false, err
	}
	e, err := d.store.BeginDispatch(ctx, c)
	if err != nil {
		if err == ErrPayload {
			return true, d.store.BlockClaim(ctx, c, ClassPayload)
		}
		return true, err
	}
	netCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Renewals never hold a transaction across Deliver. If renewal fails, abort
	// the socket context; possible end-of-DATA still has unknown semantics.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		timer := time.NewTicker(d.ttl / 3)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-netCtx.Done():
				return
			case <-timer.C:
				if d.store.Renew(netCtx, c, d.ttl) != nil {
					cancel()
					return
				}
			}
		}
	}()
	r := d.transport.Deliver(netCtx, e)
	close(stop)
	wg.Wait()
	if !r.valid() {
		r = Receipt{Delivery: Indeterminate, Stage: StageReply, Class: ClassUncertain}
	}
	// Cancellation of the network request must not discard its known receipt.
	ackCtx, ackCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer ackCancel()
	return true, d.store.RecordOutcome(ackCtx, c, r)
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	base := time.Duration(1<<uint(attempt-1)) * time.Second
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return base
	}
	// Full positive jitter bounded by twice the exponential base (<=256s).
	return base + time.Duration(binary.BigEndian.Uint64(raw[:])%uint64(base))
}
