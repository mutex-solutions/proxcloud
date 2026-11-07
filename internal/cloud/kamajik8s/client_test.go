package kamajik8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testServer spins a TLS fake apiserver and returns a Client whose kubeconfig trusts it
// (token auth). handler sees every request after the auth assert.
func testServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("missing bearer token, got %q", got)
		}
		handler(w, r)
	}))
	t.Cleanup(ts.Close)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	kubeconfig := fmt.Sprintf(`
apiVersion: v1
kind: Config
current-context: test
clusters:
- name: test
  cluster:
    server: %s
    certificate-authority-data: %s
contexts:
- name: test
  context: {cluster: test, user: test}
users:
- name: test
  user: {token: test-token}
`, ts.URL, base64.StdEncoding.EncodeToString(caPEM))
	c, err := New(kubeconfig)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestKubeconfigErrors(t *testing.T) {
	for name, cfg := range map[string]string{
		"empty":           "",
		"no context":      "current-context: x\nclusters: []\ncontexts: []\nusers: []",
		"no cluster auth": "current-context: c\ncontexts:\n- name: c\n  context: {cluster: k, user: u}\nclusters:\n- name: k\n  cluster: {server: https://x}\nusers:\n- name: u\n  user: {}",
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestApplyAndNotFound(t *testing.T) {
	var gotPath, gotCT, gotQuery string
	var gotBody map[string]any
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch:
			gotPath, gotCT, gotQuery = r.URL.Path, r.Header.Get("Content-Type"), r.URL.RawQuery
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","message":"not here"}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","message":"already gone"}`))
		}
	})
	ctx := context.Background()

	if err := c.EnsureNamespace(ctx, "pc-p1", map[string]string{"a": "b"}); err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	if gotPath != "/api/v1/namespaces/pc-p1" {
		t.Errorf("path = %s", gotPath)
	}
	if gotCT != "application/apply-patch+yaml" {
		t.Errorf("content-type = %s", gotCT)
	}
	if !strings.Contains(gotQuery, "fieldManager=proxcloud") || !strings.Contains(gotQuery, "force=true") {
		t.Errorf("query = %s", gotQuery)
	}
	if gotBody["kind"] != "Namespace" {
		t.Errorf("body kind = %v", gotBody["kind"])
	}

	// GET on an absent object → (nil, nil); DELETE absent → nil (idempotent).
	app, err := c.GetApplication(ctx, "argocd", "nope")
	if err != nil || app != nil {
		t.Errorf("GetApplication absent = %v, %v; want nil, nil", app, err)
	}
	if err := c.DeleteApplication(ctx, "argocd", "nope"); err != nil {
		t.Errorf("DeleteApplication absent: %v", err)
	}
}

func TestApplyApplicationRequiresIdentity(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := c.ApplyApplication(context.Background(), map[string]any{"metadata": map[string]any{}}); err == nil {
		t.Fatal("want error for missing name/namespace")
	}
}

func TestGetSecretDataAndList(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/secrets/"):
			_, _ = fmt.Fprintf(w, `{"data":{"admin.conf":"%s"}}`, base64.StdEncoding.EncodeToString([]byte("kubecfg")))
		case strings.HasSuffix(r.URL.Path, "/applications"):
			if r.URL.Query().Get("labelSelector") != "proxcloud.io/project=p1" {
				t.Errorf("labelSelector = %s", r.URL.Query().Get("labelSelector"))
			}
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"pc-c-1"}},{"metadata":{"name":"pc-c-2"}}]}`))
		}
	})
	ctx := context.Background()

	data, err := c.GetSecretData(ctx, "pc-p1", "x-admin-kubeconfig")
	if err != nil {
		t.Fatalf("GetSecretData: %v", err)
	}
	if string(data["admin.conf"]) != "kubecfg" {
		t.Errorf("secret data = %q", data["admin.conf"])
	}

	apps, err := c.ListApplications(ctx, "argocd", "proxcloud.io/project=p1")
	if err != nil {
		t.Fatalf("ListApplications: %v", err)
	}
	if len(apps) != 2 {
		t.Errorf("apps = %d", len(apps))
	}
}
