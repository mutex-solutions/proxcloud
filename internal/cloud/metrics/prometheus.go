// prometheus.go is the Prometheus-compatible usage source: the same MeasureFetcher
// contract as the Gnocchi client, backed by a PromQL endpoint (vanilla Prometheus, Mimir,
// VictoriaMetrics, Thanos — anything serving /api/v1/query). The billing math is kept
// IDENTICAL to the gnocchi path: a cumulative counter's month usage = max − min of the raw
// samples inside the month window (NOT PromQL increase(), which extrapolates to window
// boundaries and is documented as unsuitable for exact/billing use). Counter resets
// undercount the same way they do on the gnocchi path — deliberate parity.
package metrics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Prometheus metric schemas. The three known producers of per-instance OpenStack usage
// emit DIFFERENT metric names and label keys, so the schema is configuration:
//
//   - libvirt-exporter (kolla's prometheus-libvirt-exporter; the verified reference layout):
//     traffic = libvirt_domain_interface_stats_{receive,transmit}_bytes_total
//     {domain, instance=<computeHost:port>, target_device=<tap…>}; the nova uuid lives on
//     the join metric libvirt_domain_openstack_info{domain, instance_id, project_id, …}.
//   - ceilometer-pushgateway (ceilometer `prometheus://` publisher): bare names
//     network_{incoming,outgoing}_bytes with labels resource_id/project_id/user_id.
//   - ceilometer-exporter (ceilometer [polling] enable_prometheus_exporter, and sg-core):
//     ceilometer_network_{incoming,outgoing}_bytes with labels resource/project/user.
const (
	PromSchemaLibvirtExporter    = "libvirt-exporter"
	PromSchemaCeilometerPushgw   = "ceilometer-pushgateway"
	PromSchemaCeilometerExporter = "ceilometer-exporter"
	defaultPromTimeoutSeconds    = 30
	// Interface discovery looks back a fixed window that safely covers any point of the
	// current month (the measures themselves are strictly bounded by the month start, so
	// over-discovery of dead interfaces is harmless — their windowed delta is 0/empty).
	promDiscoveryLookback = 32 * 24 * time.Hour
)

// PrometheusConfig is the connection + schema config for one provider's Prometheus-compatible
// usage source (built by externalservice.PrometheusMetricsConfig from config.metrics.* and the
// decrypted secret.prometheus* leaves). URL is the full base up to (not including) /api/v1 —
// e.g. "https://mimir.example.com/prometheus" (Mimir), "http://prom:9090" (vanilla),
// "http://vmselect:8481/select/0/prometheus" (VictoriaMetrics cluster).
type PrometheusConfig struct {
	URL            string
	Schema         string            // one of the PromSchema* constants; default libvirt-exporter
	Headers        map[string]string // extra request headers, e.g. X-Scope-OrgID for Mimir
	BasicUser      string
	BasicPassword  string
	BearerToken    string
	InsecureTLS    bool
	CACert         string // optional PEM bundle
	TimeoutSeconds int
}

// promSchema is the resolved per-schema query shape.
type promSchema struct {
	incomingMetric string
	outgoingMetric string
	resourceLabel  string // the label whose value identifies the interface resource
	joinViaLibvirt bool   // libvirt-exporter: nova uuid → domain via libvirt_domain_openstack_info
}

func schemaFor(name string) (promSchema, error) {
	switch name {
	case "", PromSchemaLibvirtExporter:
		return promSchema{
			incomingMetric: "libvirt_domain_interface_stats_receive_bytes_total",
			outgoingMetric: "libvirt_domain_interface_stats_transmit_bytes_total",
			resourceLabel:  "target_device",
			joinViaLibvirt: true,
		}, nil
	case PromSchemaCeilometerPushgw:
		return promSchema{
			incomingMetric: "network_incoming_bytes",
			outgoingMetric: "network_outgoing_bytes",
			resourceLabel:  "resource_id",
		}, nil
	case PromSchemaCeilometerExporter:
		return promSchema{
			incomingMetric: "ceilometer_network_incoming_bytes",
			outgoingMetric: "ceilometer_network_outgoing_bytes",
			resourceLabel:  "resource",
		}, nil
	default:
		return promSchema{}, fmt.Errorf("prometheus: unknown metrics schema %q", name)
	}
}

// Prometheus implements MeasureFetcher against a Prometheus-compatible query API.
type Prometheus struct {
	cfg    PrometheusConfig
	schema promSchema
	hc     *http.Client
	base   string // cfg.URL without trailing slash
	now    func() time.Time
}

// NewPrometheus validates the config and builds the client. No network I/O happens here.
func NewPrometheus(cfg PrometheusConfig) (*Prometheus, error) {
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return nil, fmt.Errorf("prometheus: url must be http(s), got %q", cfg.URL)
	}
	schema, err := schemaFor(cfg.Schema)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if cfg.TimeoutSeconds <= 0 {
		timeout = defaultPromTimeoutSeconds * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureTLS || cfg.CACert != "" {
		tlsCfg := &tls.Config{InsecureSkipVerify: cfg.InsecureTLS} //nolint:gosec // operator-configured escape hatch
		if cfg.CACert != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(cfg.CACert)) {
				return nil, fmt.Errorf("prometheus: caCert is not a valid PEM bundle")
			}
			tlsCfg.RootCAs = pool
		}
		transport.TLSClientConfig = tlsCfg
	}
	return &Prometheus{
		cfg:    cfg,
		schema: schema,
		hc:     &http.Client{Timeout: timeout, Transport: transport},
		base:   trimSlash(cfg.URL),
		now:    func() time.Time { return time.Now().UTC() },
	}, nil
}

// WithNow overrides the clock (tests).
func (p *Prometheus) WithNow(now func() time.Time) *Prometheus { p.now = now; return p }

// Ping is a read-only connectivity check: an instant `vector(1)` query proves the endpoint,
// auth, tenant header, and path prefix all work.
func (p *Prometheus) Ping(ctx context.Context) error {
	_, err := p.queryVector(ctx, "vector(1)", nil)
	return err
}

// SearchInstanceInterfaces discovers a server's network interfaces as Resources, shaped
// exactly like the gnocchi ones: Name = the tap device (feeds the tap→PORT public/private
// classifier), Metrics = the per-direction "metric refs" (here: PromQL series selectors).
// The libvirt schema resolves the nova uuid to the libvirt domain first; the ceilometer
// schemas match the interface resource label against the uuid directly.
func (p *Prometheus) SearchInstanceInterfaces(ctx context.Context, instanceID string) ([]Resource, error) {
	if instanceID == "" {
		return nil, nil
	}
	lookback := promDuration(promDiscoveryLookback)
	if p.schema.joinViaLibvirt {
		// Resolve nova uuid → (domain, scrape-target host) PAIRS and pin BOTH labels in every
		// downstream selector: libvirt domain names ("instance-%08x") are only
		// cell-local-unique, so on a Prometheus/Mimir tenant fed by more than one nova cell
		// or cloud a bare domain match could sum another tenant's traffic into this bill.
		// Matching hosts where THIS instance_id actually ran keeps live-migration segments
		// (last_over_time over the lookback sees old + new host) while excluding same-named
		// domains elsewhere.
		pairs, err := p.queryVector(ctx,
			fmt.Sprintf(`count by (domain, instance) (last_over_time(libvirt_domain_openstack_info{instance_id=%q}[%s]))`,
				instanceID, lookback), nil)
		if err != nil {
			return nil, err
		}
		var domains, hosts []string
		for _, s := range pairs {
			domains = appendUnique(domains, s.Metric["domain"])
			hosts = appendUnique(hosts, s.Metric["instance"])
		}
		if len(domains) == 0 {
			return nil, nil // instance unknown to the exporter (yet) → no interfaces, 0 usage
		}
		match := fmt.Sprintf(`domain=~%q`, regexAlternation(domains))
		if hostSel := regexAlternation(hosts); hostSel != "" {
			match += fmt.Sprintf(`,instance=~%q`, hostSel)
		}
		devices, err := p.labelValues(ctx,
			fmt.Sprintf(`count by (target_device) (last_over_time(%s{%s}[%s]))`,
				p.schema.incomingMetric, match, lookback),
			"target_device")
		if err != nil {
			return nil, err
		}
		out := make([]Resource, 0, len(devices))
		for _, dev := range devices {
			sel := fmt.Sprintf(`{%s,target_device=%q}`, match, dev)
			out = append(out, Resource{
				ID:   dev,
				Name: dev, // target_device IS the tap name
				Metrics: map[string]string{
					// libvirt receive/transmit → incoming/outgoing mirrors ceilometer's libvirt
					// inspector mapping (network.incoming.bytes ← rx), i.e. gnocchi parity.
					"network.incoming.bytes": p.schema.incomingMetric + sel,
					"network.outgoing.bytes": p.schema.outgoingMetric + sel,
				},
			})
		}
		return out, nil
	}
	// ceilometer schemas: the interface resource label value embeds the instance uuid.
	rids, err := p.labelValues(ctx,
		fmt.Sprintf(`count by (%s) (last_over_time(%s{%s=~%q}[%s]))`,
			p.schema.resourceLabel, p.schema.incomingMetric, p.schema.resourceLabel,
			".*"+regexp.QuoteMeta(instanceID)+".*", lookback),
		p.schema.resourceLabel)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(rids))
	for _, rid := range rids {
		sel := fmt.Sprintf(`{%s=%q}`, p.schema.resourceLabel, rid)
		out = append(out, Resource{
			ID:   rid,
			Name: tapNameFrom(rid),
			Metrics: map[string]string{
				"network.incoming.bytes": p.schema.incomingMetric + sel,
				"network.outgoing.bytes": p.schema.outgoingMetric + sel,
			},
		})
	}
	return out, nil
}

// MeasuresMBForCurrentMonth returns the month-to-date usage in MB for one metric ref (a
// PromQL series selector produced by SearchInstanceInterfaces): per-series max−min over the
// window since month start, summed across series, divided by 1MiB in decimal64.
//
// Per-series-then-sum matters: a live-migrated VM's counters continue under a new scrape
// target (and reset), so its usage is the sum of each segment's delta — pinning one series
// (or taking a global max−min) would drop or corrupt migrated months. The subtraction is
// computed by the backend in float64; byte counters are exact in float64 up to 2^53 (~9 PB
// per interface-month), far above realistic traffic. `granularity` is gnocchi-only and
// ignored here.
func (p *Prometheus) MeasuresMBForCurrentMonth(ctx context.Context, metricRef string, _ int, start time.Time) (decimal.Decimal, error) {
	if metricRef == "" {
		return decimal.Zero, nil
	}
	now := p.now()
	window := now.Sub(start.UTC())
	if window < time.Minute {
		window = time.Minute
	}
	w := promDuration(window)
	q := fmt.Sprintf(`sum(max_over_time(%s[%s]) - min_over_time(%s[%s]))`, metricRef, w, metricRef, w)
	// Pin the evaluation instant to the same clock the window was computed from, so the
	// range [now−window, now] starts exactly at the month boundary.
	samples, err := p.queryVector(ctx, q, &now)
	if err != nil {
		return decimal.Zero, err
	}
	if len(samples) == 0 {
		return decimal.Zero, nil // no samples in the window → 0, same as gnocchi's empty measures
	}
	deltaBytes, err := decimal.NewFromString(samples[0].value())
	if err != nil {
		return decimal.Zero, fmt.Errorf("prometheus: parse value %q: %w", samples[0].value(), err)
	}
	if deltaBytes.IsNegative() {
		// max_over_time ≥ min_over_time per series, so a negative sum can't legitimately
		// happen; guard anyway rather than emit a negative charge.
		return decimal.Zero, nil
	}
	return divDecimal64(deltaBytes, decimal.NewFromInt(bytesPerMB))
}

// CountTrafficSeries counts distinct incoming-traffic series in the lookback window at the
// given instant (zero `at` = now). It backs the admin "Test connection" probe: >0 proves the
// endpoint actually carries per-instance usage data under the configured schema.
func (p *Prometheus) CountTrafficSeries(ctx context.Context, at time.Time, lookback time.Duration) (int, error) {
	q := fmt.Sprintf(`count(last_over_time(%s[%s]))`, p.schema.incomingMetric, promDuration(lookback))
	var atp *time.Time
	if !at.IsZero() {
		atp = &at
	}
	samples, err := p.queryVector(ctx, q, atp)
	if err != nil {
		return 0, err
	}
	if len(samples) == 0 {
		return 0, nil
	}
	d, err := decimal.NewFromString(samples[0].value())
	if err != nil {
		return 0, fmt.Errorf("prometheus: parse count %q: %w", samples[0].value(), err)
	}
	return int(d.IntPart()), nil
}

// InstantValues runs an instant vector query and returns value-by-label: one entry per sample,
// keyed by the sample's keyLabel value (samples without the label are skipped). The dbaas disk
// autoscale leg uses it for kubelet volume-stats ratios keyed by persistentvolumeclaim.
func (p *Prometheus) InstantValues(ctx context.Context, promql, keyLabel string) (map[string]float64, error) {
	samples, err := p.queryVector(ctx, promql, nil)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(samples))
	for _, s := range samples {
		key := s.Metric[keyLabel]
		if key == "" {
			continue
		}
		if v, err := strconv.ParseFloat(s.value(), 64); err == nil {
			out[key] = v
		}
	}
	return out, nil
}

// ---- HTTP + PromQL plumbing ----

type promSample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

// value returns the sample value (index 1) as its raw string form.
func (s promSample) value() string {
	v, _ := s.Value[1].(string)
	return v
}

// queryVector POSTs an instant query to /api/v1/query and returns the vector samples.
// POST keeps long selectors out of the URL and is supported by every compatible backend.
func (p *Prometheus) queryVector(ctx context.Context, promql string, at *time.Time) ([]promSample, error) {
	form := url.Values{"query": {promql}}
	if at != nil {
		form.Set("time", at.UTC().Format(time.RFC3339Nano))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/api/v1/query", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range p.cfg.Headers {
		if k != "" {
			req.Header.Set(k, v)
		}
	}
	// Auth precedence: bearer wins when both are (mis)configured — it is the more specific
	// credential; the admin UI only offers one mode at a time.
	switch {
	case p.cfg.BearerToken != "":
		req.Header.Set("Authorization", "Bearer "+p.cfg.BearerToken)
	case p.cfg.BasicUser != "" || p.cfg.BasicPassword != "":
		req.SetBasicAuth(p.cfg.BasicUser, p.cfg.BasicPassword)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: query: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("prometheus: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string       `json:"resultType"`
			Result     []promSample `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("prometheus: decode response: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prometheus: query failed: %s", truncate(out.Error, 300))
	}
	return out.Data.Result, nil
}

// labelValues runs an aggregating query and returns the (sorted, deduped) values of one
// result label — the discovery primitive.
func (p *Prometheus) labelValues(ctx context.Context, promql, label string) ([]string, error) {
	samples, err := p.queryVector(ctx, promql, nil)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(samples))
	for _, s := range samples {
		v := s.Metric[label]
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}

// appendUnique appends v when non-empty and not already present (tiny inputs — linear scan).
func appendUnique(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// regexAlternation builds a PromQL regex matching any of the values EXACTLY. Prometheus
// anchors label-matcher regexes itself, but the explicit ^(?:…)$ keeps the exact-match
// guarantee on any "compatible" backend that might not (double anchoring is harmless RE2) —
// without it, `instance-0001` would also match `instance-00010` there and pull another
// domain's traffic into the bill. Empty input stays "" so callers can omit the matcher.
func regexAlternation(values []string) string {
	if len(values) == 0 {
		return ""
	}
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = regexp.QuoteMeta(v)
	}
	return "^(?:" + strings.Join(quoted, "|") + ")$"
}

// tapNameFrom extracts the tap device from a ceilometer interface resource id (which embeds
// it, e.g. "instance-…-<uuid>-tapab12cd34-56"); falls back to the raw id when no tap token
// is present so prefix-matching degrades to never-public rather than crashing.
var tapRe = regexp.MustCompile(`tap[0-9a-fA-F][0-9a-fA-F-]*`)

func tapNameFrom(resourceID string) string {
	if m := tapRe.FindString(resourceID); m != "" {
		return m
	}
	return resourceID
}

// promDuration renders a Go duration as a PromQL duration in whole seconds.
func promDuration(d time.Duration) string {
	s := int64(d.Seconds())
	if s < 1 {
		s = 1
	}
	return fmt.Sprintf("%ds", s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
