package certfile

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePair(t *testing.T, dir string, serial int64) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "*.sbx.test"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"*.sbx.test"}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	_ = os.WriteFile(filepath.Join(dir, "tls.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600)
}

func serial(t *testing.T, get func(*tls.ClientHelloInfo) (*tls.Certificate, error)) int64 {
	t.Helper()
	c, err := get(nil)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := x509.ParseCertificate(c.Certificate[0])
	return x.SerialNumber.Int64()
}

// A renewal written in place is served after the reload interval; a broken write keeps the last good one.
func TestFilesReload(t *testing.T) {
	dir := t.TempDir()
	if _, err := Files(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")); err == nil {
		t.Fatal("missing files must fail the first load")
	}
	writePair(t, dir, 1)
	reload = time.Hour
	get, err := Files(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	writePair(t, dir, 2)
	if s := serial(t, get); s != 1 {
		t.Fatalf("read again before the interval: serial %d", s)
	}
	reload = 0
	if s := serial(t, get); s != 2 {
		t.Fatalf("renewal not picked up: serial %d", s)
	}
	_ = os.WriteFile(filepath.Join(dir, "tls.key"), []byte("half written"), 0o600)
	if s := serial(t, get); s != 2 {
		t.Fatalf("a broken file must keep the last certificate: serial %d", s)
	}
}
