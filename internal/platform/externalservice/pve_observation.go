package externalservice

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// RawPVEObservationGeneration derives an opaque revision from the STORED provider
// row, before decryption. It retains neither ciphertext nor plaintext credentials.
// Re-encryption may conservatively start a new evidence window; same-ID credential
// rotation cannot combine observations from two different credentials.
func RawPVEObservationGeneration(e *ExternalService) string {
	if e == nil || !e.IsProxmox() {
		return ""
	}
	c := e.PVEConfig()
	b, _ := json.Marshal([]any{"pve-observation-generation/v1", e.ID, e.Type, e.Status, e.PVERegion(), c.APIURL, c.APIURLs, c.TokenID, e.PVEPoolPrefix(), c.Fingerprint, c.CACert, c.InsecureTLS, c.Secret})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
