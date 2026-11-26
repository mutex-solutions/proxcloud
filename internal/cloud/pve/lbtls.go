package pve

// lbtls.go — TLS on a load balancer: the check of an uploaded certificate, the lines of HAProxy's TLS policy and
// the delivery of the certificate files to an appliance.
//
// A tls listener ends TLS and speaks plain TCP to its members; an https listener ends TLS and speaks HTTP to them.
// Both present the certificates of LBListener.Certificates, chosen by the name the client asks for (SNI); the first
// one answers a client that asks for no name or for one no certificate holds.
//
// A certificate reaches an appliance as one PEM file (chain, then key) in lbCertDir, named by the SHA-256 of its
// chain and never by its id. The config names the files, so a change of certificate is a new file and a new config
// line: it rides the check, swap and roll back of the config (haproxyReload), the file of before is still there if
// HAProxy refuses the new config, and the config's hash covers which certificates are served. It does not cover
// the files' bytes: a file damaged or removed on the appliance leaves the hash as it was, so the applied-state read
// asks for the files too (certstate, lbCertFaults).

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// lbCertDir holds the certificate files of an appliance: root's, mode 0700, each file 0600.
	lbCertDir = "/etc/haproxy/certs"
	// lbMaxListenerCerts: HAProxy reads a config line of at most 2048 characters, and a certificate takes 92 of the
	// bind line.
	lbMaxListenerCerts = 10
	// LBMaxCertBundle is what a certificate's chain and key may hold together, and lbMaxChain how many certificates
	// the chain may hold: the server's own and its intermediates.
	LBMaxCertBundle = 16 << 10
	lbMaxChain      = 5

	// haproxyTLS is the TLS policy of every tls and https listener, written into "global" only when the config holds
	// one: TLS 1.2 and 1.3, the three TLS 1.3 suites, and for TLS 1.2 the six suites with forward secrecy and an
	// AEAD cipher, for an RSA and for an ECDSA certificate. No suite with DHE: HAProxy has no DH parameters here, so
	// it would offer none anyway. HAProxy takes the list for TLS 1.2 and the one for TLS 1.3 in two directives.
	// (Where the list comes from: docs/architecture/c4/CHANGELOG.md, 2026-11-23.)
	haproxyTLS = `  ssl-default-bind-options ssl-min-ver TLSv1.2
  ssl-default-bind-ciphersuites TLS_AES_128_GCM_SHA256:TLS_AES_256_GCM_SHA384:TLS_CHACHA20_POLY1305_SHA256
  ssl-default-bind-ciphers ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-CHACHA20-POLY1305:ECDHE-RSA-CHACHA20-POLY1305
`
)

var (
	// lbCertIDRe is what a listener may name as a certificate: the id of a certificate of the project.
	lbCertIDRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	// lbCertFileRe is a certificate file's name without ".pem": a SHA-256 in hexadecimal digits, so nothing else can
	// become a path on an appliance or a word of the config.
	lbCertFileRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// lbTerminatesTLS: the listener protocol ends TLS on the balancer.
func lbTerminatesTLS(protocol string) bool { return protocol == "tls" || protocol == "https" }

// lbCertFiles is the listener's certificate files as the config may name them: none when one is not a file name.
func lbCertFiles(l LBListener) []string {
	if len(l.CertFiles) == 0 || len(l.CertFiles) > lbMaxListenerCerts || slices.ContainsFunc(l.CertFiles, func(f string) bool { return !lbCertFileRe.MatchString(f) }) {
		return nil
	}
	return l.CertFiles
}

// lbBindTLS is what follows the address of a tls or https listener's bind line.
func lbBindTLS(l LBListener) string {
	var b strings.Builder
	b.WriteString(" ssl")
	for _, f := range lbCertFiles(l) {
		b.WriteString(" crt " + lbCertDir + "/" + f + ".pem")
	}
	if l.Protocol == "https" {
		b.WriteString(" alpn h2,http/1.1")
	}
	return b.String()
}

// LBCertFile is one certificate as an appliance holds it: Name is the SHA-256 of its chain, PEM the chain and then
// the key. PEM is a secret: it is written to the appliance and to nothing else.
type LBCertFile struct {
	Name string
	PEM  string
}

// String and LogValue keep a certificate file out of a log line whatever prints it.
func (LBCertFile) String() string       { return "[certificate file redacted]" }
func (LBCertFile) LogValue() slog.Value { return slog.StringValue("[certificate file redacted]") }

// LBCertificate is what a certificate says of itself, with nothing of its key but the kind.
type LBCertificate struct {
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	DNSNames    []string  `json:"dnsNames"` // the names and addresses it is valid for (subject alternative names)
	NotBefore   time.Time `json:"notBefore"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"fingerprint"` // SHA-256 of the server's certificate, as openssl x509 -fingerprint prints it without the colons
	KeyType     string    `json:"keyType"`     // "RSA 2048", "ECDSA P-256"
	ChainLength int       `json:"chainLength"`
	// File is the name of the certificate's file on an appliance: the SHA-256 of the whole chain.
	File string `json:"file"`
}

// CheckLBCertificate checks an uploaded certificate: chainPEM holds the server's certificate first and then its
// intermediates, keyPEM the private key. It answers what the certificate says, the file an appliance gets (the chain
// and the key, written afresh so that the same upload gives the same bytes), warnings that do not refuse it, and a
// text for the customer when it is refused. No text holds anything of the key.
func CheckLBCertificate(chainPEM, keyPEM string, now time.Time) (info LBCertificate, bundle string, warnings []string, refused string) {
	if len(chainPEM)+len(keyPEM) > LBMaxCertBundle {
		return info, "", nil, fmt.Sprintf("The certificate and its key are larger than %d KiB together", LBMaxCertBundle>>10)
	}
	var chain []*x509.Certificate
	for rest := []byte(chainPEM); ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			if len(bytes.TrimSpace(rest)) != 0 {
				return info, "", nil, "The certificate is not PEM text: it holds something outside its BEGIN and END lines"
			}
			break
		}
		if block.Type != "CERTIFICATE" {
			return info, "", nil, "The certificate field takes certificates only: the private key goes into its own field"
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return info, "", nil, fmt.Sprintf("Certificate %d of the chain cannot be read", len(chain)+1)
		}
		chain = append(chain, c)
	}
	switch {
	case len(chain) == 0:
		return info, "", nil, "The certificate is not PEM text: no BEGIN CERTIFICATE line was found"
	case len(chain) > lbMaxChain:
		return info, "", nil, fmt.Sprintf("The chain holds more than %d certificates: send the server's certificate and its intermediates, not the root store", lbMaxChain)
	}
	block, rest := pem.Decode([]byte(keyPEM))
	switch {
	case block == nil || len(bytes.TrimSpace(rest)) != 0:
		return info, "", nil, "The private key is not PEM text with one key in it"
	case block.Type == "ENCRYPTED PRIVATE KEY" || strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED"):
		return info, "", nil, "The private key is encrypted with a passphrase. Remove the passphrase before the upload: the platform encrypts the key itself"
	}
	var key any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return info, "", nil, "The private key is not PEM text with one key in it"
	}
	if err != nil {
		return info, "", nil, "The private key cannot be read"
	}
	leaf := chain[0]
	const kinds = "The key must be RSA of 2048 to 8192 bits, or ECDSA on the curve P-256 or P-384"
	switch k := key.(type) {
	case *rsa.PrivateKey:
		if bits := k.N.BitLen(); bits < 2048 || bits > 8192 {
			return info, "", nil, kinds
		}
		if pub, ok := leaf.PublicKey.(*rsa.PublicKey); !ok || !k.PublicKey.Equal(pub) {
			return info, "", nil, "The private key does not belong to the certificate: the first certificate must be the server's own"
		}
		info.KeyType = fmt.Sprintf("RSA %d", k.N.BitLen())
	case *ecdsa.PrivateKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return info, "", nil, kinds
		}
		if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || !k.PublicKey.Equal(pub) {
			return info, "", nil, "The private key does not belong to the certificate: the first certificate must be the server's own"
		}
		info.KeyType = "ECDSA " + k.Curve.Params().Name
	default:
		return info, "", nil, kinds
	}
	// A signature over an MD2, MD5 or SHA-1 digest. The HAProxy of an Ubuntu 26.04 appliance refuses the certificate
	// file (OpenSSL: "ca md too weak", for the server's certificate as for an intermediate), so the config that names
	// it was refused at the push; the one of a Debian 13 appliance loads it. The upload refuses on every site what
	// one of the two refuses (TestLBApplianceTLS, step 1b, asks both). OpenSSL does not look at the signature of a
	// certificate that signed itself, and neither does this: an old root at the end of a chain is taken.
	// ponytail: "signed itself" is read from the two names alone, so a certificate issued under its own name by
	// another key passes here and is refused by that appliance, as before; compare the key identifiers too if one
	// turns up. This stands before the chain's order: Go refuses a SHA-1 signature there too, and the answer would be
	// that the next certificate did not sign this one.
	for i, c := range chain {
		switch c.SignatureAlgorithm {
		case x509.MD2WithRSA, x509.MD5WithRSA, x509.SHA1WithRSA, x509.DSAWithSHA1, x509.ECDSAWithSHA1:
			if !bytes.Equal(c.RawSubject, c.RawIssuer) {
				return info, "", nil, fmt.Sprintf("Certificate %d of the chain is signed with %s, which the load balancer does not accept: have it issued again with SHA-256 or stronger", i+1, c.SignatureAlgorithm)
			}
		}
	}
	for i := 0; i+1 < len(chain); i++ {
		if chain[i].CheckSignatureFrom(chain[i+1]) != nil {
			return info, "", nil, fmt.Sprintf("Certificate %d of the chain did not sign certificate %d: put the server's certificate first, then each intermediate that signed the one before it", i+2, i+1)
		}
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return info, "", nil, "The private key cannot be read"
	}
	var out bytes.Buffer
	whole := sha256.New()
	for _, c := range chain {
		_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
		whole.Write(c.Raw)
	}
	_ = pem.Encode(&out, &pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sum := sha256.Sum256(leaf.Raw)
	info.Subject, info.Issuer = leaf.Subject.String(), leaf.Issuer.String()
	info.NotBefore, info.NotAfter = leaf.NotBefore.UTC(), leaf.NotAfter.UTC()
	info.Fingerprint, info.File, info.ChainLength = hex.EncodeToString(sum[:]), hex.EncodeToString(whole.Sum(nil)), len(chain)
	info.DNSNames = slices.Clone(leaf.DNSNames)
	for _, ip := range leaf.IPAddresses {
		info.DNSNames = append(info.DNSNames, ip.String())
	}
	switch {
	case now.After(leaf.NotAfter):
		warnings = append(warnings, "This certificate expired on "+leaf.NotAfter.UTC().Format("2 January 2006")+": clients will refuse it")
	case now.Before(leaf.NotBefore):
		warnings = append(warnings, "This certificate is not valid before "+leaf.NotBefore.UTC().Format("2 January 2006"))
	}
	if len(info.DNSNames) == 0 {
		info.DNSNames = []string{}
		warnings = append(warnings, "This certificate names no host (it has no subject alternative name): browsers refuse it")
	}
	return info, out.String(), warnings, ""
}

const (
	// lbCertStateFn defines certstate(): what the certificate directory and every certificate file in it is, in
	// three commands whatever the number of files: "crtdir <mode>:<owner>:<group>:<kind>", then
	// "crt <path> <mode>:<owner>:<group>" per file, then sha256sum's own "<digest>  <path>" per file. The digest is
	// all it says of a file's content. The file's name is the SHA-256 of the chain's certificates, not of the file's
	// bytes (which hold the key too), so the digest is compared by the portal with the one of the file it would
	// write (lbCertFaults), and no digest travels in a command line: the guest agent logs those.
	//
	// Why not "haproxy -c" on the config in use: it would start a second HAProxy that parses every key on each
	// pass, it prints HAProxy's own words about a file, it does not see a file that was exchanged for another valid
	// one or a file anybody may read, and it cannot say which file to write again.
	lbCertStateFn = `certstate() (
  stat -c 'crtdir %a:%U:%G:%F' ` + lbCertDir + ` 2>/dev/null || :
  stat -c 'crt %n %a:%U:%G' ` + lbCertDir + `/*.pem 2>/dev/null || :
  sha256sum ` + lbCertDir + `/*.pem 2>/dev/null || :
)
`
	// lbCertDirScript makes lbCertDir a directory only root can enter, and proves it, before anything is written
	// into it: the guest agent creates a file with mode 0666 whatever the umask, so a key is only ever written
	// inside a directory nobody else reaches, under its temporary name too. A leftover of a push that did not end
	// goes. It ends in certstate: the push writes only the files that are not there as it would write them.
	lbCertDirScript = lbCertStateFn + `set -e
D=` + lbCertDir + `
[ ! -L $D ] || { echo "$D is a link" >&2; exit 1; }
mkdir -p $D
chown root:root $D
chmod 0700 $D
[ "$(stat -c %a:%U:%G:%F $D)" = "700:root:root:directory" ] || { echo "$D is not a directory of root alone" >&2; exit 1; }
rm -f $D/*.new
certstate`
	// lbCertPrepareScript makes each file the push is about to write, empty, root's alone and with mode 0600, and
	// proves it, before a byte of a key is in it. The guest agent gives mode 0666 to a file it creates and leaves
	// the mode of one that exists (qemu-ga's guest-file-open: it creates with O_EXCL and sets the mode then, and
	// only opens a file that is there; measured in the spike of v0.11.1, "an overwrite keeps 0600", and held on a
	// real guest's file system by the watcher of test/integration/lb_tls_test.go). Until v0.11.1-rc.2 the agent
	// created the file and the chmod came a command later: a key was in a 0666 file for that moment, behind the
	// 0700 directory alone (drill R3-o4). Its arguments are the names.
	lbCertPrepareScript = `set -e
umask 077
D=` + lbCertDir + `
for n in "$@"; do
  rm -f $D/$n.pem.new
  : > $D/$n.pem.new
  [ "$(stat -c %a:%U:%G $D/$n.pem.new)" = 600:root:root ] || { echo "a certificate file could not be made root's alone" >&2; exit 1; }
done`
	// lbCertInstallScript gives each file written as <name>.pem.new to root with mode 0600, proves it, and moves it
	// in place. Its arguments are the names. The chown and chmod change nothing on a file that is as it was made
	// (lbCertPrepareScript); they stay for a guest agent that would not leave a file's mode alone.
	lbCertInstallScript = `set -e
D=` + lbCertDir + `
for n in "$@"; do
  chown root:root $D/$n.pem.new
  chmod 0600 $D/$n.pem.new
  [ "$(stat -c %a:%U:%G $D/$n.pem.new)" = 600:root:root ] || { echo "a certificate file is not root's alone" >&2; exit 1; }
  mv -f $D/$n.pem.new $D/$n.pem
done`
	// lbCertPrune, after a config was seen to be served, removes every certificate file it does not name. Before
	// that the files of the config of before stay: it is what HAProxy is put back on when it refuses the new one.
	lbCertPrune = `for c in ` + lbCertDir + `/*.pem ` + lbCertDir + `/*.new; do [ -e "$c" ] || continue; grep -qF " $c" $F || rm -f "$c"; done
`
)

// lbCertFaults reads certstate's lines (out) for certs, the files a config names. stale are the certificates whose
// file is not there, is not the bytes the portal would write, or is not root's alone with mode 0600; why says the
// first thing found wrong, the directory's mode included, in words that hold the start of a file's name and nothing
// of its content, and is "" when everything is as a push leaves it.
func lbCertFaults(out string, certs []LBCertFile) (stale []LBCertFile, why string) {
	if len(certs) == 0 {
		return nil, ""
	}
	dir, mode, sum := false, map[string]string{}, map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		switch f := strings.Fields(line); {
		case len(f) == 2 && f[0] == "crtdir":
			dir = f[1] == "700:root:root:directory"
		case len(f) == 3 && f[0] == "crt":
			mode[f[1]] = f[2]
		case len(f) == 2 && len(f[0]) == sha256.Size*2:
			sum[f[1]] = f[0]
		}
	}
	if !dir {
		why = "The certificate directory is missing or is not root's alone"
	}
	for _, c := range certs {
		path, want, fault := lbCertDir+"/"+c.Name+".pem", sha256.Sum256([]byte(c.PEM)), ""
		switch {
		case mode[path] == "":
			fault = "is missing"
		case sum[path] != hex.EncodeToString(want[:]):
			fault = "is not the file that was written"
		case mode[path] != "600:root:root":
			fault = "is not root's alone"
		default:
			continue
		}
		stale = append(stale, c)
		if why == "" {
			why = "Certificate file " + c.Name[:min(12, len(c.Name))] + " " + fault
		}
	}
	return stale, why
}

// pushCertificates brings the certificate files a config names in place, before the config: the directory first,
// made and proven private, then each file the appliance does not hold as the portal would write it (missing,
// other bytes, another mode or owner: the directory's command says what is there) under a temporary name inside
// it, then mode, owner and the move in place. A file that is there already, byte for byte and root's alone, is not
// written again: a write is one call through the guest agent, about 0.6 s, inside the tenant's own request. A file
// that is written is made first, empty and with mode 0600 (lbCertPrepareScript): the agent's write fills it and
// leaves its mode. written is how many files the push wrote.
func (s *Service) pushCertificates(ctx context.Context, node string, vmid int, certs []LBCertFile) (written int, err error) {
	if len(certs) == 0 {
		return 0, nil
	}
	for i, c := range certs {
		if !lbCertFileRe.MatchString(c.Name) {
			return 0, fmt.Errorf("certificate file %d has no valid name", i+1)
		}
	}
	exit, out, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", lbCertDirScript)
	if err != nil {
		return 0, err
	}
	if exit != 0 {
		return 0, fmt.Errorf("the appliance's certificate directory could not be made private, nothing was written: %s", strings.TrimSpace(stderr))
	}
	stale, _ := lbCertFaults(out, certs)
	if len(stale) == 0 {
		return 0, nil
	}
	names := make([]string, len(stale))
	for i, c := range stale {
		names[i] = c.Name
	}
	exit, _, stderr, err = s.C.GuestRun(ctx, node, vmid, guestTimeout, append([]string{"sh", "-c", lbCertPrepareScript, "sh"}, names...)...)
	if err != nil {
		return 0, err
	}
	if exit != 0 {
		return 0, fmt.Errorf("the certificate files could not be made private, nothing was written: %s", strings.TrimSpace(stderr))
	}
	for _, c := range stale {
		if err := s.C.GuestWriteFile(ctx, node, vmid, lbCertDir+"/"+c.Name+".pem.new", c.PEM); err != nil {
			return 0, fmt.Errorf("write certificate file %s: %w", c.Name[:12], err)
		}
	}
	exit, _, stderr, err = s.C.GuestRun(ctx, node, vmid, guestTimeout, append([]string{"sh", "-c", lbCertInstallScript, "sh"}, names...)...)
	if err != nil {
		return 0, err
	}
	if exit != 0 {
		return 0, fmt.Errorf("the certificate files could not be put in place: %s", strings.TrimSpace(stderr))
	}
	return len(stale), nil
}
