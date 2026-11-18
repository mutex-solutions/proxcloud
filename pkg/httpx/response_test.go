package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

// failedWriter is a handler's writer that wants to know why a request failed (a create that audits its refusals).
type failedWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (f *failedWriter) Failed(err error) { f.err = err }

// A backend's error goes to the operator and never to the caller: the log line is structured, carries the request id
// and the cause, and the writer that asked is told the whole error. The API log of v0.8.0 held these lines as plain
// text without a request id.
func TestWriteErrorWithCause(t *testing.T) {
	var logged bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	defer slog.SetDefault(old)

	w := &failedWriter{ResponseRecorder: httptest.NewRecorder()}
	w.Header().Set(RequestIDHeader, "req-7")
	cause := errors.New("pve: 403 Permission check failed (/storage/nvme-u2, Permissions.Modify|Datastore.Allocate)")
	WriteError(w, NewError(http.StatusBadGateway, http.StatusBadGateway, "The compute backend is unavailable. Try again later.").WithCause(cause))

	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "nvme-u2") || !strings.Contains(w.Body.String(), "compute backend is unavailable") {
		t.Errorf("the caller got %d %s", w.Code, w.Body.String())
	}
	var line map[string]any
	if err := json.Unmarshal(logged.Bytes(), &line); err != nil {
		t.Fatalf("the log line is not one JSON object: %q", logged.String())
	}
	if line["level"] != "ERROR" || line["msg"] != "backend request failed" || line["reqid"] != "req-7" || line["err"] != cause.Error() {
		t.Errorf("the log line: %v", line)
	}
	var he *HTTPError
	if !errors.As(w.err, &he) || he.Cause != cause {
		t.Errorf("the writer was told %v", w.err)
	}

	// An answer without a cause is the caller's own mistake: nothing to log.
	logged.Reset()
	WriteError(httptest.NewRecorder(), BadRequest("name is required"))
	if logged.Len() != 0 {
		t.Errorf("a 400 was logged: %s", logged.String())
	}
}
