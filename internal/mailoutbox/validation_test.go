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

func validationFixture() ValidationApproved {
	now := time.Date(2026, 11, 9, 12, 0, 0, 123456000, time.UTC)
	return ValidationApproved{ValidationID: "0123456789abcdef01234567", ApprovalGeneration: "12345678-1234-4234-8234-123456789abc", Date: now, ExpiresAt: now.Add(ValidationApprovalMaxAge), Recipients: []string{"MixedCaseLocal@EXAMPLE.invalid", "other@example.invalid", "MixedCaseLocal@example.invalid"}, FullName: "PRIVATE_NAME", LoginURL: "https://console.example.invalid/login", From: "Sender <sender@example.invalid>", Subject: "Validation approval", TextBody: "Your validation is approved. Activation is separate.", HTMLBody: "<p>Validation approved.</p>"}
}
func TestValidationApprovedFixedContractRecipientsAndBinding(t *testing.T) {
	in := validationFixture()
	ps, err := prepareValidation(in)
	if err != nil || len(ps) != 2 {
		t.Fatal(err, len(ps))
	}
	es, err := PrepareValidationApproved(in)
	if err != nil || len(es) != 2 {
		t.Fatal(err)
	}
	if es[0].To() != "MixedCaseLocal@example.invalid" {
		t.Fatal("local part case changed", es[0].To())
	}
	if strings.Count(string(es[0].Wire()), "\r\nTo:") != 1 || strings.Contains(string(es[0].Wire()), "other@example.invalid") {
		t.Fatal("recipient leak")
	}
	if ps[0].event != hash(ValidationApprovedKind+"\x00"+in.ValidationID+"\x00"+in.ApprovalGeneration) || ps[0].recipient != hash("recipient-00") {
		t.Fatal("identity uses PII")
	}
	for _, p := range ps {
		if !validValidationPayload(p.plaintext, p.event, p.recipient, p.expiry) {
			t.Fatal("canonical payload denied")
		}
	}
	for _, change := range []func(*ValidationApproved){func(v *ValidationApproved) { v.Subject += "x" }, func(v *ValidationApproved) { v.FullName += "x" }, func(v *ValidationApproved) { v.LoginURL += "/" }, func(v *ValidationApproved) { v.From = "other@example.invalid" }, func(v *ValidationApproved) { v.Recipients = append(v.Recipients, "new@example.invalid") }, func(v *ValidationApproved) {
		v.Date = v.Date.Add(time.Nanosecond)
		v.ExpiresAt = v.ExpiresAt.Add(time.Nanosecond)
	}} {
		changed := in
		change(&changed)
		next, err := prepareValidation(changed)
		if err != nil || bytes.Equal(ps[0].plaintext, next[0].plaintext) {
			t.Fatal("immutable input not bound", err)
		}
	}
	var object map[string]any
	_ = json.Unmarshal(ps[0].plaintext, &object)
	object["unknown"] = "ignored?"
	raw, _ := json.Marshal(object)
	if validValidationPayload(raw, ps[0].event, ps[0].recipient, ps[0].expiry) || validValidationPayload(ps[0].plaintext, strings.Repeat("a", 64), ps[0].recipient, ps[0].expiry) || validValidationPayload(ps[0].plaintext, ps[0].event, hash("recipient-19"), ps[0].expiry) {
		t.Fatal("drift accepted")
	}
	var log bytes.Buffer
	slog.New(slog.NewJSONHandler(&log, nil)).Info("dto", "input", in, "envelope", es[0])
	for _, s := range []string{fmt.Sprint(in), fmt.Sprintf("%+v %#v", in, in), log.String(), fmt.Sprintf("%#v", es[0])} {
		for _, pii := range []string{"PRIVATE_NAME", "MixedCaseLocal", "sender@example.invalid", "console.example.invalid"} {
			if strings.Contains(s, pii) {
				t.Fatal("redaction leaked")
			}
		}
	}
}
func TestValidationApprovedBoundsAndExplicitNonDelivery(t *testing.T) {
	for _, change := range []func(*ValidationApproved){
		func(v *ValidationApproved) { v.ValidationID = "email@example.invalid" }, func(v *ValidationApproved) { v.ApprovalGeneration = "not-uuid" }, func(v *ValidationApproved) { v.ApprovalGeneration = "00000000-0000-0000-0000-000000000000" }, func(v *ValidationApproved) { v.ExpiresAt = v.ExpiresAt.Add(time.Nanosecond) }, func(v *ValidationApproved) { v.Date = time.Time{} }, func(v *ValidationApproved) { v.Subject = "bad\nsubject" }, func(v *ValidationApproved) { v.FullName = "bad\nname" }, func(v *ValidationApproved) { v.HTMLBody = strings.Repeat("x", MaxWireBytes+1) }, func(v *ValidationApproved) { v.Disposition = Accepted }, func(v *ValidationApproved) { v.Recipients = []string{"bad"} }, func(v *ValidationApproved) { v.Recipients = []string{""} }, func(v *ValidationApproved) {
			v.Recipients = []string{"valid@example.invalid", "bad"}
			v.Disposition = Suppressed
		}, func(v *ValidationApproved) { v.Recipients = make([]string, MaxRecipients+1) }, func(v *ValidationApproved) { v.LoginURL = "https://u:p@example.invalid" }, func(v *ValidationApproved) { v.LoginURL = "https://example.invalid/?token=x" }, func(v *ValidationApproved) { v.LoginURL = "https://example.invalid/#reset" }, func(v *ValidationApproved) { v.LoginURL = "mailto:x@example.invalid" },
	} {
		v := validationFixture()
		change(&v)
		if _, err := prepareValidation(v); err == nil {
			t.Fatalf("invalid accepted: %v", v)
		}
	}
	for _, state := range []string{Suppressed, Blocked, NoRecipient} {
		v := validationFixture()
		v.Disposition = state
		v.From = ""
		if state == NoRecipient {
			v.Recipients = nil
		}
		ps, err := prepareValidation(v)
		if err != nil || ps[0].state != state {
			t.Fatal(state, err)
		}
	}
	v := validationFixture()
	v.Recipients = nil
	v.From = ""
	ps, err := prepareValidation(v)
	if err != nil || len(ps) != 1 || ps[0].state != NoRecipient {
		t.Fatal("empty set not explicit", err)
	}
	for _, pair := range []struct {
		k string
		v int
	}{{ValidationApprovedKind, 2}, {"unknown/v1", 1}, {"", 1}} {
		if _, err := payloadAAD("id", pair.k, pair.v); err != ErrPayload {
			t.Fatal("kind/version fallback")
		}
	}
	if a, _ := payloadAAD("id", Kind, Version); !bytes.Equal(a, []byte("id\x00savings-reminder/v1\x001")) {
		t.Fatal("savings AAD changed")
	}
	if a, _ := payloadAAD("id", InviteKind, Version); !bytes.Equal(a, []byte("id\x00project-invite/v1\x001")) {
		t.Fatal("invite AAD changed")
	}
}
