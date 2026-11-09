package mailoutbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func reminderFixture() Reminder {
	return Reminder{ContractID: "contract_1", ExpiryGeneration: "end_1", WindowDays: 7, EndDate: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), Date: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), Recipients: []Recipient{{Key: "user_1", Address: "One@EXAMPLE.invalid"}}, From: "Example Billing <billing@example.invalid>", Subject: "An immutable reminder ✓", TextBody: "private body", HTMLBody: "<b>private body</b>"}
}
func TestPrepareImmutableSingleRecipientAndBounds(t *testing.T) {
	r := reminderFixture()
	r.Recipients = append(r.Recipients, Recipient{Key: "user_2", Address: "two@example.invalid"})
	es, err := PrepareReminder(r)
	if err != nil || len(es) != 2 {
		t.Fatal(err)
	}
	if es[0].To() != "One@example.invalid" {
		t.Fatal("domain not normalized")
	}
	for _, e := range es {
		wire := e.Wire()
		if !bytes.Contains(wire, []byte("Message-ID: <")) || !bytes.Contains(wire, []byte("Date: Mon, 09 Nov 2026 00:00:00 +0000")) {
			t.Fatal("frozen headers missing")
		}
		wire[0] = '!'
		if e.Wire()[0] == '!' {
			t.Fatal("wire is mutable")
		}
	}
	again, _ := PrepareReminder(r)
	if !bytes.Equal(es[0].Wire(), again[0].Wire()) {
		t.Fatal("wire not stable")
	}
	if bytes.Equal(es[0].Wire(), es[1].Wire()) {
		t.Fatal("per-recipient identity shared")
	}
	for _, mutate := range []func(*Reminder){
		func(r *Reminder) { r.Subject = "private\r\nBcc: leak@example.invalid" },
		func(r *Reminder) { r.From = "x@example.invalid\r\nBcc: leak@example.invalid" },
		func(r *Reminder) { r.Recipients[0].Address = "one@example.invalid,two@example.invalid" },
		func(r *Reminder) { r.Recipients[0].Key = "one@example.invalid" },
		func(r *Reminder) { r.HTMLBody = strings.Repeat("x", MaxWireBytes) },
		func(r *Reminder) {
			for i := 0; i < 21; i++ {
				r.Recipients = append(r.Recipients, Recipient{Key: fmt.Sprint(i), Address: fmt.Sprintf("%d@example.invalid", i)})
			}
		},
		func(r *Reminder) { r.Disposition = "accepted" },
	} {
		r := reminderFixture()
		mutate(&r)
		if _, err = PrepareReminder(r); err == nil {
			t.Fatal("invalid preparation allowed")
		}
	}
	r = reminderFixture()
	r.Recipients = append(r.Recipients, r.Recipients[0])
	if _, err = PrepareReminder(r); err == nil {
		t.Fatal("duplicate recipient")
	}
	r = reminderFixture()
	r.Recipients = nil
	if p, err := prepare(r); err != nil || p[0].state != NoRecipient {
		t.Fatal("missing recipient not explicit")
	}
	r = reminderFixture()
	r.Disposition = Suppressed
	if p, err := prepare(r); err != nil || p[0].state != Suppressed {
		t.Fatal("suppression not explicit")
	}
}
func TestNoPIIStatusAndLogValues(t *testing.T) {
	r := reminderFixture()
	es, _ := PrepareReminder(r)
	var b bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&b, nil))
	l.Info("opaque", "dto", r, "envelope", es[0])
	raw, _ := json.Marshal(Status{ID: "opaque", State: Unknown, Reason: string(ClassUncertain)})
	for _, private := range []string{"One@", "billing@", "private body", "immutable reminder"} {
		if strings.Contains(b.String()+string(raw), private) {
			t.Fatal("PII leaked")
		}
	}
	if (Receipt{Delivery: RelayAccepted, Stage: StageConnect, Class: ClassOK}).valid() {
		t.Fatal("accepted before DATA reply")
	}
	if (Receipt{Delivery: NotAccepted, Stage: StageReply, Class: "raw SMTP PII"}).valid() {
		t.Fatal("raw error accepted")
	}
	d := NewDispatcher(nil)
	if d.Enabled() {
		t.Fatal("default dispatch on")
	}
	if _, err := d.RunOnce(t.Context()); err != ErrDisabled {
		t.Fatal("disabled runner")
	}
}
