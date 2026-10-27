package chargefanout

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type fakePub struct {
	queue string
	msgs  [][]byte
}

func (f *fakePub) Publish(_ context.Context, queue string, body []byte) error {
	f.queue = queue
	f.msgs = append(f.msgs, body)
	return nil
}

type fakeLister struct{ ids []string }

func (f fakeLister) ActiveProfileIDs(context.Context) ([]string, error) { return f.ids, nil }

type fakeConsumer struct{ handler func([]byte) error }

func (f *fakeConsumer) Consume(_ string, handler func([]byte) error) (func() error, error) {
	f.handler = handler
	return func() error { return nil }, nil
}

type fakeCharger struct{ calls []Msg }

func (f *fakeCharger) ChargeProfileByID(_ context.Context, id, timeUnit string, _ time.Time) error {
	f.calls = append(f.calls, Msg{ProfileID: id, TimeUnit: timeUnit})
	return nil
}

// TestPublishFansOutOnePerProfile: one message per ACTIVE profile, to the charge queue.
func TestPublishFansOutOnePerProfile(t *testing.T) {
	pub := &fakePub{}
	n, err := Publish(context.Background(), pub, fakeLister{ids: []string{"p1", "p2", "p3"}}, "minute")
	if err != nil || n != 3 {
		t.Fatalf("Publish = %d (err %v), want 3", n, err)
	}
	if pub.queue != Queue {
		t.Fatalf("queue = %q, want %q", pub.queue, Queue)
	}
	if len(pub.msgs) != 3 {
		t.Fatalf("published %d msgs, want 3", len(pub.msgs))
	}
	var m Msg
	if err := json.Unmarshal(pub.msgs[0], &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.ProfileID != "p1" || m.TimeUnit != "minute" {
		t.Fatalf("msg[0] = %+v, want {p1 minute}", m)
	}
}

// TestConsumerDecodesAndCharges: a message → ChargeProfileByID with the decoded fields.
func TestConsumerDecodesAndCharges(t *testing.T) {
	con := &fakeConsumer{}
	charger := &fakeCharger{}
	if _, err := StartConsumer(con, charger, nil); err != nil {
		t.Fatalf("StartConsumer: %v", err)
	}
	body, _ := json.Marshal(Msg{ProfileID: "bp9", TimeUnit: "hour"})
	if err := con.handler(body); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(charger.calls) != 1 || charger.calls[0].ProfileID != "bp9" || charger.calls[0].TimeUnit != "hour" {
		t.Fatalf("charger calls = %+v, want one {bp9 hour}", charger.calls)
	}
	// a bad message surfaces an error (→ nack) and does not charge.
	if err := con.handler([]byte("not-json")); err == nil {
		t.Fatalf("bad message should error")
	}
	if len(charger.calls) != 1 {
		t.Fatalf("bad message must not charge: %+v", charger.calls)
	}
}
