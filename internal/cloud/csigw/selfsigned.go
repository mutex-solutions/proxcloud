package csigw

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// selfSignedFor is how long a self-signed gateway certificate lasts. The clusters' controllers trust it as their CA,
// so a new one means a new Secret for every cluster: it is made once and kept.
const selfSignedFor = 10 * 365 * 24 * time.Hour

// EnsureSelfSigned writes an ECDSA P-256 key to keyFile and a self-signed certificate for gatewayURL's host to
// certFile, unless the certificate exists already: a restart keeps the one the clusters trust. The key is written
// first, so a key without its certificate is from a start that stopped half-way and is made again; a certificate
// without its key is refused. The certificate is its own CA.
func EnsureSelfSigned(certFile, keyFile, gatewayURL string) error {
	_, certErr := os.Stat(certFile)
	_, keyErr := os.Stat(keyFile)
	switch {
	case certErr == nil && keyErr == nil:
		return nil
	case certErr == nil:
		return fmt.Errorf("csi gateway: certificate %s has no key %s: restore the key, or remove both to make a new pair (every cluster then needs its gateway Secret again)", certFile, keyFile)
	case !errors.Is(certErr, os.ErrNotExist):
		return certErr
	}
	u, err := url.Parse(gatewayURL)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("csi gateway: no host in %q for the certificate", gatewayURL)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: u.Hostname(), Organization: []string{"ProxCloud gateway"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{u.Hostname()}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// The key first: a certificate on disk is what says the pair is complete.
	if err := writeFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// writeFile writes through a temporary file in the same directory, so a crash leaves the old state or the new one.
func writeFile(name string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}
