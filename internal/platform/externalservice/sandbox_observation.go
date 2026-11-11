package externalservice

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// RawSandboxObservationGeneration is an opaque revision of the STORED provider,
// before secret decryption. Any credential-only edit invalidates a frozen CREATE
// candidate (including clearing a wake token). No ciphertext or plaintext is
// retained in the revision; conservative re-encryption invalidation is intended.
func RawSandboxObservationGeneration(e *ExternalService) string {
	if e == nil || !e.IsSandbox() {
		return ""
	}
	b, err := json.Marshal([]any{"sandbox-provider-generation/v1", e.ID, e.Type, e.Status, e.Config, e.Secret})
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
