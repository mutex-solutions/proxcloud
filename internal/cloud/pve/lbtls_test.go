package pve

import (
	"context"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
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

// certGuest is the guest agent of an appliance for the delivery's order: it logs each command by what it is and each
// file written, and answers a command with the exit code set for it.
type certGuest struct {
	mu    sync.Mutex
	log   []string
	exits map[string]int
	runs  map[int]int
	files map[string]string
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
		case strings.Contains(cmd, "a certificate file is not root's alone"):
			what = "certificate install " + strings.Join(r.Form["command"][4:], ",")
		case strings.Contains(cmd, "dpkg-query"):
			what = "ready"
		case strings.Contains(cmd, "haproxy -c"):
			what = "reload"
		}
		g.log = append(g.log, what)
		pid := len(g.runs) + 1
		g.runs[pid] = g.exits[strings.SplitN(what, " ", 3)[0]+" "+strings.SplitN(what+" ", " ", 3)[1]]
		write(map[string]any{"pid": pid})
	case "GET exec-status":
		pid, _ := strconv.Atoi(r.Form.Get("pid"))
		write(map[string]any{"exited": 1, "exitcode": g.runs[pid], "err-data": "refused by the guest"})
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
	if err := ps.PushPairHAProxy(ctx, "n1", 100, "config", certs...); err != nil {
		t.Fatal(err)
	}
	want := []string{"ready", "certificate directory", "write /etc/haproxy/certs/" + a + ".pem.new", "write /etc/haproxy/certs/" + b + ".pem.new",
		"certificate install " + a + "," + b, "write /etc/haproxy/haproxy.cfg.new", "reload"}
	if !slices.Equal(g.log, want) {
		t.Errorf("order of a push:\n%s\nwant\n%s", strings.Join(g.log, "\n"), strings.Join(want, "\n"))
	}
	if g.files["/etc/haproxy/certs/"+a+".pem.new"] != "PEM-A" || len(g.files) != 3 {
		t.Errorf("files written: %v", g.log)
	}
	// A single appliance: the same order.
	g, ps = guest()
	if err := ps.PushHAProxy(ctx, "n1", 100, "config", certs[:1]...); err != nil || !slices.Equal(g.log, []string{"ready", "certificate directory",
		"write /etc/haproxy/certs/" + a + ".pem.new", "certificate install " + a, "write /etc/haproxy/haproxy.cfg.new", "reload"}) {
		t.Errorf("single appliance: %v\n%s", err, strings.Join(g.log, "\n"))
	}
	// A directory that cannot be made private: nothing is written, not the config either.
	g, ps = guest()
	g.exits["certificate directory"] = 1
	if err := ps.PushPairHAProxy(ctx, "n1", 100, "config", certs...); err == nil || !strings.Contains(err.Error(), "nothing was written") || len(g.files) != 0 {
		t.Errorf("a directory that is not private: %v, wrote %v", err, g.log)
	}
	// Files that could not be given to root alone: the config that names them is not written.
	g, ps = guest()
	g.exits["certificate install"] = 1
	if err := ps.PushPairHAProxy(ctx, "n1", 100, "config", certs...); err == nil || slices.Contains(g.log, "write /etc/haproxy/haproxy.cfg.new") || slices.Contains(g.log, "reload") {
		t.Errorf("files not put in place: %v, log %v", err, g.log)
	}
	// A name that is not a digest reaches no command and no path.
	g, ps = guest()
	if err := ps.PushPairHAProxy(ctx, "n1", 100, "config", LBCertFile{Name: "x; rm -rf /", PEM: "PEM"}); err == nil || !slices.Equal(g.log, []string{"ready"}) {
		t.Errorf("a bad name: %v, log %v", err, g.log)
	}
	// No certificate: the push of before, to the byte of its calls.
	g, ps = guest()
	if err := ps.PushPairHAProxy(ctx, "n1", 100, "config"); err != nil || !slices.Equal(g.log, []string{"ready", "write /etc/haproxy/haproxy.cfg.new", "reload"}) {
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
