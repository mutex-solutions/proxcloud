package pve

import (
	"context"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve/lbcerttest"
)

func tlsGoldenSpec() LBSpec {
	a, b := strings.Repeat("a1", 32), strings.Repeat("b2", 32)
	return LBSpec{
		Listeners: []LBListener{
			{Port: 443, Protocol: "https", Pool: "web", Certificates: []string{"c1", "c2"}, CertFiles: []string{a, b}},
			{Port: 6443, Protocol: "tls", Pool: "api", Certificates: []string{"c2"}, CertFiles: []string{b}, ConnectionLimit: 500},
			{Port: 80, Protocol: "http", Pool: "web"},
		},
		Pools: []LBPool{
			{Name: "web", Protocol: "http", Algorithm: "roundrobin", Members: []LBMember{{ServerID: "101", Port: 8080, Weight: 1, Address: "10.0.0.11"}}},
			{Name: "api", Protocol: "tcp", Algorithm: "leastconn", ProxyProtocol: true, Members: []LBMember{{ServerID: "102", Port: 6443, Weight: 1, Address: "10.0.0.12"}}},
		},
	}
}

// The TLS policy is in "global" only when a listener ends TLS; an https listener speaks HTTP to its members with the
// two forwarded headers and offers HTTP/2, a tls listener speaks TCP; each names its certificate files in order, the
// first being the default. The goldens are checked by the HAProxy of both appliance images with real files
// (TestLBApplianceTLS).
func TestRenderHAProxyTLS(t *testing.T) {
	s := tlsGoldenSpec()
	golden(t, "haproxy-tls.cfg", RenderHAProxy(s))
	golden(t, "haproxy-tls-pair.cfg", RenderHAProxyOn(s, []string{"203.0.113.10"}))
	cfg := RenderHAProxy(s)
	for _, want := range []string{
		"  ssl-default-bind-options ssl-min-ver TLSv1.2\n",
		"  ssl-default-bind-ciphersuites TLS_AES_128_GCM_SHA256:TLS_AES_256_GCM_SHA384:TLS_CHACHA20_POLY1305_SHA256\n",
		"\nfrontend l443\n  bind :443 ssl crt /etc/haproxy/certs/" + s.Listeners[0].CertFiles[0] + ".pem crt /etc/haproxy/certs/" + s.Listeners[0].CertFiles[1] + ".pem alpn h2,http/1.1\n  mode http\n  default_backend p-web\n  option forwardfor\n  http-request set-header X-Forwarded-Proto https\n",
		"\nfrontend l6443\n  bind :6443 ssl crt /etc/haproxy/certs/" + s.Listeners[1].CertFiles[0] + ".pem\n  mode tcp\n  default_backend p-api\n  maxconn 500\n",
		"\nfrontend l80\n  bind :80\n  mode http\n  default_backend p-web\n  option forwardfor\n\n",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the config lacks %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, ":DHE-") || strings.Contains(cfg, " DHE-") || strings.Contains(cfg, "strict-sni") || strings.Count(cfg, "ECDHE-") != 6 {
		t.Errorf("cipher line:\n%s", cfg)
	}
	// No listener that ends TLS: not a byte of the policy (the goldens of before hold that for whole configs).
	plain := tlsGoldenSpec()
	plain.Listeners = plain.Listeners[2:]
	if got := RenderHAProxy(plain); strings.Contains(got, "ssl") {
		t.Errorf("a config without TLS names it:\n%s", got)
	}
	// What only the platform may set does not survive a tenant's request, and a file name that is not a digest never
	// reaches the config.
	asked := tlsGoldenSpec()
	asked.Listeners[0].CertFiles = []string{"../../etc/shadow"}
	if got := RenderHAProxy(asked); strings.Contains(got, "shadow") || strings.Contains(got, "frontend l443") {
		t.Errorf("a file name off the pattern was rendered:\n%s", got)
	}
	if msg := asked.Validate(); msg != "" || asked.Listeners[0].CertFiles != nil || asked.Listeners[1].CertFiles != nil {
		t.Errorf("validate: %q, files %q", msg, asked.Listeners[0].CertFiles)
	}
	for name, breakIt := range map[string]func(*LBSpec){
		"no certificate":          func(s *LBSpec) { s.Listeners[0].Certificates = nil },
		"eleven certificates":     func(s *LBSpec) { s.Listeners[0].Certificates = strings.Split("a,b,c,d,e,f,g,h,i,j,k", ",") },
		"a certificate twice":     func(s *LBSpec) { s.Listeners[0].Certificates = []string{"c1", "c2", "c1"} },
		"an id that is a path":    func(s *LBSpec) { s.Listeners[0].Certificates = []string{"../c1"} },
		"https in front of tcp":   func(s *LBSpec) { s.Listeners[0].Pool = "api" },
		"a certificate on http":   func(s *LBSpec) { s.Listeners[2].Certificates = []string{"c1"} },
		"tls and https on a port": func(s *LBSpec) { s.Listeners[1].Port = 443 },
	} {
		bad := tlsGoldenSpec()
		breakIt(&bad)
		if bad.Validate() == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	if !asked.HasTLS() || plain.HasTLS() {
		t.Error("HasTLS")
	}
}

// What an upload is checked for, on real certificates: the key belongs to the first certificate, the chain is in
// order, the key is RSA or ECDSA of an accepted size and has no passphrase, the chain is short and the whole is
// small. An expired certificate is taken with a warning. No answer holds anything of the key.
func TestCheckLBCertificate(t *testing.T) {
	now, soon := time.Now(), time.Now().Add(72*time.Hour)
	rsaKey := lbcerttest.RSA(t, 2048)
	good := lbcerttest.New(t, rsaKey, soon, "www.example.org", "example.org", "203.0.113.7")
	info, bundle, warnings, refused := CheckLBCertificate(good.Chain, good.Key, now)
	if refused != "" || len(warnings) != 0 {
		t.Fatalf("a good certificate: %q %q", refused, warnings)
	}
	if info.KeyType != "RSA 2048" || info.ChainLength != 2 || !slices.Equal(info.DNSNames, []string{"www.example.org", "example.org", "203.0.113.7"}) ||
		info.Subject != "CN=www.example.org" || info.Issuer != "CN=test intermediate" || !lbCertFileRe.MatchString(info.File) || !lbCertFileRe.MatchString(info.Fingerprint) ||
		info.File == info.Fingerprint || !info.NotAfter.Equal(good.Leaf.NotAfter) {
		t.Errorf("what the certificate says: %+v", info)
	}
	if strings.Count(bundle, "BEGIN CERTIFICATE") != 2 || strings.Count(bundle, "BEGIN PRIVATE KEY") != 1 || !strings.HasSuffix(strings.TrimSpace(bundle), "END PRIVATE KEY-----") {
		t.Errorf("the file of an appliance: %d bytes, not chain then key", len(bundle))
	}
	// The same upload gives the same file, in whatever form the key came.
	pkcs1 := lbcerttest.PEM("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey))
	if again, same, _, refused := CheckLBCertificate("\n"+good.Chain+"\n", pkcs1, now); refused != "" || same != bundle || again.File != info.File {
		t.Errorf("the same certificate with a PKCS #1 key: %q, same file %v", refused, same == bundle)
	}
	// The chain without its intermediate is another file under the same fingerprint.
	leafOnly := good.Chain[:strings.Index(good.Chain, "-----END CERTIFICATE-----")+len("-----END CERTIFICATE-----\n")]
	if alone, _, _, refused := CheckLBCertificate(leafOnly, good.Key, now); refused != "" || alone.Fingerprint != info.Fingerprint || alone.File == info.File || alone.File != alone.Fingerprint {
		t.Errorf("the server's certificate alone: %q %+v", refused, alone)
	}
	for _, curve := range []elliptic.Curve{elliptic.P256(), elliptic.P384()} {
		k := lbcerttest.EC(t, curve)
		c := lbcerttest.New(t, k, soon, "ec.example.org")
		sec1, _ := x509.MarshalECPrivateKey(k)
		for _, key := range []string{c.Key, lbcerttest.PEM("EC PRIVATE KEY", sec1)} {
			if info, _, _, refused := CheckLBCertificate(c.Chain, key, now); refused != "" || info.KeyType != "ECDSA "+curve.Params().Name {
				t.Errorf("%s: %q %q", curve.Params().Name, refused, info.KeyType)
			}
		}
	}
	old := lbcerttest.New(t, rsaKey, now.Add(-time.Hour), "old.example.org")
	if _, _, warnings, refused := CheckLBCertificate(old.Chain, old.Key, now); refused != "" || len(warnings) != 1 || !strings.Contains(warnings[0], "expired on") {
		t.Errorf("an expired certificate: %q %q", refused, warnings)
	}
	bare := lbcerttest.New(t, rsaKey, soon)
	if _, _, warnings, refused := CheckLBCertificate(bare.Chain, bare.Key, now); refused != "" || len(warnings) != 1 || !strings.Contains(warnings[0], "names no host") {
		t.Errorf("a certificate without names: %q %q", refused, warnings)
	}

	other := lbcerttest.New(t, lbcerttest.RSA(t, 2048), soon, "other.example.org")
	weak := lbcerttest.New(t, lbcerttest.RSA(t, 1024), soon, "weak.example.org")
	p521 := lbcerttest.New(t, lbcerttest.EC(t, elliptic.P521()), soon, "p521.example.org")
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	ed := lbcerttest.New(t, edKey, soon, "ed.example.org")
	mid := good.Chain[strings.Index(good.Chain, "-----END CERTIFICATE-----")+len("-----END CERTIFICATE-----\n"):]
	//nolint:staticcheck // the legacy passphrase format is what an old tool still writes
	legacy, err := x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey), []byte("passphrase"), x509.PEMCipherAES256)
	if err != nil {
		t.Fatal(err)
	}
	keyBody := strings.Split(good.Key, "\n")[1] // a line of the key itself: no answer may hold it
	for name, c := range map[string]struct{ chain, key, want string }{
		"another certificate's key":   {good.Chain, other.Key, "does not belong to the certificate"},
		"the intermediate first":      {mid + leafOnly, good.Key, "does not belong to the certificate"},
		"an unrelated second":         {leafOnly + other.Chain, good.Key, "did not sign certificate 1"},
		"RSA 1024":                    {weak.Chain, weak.Key, "RSA of 2048 to 8192 bits"},
		"ECDSA P-521":                 {p521.Chain, p521.Key, "P-256 or P-384"},
		"Ed25519":                     {ed.Chain, ed.Key, "The key must be RSA"},
		"six certificates":            {leafOnly + strings.Repeat(mid, 5), good.Key, "more than 5 certificates"},
		"larger than 16 KiB":          {good.Chain + strings.Repeat("\n", LBMaxCertBundle), good.Key, "larger than 16 KiB"},
		"a key with a passphrase":     {good.Chain, string(pem.EncodeToMemory(legacy)), "encrypted with a passphrase"},
		"a PKCS #8 key, passphrase":   {good.Chain, lbcerttest.PEM("ENCRYPTED PRIVATE KEY", []byte("x")), "encrypted with a passphrase"},
		"the key among the chain":     {good.Chain + good.Key, good.Key, "certificates only"},
		"two keys":                    {good.Chain, good.Key + other.Key, "one key in it"},
		"no certificate":              {"", good.Key, "no BEGIN CERTIFICATE line"},
		"text after the certificate":  {good.Chain + "hello", good.Key, "outside its BEGIN and END lines"},
		"a certificate that is noise": {lbcerttest.PEM("CERTIFICATE", []byte("noise")), good.Key, "Certificate 1 of the chain cannot be read"},
		"a key that is noise":         {good.Chain, lbcerttest.PEM("PRIVATE KEY", []byte("noise")), "The private key cannot be read"},
		"a certificate as the key":    {good.Chain, leafOnly, "one key in it"},
	} {
		_, bundle, _, refused := CheckLBCertificate(c.chain, c.key, now)
		if !strings.Contains(refused, c.want) || bundle != "" {
			t.Errorf("%s: %q, want %q", name, refused, c.want)
		}
		if strings.Contains(refused, keyBody) || strings.Contains(refused, "PRIVATE KEY-----") {
			t.Errorf("%s: the answer holds the key", name)
		}
	}
}

// A signature an appliance's HAProxy refuses is refused at the upload, with the certificate's place in the chain and
// the algorithm: SHA-1 (and MD5, MD2) on a certificate that did not sign itself. A certificate that signed itself is
// taken whatever its digest, the old SHA-1 root at the end of a chain among them: OpenSSL does not look at that
// signature. What HAProxy itself says of these certificates, and of an MD5 and a DSA one: TestLBApplianceTLS.
func TestCheckLBCertificateSignature(t *testing.T) {
	now, soon := time.Now(), time.Now().Add(72*time.Hour)
	key := lbcerttest.RSA(t, 2048)
	first := func(chain string) string {
		return chain[:strings.Index(chain, "-----END CERTIFICATE-----")+len("-----END CERTIFICATE-----\n")]
	}
	sha1Leaf := lbcerttest.NewSigned(t, key, soon, lbcerttest.Sign{Leaf: x509.ECDSAWithSHA1}, "sha1.example.org")
	sha1RSA := lbcerttest.NewSigned(t, key, soon, lbcerttest.Sign{Leaf: x509.SHA1WithRSA, RSA: true}, "sha1.example.org")
	sha1Mid := lbcerttest.NewSigned(t, key, soon, lbcerttest.Sign{Intermediate: x509.ECDSAWithSHA1}, "sha1.example.org")
	sha1Root := lbcerttest.NewSigned(t, key, soon, lbcerttest.Sign{Root: x509.ECDSAWithSHA1}, "sha1.example.org")
	sha256RSA := lbcerttest.NewSigned(t, key, soon, lbcerttest.Sign{RSA: true}, "sha256.example.org")
	sha256 := lbcerttest.New(t, key, soon, "sha256.example.org")
	self := lbcerttest.SelfSigned(t, key, soon, x509.SHA1WithRSA, "self.example.org")
	for _, c := range []struct{ name, chain, want string }{
		{"a SHA-1 server certificate alone", first(sha1Leaf.Chain), "Certificate 1 of the chain is signed with ECDSA-SHA1"},
		{"a SHA-1 server certificate with its intermediate", sha1Leaf.Chain, "Certificate 1 of the chain is signed with ECDSA-SHA1"},
		{"a SHA-1 server certificate of an RSA authority", sha1RSA.Chain, "Certificate 1 of the chain is signed with SHA1-RSA"},
		{"a SHA-1 intermediate", sha1Mid.Chain, "Certificate 2 of the chain is signed with ECDSA-SHA1"},
		{"a SHA-1 certificate that signed itself", self.Chain, ""},
		{"a chain that ends in its SHA-1 root", sha1Root.Chain + sha1Root.Root, ""},
		{"SHA-256 (the control)", sha256.Chain, ""},
		{"SHA-256 of an RSA authority (the control)", sha256RSA.Chain, ""},
	} {
		info, bundle, _, refused := CheckLBCertificate(c.chain, sha256.Key, now)
		switch {
		case c.want == "" && (refused != "" || bundle == "" || info.ChainLength != strings.Count(c.chain, "BEGIN CERTIFICATE")):
			t.Errorf("%s: refused with %q, want it taken whole", c.name, refused)
		case c.want != "" && (!strings.Contains(refused, c.want) || !strings.Contains(refused, "SHA-256") || bundle != ""):
			t.Errorf("%s: %q, want %q", c.name, refused, c.want)
		}
	}
}

// The signatures Go has no name for (x509.UnknownSignatureAlgorithm), on certificates openssl made (testdata/sig-*.pem,
// certificates only): RSA-PSS with SHA-1 is named, as the SHA-1 signature it is; SHA-224, and RSA-PSS with SHA-256
// and a salt of 20 bytes, are not. What the HAProxy of each appliance image says of the three: TestLBApplianceTLS.
func TestLBWeakSignatureUnnamed(t *testing.T) {
	for file, want := range map[string]string{"sig-rsapss-sha1.pem": "SHA1-RSAPSS", "sig-sha224.pem": "", "sig-rsapss-sha256-salt20.pem": ""} {
		text, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(text)
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if c.SignatureAlgorithm != x509.UnknownSignatureAlgorithm {
			t.Fatalf("%s: Go names its signature %v, so it shows nothing here", file, c.SignatureAlgorithm)
		}
		if got := lbWeakSignature(c); got != want {
			t.Errorf("%s: %q, want %q", file, got, want)
		}
	}
}

// certGuest is the guest agent of an appliance for the delivery's order: it logs each command by what it is and each
// file written, and answers a command with the exit code set for it.
type certGuest struct {
	mu    sync.Mutex
	log   []string
	exits map[string]int
	runs  map[int]int
	files map[string]string
	// cmds are the commands as they were sent, and out is what the next ones print.
	cmds []string
	out  string
	outs map[int]string
	// modes holds what stat says of a certificate file that is not root's alone; the install command clears it.
	modes map[string]string
}

// certState is what certstate prints of the guest's certificate directory: the files the install command moved in
// place, as they are now.
func (g *certGuest) certState() string {
	out, sums := "crtdir 700:root:root:directory\n", ""
	for _, path := range slices.Sorted(maps.Keys(g.files)) {
		if !strings.HasPrefix(path, lbCertDir+"/") || !strings.HasSuffix(path, ".pem") {
			continue
		}
		mode := "600:root:root"
		if m := g.modes[path]; m != "" {
			mode = m
		}
		sum := sha256.Sum256([]byte(g.files[path]))
		out += "crt " + path + " " + mode + "\n"
		sums += hex.EncodeToString(sum[:]) + "  " + path + "\n"
	}
	return out + sums
}

func (g *certGuest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	_ = r.ParseForm()
	write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	switch r.Method + " " + r.URL.Path[strings.LastIndex(r.URL.Path, "/agent/")+7:] {
	case "POST exec":
		cmd, what := strings.Join(r.Form["command"], " "), "other"
		switch {
		case strings.Contains(cmd, "is not a directory of root alone"):
			what = "certificate directory"
		case strings.Contains(cmd, "a certificate file could not be made root's alone"):
			what = "certificate prepare " + strings.Join(r.Form["command"][4:], ",")
		case strings.Contains(cmd, "a certificate file is not root's alone"):
			what = "certificate install " + strings.Join(r.Form["command"][4:], ",")
		case strings.Contains(cmd, "command -v ss"):
			what = "ready"
		case strings.Contains(cmd, "haproxy -c"):
			what = "reload"
		}
		g.log = append(g.log, what)
		g.cmds = append(g.cmds, cmd)
		pid := len(g.runs) + 1
		g.runs[pid] = g.exits[strings.SplitN(what, " ", 3)[0]+" "+strings.SplitN(what+" ", " ", 3)[1]]
		if g.outs == nil {
			g.outs = map[int]string{}
		}
		g.outs[pid] = g.out
		switch {
		case what == "certificate directory" && g.runs[pid] == 0:
			g.outs[pid] = g.certState()
		case strings.HasPrefix(what, "certificate install") && g.runs[pid] == 0:
			for _, name := range r.Form["command"][4:] {
				path := lbCertDir + "/" + name + ".pem"
				g.files[path] = g.files[path+".new"]
				delete(g.files, path+".new")
				delete(g.modes, path)
			}
		}
		write(map[string]any{"pid": pid})
	case "GET exec-status":
		pid, _ := strconv.Atoi(r.Form.Get("pid"))
		write(map[string]any{"exited": 1, "exitcode": g.runs[pid], "out-data": g.outs[pid], "err-data": "refused by the guest"})
	case "POST file-write":
		g.log = append(g.log, "write "+r.Form.Get("file"))
		g.files[r.Form.Get("file")] = r.Form.Get("content")
		write(nil)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// The order of a push with certificates (the guest agent creates every file with mode 0666): the directory is made
// private and proven so before anything is written; every file is written inside it, under its temporary name too;
// mode, owner and the move in place come next; the config that names the files comes last, and its apply removes
// the files it no longer names only after HAProxy was seen to serve it.
func TestCertificatesReachAnApplianceBeforeItsConfig(t *testing.T) {
	ctx := context.Background()
	guest := func() (*certGuest, *Service) {
		g := &certGuest{exits: map[string]int{}, runs: map[int]int{}, files: map[string]string{}}
		srv := httptest.NewServer(g)
		t.Cleanup(srv.Close)
		ps, err := NewService(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
		if err != nil {
			t.Fatal(err)
		}
		return g, ps
	}
	a, b := strings.Repeat("a1", 32), strings.Repeat("b2", 32)
	certs := []LBCertFile{{Name: a, PEM: "PEM-A"}, {Name: b, PEM: "PEM-B"}}

	g, ps := guest()
	if _, err := ps.PushPairHAProxy(ctx, "n1", 100, "config", certs...); err != nil {
		t.Fatal(err)
	}
	want := []string{"ready", "certificate directory", "certificate prepare " + a + "," + b, "write /etc/haproxy/certs/" + a + ".pem.new",
		"write /etc/haproxy/certs/" + b + ".pem.new", "certificate install " + a + "," + b, "write /etc/haproxy/haproxy.cfg.new", "reload"}
	if !slices.Equal(g.log, want) {
		t.Errorf("order of a push:\n%s\nwant\n%s", strings.Join(g.log, "\n"), strings.Join(want, "\n"))
	}
	if g.files["/etc/haproxy/certs/"+a+".pem"] != "PEM-A" || len(g.files) != 3 {
		t.Errorf("files written: %v", g.log)
	}
	// A single appliance: the same order.
	g, ps = guest()
	if _, err := ps.PushHAProxy(ctx, "n1", 100, "config", certs[:1]...); err != nil || !slices.Equal(g.log, []string{"ready", "certificate directory", "certificate prepare " + a,
		"write /etc/haproxy/certs/" + a + ".pem.new", "certificate install " + a, "write /etc/haproxy/haproxy.cfg.new", "reload"}) {
		t.Errorf("single appliance: %v\n%s", err, strings.Join(g.log, "\n"))
	}
	// A directory that cannot be made private: nothing is written, not the config either.
	g, ps = guest()
	g.exits["certificate directory"] = 1
	if _, err := ps.PushPairHAProxy(ctx, "n1", 100, "config", certs...); err == nil || !strings.Contains(err.Error(), "nothing was written") || len(g.files) != 0 {
		t.Errorf("a directory that is not private: %v, wrote %v", err, g.log)
	}
	// Files that could not be given to root alone: the config that names them is not written.
	g, ps = guest()
	g.exits["certificate install"] = 1
	if _, err := ps.PushPairHAProxy(ctx, "n1", 100, "config", certs...); err == nil || slices.Contains(g.log, "write /etc/haproxy/haproxy.cfg.new") || slices.Contains(g.log, "reload") {
		t.Errorf("files not put in place: %v, log %v", err, g.log)
	}
	// A name that is not a digest reaches no command and no path.
	g, ps = guest()
	if _, err := ps.PushPairHAProxy(ctx, "n1", 100, "config", LBCertFile{Name: "x; rm -rf /", PEM: "PEM"}); err == nil || !slices.Equal(g.log, []string{"ready"}) {
		t.Errorf("a bad name: %v, log %v", err, g.log)
	}
	// No certificate: the push of before, to the byte of its calls.
	g, ps = guest()
	if _, err := ps.PushPairHAProxy(ctx, "n1", 100, "config"); err != nil || !slices.Equal(g.log, []string{"ready", "write /etc/haproxy/haproxy.cfg.new", "reload"}) {
		t.Errorf("no certificate: %v, log %v", err, g.log)
	}

	// In the scripts themselves: the directory's proof precedes its use, a file's mode is proven before the move,
	// and the apply prunes after the swap that may still put the old config back.
	if i, j := strings.Index(lbCertDirScript, "chmod 0700"), strings.Index(lbCertDirScript, `"700:root:root:directory"`); i < 0 || j < i || !strings.Contains(lbCertDirScript, "[ ! -L $D ]") {
		t.Errorf("directory script:\n%s", lbCertDirScript)
	}
	if i, j := strings.Index(lbCertInstallScript, "600:root:root"), strings.Index(lbCertInstallScript, "mv -f"); i < 0 || j < i {
		t.Errorf("install script:\n%s", lbCertInstallScript)
	}
	if i, j, k := strings.Index(haproxyReload, "swap || {"), strings.Index(haproxyReload, lbCertPrune), strings.Index(haproxyReload, "haproxy -c -q"); i < 0 || j < i || k > i {
		t.Errorf("the apply prunes certificate files before its swap is proven:\n%s", haproxyReload)
	}
	if s := (LBCertFile{Name: a, PEM: "PEM-A"}); strings.Contains(s.String()+s.LogValue().String(), "PEM-A") {
		t.Error("a certificate file prints its content")
	}
}

// A push writes the certificate files the appliance does not hold as the portal would write them, and no other
// (drill Q3-o2 of v0.11.1-rc.1: every push wrote every file again, about 0.6 s each through the guest agent, inside
// the tenant's request: 13.5 s with eleven). The directory's command, which a push sends anyway, answers what is
// there; the order stays certificates before config, and the config's check, swap and roll back are as they were.
func TestPushWritesOnlyTheCertificateFilesThatDiffer(t *testing.T) {
	ctx := context.Background()
	g := &certGuest{exits: map[string]int{}, runs: map[int]int{}, files: map[string]string{}, modes: map[string]string{}}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	ps, err := NewService(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	var certs []LBCertFile
	for i := range 11 { // the drill's balancer: eleven certificates
		certs = append(certs, LBCertFile{Name: strings.Repeat(fmt.Sprintf("%02x", 0xa0+i), 32), PEM: fmt.Sprintf("PEM-%d", i)})
	}
	path := func(c LBCertFile) string { return "/etc/haproxy/certs/" + c.Name + ".pem" }
	push := func(what string, certs []LBCertFile, written ...LBCertFile) {
		t.Helper()
		g.mu.Lock()
		g.log = nil
		g.mu.Unlock()
		if _, err := ps.PushHAProxy(ctx, "n1", 100, "config", certs...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		want := []string{"ready", "certificate directory"}
		var names []string
		for _, c := range written {
			names = append(names, c.Name)
		}
		if len(written) > 0 { // each file is made, empty and with mode 0600, before the agent writes into it
			want = append(want, "certificate prepare "+strings.Join(names, ","))
		}
		for _, c := range written {
			want = append(want, "write "+path(c)+".new")
		}
		if len(written) > 0 {
			want = append(want, "certificate install "+strings.Join(names, ","))
		}
		want = append(want, "write /etc/haproxy/haproxy.cfg.new", "reload")
		g.mu.Lock()
		defer g.mu.Unlock()
		if !slices.Equal(g.log, want) {
			t.Errorf("%s: the guest got\n%s\nwant\n%s", what, strings.Join(g.log, "\n"), strings.Join(want, "\n"))
		}
		for _, c := range certs {
			if g.files[path(c)] != c.PEM || g.modes[path(c)] != "" {
				t.Errorf("%s: file %s after the push is not the certificate's, or not root's alone", what, c.Name[:12])
			}
		}
	}
	damage := func(change func()) {
		g.mu.Lock()
		defer g.mu.Unlock()
		change()
	}
	push("the first push: every file", certs[:10], certs[:10]...)
	// The same certificates again, as with any change of a member or a limit: 4 calls where there were 15.
	push("nothing differs: no file", certs[:10])
	push("an eleventh certificate: that file alone", certs, certs[10])
	damage(func() { g.files[path(certs[3])] = "" })
	push("a truncated file: that file alone", certs, certs[3])
	damage(func() { delete(g.files, path(certs[0])); g.files[path(certs[7])] = certs[8].PEM })
	push("a file gone and one with another certificate's bytes", certs, certs[0], certs[7])
	damage(func() { g.modes[path(certs[5])] = "644:root:root" })
	push("a file anybody reads is written again, not only closed", certs, certs[5])
	push("and nothing after it", certs)
	// A file the config no longer names is not looked at: the apply prunes it once the new config is served.
	push("fewer certificates", certs[:2])
	// A directory that cannot be made private still stops the push before any file, and before the config.
	damage(func() { g.exits["certificate directory"] = 1; delete(g.files, path(certs[0])) })
	if _, err := ps.PushHAProxy(ctx, "n1", 100, "config", certs[:2]...); err == nil || !strings.Contains(err.Error(), "nothing was written") || g.files[path(certs[0])] != "" {
		t.Errorf("a directory that is not private: %v", err)
	}
}

// The applied-state read proves the certificate files a config names (drill Q3-1 of v0.11.1-rc.1: HAProxy served from
// memory with a truncated file, the row read ACTIVE, and at its next start HAProxy would have refused the whole
// config, the plain listeners with it). The read says of each named file whether it is there, whether its bytes are
// the ones the portal would write, and whether it is root's alone in a directory of root alone. What it answers
// holds the start of a file's name and nothing of its content, not its digest either. It is the same one command:
// without a certificate the command is the one of before, to the byte.
func TestStateReadProvesTheCertificateFiles(t *testing.T) {
	ctx := context.Background()
	a, b := strings.Repeat("a1", 32), strings.Repeat("b2", 32)
	certs := []LBCertFile{{Name: a, PEM: "PEM-A"}, {Name: b, PEM: "PEM-B"}}
	digest := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		return hex.EncodeToString(sum[:])
	}
	path := func(name string) string { return "/etc/haproxy/certs/" + name + ".pem" }
	// state is what certstate prints for a directory and its files, each given as name, mode and content.
	state := func(dir string, files ...[3]string) string {
		out := ""
		if dir != "" {
			out = "crtdir " + dir + "\n"
		}
		for _, f := range files {
			out += "crt " + path(f[0]) + " " + f[1] + "\n"
		}
		for _, f := range files {
			out += digest(f[2]) + "  " + path(f[0]) + "\n"
		}
		return out
	}
	const dirOK, fileOK = "700:root:root:directory", "600:root:root"
	good := state(dirOK, [3]string{a, fileOK, "PEM-A"}, [3]string{b, fileOK, "PEM-B"})
	for name, c := range map[string]struct {
		out   string
		stale []string
		why   string
	}{
		"as a push leaves it":           {good, nil, ""},
		"with a file no config names":   {good + "crt " + path(strings.Repeat("c3", 32)) + " 644:root:root\n", nil, ""},
		"a file gone":                   {state(dirOK, [3]string{b, fileOK, "PEM-B"}), []string{a}, "Certificate file a1a1a1a1a1a1 is missing"},
		"a file truncated":              {state(dirOK, [3]string{a, fileOK, "PEM-A"}, [3]string{b, fileOK, ""}), []string{b}, "Certificate file b2b2b2b2b2b2 is not the file that was written"},
		"another certificate's bytes":   {state(dirOK, [3]string{a, fileOK, "PEM-B"}, [3]string{b, fileOK, "PEM-B"}), []string{a}, "Certificate file a1a1a1a1a1a1 is not the file that was written"},
		"a file anybody reads":          {state(dirOK, [3]string{a, "644:root:root", "PEM-A"}, [3]string{b, fileOK, "PEM-B"}), []string{a}, "Certificate file a1a1a1a1a1a1 is not root's alone"},
		"a file of another user":        {state(dirOK, [3]string{a, fileOK, "PEM-A"}, [3]string{b, "600:haproxy:haproxy", "PEM-B"}), []string{b}, "Certificate file b2b2b2b2b2b2 is not root's alone"},
		"a directory anybody enters":    {state("755:root:root:directory", [3]string{a, fileOK, "PEM-A"}, [3]string{b, fileOK, "PEM-B"}), nil, "The certificate directory is missing or is not root's alone"},
		"a directory that is a link":    {state("777:root:root:symbolic link", [3]string{a, fileOK, "PEM-A"}, [3]string{b, fileOK, "PEM-B"}), nil, "The certificate directory is missing or is not root's alone"},
		"no directory":                  {"", []string{a, b}, "The certificate directory is missing or is not root's alone"},
		"an answer that is not the one": {"something else\n", []string{a, b}, "The certificate directory is missing or is not root's alone"},
	} {
		stale, why := lbCertFaults(c.out, certs)
		var names []string
		for _, s := range stale {
			names = append(names, s.Name)
		}
		if !slices.Equal(names, c.stale) || why != c.why {
			t.Errorf("%s: stale %v, %q; want %v, %q", name, names, why, c.stale, c.why)
		}
		if strings.Contains(why, "PEM-") || strings.Contains(why, digest("PEM-A")) || strings.Contains(why, digest("PEM-B")) {
			t.Errorf("%s: the answer holds something of a file's content: %q", name, why)
		}
	}
	// A config that names no certificate is asked nothing, whatever the guest holds.
	if stale, why := lbCertFaults("", nil); stale != nil || why != "" {
		t.Errorf("no certificate: %v %q", stale, why)
	}

	guest := func(out string) (*certGuest, *Service) {
		g := &certGuest{exits: map[string]int{}, runs: map[int]int{}, files: map[string]string{}, out: out}
		srv := httptest.NewServer(g)
		t.Cleanup(srv.Close)
		ps, err := NewService(Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
		if err != nil {
			t.Fatal(err)
		}
		return g, ps
	}
	// A single appliance. Without a certificate: the command of v0.11.1-rc.1, written out here.
	before := "sh -c " + lbServingFn + "serving && echo ok $(sha256sum < /etc/haproxy/haproxy.cfg | cut -c1-64)"
	g, ps := guest("ok " + digest("config") + "\n")
	if ok, why, err := ps.HAProxyServing(ctx, "n1", 100, digest("config")); err != nil || !ok || why != "" || len(g.cmds) != 1 || g.cmds[0] != before {
		t.Errorf("no certificate: %v %q %v, sent %d commands:\n%s", ok, why, err, len(g.cmds), strings.Join(g.cmds, "\n"))
	}
	// With certificates: still one command, the one of before with the files' state after it.
	g, ps = guest("ok " + digest("config") + "\n" + good)
	if ok, why, err := ps.HAProxyServing(ctx, "n1", 100, digest("config"), certs...); err != nil || !ok || why != "" || len(g.cmds) != 1 ||
		g.cmds[0] != "sh -c "+lbCertStateFn+strings.TrimPrefix(before, "sh -c ")+" && certstate" {
		t.Errorf("certificates in place: %v %q %v, sent %d commands:\n%s", ok, why, err, len(g.cmds), strings.Join(g.cmds, "\n"))
	}
	g, ps = guest("ok " + digest("config") + "\n" + state(dirOK, [3]string{a, fileOK, "PEM-A"}, [3]string{b, fileOK, ""}))
	if ok, why, err := ps.HAProxyServing(ctx, "n1", 100, digest("config"), certs...); err != nil || ok || why != "Certificate file b2b2b2b2b2b2 is not the file that was written" {
		t.Errorf("a truncated file: %v %q %v", ok, why, err)
	}
	// A config edited by hand is said first, and a HAProxy that does not serve is said as before.
	g, ps = guest("ok " + digest("another config") + "\n" + good)
	if ok, why, err := ps.HAProxyServing(ctx, "n1", 100, digest("config"), certs...); err != nil || ok || why != "HAProxy's configuration file is not the one that was applied" {
		t.Errorf("another config: %v %q %v", ok, why, err)
	}
	g, ps = guest("HAProxy is not running\n")
	g.exits["other "] = 1
	if ok, why, err := ps.HAProxyServing(ctx, "n1", 100, digest("config"), certs...); err != nil || ok || why != "HAProxy is not running" {
		t.Errorf("HAProxy stopped: %v %q %v", ok, why, err)
	}

	// An appliance of a pair: the state line first, as before, then the files' state from the same command.
	line := "MASTER 1 1 1 " + digest("config") + " " + digest("keepalived") + " - 0\n"
	g, ps = guest(line)
	st, err := ps.ReadVRRPState(ctx, "n1", 100, "203.0.113.10")
	if err != nil || st.State != "MASTER" || !st.Serving || st.HAProxySum != digest("config") || st.CertFiles != "" || len(g.cmds) != 1 || strings.Contains(g.cmds[0], "certstate") {
		t.Errorf("a pair without a certificate: %+v %v\n%s", st, err, strings.Join(g.cmds, "\n"))
	}
	without := g.cmds[0]
	g, ps = guest(line + good)
	if st, err := ps.ReadVRRPState(ctx, "n1", 100, "203.0.113.10", certs...); err != nil || st.State != "MASTER" || !st.Serving || !st.HoldsVIP || st.HAProxySum != digest("config") ||
		st.KeepalivedSum != digest("keepalived") || st.CertFiles != "" || len(g.cmds) != 1 || g.cmds[0] != "sh -c "+lbCertStateFn+strings.TrimPrefix(without, "sh -c ")+"; certstate" {
		t.Errorf("a pair with its certificates in place: %+v %v\n%s", st, err, strings.Join(g.cmds, "\n"))
	}
	g, ps = guest(line + state(dirOK, [3]string{b, fileOK, "PEM-B"}))
	if st, err := ps.ReadVRRPState(ctx, "n1", 100, "203.0.113.10", certs...); err != nil || st.State != "MASTER" || !st.Serving || st.CertFiles != "Certificate file a1a1a1a1a1a1 is missing" {
		t.Errorf("a pair with a file gone: %+v %v", st, err)
	}

	// certstate itself: three commands whatever the number of files, and none that prints a file's content.
	if strings.Count(lbCertStateFn, "\n") != 5 || strings.Contains(lbCertStateFn, "cat ") || strings.Contains(lbCertStateFn, "haproxy -c") {
		t.Errorf("certstate:\n%s", lbCertStateFn)
	}
}
