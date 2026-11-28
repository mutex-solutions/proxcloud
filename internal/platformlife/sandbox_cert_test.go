package platformlife

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// rc2Certificate is the certificate 0.12.0-rc.1 and rc.2 made for sandbox-proxy: its own issuer, marked as a
// certificate authority, with keyCertSign. The drill of rc.2 saw the E2B Python SDK 2.63.0 refuse it
// ("CaUsedAsEndEntity").
func rc2Certificate(t *testing.T, domain string, now time.Time) (certPEM, keyPEM string) {
	t.Helper()
	return certEnding(t, domain, now, now.AddDate(5, 0, 0), true)
}

// certEnding is a certificate for domain that is its own issuer and ends at notAfter: the shape the run makes, or
// with ca the shape of rc.2. It serves the cases that turn on the day a certificate ends. selfSignedWildcard takes
// a start and adds five years, and a start five years before the wanted end does not lead back to it across a
// 29 February: on 9 February 2028 a certificate made that way for "20 days left" has 21, and on 30 January 2029
// one made for 31 has 30.
func certEnding(t *testing.T, domain string, now, notAfter time.Time, ca bool) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	usage := x509.KeyUsageDigitalSignature
	if ca {
		usage |= x509.KeyUsageCertSign
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "*." + domain}, DNSNames: []string{"*." + domain, domain},
		NotBefore: now.Add(-time.Hour), NotAfter: notAfter,
		KeyUsage: usage, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: ca,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func parseCert(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatalf("no PEM block in %q", certPEM)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// strictServerCertProblems is what a TLS library that follows the rules to the letter holds against c as the
// certificate a server presents for domain. Go's own verification (and OpenSSL's) takes an authority's certificate
// from a server, so a handshake with Go proves nothing here: the properties are read one by one.
func strictServerCertProblems(c *x509.Certificate, domain string, now time.Time) []string {
	var out []string
	if c.IsCA || !c.BasicConstraintsValid {
		out = append(out, "it is marked as a certificate authority, or carries no basic constraints")
	}
	if c.KeyUsage != x509.KeyUsageDigitalSignature {
		out = append(out, "key usage is not digitalSignature alone")
	}
	if !slices.Equal(c.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) || len(c.UnknownExtKeyUsage) != 0 {
		out = append(out, "extended key usage is not serverAuth alone")
	}
	if !slices.Equal(c.DNSNames, []string{"*." + domain, domain}) || len(c.IPAddresses)+len(c.EmailAddresses)+len(c.URIs) != 0 {
		out = append(out, "the names are not the wildcard and the apex")
	}
	if !c.NotBefore.Equal(now.Add(-time.Hour).Truncate(time.Second)) || !c.NotAfter.Equal(now.AddDate(5, 0, 0).Truncate(time.Second)) {
		out = append(out, "validity is not from an hour ago to five years from now")
	}
	if pub, ok := c.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		out = append(out, "the key is not ECDSA P-256")
	}
	if c.Subject.String() != c.Issuer.String() || c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) != nil {
		out = append(out, "it is not signed by its own key")
	}
	return out
}

// The certificate a run makes is a server certificate, served as it is and trusted by pinning the same file: a TLS
// server presents exactly it, a client that trusts the published file reaches a sandbox name and the apex, and a
// strict client finds nothing against it. The shape of rc.2 fails the same reading (the control).
func TestSandboxCertificateIsAServerCertificate(t *testing.T) {
	const domain = "sbx.example.org"
	now := time.Now().UTC()
	certPEM, keyPEM, err := selfSignedWildcard(domain, now)
	if err != nil {
		t.Fatal(err)
	}
	if probs := strictServerCertProblems(parseCert(t, certPEM), domain, now); len(probs) != 0 {
		t.Errorf("a strict client refuses the run's certificate: %v", probs)
	}
	old, _ := rc2Certificate(t, domain, now)
	if probs := strictServerCertProblems(parseCert(t, old), domain, now); len(probs) != 2 {
		t.Fatalf("the control: the shape of rc.2 must fail as an authority with keyCertSign and nothing else: %v", probs)
	}

	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	dial := func(trust, name string) (*tls.ConnectionState, error) {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(trust)) {
			t.Fatal("the trust file holds no certificate")
		}
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", ln.Addr().String(), &tls.Config{RootCAs: roots, ServerName: name, MinVersion: tls.VersionTLS12})
		if err != nil {
			return nil, err
		}
		defer c.Close()
		st := c.ConnectionState()
		return &st, nil
	}
	for _, name := range []string{"49983-sbx-0123456789abcdef." + domain, domain} {
		st, err := dial(certPEM, name)
		if err != nil {
			t.Fatalf("%s with the published file as trust: %v", name, err)
		}
		if len(st.PeerCertificates) != 1 || !st.PeerCertificates[0].Equal(parseCert(t, certPEM)) {
			t.Fatalf("%s: the server presents %d certificates, not the published one alone", name, len(st.PeerCertificates))
		}
		if probs := strictServerCertProblems(st.PeerCertificates[0], domain, now); len(probs) != 0 {
			t.Errorf("%s: the presented certificate: %v", name, probs)
		}
	}
	// What it does not cover, and what another file does not open.
	if _, err := dial(certPEM, "a.b."+domain); err == nil {
		t.Error("a name two labels under the domain was accepted")
	}
	if _, err := dial(certPEM, "example.org"); err == nil {
		t.Error("another domain was accepted")
	}
	if _, err := dial(old, "x."+domain); err == nil {
		t.Error("a client that trusts another file was let in")
	}
}

// Which stored certificate a run replaces, and the words the plan carries for it.
func TestCertificateProblem(t *testing.T) {
	const domain = "sbx.example.org"
	now := time.Now().UTC()
	leaf, _, err := selfSignedWildcard(domain, now)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := rc2Certificate(t, domain, now)
	ending, _ := certEnding(t, domain, now, now.AddDate(0, 0, 20), false)
	for _, c := range []struct{ name, cert, domain, want string }{
		{"the run's own", leaf, domain, ""},
		{"the shape of rc.2", old, domain, "marked as a certificate authority"},
		{"another domain", leaf, "other.example.org", "does not name *.other.example.org"},
		{"20 days left", ending, domain, "it ends on " + now.AddDate(0, 0, 20).Format(time.DateOnly)},
		{"not a certificate", "cert", domain, "cannot be read"},
		{"none", "", domain, "cannot be read"},
	} {
		got := sandboxCertificateProblem(c.cert, c.domain)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := certificateProblem(leaf, domain, now.AddDate(4, 11, 0)); got != "" {
		t.Errorf("a new certificate is not valid in four years and eleven months: %s", got)
	}
}

// A platform that built sandboxes with rc.1 or rc.2 holds the refused shape. The plan says that the next apply
// replaces the certificate and that clients must fetch the new one; the apply writes a server certificate to the
// record, the status and the proxy's Secret, and changes nothing else; then the row is gone and the next apply keeps
// the certificate. Both placements go through the same function.
func TestApplyReplacesACertificateOfTheRefusedShape(t *testing.T) {
	const domain = "sbx.example.org"
	for _, placement := range []string{SandboxShared, SandboxSeparate} {
		t.Run(placement, func(t *testing.T) {
			sb := sharedSandbox()
			if placement == SandboxSeparate {
				sb = separateSandbox()
				sb.System.Nodes = []string{"pve1"}
			}
			p := sandboxPlatform(t, RunApply, sb)
			target := p.wl
			if placement == SandboxSeparate {
				target = p.sbx
				p.mutate(func(_ *Run, in *Installation, _ *Secrets) { routed(in) })
				for _, name := range []string{"pc-sandbox-sandbox-pve1-x", "pc-sandbox-sandbox-system-pve1-x"} {
					m := meta(name, map[string]any{"cluster.x-k8s.io/cluster-name": SandboxClusterName})
					m["status"] = map[string]any{"phase": "Running"}
					p.wl.put("/apis/cluster.x-k8s.io/v1beta2/namespaces/pc-sandbox/machines/"+name, m)
				}
			}
			p.upTo(stageSeedPlatform)
			if end := p.until(30); end != "complete" {
				t.Fatalf("end %s: %+v\n%+v\n%s", end, p.stage(stageCluster), p.stage(stageWorkloadPlatform), p.logs)
			}
			row := func() *PlanChange {
				_, in, _ := p.state.snapshot()
				plan, err := PlanFor(in, "")
				if err != nil {
					t.Fatal(err)
				}
				for _, c := range plan.Changes {
					if c.Name == "sandbox-certificate" {
						if len(plan.Blocking) != 0 {
							t.Errorf("the row blocks the apply: %v", plan.Blocking)
						}
						return &c
					}
				}
				return nil
			}
			secret := func(key string) string { // the fake keeps a Secret as the API server does: data, base64
				raw, _ := base64.StdEncoding.DecodeString(str(target.obj(seedNS+"sandbox-system/secrets/sandbox-proxy-tls"), "data", key))
				return string(raw)
			}
			served := func() string { return secret("tls.crt") }
			_, in, sec := p.state.snapshot()
			if probs := strictServerCertProblems(parseCert(t, sec.SandboxTLSCert), domain, parseCert(t, sec.SandboxTLSCert).NotBefore.Add(time.Hour)); len(probs) != 0 ||
				served() != sec.SandboxTLSCert || in.Status.Sandbox.Certificate != sec.SandboxTLSCert {
				t.Fatalf("the first build: %v; served and published agree: %v", probs, served() == sec.SandboxTLSCert && in.Status.Sandbox.Certificate == sec.SandboxTLSCert)
			}
			if c := row(); c != nil {
				t.Fatalf("a row for a certificate the run keeps: %+v", c)
			}

			// What rc.2 left: the refused shape in the record, in the status and in the Secret.
			old, oldKey := rc2Certificate(t, domain, time.Now().UTC())
			p.mutate(func(_ *Run, in *Installation, sec *Secrets) {
				sec.SandboxTLSCert, sec.SandboxTLSKey, in.Status.Sandbox.Certificate = old, oldKey, old
			})
			c := row()
			if c == nil || c.Blocking != "" || c.Stage != stageWorkloadPlatform || c.To != "a new self-signed certificate for *."+domain ||
				!strings.Contains(c.Impact, "marked as a certificate authority") || !strings.Contains(c.Impact, "Clients must fetch the new one") ||
				!strings.Contains(c.Impact, "status.sandbox.certificate") || !strings.Contains(c.Impact, "Nothing else of the sandboxes changes") {
				t.Fatalf("the plan row: %+v", c)
			}
			_, before, _ := p.state.snapshot()
			pveWrites, pools, acl, tokens := len(p.pve.writes), slices.Clone(p.pve.pools), slices.Clone(p.pve.acl), len(p.pve.tokens)
			p.again(stageWorkloadPlatform)
			if end := p.until(30); end != "complete" {
				t.Fatalf("the apply: %s: %+v\n%s", end, p.stage(stageWorkloadPlatform), p.logs)
			}
			_, in, sec = p.state.snapshot()
			next := parseCert(t, sec.SandboxTLSCert)
			if sec.SandboxTLSCert == old || sec.SandboxTLSKey == oldKey || next.IsCA || next.KeyUsage != x509.KeyUsageDigitalSignature {
				t.Fatalf("the apply kept the refused certificate, or made another authority: ca %v, usage %v", next.IsCA, next.KeyUsage)
			}
			if in.Status.Sandbox.Certificate != sec.SandboxTLSCert || served() != sec.SandboxTLSCert {
				t.Fatal("the status or the proxy's Secret does not hold the new certificate")
			}
			if pair, err := tls.X509KeyPair([]byte(served()), []byte(secret("tls.key"))); err != nil || len(pair.Certificate) != 1 {
				t.Fatalf("the Secret's key does not belong to its certificate: %v", err)
			}
			if !strings.Contains(p.logs.String(), "the sandbox certificate is replaced") {
				t.Error("the log does not say that the certificate was replaced")
			}
			// Everything else is as it was: the record but for the certificate, and nothing written on Proxmox.
			was, is := *before.Status.Sandbox, *in.Status.Sandbox
			was.Certificate, is.Certificate = "", ""
			if !reflect.DeepEqual(was, is) || len(p.pve.writes) != pveWrites || !slices.Equal(p.pve.pools, pools) || !slices.Equal(p.pve.acl, acl) || len(p.pve.tokens) != tokens ||
				!slices.Equal(p.pve.pools, []string{pve.SandboxPool}) {
				t.Fatalf("the apply changed more than the certificate: %+v -> %+v; %d Proxmox writes more", was, is, len(p.pve.writes)-pveWrites)
			}
			p.leaks(t, sec.SandboxTLSKey, oldKey)
			if c := row(); c != nil {
				t.Fatalf("the row stays after the apply: %+v", c)
			}
			kept := sec.SandboxTLSCert
			p.again(stageWorkloadPlatform)
			if end := p.until(30); end != "complete" {
				t.Fatalf("the next apply: %s", end)
			}
			if _, _, sec := p.state.snapshot(); sec.SandboxTLSCert != kept {
				t.Error("the next apply made a certificate again")
			}
		})
	}
}

// The plan row comes 30 days before the certificate ends, and not for sandboxes that are being switched off or that
// no run built a certificate for.
func TestPlanSaysWhenTheSandboxCertificateEnds(t *testing.T) {
	const domain = "sbx.example.org"
	b := sandboxBOM(t)
	in := validInstallation(t)
	in.Status.BOM = &BOMRef{Release: b.Release, SHA256: b.SHA256}
	in.Status.Clusters = []ClusterStatus{{Name: ClusterName}}
	in.Spec.Sandbox = sharedSandbox()
	now := time.Now().UTC()
	built := SandboxStatus{Placement: SandboxShared, NodeRange: "10.200.200.64/28", ProxyAddress: "10.200.200.210", Count: 1, Nodes: []string{"pve1"}, BuiltAt: &now}
	in.Status.Sandbox = &built
	names := func() []string {
		p, err := PlanFor(in, "")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range p.Changes {
			out = append(out, c.Name)
		}
		return out
	}
	if got := names(); len(got) != 0 {
		t.Fatalf("no certificate yet: %v", got)
	}
	in.Status.Sandbox.Certificate, _ = certEnding(t, domain, now, now.AddDate(0, 0, 31), false)
	if got := names(); len(got) != 0 {
		t.Fatalf("31 days left: %v", got)
	}
	in.Status.Sandbox.Certificate, _ = certEnding(t, domain, now, now.AddDate(0, 0, 29), false)
	if got := names(); !slices.Equal(got, []string{"sandbox-certificate"}) {
		t.Fatalf("29 days left: %v", got)
	}
	// With more machines asked for, the row stands beside the three rows of the group.
	in.Spec.Sandbox.Count = 2
	if got := names(); !slices.Equal(got, []string{"sandbox", "sandbox-pool", "sandbox-access", "sandbox-certificate"}) {
		t.Fatalf("growth: %v", got)
	}
	in.Spec.Sandbox = &SandboxSpec{Placement: SandboxOff}
	if got := names(); slices.Contains(got, "sandbox-certificate") {
		t.Fatalf("off: %v", got)
	}
}
