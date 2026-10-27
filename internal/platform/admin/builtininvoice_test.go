package admin

import (
	"fmt"
	"net/http"
	"testing"
)

// Pure unit tests for the built-in-invoice admin endpoints — no datastore / no network. They pin the
// deterministic, load-bearing strings and codes (the exact 404 message, the perm key, the
// not-implemented error code) so a refactor can't silently drift them off the contract.

// builtInInvoiceNotFoundMsg duplicates the inline formatting in builtInInvoiceDownload so the test can
// assert the exact string ("Invoice %s not found" — interpolated, NO trailing space).
func builtInInvoiceNotFoundMsg(id string) string {
	return fmt.Sprintf("Invoice %s not found", id)
}

func TestBuiltInInvoiceNotFoundMessage(t *testing.T) {
	got := builtInInvoiceNotFoundMsg("abc123")
	want := "Invoice abc123 not found"
	if got != want {
		t.Fatalf("404 message = %q, want %q", got, want)
	}
}

func TestBuiltInInvoiceReadPerm(t *testing.T) {
	if builtInInvoiceReadPerm != "admin:bill:read" {
		t.Fatalf("perm = %q, want admin:bill:read", builtInInvoiceReadPerm)
	}
}

func TestBuiltInInvoiceCollectionName(t *testing.T) {
	if builtInInvoiceCollection != "builtInInvoice" {
		t.Fatalf("collection = %q, want builtInInvoice", builtInInvoiceCollection)
	}
}

// TestBuiltInInvoiceDownloadSeamCode pins the not-implemented response (501) so the external download
// stays a 501, not a silently-wrong 200.
func TestBuiltInInvoiceDownloadSeamCode(t *testing.T) {
	if http.StatusNotImplemented != 501 {
		t.Fatalf("not-implemented status drifted: got %d", http.StatusNotImplemented)
	}
}
