package sandboximport

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

const Registry = "registry.example.test/t"
const template = "0123456789abcdef0123"

var policy = Policy{TemplateRegistry: Registry, SourceRepositories: map[string][]string{"https://registry.example.test": {"migration/app"}, "https://registry.other.test": {"migration/app"}}, AllowAnonymous: true}

const project = "0123456789abcdef01234567"

var hash = "sha256:" + strings.Repeat("a", 64)

func TestSource(t *testing.T) {
	for _, s := range []string{Registry + "/" + project + "/" + template + "@" + hash, "registry.example.test/migration/app@" + hash, "registry.other.test/migration/app@" + hash} {
		if _, e := policy.Source(s, project); e != nil {
			t.Errorf("%s: %v", s, e)
		}
	}
	for _, s := range []string{Registry + "/ffffffffffffffffffffffff/app@" + hash, "https://registry.example.test/migration/app@" + hash, "registry.example.test:444/migration/app@" + hash, "registry.example.test.evil/migration/app@" + hash, "registry.example.test/migration/app:latest", "registry.example.test/migration/app:tag@" + hash, "registry.example.test/migration/../app@" + hash, "registry.example.test/t-evil/x@" + hash, "registry.other.test/t/" + project + "/" + template + "@" + hash} {
		if _, e := policy.Source(s, project); e == nil {
			t.Errorf("accepted %s", s)
		}
	}
}

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestGuard(t *testing.T) {
	calls := 0
	g := GuardTransport{Repositories: map[string]bool{"registry.example.test/migration/app": true}, Base: rt(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	})}
	for _, s := range []string{"http://registry.example.test/v2/", "https://127.0.0.1/v2/", "https://metadata.google.internal/v2/", "https://registry.example.test/service/token?scope=repository:other/repo:pull", "https://registry.other.test/v2/migration/app/manifests/x", "https://registry.other.test/service/token", "https://registry.other.test/v2/", "https://registry.example.test/v2/migration/app/../other/x", "https://registry.example.test/v2/migration/app/%2e%2e/x"} {
		r, _ := http.NewRequest("GET", s, nil)
		if _, e := g.RoundTrip(r); e == nil {
			t.Errorf("allowed %s", s)
		}
	}
	if calls != 0 {
		t.Fatalf("untrusted requests reached network: %d", calls)
	}
	for _, s := range []string{"https://registry.example.test/v2/migration/app/manifests/" + hash, "https://registry.example.test/service/token?scope=repository:migration/app:pull"} {
		r, _ := http.NewRequest("GET", s, nil)
		if _, e := g.RoundTrip(r); e != nil {
			t.Fatal(e)
		}
	}
}
func TestCredentialRepositoryBoundary(t *testing.T) {
	c := Credentials{"registry.example.test/migration/app": {Username: "source", Password: "a"}, Registry + "/" + project + "/" + template: {Username: "push", Password: "b"}}
	for ref, want := range map[string]string{"registry.example.test/migration/app": "source", Registry + "/" + project + "/" + template: "push", Registry + "/other/app": ""} {
		repo, _ := name.NewRepository(ref)
		a, e := c.Resolve(repo)
		if e != nil {
			t.Fatal(e)
		}
		v, _ := a.Authorization()
		if v.Username != want {
			t.Fatalf("%s identity=%s", ref, v.Username)
		}
	}
}

// Exercise the actual OCI library against an in-memory registry while retaining production
// URL guards. The rewrite occurs ONLY in this test transport, after guard authorization.
func TestImportRegistryIntegration(t *testing.T) {
	var enforce atomic.Bool
	var public atomic.Bool
	handler := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if enforce.Load() {
			u, _, _ := r.BasicAuth()
			if strings.HasPrefix(r.URL.Path, "/v2/migration/") {
				want := "source"
				if public.Load() {
					want = ""
				}
				if u != want {
					w.WriteHeader(403)
					return
				}
				// Even an anonymous source must never receive a platform robot.
				if public.Load() && r.Header.Get("Authorization") != "" {
					t.Error("anonymous source received authorization")
				}
			}
			if strings.HasPrefix(r.URL.Path, "/v2/t/") {
				want := "pull"
				if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
					want = "push"
				}
				if u != want && !(u == "push" && (r.Method == http.MethodGet || r.Method == http.MethodHead)) {
					w.WriteHeader(403)
					return
				}
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	base := rt(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		u := *r.URL
		copy.URL = &u
		copy.URL.Scheme = "http"
		copy.URL.Host = strings.TrimPrefix(server.URL, "http://")
		copy.Host = copy.URL.Host
		return http.DefaultTransport.RoundTrip(copy)
	})
	srcRepo := "registry.example.test/migration/app"
	dstRepo := Registry + "/" + project + "/" + template
	img, e := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: "amd64", Config: v1.Config{User: "app", WorkingDir: "/srv", Env: []string{"HELLO=preserved", "PATH=/bin"}}})
	if e != nil {
		t.Fatal(e)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "data", Mode: 0644, Size: 5})
	_, _ = tw.Write([]byte("hello"))
	_ = tw.Close()
	layer, e := tarball.LayerFromReader(bytes.NewReader(buf.Bytes()))
	if e != nil {
		t.Fatal(e)
	}
	img, e = mutate.AppendLayers(img, layer)
	if e != nil {
		t.Fatal(e)
	}
	srcTag, _ := name.NewTag(srcRepo + ":test")
	if e = remote.Write(srcTag, img, remote.WithTransport(base)); e != nil {
		t.Fatal(e)
	}
	d, _ := img.Digest()
	source := srcRepo + "@" + d.String()
	srcAuth := authn.AuthConfig{Username: "source", Password: "source-only"}
	pushAuth := authn.AuthConfig{Username: "push", Password: "push-only"}
	pullAuth := authn.AuthConfig{Username: "pull", Password: "pull-only"}
	enforce.Store(true)
	if _, err := run(context.Background(), source, srcRepo, d.String(), dstRepo, dstRepo+":build-1", false, authn.AuthConfig{}, pushAuth, pullAuth, base); err == nil {
		t.Fatal("source permission check bypassed")
	}
	result, e := run(context.Background(), source, srcRepo, d.String(), dstRepo, dstRepo+":build-1", false, srcAuth, pushAuth, pullAuth, base)
	if e != nil {
		t.Fatal(e)
	}
	public.Store(true)
	// The same OCI transfer works anonymously even though destination push/pull
	// remain authenticated and repository-scoped on the SAME registry host.
	_, e = run(context.Background(), source, srcRepo, d.String(), dstRepo+"-public", dstRepo+"-public:build-1", false, authn.AuthConfig{}, pushAuth, pullAuth, base)
	if e != nil {
		t.Fatalf("public migration: %v", e)
	}
	if _, err := run(context.Background(), source, srcRepo, d.String(), dstRepo, dstRepo+":build-1", false, srcAuth, pushAuth, pullAuth, base); err == nil {
		t.Fatal("supplied source auth silently fell back to anonymous")
	}
	enforce.Store(false)
	if e != nil {
		t.Fatal(e)
	}
	if result.User != "app" || result.Workdir != "/srv" || result.DestinationDigest != d.String() {
		t.Fatalf("result=%+v", result)
	}
	dst, _ := name.NewDigest(result.Image)
	out, e := remote.Image(dst, remote.WithTransport(base))
	if e != nil {
		t.Fatal(e)
	}
	cfg, _ := out.ConfigFile()
	b, _ := json.Marshal(cfg.Config.Env)
	if !strings.Contains(string(b), "HELLO=preserved") {
		t.Fatal("image ENV lost")
	}
	result, e = run(context.Background(), result.Image, dstRepo, d.String(), dstRepo, dstRepo, true, authn.AuthConfig{}, authn.AuthConfig{}, authn.AuthConfig{}, base)
	if e != nil || result.Image != dst.String() {
		t.Fatalf("canonical registration=%+v %v", result, e)
	}
	for _, cfg := range []*v1.ConfigFile{{OS: "linux", Architecture: "arm64"}, {OS: "linux", Architecture: "amd64", Config: v1.Config{User: "1000:1000"}}, {OS: "linux", Architecture: "amd64", Config: v1.Config{Env: []string{"PROXCLOUD_START_CMD=evil"}}}} {
		bad, _ := mutate.ConfigFile(empty.Image, cfg)
		d, _ := bad.Digest()
		if e = remote.Write(srcTag, bad, remote.WithTransport(base)); e != nil {
			t.Fatal(e)
		}
		if _, e = run(context.Background(), srcRepo+"@"+d.String(), srcRepo, d.String(), dstRepo, dstRepo+":build-1", false, authn.AuthConfig{}, authn.AuthConfig{}, authn.AuthConfig{}, base); e == nil {
			t.Fatalf("accepted unsupported config: %+v", cfg)
		}
	}
}
