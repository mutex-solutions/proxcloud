//go:build integration

package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/powerdns"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/auth"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"golang.org/x/net/dns/dnsmessage"
)

// Real HTTP handler + actual PostgreSQL/advisory locks + actual TCP DNS packets.
// The proof publisher is independent of PowerDNS: a zone cannot vouch for itself.
type dnsFixture struct {
	*suspendedFixture
	server     *httptest.Server
	mu         sync.Mutex
	zones      map[string]powerdns.Zone
	txt        map[string][]string
	alias      bool
	failDNS    bool
	failCreate bool
	posts      int
	dnsCalls   atomic.Int32
	apiCalls   atomic.Int32
	resolver   string
}

func newDNSFixture(t *testing.T) *dnsFixture {
	t.Helper()
	f := &dnsFixture{suspendedFixture: newSuspendedFixture(t), zones: map[string]powerdns.Zone{}, txt: map[string][]string{}}
	f.seed(t, pgdoc.M{"status": project.StatusEnabled})
	f.h.SetDNSOwnership(f.db.C("dns_ownership"))
	if err := f.db.C("dns_ownership").Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.resolver = l.Addr().String()
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.answerDNS(c)
		}
	}()
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("X-API-Key") != "test-key" {
			w.WriteHeader(401)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/servers/localhost/zones")
		name = strings.TrimPrefix(name, "/")
		switch {
		case r.Method == "GET" && name == "":
			out := []powerdns.Zone{}
			for _, z := range f.zones {
				out = append(out, z)
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "POST":
			f.posts++
			if f.failCreate {
				w.WriteHeader(503)
				return
			}
			var in powerdns.Zone
			_ = json.NewDecoder(r.Body).Decode(&in)
			if _, ok := f.zones[in.Name]; ok {
				w.WriteHeader(409)
				return
			}
			in.ID = in.Name
			f.zones[in.Name] = in
			_ = json.NewEncoder(w).Encode(in)
		case r.Method == "DELETE":
			delete(f.zones, name)
			w.WriteHeader(204)
		case r.Method == "GET":
			z, ok := f.zones[name]
			if !ok {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(z)
		case r.Method == "PATCH":
			w.WriteHeader(204)
		default:
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(f.server.Close)
	for _, id := range []string{"dns", "dns2"} {
		if err := f.db.C("externalService").Upsert(context.Background(), id, pgdoc.M{"name": id, "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{"provider": "powerdns", "apiUrl": f.server.URL, "region": "r1", "nameservers": []string{"ns.example.net"}, "ownershipResolvers": []string{f.resolver}}, "secret": pgdoc.M{"apiKey": "test-key"}}); err != nil {
			t.Fatal(err)
		}
	}
	// Both providers intentionally alias one authoritative server: global create
	// serialization must not be keyed only by provider ID or exact zone name.
	_, err = f.db.C("project").SetFields(context.Background(), pgdoc.M{"_id": "p1"}, pgdoc.M{"services": []any{pgdoc.M{"serviceId": "dns", "provider": "powerdns", "region": "r1"}, pgdoc.M{"serviceId": "dns2", "provider": "powerdns", "region": "r1"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *dnsFixture) answerDNS(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	var size [2]byte
	if _, err := io.ReadFull(c, size[:]); err != nil {
		return
	}
	wire := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(c, wire); err != nil {
		return
	}
	var q dnsmessage.Message
	if q.Unpack(wire) != nil || len(q.Questions) != 1 {
		return
	}
	f.dnsCalls.Add(1)
	f.mu.Lock()
	values := append([]string{}, f.txt[q.Questions[0].Name.String()]...)
	alias, fail := f.alias, f.failDNS
	f.mu.Unlock()
	q.Response = true
	q.RecursionAvailable = true
	if fail {
		q.RCode = dnsmessage.RCodeServerFailure
	}
	for _, v := range values {
		q.Answers = append(q.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.TXTResource{TXT: []string{v}}})
	}
	if alias {
		q.Answers = append(q.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET}, Body: &dnsmessage.CNAMEResource{CNAME: q.Questions[0].Name}})
	}
	wire, err := q.Pack()
	if err != nil {
		return
	}
	binary.BigEndian.PutUint16(size[:], uint16(len(wire)))
	_, _ = c.Write(append(size[:], wire...))
}

func (f *dnsFixture) request(sub, method, path string, body any) (int, string) {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
	w := httptest.NewRecorder()
	if sub == "" {
		// Use the real production authentication middleware for the no-credential
		// case; invoking an internal handler without RequestContext is not HTTP.
		auth.New(slog.Default()).Enforce(f.router).ServeHTTP(w, req)
	} else {
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		f.router.ServeHTTP(w, req)
	}
	return w.Code, w.Body.String()
}
func dataMap(t *testing.T, code int, body string) map[string]any {
	t.Helper()
	if code != 200 {
		t.Fatalf("HTTP %d: %s", code, body)
	}
	var e struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatal(err)
	}
	return e.Data
}
func (f *dnsFixture) challenge(t *testing.T, pid, svc, name string) map[string]any {
	t.Helper()
	code, body := f.request(suspendedSub, "POST", "/project/"+pid+"/dns-ownership", map[string]string{"serviceId": svc, "name": name})
	return dataMap(t, code, body)
}
func (f *dnsFixture) publish(d map[string]any) {
	f.mu.Lock()
	f.txt[d["recordName"].(string)] = append(f.txt[d["recordName"].(string)], d["recordValue"].(string))
	f.mu.Unlock()
}
func (f *dnsFixture) verify(t *testing.T, pid, svc string, d map[string]any) map[string]any {
	t.Helper()
	code, body := f.request(suspendedSub, "POST", "/project/"+pid+"/dns-ownership/"+d["id"].(string)+"/verify", map[string]string{"serviceId": svc})
	return dataMap(t, code, body)
}
func (f *dnsFixture) create(pid, svc, name, id string) (int, string) {
	return f.request(suspendedSub, "POST", "/project/"+pid+"/cloud?serviceId="+svc, map[string]any{"type": "DNS_ZONE", "data": map[string]any{"name": name, "ownershipId": id}})
}
func (f *dnsFixture) resetBackoff(t *testing.T, d map[string]any) {
	t.Helper()
	_, err := f.db.C("dns_ownership").SetFields(context.Background(), pgdoc.M{"_id": d["id"]}, pgdoc.M{"nextVerifyAt": time.Now().Add(-time.Minute)}, nil)
	if err != nil {
		t.Fatal(err)
	}
}
func (f *dnsFixture) addProject(t *testing.T, id string) {
	t.Helper()
	mustInsert(t, f.db, "project", pgdoc.M{"_id": id, "name": id, "organizationId": "o1", "status": project.StatusEnabled, "memberships": []any{pgdoc.M{"sub": suspendedSub, "role": project.RoleOwner}}, "services": []any{pgdoc.M{"serviceId": "dns", "region": "r1"}, pgdoc.M{"serviceId": "dns2", "region": "r1"}}})
}

func TestDNSOwnershipHTTPGate(t *testing.T) {
	f := newDNSFixture(t)
	// No authentication or proof-looking booleans can trigger any upstream create.
	for _, tc := range []struct {
		sub, path string
		body      any
	}{{"", "/project/p1/dns-ownership", map[string]string{"serviceId": "dns", "name": "example.org"}}, {"", "/project/p1/cloud?serviceId=dns", map[string]any{"type": "DNS_ZONE", "data": map[string]any{"name": "example.org", "verified": true}}}} {
		if code, body := f.request(tc.sub, "POST", tc.path, tc.body); code == 200 {
			t.Fatalf("unauth accepted %s", body)
		}
	}
	if code, _ := f.create("p1", "dns", "example.org", ""); code != 409 {
		t.Fatal(code)
	}
	for _, name := range []string{"co.uk", "github.io", "x.internal", "example.org/path", "a..example.org"} {
		if code, _ := f.request(suspendedSub, "POST", "/project/p1/dns-ownership", map[string]string{"serviceId": "dns", "name": name}); code != 409 {
			t.Fatalf("accepted %s: %d", name, code)
		}
	}
	d := f.challenge(t, "p1", "dns", "BÜCHER.de")
	if d["name"] != "xn--bcher-kva.de." {
		t.Fatal(d)
	}
	id := d["id"].(string)
	if code, _ := f.create("p1", "dns", "bücher.de", id); code != 409 {
		t.Fatal(code)
	}
	var stored map[string]any
	_, err := f.db.C("dns_ownership").Get(context.Background(), id, &stored)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(stored)
	if strings.Contains(string(b), d["recordValue"].(string)) || len(stored["tokenHash"].(string)) != 64 {
		t.Fatal("plaintext token persisted")
	}
	code, body := f.request(suspendedSub, "GET", "/project/p1/dns-ownership/"+id+"?serviceId=dns", nil)
	status := dataMap(t, code, body)
	if status["recordValue"] != nil || strings.Contains(body, "tokenHash") {
		t.Fatal(body)
	}
	f.publish(d)
	verified := f.verify(t, "p1", "dns", d)
	if verified["state"] != "VERIFIED" {
		t.Fatal(verified)
	}
	// Scope cannot move between attached providers, projects, or canonical domains.
	f.addProject(t, "p2")
	for _, tc := range []struct{ p, s, n string }{{"p2", "dns", "bücher.de"}, {"p1", "dns2", "bücher.de"}, {"p1", "dns", "other.org"}, {"p1", "foreign", "bücher.de"}} {
		if code, body := f.create(tc.p, tc.s, tc.n, id); code == 200 {
			t.Fatal(body)
		}
	}
	if code, body := f.create("p1", "dns", "XN--BCHER-KVA.DE.", id); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := f.create("p1", "dns", "bücher.de", id); code == 200 {
		t.Fatal("replay created")
	}
	f.mu.Lock()
	delete(f.zones, "xn--bcher-kva.de.")
	f.mu.Unlock() // even after deletion a consumed proof cannot replay
	if code, _ := f.create("p1", "dns", "bücher.de", id); code != 409 {
		t.Fatal(code)
	}
	f.mu.Lock()
	posts := f.posts
	f.mu.Unlock()
	if posts != 1 {
		t.Fatalf("upstream creates=%d", posts)
	}
}

func TestDNSOwnershipLifecycleFailureClosed(t *testing.T) {
	f := newDNSFixture(t)
	d := f.challenge(t, "p1", "dns", "proof.example.org")
	f.publish(d)
	id := d["id"].(string)
	// A tenant cannot inject a resolver, server URL or token into verification.
	if code, _ := f.request(suspendedSub, "POST", "/project/p1/dns-ownership/"+id+"/verify", map[string]string{"serviceId": "dns", "resolver": "127.0.0.1:80"}); code != 400 {
		t.Fatal(code)
	}
	f.mu.Lock()
	f.alias = true
	f.mu.Unlock()
	if code, _ := f.request(suspendedSub, "POST", "/project/p1/dns-ownership/"+id+"/verify", map[string]string{"serviceId": "dns"}); code != 502 {
		t.Fatal(code)
	}
	calls := f.dnsCalls.Load()
	if code, _ := f.request(suspendedSub, "POST", "/project/p1/dns-ownership/"+id+"/verify", map[string]string{"serviceId": "dns"}); code != 429 || f.dnsCalls.Load() != calls {
		t.Fatal("backoff bypass", code)
	}
	f.resetBackoff(t, d)
	f.mu.Lock()
	f.alias = false
	f.failDNS = true
	f.mu.Unlock()
	if code, _ := f.request(suspendedSub, "POST", "/project/p1/dns-ownership/"+id+"/verify", map[string]string{"serviceId": "dns"}); code != 502 {
		t.Fatal(code)
	}
	f.resetBackoff(t, d)
	f.mu.Lock()
	f.failDNS = false
	f.txt[d["recordName"].(string)] = []string{"wrong-exact-token"}
	f.mu.Unlock()
	if code, _ := f.request(suspendedSub, "POST", "/project/p1/dns-ownership/"+id+"/verify", map[string]string{"serviceId": "dns"}); code != 409 {
		t.Fatal(code)
	}
	f.resetBackoff(t, d)
	f.publish(d)
	_, err := f.db.C("dns_ownership").SetFields(context.Background(), pgdoc.M{"_id": id}, pgdoc.M{"expiresAt": time.Now().Add(-time.Second)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := f.request(suspendedSub, "POST", "/project/p1/dns-ownership/"+id+"/verify", map[string]string{"serviceId": "dns"}); code != 409 {
		t.Fatal(code)
	}
	d = f.challenge(t, "p1", "dns", "cancel.example.org")
	f.publish(d)
	if code, _ := f.request(suspendedSub, "DELETE", "/project/p1/dns-ownership/"+d["id"].(string)+"?serviceId=dns", nil); code != 200 {
		t.Fatal(code)
	}
	if code, _ := f.request(suspendedSub, "POST", "/project/p1/dns-ownership/"+d["id"].(string)+"/verify", map[string]string{"serviceId": "dns"}); code != 409 {
		t.Fatal(code)
	}
	d = f.challenge(t, "p1", "dns", "expired.example.org")
	f.publish(d)
	f.verify(t, "p1", "dns", d)
	_, err = f.db.C("dns_ownership").SetFields(context.Background(), pgdoc.M{"_id": d["id"]}, pgdoc.M{"verifiedUntil": time.Now().Add(-time.Second)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := f.create("p1", "dns", "expired.example.org", d["id"].(string)); code != 409 {
		t.Fatal(code)
	}
	d = f.challenge(t, "p1", "dns", "failure.example.org")
	f.publish(d)
	f.verify(t, "p1", "dns", d)
	f.mu.Lock()
	f.failCreate = true
	f.mu.Unlock()
	if code, _ := f.create("p1", "dns", "failure.example.org", d["id"].(string)); code != 502 {
		t.Fatal(code)
	}
	code, body := f.request(suspendedSub, "GET", "/project/p1/dns-ownership/"+d["id"].(string)+"?serviceId=dns", nil)
	if dataMap(t, code, body)["state"] != "CONSUMED" {
		t.Fatal(body)
	}
	if code, _ := f.create("p1", "dns", "failure.example.org", d["id"].(string)); code != 409 {
		t.Fatal("failed HTTP rolled back consume", code)
	}
}

func TestDNSPendingIsNonexclusiveAndCreatesSerialize(t *testing.T) {
	f := newDNSFixture(t)
	f.addProject(t, "p2")
	// A pending parent claim does not exclude another project's verified child.
	_ = f.challenge(t, "p1", "dns", "example.org")
	d := f.challenge(t, "p2", "dns2", "child.example.org")
	f.publish(d)
	f.verify(t, "p2", "dns2", d)
	if code, body := f.create("p2", "dns2", "child.example.org", d["id"].(string)); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	// Two valid simultaneous overlapping proofs, obtained before either zone exists.
	a := f.challenge(t, "p1", "dns", "race.other.org")
	b := f.challenge(t, "p2", "dns2", "sub.race.other.org")
	f.publish(a)
	f.publish(b)
	f.verify(t, "p1", "dns", a)
	f.verify(t, "p2", "dns2", b)
	results := make(chan int, 2)
	start := make(chan struct{})
	for _, x := range []struct {
		p, s, n string
		d       map[string]any
	}{{"p1", "dns", "race.other.org", a}, {"p2", "dns2", "sub.race.other.org", b}} {
		go func(x struct {
			p, s, n string
			d       map[string]any
		}) {
			<-start
			code, _ := f.create(x.p, x.s, x.n, x.d["id"].(string))
			results <- code
		}(x)
	}
	close(start)
	x, y := <-results, <-results
	if !((x == 200 && y == 409) || (y == 200 && x == 409)) {
		t.Fatalf("race statuses %d %d", x, y)
	}
	// No self-vouch: even a correctly published external TXT cannot authorize a
	// foreign overlap; the LIVE server guard is checked before a DNS query.
	d = f.challenge(t, "p1", "dns", "deep.child.example.org")
	f.publish(d)
	calls := f.dnsCalls.Load()
	if code, _ := f.request(suspendedSub, "POST", "/project/p1/dns-ownership/"+d["id"].(string)+"/verify", map[string]string{"serviceId": "dns"}); code != 409 || f.dnsCalls.Load() != calls {
		t.Fatal("foreign parent accepted / queried", code)
	}
}

func TestDNSOwnershipConcurrentVerificationAndQuota(t *testing.T) {
	f := newDNSFixture(t)
	// >8 project verifications exercise the bounded secondary lock pool. No
	// holder may reserve it while waiting for a nested global session lock.
	var wg sync.WaitGroup
	errs := make(chan string, 12)
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		pid := fmt.Sprintf("p%d", i+10)
		f.addProject(t, pid)
		d := f.challenge(t, pid, "dns", pid+".example.org")
		f.publish(d)
		wg.Add(1)
		go func(pid string, d map[string]any) {
			defer wg.Done()
			<-start
			code, body := f.request(suspendedSub, "POST", "/project/"+pid+"/dns-ownership/"+d["id"].(string)+"/verify", map[string]string{"serviceId": "dns"})
			if code != 200 {
				errs <- fmt.Sprintf("%d %s", code, body)
			}
		}(pid, d)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for i := 0; i < 10; i++ {
		f.challenge(t, "p1", "dns", fmt.Sprintf("limit%d.example.org", i))
	}
	if code, _ := f.request(suspendedSub, "POST", "/project/p1/dns-ownership", map[string]string{"serviceId": "dns", "name": "over.example.org"}); code != 429 {
		t.Fatal("live quota", code)
	}
	_, err := f.db.C("dns_ownership").SetFields(context.Background(), pgdoc.M{"projectId": "p1"}, pgdoc.M{"state": "CANCELLED"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 10; i < 20; i++ {
		d := f.challenge(t, "p1", "dns", fmt.Sprintf("limit%d.example.org", i))
		_, err = f.db.C("dns_ownership").SetFields(context.Background(), pgdoc.M{"_id": d["id"]}, pgdoc.M{"state": "CANCELLED"}, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if code, _ := f.request(suspendedSub, "POST", "/project/p1/dns-ownership", map[string]string{"serviceId": "dns", "name": "budget.example.org"}); code != 429 {
		t.Fatal("cancel reset hourly budget", code)
	}
}

func TestDNSOwnershipInvalidatedProofDoesNotReadUpstream(t *testing.T) {
	f := newDNSFixture(t)
	d := f.challenge(t, "p1", "dns", "binding.example.org")
	f.publish(d)
	f.verify(t, "p1", "dns", d)
	_, err := f.db.C("externalService").SetFields(context.Background(), pgdoc.M{"_id": "dns"}, pgdoc.M{"config.nameservers": []string{"changed.example.net"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.apiCalls.Load()
	if code, body := f.create("p1", "dns", "binding.example.org", d["id"].(string)); code != 409 {
		t.Fatalf("%d %s", code, body)
	}
	if f.apiCalls.Load() != calls {
		t.Fatal("invalidated proof triggered upstream I/O")
	}
}

func TestDNSOwnershipProjectBusyDoesNotQueuePoolWaiters(t *testing.T) {
	f := newDNSFixture(t)
	f.addProject(t, "p2")
	d := f.challenge(t, "p1", "dns", "busy.example.org")
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.db.WithAdvisoryLock(context.Background(), "dns-ownership:project:p1", false, func() error { close(held); <-release; return nil })
		done <- err
	}()
	<-held
	var wg sync.WaitGroup
	errs := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := f.request(suspendedSub, "POST", "/project/p1/dns-ownership/"+d["id"].(string)+"/verify", map[string]string{"serviceId": "dns"})
			if code != 429 {
				errs <- fmt.Sprintf("%d %s", code, body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Holder is still blocked; another project's cheap challenge must progress.
	f.challenge(t, "p2", "dns", "notblocked.example.org")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
