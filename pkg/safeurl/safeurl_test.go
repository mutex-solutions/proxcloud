package safeurl

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestBlocked(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fd00::1", "fe80::1", "::ffff:10.0.0.1", "224.0.0.1"} {
		if !Blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if Blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func wantRefused(t *testing.T, err error, part string) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Msg, part) {
		t.Fatalf("want a refusal with %q, got %v", part, err)
	}
}

func TestCheckRefusals(t *testing.T) {
	ctx := context.Background()
	for raw, part := range map[string]string{
		"ftp://example.com/a":            "http or https",
		"file:///etc/passwd":             "http or https",
		"http://user:pw@example.com/a":   "no credentials",
		"http://":                        "http or https",
		"http://127.0.0.1/":              "internal",
		"http://localhost/":              "internal",
		"http://[::1]/":                  "internal",
		"http://[::ffff:10.0.0.1]/":      "internal",
		"http://10.0.0.1:8006/":          "internal",
		"http://169.254.169.254/latest/": "internal",
		"http://192.168.1.1/":            "internal",
		"http://100.64.0.1/":             "internal",
		"http://no-such-host.invalid/a":  "does not resolve",
		"http://2130706433/":             "",
	} {
		_, err := Check(ctx, raw, nil)
		if err == nil {
			t.Fatalf("%s must be refused", raw)
		}
		wantRefused(t, err, part)
	}
	// The operator's own networks count too.
	block := netip.MustParsePrefix("203.0.113.0/24")
	_, err := Check(ctx, "http://203.0.113.5/", block.Contains)
	wantRefused(t, err, "internal")
}

// Loopback stands in for the public internet here; 127.0.0.2 is the internal network.
func TestProbeRedirects(t *testing.T) {
	ctx := context.Background()
	inner := netip.MustParseAddr("127.0.0.2")
	blocked := func(ip netip.Addr) bool { return ip.Unmap() == inner }
	mux := http.NewServeMux()
	mux.HandleFunc("/img", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "7")
		_, _ = w.Write([]byte("qcowfmt"))
	})
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/to-img", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/img", http.StatusFound) })
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	var innerURL string
	mux.HandleFunc("/to-inner", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, innerURL, http.StatusFound) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	innerURL = strings.Replace(srv.URL, "127.0.0.1", "127.0.0.2", 1) + "/img"

	got, err := probe(ctx, srv.URL+"/to-img", blocked)
	if err != nil || got.URL != srv.URL+"/img" || got.Size != 7 {
		t.Fatalf("the final URL and its size: %+v %v", got, err)
	}
	_, err = probe(ctx, srv.URL+"/to-inner", blocked)
	wantRefused(t, err, "internal")
	_, err = probe(ctx, innerURL, blocked)
	wantRefused(t, err, "internal")
	_, err = probe(ctx, srv.URL+"/gone", blocked)
	wantRefused(t, err, "status 404")
	_, err = probe(ctx, srv.URL+"/loop", blocked)
	wantRefused(t, err, "too many")
}
