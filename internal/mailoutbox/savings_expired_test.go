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

func expiredFixture() SavingsExpired {
	now := time.Date(2026, 11, 9, 12, 0, 0, 123456000, time.UTC)
	end := now.Add(-time.Hour)
	return SavingsExpired{ContractID: "0123456789abcdef01234567", TermGeneration: hash(end.Format(time.RFC3339Nano)), CanonicalEndDate: end, Date: now, ExpiresAt: now.Add(SavingsExpiredMaxAge), Recipients: []string{"MixedCaseLocal@EXAMPLE.invalid", "other@example.invalid", "mixedcaselocal@example.invalid"}, FullName: "PRIVATE_NAME", SavingsPlanName: "PRIVATE_PLAN", Currency: "USD", StartDate: "2026-10-01", MonthlyCommittedAmount: "10.00", From: "Sender <sender@example.invalid>", Subject: "Contract expired", TextBody: "Your contract expired.", HTMLBody: "<p>Expired.</p>"}
}
func TestSavingsExpiredCanonicalTermRecipientsWireAndRedaction(t *testing.T) {
	in := expiredFixture()
	ps, err := prepareSavingsExpired(in)
	if err != nil || len(ps) != 2 {
		t.Fatal(err, len(ps))
	}
	env, err := PrepareSavingsExpired(in)
	if err != nil || len(env) != 2 {
		t.Fatal(err)
	}
	if env[0].To() != "MixedCaseLocal@example.invalid" || strings.Contains(string(env[0].Wire()), "other@example.invalid") || strings.Count(string(env[0].Wire()), "\r\nTo:") != 1 {
		t.Fatal("RCPT/case leak")
	}
	if ps[0].event != hash(SavingsExpiredKind+"\x00"+in.ContractID+"\x00"+in.TermGeneration) || ps[0].recipient != hash("recipient-00") {
		t.Fatal("opaque identity")
	}
	for _, p := range ps {
		if !validSavingsExpiredPayload(p.plaintext, p.event, p.recipient, p.expiry) {
			t.Fatal("canonical decode")
		}
	}
	zone := in
	zone.CanonicalEndDate = in.CanonicalEndDate.In(time.FixedZone("fixture", 7*3600))
	next, err := prepareSavingsExpired(zone)
	if err != nil || !bytes.Equal(ps[0].plaintext, next[0].plaintext) {
		t.Fatal("UTC canonical identity", err)
	}
	for _, change := range []func(*SavingsExpired){func(v *SavingsExpired) { v.Subject += "x" }, func(v *SavingsExpired) { v.FullName += "x" }, func(v *SavingsExpired) { v.From = "other@example.invalid" }, func(v *SavingsExpired) { v.Recipients = append(v.Recipients, "new@example.invalid") }, func(v *SavingsExpired) {
		v.Date = v.Date.Add(time.Microsecond)
		v.ExpiresAt = v.ExpiresAt.Add(time.Microsecond)
	}} {
		v := in
		change(&v)
		next, err := prepareSavingsExpired(v)
		if err != nil || next[0].event != ps[0].event || bytes.Equal(next[0].plaintext, ps[0].plaintext) {
			t.Fatal("same-event drift binding", err)
		}
	}
	var object map[string]any
	_ = json.Unmarshal(ps[0].plaintext, &object)
	object["unknown"] = "ignored?"
	raw, _ := json.Marshal(object)
	if validSavingsExpiredPayload(raw, ps[0].event, ps[0].recipient, ps[0].expiry) || validSavingsExpiredPayload(ps[0].plaintext, ps[0].event, ps[0].recipient, ps[0].expiry.Add(time.Microsecond)) {
		t.Fatal("canonical drift admitted")
	}
	var log bytes.Buffer
	slog.New(slog.NewJSONHandler(&log, nil)).Info("dto", "input", in, "env", env[0])
	for _, s := range []string{fmt.Sprint(in), fmt.Sprintf("%+v %#v", in, in), log.String(), fmt.Sprintf("%#v", env[0])} {
		for _, pii := range []string{"PRIVATE_NAME", "PRIVATE_PLAN", "MixedCaseLocal", "sender@example.invalid"} {
			if strings.Contains(s, pii) {
				t.Fatal("PII redaction")
			}
		}
	}
	for _, kind := range []string{Kind, InviteKind, ValidationApprovedKind, SavingsExpiredKind} {
		a, e := payloadAAD("id", kind, Version)
		if e != nil || !bytes.Equal(a, []byte("id\x00"+kind+"\x001")) {
			t.Fatal("legacy AAD changed")
		}
	}
}
func TestSavingsExpiredInvalidBoundsAndNonDelivery(t *testing.T) {
	for _, change := range []func(*SavingsExpired){func(v *SavingsExpired) { v.TermGeneration = "fresh-uuid" }, func(v *SavingsExpired) { v.TermGeneration = strings.Repeat("a", 64) }, func(v *SavingsExpired) { v.ContractID = "email@example.invalid" }, func(v *SavingsExpired) { v.CanonicalEndDate = v.CanonicalEndDate.Add(time.Nanosecond) }, func(v *SavingsExpired) { v.Date = time.Time{} }, func(v *SavingsExpired) { v.ExpiresAt = v.ExpiresAt.Add(time.Microsecond) }, func(v *SavingsExpired) {
		v.Date = v.Date.Add(time.Nanosecond)
		v.ExpiresAt = v.ExpiresAt.Add(time.Nanosecond)
	}, func(v *SavingsExpired) { v.Subject = "bad\nheader" }, func(v *SavingsExpired) { v.FullName = "bad\nname" }, func(v *SavingsExpired) { v.HTMLBody = strings.Repeat("x", MaxWireBytes+1) }, func(v *SavingsExpired) { v.TextBody = strings.Repeat("x", 800000); v.HTMLBody = "" }, func(v *SavingsExpired) { v.Disposition = Accepted }, func(v *SavingsExpired) { v.Recipients = []string{"bad"} }, func(v *SavingsExpired) { v.Recipients = []string{""} }, func(v *SavingsExpired) {
		v.Disposition = Suppressed
		v.Recipients = []string{"valid@example.invalid", "bad"}
	}, func(v *SavingsExpired) { v.Recipients = make([]string, MaxRecipients+1) }, func(v *SavingsExpired) { v.Recipients = []string{"a@example.invalid, b@example.invalid"} }, func(v *SavingsExpired) { v.Recipients = []string{"a\r\nBcc: secret@example.invalid"} }} {
		v := expiredFixture()
		change(&v)
		if _, err := PrepareSavingsExpired(v); err == nil {
			t.Fatal("invalid admitted")
		}
	}
	for _, state := range []string{Suppressed, Blocked, NoRecipient} {
		v := expiredFixture()
		v.From = ""
		v.Disposition = state
		if state == NoRecipient {
			v.Recipients = nil
		}
		ps, e := prepareSavingsExpired(v)
		if e != nil || ps[0].state != state {
			t.Fatal("explicit non-delivery", e)
		}
	}
}
