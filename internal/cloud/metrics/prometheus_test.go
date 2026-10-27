package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// promVector renders a Prometheus instant-query success envelope with the given
// (labels, value) samples.
func promVector(samples ...map[string]any) string {
	result := make([]map[string]any, 0, len(samples))
	for _, s := range samples {
		labels := map[string]string{}
		for k, v := range s {
			if k != "__value__" {
				labels[k] = v.(string)
			}
		}
		result = append(result, map[string]any{
			"metric": labels,
			"value":  []any{1780000000.0, s["__value__"].(string)},
		})
	}
	b, _ := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	return string(b)
}

// fakeProm dispatches on substrings of the PromQL query. It also records every request's
// headers for the auth assertions — written from the handler goroutine, read from the test
// goroutine, so access goes through the mutex.
type fakeProm struct {
	t       *testing.T
	respond func(query string) string

	mu       sync.Mutex
	lastHead http.Header
	lastPath string
}

func (f *fakeProm) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			f.t.Errorf("parse form: %v", err)
			return
		}
		f.mu.Lock()
		f.lastHead = r.Header.Clone()
		f.lastPath = r.URL.Path
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, f.respond(r.PostFormValue("query")))
	}
}

func (f *fakeProm) head() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastHead
}

func (f *fakeProm) path() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPath
}

// TestPrometheusLibvirtSchema drives the full MeasureFetcher contract against a faked
// libvirt-exporter layout: nova-uuid → domain join, target_device discovery, per-direction
// selectors, and the summed max−min → MB math.
func TestPrometheusLibvirtSchema(t *testing.T) {
	f := &fakeProm{t: t, respond: func(q string) string {
		switch {
		case strings.Contains(q, "libvirt_domain_openstack_info"):
			if !strings.Contains(q, `instance_id="uuid-1"`) {
				t.Fatalf("domain query missing instance_id selector: %s", q)
			}
			// Two hosts for the same domain = a live-migrated instance; both must be pinned.
			return promVector(
				map[string]any{"domain": "instance-0001", "instance": "host-a:9177", "__value__": "1"},
				map[string]any{"domain": "instance-0001", "instance": "host-b:9177", "__value__": "1"},
			)
		case strings.Contains(q, "count by (target_device)"):
			if !strings.Contains(q, `domain=~"^(?:instance-0001)$"`) {
				t.Fatalf("device query missing domain selector: %s", q)
			}
			if !strings.Contains(q, `instance=~"^(?:host-a:9177|host-b:9177)$"`) {
				t.Fatalf("device query missing host pin: %s", q)
			}
			return promVector(
				map[string]any{"target_device": "tapA", "__value__": "1"},
				map[string]any{"target_device": "tapB", "__value__": "1"},
			)
		case strings.Contains(q, "max_over_time"):
			// 10 MiB for receive selectors, 5 MiB for transmit.
			if strings.Contains(q, "receive_bytes_total") {
				return promVector(map[string]any{"__value__": "10485760"})
			}
			return promVector(map[string]any{"__value__": "5242880"})
		default:
			t.Fatalf("unexpected query: %s", q)
			return ""
		}
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p, err := NewPrometheus(PrometheusConfig{URL: srv.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	p.WithNow(func() time.Time { return now })

	ifaces, err := p.SearchInstanceInterfaces(context.Background(), "uuid-1")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(ifaces) != 2 || ifaces[0].Name != "tapA" || ifaces[1].Name != "tapB" {
		t.Fatalf("unexpected interfaces: %+v", ifaces)
	}
	inRef := ifaces[0].Metrics["network.incoming.bytes"]
	outRef := ifaces[0].Metrics["network.outgoing.bytes"]
	if !strings.Contains(inRef, "receive_bytes_total") || !strings.Contains(outRef, "transmit_bytes_total") {
		t.Fatalf("direction mapping wrong: in=%s out=%s", inRef, outRef)
	}
	if !strings.Contains(inRef, `target_device="tapA"`) {
		t.Fatalf("selector missing device pin: %s", inRef)
	}
	if !strings.Contains(inRef, `instance=~"^(?:host-a:9177|host-b:9177)$"`) {
		t.Fatalf("selector missing host pin (cross-cell collision guard): %s", inRef)
	}

	monthStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	in, err := p.MeasuresMBForCurrentMonth(context.Background(), inRef, 0, monthStart)
	if err != nil {
		t.Fatalf("measures in: %v", err)
	}
	out, err := p.MeasuresMBForCurrentMonth(context.Background(), outRef, 0, monthStart)
	if err != nil {
		t.Fatalf("measures out: %v", err)
	}
	if in.String() != "10" || out.String() != "5" {
		t.Fatalf("MB math wrong: in=%s out=%s", in, out)
	}
}

// TestPrometheusCeilometerSchemas checks discovery + tap extraction for the two ceilometer
// layouts (bare pushgateway names with resource_id, prefixed exporter names with resource).
func TestPrometheusCeilometerSchemas(t *testing.T) {
	cases := []struct {
		schema, metric, label string
	}{
		{PromSchemaCeilometerPushgw, "network_incoming_bytes", "resource_id"},
		{PromSchemaCeilometerExporter, "ceilometer_network_incoming_bytes", "resource"},
	}
	for _, c := range cases {
		t.Run(c.schema, func(t *testing.T) {
			rid := "instance-00000002-uuid-9-tapab12cd34-56"
			f := &fakeProm{t: t, respond: func(q string) string {
				if !strings.Contains(q, c.metric) || !strings.Contains(q, c.label+"=~") || !strings.Contains(q, "uuid-9") {
					t.Fatalf("unexpected discovery query: %s", q)
				}
				return promVector(map[string]any{c.label: rid, "__value__": "1"})
			}}
			srv := httptest.NewServer(f.handler())
			defer srv.Close()
			p, err := NewPrometheus(PrometheusConfig{URL: srv.URL, Schema: c.schema})
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			ifaces, err := p.SearchInstanceInterfaces(context.Background(), "uuid-9")
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(ifaces) != 1 {
				t.Fatalf("expected 1 interface, got %+v", ifaces)
			}
			if ifaces[0].Name != "tapab12cd34-56" {
				t.Fatalf("tap extraction wrong: %q", ifaces[0].Name)
			}
			if !strings.Contains(ifaces[0].Metrics["network.incoming.bytes"], fmt.Sprintf("%s{%s=%q}", c.metric, c.label, rid)) {
				t.Fatalf("selector wrong: %s", ifaces[0].Metrics["network.incoming.bytes"])
			}
		})
	}
}

// TestPrometheusAuthHeaders asserts each auth mode + extra headers (the Mimir X-Scope-OrgID
// case) and the path prefix land on the wire.
func TestPrometheusAuthHeaders(t *testing.T) {
	f := &fakeProm{t: t, respond: func(string) string {
		return promVector(map[string]any{"__value__": "1"})
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	t.Run("basic+tenant-header+prefix", func(t *testing.T) {
		p, err := NewPrometheus(PrometheusConfig{
			URL:       srv.URL + "/prometheus",
			BasicUser: "u", BasicPassword: "pw",
			Headers: map[string]string{"X-Scope-OrgID": "openstack"},
		})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		if err := p.Ping(context.Background()); err != nil {
			t.Fatalf("ping: %v", err)
		}
		if got := f.path(); got != "/prometheus/api/v1/query" {
			t.Fatalf("path prefix lost: %s", got)
		}
		if f.head().Get("X-Scope-OrgID") != "openstack" {
			t.Fatalf("tenant header missing")
		}
		user, pass, ok := (&http.Request{Header: f.head()}).BasicAuth()
		if !ok || user != "u" || pass != "pw" {
			t.Fatalf("basic auth missing/wrong: %v %s %s", ok, user, pass)
		}
	})

	t.Run("bearer", func(t *testing.T) {
		p, err := NewPrometheus(PrometheusConfig{URL: srv.URL, BearerToken: "tok123"})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		if err := p.Ping(context.Background()); err != nil {
			t.Fatalf("ping: %v", err)
		}
		if got := f.head().Get("Authorization"); got != "Bearer tok123" {
			t.Fatalf("bearer missing: %q", got)
		}
	})
}

// TestPrometheusErrors: HTTP-level failures and PromQL error envelopes both surface as
// errors (they must NOT read as "0 usage").
func TestPrometheusErrors(t *testing.T) {
	t.Run("http-401", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, "no org id")
		}))
		defer srv.Close()
		p, _ := NewPrometheus(PrometheusConfig{URL: srv.URL})
		if err := p.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("expected 401 error, got %v", err)
		}
	})
	t.Run("promql-error-envelope", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
		}))
		defer srv.Close()
		p, _ := NewPrometheus(PrometheusConfig{URL: srv.URL})
		if err := p.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "parse error") {
			t.Fatalf("expected promql error, got %v", err)
		}
	})
	t.Run("bad-scheme", func(t *testing.T) {
		if _, err := NewPrometheus(PrometheusConfig{URL: "mimir.example.com"}); err == nil {
			t.Fatal("expected scheme validation error")
		}
	})
	t.Run("bad-schema", func(t *testing.T) {
		if _, err := NewPrometheus(PrometheusConfig{URL: "http://x", Schema: "nope"}); err == nil {
			t.Fatal("expected schema validation error")
		}
	})
}

// TestPrometheusMeasuresEdgeCases: empty vector → 0 (gnocchi parity for no-measures) and the
// negative-sum guard → 0 rather than a negative charge.
func TestPrometheusMeasuresEdgeCases(t *testing.T) {
	val := `{"status":"success","data":{"resultType":"vector","result":[]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, val)
	}))
	defer srv.Close()
	p, _ := NewPrometheus(PrometheusConfig{URL: srv.URL})
	start := time.Now().UTC().Add(-time.Hour)

	got, err := p.MeasuresMBForCurrentMonth(context.Background(), `m{a="b"}`, 0, start)
	if err != nil || !got.IsZero() {
		t.Fatalf("empty vector: got %s, err %v", got, err)
	}

	val = promVector(map[string]any{"__value__": "-5"})
	got, err = p.MeasuresMBForCurrentMonth(context.Background(), `m{a="b"}`, 0, start)
	if err != nil || !got.IsZero() {
		t.Fatalf("negative guard: got %s, err %v", got, err)
	}

	got, err = p.MeasuresMBForCurrentMonth(context.Background(), "", 0, start)
	if err != nil || !got.IsZero() {
		t.Fatalf("empty ref: got %s, err %v", got, err)
	}
}
