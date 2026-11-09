package sandboxproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

const dom = "sbx.test"

func TestParseTarget(t *testing.T) {
	tests := []struct {
		name, host string
		hdr        map[string]string
		id         string
		port       int
		err        error
	}{
		{"host", "8080-sbx-1a2b3c4d.sbx.test", nil, "sbx-1a2b3c4d", 8080, nil},
		{"host with port", "49983-sbx-1a2b3c4d.sbx.test:443", nil, "sbx-1a2b3c4d", 49983, nil},
		{"wrong domain", "8080-sbx-1a2b3c4d.other.test", nil, "", 0, errInvalidHost},
		{"no dash", "8080.sbx.test", nil, "", 0, errInvalidHost},
		{"bad port", "abc-sbx-1a2b3c4d.sbx.test", nil, "", 0, errInvalidPort},
		{"port zero", "0-sbx-1a2b3c4d.sbx.test", nil, "", 0, errInvalidPort},
		{"port too big", "65536-sbx-1a2b3c4d.sbx.test", nil, "", 0, errInvalidPort},
		{"bad id", "80-sbx-XYZ.sbx.test", nil, "", 0, errInvalidSandboxID},
		{"headers shared host", "sandbox.sbx.test", map[string]string{"E2b-Sandbox-Id": "sbx-1a2b3c4d", "E2b-Sandbox-Port": "3000"}, "sbx-1a2b3c4d", 3000, nil},
		{"headers localhost", "localhost:8080", map[string]string{"E2b-Sandbox-Id": "sbx-1a2b3c4d", "E2b-Sandbox-Port": "3000"}, "sbx-1a2b3c4d", 3000, nil},
		{"headers ip", "10.0.0.1", map[string]string{"E2b-Sandbox-Id": "sbx-1a2b3c4d", "E2b-Sandbox-Port": "3000"}, "sbx-1a2b3c4d", 3000, nil},
		{"headers ignored on sandbox host", "80-sbx-aaaaaaaa.sbx.test", map[string]string{"E2b-Sandbox-Id": "sbx-1a2b3c4d", "E2b-Sandbox-Port": "3000"}, "sbx-aaaaaaaa", 80, nil},
		{"missing port header", "localhost", map[string]string{"E2b-Sandbox-Id": "sbx-1a2b3c4d"}, "", 0, errMissingHeader},
		{"headers bad id", "localhost", map[string]string{"E2b-Sandbox-Id": "nope", "E2b-Sandbox-Port": "3000"}, "", 0, errInvalidSandboxID},
		{"unrelated host", "example.com", nil, "", 0, errInvalidHost},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.Host = tc.host
			for k, v := range tc.hdr {
				r.Header.Set(k, v)
			}
			id, port, err := parseTarget(r, dom)
			if !errors.Is(err, tc.err) || id != tc.id || port != tc.port {
				t.Fatalf("got (%q,%d,%v) want (%q,%d,%v)", id, port, err, tc.id, tc.port, tc.err)
			}
		})
	}
}

// start runs the proxy in front of a lookup that always resolves to 127.0.0.1.
func start(t *testing.T, tok string) *httptest.Server {
	t.Helper()
	lk := func(_ context.Context, id string) (Pod, error) {
		if id == "sbx-00000000" {
			return Pod{}, ErrNotFound
		}
		return Pod{IP: "127.0.0.1", TrafficToken: tok}, nil
	}
	s := httptest.NewServer(Handler(dom, lk, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(s.Close)
	return s
}

func get(t *testing.T, s *httptest.Server, host, path string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", s.URL+path, nil)
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func upstream(t *testing.T, h http.Handler) (*httptest.Server, int) {
	t.Helper()
	u := httptest.NewServer(h)
	t.Cleanup(u.Close)
	pu, _ := url.Parse(u.URL)
	p, _ := strconv.Atoi(pu.Port())
	return u, p
}

func TestInternalRoutesBlockedBeforeLookup(t *testing.T) {
	called := false
	lk := func(context.Context, string) (Pod, error) { called = true; return Pod{}, ErrNotFound }
	s := httptest.NewServer(Handler(dom, lk, slog.Default()))
	defer s.Close()
	for _, p := range []string{"/init", "/freeze", "/unfreeze", "/collapse", "/fsfreeze", "/fsthaw", "//init", "/init/", "/./init", "/INIT", "/x/../init"} {
		code, body := get(t, s, "49983-sbx-1a2b3c4d."+dom, p, nil)
		if code != 404 || !strings.Contains(body, `"code":404`) || !strings.Contains(body, p) {
			t.Fatalf("%s: %d %s", p, code, body)
		}
	}
	if called {
		t.Fatal("lookup ran for a reserved path")
	}
	// other port / other path are not blocked (reach lookup -> 502 not found)
	if code, _ := get(t, s, "3000-sbx-1a2b3c4d."+dom, "/init", nil); code != 502 {
		t.Fatalf("non-envd port /init: %d", code)
	}
	if code, _ := get(t, s, "49983-sbx-1a2b3c4d."+dom, "/health", nil); code != 502 {
		t.Fatalf("envd /health: %d", code)
	}
}

func TestErrors(t *testing.T) {
	s := start(t, "")
	code, body := get(t, s, "8080-sbx-00000000."+dom, "/", nil)
	if code != 502 || !strings.Contains(body, "The sandbox was not found") || !strings.Contains(body, `"sandboxId":"sbx-00000000"`) {
		t.Fatalf("%d %s", code, body)
	}
	// closed port: grab a free port then close it
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	code, body = get(t, s, fmt.Sprintf("%d-sbx-1a2b3c4d.%s", port, dom), "/", nil)
	if code != 502 || !strings.Contains(body, "port is not open") || !strings.Contains(body, fmt.Sprintf(`"port":%d`, port)) {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := get(t, s, "example.com", "/", nil); code != 400 {
		t.Fatalf("bad host: %d", code)
	}
}

func TestTrafficToken(t *testing.T) {
	_, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	s := start(t, "secret")
	host := fmt.Sprintf("%d-sbx-1a2b3c4d.%s", port, dom)
	if code, body := get(t, s, host, "/", nil); code != 403 || !strings.Contains(body, "is missing") {
		t.Fatalf("missing: %d %s", code, body)
	}
	if code, body := get(t, s, host, "/", map[string]string{TrafficTokenHeader: "wrong"}); code != 403 || !strings.Contains(body, "is invalid") {
		t.Fatalf("invalid: %d %s", code, body)
	}
	if code, body := get(t, s, host, "/", map[string]string{TrafficTokenHeader: "secret"}); code != 200 || body != "ok" {
		t.Fatalf("valid: %d %s", code, body)
	}
}

func TestEnvdPortSkipsTokenAndKeepsHost(t *testing.T) {
	// envd port cannot be the httptest port, so verify the Host rewrite on a normal port instead.
	var gotHost string
	_, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotHost = r.Host }))
	s := start(t, "")
	host := fmt.Sprintf("%d-sbx-1a2b3c4d.%s", port, dom)
	get(t, s, host, "/", nil)
	if gotHost != host {
		t.Fatalf("host %q want %q", gotHost, host)
	}
}

func TestStreamingFlush(t *testing.T) {
	release := make(chan struct{})
	_, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "second\n")
	}))
	s := start(t, "")
	req, _ := http.NewRequest("GET", s.URL, nil)
	req.Host = fmt.Sprintf("%d-sbx-1a2b3c4d.%s", port, dom)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	br := bufio.NewReader(res.Body)
	line, err := br.ReadString('\n') // must arrive while upstream is still blocked
	if err != nil || line != "first\n" {
		t.Fatalf("got %q %v", line, err)
	}
	close(release)
	if line, _ = br.ReadString('\n'); line != "second\n" {
		t.Fatalf("got %q", line)
	}
}

func TestWebSocketUpgrade(t *testing.T) {
	_, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "no upgrade", 400)
			return
		}
		c, rw, _ := w.(http.Hijacker).Hijack()
		defer c.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		line, _ := rw.ReadString('\n')
		rw.WriteString("echo:" + line)
		rw.Flush()
	}))
	s := start(t, "")
	c, err := net.Dial("tcp", strings.TrimPrefix(s.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: %d-sbx-1a2b3c4d.%s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", port, dom)
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("status %q", status)
	}
	for {
		l, _ := br.ReadString('\n')
		if l == "\r\n" || l == "" {
			break
		}
	}
	fmt.Fprint(c, "hello\n")
	if got, _ := br.ReadString('\n'); got != "echo:hello\n" {
		t.Fatalf("got %q", got)
	}
}

func TestCache(t *testing.T) {
	calls := 0
	var next error
	lk := func(context.Context, string) (Pod, error) {
		calls++
		if next != nil {
			return Pod{}, next
		}
		return Pod{IP: "1.2.3.4"}, nil
	}
	c := NewCache(lk, 10*time.Second, 2*time.Second)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	next = ErrNotFound
	c.Lookup(ctx, "a")
	c.Lookup(ctx, "a")
	if calls != 1 {
		t.Fatalf("miss not cached: %d", calls)
	}
	now = now.Add(3 * time.Second) // miss TTL 2s expired
	next = nil
	if p, err := c.Lookup(ctx, "a"); err != nil || p.IP != "1.2.3.4" || calls != 2 {
		t.Fatalf("%v %v %d", p, err, calls)
	}
	now = now.Add(5 * time.Second) // within hit TTL 10s
	c.Lookup(ctx, "a")
	if calls != 2 {
		t.Fatalf("hit not cached: %d", calls)
	}
	now = now.Add(6 * time.Second)
	c.Lookup(ctx, "a")
	if calls != 3 {
		t.Fatalf("hit not expired: %d", calls)
	}
	// transient errors are not cached
	now = now.Add(time.Hour)
	next = errors.New("boom")
	c.Lookup(ctx, "b")
	c.Lookup(ctx, "b")
	if calls != 5 {
		t.Fatalf("error cached: %d", calls)
	}
}

type fakeLister struct{ items []map[string]any }

func (f fakeLister) ListPodsAllNamespaces(context.Context, string) ([]map[string]any, error) {
	return f.items, nil
}

func TestK8sLookup(t *testing.T) {
	pod := func(phase, ip string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{sandbox.AnnotationTrafficToken: "tk"}},
			"status": map[string]any{"phase": phase, "podIP": ip,
				"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
		}
	}
	notReady := pod("Running", "10.1.1.2")
	notReady["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": "False"}}
	deleting := pod("Running", "10.1.1.3")
	deleting["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-07T00:00:00Z"
	p, err := K8sLookup(fakeLister{[]map[string]any{pod("Pending", ""), pod("Running", ""), notReady, deleting, pod("Running", "10.1.1.1")}})(context.Background(), "x")
	if err != nil || p.IP != "10.1.1.1" || p.TrafficToken != "tk" {
		t.Fatalf("%v %v", p, err)
	}
	for _, only := range []map[string]any{pod("Pending", ""), notReady, deleting} {
		if _, err := K8sLookup(fakeLister{[]map[string]any{only}})(context.Background(), "x"); !errors.Is(err, ErrNotReady) {
			t.Fatalf("a booting or terminating pod must not get traffic (and must not be cached as missing): %v", err)
		}
	}
	if _, err := K8sLookup(fakeLister{nil})(context.Background(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no pod: %v", err)
	}
}
