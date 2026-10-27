package auth

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

// TestSignSigV4Roundtrip proves the signer and verifier agree: a request
// signed by SignSigV4 passes verifySigV4 with the same key pair.
func TestSignSigV4Roundtrip(t *testing.T) {
	a := New(slog.Default())
	a.SetHmacLookup(func(_ context.Context, id string) (string, bool) {
		if id == "pkdeadbeef" {
			return "sksecret", true
		}
		return "", false
	})

	body := []byte(`{"email":"x@y.z"}`)
	req, _ := http.NewRequest("POST", "/admin-api/v1/users?limit=5&marker=a%2Fb", bytes.NewReader(body))
	req.Host = "mcp.internal"
	req.Header.Set("Content-Type", "application/json")
	SignSigV4(req, "pkdeadbeef", "sksecret", body, time.Now())

	rc, ok := a.verifySigV4(req, req.Header.Get("Authorization"))
	if !ok {
		t.Fatal("signed request did not verify")
	}
	if rc.SigV4KeyID != "pkdeadbeef" {
		t.Fatalf("SigV4KeyID = %q", rc.SigV4KeyID)
	}

	// Tampered body must fail.
	req2, _ := http.NewRequest("POST", "/admin-api/v1/users", bytes.NewReader([]byte(`{}`)))
	req2.Host = "mcp.internal"
	SignSigV4(req2, "pkdeadbeef", "sksecret", []byte(`{}`), time.Now())
	req2.Body = http.NoBody
	req2.Header.Set("X-Amz-Date", time.Now().UTC().Format("20060102T150405Z")) // keep date fresh
	// body now empty but signature covered `{}` → mismatch
	if _, ok := a.verifySigV4(req2, req2.Header.Get("Authorization")); ok {
		t.Fatal("tampered request verified")
	}
}

// Regression: a hand-built GET with r.Body == nil (MCP in-process dispatch)
// must verify, not panic (io.ReadAll(nil) crashed the pod on final-test drill).
func TestSignSigV4NilBody(t *testing.T) {
	a := New(slog.Default())
	a.SetHmacLookup(func(_ context.Context, id string) (string, bool) { return "sksecret", id == "pkdeadbeef" })

	req, _ := http.NewRequest("GET", "/admin-api/v1/users?limit=5", nil)
	req.Host = "mcp.internal"
	req.Body = nil // NewRequest with nil reader leaves Body nil on client-style requests
	SignSigV4(req, "pkdeadbeef", "sksecret", nil, time.Now())
	if _, ok := a.verifySigV4(req, req.Header.Get("Authorization")); !ok {
		t.Fatal("nil-body GET did not verify")
	}
}
