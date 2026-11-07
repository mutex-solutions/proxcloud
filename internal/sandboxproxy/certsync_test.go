package sandboxproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

type memSecrets struct {
	data   map[string]map[string][]byte
	writes int
}

func (m *memSecrets) GetSecretData(_ context.Context, ns, name string) (map[string][]byte, error) {
	return m.data[ns+"/"+name], nil
}

func (m *memSecrets) PatchSecretData(_ context.Context, ns, name string, sd map[string]string) error {
	m.writes++
	d := m.data[ns+"/"+name]
	for k, v := range sd {
		d[k] = []byte(v)
	}
	return nil
}

func pemPair(t *testing.T, serial int64) (crt, key []byte) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "*.sbx.test"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
}

func TestSyncCert(t *testing.T) {
	ctx := context.Background()
	from, to := SecretRef{"pc-sandbox", "sandbox-wildcard-tls"}, SecretRef{"sandbox-system", "sandbox-proxy-tls"}
	crt, key := pemPair(t, 1)
	src := &memSecrets{data: map[string]map[string][]byte{"pc-sandbox/sandbox-wildcard-tls": {"tls.crt": crt, "tls.key": key, "ca.crt": []byte("x")}}}
	dst := &memSecrets{data: map[string]map[string][]byte{}}

	if _, err := SyncCert(ctx, src, from, dst, to); err == nil {
		t.Fatal("a missing target Secret must be reported, not created")
	}
	dst.data["sandbox-system/sandbox-proxy-tls"] = map[string][]byte{}
	if ch, err := SyncCert(ctx, src, from, dst, to); err != nil || !ch {
		t.Fatalf("first copy: %v %v", ch, err)
	}
	if ch, err := SyncCert(ctx, src, from, dst, to); err != nil || ch || dst.writes != 1 {
		t.Fatalf("unchanged source must not write: %v %v writes=%d", ch, err, dst.writes)
	}
	if _, ok := dst.data["sandbox-system/sandbox-proxy-tls"]["ca.crt"]; ok {
		t.Fatal("only the certificate and its key cross")
	}

	// A renewal is copied; a half-written or mismatched source never replaces a working one.
	crt2, key2 := pemPair(t, 2)
	src.data["pc-sandbox/sandbox-wildcard-tls"]["tls.crt"], src.data["pc-sandbox/sandbox-wildcard-tls"]["tls.key"] = crt2, key2
	if ch, err := SyncCert(ctx, src, from, dst, to); err != nil || !ch || string(dst.data["sandbox-system/sandbox-proxy-tls"]["tls.crt"]) != string(crt2) {
		t.Fatalf("renewal: %v %v", ch, err)
	}
	src.data["pc-sandbox/sandbox-wildcard-tls"]["tls.key"] = key // key of the old certificate
	if _, err := SyncCert(ctx, src, from, dst, to); err == nil || string(dst.data["sandbox-system/sandbox-proxy-tls"]["tls.key"]) != string(key2) {
		t.Fatalf("a mismatched pair was copied: %v", err)
	}
	src.data["pc-sandbox/sandbox-wildcard-tls"]["tls.crt"] = nil
	if _, err := SyncCert(ctx, src, from, dst, to); err == nil {
		t.Fatal("an empty source must be reported")
	}
}
