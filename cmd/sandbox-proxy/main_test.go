package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	testDomain = "sbx.test"
	fnHost     = "app-0a0a0a." + testDomain
)

// fakeCluster has one sandbox pod and one function with one pod, both on this host.
type fakeCluster struct {
	fnPort          string
	deploymentLists atomic.Int32
}

func readyPod(annotations map[string]any) map[string]any {
	return map[string]any{"metadata": map[string]any{"annotations": annotations}, "status": map[string]any{
		"phase": "Running", "podIP": "127.0.0.1", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}
}

func (c *fakeCluster) ListPodsAllNamespaces(context.Context, string) ([]map[string]any, error) {
	return []map[string]any{readyPod(nil)}, nil
}

func (c *fakeCluster) ListCRsAllNamespaces(context.Context, string, string, string, string) ([]map[string]any, error) {
	c.deploymentLists.Add(1)
	return []map[string]any{{"metadata": map[string]any{
		"name": "fn-00000000000000aa", "namespace": "fn-p1",
		"labels":      map[string]any{"proxcloud.io/function-host": "app-0a0a0a"},
		"annotations": map[string]any{"proxcloud.io/function-port": c.fnPort},
	}}}, nil
}

func (c *fakeCluster) ListPods(context.Context, string, string) ([]map[string]any, error) {
	return []map[string]any{readyPod(map[string]any{})}, nil
}

func answer(h http.Handler, host string) string {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://"+host+"/", nil))
	return fmt.Sprint(w.Code, " ", strings.TrimSpace(w.Body.String()))
}

// Without FUNCTION_DOMAIN, or with it but without a wake URL and a token of its own, the proxy is
// the sandbox proxy it was: a function host is refused as any host that names no sandbox port,
// the function Deployments are never listed, and nothing is reported to the portal. The sandbox
// wake token is not a function wake token.
func TestFunctionModeOff(t *testing.T) {
	var portal atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { portal.Add(1) }))
	defer api.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("up")) }))
	defer up.Close()
	port := up.URL[strings.LastIndex(up.URL, ":")+1:]
	for name, env := range map[string]map[string]string{
		"no function domain":    {"PROXCLOUD_FUNCTION_WAKE_URL": api.URL, "PROXCLOUD_FUNCTION_WAKE_TOKEN": "fn-token"},
		"no wake url or token":  {"FUNCTION_DOMAIN": testDomain},
		"no token":              {"FUNCTION_DOMAIN": testDomain, "PROXCLOUD_FUNCTION_WAKE_URL": api.URL},
		"no url":                {"FUNCTION_DOMAIN": testDomain, "PROXCLOUD_FUNCTION_WAKE_TOKEN": "fn-token"},
		"only the sandbox wake": {"FUNCTION_DOMAIN": testDomain, "PROXCLOUD_WAKE_URL": api.URL, "PROXCLOUD_WAKE_TOKEN": "sandbox-token", "PROXCLOUD_FUNCTION_WAKE_URL": api.URL},
	} {
		t.Run(name, func(t *testing.T) {
			env["SANDBOX_DOMAIN"] = testDomain
			var logged bytes.Buffer
			c := &fakeCluster{fnPort: port}
			h, drain, functions := handler(func(k string) string { return env[k] }, c, slog.New(slog.NewTextHandler(&logged, nil)))
			if functions {
				t.Fatal("function mode is on")
			}
			if got := answer(h, fnHost); got != `400 {"code":400,"message":"invalid sandbox port"}` {
				t.Errorf("a function host: %s", got)
			}
			if got := answer(h, port+"-sbx-12345678."+testDomain); got != "200 up" {
				t.Errorf("a sandbox host: %s", got)
			}
			drain()
			if c.deploymentLists.Load() != 0 || portal.Load() != 0 {
				t.Errorf("%d Deployment lists, %d calls to the portal", c.deploymentLists.Load(), portal.Load())
			}
			// A domain without the means to wake is a mistake the log must name.
			if said := strings.Contains(logged.String(), "functions are not served"); said != (env["FUNCTION_DOMAIN"] != "") {
				t.Errorf("log: %q", logged.String())
			}
		})
	}
}

// With the three settings the same handler serves both, and the portal's function routes are
// called with the function token only.
func TestFunctionModeOn(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	api := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
	}))
	defer api.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("up")) }))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	env := map[string]string{
		"SANDBOX_DOMAIN": testDomain, "FUNCTION_DOMAIN": "SBX.test", // a domain is not case sensitive
		"PROXCLOUD_WAKE_URL": api.URL + "/internal/sandboxes", "PROXCLOUD_WAKE_TOKEN": "sandbox-token",
		"PROXCLOUD_FUNCTION_WAKE_URL": api.URL + "/internal/functions", "PROXCLOUD_FUNCTION_WAKE_TOKEN": "fn-token",
	}
	c := &fakeCluster{fnPort: u.Port()}
	h, drain, functions := handler(func(k string) string { return env[k] }, c, slog.New(slog.DiscardHandler))
	if !functions {
		t.Fatal("function mode is off")
	}
	if got := answer(h, fnHost); got != "200 up" {
		t.Errorf("a function host: %s", got)
	}
	if got := answer(h, u.Port()+"-sbx-12345678."+testDomain); got != "200 up" {
		t.Errorf("a sandbox host: %s", got)
	}
	drain() // the last activity report
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("no activity report reached the portal")
	}
	for _, call := range calls {
		if call != "/internal/functions/activity Bearer fn-token" {
			t.Errorf("portal call %q, want the function routes with the function token", call)
		}
	}
}
