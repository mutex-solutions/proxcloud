package project

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// ListedSandbox's required fields (E2B openapi.yml) must always be present.
func TestE2BListedShape(t *testing.T) {
	d := map[string]any{"template": "python", "runningSince": "2026-11-06T01:00:00Z", "shutdownTime": "2026-11-06T02:00:00Z",
		"cpu": 2, "memoryGiB": 4, "metadata": map[string]string{"run": "42"}}
	got := e2bListed("sbx-00000001", d)
	for _, k := range []string{"templateID", "sandboxID", "clientID", "startedAt", "cpuCount", "memoryMB", "diskSizeMB", "endAt", "state", "envdVersion"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	if got["memoryMB"] != 4096 || got["state"] != "running" || got["envdVersion"] != sandbox.EnvdVersion {
		t.Errorf("listed = %v", got)
	}
	if !metadataMatches(d["metadata"], url.Values{"run": {"42"}}) || metadataMatches(d["metadata"], url.Values{"run": {"7"}}) {
		t.Error("metadata filter")
	}
}

func TestE2BNetworkMapping(t *testing.T) {
	no := false
	if n := (*e2bNetworkBody)(nil).policy(&no); n == nil || n.AllowsInternet() {
		t.Errorf("allow_internet_access=false must deny all egress: %+v", n)
	}
	if n := (*e2bNetworkBody)(nil).policy(nil); n != nil {
		t.Errorf("no network config = open internet, got %+v", n)
	}
	var b e2bNetworkBody
	_ = json.Unmarshal([]byte(`{"allowOut":["1.1.1.1"],"denyOut":["0.0.0.0/0"]}`), &b)
	if n := b.policy(nil); n == nil || !n.AllowsInternet() || len(n.AllowOut) != 1 {
		t.Errorf("allow-list mapping: %+v", n)
	}
	for body, want := range map[string]string{
		`{"maskRequestHost":"x"}`:            "maskRequestHost",
		`{"httpsPorts":[8443]}`:              "httpsPorts",
		`{"egressProxy":{"url":"http://p"}}`: "egressProxy",
		`{"rules":{"a.com":{}}}`:             "rules",
		`{"rules":{}}`:                       "",
	} {
		var nb e2bNetworkBody
		_ = json.Unmarshal([]byte(body), &nb)
		if got := nb.unsupported(); !strings.Contains(got, want) || (want == "" && got != "") {
			t.Errorf("%s: unsupported() = %q, want %q", body, got, want)
		}
	}
}

// What a create asks for that this location cannot honour is refused, never silently dropped: without pause (SB7) an
// autoPause sandbox would be deleted at its timeout.
func TestE2BCreateRefusals(t *testing.T) {
	for body, want := range map[string]string{
		`{}`:                                     "templateID",
		`{"templateID":"base","autoPause":true}`: "autoPause",
		`{"templateID":"base","autoResume":{"enabled":true}}`:     "autoResume",
		`{"templateID":"base","volumeMounts":[{"name":"v"}]}`:     "volumeMounts",
		`{"templateID":"base","mcp":{"github":{}}}`:               "mcp",
		`{"templateID":"base","network":{"httpsPorts":[8443]}}`:   "httpsPorts",
		`{"templateID":"base","mcp":null,"network":{"rules":{}}}`: "",
		`{"templateID":"base","autoPause":false}`:                 "",
	} {
		var b e2bCreateBody
		if err := json.Unmarshal([]byte(body), &b); err != nil {
			t.Fatal(err)
		}
		if got := b.e2bRefusal(); !strings.Contains(got, want) || (want == "" && got != "") {
			t.Errorf("%s: refusal %q, want %q", body, got, want)
		}
	}
}

// Without a valid key every E2B call is a 401 in E2B's {code, message} shape; /health is open; anything not served
// is a 404 in the same shape.
func TestE2BAuthAndErrors(t *testing.T) {
	h := &Handler{}
	r := chi.NewRouter()
	r.Route("/e2b", h.E2BRoutes)
	for _, c := range []struct {
		method, path, key string
		status            int
	}{
		{"GET", "/e2b/health", "", 200},
		{"GET", "/e2b/v2/sandboxes", "", 401},
		{"POST", "/e2b/v2/sandboxes", "e2b_short", 401},
		{"GET", "/e2b/v2/sandboxes", "pk0123.sk0123", 401},
		{"GET", "/e2b/volumes", "", 404},
		{"POST", "/e2b/v3/templates", "", 404},
		{"POST", "/e2b/sandboxes/sbx-0123456789abcdef/pause", "", 404},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.key != "" {
			req.Header.Set("X-API-Key", c.key)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != c.status {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, rec.Code, c.status)
		}
		if c.status >= 400 {
			var e struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Code != c.status || e.Message == "" {
				t.Errorf("%s %s: body %q is not E2B's Error schema", c.method, c.path, rec.Body.String())
			}
		}
	}
	if hashSandboxKey("e2b_x") == hashSandboxKey("e2b_y") || len(hashSandboxKey("e2b_x")) != 64 {
		t.Error("key hash")
	}
}
