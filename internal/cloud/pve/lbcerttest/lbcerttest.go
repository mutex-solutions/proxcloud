// Package lbcerttest makes real certificates for the tests of a load balancer's TLS: a root, an intermediate it
// signed and a server certificate the intermediate signed, as a public authority issues them.
package lbcerttest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

// Cert is a server certificate as a tenant uploads it.
type Cert struct {
	Chain string // PEM: the server's certificate, then the intermediate
	Key   string // PEM, PKCS #8
	Root  string // PEM of the root, for a client that verifies
	Leaf  *x509.Certificate
}

// RSA is a fresh RSA key.
func RSA(t testing.TB, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// EC is a fresh ECDSA key.
func EC(t testing.TB, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// PEM encodes one block.
func PEM(kind string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
}

// New issues a server certificate for names (host names and IP addresses) on key, valid until notAfter.
func New(t testing.TB, key crypto.Signer, notAfter time.Time, names ...string) Cert {
	t.Helper()
	issue := func(cn string, ca bool, pub crypto.PublicKey, parent *x509.Certificate, signer crypto.Signer, until time.Time) *x509.Certificate {
		serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
		tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: until,
			BasicConstraintsValid: true, IsCA: ca, KeyUsage: x509.KeyUsageDigitalSignature}
		if ca {
			tpl.KeyUsage |= x509.KeyUsageCertSign
		} else {
			tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			for _, n := range names {
				if ip := net.ParseIP(n); ip != nil {
					tpl.IPAddresses = append(tpl.IPAddresses, ip)
				} else {
					tpl.DNSNames = append(tpl.DNSNames, n)
				}
			}
		}
		if parent == nil {
			parent = tpl
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, parent, pub, signer)
		if err != nil {
			t.Fatal(err)
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	far := time.Now().Add(240 * time.Hour)
	rootKey, midKey := EC(t, elliptic.P256()), EC(t, elliptic.P256())
	root := issue("test root", true, rootKey.Public(), nil, rootKey, far)
	mid := issue("test intermediate", true, midKey.Public(), root, rootKey, far)
	cn := "server"
	if len(names) > 0 {
		cn = names[0]
	}
	leaf := issue(cn, false, key.Public(), mid, midKey, notAfter)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return Cert{Chain: PEM("CERTIFICATE", leaf.Raw) + PEM("CERTIFICATE", mid.Raw), Key: PEM("PRIVATE KEY", der), Root: PEM("CERTIFICATE", root.Raw), Leaf: leaf}
}
