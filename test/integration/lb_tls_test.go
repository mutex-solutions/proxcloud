//go:build integration

package integration

import (
	"bufio"
	"context"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve/lbcerttest"
)

// lbTLSMembers starts the members in the guest: an HTTP one that answers with the head of the request it got, and a
// TCP one that echoes.
const lbTLSMembers = `cat > /usr/local/bin/lb-head <<'EOF'
#!/bin/sh
head=""
while IFS= read -r line; do
  line=$(printf %s "$line" | tr -d '\r')
  [ -n "$line" ] || break
  head="$head$line
"
done
printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %s\r\nConnection: close\r\n\r\n%s' "${#head}" "$head"
EOF
chmod 0755 /usr/local/bin/lb-head
systemd-run -q --unit=lb-tls-http socat TCP-LISTEN:18080,bind=127.0.0.1,fork,reuseaddr EXEC:/usr/local/bin/lb-head
systemd-run -q --unit=lb-tls-echo socat TCP-LISTEN:19000,bind=127.0.0.1,fork,reuseaddr EXEC:cat`

// TLS on an appliance, in a real guest of each image an appliance is built from: the distribution's HAProxy and
// OpenSSL, real certificates (a root, an intermediate, a server certificate; RSA and ECDSA), the product's own push
// through the guest agent API, and real handshakes from outside the guest. Nothing of the guest is modelled; the
// agent creates every file with mode 0666 as the real one does.
//
// Shown: the certificate directory and files as the product leaves them; the certificate chosen by the name a
// client asks for, the first one for any other name and for none; TLS 1.2 and 1.3 accepted, 1.1 and 1.0 refused;
// no suite without forward secrecy, none with CBC, none with DHE; HTTP/2 offered on https alone; the two forwarded
// headers; a change of certificate that drops no connection and fails no handshake; a config HAProxy refuses, which
// leaves the certificate in use in place; a certificate file truncated, deleted or opened up behind HAProxy, which the
// state read finds and the next push puts right before a start of HAProxy would refuse the config; and the files a
// config no longer names, gone after it is served.
func TestLBApplianceTLS(t *testing.T) {
	images := []string{"debian:trixie", "ubuntu:26.04"}
	// Per certificate the upload refuses for its signature (step 1b): the images that were asked, and the ones whose
	// HAProxy refused it.
	sigAsked, sigRefused := map[string]int{}, map[string]int{}
	defer func() {
		for what, n := range sigAsked {
			if n == len(images) && sigRefused[what] == 0 {
				t.Errorf("%s: the upload refuses it for its signature and the HAProxy of no appliance image does", what)
			}
		}
	}()
	for _, image := range images {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
			defer cancel()
			// openssl is the test's own client for the suites Go's TLS has none of.
			c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
				ContainerRequest: testcontainers.ContainerRequest{
					Image:        lbImage(image),
					Cmd:          lbInit("systemd-sysv", "procps", "openssl"),
					Privileged:   true,
					Tmpfs:        map[string]string{"/run": "", "/run/lock": ""},
					ExposedPorts: []string{"443/tcp", "8443/tcp"},
				},
				Started: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Terminate(context.Background()) })
			g := &guestAgent{t: t, c: c, runs: map[int]*guestRun{}}
			g.booted(ctx)
			srv := httptest.NewServer(g)
			defer srv.Close()
			ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
			if err != nil {
				t.Fatal(err)
			}
			host, err := c.Host(ctx)
			if err != nil {
				t.Fatal(err)
			}
			addr := func(port string) string {
				t.Helper()
				p, err := c.MappedPort(ctx, port)
				if err != nil {
					t.Fatal(err)
				}
				return net.JoinHostPort(host, p.Port())
			}
			https, tlsTCP := addr("443/tcp"), addr("8443/tcp")

			// Three certificates as a tenant uploads them, through the upload's own check.
			until := time.Now().Add(72 * time.Hour)
			upload := func(c lbcerttest.Cert) pve.LBCertFile {
				t.Helper()
				info, bundle, warnings, refused := pve.CheckLBCertificate(c.Chain, c.Key, time.Now())
				if refused != "" || len(warnings) != 0 {
					t.Fatalf("upload: %q %q", refused, warnings)
				}
				return pve.LBCertFile{Name: info.File, PEM: bundle}
			}
			certA := lbcerttest.New(t, lbcerttest.RSA(t, 2048), until, "a.example.org")
			certB := lbcerttest.New(t, lbcerttest.EC(t, elliptic.P256()), until, "b.example.org")
			certA2 := lbcerttest.New(t, lbcerttest.RSA(t, 2048), until, "a.example.org") // the one that replaces A
			fileA, fileB, fileA2 := upload(certA), upload(certB), upload(certA2)
			spec := func(first pve.LBCertFile) pve.LBSpec {
				return pve.LBSpec{
					Listeners: []pve.LBListener{
						{Port: 443, Protocol: "https", Pool: "web", Certificates: []string{"a", "b"}, CertFiles: []string{first.Name, fileB.Name}},
						{Port: 8443, Protocol: "tls", Pool: "echo", Certificates: []string{"a"}, CertFiles: []string{first.Name}},
					},
					Pools: []pve.LBPool{
						{Name: "web", Protocol: "http", Algorithm: "roundrobin", Members: []pve.LBMember{{ServerID: "101", Port: 18080, Weight: 1, Address: "127.0.0.1"}}},
						{Name: "echo", Protocol: "tcp", Algorithm: "roundrobin", Members: []pve.LBMember{{ServerID: "102", Port: 19000, Weight: 1, Address: "127.0.0.1"}}},
					},
				}
			}
			certDir := func() string {
				t.Helper()
				return g.run(ctx, `cd /etc/haproxy/certs && for f in *; do [ -e "$f" ] && stat -c '%n %a:%U:%G' "$f"; done; stat -c 'dir %a:%U:%G:%F' .`)
			}
			holds := func(files ...pve.LBCertFile) string {
				lines := []string{}
				for _, f := range files {
					lines = append(lines, f.Name+".pem 600:root:root")
				}
				slices.Sort(lines)
				return strings.Join(append(lines, "dir 700:root:root:directory"), "\n")
			}

			// A watcher on the mode of every file a push writes into the certificate directory, as fast as the guest
			// runs it, for the whole test: a file that holds a key never has a mode wider than 0600, not for the moment
			// between the agent's write and the chmod either (drill R3-o4 of v0.11.1-rc.2: the agent created it 0666).
			g.run(ctx, `printf '%s\n' 'while :; do stat -c "%a %n" /etc/haproxy/certs/*.new 2>/dev/null | grep -v "^600 " >> /root/cert-modes; done' > /root/cert-watch.sh
: > /root/cert-modes; systemd-run --quiet --unit=cert-watch sh /root/cert-watch.sh`)

			// 1. The first push (on the plain image through the product's own install): the directory and the files
			// before the config.
			config := pve.RenderHAProxy(spec(fileA))
			for deadline := time.Now().Add(8 * time.Minute); ; time.Sleep(time.Second) {
				_, err := ps.PushHAProxy(ctx, "n1", 100, config, fileA, fileB)
				if err == nil {
					break
				}
				if !errors.Is(err, pve.ErrLBNotReady) || time.Now().After(deadline) {
					t.Fatalf("first push: %v", err)
				}
			}
			// The control: a file the agent creates in that directory by itself, as every certificate file was
			// created until v0.11.1-rc.2, is 0666 and the watcher sees it. It is all the watcher has seen: the two
			// files of the first push were never wider than 0600.
			const control = "/etc/haproxy/certs/control.pem.new"
			if err := ps.C.GuestWriteFile(ctx, "n1", 100, control, "x"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(300 * time.Millisecond)
			if got := g.run(ctx, "rm -f "+control+"; sort -u /root/cert-modes"); got != "666 "+control {
				t.Fatalf("the watcher after the first push and the control saw %q, want the control's file alone, with mode 666", got)
			}
			t.Log(g.run(ctx, "haproxy -v | head -n1; openssl version"))
			// The members come up after HAProxy (socat is the image's, or came with the product's install): wait for their checks.
			g.run(ctx, lbTLSMembers)
			for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
				health, _, err := ps.LBHealth(ctx, "n1", 100, spec(fileA))
				if err == nil && len(health) == 2 && health[0].Status == "UP" && health[1].Status == "UP" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the members did not come up: %+v %v", health, err)
				}
			}
			if got := certDir(); got != holds(fileA, fileB) {
				t.Fatalf("the certificate directory after the first push:\n%s\nwant\n%s", got, holds(fileA, fileB))
			}
			if ok, why, err := ps.HAProxyServing(ctx, "n1", 100, lbSum(config)); err != nil || !ok {
				t.Fatalf("serving: %v %q %v", ok, why, err)
			}
			// The key, searched for in every file of the guest by a line of its own text: it is in the certificate files
			// and in nothing else (no temporary file, no journal, no state file, no copy of the config).
			keyFiles := func() string {
				t.Helper()
				find := "grep -rlaF"
				for _, f := range []pve.LBCertFile{fileA, fileB, fileA2} {
					_, key, _ := strings.Cut(f.PEM, "-----BEGIN PRIVATE KEY-----\n")
					find += " -e '" + strings.SplitN(key, "\n", 3)[1] + "'"
				}
				return g.run(ctx, find+" --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev / 2>/dev/null | sort")
			}
			inDir := func(files ...pve.LBCertFile) string {
				paths := []string{}
				for _, f := range files {
					paths = append(paths, "/etc/haproxy/certs/"+f.Name+".pem")
				}
				slices.Sort(paths)
				return strings.Join(paths, "\n")
			}
			if got := keyFiles(); got != inDir(fileA, fileB) {
				t.Errorf("files of the guest that hold a key after the first push:\n%s\nwant\n%s", got, inDir(fileA, fileB))
			}
			// HAProxy has no word to say on the config, the policy lines among it; nor on the goldens with real files.
			if out := g.run(ctx, "haproxy -c -f /etc/haproxy/haproxy.cfg 2>&1 || true"); strings.Contains(out, "WARNING") || strings.Contains(out, "ALERT") {
				t.Errorf("haproxy -c on the pushed config:\n%s", out)
			}
			for _, name := range []string{"haproxy-tls.cfg", "haproxy-tls-pair.cfg", "haproxy-tls-k8s.cfg"} {
				golden, err := os.ReadFile("../../internal/cloud/pve/testdata/" + name)
				if err != nil {
					t.Fatal(err)
				}
				at := strings.NewReplacer(strings.Repeat("a1", 32), fileA.Name, strings.Repeat("b2", 32), fileB.Name).Replace(string(golden))
				if err := c.CopyToContainer(ctx, []byte(at), "/root/"+name, 0o644); err != nil {
					t.Fatal(err)
				}
				if out := g.run(ctx, "haproxy -c -f /root/"+name+" 2>&1"); strings.Contains(out, "WARNING") || strings.Contains(out, "ALERT") {
					t.Errorf("haproxy -c on %s:\n%s", name, out)
				}
			}

			// 1b. The signature of a certificate: the upload's check and this image's HAProxy, asked about the same
			// bytes. The config is the product's own with the first certificate's file exchanged, so its TLS policy
			// lines are in force; `haproxy -c` loads the file as a push's check does. The certificates signed with
			// SHA-1 and SHA-256 are made here; the MD5, the DSA, the RSA-PSS, the SHA-224 and the RIPEMD-160 ones by
			// the guest's openssl, which Go has no signer for. Go has no name either for the last four (RSA-PSS with
			// SHA-1, SHA-224, RSA-PSS with SHA-256 and a salt of 20 bytes, RSA with RIPEMD-160): the upload reads the
			// digest of the first from its parameters and the last by its identifier and refuses them, and takes the
			// other two, as both HAProxy do. Held for every case on
			// every image: what this HAProxy refuses, the upload refused (a certificate taken by the upload and
			// refused at the push was the fault), and the controls are taken by both. The images do not agree on the weak ones (Ubuntu 26.04's OpenSSL refuses them, "ca md
			// too weak"; Debian 13's loads them), so the other direction is held over the images together, at the
			// end of the test: what the upload refuses, the HAProxy of one image at least refuses.
			sigKey := lbcerttest.RSA(t, 2048)
			sigged := func(sign lbcerttest.Sign) lbcerttest.Cert {
				return lbcerttest.NewSigned(t, sigKey, until, sign, "sig.example.org")
			}
			first := func(chain string) string {
				return chain[:strings.Index(chain, "-----END CERTIFICATE-----")+len("-----END CERTIFICATE-----\n")]
			}
			sha1Leaf, sha1RSA := sigged(lbcerttest.Sign{Leaf: x509.ECDSAWithSHA1}), sigged(lbcerttest.Sign{Leaf: x509.SHA1WithRSA, RSA: true})
			sha1Mid, sha1Root := sigged(lbcerttest.Sign{Intermediate: x509.ECDSAWithSHA1}), sigged(lbcerttest.Sign{Root: x509.ECDSAWithSHA1})
			sha256EC, sha256RSA := sigged(lbcerttest.Sign{}), sigged(lbcerttest.Sign{RSA: true})
			selfSHA1 := lbcerttest.SelfSigned(t, sigKey, until, x509.SHA1WithRSA, "sig.example.org")
			var ossl []string // by the guest's openssl: MD5, DSA with SHA-1, one that signed itself with MD5, RSA-PSS with SHA-1, SHA-224, RSA-PSS with SHA-256, their key, RIPEMD-160
			for rest := []byte(g.run(ctx, `set -e; mkdir /root/sig; cd /root/sig; q() { "$@" >/dev/null 2>&1; }
q openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.pem -subj /CN=sig-ca -days 2
q openssl dsaparam -genkey -out dsa.key 2048
q openssl req -x509 -new -key dsa.key -sha256 -out dsa.pem -subj /CN=sig-dsa-ca -days 2
q openssl req -newkey rsa:2048 -nodes -keyout leaf.key -out leaf.csr -subj /CN=sig.example.org
q openssl x509 -req -in leaf.csr -CA ca.pem -CAkey ca.key -CAcreateserial -md5 -days 2 -out md5.pem
q openssl x509 -req -in leaf.csr -CA dsa.pem -CAkey dsa.key -CAcreateserial -sha1 -days 2 -out dsasha1.pem
q openssl req -x509 -new -key leaf.key -md5 -out self.pem -subj /CN=sig.example.org -days 2
q openssl x509 -req -in leaf.csr -CA ca.pem -CAkey ca.key -CAcreateserial -sha1 -sigopt rsa_padding_mode:pss -days 2 -out pss1.pem
q openssl x509 -req -in leaf.csr -CA ca.pem -CAkey ca.key -CAcreateserial -sha224 -days 2 -out sha224.pem
q openssl x509 -req -in leaf.csr -CA ca.pem -CAkey ca.key -CAcreateserial -sha256 -sigopt rsa_padding_mode:pss -sigopt rsa_pss_saltlen:20 -days 2 -out pss256.pem
q openssl x509 -req -in leaf.csr -CA ca.pem -CAkey ca.key -CAcreateserial -ripemd160 -days 2 -out rmd160.pem
cat md5.pem dsasha1.pem self.pem pss1.pem sha224.pem pss256.pem leaf.key rmd160.pem`)); ; {
				var block *pem.Block
				if block, rest = pem.Decode(rest); block == nil {
					break
				}
				ossl = append(ossl, lbcerttest.PEM(block.Type, block.Bytes))
			}
			if len(ossl) != 8 {
				t.Fatalf("the guest's openssl made %d PEM blocks, want seven certificates and a key", len(ossl))
			}
			for i, want := range map[int]x509.SignatureAlgorithm{0: x509.MD5WithRSA, 1: x509.DSAWithSHA1, 2: x509.MD5WithRSA, 3: x509.UnknownSignatureAlgorithm, 4: x509.UnknownSignatureAlgorithm, 5: x509.UnknownSignatureAlgorithm, 7: x509.UnknownSignatureAlgorithm} {
				block, _ := pem.Decode([]byte(ossl[i]))
				if got, err := x509.ParseCertificate(block.Bytes); err != nil {
					t.Fatalf("certificate %d of the guest's openssl: %v", i+1, err)
				} else if got.SignatureAlgorithm != want {
					t.Fatalf("certificate %d of the guest's openssl is signed with %v, want %v", i+1, got.SignatureAlgorithm, want)
				}
			}
			if err := c.CopyToContainer(ctx, []byte(strings.ReplaceAll(config, "/etc/haproxy/certs/"+fileA.Name+".pem", "/root/sig.pem")), "/root/sig.cfg", 0o644); err != nil {
				t.Fatal(err)
			}
			for _, sc := range []struct {
				what, chain, key string
				weak             bool // the upload refuses it for its signature
			}{
				{"SHA-256 of ECDSA authorities (the control)", sha256EC.Chain, sha256EC.Key, false},
				{"SHA-256 of RSA authorities (the control)", sha256RSA.Chain, sha256RSA.Key, false},
				{"a SHA-1 server certificate alone", first(sha1Leaf.Chain), sha1Leaf.Key, true},
				{"a SHA-1 server certificate with its intermediate", sha1Leaf.Chain, sha1Leaf.Key, true},
				{"a SHA-1 server certificate of an RSA authority", sha1RSA.Chain, sha1RSA.Key, true},
				{"a SHA-1 intermediate", sha1Mid.Chain, sha1Mid.Key, true},
				{"an MD5 server certificate", ossl[0], ossl[6], true},
				{"a DSA with SHA-1 server certificate", ossl[1], ossl[6], true},
				{"an RSA-PSS with SHA-1 server certificate", ossl[3], ossl[6], true},
				{"a RIPEMD-160 server certificate", ossl[7], ossl[6], true},
				{"a SHA-224 server certificate (Go has no name for it; the control)", ossl[4], ossl[6], false},
				{"an RSA-PSS with SHA-256 and a salt of 20 bytes server certificate (Go has no name for it; the control)", ossl[5], ossl[6], false},
				{"a SHA-1 certificate that signed itself", selfSHA1.Chain, selfSHA1.Key, false},
				{"an MD5 certificate that signed itself", ossl[2], ossl[6], false},
				{"a chain that ends in its SHA-1 root", sha1Root.Chain + sha1Root.Root, sha1Root.Key, false},
			} {
				_, _, _, refused := pve.CheckLBCertificate(sc.chain, sc.key, time.Now())
				if err := c.CopyToContainer(ctx, []byte(sc.chain+sc.key), "/root/sig.pem", 0o600); err != nil {
					t.Fatal(err)
				}
				code, out, _ := g.sh(ctx, "sig", "sh", "-c", "haproxy -c -f /root/sig.cfg 2>&1")
				said := ""
				if i := strings.Index(out, "unable to load"); i >= 0 {
					said, _, _ = strings.Cut(out[i:], "\n")
				}
				t.Logf("%s: the upload %q; haproxy -c exit %d %s", sc.what, refused, code, said)
				switch {
				case code != 0 && !strings.Contains(out, "/root/sig.pem"):
					t.Fatalf("%s: haproxy -c refuses the config for something else than the certificate:\n%s", sc.what, out)
				case code != 0 && refused == "":
					t.Errorf("%s: taken by the upload and refused by this HAProxy:\n%s", sc.what, out)
				case (refused != "") != sc.weak || sc.weak && !strings.Contains(refused, "is signed with"):
					t.Errorf("%s: the upload answers %q, want refused for its signature: %v", sc.what, refused, sc.weak)
				}
				if sc.weak {
					sigAsked[sc.what]++
					if code != 0 {
						sigRefused[sc.what]++
					}
				}
			}
			g.run(ctx, "rm -rf /root/sig /root/sig.pem /root/sig.cfg")

			// 2. The certificate a client gets, by the name it asks for. The chain verifies against the root alone, so
			// the intermediate was sent.
			dial := func(to string, conf *tls.Config) (*tls.Conn, error) {
				return tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", to, conf)
			}
			roots := func(cs ...lbcerttest.Cert) *x509.CertPool {
				pool := x509.NewCertPool()
				for _, c := range cs {
					pool.AppendCertsFromPEM([]byte(c.Root))
				}
				return pool
			}
			served := func(to, name string) *x509.Certificate {
				t.Helper()
				conn, err := dial(to, &tls.Config{ServerName: name, InsecureSkipVerify: true}) // the test reads the certificate itself
				if err != nil {
					t.Fatalf("handshake with %s as %q: %v", to, name, err)
				}
				defer conn.Close()
				return conn.ConnectionState().PeerCertificates[0]
			}
			for _, c := range []struct {
				to, name string
				want     lbcerttest.Cert
				verify   bool
			}{
				{https, "a.example.org", certA, true}, {https, "b.example.org", certB, true},
				{https, "other.example.org", certA, false}, {https, "", certA, false},
				{tlsTCP, "a.example.org", certA, true}, {tlsTCP, "b.example.org", certA, false},
			} {
				if got := served(c.to, c.name); !got.Equal(c.want.Leaf) {
					t.Errorf("%s asked for %q: got the certificate of %v", c.to, c.name, got.DNSNames)
				}
				if c.verify {
					conn, err := dial(c.to, &tls.Config{ServerName: c.name, RootCAs: roots(c.want)})
					if err != nil {
						t.Errorf("%s as %q does not verify against the root: %v", c.to, c.name, err)
						continue
					}
					conn.Close()
				}
			}

			// 3. The policy, by handshakes. TLS 1.3 and 1.2 are taken; 1.1 and 1.0 are not.
			for version, want := range map[uint16]bool{tls.VersionTLS13: true, tls.VersionTLS12: true, tls.VersionTLS11: false, tls.VersionTLS10: false} {
				for _, to := range []string{https, tlsTCP} {
					conn, err := dial(to, &tls.Config{ServerName: "a.example.org", RootCAs: roots(certA), MinVersion: version, MaxVersion: version})
					if (err == nil) != want {
						t.Errorf("%s with %s: %v, want accepted %v", to, tls.VersionName(version), err, want)
					}
					if err == nil {
						if conn.ConnectionState().Version != version {
							t.Errorf("%s: asked %s, got %s", to, tls.VersionName(version), tls.VersionName(conn.ConnectionState().Version))
						}
						conn.Close()
					}
				}
			}
			// TLS 1.2: each of the six suites of the policy with the certificate of its kind, and nothing else: no
			// RSA key exchange, no CBC.
			tls12 := func(name string, suite uint16) error {
				conn, err := dial(https, &tls.Config{ServerName: name, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{suite}})
				if err == nil {
					conn.Close()
				}
				return err
			}
			for suite, name := range map[uint16]string{
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256: "a.example.org", tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384: "a.example.org", tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256: "a.example.org",
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256: "b.example.org", tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384: "b.example.org", tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256: "b.example.org",
			} {
				if err := tls12(name, suite); err != nil {
					t.Errorf("TLS 1.2 with %s: %v", tls.CipherSuiteName(suite), err)
				}
			}
			for _, suite := range []uint16{tls.TLS_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_RSA_WITH_AES_256_GCM_SHA384, tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA, tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256, tls.TLS_RSA_WITH_AES_128_CBC_SHA} {
				if err := tls12("a.example.org", suite); err == nil {
					t.Errorf("TLS 1.2 with %s was accepted", tls.CipherSuiteName(suite))
				}
			}
			// DHE, which Go's TLS does not speak: openssl in the guest. The control is the same call with a suite of
			// the policy.
			sclient := func(cipher string) int {
				code, _, _ := g.sh(ctx, "sclient", "sh", "-c", "openssl s_client -connect 127.0.0.1:443 -servername a.example.org -tls1_2 -cipher "+cipher+" </dev/null >/dev/null 2>&1")
				return code
			}
			if code := sclient("ECDHE-RSA-AES256-GCM-SHA384"); code != 0 {
				t.Errorf("control: openssl with a suite of the policy: exit %d", code)
			}
			for _, cipher := range []string{"DHE-RSA-AES256-GCM-SHA384", "DHE-RSA-AES128-GCM-SHA256", "DHE-RSA-CHACHA20-POLY1305"} {
				if code := sclient(cipher + ":@SECLEVEL=0"); code == 0 {
					t.Errorf("a handshake with %s was accepted", cipher)
				}
			}
			// HTTP/2 is offered on https, nothing on tls.
			for to, want := range map[string]string{https: "h2", tlsTCP: ""} {
				conn, err := dial(to, &tls.Config{ServerName: "a.example.org", RootCAs: roots(certA), NextProtos: []string{"h2", "http/1.1"}})
				if err != nil {
					t.Fatalf("ALPN on %s: %v", to, err)
				}
				if got := conn.ConnectionState().NegotiatedProtocol; got != want {
					t.Errorf("ALPN on %s: %q, want %q", to, got, want)
				}
				conn.Close()
			}

			// 4. https: the member is spoken to in HTTP and reads who the client was and that it came over TLS, whatever
			// the client claims; over HTTP/2 and over HTTP/1.1. X-Forwarded-Proto is replaced. X-Forwarded-For is
			// appended (drill Q3-o1 of v0.11.1-rc.1): the client's own value stays first and proves nothing, the
			// last value is the address HAProxy saw, which is what the guides tell a server to read.
			for _, h2 := range []bool{true, false} {
				tr := &http.Transport{TLSClientConfig: &tls.Config{ServerName: "a.example.org", RootCAs: roots(certA)}, ForceAttemptHTTP2: h2, DisableKeepAlives: true}
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+https+"/path", nil)
				req.Host = "a.example.org"
				req.Header.Set("X-Forwarded-Proto", "http")
				req.Header.Set("X-Forwarded-For", "198.51.100.7")
				res, err := (&http.Client{Transport: tr, Timeout: 15 * time.Second}).Do(req)
				if err != nil {
					t.Fatalf("GET over https (HTTP/2 %v): %v", h2, err)
				}
				body, _ := io.ReadAll(res.Body)
				res.Body.Close()
				head := strings.ToLower(string(body))
				if res.StatusCode != 200 || (res.ProtoMajor == 2) != h2 || !strings.HasPrefix(head, "get /path http/1.1\n") ||
					strings.Count(head, "x-forwarded-proto:") != 1 || !strings.Contains(head, "x-forwarded-proto: https\n") || !strings.Contains(head, "x-forwarded-for: ") {
					t.Errorf("what the member got (HTTP/2 %v, answer %s %d):\n%s", h2, res.Proto, res.StatusCode, body)
				}
				var forwarded []string
				for _, line := range strings.Split(head, "\n") {
					if values, ok := strings.CutPrefix(line, "x-forwarded-for:"); ok {
						for _, v := range strings.Split(values, ",") {
							forwarded = append(forwarded, strings.TrimSpace(v))
						}
					}
				}
				if len(forwarded) != 2 || forwarded[0] != "198.51.100.7" || forwarded[1] == forwarded[0] || net.ParseIP(forwarded[1]) == nil {
					t.Errorf("X-Forwarded-For at the member (HTTP/2 %v): %q, want the client's own value and then the address HAProxy saw", h2, forwarded)
				}
			}

			// 5. A change of certificate: A2 in A's place, on both listeners. A connection open through the tls listener
			// lives on, and of the handshakes made meanwhile none fails.
			open, err := dial(tlsTCP, &tls.Config{ServerName: "a.example.org", RootCAs: roots(certA)})
			if err != nil {
				t.Fatal(err)
			}
			defer open.Close()
			echo := func(word string) error {
				_ = open.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := fmt.Fprintln(open, word); err != nil {
					return err
				}
				got, err := bufio.NewReader(open).ReadString('\n')
				if err != nil || strings.TrimSpace(got) != word {
					return fmt.Errorf("echo of %q: %q %v", word, got, err)
				}
				return nil
			}
			if err := echo("before"); err != nil {
				t.Fatalf("the open connection: %v", err)
			}
			stop, done := make(chan struct{}), make(chan struct{})
			var mu sync.Mutex
			made, failed := 0, []string{}
			go func() {
				defer close(done)
				for {
					select {
					case <-stop:
						return
					default:
					}
					conn, err := dial(https, &tls.Config{ServerName: "a.example.org", RootCAs: roots(certA, certA2)})
					mu.Lock()
					made++
					if err != nil {
						failed = append(failed, err.Error())
					} else {
						conn.Close()
					}
					mu.Unlock()
					time.Sleep(20 * time.Millisecond)
				}
			}()
			rotated := pve.RenderHAProxy(spec(fileA2))
			if lbSum(rotated) == lbSum(config) {
				t.Fatal("a change of certificate left the config's hash as it was: nothing would be pushed")
			}
			for i, to := range []struct {
				config string
				first  pve.LBCertFile
			}{{rotated, fileA2}, {config, fileA}, {rotated, fileA2}} { // there, back and there again: three reloads
				time.Sleep(300 * time.Millisecond)
				if _, err := ps.PushHAProxy(ctx, "n1", 100, to.config, to.first, fileB); err != nil {
					t.Fatalf("change of certificate %d: %v", i+1, err)
				}
				if got := served(https, "a.example.org"); !got.Equal(map[bool]lbcerttest.Cert{true: certA2, false: certA}[to.first.Name == fileA2.Name].Leaf) {
					t.Fatalf("change %d: a new connection got the certificate of before", i+1)
				}
				if got := certDir(); got != holds(to.first, fileB) {
					t.Errorf("change %d: the certificate directory:\n%s\nwant\n%s", i+1, got, holds(to.first, fileB))
				}
			}
			time.Sleep(300 * time.Millisecond)
			close(stop)
			<-done
			if made < 10 || len(failed) != 0 {
				t.Errorf("handshakes over three reloads: %d made, %d failed: %v", made, len(failed), failed)
			}
			t.Logf("%d handshakes over three reloads, none failed", made)
			if err := echo("after"); err != nil {
				t.Errorf("the connection open since before the changes: %v", err)
			}
			if got := served(tlsTCP, "a.example.org"); !got.Equal(certA2.Leaf) {
				t.Error("the tls listener still presents the certificate of before")
			}

			// 6. A config HAProxy refuses: it names a certificate file that is not there. The apply answers so, the
			// config of before is served and its certificate files are all still in place.
			missing := spec(pve.LBCertFile{Name: strings.Repeat("f", 64)})
			if _, err := ps.PushHAProxy(ctx, "n1", 100, pve.RenderHAProxy(missing), fileB); err == nil || !strings.Contains(err.Error(), "the last config stays in use") {
				t.Fatalf("a config naming a missing file: %v", err)
			}
			// And a file that is no certificate, under a name the config has: refused the same way.
			noise := pve.LBCertFile{Name: strings.Repeat("e", 64), PEM: "not a certificate\n"}
			if _, err := ps.PushHAProxy(ctx, "n1", 100, pve.RenderHAProxy(spec(noise)), noise, fileB); err == nil || !strings.Contains(err.Error(), "the last config stays in use") {
				t.Fatalf("a file that is no certificate: %v", err)
			}
			if got := served(https, "a.example.org"); !got.Equal(certA2.Leaf) {
				t.Error("after two refused configs HAProxy does not present the certificate it had")
			}
			if got := certDir(); got != holds(fileA2, fileB, noise) { // the refused file waits for the next served config
				t.Errorf("after two refused configs:\n%s", got)
			}
			if ok, why, err := ps.HAProxyServing(ctx, "n1", 100, lbSum(rotated)); err != nil || !ok {
				t.Errorf("after two refused configs, serving the one before: %v %q %v", ok, why, err)
			}

			// 7. A directory someone opened up is closed again by the next push; one that is a link is refused before
			// anything is written.
			g.run(ctx, "chmod 0755 /etc/haproxy/certs")
			if _, err := ps.PushHAProxy(ctx, "n1", 100, rotated, fileA2, fileB); err != nil {
				t.Fatal(err)
			}
			if got := certDir(); got != holds(fileA2, fileB) {
				t.Errorf("after a push onto an opened directory:\n%s", got)
			}
			g.run(ctx, "mv /etc/haproxy/certs /etc/haproxy/certs.real && mkdir -m 0777 /tmp/open && ln -s /tmp/open /etc/haproxy/certs")
			if _, err := ps.PushHAProxy(ctx, "n1", 100, config, fileA, fileB); err == nil || !strings.Contains(err.Error(), "nothing was written") {
				t.Errorf("a certificate directory that is a link: %v", err)
			}
			if got := g.run(ctx, "ls /tmp/open | wc -l"); got != "0" {
				t.Errorf("a key was written through a link: %s files", got)
			}
			g.run(ctx, "rm /etc/haproxy/certs && mv /etc/haproxy/certs.real /etc/haproxy/certs")
			// After three changes of certificate, two refused configs and a refused directory: the two certificates in
			// use, and no key anywhere else.
			if got := keyFiles(); got != inDir(fileA2, fileB) {
				t.Errorf("files of the guest that hold a key after the changes:\n%s\nwant\n%s", got, inDir(fileA2, fileB))
			}

			// 8. A certificate file damaged or gone while HAProxy serves it from memory (drill Q3-1 of v0.11.1-rc.1). The
			// question of that candidate, listeners and the config's hash, still reads "serving", while the config in
			// use would not start again: HAProxy refuses the whole file. The state read with the config's certificate
			// files says what is wrong, in words that name a file and hold nothing of it; the push that follows such
			// an answer puts the file right with the one reload of a push, and no handshake made meanwhile fails. A
			// file or a directory somebody opened up is found and closed the same way. Each of these pushes writes the
			// one file that was not in order and no other.
			certs := []pve.LBCertFile{fileA2, fileB}
			asked := func() (bool, string) {
				t.Helper()
				ok, why, err := ps.HAProxyServing(ctx, "n1", 100, lbSum(rotated), certs...)
				if err != nil {
					t.Fatal(err)
				}
				return ok, why
			}
			startable := func() bool {
				code, _, _ := g.sh(ctx, "startable", "haproxy", "-c", "-q", "-f", "/etc/haproxy/haproxy.cfg")
				return code == 0
			}
			reloads := func() int {
				t.Helper()
				n, err := strconv.Atoi(g.run(ctx, `echo "show proc" | socat -t5 - /run/haproxy-master.sock | awk '$2=="master"{print $3}'`))
				if err != nil {
					t.Fatalf("HAProxy's reload count: %v", err)
				}
				return n
			}
			onDisk := func(f pve.LBCertFile) bool {
				return g.run(ctx, "sha256sum < /etc/haproxy/certs/"+f.Name+".pem | cut -c1-64") == lbSum(f.PEM)
			}
			if ok, why := asked(); !ok || !onDisk(fileA2) || !onDisk(fileB) {
				t.Fatalf("before any damage: serving %v %q, the files are the certificates' %v %v", ok, why, onDisk(fileA2), onDisk(fileB))
			}
			// A pair's state read asks the same of its appliance, behind its own script (this guest has no keepalived:
			// the read says STOPPED, and everything else as on a pair).
			pairRead := func(what, want string) {
				t.Helper()
				st, err := ps.ReadVRRPState(ctx, "n1", 100, "", certs...)
				if err != nil || !st.HAProxy || !st.Serving || st.HAProxySum != lbSum(rotated) || st.CertFiles != want {
					t.Errorf("%s: a pair's state read: %+v %v; want it serving the config with certificate files %q", what, st, err, want)
				}
			}
			pairRead("before any damage", "")
			stop, done = make(chan struct{}), make(chan struct{})
			made, failed = 0, nil
			go func() {
				defer close(done)
				for {
					select {
					case <-stop:
						return
					default:
					}
					conn, err := dial(https, &tls.Config{ServerName: "a.example.org", RootCAs: roots(certA2)})
					mu.Lock()
					made++
					if err != nil {
						failed = append(failed, err.Error())
					} else {
						conn.Close()
					}
					mu.Unlock()
					time.Sleep(20 * time.Millisecond)
				}
			}()
			pathA2, pathB := "/etc/haproxy/certs/"+fileA2.Name+".pem", "/etc/haproxy/certs/"+fileB.Name+".pem"
			// A push with nothing out of order writes the config and no certificate file (drill Q3-o2: every push
			// wrote every file again, 0.6 s each on the lab).
			_, mark := g.writtenSince(0)
			if _, err := ps.PushHAProxy(ctx, "n1", 100, rotated, certs...); err != nil {
				t.Fatal(err)
			}
			if got, _ := g.writtenSince(mark); !slices.Equal(got, []string{"/etc/haproxy/haproxy.cfg.new"}) {
				t.Errorf("a push whose certificate files are all in place wrote %v", got)
			}
			for _, c := range []struct {
				what, damage, why string
				startable         bool
				written           []string // the one file that was not in order, then the config
			}{
				{"a truncated file", "truncate -s 0 " + pathA2, "Certificate file " + fileA2.Name[:12] + " is not the file that was written", false, []string{pathA2 + ".new", "/etc/haproxy/haproxy.cfg.new"}},
				{"a deleted file", "rm " + pathB, "Certificate file " + fileB.Name[:12] + " is missing", false, []string{pathB + ".new", "/etc/haproxy/haproxy.cfg.new"}},
				{"a file anybody reads", "chmod 0644 " + pathA2, "Certificate file " + fileA2.Name[:12] + " is not root's alone", true, []string{pathA2 + ".new", "/etc/haproxy/haproxy.cfg.new"}},
				{"a directory anybody enters", "chmod 0755 /etc/haproxy/certs", "The certificate directory is missing or is not root's alone", true, []string{"/etc/haproxy/haproxy.cfg.new"}},
			} {
				g.run(ctx, c.damage)
				if ok, why, err := ps.HAProxyServing(ctx, "n1", 100, lbSum(rotated)); err != nil || !ok {
					t.Fatalf("%s: HAProxy does not serve from memory, so this is not the drill's case: %v %q %v", c.what, ok, why, err)
				}
				if got := startable(); got != c.startable {
					t.Errorf("%s: haproxy -c on the config in use answers %v, want %v", c.what, got, c.startable)
				}
				if ok, why := asked(); ok || why != c.why {
					t.Fatalf("%s: the state read with the certificate files: serving %v, %q; want %q", c.what, ok, why, c.why)
				}
				pairRead(c.what, c.why)
				before := reloads()
				_, mark := g.writtenSince(0)
				if _, err := ps.PushHAProxy(ctx, "n1", 100, rotated, certs...); err != nil {
					t.Fatalf("%s: the push that follows: %v", c.what, err)
				}
				if got := reloads(); got != before+1 {
					t.Errorf("%s: %d reloads for one push", c.what, got-before)
				}
				if got, _ := g.writtenSince(mark); !slices.Equal(got, c.written) {
					t.Errorf("%s: the push wrote %v, want %v", c.what, got, c.written)
				}
				if got := certDir(); got != holds(fileA2, fileB) || !onDisk(fileA2) || !onDisk(fileB) {
					t.Errorf("%s: the certificate directory after the push (the files are the certificates' %v %v):\n%s", c.what, onDisk(fileA2), onDisk(fileB), got)
				}
				if ok, why := asked(); !ok || !startable() {
					t.Errorf("%s: after the push: serving %v %q, the config in use would start %v", c.what, ok, why, startable())
				}
				pairRead(c.what+", after the push", "")
				if got := served(https, "a.example.org"); !got.Equal(certA2.Leaf) {
					t.Errorf("%s: a new connection does not get the certificate", c.what)
				}
			}
			time.Sleep(300 * time.Millisecond)
			close(stop)
			<-done
			if made < 10 || len(failed) != 0 {
				t.Errorf("handshakes while four damaged files were put right: %d made, %d failed: %v", made, len(failed), failed)
			}
			t.Logf("%d handshakes while four damaged files were put right, none failed", made)
			// And HAProxy's next start, which is where the damage would have shown: it finds a config it takes.
			g.run(ctx, "systemctl restart haproxy")
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
				ok, why := asked()
				if ok {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("HAProxy after a restart: %q", why)
				}
			}
			if a, b := served(https, "a.example.org"), served(https, "b.example.org"); !a.Equal(certA2.Leaf) || !b.Equal(certB.Leaf) {
				t.Error("after a restart HAProxy does not present the two certificates")
			}
			if got := keyFiles(); got != inDir(fileA2, fileB) {
				t.Errorf("files of the guest that hold a key after the damaged files were put right:\n%s\nwant\n%s", got, inDir(fileA2, fileB))
			}

			// 9. The last listener that ends TLS goes: so do the certificate files, after the config without them is
			// served.
			plain := pve.LBSpec{Listeners: []pve.LBListener{{Port: 8443, Protocol: "tcp", Pool: "echo"}}, Pools: spec(fileA).Pools[1:]}
			if _, err := ps.PushHAProxy(ctx, "n1", 100, pve.RenderHAProxy(plain)); err != nil {
				t.Fatal(err)
			}
			if got := certDir(); got != holds() {
				t.Errorf("after the last TLS listener went:\n%s", got)
			}
			if got := keyFiles(); got != "" {
				t.Errorf("files of the guest that still hold a key after the last TLS listener went:\n%s", got)
			}
			// Over every push of this test, rotations and repairs included, the watcher saw no file of a push with
			// a mode wider than 0600 but the control's.
			if got := g.run(ctx, "systemctl stop cert-watch; sort -u /root/cert-modes"); got != "666 /etc/haproxy/certs/control.pem.new" {
				t.Errorf("a certificate file had a mode wider than 0600 while it was written:\n%s", got)
			}
		})
	}
}
