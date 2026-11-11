package safeurl

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// Everything not globally routable is blocked, in every form an address can take.
func TestBlocked(t *testing.T) {
	for _, tt := range []struct {
		ip      string
		blocked bool
	}{
		{"0.0.0.0", true}, {"0.1.2.3", true},
		{"10.1.2.3", true},
		{"100.64.0.1", true}, {"100.127.255.255", true}, {"100.128.0.1", false},
		{"127.0.0.1", true}, {"127.8.8.8", true},
		{"169.254.169.254", true},
		{"172.16.0.1", true}, {"172.31.255.255", true}, {"172.32.0.1", false},
		{"192.0.0.1", true}, {"192.0.1.1", false},
		{"192.0.2.1", true},
		{"192.31.196.1", true}, {"192.52.193.1", true}, {"192.88.99.1", true},
		{"192.168.1.1", true}, {"192.175.48.1", true},
		{"198.18.0.1", true}, {"198.19.255.255", true}, {"198.20.0.1", false},
		{"198.51.100.1", true}, {"203.0.113.1", true},
		{"224.0.0.1", true}, {"239.255.255.255", true},
		{"240.0.0.1", true}, {"255.255.255.255", true},
		{"1.1.1.1", false}, {"8.8.8.8", false}, {"223.255.255.255", false},
		{"::", true}, {"::1", true},
		{"::ffff:10.0.0.1", true}, {"::ffff:8.8.8.8", false},
		{"::10.0.0.1", true}, {"::8.8.8.8", true},
		{"64:ff9b::a00:1", true}, {"64:ff9b::808:808", true},
		{"100::1", true},
		{"2001::1", true}, {"2001:0:4136:e378:8000:63bf:3fff:fdd2", true}, {"2001:2::1", true}, {"2001:db8::1", true},
		{"2002:a00:1::", true}, {"2002:808:808::", true},
		{"3fff::1", true},
		{"fc00::1", true}, {"fd00::1", true}, {"fe80::1", true}, {"fe80::1%eth0", true}, {"fec0::1", true},
		{"ff02::1", true},
		{"2606:4700:4700::1111", false}, {"2001:4860:4860::8888", false}, {"2a00:1450:4001::1", false},
	} {
		if got := Blocked(netip.MustParseAddr(tt.ip)); got != tt.blocked {
			t.Errorf("%s: blocked = %v, want %v", tt.ip, got, tt.blocked)
		}
	}
	if !Blocked(netip.Addr{}) {
		t.Error("an invalid address must be blocked")
	}
}

func wantRefused(t *testing.T, err error, part string) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Msg, part) {
		t.Fatalf("want a refusal with %q, got %v", part, err)
	}
}

func TestOpenRefusals(t *testing.T) {
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
		_, err := Open(ctx, raw, nil)
		if err == nil {
			t.Fatalf("%s must be refused", raw)
		}
		wantRefused(t, err, part)
	}
	// The operator's own networks count too.
	_, err := Open(ctx, "http://8.8.8.8/", netip.MustParsePrefix("8.8.8.0/24").Contains)
	wantRefused(t, err, "internal")
}

// Loopback stands in for the public internet here; 127.0.0.2 is the internal network.
func TestOpenRedirects(t *testing.T) {
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
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("b"))
	})
	var innerURL string
	mux.HandleFunc("/to-inner", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, innerURL, http.StatusFound) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	innerURL = strings.Replace(srv.URL, "127.0.0.1", "127.0.0.2", 1) + "/img"

	got, err := open(ctx, srv.URL+"/to-img", blocked)
	if err != nil || got.Size != 7 {
		t.Fatalf("the size: %+v %v", got, err)
	}
	body, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if string(body) != "qcowfmt" {
		t.Fatalf("the body stays readable after open: %q", body)
	}
	if got, err = open(ctx, srv.URL+"/chunked", blocked); err != nil || got.Size != -1 {
		t.Fatalf("an unstated size: %+v %v", got, err)
	}
	got.Body.Close()
	_, err = open(ctx, srv.URL+"/to-inner", blocked)
	wantRefused(t, err, "internal")
	_, err = open(ctx, innerURL, blocked)
	wantRefused(t, err, "internal")
	_, err = open(ctx, srv.URL+"/gone", blocked)
	wantRefused(t, err, "status 404")
	_, err = open(ctx, srv.URL+"/loop", blocked)
	wantRefused(t, err, "too many")
}

// The check at connect time is what closes a name that answers differently the second time: the dialled address is
// refused whatever the first lookup said.
func TestOpenChecksTheDialledAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	calls := 0
	_, err := open(context.Background(), srv.URL, func(netip.Addr) bool { calls++; return calls > 1 })
	wantRefused(t, err, "internal")
	if calls < 2 {
		t.Fatalf("checked %d times", calls)
	}
}
