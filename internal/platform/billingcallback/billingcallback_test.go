package billingcallback

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "s3cr3t-token"

type recordingSuspender struct {
	suspended, resumed []string
	err                error
}

func (r *recordingSuspender) SuspendBillingProfileClouds(_ context.Context, id string) error {
	r.suspended = append(r.suspended, id)
	return r.err
}
func (r *recordingSuspender) ResumeBillingProfileClouds(_ context.Context, id string) error {
	r.resumed = append(r.resumed, id)
	return r.err
}

type recordingNotifier struct {
	key  string
	to   []string
	vars map[string]any
	err  error
}

func (r *recordingNotifier) SendTemplate(_ context.Context, key string, to []string, vars map[string]any) error {
	r.key, r.to, r.vars = key, to, vars
	return r.err
}

func mount(t *testing.T, h *Handler) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, path, token, body string) *http.Response {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestNoTokenMountsNothing is the fail-closed guarantee. These endpoints can pause every server a
// customer owns; if the token is not configured, the handler must not exist at all rather than
// existing unauthenticated.
func TestNoTokenMountsNothing(t *testing.T) {
	for _, tok := range []string{"", "   "} {
		if h := New(&recordingSuspender{}, nil, nil, tok, nil); h != nil {
			t.Errorf("New with token %q returned a handler; it must refuse to mount", tok)
		}
	}
	// A nil handler's Routes must be a safe no-op, not a panic.
	mux := http.NewServeMux()
	(*Handler)(nil).Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/billing-profiles/x/suspend-clouds", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unmounted callback should 404, got %d", rec.Code)
	}
}

// TestRejectsBadToken: a wrong or missing token must never reach the cloud operations.
func TestRejectsBadToken(t *testing.T) {
	sus := &recordingSuspender{}
	srv := mount(t, New(sus, nil, nil, testToken, nil))

	for _, tc := range []struct{ name, token string }{
		{"missing", ""},
		{"wrong", "not-the-token"},
		{"prefix-of-real", testToken[:4]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := post(t, srv, "/internal/v1/billing-profiles/bp-1/suspend-clouds", tc.token, "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
	if len(sus.suspended) != 0 {
		t.Errorf("an unauthorized request reached the cloud suspender: %v", sus.suspended)
	}
}

func TestSuspendAndResumeClouds(t *testing.T) {
	sus := &recordingSuspender{}
	srv := mount(t, New(sus, nil, nil, testToken, nil))

	if resp := post(t, srv, "/internal/v1/billing-profiles/bp-7/suspend-clouds", testToken, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend status = %d", resp.StatusCode)
	}
	if resp := post(t, srv, "/internal/v1/billing-profiles/bp-7/resume-clouds", testToken, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("resume status = %d", resp.StatusCode)
	}
	if len(sus.suspended) != 1 || sus.suspended[0] != "bp-7" {
		t.Errorf("suspended = %v, want [bp-7]", sus.suspended)
	}
	if len(sus.resumed) != 1 || sus.resumed[0] != "bp-7" {
		t.Errorf("resumed = %v, want [bp-7]", sus.resumed)
	}
}

// TestSuspendFailureIsReported: unlike mail, a failed cloud suspend is surfaced as a 500. The
// engine still swallows it (suspension is best-effort there), but the caller and the logs should
// see that the cloud leg did not happen.
func TestSuspendFailureIsReported(t *testing.T) {
	sus := &recordingSuspender{err: fmt.Errorf("nova unreachable")}
	srv := mount(t, New(sus, nil, nil, testToken, nil))

	resp := post(t, srv, "/internal/v1/billing-profiles/bp-1/suspend-clouds", testToken, "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestActivateProjects(t *testing.T) {
	var got string
	activate := func(_ context.Context, id string) error { got = id; return nil }
	srv := mount(t, New(nil, activate, nil, testToken, nil))

	if resp := post(t, srv, "/internal/v1/billing-profiles/bp-9/activate-projects", testToken, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got != "bp-9" {
		t.Errorf("activated %q, want bp-9", got)
	}
}

func TestNotify(t *testing.T) {
	n := &recordingNotifier{}
	srv := mount(t, New(nil, nil, n, testToken, nil))

	body := `{"key":"billing_profile_validated","to":["a@example.com"],"vars":{"loginUrl":"https://x"}}`
	resp := post(t, srv, "/internal/v1/notify", testToken, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if n.key != "billing_profile_validated" || len(n.to) != 1 || n.to[0] != "a@example.com" {
		t.Errorf("notifier got key=%q to=%v", n.key, n.to)
	}
	if n.vars["loginUrl"] != "https://x" {
		t.Errorf("vars = %v", n.vars)
	}
}

// TestNotifyFailureIsNotAnError: mail is best-effort. A send failure must report sent=false with a
// 200, never a 5xx — the engine must not retry or roll back a billing state change because an
// email bounced.
func TestNotifyFailureIsNotAnError(t *testing.T) {
	n := &recordingNotifier{err: fmt.Errorf("smtp down")}
	srv := mount(t, New(nil, nil, n, testToken, nil))

	resp := post(t, srv, "/internal/v1/notify", testToken, `{"key":"k","to":["a@b.c"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (mail failures must not be errors)", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Sent bool `json:"sent"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Sent {
		t.Error("sent should be false when the mailer failed")
	}
}

func TestNotifyValidatesBody(t *testing.T) {
	srv := mount(t, New(nil, nil, &recordingNotifier{}, testToken, nil))
	for _, body := range []string{`not json`, `{"to":["a@b.c"]}`, `{"key":"k"}`, `{"key":"k","to":[]}`} {
		resp := post(t, srv, "/internal/v1/notify", testToken, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

// TestUnwiredCollaboratorsDegrade: a partially-configured deployment must report the no-op rather
// than panicking mid billing state transition.
func TestUnwiredCollaboratorsDegrade(t *testing.T) {
	srv := mount(t, New(nil, nil, nil, testToken, nil))
	for _, path := range []string{
		"/internal/v1/billing-profiles/bp-1/suspend-clouds",
		"/internal/v1/billing-profiles/bp-1/resume-clouds",
		"/internal/v1/billing-profiles/bp-1/activate-projects",
	} {
		if resp := post(t, srv, path, testToken, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 with applied=false", path, resp.StatusCode)
		}
	}
}
