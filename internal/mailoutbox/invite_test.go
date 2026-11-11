package mailoutbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

func inviteFixture() Invite {
	now := time.Date(2026, 11, 10, 0, 0, 0, 123456000, time.UTC)
	return Invite{InviteID: "0123456789abcdef01234567", Date: now, ExpiresAt: now.Add(24 * time.Hour), Recipient: Recipient{Key: "recipient-00", Address: "one@example.invalid"}, From: "Sender <sender@example.invalid>", Subject: "Project invitation", TextBody: "Join https://console.example.invalid/join-project?invite-token=PRIVATE_TOKEN", HTMLBody: "<a href=\"https://console.example.invalid/join-project?invite-token=PRIVATE_TOKEN\">Join</a>"}
}

func TestInviteFiniteContractPrivacyAndFrozenWire(t *testing.T) {
	in := inviteFixture()
	es, err := PrepareInvite(in)
	if err != nil || len(es) != 1 || es[0].To() != in.Recipient.Address {
		t.Fatal("single-recipient preparation", err)
	}
	again, _ := PrepareInvite(in)
	if !bytes.Equal(es[0].Wire(), again[0].Wire()) || !bytes.Contains(es[0].Wire(), []byte("multipart/alternative")) || bytes.Contains(es[0].Wire(), []byte("attachment")) {
		t.Fatal("not a fixed frozen wire envelope")
	}
	copy := es[0].Wire()
	copy[0] = '!'
	if es[0].Wire()[0] == '!' {
		t.Fatal("mutable wire")
	}
	var log bytes.Buffer
	slog.New(slog.NewJSONHandler(&log, nil)).Info("fixture", "invite", in, "envelope", es[0])
	raw, _ := json.Marshal(Status{ID: "opaque", State: Unknown, Reason: string(ClassPayload)})
	public := log.String() + string(raw) + fmt.Sprintf("%v %+v %#v", in, in, in)
	for _, private := range []string{"PRIVATE_TOKEN", "one@example", "sender@", "Project invitation", "/join-project", "TextBody"} {
		if strings.Contains(public, private) {
			t.Fatal("credential/PII leaked through public logging/status")
		}
	}
	for _, field := range []string{"Attachments", "Kind", "TemplateKey", "Vars", "Transport", "Job", "Recipients"} {
		if _, ok := reflect.TypeOf(in).FieldByName(field); ok {
			t.Fatal("generic surface", field)
		}
	}
	for _, mutate := range []func(*Invite){
		func(v *Invite) { v.InviteID = "token_PRIVATE_TOKEN" },
		func(v *Invite) { v.Recipient.Key = "one@example.invalid" },
		func(v *Invite) { v.Recipient.Address = "a@example.invalid,b@example.invalid" },
		func(v *Invite) { v.Subject = "secret\r\nBcc: a@example.invalid" },
		func(v *Invite) { v.From = "sender@example.invalid\r\nX: secret" },
		func(v *Invite) { v.Date = time.Time{} },
		func(v *Invite) { v.ExpiresAt = v.Date },
		func(v *Invite) { v.ExpiresAt = v.Date.Add(24*time.Hour + time.Microsecond) },
		func(v *Invite) { v.TextBody = strings.Repeat("x", MaxWireBytes) },
		func(v *Invite) { v.Subject = strings.Repeat("x", 513) },
		func(v *Invite) { v.Disposition = Accepted },
		func(v *Invite) { v.Disposition = NoRecipient },
	} {
		v := inviteFixture()
		mutate(&v)
		if _, err := PrepareInvite(v); err != ErrInvalid {
			t.Fatal("invalid fixed invite accepted", err)
		}
	}
	for _, state := range []string{Suppressed, Blocked, NoRecipient} {
		v := inviteFixture()
		v.From, v.Disposition = "", state
		if state == NoRecipient {
			v.Recipient.Address = ""
		}
		ps, err := prepareInvite(v)
		if err != nil || ps[0].state != state || len(ps[0].plaintext) == 0 {
			t.Fatal("non-delivery missing", err)
		}
	}
}

func TestInviteEnvelopeValueAndPointerFormattingRedacted(t *testing.T) {
	es, err := PrepareInvite(inviteFixture())
	if err != nil || len(es) != 1 {
		t.Fatal("invite envelope fixture unavailable")
	}
	for _, v := range []any{es[0], &es[0]} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			public := fmt.Sprintf(format, v)
			if public != "[mail envelope redacted]" {
				t.Fatal("envelope formatting must return only the redaction marker")
			}
			for _, private := range []string{"from", "to", "wire", "[]uint8", "0x", "PRIVATE_TOKEN", "one@example", "sender@", "invite-token"} {
				if strings.Contains(public, private) {
					t.Fatal("envelope formatting exposed a private field or byte representation")
				}
			}
		}
	}
}

func TestInviteIdentityAllInputsAndKindAAD(t *testing.T) {
	in := inviteFixture()
	ps, _ := prepareInvite(in)
	if ps[0].event != hash(InviteKind+"\x00"+in.InviteID) {
		t.Fatal("identity is not persistent document ID")
	}
	for _, mutate := range []func(*Invite){
		func(v *Invite) { v.Subject += " " }, func(v *Invite) { v.HTMLBody += " " },
		func(v *Invite) { v.TextBody += " " }, func(v *Invite) { v.From = "other@example.invalid" },
		func(v *Invite) { v.Recipient.Address = "two@example.invalid" }, func(v *Invite) { v.Recipient.Key += "1" },
		func(v *Invite) { v.Date = v.Date.Add(time.Nanosecond); v.ExpiresAt = v.ExpiresAt.Add(time.Nanosecond) },
		func(v *Invite) { v.ExpiresAt = v.ExpiresAt.Add(-time.Nanosecond) }, func(v *Invite) { v.Disposition = Blocked },
	} {
		v := in
		mutate(&v)
		other, err := prepareInvite(v)
		if err != nil || ps[0].event != other[0].event || bytes.Equal(ps[0].plaintext, other[0].plaintext) {
			t.Fatal("immutable input not fingerprinted", err)
		}
	}
	v := in
	v.InviteID = "1123456789abcdef01234567"
	other, _ := prepareInvite(v)
	if other[0].event == ps[0].event {
		t.Fatal("manual invites collapsed")
	}
	old, err := payloadAAD("row-id", Kind, Version)
	if err != nil || string(old) != "row-id\x00savings-reminder/v1\x001" || !bytes.Equal(old, aad("row-id")) {
		t.Fatal("legacy savings AAD changed")
	}
	newAAD, err := payloadAAD("row-id", InviteKind, Version)
	if err != nil || bytes.Equal(old, newAAD) || string(newAAD) != "row-id\x00project-invite/v1\x001" {
		t.Fatal("invite domain not separate")
	}
	for _, tc := range []struct {
		kind    string
		version int
	}{{"unknown/v1", 1}, {InviteKind, 2}, {Kind, 0}} {
		if _, err := payloadAAD("row-id", tc.kind, tc.version); err != ErrPayload {
			t.Fatal("unknown kind/version fallback")
		}
	}
}
