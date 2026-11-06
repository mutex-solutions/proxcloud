package auth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func TestGuardRefusesProtectedAndAnonymizesPublic(t *testing.T) {
	const pat = "pk0123456789abcdef0123456789abcdef.sk0123456789abcdef0123456789abcdef01234567"
	a := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.SetRealms([]Realm{{Name: "main", IssuerURI: "https://id.test/realms/customers"}})
	a.SetClientKeyLookup(func(*http.Request, string) (ClientKey, bool) {
		return ClientKey{Secret: "sk0123456789abcdef0123456789abcdef01234567", Sub: "u1"}, true
	})
	var seen *httpx.RequestContext
	h := a.Enforce(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = httpx.RC(r.Context()) }))
	do := func(path string) *httptest.ResponseRecorder {
		seen = nil
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+pat)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := do("/api/v1/organizations"); w.Code != http.StatusOK || seen == nil || seen.Sub != "u1" || !seen.PAT {
		t.Fatalf("no guard: %d %+v", w.Code, seen)
	}

	a.SetGuard(func(_ context.Context, rc *httpx.RequestContext) string {
		if rc.Sub != "u1" {
			t.Errorf("guard got %+v", rc)
		}
		return "sso_required"
	})
	w := do("/api/v1/organizations")
	if w.Code != http.StatusForbidden || seen != nil || !strings.Contains(w.Body.String(), "sso_required") {
		t.Fatalf("protected path: %d %s, handler saw %+v", w.Code, w.Body, seen)
	}
	// /mcp does its own gate on the context, so a refused identity must arrive anonymous.
	if w := do("/mcp"); w.Code != http.StatusOK || seen == nil || seen.Sub != "" {
		t.Fatalf("public path: %d %+v", w.Code, seen)
	}
}

func TestStringList(t *testing.T) {
	if got := stringList("a"); len(got) != 1 || got[0] != "a" {
		t.Fatal(got)
	}
	if got := stringList([]any{"a", 1, "b"}); len(got) != 2 || got[1] != "b" {
		t.Fatal(got)
	}
	if got := stringList(nil); got != nil {
		t.Fatal(got)
	}
}
