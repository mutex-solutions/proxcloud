package pve

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func deadURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return "http://" + l.Addr().String()
}

func TestFailsOverToAnotherEndpointAndRemembersIt(t *testing.T) {
	hits := 0
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"data":{"version":"9.2"}}`))
	}))
	defer live.Close()
	cfg := Config{APIURL: deadURL(t), APIURLs: []string{live.URL}, TokenID: "t", Secret: "s"}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Version(context.Background()); err != nil {
		t.Fatalf("no failover: %v", err)
	}
	// A new client, as every request gets, starts at the one that answered.
	c2, _ := New(cfg)
	if c2.goodIndex() != 1 || c2.BaseURL() != live.URL+"/api2/json" {
		t.Fatalf("goodIndex = %d, base %s", c2.goodIndex(), c2.BaseURL())
	}
	// A refused connection never sent the request, so a mutation may move on too.
	c3, _ := New(Config{APIURL: deadURL(t), APIURLs: []string{live.URL}, TokenID: "t", Secret: "s"})
	if err := c3.post(context.Background(), "/x", nil, nil); err != nil {
		t.Fatalf("post did not fail over: %v", err)
	}
}

func TestMutationIsNotRetriedOnceSent(t *testing.T) {
	second := 0
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	defer broken.Close()
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { second++ }))
	defer other.Close()
	c, _ := New(Config{APIURL: broken.URL, APIURLs: []string{other.URL}, TokenID: "t", Secret: "s"})
	if err := c.post(context.Background(), "/x", nil, nil); err == nil {
		t.Fatal("expected the broken connection to surface")
	}
	if second != 0 {
		t.Fatal("a mutation that may have run was sent again")
	}
	if err := c.get(context.Background(), "/x", nil, nil); err != nil || second != 1 {
		t.Fatalf("a read should fail over: %v, %d", err, second)
	}
}
