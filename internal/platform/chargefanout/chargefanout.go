// Package chargefanout adapts native billing to primary PostgreSQL durable jobs.
// One frozen original-cycle message per profile; no broker and no processing-time
// cutoff. Publishing retries are safe, including a partially published profile list.
package chargefanout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/platform/billingjob"
)

type ProfileLister interface {
	ActiveProfileIDs(context.Context) ([]string, error)
}
type Charger interface {
	ChargeProfileByID(context.Context, string, string, time.Time) error
}
type Enqueuer interface {
	Enqueue(context.Context, string, string, []byte, int) (string, error)
}

func Publish(ctx context.Context, q Enqueuer, lister ProfileLister, unit string, original time.Time) (int, error) {
	// Validate cadence even with an empty profile list.
	if _, err := billingjob.NewCycle("validation", unit, original); err != nil {
		return 0, err
	}
	ids, err := lister.ActiveProfileIDs(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		cycle, err := billingjob.NewCycle(id, unit, original)
		if err != nil {
			return n, err
		}
		body, err := json.Marshal(cycle)
		if err != nil {
			return n, err
		}
		if _, err = q.Enqueue(ctx, durablejobs.Billing, cycle.Key(), body, 8); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
func strict(body []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return durablejobs.ErrPayload
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return durablejobs.ErrPayload
	}
	return nil
}
func Decode(j *durablejobs.Job) (billingjob.Cycle, error) {
	var c billingjob.Cycle
	if j.Kind != durablejobs.Billing || strict(j.Payload, &c) != nil || c.Validate() != nil || c.Key() != j.Key {
		return c, durablejobs.ErrPayload
	}
	return c, nil
}
func Handler(charger Charger) durablejobs.Handler {
	return func(ctx context.Context, j *durablejobs.Job) error {
		c, err := Decode(j)
		if err != nil {
			return err
		}
		return charger.ChargeProfileByID(ctx, c.ProfileID, c.TimeUnit, c.CycleAt)
	}
}

type probe struct {
	Version int    `json:"version"`
	Nonce   string `json:"nonce"`
}

func Validate(j *durablejobs.Job) error {
	switch j.Kind {
	case durablejobs.Billing:
		_, err := Decode(j)
		return err
	case durablejobs.Selftest:
		var p probe
		if strict(j.Payload, &p) != nil || p.Version != 1 || p.Nonce != j.Key {
			return durablejobs.ErrPayload
		}
		if _, err := uuid.Parse(p.Nonce); err != nil {
			return durablejobs.ErrPayload
		}
		return nil
	default:
		return durablejobs.ErrPayload
	}
}
func SelftestHandler(_ context.Context, j *durablejobs.Job) error { return Validate(j) }

// Selftest exercises the running worker's real enqueue->claim->DB ACK path. It
// touches no billing data and can be executed with scheduler disabled.
func Selftest(ctx context.Context, q *durablejobs.Store) (string, error) {
	nonce := uuid.NewString()
	body, _ := json.Marshal(probe{Version: 1, Nonce: nonce})
	id, err := q.Enqueue(ctx, durablejobs.Selftest, nonce, body, 3)
	if err != nil {
		return "", err
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := q.Get(ctx, id)
		if err != nil {
			return id, err
		}
		switch j.State {
		case "succeeded":
			return id, nil
		case "dead":
			return id, fmt.Errorf("selftest job is dead")
		}
		select {
		case <-ctx.Done():
			return id, ctx.Err()
		case <-tick.C:
		}
	}
}
