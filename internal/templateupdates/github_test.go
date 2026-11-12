package templateupdates

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testPAT = "ghp_TESTSECRETdonotleak0123456789abcdef"

func TestNewClientRefusesEmptyToken(t *testing.T) {
	if _, err := NewClient("  ", Options{}); !errors.Is(err, ErrNoToken) {
		t.Fatalf("empty token: %v", err)
	}
	if _, err := NewClient("a b", Options{}); err == nil {
		t.Fatal("malformed token accepted")
	}
}

func TestValidateSendsBearerAndChecksScopes(t *testing.T) {
	var gotAuth, gotVersion string
	scopes := "repo, read:packages"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotVersion = r.Header.Get("Authorization"), r.Header.Get("X-GitHub-Api-Version")
		if r.URL.Path != "/user" {
			http.NotFound(w, r)
			return
		}
		if scopes != "-" {
			w.Header().Set("X-OAuth-Scopes", scopes)
		}
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Used", "1")
		w.Header().Set("X-RateLimit-Reset", "1800000000")
		fmt.Fprint(w, `{"login":"octo"}`)
	}))
	defer srv.Close()
	c, _ := NewClient(testPAT, Options{APIBase: srv.URL})
	u, err := c.Validate(context.Background())
	if err != nil || u.Login != "octo" {
		t.Fatalf("validate: %+v %v", u, err)
	}
	if gotAuth != "Bearer "+testPAT || gotVersion != "2022-11-28" {
		t.Fatalf("auth header %q version %q", gotAuth, gotVersion)
	}
	if rl := c.LastRateLimit(); rl == nil || rl.Limit != 5000 || rl.Remaining != 4999 || rl.Reset.Unix() != 1800000000 {
		t.Fatalf("rate limit not parsed: %+v", rl)
	}
	scopes = "repo"
	if _, err := c.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "read:packages") {
		t.Fatalf("missing scope accepted: %v", err)
	}
	scopes = "-" // fine-grained / app token: no X-OAuth-Scopes header at all
	if _, err := c.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "classic") {
		t.Fatalf("fine-grained token accepted: %v", err)
	}
}

func TestRateLimitIsSurfaced(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		headers map[string]string
	}{
		{"primary", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Limit": "5000", "X-RateLimit-Reset": "1800000000"}},
		{"secondary", 403, map[string]string{"Retry-After": "60"}},
		{"429", 429, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			}))
			defer srv.Close()
			c, _ := NewClient(testPAT, Options{APIBase: srv.URL})
			_, err := c.ListVersions(context.Background(), "proxcloud-vm-debian-13")
			var rl *RateLimitError
			if !errors.As(err, &rl) {
				t.Fatalf("want RateLimitError, got %v", err)
			}
			if tc.name == "primary" && (rl.Remaining != 0 || rl.Reset.IsZero() || !strings.Contains(rl.Error(), "resets at")) {
				t.Fatalf("primary limit detail: %+v %q", rl, rl.Error())
			}
			if tc.name == "secondary" && rl.RetryAfter != time.Minute {
				t.Fatalf("retry-after: %v", rl.RetryAfter)
			}
			if strings.Contains(err.Error(), testPAT) {
				t.Fatal("token leaked into the error")
			}
		})
	}
}

func TestPlainForbiddenIsNotRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message":"Resource not accessible"}`)
	}))
	defer srv.Close()
	c, _ := NewClient(testPAT, Options{APIBase: srv.URL})
	_, err := c.ListVersions(context.Background(), "proxcloud-vm-debian-13")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 403 || !strings.Contains(he.Message, "read:packages") {
		t.Fatalf("want HTTPError 403, got %v", err)
	}
}

func TestListVersionsPaginates(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/orgs/mutex-solutions/packages/container/proxcloud-vm-debian-13/versions" || r.URL.Query().Get("per_page") != "100" {
			http.Error(w, "bad path "+r.URL.String(), 400)
			return
		}
		page := r.URL.Query().Get("page")
		switch page {
		case "1":
			// A hostile next link pointing elsewhere must not be followed (we build our own URL).
			w.Header().Set("Link", `<https://evil.example/steal?page=2>; rel="next", <x>; rel="last"`)
			fmt.Fprint(w, `[{"id":1,"name":"sha256:`+strings.Repeat("a", 64)+`","created_at":"2026-11-10T00:00:00Z","metadata":{"package_type":"container","container":{"tags":["debian-13-os-20261110000000-aaaaaaaaaaaa"]}}}]`)
		case "2":
			w.Header().Set("Link", `<x>; rel="prev"`)
			fmt.Fprint(w, `[{"id":2,"name":"sha256:`+strings.Repeat("b", 64)+`","created_at":"2026-11-09T00:00:00Z","metadata":{"package_type":"container","container":{"tags":[]}}}]`)
		default:
			http.Error(w, "unexpected page", 400)
		}
	}))
	defer srv.Close()
	c, _ := NewClient(testPAT, Options{APIBase: srv.URL})
	vs, err := c.ListVersions(context.Background(), "proxcloud-vm-debian-13")
	if err != nil || len(vs) != 2 || calls.Load() != 2 {
		t.Fatalf("versions %d calls %d err %v", len(vs), calls.Load(), err)
	}
	if vs[0].Metadata.Container.Tags[0] != "debian-13-os-20261110000000-aaaaaaaaaaaa" {
		t.Fatalf("tags not decoded: %+v", vs[0])
	}
	if _, err := c.ListVersions(context.Background(), "../etc"); err == nil {
		t.Fatal("invalid package name accepted")
	}
}

// TestTokenNeverLogged drives every client path with a debug logger capturing everything and
// checks the PAT never appears in logs, errors or (for the registry) any non-registry request.
func TestTokenNeverLogged(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("authorization forwarded to blob storage: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, "blob")
	}))
	defer storage.Close()
	var regHost string
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"token":"regtok"}`)
		case r.Header.Get("Authorization") != "Bearer regtok":
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://`+regHost+`/token",service="ghcr.io",scope="repository:mutex-solutions/proxcloud-vm-debian-13:pull"`)
			w.WriteHeader(401)
		case strings.Contains(r.URL.Path, "/blobs/"):
			http.Redirect(w, r, storage.URL+"/signed?sig=x", http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"nope"}]}`)
		}
	}))
	defer reg.Close()
	regHost = strings.TrimPrefix(reg.URL, "https://")
	// The TLS test servers share one client that trusts both.
	hc := reg.Client()
	hc.Transport.(*http.Transport).TLSClientConfig.RootCAs.AddCert(storage.Certificate())
	c, _ := NewClient(testPAT, Options{APIBase: reg.URL, Registry: regHost, HTTP: hc})
	slog.Debug("client built", "client", c, "fmt", fmt.Sprintf("%+v", c)) // even a careless %+v of the client

	_, _, err1 := c.GetManifest(context.Background(), "proxcloud-vm-debian-13", "sha256:"+strings.Repeat("c", 64))
	rc, err2 := c.OpenBlob(context.Background(), "proxcloud-vm-debian-13", "sha256:"+strings.Repeat("d", 64))
	if err2 == nil {
		b, _ := io.ReadAll(rc)
		rc.Close()
		if string(b) != "blob" {
			t.Fatalf("blob via redirect: %q", b)
		}
	}
	_, err3 := c.Validate(context.Background())
	all := logs.String()
	for _, e := range []error{err1, err2, err3} {
		if e != nil {
			all += e.Error()
		}
	}
	if err2 != nil {
		t.Fatalf("blob: %v", err2)
	}
	if strings.Contains(all, testPAT) {
		t.Fatalf("PAT leaked: %s", all)
	}
}

func TestStringerHidesToken(t *testing.T) {
	c, _ := NewClient(testPAT, Options{})
	if s := fmt.Sprintf("%v %+v %#v", c, c, c); strings.Contains(s, testPAT) {
		t.Fatalf("client formatting leaks the token: %s", s)
	}
}
