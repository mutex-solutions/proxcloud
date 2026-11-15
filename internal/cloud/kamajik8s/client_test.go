package kamajik8s

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestCustomResourceHelpers(t *testing.T) {
	type call struct{ method, path, ct string }
	var calls []call
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, call{r.Method, r.URL.Path, r.Header.Get("Content-Type")})
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","message":"nope"}`))
			return
		}
		_, _ = w.Write([]byte(`{"metadata":{"name":"sbx-1"}}`))
	})
	ctx := context.Background()
	const g, v, pl = "agents.x-k8s.io", "v1beta1", "sandboxes"
	base := "/apis/agents.x-k8s.io/v1beta1/namespaces/sb-p1/sandboxes"

	if err := c.ApplyCR(ctx, g, v, pl, map[string]any{"metadata": map[string]any{"name": "sbx-1"}}); err == nil {
		t.Error("ApplyCR without namespace: want error")
	}
	if err := c.ApplyCR(ctx, g, v, pl, map[string]any{"metadata": map[string]any{"name": "sbx-1", "namespace": "sb-p1"}}); err != nil {
		t.Fatalf("ApplyCR: %v", err)
	}
	if got, err := c.GetCR(ctx, g, v, pl, "sb-p1", "sbx-1"); err != nil || got != nil {
		t.Errorf("GetCR absent = %v, %v; want nil, nil", got, err)
	}
	if _, err := c.MergePatchCR(ctx, g, v, pl, "sb-p1", "sbx-1", map[string]any{"spec": map[string]any{"shutdownTime": "x"}}); err != nil {
		t.Fatalf("MergePatchCR: %v", err)
	}
	if err := c.DeleteCR(ctx, g, v, pl, "sb-p1", "sbx-1"); err != nil {
		t.Fatalf("DeleteCR: %v", err)
	}
	if err := c.ApplyResourceQuota(ctx, map[string]any{"metadata": map[string]any{"name": "q", "namespace": "sb-p1"}}); err != nil {
		t.Fatalf("ApplyResourceQuota: %v", err)
	}
	want := []call{
		{http.MethodPatch, base + "/sbx-1", "application/apply-patch+yaml"},
		{http.MethodGet, base + "/sbx-1", ""},
		{http.MethodPatch, base + "/sbx-1", "application/merge-patch+json"},
		{http.MethodDelete, base + "/sbx-1", ""},
		{http.MethodPatch, "/api/v1/namespaces/sb-p1/resourcequotas/q", "application/apply-patch+yaml"},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
}

func TestApplyClusterScoped(t *testing.T) {
	var paths []string
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.Header.Get("Content-Type") != "application/apply-patch+yaml" ||
			r.URL.Query().Get("fieldManager") != fieldManager || r.URL.Query().Get("force") != "true" {
			t.Errorf("request = %s %s ?%s", r.Method, r.Header.Get("Content-Type"), r.URL.RawQuery)
		}
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	})
	ctx := context.Background()
	crb := map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
		"metadata": map[string]any{"name": "argocd-manager"}}
	if err := c.ApplyClusterScoped(ctx, "rbac.authorization.k8s.io", "v1", "clusterrolebindings", crb); err != nil {
		t.Fatalf("ApplyClusterScoped: %v", err)
	}
	if err := c.ApplyClusterScoped(ctx, "", "v1", "persistentvolumes", map[string]any{"metadata": map[string]any{"name": "pv-1"}}); err != nil {
		t.Fatalf("ApplyClusterScoped core: %v", err)
	}
	for _, bad := range []map[string]any{
		{"metadata": map[string]any{}},
		{"metadata": map[string]any{"name": "x", "namespace": "kube-system"}},
	} {
		if err := c.ApplyClusterScoped(ctx, "rbac.authorization.k8s.io", "v1", "clusterroles", bad); err == nil {
			t.Errorf("ApplyClusterScoped(%v): want error", bad)
		}
	}
	want := []string{"/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/argocd-manager", "/api/v1/persistentvolumes/pv-1"}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

func TestCreateServiceAccountToken(t *testing.T) {
	var body map[string]any
	reply := `{"status":{"token":"sa-token","expirationTimestamp":"2027-11-15T10:00:00Z"}}`
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/namespaces/kube-system/serviceaccounts/argocd-manager/token" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(reply))
	})
	ctx := context.Background()
	tok, exp, err := c.CreateServiceAccountToken(ctx, "kube-system", "argocd-manager", 365*24*time.Hour)
	if err != nil {
		t.Fatalf("CreateServiceAccountToken: %v", err)
	}
	if tok != "sa-token" || !exp.Equal(time.Date(2027, 11, 15, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("token, expiry = %q, %v", tok, exp)
	}
	spec, _ := body["spec"].(map[string]any)
	if body["kind"] != "TokenRequest" || body["apiVersion"] != "authentication.k8s.io/v1" || spec["expirationSeconds"] != float64(31536000) {
		t.Errorf("body = %v", body)
	}
	for _, r := range []string{
		`{"status":{"expirationTimestamp":"2027-11-15T10:00:00Z"}}`, // no token
		`{"status":{"token":"t","expirationTimestamp":"soon"}}`,     // bad expiry
		`{"status":{"token":"t"}}`,                                  // no expiry
	} {
		reply = r
		if _, _, err := c.CreateServiceAccountToken(ctx, "kube-system", "argocd-manager", time.Hour); err == nil {
			t.Errorf("reply %s: want error", r)
		}
	}
}

func TestCanI(t *testing.T) {
	reply := `{"status":{"allowed":true}}`
	var attrs map[string]any
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Kind string
			Spec struct {
				ResourceAttributes map[string]any `json:"resourceAttributes"`
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Kind != "SelfSubjectAccessReview" {
			t.Errorf("kind = %q", body.Kind)
		}
		attrs = body.Spec.ResourceAttributes
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(reply))
	})
	ctx := context.Background()
	ok, err := c.CanI(ctx, AccessCheck{Verb: "*", Group: "*", Resource: "*"})
	if err != nil || !ok {
		t.Fatalf("CanI cluster-admin = %v, %v", ok, err)
	}
	if fmt.Sprint(attrs) != "map[group:* resource:* verb:*]" {
		t.Errorf("attrs = %v", attrs)
	}
	reply = `{"status":{"allowed":false}}`
	ok, err = c.CanI(ctx, AccessCheck{Namespace: "proxcloud-system", Verb: "get", Resource: "secrets", Name: "t"})
	if err != nil || ok {
		t.Fatalf("CanI denied = %v, %v", ok, err)
	}
	if fmt.Sprint(attrs) != "map[name:t namespace:proxcloud-system resource:secrets verb:get]" {
		t.Errorf("attrs = %v", attrs)
	}
	// A 2xx answer without a verdict is a denial, not an error and never an allow.
	for _, r := range []string{`{}`, `{"status":{}}`, ``} {
		reply = r
		if ok, err := c.CanI(ctx, AccessCheck{Verb: "*", Group: "*", Resource: "*"}); err != nil || ok {
			t.Errorf("reply %q: CanI = %v, %v; want false, nil", r, ok, err)
		}
	}
}

func TestBuildKubeconfig(t *testing.T) {
	hits := 0
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if got := r.Header.Get("Authorization"); got != "Bearer sa-token" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(ts.Close)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})

	kc, err := BuildKubeconfig("pc-platform-seed", ts.URL, caPEM, "sa-token")
	if err != nil {
		t.Fatalf("BuildKubeconfig: %v", err)
	}
	if strings.Contains(kc, "client-") || !strings.Contains(kc, "current-context: pc-platform-seed") {
		t.Errorf("kubeconfig:\n%s", kc)
	}
	c, err := New(kc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.ListNodes(context.Background()); err != nil || hits != 1 {
		t.Fatalf("ListNodes = %v, hits %d", err, hits)
	}

	// Another CA: the handshake fails before any request, so the token never leaves.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other"}, NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	other, _ := BuildKubeconfig("x", ts.URL, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "sa-token")
	oc, err := New(other)
	if err != nil {
		t.Fatalf("New other: %v", err)
	}
	if _, err := oc.ListNodes(context.Background()); !errors.Is(err, ErrUnreachable) || hits != 1 {
		t.Errorf("wrong CA: err = %v, hits %d", err, hits)
	}

	// Only the certificate blocks are written: text around them, even a broken key, stays out.
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	wrapped, err := BuildKubeconfig("x", ts.URL, []byte("subject=CN=seed\n"+string(caPEM)+string(keyPEM[:40])), "t")
	if err != nil {
		t.Fatalf("CA with text around it: %v", err)
	}
	if !strings.Contains(wrapped, base64.StdEncoding.EncodeToString(caPEM)+"\n") {
		t.Errorf("CA with text around it: want only the certificate, got\n%s", wrapped)
	}

	for name, args := range map[string][]string{
		"CA with a key block": {"x", ts.URL, string(caPEM) + string(keyPEM), "t"},
		"bad certificate":     {"x", ts.URL, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")})), "t"},
		"http server":         {"x", "http://10.0.0.1:6443", string(caPEM), "t"},
		"no host":             {"x", "https://", string(caPEM), "t"},
		"no CA":               {"x", ts.URL, "", "t"},
		"garbage CA":          {"x", ts.URL, "not pem", "t"},
		"no token":            {"x", ts.URL, string(caPEM), ""},
		"no name":             {"", ts.URL, string(caPEM), "t"},
	} {
		if _, err := BuildKubeconfig(args[0], args[1], []byte(args[2]), args[3]); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
