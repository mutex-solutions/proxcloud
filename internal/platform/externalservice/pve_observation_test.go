package externalservice

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

func TestObservationGenerationEncryptedCredentialRevisionBeforeDecrypt(t *testing.T) {
	enc := textcrypt.New("fixture-key")
	svc := NewService(nil, enc)
	const secret = "plaintext-canary-never-a-generation"
	es := &ExternalService{ID: "provider", Type: TypeCloud, Status: "PUBLIC", Config: map[string]any{"provider": "proxmox", "region": "r", "tokenId": "same@pve!portal"}, Secret: map[string]any{"tokenSecret": enc.Encrypt(secret)}}
	generation := RawPVEObservationGeneration(es)
	svc.Decrypt(es)
	if es.ObservationGeneration != generation || es.PVEConfig().Secret != secret {
		t.Fatal("revision not stamped before decrypt")
	}
	svc.Decrypt(es)
	if es.ObservationGeneration != generation {
		t.Fatal("repeat decrypt changed revision")
	}
	b, _ := json.Marshal(es)
	if strings.Contains(string(b), generation) {
		t.Fatal("transient revision serialized")
	}
	es.Secret = map[string]any{"tokenSecret": enc.Encrypt("rotated-canary")}
	if RawPVEObservationGeneration(es) == generation {
		t.Fatal("same token ID rotation reused revision")
	}
}
