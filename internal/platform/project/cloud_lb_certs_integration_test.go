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
	record := func(f *pairWire) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		})
	}
	srv := httptest.NewServer(record(f))
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

	// The certificate is changed: the listeners name the new one. One push, the new file before the config. The
	// certificate that stays is on each appliance already and is not written again (drill Q3-o2 of v0.11.1-rc.1).
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
		want = append(want, vm+" certificate directory", vm+" write /etc/haproxy/certs/"+c2.File+".pem.new", vm+" certificate install", vm+" write config", vm+" reload")
	}
	if !slices.Equal(got, want) {
		t.Errorf("order of the push of a changed certificate:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if last := f.files[len(f.files)-1]; !strings.Contains(last, "bind 203.0.113.10:443 ssl crt /etc/haproxy/certs/"+c2.File+".pem crt /etc/haproxy/certs/"+c1.File+".pem alpn") {
		t.Errorf("the first certificate is not the new one:\n%s", last)
	}

	// A change of certificate is no damage. An appliance does not hold a new certificate's file until its push, and
	// the state read asks an appliance only for the files of the config its row records for it. Read as a fault, a
	// change of certificate logged a config not served on both appliances, and the appliance that holds the address
	// was pushed after the standby had refused the config. Here the standby (301) refuses a config with a
	// certificate new to both: the holder (302) gets nothing, and takes it at the pass after, when 301 does.
	logMu.Lock()
	logMark := logs.Len()
	logMu.Unlock()
	if err := update(c1.ID); err != nil {
		t.Fatal(err)
	}
	if status, err := h.pveLBApply(ctx, es, proj, "lb-abc"); err != nil || status != lbStatusActive {
		t.Fatalf("apply without the second certificate: %s %v", status, err)
	}
	f.set("301", func(g *lbGuest) { g.applyFails = "[ALERT] (1) : refused on the standby" })
	if err := update(c2.ID, c1.ID); err != nil {
		t.Fatal(err)
	}
	mark = len(wire)
	if status, err := h.pveLBApply(ctx, es, proj, "lb-abc"); err != nil || status != lbStatusError || !slices.Equal(events(mark), steps("301", c2.File)) {
		t.Fatalf("a config with a new certificate that the standby refuses: %s %v, wrote\n%s", status, err, strings.Join(events(mark), "\n"))
	}
	f.set("301", func(g *lbGuest) { g.applyFails = "" })
	mark = len(wire)
	taken := append([]string{"301 certificate directory", "301 write config", "301 reload"}, steps("302", c2.File)...)
	if status, err := h.pveLBApply(ctx, es, proj, "lb-abc"); err != nil || status != lbStatusActive || !slices.Equal(events(mark), taken) {
		t.Fatalf("the pass after the standby took it: %s %v, wrote\n%s", status, err, strings.Join(events(mark), "\n"))
	}
	logMu.Lock()
	if out := logs.String()[logMark:]; strings.Contains(out, "does not run what its row records") {
		t.Errorf("a change of certificate was logged as a config that is not served:\n%s", out)
	}
	logMu.Unlock()

	// A certificate file damaged on an appliance while HAProxy serves from memory (drill Q3-1 of v0.11.1-rc.1: the
	// row read ACTIVE, nothing pushed, and HAProxy would have refused its whole config at its next start). The state
	// read of the pass asks for the files of the config in the command it already sent; the appliance whose files
	// are not as a push leaves them is pushed again, the other is not, and with both the standby comes first. HAProxy
	// is never stopped for it: what a heal costs is the one reload of a push, and the write of the one file that
	// was not in order (none for a directory somebody opened up: the push's first command closes it).
	file1, err := h.lbCertOpen(c1)
	if err != nil {
		t.Fatal(err)
	}
	file2, err := h.lbCertOpen(c2)
	if err != nil {
		t.Fatal(err)
	}
	certPath := func(file string) string { return lbGuestCertDir + "/" + file + ".pem" }
	pushOf := func(vm string, files ...string) []string {
		out := []string{vm + " certificate directory"}
		for _, file := range files {
			out = append(out, vm+" write "+certPath(file)+".new")
		}
		if len(files) > 0 {
			out = append(out, vm+" certificate install")
		}
		return append(out, vm+" write config", vm+" reload")
	}
	for _, c := range []struct {
		what   string
		damage map[string]func(g *lbGuest)
		want   []string
		logged string
	}{
		{"a file truncated on the appliance that holds the address", map[string]func(g *lbGuest){"302": func(g *lbGuest) { g.disk[certPath(c1.File)] = "" }},
			pushOf("302", c1.File), "Certificate file " + c1.File[:12] + " is not the file that was written"},
		{"a file deleted on the standby", map[string]func(g *lbGuest){"301": func(g *lbGuest) { delete(g.disk, certPath(c2.File)) }},
			pushOf("301", c2.File), "Certificate file " + c2.File[:12] + " is missing"},
		{"a file anybody reads on one, a file gone on the other", map[string]func(g *lbGuest){
			"301": func(g *lbGuest) { g.certMode = map[string]string{certPath(c1.File): "644:root:root"} },
			"302": func(g *lbGuest) { delete(g.disk, certPath(c1.File)) }},
			append(pushOf("301", c1.File), pushOf("302", c1.File)...), "Certificate file " + c1.File[:12] + " is not root's alone"},
		{"a directory anybody enters", map[string]func(g *lbGuest){"301": func(g *lbGuest) { g.certMode = map[string]string{lbGuestCertDir: "755:root:root:directory"} }},
			pushOf("301"), "The certificate directory is missing or is not root's alone"},
	} {
		for vm, change := range c.damage {
			f.set(vm, change)
		}
		reloads := f.count("301 haproxy reload") + f.count("302 haproxy reload")
		mark = len(wire)
		status, err := h.pveLBApply(ctx, es, proj, "lb-abc")
		if got := saved(); err != nil || status != lbStatusActive || got.StatusMessage != "" || !slices.Equal(events(mark), c.want) ||
			f.count("301 haproxy reload")+f.count("302 haproxy reload") != reloads+len(c.damage) {
			t.Fatalf("%s: %s %v %q, wrote\n%s\nwant\n%s", c.what, status, err, got.StatusMessage, strings.Join(events(mark), "\n"), strings.Join(c.want, "\n"))
		}
		for _, vm := range []string{"301", "302"} {
			f.set(vm, func(g *lbGuest) {
				if g.disk[certPath(c1.File)] != file1.PEM || g.disk[certPath(c2.File)] != file2.PEM || len(g.certMode) != 0 || len(g.certFiles()) != 2 || !g.serving() {
					t.Errorf("%s: appliance %s after the pass: files %v, modes %v, serving %v", c.what, vm, g.certFiles(), g.certMode, g.serving())
				}
			})
		}
		logMu.Lock()
		if !strings.Contains(logs.String(), c.logged) {
			t.Errorf("%s: no log line says %q", c.what, c.logged)
		}
		logMu.Unlock()
		// The pass after: one state read per appliance, each asked for its files, and nothing written.
		mark = len(wire)
		f.mu.Lock()
		execs, asked := f.execs, f.certReads
		f.mu.Unlock()
		if status, err := h.pveLBApply(ctx, es, proj, "lb-abc"); err != nil || status != lbStatusActive || len(events(mark)) != 0 {
			t.Fatalf("%s: the pass after: %s %v, wrote %v", c.what, status, err, events(mark))
		}
		f.mu.Lock()
		if f.execs != execs+2 || f.certReads != asked+2 {
			t.Errorf("%s: the pass after sent %d commands, %d of them asked for the certificate files; want 2 and 2", c.what, f.execs-execs, f.certReads-asked)
		}
		f.mu.Unlock()
	}
	// What the portal compares is a file's digest: it is in no log line and not in the row either.
	logMu.Lock()
	for _, file := range []pve.LBCertFile{file1, file2} {
		if strings.Contains(logs.String(), lbHash(file.PEM)) {
			t.Error("a log line holds the digest of a certificate file")
		}
	}
	logMu.Unlock()
	if raw, _ := json.Marshal(saved()); strings.Contains(string(raw), lbHash(file1.PEM)) || strings.Contains(string(raw), lbHash(file2.PEM)) {
		t.Error("the balancer's row holds the digest of a certificate file")
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
	if got := saved(); err != nil || len(events(mark)) != 0 || !strings.Contains(got.StatusMessage, "names a certificate that is not a certificate of this project") ||
		!slices.Equal(got.Listeners[0].CertFiles, []string{c2.File}) { // the row still renders the config the appliances serve
		t.Errorf("apply with a certificate gone: %s %v %q, wrote %v", status, err, got.StatusMessage, events(mark))
	}

	// A pair with one appliance (drill Q0b-1 of v0.11.1), on a second Proxmox of its own: a replace whose new clone
	// failed leaves a tenant's pair so. The certificate follows the config: it goes to the one appliance after
	// keepalived, which is what lets HAProxy bind the address, and that appliance serves alone; when the second
	// appliance comes it gets the certificate with its config, and the first is given nothing again.
	alone := newPairWire(t, "")
	delete(alone.guests, "302")
	*alone.guests["301"] = lbGuest{packages: "installed", haproxy: true}
	srv2 := httptest.NewServer(record(alone))
	defer srv2.Close()
	ps2, err := pve.NewService(pve.Config{APIURL: srv2.URL, TokenID: "t", Secret: "s"}, "svc2", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	h2 := &Handler{cloud: h.cloud, sdnLock: h.sdnLock, publicIPs: h.publicIPs, pveFor: func(*externalservice.ExternalService) (*pve.Service, error) { return ps2, nil }}
	h2.SetLBCertificates(db.C("lb_certificates"), keys)
	es2 := &externalservice.ExternalService{ID: "svc2", Config: es.Config}
	row2 := &cloud.CloudResource{ExternalID: "lb-abc", ServiceID: "svc2", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(row2, &loadBalancer{ID: "abc", Name: "web", Status: lbStatusDegraded, NetworkID: "v1", PublicNetworkID: "vmbr1", HA: true,
		ServerID: "301", PrivateIP: "10.0.0.31", PrivatePrefix: 24, PublicIP: "203.0.113.11", VIPs: []string{"203.0.113.11"},
		Appliances: []lbAppliance{{ServerID: "301", PrivateIP: "10.0.0.31"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.cloud.Insert(ctx, row2); err != nil {
		t.Fatal(err)
	}
	third := lbcerttest.New(t, lbcerttest.RSA(t, 2048), until, "alone.example.org")
	c3, _, err := h.lbCertStore(ctx, "p1", "sub", "alone", third.Chain, third.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := h2.pveLBUpdateConfig(ctx, es2, proj, row2, map[string]any{
		"listeners": []any{map[string]any{"port": 443, "protocol": "https", "pool": "web", "certificates": []string{c3.ID}}},
		"pools":     []any{map[string]any{"name": "web", "protocol": "http"}},
	}); err != nil {
		t.Fatal(err)
	}
	mark = len(wire)
	if status, err := h2.pveLBApply(ctx, es2, proj, "lb-abc"); err != nil || status != lbStatusDegraded {
		t.Fatalf("one appliance with a certificate: %s %v\n%s", status, err, strings.Join(alone.log, "\n"))
	}
	cr2, _ := h2.cloud.FindByServiceIDAndExternalID(ctx, "svc2", "lb-abc")
	one, _ := lbOfRow(cr2)
	if a := one.Appliances[0]; a.State != lbAppUp || a.Role != "MASTER" || !a.HoldsVIP || !strings.Contains(one.StatusMessage, "One appliance carries the traffic") ||
		!alone.guests["301"].serving() || !slices.Equal(events(mark), steps("301", c3.File)) {
		t.Fatalf("one appliance with a certificate: %q, %+v, wrote\n%s", one.StatusMessage, a, strings.Join(events(mark), "\n"))
	}
	if k, c := alone.first("301 keepalived reload"), alone.first("301 write /etc/haproxy/certs/"+c3.File+".pem.new"); k < 0 || k > c {
		t.Fatalf("the certificate reached the appliance before keepalived:\n%s", strings.Join(alone.log, "\n"))
	}
	alone.mu.Lock()
	alone.guests["302"] = &lbGuest{packages: "installed", haproxy: true}
	alone.mu.Unlock()
	if _, err := h2.lbEdit(ctx, es2, "lb-abc", func(lb *loadBalancer) { lbPutIn(lb, lbAppliance{ServerID: "302", PrivateIP: "10.0.0.32"}) }); err != nil {
		t.Fatal(err)
	}
	mark = len(wire)
	if status, err := h2.pveLBApply(ctx, es2, proj, "lb-abc"); err != nil || status != lbStatusActive || !slices.Equal(events(mark), steps("302", c3.File)) {
		t.Fatalf("the second appliance with a certificate: %s %v, wrote\n%s", status, err, strings.Join(events(mark), "\n"))
	}

	// A balancer on one VM, on a third Proxmox of its own (the balancer of drill Q3 of v0.11.1-rc.1): the same
	// rule. Its one state read asks for the files too, a damaged or missing file is written again in the pass that
	// sees it, and the row never leaves ACTIVE over it.
	one1 := newPairWire(t, "")
	delete(one1.guests, "302")
	*one1.guests["301"] = lbGuest{packages: "installed", haproxy: true}
	srv3 := httptest.NewServer(record(one1))
	defer srv3.Close()
	ps3, err := pve.NewService(pve.Config{APIURL: srv3.URL, TokenID: "t", Secret: "s"}, "svc3", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	h3 := &Handler{cloud: h.cloud, sdnLock: h.sdnLock, publicIPs: h.publicIPs, pveFor: func(*externalservice.ExternalService) (*pve.Service, error) { return ps3, nil }}
	h3.SetLBCertificates(db.C("lb_certificates"), keys)
	es3 := &externalservice.ExternalService{ID: "svc3", Config: es.Config}
	row3 := &cloud.CloudResource{ExternalID: "lb-abc", ServiceID: "svc3", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(row3, &loadBalancer{ID: "abc", Name: "one", Status: lbStatusBuild, NetworkID: "v1", PublicNetworkID: "vmbr1",
		ServerID: "301", PrivateIP: "10.0.0.31", PrivatePrefix: 24, PublicIP: "203.0.113.12"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h3.cloud.Insert(ctx, row3); err != nil {
		t.Fatal(err)
	}
	if err := h3.pveLBUpdateConfig(ctx, es3, proj, row3, map[string]any{
		"listeners": []any{map[string]any{"port": 443, "protocol": "https", "pool": "web", "certificates": []string{c3.ID}}, map[string]any{"port": 80, "protocol": "http", "pool": "web"}},
		"pools":     []any{map[string]any{"name": "web", "protocol": "http"}},
	}); err != nil {
		t.Fatal(err)
	}
	mark = len(wire)
	if status, err := h3.pveLBApply(ctx, es3, proj, "lb-abc"); err != nil || status != lbStatusActive || !slices.Equal(events(mark), steps("301", c3.File)) {
		t.Fatalf("one VM with a certificate: %s %v, wrote\n%s", status, err, strings.Join(events(mark), "\n"))
	}
	file3, err := h.lbCertOpen(c3)
	if err != nil {
		t.Fatal(err)
	}
	quiet := func(what string) {
		t.Helper()
		mark := len(wire)
		one1.mu.Lock()
		execs, asked := one1.execs, one1.certReads
		one1.mu.Unlock()
		if status, err := h3.pveLBApply(ctx, es3, proj, "lb-abc"); err != nil || status != lbStatusActive || len(events(mark)) != 0 {
			t.Fatalf("one VM, %s: %s %v, wrote %v", what, status, err, events(mark))
		}
		one1.mu.Lock()
		defer one1.mu.Unlock()
		if one1.execs != execs+1 || one1.certReads != asked+1 {
			t.Errorf("one VM, %s: %d commands, %d of them asked for the certificate files; want 1 and 1", what, one1.execs-execs, one1.certReads-asked)
		}
	}
	quiet("a quiet pass")
	for what, damage := range map[string]func(g *lbGuest){
		"a truncated file":     func(g *lbGuest) { g.disk[certPath(c3.File)] = "" },
		"a deleted file":       func(g *lbGuest) { delete(g.disk, certPath(c3.File)) },
		"a file anybody reads": func(g *lbGuest) { g.certMode = map[string]string{certPath(c3.File): "644:root:root"} },
	} {
		one1.set("301", damage)
		reloads := one1.count("301 haproxy reload")
		mark = len(wire)
		status, err := h3.pveLBApply(ctx, es3, proj, "lb-abc")
		cr3, _ := h3.cloud.FindByServiceIDAndExternalID(ctx, "svc3", "lb-abc")
		got, _ := lbOfRow(cr3)
		if err != nil || status != lbStatusActive || got.StatusMessage != "" || !slices.Equal(events(mark), steps("301", c3.File)) || one1.count("301 haproxy reload") != reloads+1 {
			t.Fatalf("one VM, %s: %s %v %q, wrote\n%s", what, status, err, got.StatusMessage, strings.Join(events(mark), "\n"))
		}
		one1.set("301", func(g *lbGuest) {
			if g.disk[certPath(c3.File)] != file3.PEM || len(g.certMode) != 0 || !g.serving() {
				t.Errorf("one VM, %s: the file after the pass is not the certificate's, or not root's alone", what)
			}
		})
		quiet("the pass after " + what)
	}
	logMu.Lock()
	if strings.Contains(logs.String(), lbHash(file3.PEM)) {
		t.Error("a log line holds the digest of a certificate file")
	}
	logMu.Unlock()

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
