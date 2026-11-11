package sandboxproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

const privatePortTestID = "sbx-12345678"

// privatePortPod uses the production builder and the same JSON representation
// that K8sLookup receives. Only status and a synthetic private-traffic token are
// supplied by the fixture; no Kubernetes API or real sandbox is involved.
func privatePortPod(t *testing.T, envd bool, token string) map[string]any {
	t.Helper()
	cfg := sandbox.Config{Envd: envd, Templates: map[string]sandbox.Template{
		"proxy-test": {Image: "example.invalid/test:local", Command: []string{"sleep", "infinity"}},
	}}
	spec := sandbox.Spec{ID: privatePortTestID, ProjectID: "proxy-test", Template: "proxy-test"}
	if err := spec.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	cr := sandbox.BuildCR(cfg, "proxy-service", spec, time.Unix(1800000000, 0).UTC())
	b, err := json.Marshal(cr["spec"].(map[string]any)["podTemplate"])
	if err != nil {
		t.Fatal(err)
	}
	var pod map[string]any
	if err := json.Unmarshal(b, &pod); err != nil {
		t.Fatal(err)
	}
	pod["metadata"].(map[string]any)["annotations"].(map[string]any)[sandbox.AnnotationTrafficToken] = token
	pod["status"] = map[string]any{
		"phase": "Running", "podIP": "127.0.0.1",
		"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
	}
	return pod
}

func TestK8sLookupSurfacesProductionPodMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		envd    bool
		legacy  bool
		utility bool
	}{
		{name: "native"},
		{name: "envd explicit serve", envd: true},
		{name: "envd legacy command", envd: true, legacy: true},
		{name: "helper utility is not envd", utility: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := privatePortPod(t, tc.envd || tc.utility, "fixture-traffic-token")
			main := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			if tc.legacy {
				delete(main, "args")
			}
			if tc.utility {
				main["args"] = []any{"-ready"}
			}
			got, err := K8sLookup(fakeLister{items: []map[string]any{pod}})(context.Background(), privatePortTestID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Envd != tc.envd || got.IP != "127.0.0.1" || got.TrafficToken != "fixture-traffic-token" {
				t.Fatalf("K8sLookup = %+v; want envd=%v, fixture IP and traffic token", got, tc.envd)
			}
		})
	}
}

// Count connections as well as requests, so denial tests verify that auth runs
// before even opening an upstream connection. Every listener belongs to this test.
type privatePortListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *privatePortListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

func privatePortUpstream(t *testing.T, port int) (*privatePortListener, *atomic.Int64) {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		// Never fall back to an existing service or silently skip the reserved port.
		t.Fatalf("cannot bind own loopback fixture on port %d: %v", port, err)
	}
	counting := &privatePortListener{Listener: l}
	requests := new(atomic.Int64)
	u := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/own-auth" && r.Header.Get(EnvdTokenHeader) != "fixture-envd-token" {
			w.Header().Set("X-Fixture-Upstream", "reached")
			http.Error(w, "fixture envd token required", http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Fixture-Upstream", "reached")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "fixture upstream ok")
	}))
	// NewUnstartedServer reserves an ephemeral loopback listener; replace it with
	// our explicitly owned port before Start. No arbitrary endpoint is contacted.
	if err := u.Listener.Close(); err != nil {
		_ = l.Close()
		t.Fatal(err)
	}
	u.Listener = counting
	u.Start()
	t.Cleanup(u.Close)
	return counting, requests
}

func privatePortRequest(t *testing.T, h http.Handler, port int, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "http://proxy.invalid"+path, nil).WithContext(ctx)
	r.Host = fmt.Sprintf("%d-%s.%s", port, privatePortTestID, dom)
	r.Close = true
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHandlerPrivateNativeEnvdNumberedPortRequiresTrafficToken(t *testing.T) {
	listener, requests := privatePortUpstream(t, EnvdPort)
	h := Handler(dom, K8sLookup(fakeLister{items: []map[string]any{
		privatePortPod(t, false, "fixture-traffic-token"),
	}}), slog.New(slog.NewTextHandler(io.Discard, nil)))

	for _, tc := range []struct {
		name    string
		headers map[string]string
		message string
	}{
		{name: "missing", message: "is missing"},
		{name: "wrong", headers: map[string]string{TrafficTokenHeader: "wrong-fixture-token"}, message: "is invalid"},
		{name: "envd token is not traffic auth", headers: map[string]string{EnvdTokenHeader: "fixture-envd-token"}, message: "is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := privatePortRequest(t, h, EnvdPort, "/application", tc.headers)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), tc.message) {
				t.Fatalf("private native :49983 denial = %d %s", w.Code, w.Body.String())
			}
			if requests.Load() != 0 || listener.accepted.Load() != 0 {
				t.Fatalf("denied request reached upstream: requests=%d connections=%d", requests.Load(), listener.accepted.Load())
			}
		})
	}
	w := privatePortRequest(t, h, EnvdPort, "/application", map[string]string{TrafficTokenHeader: "fixture-traffic-token"})
	if w.Code != http.StatusOK || w.Body.String() != "fixture upstream ok" || requests.Load() != 1 {
		t.Fatalf("correct traffic token did not reach own fixture: %d %s requests=%d", w.Code, w.Body.String(), requests.Load())
	}
}

func TestHandlerEnvdPortRetainsUpstreamOwnAuthAndPublicNativeCompatibility(t *testing.T) {
	_, requests := privatePortUpstream(t, EnvdPort)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	envd := Handler(dom, K8sLookup(fakeLister{items: []map[string]any{
		privatePortPod(t, true, "fixture-traffic-token"),
	}}), log)
	for _, tc := range []struct {
		name    string
		headers map[string]string
		status  int
	}{
		{name: "missing envd token forwards to own auth", status: http.StatusUnauthorized},
		{name: "wrong envd token forwards to own auth", headers: map[string]string{EnvdTokenHeader: "wrong-fixture-token"}, status: http.StatusUnauthorized},
		{name: "own auth succeeds without traffic token", headers: map[string]string{EnvdTokenHeader: "fixture-envd-token"}, status: http.StatusOK},
		{name: "traffic token does not replace envd token", headers: map[string]string{TrafficTokenHeader: "fixture-traffic-token"}, status: http.StatusUnauthorized},
		{name: "wrong traffic token does not interfere with envd", headers: map[string]string{EnvdTokenHeader: "fixture-envd-token", TrafficTokenHeader: "wrong-fixture-token"}, status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := requests.Load()
			w := privatePortRequest(t, envd, EnvdPort, "/own-auth", tc.headers)
			if w.Code != tc.status || w.Header().Get("X-Fixture-Upstream") != "reached" || requests.Load() != before+1 {
				t.Fatalf("envd own-auth forwarding = %d %s requests=%d", w.Code, w.Body.String(), requests.Load()-before)
			}
		})
	}
	public := Handler(dom, K8sLookup(fakeLister{items: []map[string]any{
		privatePortPod(t, false, ""),
	}}), log)
	before := requests.Load()
	w := privatePortRequest(t, public, EnvdPort, "/application", nil)
	if w.Code != http.StatusOK || w.Body.String() != "fixture upstream ok" || requests.Load() != before+1 {
		t.Fatalf("public native :49983 compatibility = %d %s", w.Code, w.Body.String())
	}
}

func TestHandlerOrdinaryPortRequiresTrafficTokenInBothModes(t *testing.T) {
	listener, requests := privatePortUpstream(t, 0)
	port := listener.Addr().(*net.TCPAddr).Port
	if port == EnvdPort {
		t.Fatal("ephemeral fixture unexpectedly selected envd port")
	}
	for _, envd := range []bool{false, true} {
		t.Run(fmt.Sprintf("envd=%v", envd), func(t *testing.T) {
			h := Handler(dom, K8sLookup(fakeLister{items: []map[string]any{
				privatePortPod(t, envd, "fixture-traffic-token"),
			}}), slog.New(slog.NewTextHandler(io.Discard, nil)))
			for _, headers := range []map[string]string{nil, {TrafficTokenHeader: "wrong-fixture-token"}, {EnvdTokenHeader: "fixture-envd-token"}} {
				beforeRequests, beforeConnections := requests.Load(), listener.accepted.Load()
				w := privatePortRequest(t, h, port, "/init", headers)
				if w.Code != http.StatusForbidden || requests.Load() != beforeRequests || listener.accepted.Load() != beforeConnections {
					t.Fatalf("ordinary port denial = %d %s; upstream changed", w.Code, w.Body.String())
				}
			}
			before := requests.Load()
			// The control-path restriction is specific to :49983, not app ports.
			w := privatePortRequest(t, h, port, "/init", map[string]string{TrafficTokenHeader: "fixture-traffic-token"})
			if w.Code != http.StatusOK || w.Body.String() != "fixture upstream ok" || requests.Load() != before+1 {
				t.Fatalf("ordinary port authorized forwarding = %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandlerControlPathsRemainBlockedInBothModes(t *testing.T) {
	for _, envd := range []bool{false, true} {
		for _, private := range []bool{false, true} {
			t.Run(fmt.Sprintf("envd=%v/private=%v", envd, private), func(t *testing.T) {
				token := ""
				if private {
					token = "fixture-traffic-token"
				}
				actual := K8sLookup(fakeLister{items: []map[string]any{privatePortPod(t, envd, token)}})
				lookups := 0
				h := Handler(dom, func(ctx context.Context, id string) (Pod, error) {
					lookups++
					return actual(ctx, id)
				}, slog.New(slog.NewTextHandler(io.Discard, nil)))
				for _, path := range []string{"/init", "/freeze", "/unfreeze", "/collapse", "/fsfreeze", "/fsthaw", "//init", "/init/", "/./init", "/INIT", "/x/../init"} {
					w := privatePortRequest(t, h, EnvdPort, path, map[string]string{
						TrafficTokenHeader: "fixture-traffic-token", EnvdTokenHeader: "fixture-envd-token",
					})
					if w.Code != http.StatusNotFound || lookups != 0 {
						t.Fatalf("control path %q = %d; lookups=%d", path, w.Code, lookups)
					}
				}
			})
		}
	}
}
