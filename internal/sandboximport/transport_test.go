package sandboximport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestGuardRejectsNestedRepositoriesAndUploadLocations(t *testing.T) {
	calls := 0
	g := GuardTransport{Repositories: map[string]bool{"registry.example.test/migration/app": true}, Base: rt(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	for _, path := range []string{
		"/v2/migration/app/other/manifests/" + hash,
		"/v2/migration/app/other/blobs/uploads/u",
		"/v2/migration/app/blobs/uploads/u/other",
		"/v2/migration/app/blobs/uploads/?mount=" + hash + "&from=other/repo",
		"/v2/migration/app/blobs/uploads/u?scope=repository:other/repo:push",
		"/v2/migration/app/tags/list",
		"/service/token?scope=repository:migration/app/other:pull",
	} {
		r, _ := http.NewRequest(http.MethodPut, "https://registry.example.test"+path, nil)
		if _, err := g.RoundTrip(r); err == nil {
			t.Errorf("untrusted endpoint accepted %s", path)
		}
	}
	if calls != 0 {
		t.Fatal("untrusted endpoints reached credential-bearing transport", calls)
	}
	r, _ := http.NewRequest(http.MethodPut, "https://registry.example.test/v2/migration/app/blobs/uploads/uuid-123?_state=opaque&digest="+hash, nil)
	if _, err := g.RoundTrip(r); err != nil {
		t.Fatal("exact upload rejected", err)
	}
}

// Actual ggcr follows a 202 upload Location. A server-selected nested repository
// must fail before it reaches the network, even on the otherwise authorized host.
func TestStreamingPushRejectsMaliciousNested202UploadLocation(t *testing.T) {
	badCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/other/") {
			badCalls++
		}
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(200)
		case r.Method == http.MethodHead:
			w.WriteHeader(404)
		case r.Method == http.MethodPost:
			w.Header().Set("Location", "https://registry.example.test/v2/migration/app/other/blobs/uploads/uuid-1")
			w.WriteHeader(202)
		default:
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	base := rt(func(r *http.Request) (*http.Response, error) {
		c := r.Clone(r.Context())
		u := *r.URL
		c.URL = &u
		c.URL.Scheme = "http"
		c.URL.Host = strings.TrimPrefix(server.URL, "http://")
		return http.DefaultTransport.RoundTrip(c)
	})
	g := GuardTransport{Repositories: map[string]bool{"registry.example.test/migration/app": true}, Base: base}
	ref, _ := name.NewTag("registry.example.test/migration/app:test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := remote.Write(ref, empty.Image, remote.WithContext(ctx), remote.WithTransport(g), remote.WithAuth(authn.FromConfig(authn.AuthConfig{Username: "push", Password: "secret"})))
	if err == nil || badCalls != 0 {
		t.Fatal("nested upload reached network", err, badCalls)
	}
}
