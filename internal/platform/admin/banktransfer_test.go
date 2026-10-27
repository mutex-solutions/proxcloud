package admin

import (
	"net/http"
	"testing"
)

// TestBankTransferNotFound pins the exact 404:
// "Bank transfer %s not found " — interpolated id + a trailing space — and the 404 status/code.
func TestBankTransferNotFound(t *testing.T) {
	err := bankTransferNotFound("bt-123")
	want := "Bank transfer bt-123 not found "
	if err.Msg != want {
		t.Errorf("message=%q want %q", err.Msg, want)
	}
	if err.Status != http.StatusNotFound {
		t.Errorf("status=%d want %d", err.Status, http.StatusNotFound)
	}
	if err.Code != http.StatusNotFound {
		t.Errorf("code=%d want %d", err.Code, http.StatusNotFound)
	}
}
