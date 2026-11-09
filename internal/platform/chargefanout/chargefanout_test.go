package chargefanout

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
)

type fakeQueue struct{ jobs []*durablejobs.Job }

func (f *fakeQueue) Enqueue(_ context.Context, kind, key string, payload []byte, _ int) (string, error) {
	f.jobs = append(f.jobs, &durablejobs.Job{Kind: kind, Key: key, Payload: payload})
	return key, nil
}

type fakeLister []string

func (f fakeLister) ActiveProfileIDs(context.Context) ([]string, error) { return f, nil }

type fakeCharger struct {
	at       time.Time
	id, unit string
}

func (f *fakeCharger) ChargeProfileByID(_ context.Context, id, unit string, at time.Time) error {
	f.at = at
	f.id = id
	f.unit = unit
	return nil
}
func TestOriginalCutoffSurvivesDelayedWorker(t *testing.T) {
	q := &fakeQueue{}
	at := time.Date(2026, 10, 31, 23, 59, 42, 0, time.UTC)
	n, err := Publish(context.Background(), q, fakeLister{"p1", "p2"}, "minute", at)
	if err != nil || n != 2 {
		t.Fatalf("Publish=%d %v", n, err)
	}
	charger := &fakeCharger{}
	if err = Handler(charger)(context.Background(), q.jobs[0]); err != nil {
		t.Fatal(err)
	}
	if charger.id != "p1" || charger.unit != "minute" || !charger.at.Equal(at.Truncate(time.Minute)) {
		t.Fatalf("processing time replaced original: %+v", charger)
	}
	q.jobs[0].Key = "p2|hour|arbitrary"
	if Handler(charger)(context.Background(), q.jobs[0]) == nil {
		t.Fatal("identity/payload mismatch accepted")
	}
}
func TestRejectPoison(t *testing.T) {
	for _, body := range []string{`not-json`, `{}`, `{"version":1,"profileId":"p1","timeUnit":"hour","cycleAt":"2026-10-15T12:00:00Z","command":"rm"}`} {
		if _, err := Decode(&durablejobs.Job{Kind: durablejobs.Billing, Payload: json.RawMessage(body)}); err == nil {
			t.Fatalf("poison accepted: %s", body)
		}
	}
}
