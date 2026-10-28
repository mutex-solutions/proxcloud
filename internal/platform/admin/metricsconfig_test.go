package admin

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// applyMetrics drives the named apply closure the way serviceFieldSet would (body → doc
// mutation), without the repo. nil esSvc → setSecretLeaf stores plaintext (same fallback the
// connection-save path uses), which is exactly what these at-rest assertions want to see.
func applyMetrics(t *testing.T, doc pgdoc.M, body string) *httpx.HTTPError {
	t.Helper()
	req := httptest.NewRequest("PUT", "/service/svc-1/metrics-config", strings.NewReader(body))
	return (&Handler{}).applyMetricsConfig(req, doc)
}

func promCfgOf(t *testing.T, doc pgdoc.M) map[string]any {
	t.Helper()
	cfg, _ := doc["config"].(pgdoc.M)
	m, _ := cfg["metrics"].(pgdoc.M)
	p, _ := m["prometheus"].(map[string]any)
	if p == nil {
		if pm, ok := m["prometheus"].(pgdoc.M); ok {
			p = pm
		}
	}
	return p
}

// TestMetricsConfigMergeSemantics: a source-only toggle must NOT wipe the stored prometheus
// connection config — switching none→prometheus and back keeps the URL.
func TestMetricsConfigMergeSemantics(t *testing.T) {
	doc := pgdoc.M{"config": pgdoc.M{"metrics": map[string]any{
		"source":     "prometheus",
		"prometheus": map[string]any{"url": "https://mimir.example/prometheus", "schema": "libvirt-exporter"},
	}}}
	if err := applyMetrics(t, doc, `{"source":"none"}`); err != nil {
		t.Fatalf("toggle to none: %v", err)
	}
	if p := promCfgOf(t, doc); p["url"] != "https://mimir.example/prometheus" {
		t.Fatalf("stored prometheus config wiped by source toggle: %#v", p)
	}
	if err := applyMetrics(t, doc, `{"source":"prometheus"}`); err != nil {
		t.Fatalf("toggle back to prometheus with stored url must pass: %v", err)
	}
}

// TestMetricsConfigValidationMatrix: every save-time guard.
func TestMetricsConfigValidationMatrix(t *testing.T) {
	cases := []struct {
		name, body, wantErr string
	}{
		{"bad-source", `{"source":"graphite"}`, "source must be one of"},
		{"prometheus-without-url", `{"source":"prometheus"}`, "url is required"},
		{"non-http-url", `{"source":"prometheus","prometheus":{"url":"mimir.example"}}`, "http(s)"},
		{"userinfo-url", `{"source":"prometheus","prometheus":{"url":"https://u:p@mimir.example"}}`, "prometheusAuth"},
		{"authorization-header", `{"source":"prometheus","prometheus":{"url":"https://m.example","headers":{"authorization":"Bearer x"}}}`, "prometheusAuth"},
		{"bad-schema", `{"source":"prometheus","prometheus":{"url":"https://m.example","schema":"nope"}}`, "schema must be one of"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := applyMetrics(t, pgdoc.M{}, c.body)
			if err == nil || !strings.Contains(err.Msg, c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
	// The happy path with a full block.
	doc := pgdoc.M{}
	if err := applyMetrics(t, doc,
		`{"source":"prometheus","prometheus":{"url":"https://m.example/prometheus","schema":"libvirt-exporter","headers":{"X-Scope-OrgID":"tenant-a"}}}`); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	p := promCfgOf(t, doc)
	if p["url"] != "https://m.example/prometheus" {
		t.Fatalf("url not stored: %#v", p)
	}
	if hs, _ := p["headers"].(pgdoc.M); hs["X-Scope-OrgID"] != "tenant-a" {
		t.Fatalf("tenant header not stored: %#v", p["headers"])
	}
}

// TestMetricsConfigSecretLeaves: blank keeps, value sets, "-" clears — on the encrypted
// secret leaves (plaintext here because esSvc is nil; the encrypt branch is the same
// EncryptSecret call every other secret-writing PUT uses).
func TestMetricsConfigSecretLeaves(t *testing.T) {
	doc := pgdoc.M{"config": pgdoc.M{"metrics": map[string]any{
		"source": "none", "prometheus": map[string]any{"url": "https://m.example"},
	}}}
	if err := applyMetrics(t, doc, `{"source":"none","prometheusAuth":{"bearerToken":"tok1"}}`); err != nil {
		t.Fatalf("set token: %v", err)
	}
	secret := doc["secret"].(pgdoc.M)
	if secret["prometheusBearerToken"] != "tok1" {
		t.Fatalf("token not stored: %#v", secret)
	}
	if _, ok := secret["prometheusBasicPassword"]; ok {
		t.Fatalf("blank password must not be written")
	}
	if err := applyMetrics(t, doc, `{"source":"none","prometheusAuth":{"bearerToken":""}}`); err != nil {
		t.Fatalf("blank keep: %v", err)
	}
	if secret["prometheusBearerToken"] != "tok1" {
		t.Fatalf("blank must keep the stored token: %#v", secret)
	}
	if err := applyMetrics(t, doc, `{"source":"none","prometheusAuth":{"bearerToken":"-"}}`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := secret["prometheusBearerToken"]; ok {
		t.Fatalf("\"-\" must clear the stored token: %#v", secret)
	}
}
