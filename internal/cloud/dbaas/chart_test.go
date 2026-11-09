package dbaas

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Chart render checks: BuildValues feeds the REAL database-cluster chart through `helm template`,
// so the values contract (values.go) and the templates cannot drift apart unnoticed. Skipped when
// helm is not installed (CI runners have it).

const chartDir = "../../../deploy/charts/database-cluster"

// renderChart templates the chart with the given values and returns every rendered object.
func renderChart(t *testing.T, values map[string]any) []map[string]any {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed")
	}
	raw, err := yaml.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	vf := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(vf, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(helm, "template", "t", chartDir, "-f", vf)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var d map[string]any
		if err := dec.Decode(&d); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode rendered chart: %v", err)
		}
		if len(d) > 0 {
			docs = append(docs, d)
		}
	}
	return docs
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// chartVersion per engine: the oldest tag the chart's image map knows for it.
var chartVersion = map[string]string{
	EnginePostgreSQL: "17", EngineMySQL: "8.4", EngineMariaDB: "11.4", EngineValkey: "9.0",
	EngineFerretDB: "2.5", EngineOpenSearch: "3.3.0", EngineKafka: "4.3.0",
}

// podKind is the rendered object whose pods are the database's replicas, per engine.
var podKind = map[string]string{
	EnginePostgreSQL: "Cluster", EngineMySQL: "PerconaServerMySQL", EngineMariaDB: "MariaDB",
	EngineValkey: "ValkeyCluster", EngineFerretDB: "Cluster", EngineOpenSearch: "OpenSearchCluster",
	EngineKafka: "KafkaNodePool",
}

func chartSpec(engine string, replicas int) DatabaseSpec {
	s := testSpec(engine, chartVersion[engine])
	s.Replicas = replicas
	return s
}

// TestChartZoneSpread pins the hard zone anti-affinity of every engine's replicas: present with
// instances > 1 on scheduling.zoneKey (default topology.kubernetes.io/zone, overridable for a
// single-node site), absent on a single instance.
func TestChartZoneSpread(t *testing.T) {
	for engine := range podKind {
		t.Run(engine, func(t *testing.T) {
			find := func(docs []map[string]any) map[string]any {
				for _, d := range docs {
					if d["kind"] == podKind[engine] {
						return d
					}
				}
				t.Fatalf("no %s rendered", podKind[engine])
				return nil
			}
			// instances 3, chart default key.
			doc := asJSON(t, find(renderChart(t, BuildValues(testConfig(), chartSpec(engine, 3)))))
			if !strings.Contains(doc, `"topologyKey":"topology.kubernetes.io/zone"`) {
				t.Errorf("replicas must spread across topology.kubernetes.io/zone: %s", doc)
			}
			if engine == EnginePostgreSQL || engine == EngineFerretDB {
				if !strings.Contains(doc, `"podAntiAffinityType":"required"`) || !strings.Contains(doc, `"enablePodAntiAffinity":true`) {
					t.Errorf("CNPG anti-affinity must be required: %s", doc)
				}
			} else if !strings.Contains(doc, `"requiredDuringSchedulingIgnoredDuringExecution"`) {
				t.Errorf("anti-affinity must be a hard rule: %s", doc)
			}
			// One instance: nothing to spread.
			doc = asJSON(t, find(renderChart(t, BuildValues(testConfig(), chartSpec(engine, 1)))))
			if strings.Contains(doc, "topologyKey") {
				t.Errorf("a single instance must not carry an anti-affinity: %s", doc)
			}
			// A single-node site overrides the key.
			cfg := testConfig()
			cfg.ZoneKey = "kubernetes.io/hostname"
			doc = asJSON(t, find(renderChart(t, BuildValues(cfg, chartSpec(engine, 3)))))
			if !strings.Contains(doc, `"topologyKey":"kubernetes.io/hostname"`) || strings.Contains(doc, "topology.kubernetes.io/zone") {
				t.Errorf("zoneKey override not honoured: %s", doc)
			}
		})
	}
}

// TestChartAddressPoolOnEveryLB pins the MetalLB pool annotation on EVERY tenant-facing
// LoadBalancer Service — chart-owned (<id>-lb, <id>-ro-lb, <id>-dash-lb) and Strimzi's
// (bootstrap + per-broker, through the listener annotations) — and that no other placement
// annotation is rendered.
func TestChartAddressPoolOnEveryLB(t *testing.T) {
	const pool = "metallb.universe.tf/address-pool"
	cfg := testConfig()
	cfg.LBAddressPool = "dbaas-services"
	for _, tc := range []struct {
		name    string
		engine  string
		mutate  func(*DatabaseSpec)
		wantLBs []string // chart-owned LoadBalancer Services expected
	}{
		{"postgresql", EnginePostgreSQL, nil, []string{"-lb"}},
		{"postgresql read endpoint", EnginePostgreSQL, func(s *DatabaseSpec) { s.ReadEndpoint = true }, []string{"-lb", "-ro-lb"}},
		{"mysql", EngineMySQL, nil, []string{"-lb"}},
		{"mariadb", EngineMariaDB, nil, []string{"-lb"}},
		{"valkey", EngineValkey, nil, []string{"-lb"}},
		{"ferretdb", EngineFerretDB, nil, []string{"-lb"}},
		{"opensearch", EngineOpenSearch, nil, []string{"-lb"}},
		{"opensearch dashboards", EngineOpenSearch, func(s *DatabaseSpec) { s.DashboardsEnabled = true }, []string{"-lb", "-dash-lb"}},
		{"kafka", EngineKafka, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := chartSpec(tc.engine, 3)
			if tc.mutate != nil {
				tc.mutate(&spec)
			}
			docs := renderChart(t, BuildValues(cfg, spec))
			seen := map[string]bool{}
			for _, d := range docs {
				if d["kind"] == "Service" && digStr(d, "spec", "type") == "LoadBalancer" {
					name := digStr(d, "metadata", "name")
					seen[strings.TrimPrefix(name, spec.ID)] = true
					if digStr(d, "metadata", "annotations", pool) != "dbaas-services" {
						t.Errorf("Service %s lacks the address-pool annotation", name)
					}
					// The pool (and the display name) are the ONLY annotations: nothing places
					// the endpoint on a tenant network.
					annotations, _ := dig(d, "metadata", "annotations").(map[string]any)
					for k := range annotations {
						if k != pool && k != "proxcloud.io/display-name" {
							t.Errorf("Service %s carries an unexpected annotation %q", name, k)
						}
					}
				}
				if d["kind"] == "Kafka" {
					listeners, _ := dig(d, "spec", "kafka", "listeners").([]any)
					for _, l := range listeners {
						lm, _ := l.(map[string]any)
						if lm["type"] != "loadbalancer" {
							continue
						}
						if digStr(lm, "configuration", "bootstrap", "annotations", pool) != "dbaas-services" {
							t.Errorf("kafka bootstrap LB lacks the address-pool annotation")
						}
						brokers, _ := dig(lm, "configuration", "brokers").([]any)
						if len(brokers) != 3 {
							t.Errorf("kafka brokers = %d, want 3", len(brokers))
						}
						for _, b := range brokers {
							bm, _ := b.(map[string]any)
							if digStr(bm, "annotations", pool) != "dbaas-services" {
								t.Errorf("kafka broker %v LB lacks the address-pool annotation", bm["broker"])
							}
						}
					}
				}
			}
			for _, want := range tc.wantLBs {
				if !seen[want] {
					t.Errorf("Service %s%s not rendered (saw %v)", spec.ID, want, seen)
				}
			}
			if len(seen) != len(tc.wantLBs) {
				t.Errorf("LoadBalancer Services = %v, want %v", seen, tc.wantLBs)
			}
		})
	}
	// No pool configured → no annotation at all (MetalLB then uses any auto-assign pool).
	for _, d := range renderChart(t, BuildValues(testConfig(), chartSpec(EnginePostgreSQL, 1))) {
		if d["kind"] == "Service" && digStr(d, "metadata", "annotations", pool) != "" {
			t.Errorf("unset pool must not render an annotation")
		}
	}
}

// TestChartBackupAndRestore renders the postgresql backup + recovery shape the live drill uses: a
// plain-http S3 endpoint through the barman-cloud plugin ObjectStore, the WAL archiver wired on
// the Cluster, and a recovery bootstrap that reads the SOURCE's folder from a separate store.
func TestChartBackupAndRestore(t *testing.T) {
	cfg := testConfig()
	cfg.Backup = BackupConfig{Endpoint: "http://s3.example.com:9000", Bucket: "dbaas-backups", Prefix: "backups", Region: "us-east-1", PathStyle: true, AccessKey: "a", SecretKey: "s"}
	spec := chartSpec(EnginePostgreSQL, 3)
	spec.RestoreFrom = &RestoreSource{SourceID: "pc-d-0abc1234", TargetTime: "2026-11-01T09:30:00Z"}
	docs := renderChart(t, BuildValues(cfg, spec))

	byKindName := map[string]map[string]any{}
	for _, d := range docs {
		byKindName[asJSON(t, d["kind"])+"/"+digStr(d, "metadata", "name")] = d
	}
	store := byKindName[`"ObjectStore"/`+spec.ID+"-store"]
	if store == nil {
		t.Fatalf("no backup ObjectStore rendered")
	}
	if got := digStr(store, "spec", "configuration", "endpointURL"); got != "http://s3.example.com:9000" {
		t.Errorf("endpointURL = %q (plain http must pass through untouched)", got)
	}
	if got := digStr(store, "spec", "configuration", "destinationPath"); got != "s3://dbaas-backups/backups/"+spec.ID {
		t.Errorf("destinationPath = %q", got)
	}
	if got := digStr(store, "spec", "configuration", "s3Credentials", "accessKeyId", "name"); got != BackupSecretName(spec.ID) {
		t.Errorf("credentials secret = %q, want %q", got, BackupSecretName(spec.ID))
	}
	restoreStore := byKindName[`"ObjectStore"/`+spec.ID+"-restore-store"]
	if restoreStore == nil {
		t.Fatalf("no recovery ObjectStore rendered")
	}
	if got := digStr(restoreStore, "spec", "configuration", "destinationPath"); got != "s3://dbaas-backups/backups/pc-d-0abc1234" {
		t.Errorf("recovery path = %q (must read the SOURCE's folder)", got)
	}
	cluster := byKindName[`"Cluster"/`+spec.ID]
	if cluster == nil {
		t.Fatalf("no Cluster rendered")
	}
	j := asJSON(t, cluster)
	for _, want := range []string{
		`"isWALArchiver":true`,
		`"barmanObjectName":"` + spec.ID + `-store"`,
		`"barmanObjectName":"` + spec.ID + `-restore-store"`,
		`"serverName":"pc-d-0abc1234"`,
		`"targetTime":"2026-11-01T09:30:00Z"`,
	} {
		if !strings.Contains(j, want) {
			t.Errorf("Cluster lacks %s: %s", want, j)
		}
	}
	// A scheduled backup is rendered with the SIX-field cron CNPG wants.
	sched := byKindName[`"ScheduledBackup"/`+spec.ID+"-schedule"]
	if sched == nil || digStr(sched, "spec", "schedule") != "0 "+DefaultBackupSchedule {
		t.Errorf("ScheduledBackup = %v", sched)
	}
}
