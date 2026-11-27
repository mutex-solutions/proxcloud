package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func testHandler() *Handler {
	lookup := func(_ context.Context, id string) (string, bool) {
		if id == "pk"+strings.Repeat("a", 32) {
			return "sk" + strings.Repeat("b", 40), true
		}
		return "", false
	}
	return New(slog.Default(), lookup,
		"https://auth.example.com/realms/clients",
		"https://auth.example.com/realms/master",
		"https://api.example.com")
}

// no credential → 401 with the RFC 9728 challenge.
func TestGateUnauthenticated(t *testing.T) {
	h := testHandler()
	req := httptest.NewRequest("POST", "/mcp", nil)
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{}))
	rec := httptest.NewRecorder()
	h.gate().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "/.well-known/oauth-protected-resource") {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
}

// valid pk.sk → admin toolset (initialize round-trips through the MCP handler).
func TestGateAPIKey(t *testing.T) {
	h := testHandler()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer pk"+strings.Repeat("a", 32)+".sk"+strings.Repeat("b", 40))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{}))
	rec := httptest.NewRecorder()
	h.gate().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "proxcloud-admin") {
		t.Fatalf("expected admin server implementation, got: %s", rec.Body.String())
	}
}

// wrong secret → 401, not admin.
func TestGateBadAPIKey(t *testing.T) {
	h := testHandler()
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer pk"+strings.Repeat("a", 32)+".sk"+strings.Repeat("c", 40))
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{}))
	rec := httptest.NewRecorder()
	h.gate().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

// JWT principal routed by issuer: clients realm → client server, unknown realm → 403.
func TestGateJWTRealmRouting(t *testing.T) {
	h := testHandler()
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(init))
	req.Header.Set("Authorization", "Bearer fake.jwt.token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{
		Sub: "u1", Issuer: "https://auth.example.com/realms/clients",
	}))
	rec := httptest.NewRecorder()
	h.gate().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"proxcloud"`) {
		t.Fatalf("client realm: status=%d body=%s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest("POST", "/mcp", strings.NewReader(init))
	req2 = req2.WithContext(httpx.WithRC(req2.Context(), &httpx.RequestContext{
		Sub: "u1", Issuer: "https://evil.example.com/realms/x",
	}))
	rec2 := httptest.NewRecorder()
	h.gate().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("foreign realm: status=%d", rec2.Code)
	}
}

// Regression: dispatch must strip chi's inherited RouteContext — the tool ctx
// descends from POST /mcp, and chi reuses an inherited RouteContext, routing
// every dispatched call as POST (GET list tools 400/405'd on final-test).
func TestDispatchStripsRouteContext(t *testing.T) {
	h := testHandler()
	root := chi.NewRouter()
	root.Get("/api/v1/thing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h.SetRoot(root)

	// Simulate the poisoned ctx: chi route state from the outer POST /mcp.
	rctx := chi.NewRouteContext()
	rctx.RouteMethod = "POST"
	ctx := context.WithValue(context.Background(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, credKey{}, cred{jwt: "x.y.z"})

	status, body, err := h.dispatch(ctx, toolDef{name: "t", method: "GET", path: "/api/v1/thing"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s (RouteContext leaked?)", status, body)
	}
}

// resource metadata document shape.
func TestResourceMetadata(t *testing.T) {
	h := testHandler()
	rec := httptest.NewRecorder()
	h.resourceMetadata(rec, httptest.NewRequest("GET", "/.well-known/oauth-protected-resource", nil))
	body := rec.Body.String()
	for _, want := range []string{`"resource":"https://api.example.com/mcp"`, "realms/clients", "realms/master"} {
		if !strings.Contains(body, want) {
			t.Fatalf("metadata missing %q: %s", want, body)
		}
	}
}

// A tool call leaves no argument value in any log line: not when it is refused before it is sent, and not when the
// route behind it fails. Secrets and registry passwords travel as arguments.
func TestToolCallLogsNoArgumentValue(t *testing.T) {
	const mark = "s3cr3t-marker-value"
	var buf lockedLog
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	old := slog.Default()
	slog.SetDefault(log)
	defer slog.SetDefault(old)

	h := New(log, nil, "https://auth.example.com/realms/clients", "https://auth.example.com/realms/master", "https://api.example.com")
	root := chi.NewRouter()
	root.HandleFunc("/*", func(w http.ResponseWriter, _ *http.Request) {
		httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, "internal error")
	})
	h.SetRoot(root)
	ctx := context.WithValue(context.Background(), credKey{}, cred{jwt: "x.y.z"})
	tool := func(name string) toolDef {
		for _, d := range clientTools {
			if d.name == name {
				return d
			}
		}
		t.Fatalf("no tool %q", name)
		return toolDef{}
	}
	secret := map[string]any{"TOKEN": mark}
	registry := map[string]any{"username": "u", "password": mark}
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"create_function", map[string]any{"id": "p1", "name": "api", "image": "nginx:1", "secrets": secret, "registryAuth": registry}},
		{"create_function", map[string]any{"id": "p1", "secrets": secret}},                                                                 // refused: no name
		{"update_function", map[string]any{"id": "p1", "fn": "fn-0123456789abcdef", "registryAuth": registry, "removeRegistryAuth": true}}, // refused: both
		{"function_action", map[string]any{"id": "p1", "fn": "fn-0123456789abcdef", "action": mark}},                                       // refused: no such action
	}
	for _, c := range calls {
		status, body, err := h.dispatch(ctx, tool(c.tool), c.args)
		if err == nil && status != http.StatusInternalServerError {
			t.Errorf("%s: status %d", c.tool, status)
		}
		for what, text := range map[string]string{"answer": string(body), "error": errText(err)} {
			if strings.Contains(text, mark) {
				t.Errorf("%s: the %s carries an argument value: %s", c.tool, what, text)
			}
		}
	}
	if strings.Contains(buf.String(), mark) {
		t.Errorf("a log line carries an argument value:\n%s", buf.String())
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
