package durablejobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

// Handler is deliberately DB-only: all effects and ACK share Complete's primary
// transaction. No SMTP/HTTP/subprocess handlers are registered by this PR.
type Handler func(context.Context, *Job) error

var ErrPayload = errors.New("invalid durable job payload")

type Worker struct {
	Store       *Store
	Handlers    map[string]Handler
	Concurrency int
	Poll        time.Duration
	Lease       time.Duration
	Log         *slog.Logger
}

// Run has bounded concurrency and a timer after empty/error polls; no goroutine
// per queued row and no hot empty loop. Cancellation stops claims and SQL work;
// any unacknowledged acquisition is recovered after its DB-clock lease expires.
func (w *Worker) Run(ctx context.Context) error {
	if w.Concurrency < 1 || w.Concurrency > 16 || w.Poll < time.Millisecond || w.Lease <= 0 {
		return errors.New("invalid worker configuration")
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	kinds := make([]string, 0, len(w.Handlers))
	for kind, handler := range w.Handlers {
		if !known(kind) || External(kind) || handler == nil {
			return errors.New("invalid handler registration")
		}
		kinds = append(kinds, kind)
	}
	var group sync.WaitGroup
	for i := 0; i < w.Concurrency; i++ {
		group.Go(func() {
			for ctx.Err() == nil {
				job, err := w.Store.ClaimKinds(ctx, w.Lease, kinds)
				if err == nil && job != nil {
					w.perform(ctx, job)
					continue
				}
				if err != nil && ctx.Err() == nil {
					w.Log.Warn("job claim failed", "err", err)
				}
				timer := time.NewTimer(w.Poll + time.Duration(rand.Int64N(max(int64(w.Poll/5), 1))))
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		})
	}
	group.Wait()
	return nil
}

func (w *Worker) perform(ctx context.Context, j *Job) {
	// Transaction must finish before the lease; context cancellation plus final
	// DB-time CAS guarantees a timeout cannot commit unfenced money. No heartbeat
	// is needed while this DB-only transaction holds the job row. Renew is exposed
	// for future separately-reviewed pre-transaction resolution, not external I/O.
	work, cancel := context.WithTimeout(ctx, w.Lease*3/4)
	defer cancel()
	err := w.Store.Complete(work, j, func(tc context.Context) (err error) {
		defer func() {
			if recover() != nil {
				err = fmt.Errorf("handler panic")
			}
		}()
		h := w.Handlers[j.Kind]
		if h == nil {
			return ErrPayload
		}
		return h(tc, j)
	})
	if err == nil {
		return
	}
	if ctx.Err() != nil || errors.Is(err, ErrLeaseLost) {
		return
	}
	// Do not persist arbitrary handler errors: provider/DB errors could contain
	// secrets. The queue stores a fixed classification and logs ID/kind only.
	w.Log.Warn("durable job failed", "id", j.ID, "kind", j.Kind, "attempt", j.Attempts)
	retry, done := context.WithTimeout(ctx, 5*time.Second)
	defer done()
	if e := w.Store.Retry(retry, j, Backoff(j.Attempts), errors.Is(err, ErrPayload)); e != nil && !errors.Is(e, ErrLeaseLost) {
		w.Log.Warn("job retry failed", "id", j.ID)
	}
}

// Backoff has bounded exponential delay with jitter, max 15 minutes. Attempts
// are bounded separately in the row so permanent poison never loops forever.
func Backoff(attempt int) time.Duration {
	base := time.Second * time.Duration(1<<min(max(attempt-1, 0), 9))
	return base + time.Duration(rand.Int64N(int64(base/2)+1))
}
