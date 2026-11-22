//go:build integration

package project

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve/lbcerttest"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// A certificate from its upload to both appliances of a pair, against a real PostgreSQL and the pair's wire fake
// (which models the Proxmox API and what a guest answers, not HAProxy: TestLBApplianceTLS has the real one).
//
// Shown: the key is in the database only sealed, bound to its project and id; on the wire it is in the file writes
// into the appliances' certificate directory and in nothing else; no log line and no row of a balancer holds it; each
// appliance gets the directory check, then its certificate files, then the config that names them, the standby
// before the appliance that holds the address; a changed certificate changes the hash and is one push; a quiet pass
// writes nothing; a listener cannot name a certificate of another project or one that does not exist; a certificate
// a listener names is not deleted; one that went missing all the same leaves the appliances as they are; and
// without the portal's key nothing is stored and nothing is pushed.
func TestLBCertificatesReachThePair(t *testing.T) {
	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("lb"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = testcontainers.TerminateContainer(pgc) }()
	dsn, _ := pgc.ConnectionString(ctx, "sslmode=disable")
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)

	var logs bytes.Buffer
	var logMu sync.Mutex
	before := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(before)

	// Every request that reaches Proxmox, as it was sent.
	type call struct{ vm, what, file, body string }
	var wire []call
	var wireMu sync.Mutex
	f := newPairWire(t, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		parts := strings.Split(r.URL.Path, "/")
		c := call{what: r.Method + " " + parts[len(parts)-1], file: r.Form.Get("file"), body: r.URL.RawQuery + "\n" + r.Form.Encode() + "\n" + strings.Join(r.Form["command"], " ") + "\n" + r.Form.Get("content")}
		if i := slices.Index(parts, "qemu"); i >= 0 && i+1 < len(parts) {
			c.vm = parts[i+1]
		}
		wireMu.Lock()
		wire = append(wire, c)
		wireMu.Unlock()
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := textcrypt.NewBlobKeys("default-v1", map[string]string{"default-v1": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cloud: cloud.NewRepo(db), sdnLock: lock.New(db), publicIPs: NewPublicIPRepo(db),
		pveFor: func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil }}
	h.SetLBCertificates(db.C("lb_certificates"), keys)
	es := &externalservice.ExternalService{ID: "svc", Config: map[string]any{"network": map[string]any{"publicNetworks": []any{
		map[string]any{"id": "vmbr1", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1", "ranges": []any{"203.0.113.10-203.0.113.20"}}}}}}
	proj := &Project{ID: "p1"}

	until := time.Now().Add(72 * time.Hour)
	first := lbcerttest.New(t, lbcerttest.RSA(t, 2048), until, "www.example.org")
	second := lbcerttest.New(t, lbcerttest.RSA(t, 2048), until, "www.example.org")
	foreign := lbcerttest.New(t, lbcerttest.RSA(t, 2048), until, "other.example.org")
	// A line of each key's own text: what must be nowhere but in an appliance's certificate file.
	secrets := []string{strings.Split(first.Key, "\n")[2], strings.Split(second.Key, "\n")[2], strings.Split(foreign.Key, "\n")[2]}
	holdsKey := func(s string) bool {
		return strings.Contains(s, "PRIVATE KEY") || slices.ContainsFunc(secrets, func(k string) bool { return strings.Contains(s, k) })
	}

	// Without the portal's key: refused, nothing stored.
	noKey := &Handler{cloud: h.cloud}
	noKey.SetLBCertificates(db.C("lb_certificates"), nil)
	if _, _, err := noKey.lbCertStore(ctx, "p1", "sub", "web", first.Chain, first.Key); err == nil || err.(*httpx.HTTPError).Status != http.StatusServiceUnavailable {
		t.Fatalf("an upload without the portal's key: %v", err)
	}
	if n, _ := db.C("lb_certificates").Count(ctx, pgdoc.M{}); n != 0 {
		t.Fatalf("%d certificates stored without a key", n)
	}

	c1, _, err := h.lbCertStore(ctx, "p1", "sub", "web", first.Chain, first.Key)
	if err != nil {
		t.Fatal(err)
	}
	c2, _, err := h.lbCertStore(ctx, "p1", "sub", "web-2027", second.Chain, second.Key)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := h.lbCertStore(ctx, "p2", "sub", "web", foreign.Chain, foreign.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.lbCertStore(ctx, "p1", "sub", "web", second.Chain, second.Key); err == nil || err.(*httpx.HTTPError).Status != http.StatusConflict {
		t.Errorf("a second certificate of the same name: %v", err)
	}
	// At rest: sealed, and bound to the project and the id.
	var stored []json.RawMessage
	if err := db.C("lb_certificates").Find(ctx, pgdoc.M{}, &stored); err != nil || len(stored) != 3 {
		t.Fatalf("stored certificates: %d %v", len(stored), err)
	}
	for _, doc := range stored {
		var row lbCertificate
		_ = json.Unmarshal(doc, &row)
		blob, _ := base64.StdEncoding.DecodeString(row.Sealed)
		if holdsKey(string(doc)) || holdsKey(string(blob)) || len(blob) < 1000 || row.KeyID != "default-v1" {
			t.Errorf("certificate %s at rest is not sealed", row.Name)
		}
	}
	if file, err := h.lbCertOpen(c1); err != nil || file.Name != c1.File || !strings.Contains(file.PEM, secrets[0]) || strings.Count(file.PEM, "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("open: %v", err)
	}
	moved := *c1
	moved.ProjectID = "p2"
	if _, err := h.lbCertOpen(&moved); err == nil {
		t.Error("a blob copied to another project opened")
	}
	moved = *c1
	moved.ID = c2.ID
	if _, err := h.lbCertOpen(&moved); err == nil {
		t.Error("a blob copied to another certificate opened")
	}
	otherKeys, _ := textcrypt.NewBlobKeys("default-v1", map[string]string{"default-v1": strings.Repeat("x", 32)})
	wrong := &Handler{lbCertKeys: otherKeys}
	if _, err := wrong.lbCertOpen(c1); err == nil {
		t.Error("a blob opened with another key")
	}
	if _, err := noKey.lbCertOpen(c1); err == nil {
		t.Error("a blob opened without a key")
	}

	// A pair with an https and a tls listener.
	lb := &loadBalancer{ID: "abc", Name: "web", Status: lbStatusBuild, NetworkID: "v1", PublicNetworkID: "vmbr1", HA: true,
		ServerID: "301", PrivateIP: "10.0.0.31", PrivatePrefix: 24, PublicIP: "203.0.113.10", VIPs: []string{"203.0.113.10"},
		Appliances: []lbAppliance{{ServerID: "301", PrivateIP: "10.0.0.31"}, {ServerID: "302", PrivateIP: "10.0.0.32", HoldsVIP: true}},
		LBSpec:     pve.LBSpec{Listeners: []pve.LBListener{{Port: 80, Pool: "web"}}, Pools: []pve.LBPool{{Name: "web", Protocol: "http", Algorithm: "roundrobin"}}}}
	row := &cloud.CloudResource{ExternalID: "lb-abc", ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(row, lb); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cloud.Insert(ctx, row); err != nil {
		t.Fatal(err)
	}
	update := func(certs ...string) error {
		return h.pveLBUpdateConfig(ctx, es, proj, row, map[string]any{
			"listeners": []any{
				map[string]any{"port": 443, "protocol": "https", "pool": "web", "certificates": certs},
				map[string]any{"port": 8443, "protocol": "tls", "pool": "raw", "certificates": certs[:1]},
			},
			"pools": []any{map[string]any{"name": "web", "protocol": "http"}, map[string]any{"name": "raw", "protocol": "tcp"}},
		})
	}
	saved := func() *loadBalancer {
		t.Helper()
		cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", "lb-abc")
		if err != nil || cr == nil {
			t.Fatal(err)
		}
		got, err := lbOfRow(cr)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// A listener names certificates of its own project only.
	for name, id := range map[string]string{"another project's": other.ID, "one that does not exist": "0123456789abcdef0123456789abcdef"} {
		if err := update(id); err == nil || err.(*httpx.HTTPError).Status != http.StatusBadRequest || !strings.Contains(err.Error(), "not a certificate of this project") {
			t.Errorf("a listener naming %s certificate: %v", name, err)
		}
	}
	if err := update(c1.ID); err != nil {
		t.Fatal(err)
	}
	if got := saved(); !slices.Equal(got.Listeners[0].CertFiles, []string{c1.File}) || !slices.Equal(got.Listeners[1].CertFiles, []string{c1.File}) {
		t.Fatalf("the row's certificate files: %+v", got.Listeners)
	}
	if status, err := h.pveLBApply(ctx, es, proj, "lb-abc"); err != nil || status != lbStatusActive {
		t.Fatalf("apply: %s %v\n%s", status, err, strings.Join(f.log, "\n"))
	}
	// The order on each appliance, and between them.
	events := func(from int) (out []string) {
		wireMu.Lock()
		defer wireMu.Unlock()
		for _, c := range wire[from:] {
			switch {
			case strings.Contains(c.body, "is not a directory of root alone"):
				out = append(out, c.vm+" certificate directory")
			case strings.HasPrefix(c.file, "/etc/haproxy/certs/"):
				out = append(out, c.vm+" write "+c.file)
			case strings.Contains(c.body, "a certificate file is not root's alone"):
				out = append(out, c.vm+" certificate install")
			case c.file == "/etc/haproxy/haproxy.cfg.new":
				out = append(out, c.vm+" write config")
			case strings.Contains(c.body, "haproxy -c"):
				out = append(out, c.vm+" reload")
			}
		}
		return out
	}
	steps := func(vm, file string) []string {
		return []string{vm + " certificate directory", vm + " write /etc/haproxy/certs/" + file + ".pem.new", vm + " certificate install", vm + " write config", vm + " reload"}
	}
	if got, want := events(0), append(steps("301", c1.File), steps("302", c1.File)...); !slices.Equal(got, want) {
		t.Errorf("order of the first push:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, config := range f.files {
		if !strings.Contains(config, "bind 203.0.113.10:443 ssl crt /etc/haproxy/certs/"+c1.File+".pem alpn h2,http/1.1\n") ||
			!strings.Contains(config, "bind 203.0.113.10:8443 ssl crt /etc/haproxy/certs/"+c1.File+".pem\n") || !strings.Contains(config, "ssl-min-ver TLSv1.2") {
			t.Errorf("the pushed config:\n%s", config)
		}
	}
	for _, vm := range []string{"301", "302"} {
		if !slices.ContainsFunc(f.rules[vm], func(r pve.FirewallRule) bool {
			return r.Proto == "tcp" && r.Dest == "203.0.113.10" && r.DPort == "443,8443"
		}) {
			t.Errorf("%s rules: %+v", vm, f.rules[vm])
		}
	}
	// A quiet pass writes nothing: no certificate either.
	mark := len(wire)
	if status, err := h.pveLBApply(ctx, es, proj, "lb-abc"); err != nil || status != lbStatusActive || len(events(mark)) != 0 {
		t.Fatalf("a quiet pass: %s %v, wrote %v", status, err, events(mark))
	}

	// A certificate a listener names is not one to delete.
	if used, err := h.lbCertsUsed(ctx, "p1"); err != nil || !slices.Equal(used[c1.ID], []string{"web"}) || len(used[c2.ID]) != 0 {
		t.Errorf("who uses what: %v %v", used, err)
	}

	// The certificate is changed: the listeners name the new one. One push, the new file before the config.
	applied := saved().AppliedHash
	mark = len(wire)
	if err := update(c2.ID, c1.ID); err != nil {
		t.Fatal(err)
	}
	if status, err := h.pveLBApply(ctx, es, proj, "lb-abc"); err != nil || status != lbStatusActive {
		t.Fatalf("apply of the changed certificate: %s %v", status, err)
	}
	if got := saved(); got.AppliedHash == applied || got.AppliedHash == "" {
		t.Error("a changed certificate left the applied hash as it was")
	}
	got := events(mark)
	want := []string{}
	for _, vm := range []string{"301", "302"} {
		want = append(want, vm+" certificate directory", vm+" write /etc/haproxy/certs/"+c2.File+".pem.new", vm+" write /etc/haproxy/certs/"+c1.File+".pem.new",
			vm+" certificate install", vm+" write config", vm+" reload")
	}
	if !slices.Equal(got, want) {
		t.Errorf("order of the push of a changed certificate:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if last := f.files[len(f.files)-1]; !strings.Contains(last, "bind 203.0.113.10:443 ssl crt /etc/haproxy/certs/"+c2.File+".pem crt /etc/haproxy/certs/"+c1.File+".pem alpn") {
		t.Errorf("the first certificate is not the new one:\n%s", last)
	}

	// A certificate gone from under a listener (the race a delete can lose): the appliances keep what they serve,
	// and the row says why.
	if _, err := db.C("lb_certificates").DeleteByID(ctx, c1.ID); err != nil {
		t.Fatal(err)
	}
	if err := update(c2.ID); err != nil { // the tenant's own way out still works
		t.Fatal(err)
	}
	if _, err := db.C("lb_certificates").DeleteByID(ctx, c2.ID); err != nil {
		t.Fatal(err)
	}
	mark = len(wire)
	status, err := h.pveLBApply(ctx, es, proj, "lb-abc")
	if got := saved(); err != nil || len(events(mark)) != 0 || !strings.Contains(got.StatusMessage, "names a certificate that is not a certificate of this project") {
		t.Errorf("apply with a certificate gone: %s %v %q, wrote %v", status, err, got.StatusMessage, events(mark))
	}

	// Where the key went: into certificate files on the two appliances, and nowhere else on the wire.
	wireMu.Lock()
	for _, c := range wire {
		toCertDir := c.what == "POST file-write" && strings.HasPrefix(c.file, "/etc/haproxy/certs/") && strings.HasSuffix(c.file, ".pem.new")
		if holdsKey(c.body) != toCertDir {
			t.Errorf("%s %s (vm %s, file %q): holds a key %v", c.what, c.file, c.vm, c.file, holdsKey(c.body))
		}
	}
	wireMu.Unlock()
	// Nor in a log line, the balancer's row or the fake's own log of what was written where.
	logMu.Lock()
	if holdsKey(logs.String()) {
		t.Error("a log line holds the key")
	}
	logMu.Unlock()
	cr, _ := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", "lb-abc")
	if raw, _ := json.Marshal(cr); holdsKey(string(raw)) {
		t.Error("the balancer's row holds the key")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
