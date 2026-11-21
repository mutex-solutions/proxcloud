package csigw

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The API makes the key and certificate once, for the URL's host, as its own CA; a restart keeps them, a start that
// stopped after the key makes both again, and a certificate whose key is lost is refused rather than replaced.
func TestEnsureSelfSigned(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := EnsureSelfSigned(cert, key, "https://192.0.2.7:8444"); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil || !leaf.IPAddresses[0].Equal(net.ParseIP("192.0.2.7")) {
		t.Fatalf("not its own CA for 192.0.2.7: %v %v", err, leaf.IPAddresses)
	}
	if st, _ := os.Stat(key); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode())
	}
	before, _ := os.ReadFile(cert)
	if err := EnsureSelfSigned(cert, key, "https://other.example:8444"); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(cert); string(after) != string(before) {
		t.Fatal("a restart replaced the certificate the clusters trust")
	}
	if err := os.Remove(cert); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSelfSigned(cert, key, "https://gw.example:8444"); err != nil {
		t.Fatal(err)
	}
	if p, _ := tls.LoadX509KeyPair(cert, key); p.Leaf == nil || p.Leaf.DNSNames[0] != "gw.example" {
		t.Fatal("a key without its certificate was not made again")
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSelfSigned(cert, key, "https://gw.example:8444"); err == nil || !strings.Contains(err.Error(), "has no key") {
		t.Fatalf("a certificate without its key: %v", err)
	}
	if err := EnsureSelfSigned(filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key"), "not a url"); err == nil {
		t.Fatal("no host")
	}
}
