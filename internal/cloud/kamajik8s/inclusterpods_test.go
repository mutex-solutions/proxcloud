package kamajik8s

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestListPodsAllNamespacesAndInCluster(t *testing.T) {
	var gotPath, gotSel, gotAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotSel, gotAuth = r.URL.Path, r.URL.Query().Get("labelSelector"), r.Header.Get("Authorization")
		w.Write([]byte(`{"items":[{"metadata":{"name":"p"}}]}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(dir+"/ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	os.WriteFile(dir+"/token", []byte("tok1\n"), 0o600)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	c, err := newInCluster(dir, host, port)
	if err != nil {
		t.Fatal(err)
	}
	items, err := c.ListPodsAllNamespaces(context.Background(), "a=b")
	if err != nil || len(items) != 1 || gotPath != "/api/v1/pods" || gotSel != "a=b" || gotAuth != "Bearer tok1" {
		t.Fatalf("items=%v err=%v path=%s sel=%s auth=%s", items, err, gotPath, gotSel, gotAuth)
	}
	os.WriteFile(dir+"/token", []byte("tok2"), 0o600)
	c.rc.readAt = time.Now().Add(-2 * time.Minute)
	c.ListPodsAllNamespaces(context.Background(), "")
	if gotAuth != "Bearer tok2" {
		t.Fatalf("token not refreshed: %s", gotAuth)
	}
}
