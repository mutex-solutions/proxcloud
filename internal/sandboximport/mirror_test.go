package sandboximport

import (
	"archive/tar"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

var mirrorPolicy = MirrorPolicy{Registry: "192.0.2.226"}

func TestMirrorCheck(t *testing.T) {
	src := "ghcr.io/mutex-solutions/sandbox-base@" + hash
	dst := "192.0.2.226/library/sandbox-base:20261112-9d00563"
	if n, d, err := mirrorPolicy.Check(src, dst); err != nil || n != "base" || d != hash {
		t.Fatalf("valid pair: %s %s %v", n, d, err)
	}
	bad := [][2]string{
		{"ghcr.io/mutex-solutions/sandbox-base:20261112-9d00563", dst},         // tag, not digest
		{"ghcr.io/other/sandbox-base@" + hash, dst},                            // another owner
		{"docker.io/mutex-solutions/sandbox-base@" + hash, dst},                // another registry
		{"ghcr.io/mutex-solutions/proxcloud-api@" + hash, dst},                 // not a sandbox image
		{src, "192.0.2.226/library/sandbox-node:20261112-9d00563"},             // another name
		{src, "192.0.2.226/library/proxcloud-sandbox-import:20261112-9d00563"}, // platform image
		{src, "192.0.2.226/t/p/x:20261112-9d00563"},                            // a project path
		{src, "192.0.2.227/library/sandbox-base:20261112-9d00563"},             // another registry
		{src, "192.0.2.226/library/sandbox-base:latest"},                       // not a contract tag
		{src, "192.0.2.226/library/sandbox-base@" + hash},                      // digest destination
		{"ghcr.io/mutex-solutions/sandbox-../x@" + hash, dst},                  // path tricks
	}
	for _, b := range bad {
		if _, _, err := mirrorPolicy.Check(b[0], b[1]); err == nil {
			t.Errorf("accepted %v", b)
		}
	}
	if _, err := ParseMirrorPolicy(`{"registry":"192.0.2.226","extra":1}`); err == nil {
		t.Error("unknown policy field accepted")
	}
	for _, reg := range []string{"", "ghcr.io", "a/b", "UPPER.example"} {
		if (MirrorPolicy{Registry: reg}).Validate() == nil {
			t.Errorf("registry %q accepted", reg)
		}
	}
}

func TestMirrorSourceGuard(t *testing.T) {
	calls := 0
	g := mirrorSourceGuard{Repo: "mutex-solutions/sandbox-base", Base: rt(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})}
	for _, u := range []string{
		"http://ghcr.io/v2/", "https://ghcr.io/v2/mutex-solutions/sandbox-node/manifests/" + hash,
		"https://ghcr.io/v2/mutex-solutions/sandbox-base/manifests/latest", "https://ghcr.io/token?scope=repository:mutex-solutions/sandbox-node:pull",
		"https://ghcr.io/token?scope=repository:mutex-solutions/sandbox-base:push", "https://evil.test/v2/",
		"https://ghcr.io/v2/mutex-solutions/sandbox-base/blobs/uploads/", "https://ghcr.io/v2/_catalog",
	} {
		r, _ := http.NewRequest("GET", u, nil)
		if _, err := g.RoundTrip(r); err == nil {
			t.Errorf("allowed %s", u)
		}
	}
	r, _ := http.NewRequest("PUT", "https://ghcr.io/v2/mutex-solutions/sandbox-base/manifests/"+hash, nil)
	if _, err := g.RoundTrip(r); err == nil {
		t.Error("allowed a write to the source")
	}
	if calls != 0 {
		t.Fatalf("refused requests reached the network: %d", calls)
	}
	// The blob storage host is reached without the credential.
	var sawAuth string
	g.Base = rt(func(r *http.Request) (*http.Response, error) {
		sawAuth = r.Header.Get("Authorization")
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})
	r, _ = http.NewRequest("GET", "https://"+MirrorBlobHost+"/ghcr1/blobs/sha256:x?sig=1", nil)
	r.Header.Set("Authorization", "Bearer pat-must-not-leave")
	if _, err := g.RoundTrip(r); err != nil || sawAuth != "" {
		t.Fatalf("blob host: err=%v auth=%q", err, sawAuth)
	}
	// A blob redirect anywhere else is refused.
	g.Base = rt(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 307, Body: http.NoBody, Header: http.Header{"Location": {"https://evil.test/x"}}}, nil
	})
	r, _ = http.NewRequest("GET", "https://ghcr.io/v2/mutex-solutions/sandbox-base/blobs/"+hash, nil)
	if _, err := g.RoundTrip(r); err == nil {
		t.Fatal("followed a blob redirect off the storage host")
	}
}

// Copies through the real OCI library against two in-memory registries (ghcr.io and the sandbox registry), with the
// production guards in front; only the test transport rewrites hosts after the guards authorized the request.
func TestRunMirrorPreservesDigestAndScopesCredentials(t *testing.T) {
	type seen struct{ host, user string }
	var mu sync.Mutex
	var calls []seen
	ghcr, zot := registry.New(), registry.New()
	srv := func(h http.Handler, host string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, _, _ := r.BasicAuth()
			mu.Lock()
			calls = append(calls, seen{host, u})
			mu.Unlock()
			h.ServeHTTP(w, r)
		}))
	}
	gs, zs := srv(ghcr, "ghcr.io"), srv(zot, "zot")
	defer gs.Close()
	defer zs.Close()
	base := rt(func(r *http.Request) (*http.Response, error) {
		c := r.Clone(r.Context())
		u := *r.URL
		c.URL = &u
		c.URL.Scheme = "http"
		target := zs.URL
		if r.URL.Host == "ghcr.io" {
			target = gs.URL
		}
		c.URL.Host = strings.TrimPrefix(target, "http://")
		c.Host = c.URL.Host
		resp, err := http.DefaultTransport.RoundTrip(c)
		if resp != nil {
			resp.Request = r // as a real transport answers: upload Locations resolve against the guarded URL
		}
		return resp, err
	})
	img := mirrorTestImage(t, "hello")
	srcTag, _ := name.NewTag("ghcr.io/mutex-solutions/sandbox-base:20261112-9d00563")
	if err := remote.Write(srcTag, img, remote.WithTransport(base)); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	source := "ghcr.io/mutex-solutions/sandbox-base@" + d.String()
	dest := "192.0.2.226/library/sandbox-base:20261112-9d00563"
	pat := authn.AuthConfig{Username: "proxcloud", Password: "ghp_pat"}
	push := authn.AuthConfig{Username: "mirror", Password: "mirror-pw"}
	res, err := runMirror(context.Background(), mirrorPolicy, source, dest, pat, push, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateMirrorResult(*res, source, dest); err != nil || res.DestinationDigest != d.String() {
		t.Fatalf("result %+v: %v", res, err)
	}
	for _, c := range calls {
		if (c.host == "ghcr.io" && c.user != "" && c.user != "proxcloud") || (c.host == "zot" && c.user != "" && c.user != "mirror") {
			t.Fatalf("a credential crossed registries: %+v", c)
		}
	}
	// The destination holds the very same manifest digest.
	dt, _ := name.NewTag(dest)
	got, err := remote.Head(dt, remote.WithTransport(base))
	if err != nil || got.Digest != d {
		t.Fatalf("destination digest %v %v", got, err)
	}
	// A re-run (lost acquisition) is a no-op success; a different digest under the same tag is refused.
	if _, err := runMirror(context.Background(), mirrorPolicy, source, dest, pat, push, base); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	other := mirrorTestImage(t, "other")
	if err := remote.Write(srcTag, other, remote.WithTransport(base)); err != nil {
		t.Fatal(err)
	}
	od, _ := other.Digest()
	if _, err := runMirror(context.Background(), mirrorPolicy, "ghcr.io/mutex-solutions/sandbox-base@"+od.String(), dest, pat, push, base); err == nil || !strings.Contains(err.Error(), "never moved") {
		t.Fatalf("an existing tag was moved: %v", err)
	}
	// Missing credentials fail closed before any request.
	if _, err := RunMirror(context.Background(), mirrorPolicy, source, dest, authn.AuthConfig{}, push); err == nil {
		t.Fatal("anonymous source accepted")
	}
	if _, err := RunMirror(context.Background(), mirrorPolicy, source, dest, pat, authn.AuthConfig{}); err == nil {
		t.Fatal("anonymous destination accepted")
	}
	bad := *res
	bad.DestinationDigest = "sha256:" + strings.Repeat("b", 64)
	if ValidateMirrorResult(bad, source, dest) == nil {
		t.Fatal("a result with another destination digest was accepted")
	}
}

func mirrorTestImage(t *testing.T, content string) v1.Image {
	t.Helper()
	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: "amd64", Config: v1.Config{User: "root"}})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "f", Mode: 0o644, Size: int64(len(content))})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	layer, err := tarball.LayerFromReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.AppendLayers(img, layer)
	if err != nil {
		t.Fatal(err)
	}
	return img
}
