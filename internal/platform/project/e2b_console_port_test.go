package project

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

func TestConsolePortView(t *testing.T) {
	doc := &sandboxTemplateDoc{ID: "t1", Name: "web", CPU: 2, MemoryGiB: 4,
		Tags: map[string]string{"default": "b1", "v1": "b1"},
		Builds: []templateBuild{
			{BuildID: "b1", Status: sandbox.BuildReady, Image: "img@sha256:1"},
			{BuildID: "b2", Status: sandbox.BuildError, Reason: "RUN failed"},
		}}
	v := consoleView(doc)
	if v.Kind != "template" || !v.Ready || v.Status != sandbox.BuildError || len(v.Builds) != 2 {
		t.Fatalf("view = %+v", v)
	}
	if v.Builds[0].BuildID != "b2" || v.Builds[0].Reason != "RUN failed" || len(v.Builds[0].Tags) != 0 {
		t.Errorf("newest build first, with its reason and no tags: %+v", v.Builds[0])
	}
	if len(v.Builds[1].Tags) != 2 {
		t.Errorf("b1 carries default and v1: %v", v.Builds[1].Tags)
	}
	doc.Kind, doc.Builds, doc.Tags = "snapshot", []templateBuild{{BuildID: "s1", Status: sandbox.BuildBuilding}}, map[string]string{}
	if v := consoleView(doc); v.Kind != "snapshot" || v.Ready || v.Status != sandbox.BuildBuilding {
		t.Errorf("assembling snapshot = %+v", v)
	}
}

func TestConsoleProjectionExcludesStoredCapabilities(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	source := "source.example.org/approved/app@" + digest
	doc := &sandboxTemplateDoc{ID: "t", Name: "app", Aliases: []string{"alias"}, UploadToken: "upload-secret", Builds: []templateBuild{{
		BuildID: "b", Status: sandbox.BuildReady, From: source, Image: source,
		Import: &templateImport{Source: source, Destination: "private-destination", Key: "import-key-secret", Result: &sandboximport.Result{ConfigDigest: "config-digest-secret"}},
		Diagnostics: &buildDiagnostics{Revision: 7, TerminalSignature: "signature-secret", Cursors: map[string]buildLogCursor{"base": {Hash: "cursor-secret"}}, TerminalTail: []string{"password=tail-secret"},
			Stages: []sandbox.BuildStage{{Phase: "kaniko", Reason: "OOMKilled", Message: "password=stage-secret"}}, PodMessage: "authorization: Bearer pod-secret", Warnings: []string{"token=warning-secret"}},
	}}}
	v := consoleView(doc)
	if v.Mode != "import" || v.Digest != digest || len(v.Aliases) != 1 || v.Builds[0].Source != source || v.Builds[0].Diagnostics.Stages[0].Reason != "OOMKilled" {
		t.Fatalf("projection=%+v", v)
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"uploadToken", "fetchToken", "import-key-secret", "private-destination", "config-digest-secret", "signature-secret", "cursor-secret", "tail-secret", "stage-secret", "pod-secret", "warning-secret", `"import":`, `"registry":`, `"env":`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("projection leaked %q: %s", forbidden, encoded)
		}
	}
	doc.Builds[0].Import.Source = "https://user:password@registry.example/image"
	if got := consoleView(doc).Source; got != "" {
		t.Fatalf("malformed credential ref echoed: %s", got)
	}
}

func TestConsoleAdapterReplacesPrincipalAndRoute(t *testing.T) {
	wrong := &e2bCaller{proj: &Project{ID: "wrong-project"}}
	authorized := &e2bCaller{proj: &Project{ID: "path-project"}}
	r := httptest.NewRequest(http.MethodPost, "/project/path-project/sandbox-templates/base/builds", strings.NewReader("original"))
	r = r.WithContext(context.WithValue(r.Context(), e2bCtxKey{}, wrong))
	rc := chi.NewRouteContext()
	rc.URLParams.Add("id", "path-project")
	rc.URLParams.Add("templateId", "base")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
	r.Header.Set("X-API-Key", "browser-key")
	r.Header.Set("Authorization", "Bearer browser-key")
	adapted := consoleE2BRequest(r, authorized, "target", "build", []byte(`{"name":"new"}`))
	if e2bFrom(adapted) != authorized || chi.URLParam(adapted, "templateID") != "target" || chi.URLParam(adapted, "buildID") != "build" || chi.URLParam(adapted, "id") != "" || adapted.Header.Get("X-API-Key") != "" || adapted.Header.Get("Authorization") != "" {
		t.Fatalf("adapted request inherited untrusted state")
	}
	body, _ := io.ReadAll(adapted.Body)
	if string(body) != `{"name":"new"}` || r.Header.Get("X-API-Key") != "browser-key" || chi.URLParam(r, "templateId") != "base" {
		t.Fatal("adapter mutated outer request or lost body")
	}
}

func TestConsoleAdapterPreservesErrorAndSuccessEnvelopes(t *testing.T) {
	for _, status := range []int{400, 403, 404, 409, 429, 503} {
		response := newBufferedResponse()
		e2bErr(response, status, "public error")
		w := httptest.NewRecorder()
		writeConsoleE2B(w, response)
		if w.Code != status || !strings.Contains(w.Body.String(), `"errors":{"code":`) || !strings.Contains(w.Body.String(), `"message":"public error"`) || strings.Contains(w.Body.String(), `"data"`) {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	for _, status := range []int{200, 202, 204} {
		response := newBufferedResponse()
		response.WriteHeader(status)
		w := httptest.NewRecorder()
		writeConsoleE2B(w, response)
		want := status
		if status == 204 {
			want = 200
		}
		if w.Code != want || !strings.Contains(w.Body.String(), `"data":{}`) {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

func TestConsoleRoutesRequireSessionNotAPIKey(t *testing.T) {
	h := &Handler{}
	router := chi.NewRouter()
	h.Routes(router)
	for _, tc := range []struct{ method, suffix string }{
		{"GET", ""}, {"GET", "/capabilities"}, {"POST", "/import"}, {"POST", "/t/aliases"}, {"DELETE", "/t/aliases"},
		{"POST", "/t/prepare"}, {"GET", "/t/prepare"}, {"POST", "/t/builds"}, {"GET", "/t/builds/b/logs"}, {"DELETE", "/t"},
	} {
		r := httptest.NewRequest(tc.method, "/project/p/sandbox-templates"+tc.suffix, strings.NewReader(`{}`))
		r.Header.Set("X-API-Key", "e2b_"+strings.Repeat("a", 40))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"errors"`) {
			t.Errorf("%s %s: %d %s", tc.method, tc.suffix, w.Code, w.Body.String())
		}
	}
}

func TestConsoleBuildRecipeBoundary(t *testing.T) {
	for _, body := range []string{
		`{"name":"app","fromImage":"evil"}`, `{"name":"app","projectId":"foreign"}`, `{"name":"app","steps":[{"type":"COPY","args":[".","/app"]}]}`,
		`{"name":"app","steps":[{"type":"ENV","args":["TOKEN","secret"]}]}`, `{"name":"app","cpuCount":-1}`,
		`{"name":"app","steps":[{"type":"RUN","args":["echo hi"],"filesHash":"file1234"}]}`, `{"name":"app"} {"name":"other"}`,
	} {
		w := httptest.NewRecorder()
		if _, ok := decodeConsoleBuild(w, httptest.NewRequest("POST", "/", strings.NewReader(body))); ok || w.Code != http.StatusBadRequest {
			t.Errorf("accepted unsupported recipe: %s", body)
		}
	}
	w := httptest.NewRecorder()
	b, ok := decodeConsoleBuild(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"app","cpuCount":3,"memoryMB":3072,"buildCpu":4,"buildMemoryGiB":8,"buildDiskGiB":40,"steps":[{"type":"RUN","args":["echo ok","root"]}]}`)))
	if !ok || b.CPUCount != 3 || b.CPU != 4 || len(b.Steps[0].Args) != 2 {
		t.Fatalf("valid bounded recipe rejected: %+v", b)
	}
}
