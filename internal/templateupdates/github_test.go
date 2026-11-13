package templateupdates

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// trickleRegistry serves one blob whose body arrives in chunks spaced by gap. Auth is anonymous
// (no challenge) so only the streaming behaviour is under test.
func trickleRegistry(t *testing.T, chunks int, gap time.Duration, stallAfter int) (*httptest.Server, []byte) {
	t.Helper()
	data := bytes.Repeat([]byte("z"), chunks*64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			if stallAfter > 0 && i == stallAfter {
				select { // stop sending, keep the connection open
				case <-r.Context().Done():
				case <-time.After(10 * time.Second):
				}
				return
			}
			_, _ = w.Write(data[i*64 : (i+1)*64])
			fl.Flush()
			time.Sleep(gap)
		}
	}))
	return srv, data
}

// BLOCKER regression: a blob body that keeps arriving for longer than the per-request timeout
// must stream to the end (the old client's Client.Timeout killed it).
func TestOpenBlobTricklesPastRequestTimeout(t *testing.T) {
	srv, data := trickleRegistry(t, 12, 50*time.Millisecond, 0) // ~600ms total
	defer srv.Close()
	c, _ := NewClient(testPAT, Options{Registry: strings.TrimPrefix(srv.URL, "http://"), RegistryScheme: "http",
		RequestTimeout: 150 * time.Millisecond, BlobIdleTimeout: 300 * time.Millisecond})
	rc, err := c.OpenBlob(context.Background(), "proxcloud-vm-debian-13", dg('1'))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	start := time.Now()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, err %v", len(got), err)
	}
	if time.Since(start) < 3*150*time.Millisecond {
		t.Fatalf("the body did not outlast the request timeout (%v): the test proves nothing", time.Since(start))
	}
}

// A stream that stops delivering bytes is aborted by the idle guard, not left hanging.
func TestOpenBlobStallIsAborted(t *testing.T) {
	srv, _ := trickleRegistry(t, 12, 10*time.Millisecond, 3)
	defer srv.Close()
	c, _ := NewClient(testPAT, Options{Registry: strings.TrimPrefix(srv.URL, "http://"), RegistryScheme: "http",
		RequestTimeout: time.Second, BlobIdleTimeout: 200 * time.Millisecond})
	rc, err := c.OpenBlob(context.Background(), "proxcloud-vm-debian-13", dg('1'))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	start := time.Now()
	_, err = io.ReadAll(rc)
	if !errors.Is(err, ErrBlobStalled) || time.Since(start) > 3*time.Second {
		t.Fatalf("want ErrBlobStalled quickly, got %v after %v", err, time.Since(start))
	}
}

// Small calls stay bounded by the request timeout (a hanging API must not hang discovery).
func TestRESTCallsStayBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	c, _ := NewClient(testPAT, Options{APIBase: srv.URL, RequestTimeout: 150 * time.Millisecond})
	start := time.Now()
	if _, err := c.ListVersions(context.Background(), "proxcloud-vm-debian-13"); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("hanging API: err %v after %v", err, time.Since(start))
	}
}

// realGHCRToken looks like what ghcr.io answers an anonymous pull-token request with (base64 of "v1:<opaque>").
var realGHCRToken = base64.StdEncoding.EncodeToString([]byte("v1:mZ8xQ2k4Rw7pLt0aNcVbYh5sJe3uIo9d"))

// fakeGHCR is a GHCR /token + tags endpoint. tokenBody is the /token answer; tokenStatus/tagStatus override 200.
type fakeGHCR struct {
	mu                     sync.Mutex
	tokenBody              string
	tokenStatus, tagStatus int
	auths, scopes          []string
	services, paths        []string
	tagBearer              string
}

func (f *fakeGHCR) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.URL.Path)
	switch {
	case r.URL.Path == "/token":
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.scopes = append(f.scopes, r.URL.Query()["scope"]...)
		f.services = append(f.services, r.URL.Query()["service"]...)
		if f.tokenStatus != 0 {
			w.WriteHeader(f.tokenStatus)
			_, _ = w.Write([]byte(`{"errors":[{"code":"DENIED","message":"requested access to the resource is denied"}]}`))
			return
		}
		_, _ = w.Write([]byte(f.tokenBody))
	case strings.HasPrefix(r.URL.Path, "/v2/") && strings.HasSuffix(r.URL.Path, "/tags/list"):
		f.tagBearer = r.Header.Get("Authorization")
		if f.tagStatus != 0 {
			w.WriteHeader(f.tagStatus)
			return
		}
		_, _ = w.Write([]byte(`{"name":"x","tags":["a"]}`))
	default:
		w.WriteHeader(404)
	}
}

func fakeGHCRClient(t *testing.T, f *fakeGHCR) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := NewClient(testPAT, Options{Registry: strings.TrimPrefix(srv.URL, "http://"), RegistryScheme: "http"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// PullToken: an ANONYMOUS request (no Authorization at all — never the PAT) with exactly the one package's pull scope and
// service ghcr.io; the token is confirmed with an anonymous-token read of that package and returned.
func TestPullTokenIsPackageScoped(t *testing.T) {
	f := &fakeGHCR{tokenBody: `{"token":"` + realGHCRToken + `"}`}
	c := fakeGHCRClient(t, f)
	tok, err := c.PullToken(context.Background(), "sandbox-base")
	if err != nil || tok != realGHCRToken {
		t.Fatalf("token %q %v", tok, err)
	}
	if len(f.auths) != 1 || f.auths[0] != "" {
		t.Fatalf("the token request must carry no Authorization header: %q", f.auths)
	}
	if len(f.scopes) != 1 || f.scopes[0] != "repository:mutex-solutions/sandbox-base:pull" || len(f.services) != 1 || f.services[0] != "ghcr.io" {
		t.Fatalf("scope=%q service=%q", f.scopes, f.services)
	}
	if f.tagBearer != "Bearer "+realGHCRToken || f.paths[1] != "/v2/mutex-solutions/sandbox-base/tags/list" {
		t.Fatalf("the anonymous token was not checked against the package: %q %v", f.tagBearer, f.paths)
	}
	for _, bad := range []string{"../evil", "proxcloud-vm-debian-13", "sandbox-base/../x", ""} {
		if _, err := c.PullToken(context.Background(), bad); err == nil {
			t.Fatalf("an invalid package %q was accepted", bad)
		}
	}
	if len(f.auths) != 1 {
		t.Fatal("an invalid package reached GHCR")
	}
}

// Every answer that is not a short-lived scoped registry token is refused with a fixed code, and neither the token nor the
// PAT appears in the error; a private package is ghcr_package_not_public (no PAT fallback: the PAT is never sent).
func TestPullTokenRefusesUnscopedOrPrivate(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for name, tc := range map[string]struct {
		f    *fakeGHCR
		code string
	}{
		"base64(PAT)":               {&fakeGHCR{tokenBody: `{"token":"` + b64(testPAT) + `"}`}, CodeTokenUnscoped},
		"raw-url base64(PAT)":       {&fakeGHCR{tokenBody: `{"token":"` + base64.RawURLEncoding.EncodeToString([]byte(testPAT)) + `"}`}, CodeTokenUnscoped},
		"base64(user:PAT)":          {&fakeGHCR{tokenBody: `{"token":"` + b64("proxcloud:"+testPAT) + `"}`}, CodeTokenUnscoped},
		"v1:base64(PAT)":            {&fakeGHCR{tokenBody: `{"token":"v1:` + b64(testPAT) + `"}`}, CodeTokenUnscoped},
		"base64(v1:PAT)":            {&fakeGHCR{tokenBody: `{"token":"` + b64("v1:"+testPAT) + `"}`}, CodeTokenUnscoped},
		"base64(xx+PAT) misaligned": {&fakeGHCR{tokenBody: `{"token":"` + b64("xx"+testPAT+"yy") + `"}`}, CodeTokenUnscoped},
		"the PAT":                   {&fakeGHCR{tokenBody: `{"token":"` + testPAT + `"}`}, CodeTokenUnscoped},
		"contains the PAT":          {&fakeGHCR{tokenBody: `{"token":"abc` + testPAT + `def"}`}, CodeTokenUnscoped},
		"another ghp_":              {&fakeGHCR{tokenBody: `{"token":"ghp_someOtherClassicToken00000000000000"}`}, CodeTokenUnscoped},
		"github_pat_":               {&fakeGHCR{tokenBody: `{"token":"github_pat_11AAAAAA0abcdefghijklmnop"}`}, CodeTokenUnscoped},
		"base64(ghp_)":              {&fakeGHCR{tokenBody: `{"token":"` + b64("ghp_someOtherClassicToken0000") + `"}`}, CodeTokenUnscoped},
		"PAT in access_token":       {&fakeGHCR{tokenBody: `{"token":"` + realGHCRToken + `","access_token":"` + b64(testPAT) + `"}`}, CodeTokenUnscoped},
		"expires_in 7200":           {&fakeGHCR{tokenBody: `{"token":"` + realGHCRToken + `","expires_in":7200}`}, CodeTokenUnscoped},
		"expires_in 0":              {&fakeGHCR{tokenBody: `{"token":"` + realGHCRToken + `","expires_in":0}`}, CodeTokenUnscoped},
		"expires_in negative":       {&fakeGHCR{tokenBody: `{"token":"` + realGHCRToken + `","expires_in":-5}`}, CodeTokenUnscoped},
		"expires_in not a number":   {&fakeGHCR{tokenBody: `{"token":"` + realGHCRToken + `","expires_in":"soon"}`}, CodeTokenUnscoped},
		"empty":                     {&fakeGHCR{tokenBody: `{"token":""}`}, CodeTokenUnscoped},
		"whitespace":                {&fakeGHCR{tokenBody: `{"token":"abc def"}`}, CodeTokenUnscoped},
		"non-ascii":                 {&fakeGHCR{tokenBody: `{"token":"abcé"}`}, CodeTokenUnscoped},
		"too long":                  {&fakeGHCR{tokenBody: `{"token":"` + strings.Repeat("a", 8<<10+1) + `"}`}, CodeTokenUnscoped},
		"malformed":                 {&fakeGHCR{tokenBody: `not json`}, CodeTokenUnscoped},
		"token 401":                 {&fakeGHCR{tokenStatus: 401}, CodePackageNotPublic},
		"token 403":                 {&fakeGHCR{tokenStatus: 403}, CodePackageNotPublic},
		"token 404":                 {&fakeGHCR{tokenStatus: 404}, CodePackageNotPublic},
		"manifest read 401":         {&fakeGHCR{tokenBody: `{"token":"` + realGHCRToken + `"}`, tagStatus: 401}, CodePackageNotPublic},
		"manifest read 403":         {&fakeGHCR{tokenBody: `{"token":"` + realGHCRToken + `"}`, tagStatus: 403}, CodePackageNotPublic},
	} {
		t.Run(name, func(t *testing.T) {
			f := tc.f
			c := fakeGHCRClient(t, f)
			tok, err := c.PullToken(context.Background(), "sandbox-base")
			var pe *Permanent
			if tok != "" || !errors.As(err, &pe) || pe.Code != tc.code {
				t.Fatalf("want %s, got %q %v", tc.code, tok, err)
			}
			if strings.Contains(err.Error(), testPAT) || strings.Contains(err.Error(), b64(testPAT)[:20]) || strings.Contains(err.Error(), realGHCRToken) {
				t.Fatalf("the error leaks a credential: %v", err)
			}
			for _, a := range f.auths {
				if a != "" {
					t.Fatalf("a token request carried Authorization %q", a)
				}
			}
			if len(f.auths) != 1 {
				t.Fatalf("expected exactly one anonymous token request (no fallback), got %d", len(f.auths))
			}
		})
	}
}

func TestPullTokenAcceptsShortExpiry(t *testing.T) {
	for _, body := range []string{`{"token":"` + realGHCRToken + `","expires_in":300}`, `{"token":"` + realGHCRToken + `","expires_in":3600}`, `{"access_token":"` + realGHCRToken + `"}`} {
		c := fakeGHCRClient(t, &fakeGHCR{tokenBody: body})
		if tok, err := c.PullToken(context.Background(), "sandbox-base"); err != nil || tok != realGHCRToken {
			t.Fatalf("%s: %q %v", body, tok, err)
		}
	}
}

// A transient GHCR failure (5xx, 429) is retryable, never permanent and never a PAT fallback.
func TestPullTokenTransientIsRetryable(t *testing.T) {
	for _, st := range []int{500, 502, 429} {
		f := &fakeGHCR{tokenStatus: st}
		c := fakeGHCRClient(t, f)
		_, err := c.PullToken(context.Background(), "sandbox-base")
		if err == nil || isPermanent(err) || f.auths[0] != "" {
			t.Fatalf("%d: %v", st, err)
		}
	}
}

func TestCheckPullTokenWithoutPAT(t *testing.T) {
	if err := CheckPullToken(realGHCRToken, ""); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "ghp_x", "a b", base64.StdEncoding.EncodeToString([]byte("github_pat_x")), "v1:" + base64.RawStdEncoding.EncodeToString([]byte("ghp_abc"))} {
		if err := CheckPullToken(bad, ""); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
