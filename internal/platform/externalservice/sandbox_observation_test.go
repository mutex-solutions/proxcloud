package externalservice

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

func TestSandboxObservationGenerationCapturesEncryptedSecretBeforeDecrypt(t *testing.T) {
	enc := textcrypt.New("local-fixture-key")
	svc := NewService(nil, enc)
	makeRow := func(token string) *ExternalService {
		return &ExternalService{ID: "sandbox", Type: "CLOUD", Status: "PUBLIC", Config: map[string]any{"provider": "sandbox", "sandbox": map[string]any{"envd": true}}, Secret: enc.EncryptObject(map[string]any{"kubeconfig": "same-cluster", "proxyWakeToken": token})}
	}
	a, b := makeRow("wake-token"), makeRow("")
	raw := RawSandboxObservationGeneration(a)
	svc.Decrypt(a)
	svc.Decrypt(b)
	if len(raw) != 64 || a.ObservationGeneration != raw || b.ObservationGeneration == raw || a.SandboxConfig().ProxyWakeToken != "wake-token" || b.SandboxConfig().ProxyWakeToken != "" {
		t.Fatal("secret-only revision not frozen before decrypt")
	}
	first := a.ObservationGeneration
	svc.Decrypt(a)
	if a.ObservationGeneration != first {
		t.Fatal("decrypted value replaced raw revision")
	}
	wire, _ := json.Marshal(struct {
		Generation string `json:"-"`
	}{a.ObservationGeneration})
	if strings.Contains(string(wire), first) || strings.Contains(first, "token") {
		t.Fatal("revision escaped transient field")
	}
	if RawSandboxObservationGeneration(nil) != "" || RawSandboxObservationGeneration(&ExternalService{Config: map[string]any{"provider": "proxmox"}}) != "" {
		t.Fatal("sandbox revision altered other providers")
	}
}
