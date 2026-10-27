package sse

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestStreamAuthGate pins that the stream requires an authenticated principal and enforces project
// membership before subscribing: no principal → 401; a non-member → 403; neither subscribes.
func TestStreamAuthGate(t *testing.T) {
	pool := NewPool()
	h := NewHandler(pool)
	h.SetMembership(func(userID, projectID string) bool { return userID == "member" && projectID == "projX" })
	r := chi.NewRouter()
	h.Routes(r)

	do := func(rc *httpx.RequestContext) int {
		req := httptest.NewRequest(http.MethodGet, "/events/projX", nil)
		if rc != nil {
			req = req.WithContext(httpx.WithRC(req.Context(), rc))
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := do(nil); got != http.StatusUnauthorized {
		t.Errorf("no principal: want 401, got %d", got)
	}
	if got := do(&httpx.RequestContext{Sub: "intruder"}); got != http.StatusForbidden {
		t.Errorf("non-member: want 403, got %d", got)
	}
	if pool.Count() != 0 {
		t.Errorf("denied requests must not subscribe: pool count = %d", pool.Count())
	}
}

func TestPoolNotifyMatchesProjectAndUser(t *testing.T) {
	p := NewPool()
	a := p.add("proj1", "subA")
	b := p.add("proj2", "subB")

	p.Notify(SseData{Type: "x", ProjectID: "proj1"})
	if !recv(a) {
		t.Fatal("subA (proj1) should receive a proj1 event")
	}
	if recvEmpty(b) {
		t.Fatal("subB (proj2) should NOT receive a proj1 event")
	}

	p.Notify(SseData{Type: "x", UserID: "subB"})
	if !recv(b) {
		t.Fatal("subB should receive a userId-targeted event")
	}

	p.remove(a.id)
	p.remove(b.id)
	if p.Count() != 0 {
		t.Fatalf("count after remove = %d, want 0", p.Count())
	}
}

func recv(s *subscriber) bool {
	select {
	case <-s.ch:
		return true
	case <-time.After(time.Second):
		return false
	}
}
func recvEmpty(s *subscriber) bool {
	select {
	case <-s.ch:
		return true
	default:
		return false
	}
}

// TestStreamReceivesSyntheticEvent is the build-ahead verification: open the SSE stream, push a
// synthetic event via the Pool (what the os-notification source will do), and assert the client
// receives the framed event. Proves the end-to-end stream wiring without the real source.
func TestStreamReceivesSyntheticEvent(t *testing.T) {
	pool := NewPool()
	r := chi.NewRouter()
	// Inject an authenticated principal (the stream now requires one; no membership checker wired
	// → allowed).
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "u1"})))
		})
	})
	NewHandler(pool).Routes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events/proj1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("bad stream resp: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// wait for the subscriber to register, then push the synthetic event.
	for i := 0; i < 100 && pool.Count() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	pool.Notify(SseData{Type: "cloud_resource", ProjectID: "proj1", Data: map[string]any{"id": "r1"}})

	// read until the event frame arrives (ctx timeout aborts the read if it never comes).
	reader := bufio.NewReader(resp.Body)
	var gotEvent, gotData bool
	for !(gotEvent && gotData) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream (event=%v data=%v): %v", gotEvent, gotData, err)
		}
		if strings.TrimSpace(line) == "event: cloud_resource" {
			gotEvent = true
		}
		if gotEvent && strings.HasPrefix(line, "data: ") && strings.Contains(line, `"projectId":"proj1"`) {
			gotData = true
		}
	}
}
