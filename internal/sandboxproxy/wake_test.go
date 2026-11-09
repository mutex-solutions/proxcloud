package sandboxproxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// wakeAPI fakes the portal's wake endpoint: answer status after delay, counting calls.
func wakeAPI(t *testing.T, status int, delay time.Duration, onWake func()) (*httptest.Server, *atomic.Int32, *sync.Map) {
	t.Helper()
	var n atomic.Int32
	var bodies sync.Map
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		n.Add(1)
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies.Store(r.URL.Path, b)
		time.Sleep(delay)
		if status == 200 && onWake != nil {
			onWake()
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"errors":{"code":403,"message":"m"}}`)) // the portal envelope
	}))
	t.Cleanup(s.Close)
	return s, &n, &bodies
}

func TestWakeResumesThenForwards(t *testing.T) {
	_, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hi")) }))
	var up atomic.Bool
	lk := func(context.Context, string) (Pod, error) {
		if up.Load() {
			return Pod{IP: "127.0.0.1"}, nil
		}
		return Pod{}, ErrNotFound
	}
	api, calls, bodies := wakeAPI(t, 200, 200*time.Millisecond, func() { up.Store(true) })
	var forgot atomic.Int32
	s := httptest.NewServer(WakingHandler(dom, lk, NewWaker(api.URL+"/internal/sandboxes", "tok"), func(string) { forgot.Add(1) }, slog.Default()))
	defer s.Close()

	// Ten concurrent requests: one wake call, every request forwarded.
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := get(t, s, strconv.Itoa(port)+"-sbx-1a2b3c4d."+dom, "/", map[string]string{TrafficTokenHeader: "tt"})
			if code != 200 || body != "hi" {
				t.Errorf("%d %s", code, body)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 || forgot.Load() == 0 {
		t.Fatalf("wake calls = %d, forgets = %d", calls.Load(), forgot.Load())
	}
	b, _ := bodies.Load("/internal/sandboxes/sbx-1a2b3c4d/wake")
	if m := b.(map[string]any); m["trafficAccessToken"] != "tt" || m["port"] != float64(port) {
		t.Fatalf("body %v", m)
	}
}

func TestWakeAnswers(t *testing.T) {
	lk := func(context.Context, string) (Pod, error) { return Pod{}, ErrNotFound }
	for _, tc := range []struct {
		api, want int
		retry     bool
	}{{404, 502, false}, {403, 403, false}, {409, 503, true}, {429, 503, true}, {500, 503, true}} {
		api, calls, _ := wakeAPI(t, tc.api, 0, nil)
		s := httptest.NewServer(WakingHandler(dom, lk, NewWaker(api.URL, "tok"), nil, slog.New(slog.NewTextHandler(io.Discard, nil))))
		req, _ := http.NewRequest("GET", s.URL, nil)
		req.Host = "49983-sbx-1a2b3c4d." + dom
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if tc.api == 403 && !strings.Contains(string(body), `"message":"m"`) {
			t.Errorf("the portal's refusal must reach the client: %s", body)
		}
		if res.StatusCode != tc.want || (res.Header.Get("Retry-After") != "") != tc.retry {
			t.Errorf("api %d: got %d retry=%q", tc.api, res.StatusCode, res.Header.Get("Retry-After"))
		}
		if tc.api == 404 { // not resumable: cached, the next request does not call the API
			res, _ = http.DefaultClient.Do(req)
			res.Body.Close()
			if calls.Load() != 1 {
				t.Errorf("404 not cached: %d calls", calls.Load())
			}
		}
		s.Close()
	}
}

func TestWakeSlowAnswers503WhileWakeContinues(t *testing.T) {
	lk := func(context.Context, string) (Pod, error) { return Pod{}, ErrNotFound }
	done := make(chan struct{})
	api, _, _ := wakeAPI(t, 200, 300*time.Millisecond, func() { close(done) })
	wk := NewWaker(api.URL, "tok")
	wk.Wait = 50 * time.Millisecond
	s := httptest.NewServer(WakingHandler(dom, lk, wk, nil, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer s.Close()
	code, _ := get(t, s, "49983-sbx-1a2b3c4d."+dom, "/", nil)
	if code != 503 {
		t.Fatalf("got %d", code)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the wake did not carry on after the client got its 503")
	}
}

func TestNoWakerKeeps502(t *testing.T) {
	if NewWaker("", "tok") != nil || NewWaker("http://x", "") != nil {
		t.Fatal("wake must be off without url and token")
	}
	s := start(t, "")
	if code, _ := get(t, s, "49983-sbx-00000000."+dom, "/", nil); code != 502 {
		t.Fatalf("got %d", code)
	}
}
