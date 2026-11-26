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

// Sign says how a chain is signed where it is not as an authority signs today: the signature algorithm of the
// server's certificate, of the intermediate and of the root (zero: SHA-256 with the signing key), and RSA 2048 keys
// for the root and the intermediate in place of ECDSA P-256 ones.
type Sign struct {
	Leaf, Intermediate, Root x509.SignatureAlgorithm
	RSA                      bool
}

// issue signs one certificate. Without a parent it signs itself.
func issue(t testing.TB, cn string, ca bool, pub crypto.PublicKey, parent *x509.Certificate, signer crypto.Signer, until time.Time, alg x509.SignatureAlgorithm, names []string) *x509.Certificate {
	t.Helper()
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: until,
		BasicConstraintsValid: true, IsCA: ca, KeyUsage: x509.KeyUsageDigitalSignature, SignatureAlgorithm: alg}
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

func cn(names []string) string {
	if len(names) > 0 {
		return names[0]
	}
	return "server"
}

func pkcs8(t testing.TB, key crypto.Signer) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return PEM("PRIVATE KEY", der)
}

// New issues a server certificate for names (host names and IP addresses) on key, valid until notAfter.
func New(t testing.TB, key crypto.Signer, notAfter time.Time, names ...string) Cert {
	t.Helper()
	return NewSigned(t, key, notAfter, Sign{}, names...)
}

// NewSigned is New with the chain signed as sign says.
func NewSigned(t testing.TB, key crypto.Signer, notAfter time.Time, sign Sign, names ...string) Cert {
	t.Helper()
	far := time.Now().Add(240 * time.Hour)
	var rootKey, midKey crypto.Signer = EC(t, elliptic.P256()), EC(t, elliptic.P256())
	if sign.RSA {
		rootKey, midKey = RSA(t, 2048), RSA(t, 2048)
	}
	root := issue(t, "test root", true, rootKey.Public(), nil, rootKey, far, sign.Root, nil)
	mid := issue(t, "test intermediate", true, midKey.Public(), root, rootKey, far, sign.Intermediate, nil)
	leaf := issue(t, cn(names), false, key.Public(), mid, midKey, notAfter, sign.Leaf, names)
	return Cert{Chain: PEM("CERTIFICATE", leaf.Raw) + PEM("CERTIFICATE", mid.Raw), Key: pkcs8(t, key), Root: PEM("CERTIFICATE", root.Raw), Leaf: leaf}
}

// SelfSigned issues a server certificate that key signed itself, with alg: Chain and Root are that one certificate.
func SelfSigned(t testing.TB, key crypto.Signer, notAfter time.Time, alg x509.SignatureAlgorithm, names ...string) Cert {
	t.Helper()
	leaf := issue(t, cn(names), false, key.Public(), nil, key, notAfter, alg, names)
	return Cert{Chain: PEM("CERTIFICATE", leaf.Raw), Key: pkcs8(t, key), Root: PEM("CERTIFICATE", leaf.Raw), Leaf: leaf}
}
