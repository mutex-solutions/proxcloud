package dbaas

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

func TestReadEndpointInvariantValidationAndStrictProjection(t *testing.T) {
	for _, engine := range []string{EnginePostgreSQL, EngineMySQL, EngineMariaDB} {
		for _, instances := range []any{2, 3, float64(2), float64(3), int64(3), json.Number("3")} {
			if err := ValidateReadEndpoint(engine, instances, true); err != nil || !EffectiveReadEndpoint(engine, instances, true) {
				t.Fatalf("supported engine/count rejected: %s %T %v", engine, instances, err)
			}
		}
		for _, instances := range []any{nil, 0, -1, 1, float64(1), 1.9, 2.5, math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64, float64(uint64(1) << 63), "3", true, []any{3}, json.Number("2.5"), json.Number("9223372036854775808")} {
			if err := ValidateReadEndpoint(engine, instances, true); err == nil || err.Error() != "a read-only endpoint needs more than one instance" || EffectiveReadEndpoint(engine, instances, true) {
				t.Fatalf("malformed/single count admitted: %s %T", engine, instances)
			}
			if err := ValidateReadEndpoint(engine, instances, false); err != nil {
				t.Fatal("disabled reader prevented legacy repair", err)
			}
		}
		for _, flag := range []any{nil, false, "true", float64(1), 1} {
			if EffectiveReadEndpoint(engine, 3, flag) {
				t.Fatal("non-boolean/non-enabled reader projected")
			}
		}
	}
	for _, engine := range []string{EngineValkey, EngineOpenSearch, EngineKafka, EngineFerretDB, "unknown", ""} {
		if err := ValidateReadEndpoint(engine, 3, true); err == nil || err.Error() != "engine "+engine+" has no separate read endpoint" || EffectiveReadEndpoint(engine, 3, true) {
			t.Fatal("unsupported engine admitted", engine)
		}
		if err := ValidateReadEndpoint(engine, 1, false); err != nil {
			t.Fatal("disabled unsupported reader blocked repair")
		}
	}
}

func TestReadEndpointCreateMessagesRemainUnchanged(t *testing.T) {
	cfg := testConfig()
	for _, engine := range []string{EnginePostgreSQL, EngineMySQL, EngineMariaDB} {
		cfg.Engines[engine] = EngineOffer{Versions: []string{"fixture"}, Replicas: []int{1, 2, 3}}
		for _, n := range []int{1, 2, 3} {
			spec := testSpec(engine, "fixture")
			spec.Replicas, spec.ReadEndpoint = n, true
			err := spec.Validate(cfg)
			if n == 1 {
				if err == nil || err.Error() != "database: a read-only endpoint needs more than one instance" {
					t.Fatal("CREATE count error contract changed", err)
				}
			} else if err != nil {
				t.Fatal("valid CREATE reader rejected", err)
			}
		}
	}
	bad := testSpec(EngineFerretDB, "2")
	bad.Replicas, bad.ReadEndpoint = 3, true
	if err := bad.Validate(cfg); err == nil || err.Error() != "database: engine ferretdb has no separate read endpoint" {
		t.Fatal("CREATE unsupported engine error contract changed", err)
	}
}

type readerLookupAPI struct {
	*fakeAPI
	serviceReads []string
}

func (f *readerLookupAPI) GetService(ctx context.Context, ns, name string) (map[string]any, error) {
	f.serviceReads = append(f.serviceReads, name)
	return f.fakeAPI.GetService(ctx, ns, name)
}

func TestReadEndpointSyncProjectsOnlyPossiblePolicyWithoutDesiredRepair(t *testing.T) {
	for _, tc := range []struct {
		name, engine string
		instances    any
		flag         any
		reader, lb   bool
	}{
		{"postgres-one", EnginePostgreSQL, 1, true, false, false},
		{"postgres-two", EnginePostgreSQL, float64(2), true, true, true},
		{"mysql-three", EngineMySQL, 3, true, true, false},
		{"maria-two", EngineMariaDB, 2, true, true, false},
		{"unsupported", EngineFerretDB, 3, true, false, false},
		{"unknown-engine", "unknown", 3, true, false, false},
		{"fraction", EnginePostgreSQL, 2.5, true, false, false},
		{"string-count", EnginePostgreSQL, "3", true, false, false},
		{"string-flag", EnginePostgreSQL, 3, "true", false, false},
		{"disabled", EnginePostgreSQL, 3, false, false, false},
		{"missing-count", EnginePostgreSQL, nil, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			spec := testSpec(tc.engine, "fixture")
			values := BuildValues(cfg, spec)
			values["instances"] = tc.instances
			values["network"].(map[string]any)["readEndpoint"] = tc.flag
			app := BuildApplication(cfg, spec, "svc", "frozen-pin", values)
			before, _ := json.Marshal(app)
			api := &readerLookupAPI{fakeAPI: newFakeAPI()}
			api.apps[key(cfg.ArgoNamespace, spec.ID)] = app
			ns := NamespaceFor(spec.ProjectID)
			service := func(host string) map[string]any {
				return map[string]any{"status": map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": host}}}}}
			}
			api.services[key(ns, LBServiceNameFor(tc.engine, spec.ID))] = service("192.0.2.1")
			api.services[key(ns, spec.ID+"-ro-lb")] = service("192.0.2.2")
			rows, err := NewWithAPI(api, cfg, "svc").SyncProvider("region", spec.ProjectID).List(t.Context())
			if err != nil || len(rows) != 1 {
				t.Fatal("sync", err, len(rows))
			}
			d := rows[0].Data["database"].(map[string]any)
			wantHost := ""
			if tc.reader {
				wantHost = "192.0.2.1"
				if tc.lb {
					wantHost = "192.0.2.2"
				}
			}
			if d["read_endpoint"] != tc.reader || d["read_endpoint_lb"] != tc.lb || d["read_endpoint_host"] != wantHost {
				t.Fatal("reader projection mismatch", tc.name)
			}
			wantReads := []string{LBServiceNameFor(tc.engine, spec.ID)}
			if tc.lb {
				wantReads = append(wantReads, spec.ID+"-ro-lb")
			}
			if !reflect.DeepEqual(api.serviceReads, wantReads) {
				t.Fatal("unnecessary reader service lookup", api.serviceReads)
			}
			// Even a caller carrying a stale host cannot reintroduce it on a bad policy.
			projected := databaseDataWithHost(app, "192.0.2.1", "stray.example.invalid")["database"].(map[string]any)
			if !tc.reader && projected["read_endpoint_host"] != "" {
				t.Fatal("stray legacy reader host advertised")
			}
			after, _ := json.Marshal(app)
			if string(before) != string(after) || len(api.ops) != 0 {
				t.Fatal("sync repaired desired values or performed writes")
			}
		})
	}
	for _, instances := range []any{math.NaN(), math.Inf(1), math.MaxFloat64} {
		app := map[string]any{"spec": map[string]any{"source": map[string]any{"helm": map[string]any{"valuesObject": map[string]any{"engine": EnginePostgreSQL, "instances": instances, "network": map[string]any{"readEndpoint": true}}}}}}
		d := databaseDataWithHost(app, "writer", "stray")["database"].(map[string]any)
		if d["read_endpoint"] != false || d["read_endpoint_lb"] != false || d["read_endpoint_host"] != "" {
			t.Fatal("non-finite/overflow instance count projected as a reader")
		}
	}
}

func TestReadEndpointProjectionKeepsBillingInputContract(t *testing.T) {
	for _, tc := range []struct {
		engine string
		n      int
		wantLB float64
	}{
		{EnginePostgreSQL, 1, 0}, {EnginePostgreSQL, 2, 1}, {EngineMySQL, 3, 0}, {EngineMariaDB, 3, 0}, {EngineFerretDB, 3, 0},
	} {
		spec := testSpec(tc.engine, "fixture")
		spec.Replicas, spec.ReadEndpoint = tc.n, true
		app := BuildApplication(testConfig(), spec, "svc", "pin", BuildValues(testConfig(), spec))
		data := databaseDataWithHost(app, "192.0.2.1", "")
		items, err := billingresource.NewDatabaseClusterProvider().GetBillingInformation(t.Context(), billingapi.BillingContext{}, &cloud.CloudResource{ID: "resource", ProjectID: "p1", Data: data})
		if err != nil || len(items) != 1 || items[0].NotEligibleForBilling || items[0].Values["read_endpoint_lb"] != tc.wantLB || items[0].Values["vcpus_total"] != float64(2*tc.n) {
			t.Fatal("billing input changed beyond impossible reader flag", tc.engine, err)
		}
	}
}

func TestReadEndpointCachedChartExplainsSingleInstanceMismatch(t *testing.T) {
	for _, engine := range []string{EnginePostgreSQL, EngineMariaDB} {
		for _, n := range []int{1, 3} {
			spec := chartSpec(engine, n)
			spec.ReadEndpoint = true // render the legacy-invalid shape; no create/Apply
			docs := renderChart(t, BuildValues(testConfig(), spec))
			roService, readerBackend := false, false
			for _, doc := range docs {
				if doc["kind"] == "Service" && digStr(doc, "metadata", "name") == spec.ID+"-ro-lb" {
					roService = true
				}
				if doc["kind"] == "ConfigMap" && strings.Contains(asJSON(t, doc), "bind *:3307") {
					readerBackend = true
				}
			}
			if engine == EnginePostgreSQL && roService != (n >= 2) || engine == EngineMariaDB && readerBackend != (n >= 2) {
				t.Fatal("cached chart reader count contract drift", engine, n)
			}
		}
	}
}
