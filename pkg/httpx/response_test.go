package httpx

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteError(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// nil: nothing written, false, so `if WriteError(w, err) { return }` falls through.
	rec := httptest.NewRecorder()
	if WriteError(rec, nil) || rec.Body.Len() != 0 {
		t.Fatalf("nil err: wrote %q", rec.Body.String())
	}

	// typed, also when wrapped: its own status and message.
	rec = httptest.NewRecorder()
	if !WriteError(rec, fmt.Errorf("ctx: %w", NotFound("Thing not found"))) || rec.Code != 404 ||
		!strings.Contains(rec.Body.String(), "Thing not found") {
		t.Fatalf("typed: %d %q", rec.Code, rec.Body.String())
	}
	if logs.Len() != 0 {
		t.Fatalf("typed error logged: %s", logs.String())
	}

	// untyped: generic 500, error logged with the request id, never in the body.
	rec = httptest.NewRecorder()
	rec.Header().Set(RequestIDHeader, "req-42")
	if !WriteError(rec, errors.New("pq: password authentication failed")) || rec.Code != 500 {
		t.Fatalf("untyped: %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"errors":{"code":500,"message":"internal.error"}}` {
		t.Fatalf("untyped body %q", got)
	}
	if l := logs.String(); !strings.Contains(l, "req-42") || !strings.Contains(l, "password authentication failed") {
		t.Fatalf("log %q lacks the request id or the error", l)
	}
}

// An error with details carries them as errors.details; one without keeps the envelope it always had, and WithDetails
// leaves the shared error it copies untouched.
func TestWriteErrorDetails(t *testing.T) {
	base := NewError(409, 409, "Project is DISABLED")
	rec := httptest.NewRecorder()
	WriteError(rec, fmt.Errorf("wrapped: %w", base.WithDetails(map[string]string{"status": "DISABLED", "reason": "billing_profile_inactive"})))
	want := `{"errors":{"code":409,"message":"Project is DISABLED","details":{"reason":"billing_profile_inactive","status":"DISABLED"}}}`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != 409 || got != want {
		t.Fatalf("with details: %d %s", rec.Code, got)
	}
	rec = httptest.NewRecorder()
	WriteError(rec, base)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"errors":{"code":409,"message":"Project is DISABLED"}}` || base.Details != nil {
		t.Fatalf("without details: %s (shared error details %v)", got, base.Details)
	}
}
