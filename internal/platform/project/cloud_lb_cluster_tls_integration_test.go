//go:build integration

package project

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve/lbcerttest"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// TLS from a Service's Secret, from the controller's request to both appliances: the gateway's handler itself
// (lbgw.Handler, the requests as the controller sends them), the portal's side behind it, a real PostgreSQL and the
// pair's wire fake (which models the Proxmox API and what a guest answers, not HAProxy: TestLBApplianceTLS has the
// real one, with this mapping's golden among its configs). The controller's own side, which Secrets it reads and
// when it sends one, is cmd/lb-controller's TestTLSFromASecret.
//
// Shown: a hash the portal holds no certificate for is asked for and the Service waits without an address; the
// certificate that arrives is checked as an upload and stored sealed, bound to the cluster and its row, and one that
// was not asked for is not stored; the state holds hashes only; the appliances get the file named by the chain's
// fingerprint before the config, the standby first; a rotated Secret is a new file and one push, and the row of
// before goes once nothing names it; a Secret that is gone, or one the check refuses, leaves the Service exactly as
// it was served and says InvalidCertificate, and a refused one is not asked for again; a Service never served gets
// no address with such a Secret; no project route and no other cluster reaches a cluster's certificate; without the
// portal's key no certificate is taken; the release removes the rows; and no log line, audit event, stored state,
// balancer row or answer holds anything of a key, which is on the wire to Proxmox in the certificate file writes
// alone.
func testLBClusterTLSFromSecret(t *testing.T) {
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
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	before := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(before)

	// Every request that reaches Proxmox, as it was sent.
	type call struct{ vm, file, body string }
	var wire []call
	var wireMu sync.Mutex
	ext := lbClusterExternalID("c1")
	id := ext[len("lb-"):]
	f := newPairWire(t, pve.LBTag(id))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		parts := strings.Split(r.URL.Path, "/")
		c := call{file: r.Form.Get("file"), body: r.URL.RawQuery + "\n" + r.Form.Encode() + "\n" + strings.Join(r.Form["command"], " ") + "\n" + r.Form.Get("content")}
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
	for _, doc := range []pgdoc.M{
		{"_id": "svc", "type": "CLOUD", "config": pgdoc.M{"provider": "proxmox", "network": pgdoc.M{"publicNetworks": []any{
			pgdoc.M{"id": "vmbr1", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1", "ranges": []any{"203.0.113.10-203.0.113.20"}}}}}},
		{"_id": "k8s", "type": "CLOUD", "config": pgdoc.M{"provider": "kamaji", "cluster": pgdoc.M{"loadBalancer": pgdoc.M{"publicNetworkId": "vmbr1", "flavorId": "lb-small"}}}},
	} {
		if _, err := db.C("externalService").InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	flavors := pveflavor.New(db)
	if err := flavors.Save(ctx, &pveflavor.Flavor{Slug: "lb-small", Name: "lb-small", VCPUs: 1, RAMMb: 1024, DiskGb: 10, Active: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.C("project").InsertOne(ctx, pgdoc.M{"_id": "p1", "name": "p1", "status": StatusEnabled, "services": []any{}, "quota": pgdoc.M{"floatingIps": 4}}); err != nil {
		t.Fatal(err)
	}
	keys, err := textcrypt.NewBlobKeys("default-v1", map[string]string{"default-v1": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	store := lbgw.NewStore(db)
	h := &Handler{svc: NewService(NewRepo(db), nil, nil, nil, nil), cloud: cloud.NewRepo(db), sdnLock: lock.New(db), publicIPs: NewPublicIPRepo(db),
		esSvc: externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k")), pveFlavors: flavors, lbGW: store,
		audit:  audit.NewService(audit.NewRepo(db), logger),
		pveFor: func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil }}
	certStore := db.C("lb_certificates")
	h.SetLBCertificates(certStore, keys)
	_, secret, err := store.MintCredential(ctx, lbgw.Credential{ID: "c1", ProjectID: "p1", ComputeServiceID: "svc", KubernetesServiceID: "k8s", NetworkID: "net-1", Vnet: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	vnet := externalservice.PVEPublicNetwork{ID: "v1", CIDR: "10.0.0.0/24", Gateway: "10.0.0.1", Ranges: []string{"10.0.0.2-10.0.0.254"}}
	reserved, err := h.publicIPs.Reserve(ctx, "svc", "p1", kamajiReservation("c1"), vnet, kamajiWorkerAddrs+kamajiLBAddrs)
	if err != nil {
		t.Fatal(err)
	}
	nodes := []lbgw.Node{{Name: "w1", Address: reserved[0]}, {Name: "w2", Address: reserved[1]}}
	row := &cloud.CloudResource{ExternalID: ext, ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(row, &loadBalancer{ID: id, Name: "k8s-" + id, Status: lbStatusBuild, NetworkID: "v1", NetworkRef: "net-1",
		PublicNetworkID: "vmbr1", HA: true, FlavorID: "lb-small", ServerID: "301", PrivateIP: "10.0.0.31", PrivatePrefix: 24,
		Appliances: []lbAppliance{{ServerID: "301", PrivateIP: "10.0.0.31"}, {ServerID: "302", PrivateIP: "10.0.0.32"}},
		Cluster:    &lbCluster{ID: "c1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cloud.Insert(ctx, row); err != nil {
		t.Fatal(err)
	}

	// The gateway, with what it audits; every answer it gave is kept.
	var gwAudit []audit.AuditEvent
	gw := &lbgw.Handler{Reg: store, Portal: h, Audit: func(ev audit.AuditEvent) { gwAudit = append(gwAudit, ev) }, Log: logger}
	var answers []string
	put := func(req lbgw.Request) *lbgw.Answer {
		t.Helper()
		req.Nodes = nodes
		raw, _ := json.Marshal(req)
		r := httptest.NewRequest(http.MethodPut, lbgw.StatePath, bytes.NewReader(raw))
		r.Header.Set("Authorization", "PVEAPIToken="+lbgw.TokenID("c1")+"="+secret)
		r.Header.Set(lbgw.ProtocolHeader, "2")
		w := httptest.NewRecorder()
		gw.ServeHTTP(w, r)
		answers = append(answers, w.Body.String())
		if w.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", w.Code, w.Body)
		}
		var ans lbgw.Answer
		if err := json.Unmarshal(w.Body.Bytes(), &ans); err != nil {
			t.Fatal(err)
		}
		return &ans
	}
	read := func() *loadBalancer {
		t.Helper()
		cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", ext)
		if err != nil || cr == nil {
			t.Fatalf("row: %v", err)
		}
		lb, err := lbOfRow(cr)
		if err != nil {
			t.Fatal(err)
		}
		return lb
	}
	converge := func(what string, done func(*loadBalancer) bool) *loadBalancer {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if !lbClusterPassed(ctx, h, "c1") {
				continue // another pass is at work: the row may still be the one of before
			}
			if lb := read(); done(lb) {
				return lb
			}
		}
		lb := read()
		t.Fatalf("%s: %+v %+v %s\n%s", what, lb.Cluster, lb.Listeners, lb.StatusMessage, strings.Join(f.log, "\n"))
		return nil
	}
	certRows := func(owner string) []lbCertificate {
		t.Helper()
		var rows []lbCertificate
		if err := certStore.Find(ctx, pgdoc.M{"projectId": owner}, &rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	stored := func() *lbgw.State {
		t.Helper()
		st, err := store.State(ctx, "c1")
		if err != nil || st == nil {
			t.Fatalf("state: %v", err)
		}
		return st
	}
	certWrites := func(from int) (out []string) {
		wireMu.Lock()
		defer wireMu.Unlock()
		for _, c := range wire[from:] {
			if strings.HasPrefix(c.file, "/etc/haproxy/certs/") {
				out = append(out, c.vm+" "+c.file)
			}
		}
		return out
	}
	mark := func() int {
		wireMu.Lock()
		defer wireMu.Unlock()
		return len(wire)
	}
	status := func(ans *lbgw.Answer, uid string) lbgw.ServiceStatus {
		t.Helper()
		i := slices.IndexFunc(ans.Services, func(s lbgw.ServiceStatus) bool { return s.UID == uid })
		if i < 0 {
			t.Fatalf("no answer for %s: %+v", uid, ans.Services)
		}
		return ans.Services[i]
	}
	event := func(st lbgw.ServiceStatus, reason string) string {
		for _, ev := range st.Events {
			if ev.Reason == reason {
				return ev.Message
			}
		}
		return ""
	}

	until := time.Now().Add(72 * time.Hour)
	first := lbcerttest.New(t, lbcerttest.RSA(t, 2048), until, "shop.example.org")
	second := lbcerttest.New(t, lbcerttest.RSA(t, 2048), until, "shop.example.org")
	unasked := lbcerttest.New(t, lbcerttest.RSA(t, 2048), until, "other.example.org")
	keyLines := []string{strings.Split(first.Key, "\n")[2], strings.Split(second.Key, "\n")[2], strings.Split(unasked.Key, "\n")[2]}
	holdsKey := func(s string) bool {
		return strings.Contains(s, "PRIVATE KEY") || slices.ContainsFunc(keyLines, func(k string) bool { return strings.Contains(s, k) })
	}
	data := func(c lbcerttest.Cert) lbgw.CertificateData {
		return lbgw.CertificateData{Certificate: c.Chain, PrivateKey: c.Key}
	}
	hash := func(d lbgw.CertificateData) string {
		return lbgw.SecretHash([]byte(d.Certificate), []byte(d.PrivateKey))
	}
	hashA, hashB := hash(data(first)), hash(data(second))
	shop := func(refs ...lbgw.CertRef) lbgw.Service {
		return lbgw.Service{UID: "shop", Namespace: "web", Name: "shop", ExternalTrafficPolicy: "Cluster", Certificates: refs,
			Ports:       []lbgw.Port{{Protocol: "TCP", Port: 443, NodePort: 30443}, {Protocol: "TCP", Port: 80, NodePort: 30080}},
			Annotations: map[string]string{"proxcloud.io/lb-tls-secret": "shop-tls", "proxcloud.io/lb-tls-ports": "443"}}
	}
	plain := lbgw.Service{UID: "plain", Namespace: "web", Name: "plain", ExternalTrafficPolicy: "Cluster", Ports: []lbgw.Port{{Protocol: "TCP", Port: 80, NodePort: 30081}}}
	named := func(h string) lbgw.CertRef { return lbgw.CertRef{Secret: "shop-tls", SHA256: h} }

	// 1. A hash the portal holds nothing for: asked for, the Service waits out of the state and without an address.
	ans := put(lbgw.Request{Services: []lbgw.Service{shop(named(hashA)), plain}})
	if !slices.Equal(ans.NeedCertificates, []string{hashA}) || status(ans, "shop").State != lbgw.StateBuilding || status(ans, "shop").Address != "" ||
		len(stored().Services) != 1 || stored().Services[0].UID != "plain" || len(certRows(lbClusterCertOwner("c1"))) != 0 {
		t.Fatalf("the first request: %+v, state %+v", ans, stored().Services)
	}
	// 2. The Secret arrives, with one nobody asked for beside it: the first is stored sealed, the other not at all.
	ans = put(lbgw.Request{Services: []lbgw.Service{shop(named(hashA)), plain}, CertificateData: []lbgw.CertificateData{data(first), data(unasked)}})
	rows := certRows(lbClusterCertOwner("c1"))
	if len(ans.NeedCertificates) != 0 || len(rows) != 1 || rows[0].ID != lbClusterCertID("c1", hashA) || rows[0].SecretHash != hashA || rows[0].Name != "web/shop-tls" ||
		rows[0].KeyID != "default-v1" || rows[0].Refused != "" || rows[0].Subject == "" || len(rows[0].File) != 64 {
		t.Fatalf("after the Secret arrived: need %v, rows %+v", ans.NeedCertificates, rows)
	}
	if n, _ := certStore.Count(ctx, pgdoc.M{}); n != 1 {
		t.Fatalf("%d certificate rows: one that was not asked for was stored", n)
	}
	if st := stored(); len(st.Services) != 2 || !slices.Equal(st.Services[0].Certificates, []lbgw.CertRef{named(hashA)}) || !slices.Equal(st.CertHashes, []string{hashA}) {
		t.Fatalf("the state: %+v", st)
	}
	if file, err := h.lbCertOpen(&rows[0]); err != nil || file.Name != rows[0].File || !strings.Contains(file.PEM, keyLines[0]) {
		t.Fatalf("the row does not open: %v", err)
	}
	// Sealed to this cluster and this row: a blob moved to another cluster's row, or to a project, does not open.
	for _, owner := range []string{lbClusterCertOwner("c2"), "p1"} {
		moved := rows[0]
		moved.ProjectID = owner
		if _, err := h.lbCertOpen(&moved); err == nil {
			t.Errorf("the blob opened as %s's", owner)
		}
	}
	// 3. Both appliances get the file, then the config that names it; the standby first.
	lb := converge("the TLS Service ready", func(lb *loadBalancer) bool {
		return len(lb.Cluster.Services) == 2 && !slices.ContainsFunc(lb.Cluster.Services, func(s lbClusterSvc) bool { return s.State != lbgw.StateReady })
	})
	fileA := rows[0].File
	tlsListener := func(lb *loadBalancer) pve.LBListener {
		t.Helper()
		i := slices.IndexFunc(lb.Listeners, func(l pve.LBListener) bool { return l.Port == 443 })
		if i < 0 {
			t.Fatalf("no listener on 443: %+v", lb.Listeners)
		}
		return lb.Listeners[i]
	}
	if l := tlsListener(lb); l.Protocol != "https" || !slices.Equal(l.CertFiles, []string{fileA}) || !slices.Equal(l.Certificates, []string{rows[0].ID}) {
		t.Fatalf("the listener: %+v", l)
	}
	// What the consoles show next to the cluster: the certificate under the id its listener names, with the Secret it
	// came from and what it says in public. Nothing of the key, the sealed blob or the Secret's hash.
	view, err := h.lbClusterView(ctx, "c1")
	if err != nil || view == nil || len(view.Certificates) != 1 {
		t.Fatalf("view: %+v, %v", view, err)
	}
	if c := view.Certificates[0]; c.ID != rows[0].ID || c.Secret != rows[0].Name || c.Secret == "" || !c.NotAfter.Equal(rows[0].NotAfter) || c.NotAfter.IsZero() ||
		c.Fingerprint != rows[0].Fingerprint || c.Subject != rows[0].Subject {
		t.Fatalf("the view's certificate %+v, the row %+v", c, rows[0].LBCertificate)
	}
	if b, _ := json.Marshal(view); strings.Contains(string(b), hashA) || strings.Contains(string(b), rows[0].Sealed) || strings.Contains(string(b), "PRIVATE KEY") ||
		strings.Contains(string(b), "sealed") || strings.Contains(string(b), "secretHash") {
		t.Fatalf("the view carries more than a certificate's public part: %s", b)
	}
	holder := "301"
	if lb.Appliances[1].HoldsVIP {
		holder = "302"
	}
	writes := certWrites(0)
	if len(writes) != 2 || !strings.HasSuffix(writes[0], " /etc/haproxy/certs/"+fileA+".pem.new") || strings.HasPrefix(writes[0], holder+" ") || !strings.HasPrefix(writes[1], holder+" ") {
		t.Fatalf("certificate writes %v, the address is on %s", writes, holder)
	}
	// The config each appliance serves now (a pass for the Service without TLS may have written one before).
	if len(f.files) < 2 {
		t.Fatalf("%d configs written", len(f.files))
	}
	for _, config := range f.files[len(f.files)-2:] {
		if !strings.Contains(config, ":443 ssl crt /etc/haproxy/certs/"+fileA+".pem alpn h2,http/1.1\n") || strings.Contains(config, ":80 ssl") {
			t.Errorf("the config served:\n%s", config)
		}
	}
	for _, config := range f.files { // and no config ever had the port without TLS
		if strings.Contains(config, ":443\n") {
			t.Errorf("a config with port 443 and no TLS:\n%s", config)
		}
	}
	vip := status(put(lbgw.Request{Services: []lbgw.Service{shop(named(hashA)), plain}}), "shop").Address
	if vip == "" {
		t.Fatal("the Service has no address")
	}
	// A quiet turn writes nothing and asks for nothing.
	at := mark()
	if ans = put(lbgw.Request{Services: []lbgw.Service{shop(named(hashA)), plain}}); len(ans.NeedCertificates) != 0 || status(ans, "shop").State != lbgw.StateReady {
		t.Fatalf("a quiet turn: %+v", ans)
	}
	h.lbClusterConverge(ctx, "c1", true)
	if got := certWrites(at); len(got) != 0 {
		t.Fatalf("a quiet pass wrote %v", got)
	}

	// 4. The Secret is rotated: asked for while the Service stays served as it was, then one push of a new file.
	ans = put(lbgw.Request{Services: []lbgw.Service{shop(named(hashB)), plain}})
	if st := status(ans, "shop"); !slices.Equal(ans.NeedCertificates, []string{hashB}) || st.State != lbgw.StateReady || st.Address != vip || len(st.Events) != 0 ||
		!slices.Equal(stored().Services[0].Certificates, []lbgw.CertRef{named(hashA)}) {
		t.Fatalf("a rotation, before the new Secret arrived: %+v, state %+v", ans, stored().Services[0].Certificates)
	}
	at = mark()
	put(lbgw.Request{Services: []lbgw.Service{shop(named(hashB)), plain}, CertificateData: []lbgw.CertificateData{data(second)}})
	lb = converge("the rotated certificate served", func(lb *loadBalancer) bool {
		l := tlsListener(lb)
		return len(l.CertFiles) == 1 && l.CertFiles[0] != fileA && lb.Status == lbStatusActive && lb.AppliedHash == lbAppliedHash(lb)
	})
	fileB := tlsListener(lb).CertFiles[0]
	if got := certWrites(at); len(got) != 2 || !strings.HasSuffix(got[0], "/"+fileB+".pem.new") || status(put(lbgw.Request{Services: []lbgw.Service{shop(named(hashB)), plain}}), "shop").Address != vip {
		t.Fatalf("the rotation's writes: %v", got)
	}
	// The row of before stays while it is young (a pass that began before the new state must not lose it), and
	// goes with the first settled pass after.
	if rows = certRows(lbClusterCertOwner("c1")); len(rows) != 2 {
		t.Fatalf("%d rows after the rotation", len(rows))
	}
	old := time.Now().Add(-time.Hour).UTC()
	for _, r := range rows {
		if _, err := certStore.SetByID(ctx, r.ID, pgdoc.M{"createdAt": old}, nil); err != nil {
			t.Fatal(err)
		}
	}
	h.lbPasses.Delete("c1")
	converge("the row of before pruned", func(*loadBalancer) bool {
		rows = certRows(lbClusterCertOwner("c1"))
		return len(rows) == 1
	})
	if rows[0].SecretHash != hashB {
		t.Fatalf("the row that stayed: %+v", rows[0].SecretHash)
	}

	// 5. The Secret is gone (the controller says so): the Service stays exactly as it is served, and says why.
	at = mark()
	ans = put(lbgw.Request{Services: []lbgw.Service{shop(lbgw.CertRef{Secret: "shop-tls", Problem: lbgw.CertNotFound}), plain}})
	if st := status(ans, "shop"); st.State != lbgw.StateReady || st.Address != vip || !strings.Contains(event(st, lbgw.ReasonInvalidCertificate), "Secret web/shop-tls does not exist") ||
		!slices.Equal(stored().Services[0].Certificates, []lbgw.CertRef{named(hashB)}) || len(ans.NeedCertificates) != 0 {
		t.Fatalf("a Secret that is gone: %+v", ans)
	}
	h.lbClusterConverge(ctx, "c1", true)
	if got := certWrites(at); len(got) != 0 || !slices.Equal(tlsListener(read()).CertFiles, []string{fileB}) {
		t.Fatalf("a Secret that is gone changed the appliances: %v", got)
	}
	// The annotation alone, from a controller that sends no certificates: the same.
	if st := status(put(lbgw.Request{Services: []lbgw.Service{shop(), plain}}), "shop"); st.Address != vip || !strings.Contains(event(st, lbgw.ReasonInvalidCertificate), "upgrade the cluster") {
		t.Fatalf("the annotation without certificates: %+v", st)
	}
	// 6. A Secret whose key does not fit its certificate: asked for once, refused by the check of an upload,
	// remembered without its content, and not asked for again.
	bad := lbgw.CertificateData{Certificate: first.Chain, PrivateKey: second.Key}
	if ans = put(lbgw.Request{Services: []lbgw.Service{shop(named(hash(bad))), plain}}); !slices.Equal(ans.NeedCertificates, []string{hash(bad)}) {
		t.Fatalf("a new hash is not asked for: %+v", ans)
	}
	for i := 0; i < 2; i++ {
		req := lbgw.Request{Services: []lbgw.Service{shop(named(hash(bad))), plain}}
		if i == 0 {
			req.CertificateData = []lbgw.CertificateData{bad}
		}
		ans = put(req)
		if st := status(ans, "shop"); len(ans.NeedCertificates) != 0 || st.Address != vip || !strings.Contains(event(st, lbgw.ReasonInvalidCertificate), "The private key does not belong to the certificate") ||
			!slices.Equal(stored().Services[0].Certificates, []lbgw.CertRef{named(hashB)}) {
			t.Fatalf("a Secret the check refuses (request %d): %+v", i, ans)
		}
	}
	rows = certRows(lbClusterCertOwner("c1"))
	if i := slices.IndexFunc(rows, func(r lbCertificate) bool { return r.SecretHash == hash(bad) }); len(rows) != 2 || i < 0 || rows[i].Sealed != "" || rows[i].File != "" || rows[i].Refused == "" {
		t.Fatalf("the refused certificate's row: %+v", rows)
	}
	// 7. A Service the balancer never served, with a Secret that cannot be used: no address, and not in the state.
	fresh := lbgw.Service{UID: "fresh", Namespace: "web", Name: "fresh", ExternalTrafficPolicy: "Cluster", Ports: []lbgw.Port{{Protocol: "TCP", Port: 443, NodePort: 30444}},
		Annotations: map[string]string{"proxcloud.io/lb-tls-secret": "fresh-tls"}, Certificates: []lbgw.CertRef{{Secret: "fresh-tls", Problem: lbgw.CertWrongType}}}
	ans = put(lbgw.Request{Services: []lbgw.Service{shop(named(hashB)), plain, fresh}})
	if st := status(ans, "fresh"); st.State != lbgw.StateRefused || st.Reason != lbgw.ReasonInvalidCertificate || st.Address != "" || !strings.Contains(st.Message, "is not of type kubernetes.io/tls") ||
		slices.ContainsFunc(stored().Services, func(s lbgw.Service) bool { return s.UID == "fresh" }) {
		t.Fatalf("a Service never served with a Secret of another type: %+v", st)
	}
	// An annotation that is there and empty (a template that filled nothing in) marks ports meant for TLS: they are
	// not opened plain. A Service never served gets no address and one Event; one that is served stays as it was,
	// with its certificate, and says so. Both were plain Services with no Event.
	blank := lbgw.Service{UID: "blank", Namespace: "web", Name: "blank", ExternalTrafficPolicy: "Cluster", Ports: []lbgw.Port{{Protocol: "TCP", Port: 443, NodePort: 30445}},
		Annotations: map[string]string{"proxcloud.io/lb-tls-secret": ""}}
	emptied := shop()
	emptied.Annotations["proxcloud.io/lb-tls-secret"] = " "
	ans = put(lbgw.Request{Services: []lbgw.Service{emptied, plain, blank}})
	if st := status(ans, "blank"); st.State != lbgw.StateRefused || st.Reason != lbgw.ReasonInvalidCertificate || st.Address != "" || len(st.Events) != 0 ||
		!strings.Contains(st.Message, "proxcloud.io/lb-tls-secret is empty") || slices.ContainsFunc(stored().Services, func(s lbgw.Service) bool { return s.UID == "blank" }) {
		t.Fatalf("a Service never served with an empty TLS annotation: %+v", st)
	}
	if st := status(ans, "shop"); st.Address != vip || len(st.Events) != 1 || !strings.Contains(event(st, lbgw.ReasonInvalidCertificate), "proxcloud.io/lb-tls-secret is empty") ||
		!slices.Equal(stored().Services[0].Certificates, []lbgw.CertRef{named(hashB)}) || stored().Services[0].Annotations["proxcloud.io/lb-tls-secret"] != "shop-tls" {
		t.Fatalf("a served Service whose TLS annotation was emptied: %+v, stored %+v", st, stored().Services[0])
	}
	// A Service that is served plain and then gets the empty annotation (one v0.11.0 served without reading it, at
	// the upgrade) keeps its address and its plain port, and its Event names no certificate: it never had one.
	templated := plain
	templated.Annotations = map[string]string{"proxcloud.io/lb-tls-secret": ""}
	ans = put(lbgw.Request{Services: []lbgw.Service{shop(named(hashB)), templated}})
	was := stored().Services[1]
	if st, said := status(ans, plain.UID), event(status(ans, plain.UID), lbgw.ReasonInvalidCertificate); st.Address == "" || st.State == lbgw.StateRefused || len(st.Events) != 1 ||
		!strings.Contains(said, "proxcloud.io/lb-tls-secret is empty") || !strings.Contains(said, "as it did before this change") || strings.Contains(said, "certificate it had") ||
		was.UID != plain.UID || len(was.Annotations) != 0 || len(was.Certificates) != 0 {
		t.Fatalf("a Service served plain that got an empty TLS annotation: %+v, stored %+v", st, was)
	}
	ans = put(lbgw.Request{Services: []lbgw.Service{shop(named(hashB)), plain}})
	addresses, _ := h.publicIPs.ForServer(ctx, "svc", LBAddressOwner(id))
	if len(addresses) != 2 {
		t.Fatalf("%d addresses: a refused Service took one", len(addresses))
	}

	// 8. Nobody else reaches the cluster's certificate. A project's listener cannot name it, the project holds no
	// certificate, and another cluster that names the same Secret content is asked for it: it has a row of its own.
	spec := pve.LBSpec{Listeners: []pve.LBListener{{Port: 443, Protocol: "https", Pool: "web", Certificates: []string{lbClusterCertID("c1", hashB)}}}}
	if _, why, err := h.lbResolveCerts(ctx, "p1", &spec, true); err != nil || !strings.Contains(why, "not a certificate of this project") || len(certRows("p1")) != 0 {
		t.Fatalf("a project's listener naming the cluster's certificate: %q %v", why, err)
	}
	c2 := &lbgw.Credential{ID: "c2", ProjectID: "p1", ComputeServiceID: "svc", KubernetesServiceID: "k8s", NetworkID: "net-1", Vnet: "v1"}
	if kept, _, held, need, err := h.lbClusterTLS(ctx, c2, []lbgw.Service{shop(named(hashB))}, nil); err != nil || !slices.Equal(need, []string{hashB}) || len(kept) != 0 || held["shop"].State != lbgw.StateBuilding {
		t.Fatalf("another cluster naming the same Secret content: need %v, kept %v, %+v %v", need, kept, held, err)
	}
	// A certificate past its last day is served, which is its owner's choice, and the Service says so.
	expired := lbCertificate{ID: lbClusterCertID("c2", hashA), ProjectID: lbClusterCertOwner("c2"), SecretHash: hashA,
		LBCertificate: pve.LBCertificate{File: strings.Repeat("e", 64), NotAfter: time.Now().Add(-48 * time.Hour)}}
	if err := certStore.Upsert(ctx, expired.ID, &expired); err != nil {
		t.Fatal(err)
	}
	if kept, _, held, need, err := h.lbClusterTLS(ctx, c2, []lbgw.Service{shop(named(hashA))}, nil); err != nil || len(need) != 0 || len(kept) != 1 || held["shop"].State != "" ||
		!strings.Contains(event(held["shop"], lbgw.ReasonCertificateExpired), "expired on") {
		t.Fatalf("an expired certificate: kept %v, %+v %v", kept, held, err)
	}
	if _, err := certStore.DeleteOne(ctx, pgdoc.M{"_id": expired.ID}); err != nil {
		t.Fatal(err)
	}
	// 9. A portal without its key takes no certificate, asks for none, and gives such a Service no address.
	noKey := &Handler{svc: h.svc, cloud: h.cloud, sdnLock: h.sdnLock, publicIPs: h.publicIPs, esSvc: h.esSvc, pveFlavors: h.pveFlavors, lbGW: store, pveFor: h.pveFor}
	noKey.SetLBCertificates(certStore, nil)
	kept, _, held, need, err := noKey.lbClusterTLS(ctx, c2, []lbgw.Service{shop(named(hashA))}, []lbgw.CertificateData{data(first)})
	if st := held["shop"]; err != nil || len(need) != 0 || len(kept) != 0 || st.State != lbgw.StateRefused || st.Reason != lbgw.ReasonTLSNotAvailable || len(certRows(lbClusterCertOwner("c2"))) != 0 {
		t.Fatalf("a portal without its key: %+v, need %v, %v", st, need, err)
	}

	// 10. Nothing but the sealed blob and the appliances' certificate files holds a key.
	time.Sleep(500 * time.Millisecond) // the audit writes
	for _, collection := range []string{"lbGatewayState", "cloudResource", "auditEvent", "lb_certificates", "lbGatewayCredential"} {
		var docs []json.RawMessage
		if err := db.C(collection).Find(ctx, pgdoc.M{}, &docs); err != nil {
			t.Fatal(err)
		}
		if collection != "lb_certificates" && len(docs) == 0 {
			t.Errorf("%s is empty: the test read the wrong collection", collection)
		}
		for _, doc := range docs {
			if holdsKey(string(doc)) || strings.Contains(string(doc), "BEGIN CERTIFICATE") {
				t.Errorf("a row of %s holds a certificate or a key", collection)
			}
		}
	}
	var audited []audit.AuditEvent
	if err := db.C("auditEvent").Find(ctx, pgdoc.M{}, &audited); err != nil {
		t.Fatal(err)
	}
	taken := 0
	for _, ev := range audited {
		if ev.ResourceMetadata["field"] == "lbCertificate" && ev.Action == audit.ActionCreate && ev.ResourceMetadata["secret"] == "web/shop-tls" && ev.ResourceMetadata["fingerprint"] != "" {
			taken++
		}
		if raw, _ := json.Marshal(ev); strings.Contains(string(raw), hashA) || strings.Contains(string(raw), hashB) {
			t.Errorf("an audit event holds the hash of a Secret's content: %s", raw)
		}
	}
	if taken != 2 {
		t.Errorf("%d audit events of a certificate taken, want the two that were", taken)
	}
	gwAudited, _ := json.Marshal(gwAudit)
	logMu.Lock()
	logged := logs.String()
	logMu.Unlock()
	for where, text := range map[string]string{"the log": logged, "the gateway's audit": string(gwAudited), "an answer": strings.Join(answers, "\n")} {
		if holdsKey(text) || strings.Contains(text, "BEGIN CERTIFICATE") {
			t.Errorf("%s holds a certificate or a key", where)
		}
	}
	wireMu.Lock()
	for _, c := range wire {
		if holdsKey(c.body) && !strings.HasPrefix(c.file, "/etc/haproxy/certs/") {
			t.Errorf("a key on the wire outside a certificate file write: %s", c.file)
		}
	}
	wireMu.Unlock()

	// A cluster whose Secrets changed faster than its passes cleared up: the rows nothing names go when one more
	// certificate arrives, and the row of a Service that is held as it was served stays, though the request names
	// that Service with a Secret that is gone.
	c3 := &lbgw.Credential{ID: "c3", ProjectID: "p1", ComputeServiceID: "svc", KubernetesServiceID: "k8s", NetworkID: "net-1", Vnet: "v1"}
	if err := store.PutState(ctx, &lbgw.State{ID: "c3", Services: []lbgw.Service{shop(named(hashA))}, CertHashes: []string{hashA}}); err != nil {
		t.Fatal(err)
	}
	leftovers := []lbCertificate{{ID: lbClusterCertID("c3", hashA), ProjectID: lbClusterCertOwner("c3"), SecretHash: hashA,
		LBCertificate: pve.LBCertificate{File: strings.Repeat("a", 64), NotAfter: until}}}
	for i := range lbClusterCertRows {
		gone := fmt.Sprintf("%064x", i)
		leftovers = append(leftovers, lbCertificate{ID: lbClusterCertID("c3", gone), ProjectID: lbClusterCertOwner("c3"), SecretHash: gone})
	}
	for i := range leftovers {
		if err := certStore.Upsert(ctx, leftovers[i].ID, &leftovers[i]); err != nil {
			t.Fatal(err)
		}
	}
	api := lbgw.Service{UID: "api", Namespace: "web", Name: "api", ExternalTrafficPolicy: "Cluster", Certificates: []lbgw.CertRef{{Secret: "api-tls", SHA256: hashB}},
		Ports: []lbgw.Port{{Protocol: "TCP", Port: 8443, NodePort: 30843}}, Annotations: map[string]string{"proxcloud.io/lb-tls-secret": "api-tls"}}
	kept, _, held, _, err = h.lbClusterTLS(ctx, c3, []lbgw.Service{shop(lbgw.CertRef{Secret: "shop-tls", Problem: lbgw.CertNotFound}), api}, []lbgw.CertificateData{data(second)})
	var left []string
	for _, row := range certRows(lbClusterCertOwner("c3")) {
		left = append(left, row.SecretHash)
	}
	slices.Sort(left)
	want := []string{hashA, hashB}
	slices.Sort(want)
	if err != nil || len(kept) != 2 || !slices.Equal(kept[0].Certificates, []lbgw.CertRef{named(hashA)}) || event(held["shop"], lbgw.ReasonInvalidCertificate) == "" || !slices.Equal(left, want) {
		t.Fatalf("more rows than a cluster may leave: %d rows left, kept %+v, %+v %v", len(left), kept, held, err)
	}
	if err := h.lbDropState(ctx, "c3"); err != nil {
		t.Fatal(err)
	}

	// 11. The certificates go with the cluster: the last step of its release (LBReleaseCluster, whose appliances'
	// part TestLBClusterStateConverges runs) drops them with the state.
	if err := h.lbDropState(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if st, _ := store.State(ctx, "c1"); st != nil || len(certRows(lbClusterCertOwner("c1"))) != 0 {
		t.Fatalf("after the release: state %v, %d certificate rows", st != nil, len(certRows(lbClusterCertOwner("c1"))))
	}
}
