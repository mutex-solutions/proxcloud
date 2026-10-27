package cloud

import "testing"

// TestDownloadTokenUnguessable pins the download-token security properties: the public token is a
// 256-bit crypto-random value (64 hex chars), each mint is distinct, and it is NOT derivable from
// the stored id — only the token's hash is persisted, and hashing is a one-way, deterministic map.
func TestDownloadTokenUnguessable(t *testing.T) {
	t1, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	t2, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(t1) != 64 { // 32 bytes hex = 256-bit
		t.Errorf("token length = %d, want 64 hex chars", len(t1))
	}
	if t1 == t2 {
		t.Error("two mints produced the same token (not random)")
	}
	// The stored form is the hash, never the raw token: a DB read (the hash) must not equal the
	// public token, so a leaked _id cannot be replayed as a token.
	if hashToken(t1) == t1 {
		t.Error("stored hash must differ from the public token")
	}
	// Hashing is deterministic (lookup by hash resolves the same doc).
	if hashToken(t1) != hashToken(t1) {
		t.Error("hashToken must be deterministic")
	}
	if hashToken(t1) == hashToken(t2) {
		t.Error("distinct tokens must hash distinctly")
	}
}
