package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestResolve(t *testing.T) {
	raw := map[string]json.RawMessage{
		"context7":         json.RawMessage(`{"apiKey":"k1"}`),
		"filesystem":       json.RawMessage(`{"paths":["/a","/b"]}`),
		"notion":           json.RawMessage(`{"internalIntegrationToken":"n1"}`),
		"deepwiki":         json.RawMessage(`{}`),
		"github/acme/tool": json.RawMessage(`{"run_cmd":"node x.js","envs":{"A":"1"}}`),
	}
	ups, err := resolve(raw)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]upstream{}
	for _, u := range ups {
		by[u.prefix] = u
	}
	if u := by["context7"]; u.e.Headers["CONTEXT7_API_KEY"] != "k1" || !strings.HasPrefix(u.e.URL, "https://") {
		t.Errorf("context7: %+v", u.e)
	}
	if u := by["filesystem"]; strings.Join(u.e.Cmd, " ") != "mcp-server-filesystem /a /b" {
		t.Errorf("filesystem: %v", u.e.Cmd)
	}
	if u := by["notion"]; !strings.Contains(u.e.Env["OPENAPI_MCP_HEADERS"], `"Bearer n1"`) {
		t.Errorf("notion: %v", u.e.Env)
	}
	if u := by["acme-tool"]; u.github == nil || u.repo != "acme/tool" {
		t.Errorf("github: %+v", u)
	}
	if _, ok := by["deepwiki"]; !ok {
		t.Error("deepwiki missing")
	}

	for cfg, want := range map[string]string{
		`{"context7":{}}`:              `missing "apiKey"`,
		`{"nope":{}}`:                  "nope not available on ProxCloud",
		`{"github/x":{"run_cmd":"a"}}`: "github/<owner>/<repo>",
		`{"filesystem":{}}`:            `missing "paths"`,
	} {
		var r map[string]json.RawMessage
		_ = json.Unmarshal([]byte(cfg), &r)
		if _, err := resolve(r); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", cfg, err)
		}
	}
}

// TestGatewayPrefixesAndAuth: an upstream's tools are offered as "<prefix>-<tool>", calls are
// forwarded, and /mcp needs the bearer.
func TestGatewayPrefixesAndAuth(t *testing.T) {
	up := mcp.NewServer(&mcp.Implementation{Name: "up"}, nil)
	type in struct {
		Msg string `json:"msg"`
	}
	mcp.AddTool(up, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, a in) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "got " + a.Msg}}}, nil, nil
	})
	upSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return up }, nil))
	defer func() { upSrv.CloseClientConnections(); upSrv.Close() }()

	gw := mcp.NewServer(&mcp.Implementation{Name: "gw"}, nil)
	if err := connectAll(context.Background(), gw, []upstream{{prefix: "deepwiki", e: entry{URL: upSrv.URL}}}); err != nil {
		t.Fatal(err)
	}
	st := &state{ready: true}
	gwSrv := httptest.NewServer(handler(gw, "tok", st))
	defer func() { gwSrv.CloseClientConnections(); gwSrv.Close() }()

	res, _ := http.Post(gwSrv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", res.StatusCode)
	}
	res.Body.Close()

	hc := &http.Client{Transport: headerRT{map[string]string{"Authorization": "Bearer tok"}, http.DefaultTransport}}
	cl := mcp.NewClient(&mcp.Implementation{Name: "c"}, nil)
	cs, err := cl.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: gwSrv.URL + "/mcp", HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(lt.Tools) != 1 || lt.Tools[0].Name != "deepwiki-echo" {
		t.Fatalf("tools: %v %v", lt, err)
	}
	r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "deepwiki-echo", Arguments: map[string]any{"msg": "hi"}})
	if err != nil || r.Content[0].(*mcp.TextContent).Text != "got hi" {
		t.Fatalf("call: %v %v", r, err)
	}

	code := httptest.NewRecorder()
	handler(gw, "tok", &state{err: context.DeadlineExceeded}).ServeHTTP(code, httptest.NewRequest("GET", "/health", nil))
	if code.Code != 500 || !strings.Contains(code.Body.String(), "Failed to start MCP gateway") {
		t.Fatalf("health on failure: %d %s", code.Code, code.Body.String())
	}
}

// TestPackageServerFromBinPath: a package server is found on the servers' PATH, not only on the
// gateway's own (exec.Command looks the name up when the command is built).
func TestPackageServerFromBinPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fake-srv"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := binPath
	binPath = dir
	defer func() { binPath = old }()
	tr, err := transportFor(upstream{prefix: "x", e: entry{Cmd: []string{"fake-srv", "a"}}})
	if err != nil {
		t.Fatal(err)
	}
	c := tr.(*mcp.CommandTransport).Command
	if c.Err != nil || c.Path != filepath.Join(dir, "fake-srv") || c.Args[1] != "a" {
		t.Fatalf("cmd: path=%s err=%v args=%v", c.Path, c.Err, c.Args)
	}
	if _, err := transportFor(upstream{prefix: "y", e: entry{Cmd: []string{"not-there"}}}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing binary: %v", err)
	}
}

// TestProbeRefusesAServerThatDoesNotStart: the build-time check fails on an installed server that exits before
// answering initialize (a Node too old for it, a broken install), not only on a missing binary.
func TestProbeRefusesAServerThatDoesNotStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dead-srv"), []byte("#!/bin/sh\necho 'SyntaxError' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := binPath
	binPath = dir
	defer func() { binPath = old }()
	if err := probe(entry{Name: "dead", Fields: []string{"apiKey"}, Cmd: []string{"dead-srv"}, Env: map[string]string{"K": "{apiKey}"}}); err == nil {
		t.Fatal("probe accepted a server that exits at once")
	}
}
