package project

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

func TestReadyTemplateCatalogIsolationAndBounds(t *testing.T) {
	ready := []templateBuild{{BuildID: "b1", Status: sandbox.BuildReady, Image: "image@digest"}}
	docs := []sandboxTemplateDoc{
		{ID: "own-id", Name: "my-ready", ProjectID: "own", Builds: ready},
		{ID: "named-snapshot", Name: "named-snapshot", ProjectID: "own", Kind: "snapshot", Builds: ready},
		{ID: "hidden", Name: "hidden", ProjectID: "own", Internal: true, Builds: ready},
		{ID: "foreign", Name: "foreign", ProjectID: "other", Builds: ready},
		{ID: "pending", Name: "pending", ProjectID: "own", Builds: []templateBuild{{Status: sandbox.BuildBuilding}}},
		{ID: "no-image", Name: "no-image", ProjectID: "own", Builds: []templateBuild{{Status: sandbox.BuildReady}}},
	}
	cfg := sandbox.Config{Templates: map[string]sandbox.Template{"python": {Image: "py"}, "base": {Image: "base"}}}
	got := readyTemplateCatalog("own", cfg, docs)
	for _, want := range []string{"base", "python", "my-ready (ID own-id)", "named-snapshot"} {
		if !strings.Contains(got, want) {
			t.Errorf("catalog %q lacks %q", got, want)
		}
	}
	for _, hidden := range []string{"hidden", "foreign", "pending", "no-image"} {
		if strings.Contains(got, hidden) {
			t.Errorf("catalog leaks %s: %s", hidden, got)
		}
	}
	for i := range 40 {
		docs = append(docs, sandboxTemplateDoc{ID: fmt.Sprintf("id%d", i), Name: strings.Repeat("n", 60), ProjectID: "own", Builds: ready})
	}
	got = readyTemplateCatalog("own", cfg, docs)
	if len(got) > 1100 || !strings.Contains(got, "more in GET /templates") {
		t.Errorf("catalog not bounded: %d bytes", len(got))
	}
}

func TestEnvironmentOpenAPIParity(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(e2bOpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	env := doc["components"].(map[string]any)["schemas"].(map[string]any)["EnvVars"].(map[string]any)
	if env["maxProperties"] != float64(256) || !strings.Contains(env["description"].(string), "32768 UTF-8 bytes") || !strings.Contains(env["description"].(string), "E2B_SANDBOX_ID") {
		t.Fatal("OpenAPI env quota or reserved ID documentation diverged")
	}
}

func TestUnknownTemplateAndDocsLinks(t *testing.T) {
	h := &Handler{}
	if !strings.Contains(h.e2bDocs(), "console /docs/api/e2b") {
		t.Fatal("unconfigured console must give console-relative hint, not API-origin URL")
	}
	h.SetE2BConsoleURL("https://console.internal.example/")
	c := &e2bCaller{proj: &Project{ID: "p1"}, ss: sandbox.NewWithAPI(nil, sandbox.Config{Templates: map[string]sandbox.Template{"base": {Image: "base"}}}, "svc")}
	msg := h.unknownTemplate(context.Background(), c, "not-found")
	// Verify the actual create error path, not just the message formatter. No cluster
	// or template store is needed for an unknown name.
	req := httptest.NewRequest("POST", "/e2b/sandboxes", strings.NewReader(`{"templateID":"not-found"}`))
	req = req.WithContext(context.WithValue(req.Context(), e2bCtxKey{}, c))
	c.proj.Status = StatusEnabled
	create := httptest.NewRecorder()
	h.e2bCreate(create, req)
	if create.Code != 400 || !strings.Contains(create.Body.String(), "GET /templates") || !strings.Contains(create.Body.String(), "base") {
		t.Fatalf("unknown-template create: %d %s", create.Code, create.Body.String())
	}
	for _, want := range []string{"unknown template", "base", "name[:tag]", "GET /templates", "https://console.internal.example/docs/api/e2b"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %s", want, msg)
		}
	}
	r := chi.NewRouter()
	r.Route("/e2b", h.E2BRoutes)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/e2b/no-such-endpoint", nil))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "https://console.internal.example/docs/api/e2b") || strings.Contains(rec.Body.String(), "docs/e2b-compat.md") {
		t.Fatalf("unsupported endpoint docs: %d %s", rec.Code, rec.Body.String())
	}
}
