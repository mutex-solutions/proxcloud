package project

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/dbaas"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// Only read/ApplyApplication are permitted. Embedding an unimplemented API makes
// accidental extra backend operations fail, rather than silently succeeding.
// GetApplication returns a deep copy like a real JSON API, never shared state.
type readInvariantAPI struct {
	dbaas.K8sAPI
	app              map[string]any
	beforeSecondRead func(map[string]any)
	reads, applies   int
	secretWrites     int
	lastApplied      map[string]any
}

func copyReadInvariantApp(in map[string]any) map[string]any {
	b, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}
func (a *readInvariantAPI) GetApplication(context.Context, string, string) (map[string]any, error) {
	a.reads++
	if a.reads == 2 && a.beforeSecondRead != nil {
		a.beforeSecondRead(a.app)
	}
	return copyReadInvariantApp(a.app), nil
}
func (a *readInvariantAPI) ApplyApplication(_ context.Context, app map[string]any) error {
	a.applies++
	a.lastApplied = copyReadInvariantApp(app)
	// The API preserves server metadata/status; only requested metadata/spec are
	// replaced here. This is not an atomic-concurrency or operator-fencing model.
	for k, v := range a.lastApplied["metadata"].(map[string]any) {
		a.app["metadata"].(map[string]any)[k] = v
	}
	a.app["spec"] = a.lastApplied["spec"]
	return nil
}
func (a *readInvariantAPI) ApplySecret(context.Context, string, string, map[string]string, map[string]string, map[string]string) error {
	a.secretWrites++
	panic("unexpected Secret write")
}
func (a *readInvariantAPI) PatchSecretData(context.Context, string, string, map[string]string) error {
	a.secretWrites++
	panic("unexpected Secret patch")
}
func (a *readInvariantAPI) DeleteSecret(context.Context, string, string) error {
	a.secretWrites++
	panic("unexpected Secret delete")
}

func readInvariantValues(app map[string]any) map[string]any {
	return app["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["valuesObject"].(map[string]any)
}
func readInvariantFixture(engine string, instances any, reader bool) (*Handler, *readInvariantAPI, *Project, *externalservice.ExternalService, *cloud.CloudResource) {
	cfg := dbaas.Config{ArgoNamespace: "argocd", ArgoProject: "proxcloud-dbaas", ChartRepo: "registry.example/charts", ChartName: "database-cluster", ChartVersion: "new-default-not-existing-pin", Engines: map[string]dbaas.EngineOffer{}}
	for _, e := range []string{dbaas.EnginePostgreSQL, dbaas.EngineMySQL, dbaas.EngineMariaDB, dbaas.EngineFerretDB, dbaas.EngineValkey} {
		cfg.Engines[e] = dbaas.EngineOffer{Versions: []string{"fixture"}, Replicas: []int{1, 2, 3}}
	}
	spec := dbaas.DatabaseSpec{ID: "pc-d-01234567", ProjectID: "p1", Engine: engine, Version: "fixture", Replicas: 3, CPU: 2, MemoryGiB: 4, StorageGiB: 20, AllowedCIDRs: []string{"192.0.2.0/24"}, ReadEndpoint: reader}
	values := dbaas.BuildValues(cfg, spec)
	values["instances"] = instances
	values["fixtureExtension"] = map[string]any{"preserve": "operator-owned-data"}
	app := dbaas.BuildApplication(cfg, spec, "service", "exact-existing-pin", values)
	app["metadata"].(map[string]any)["uid"] = "stable-uid"
	app["metadata"].(map[string]any)["resourceVersion"] = "17"
	app["status"] = map[string]any{"sync": map[string]any{"status": "Synced"}}
	a := &readInvariantAPI{app: copyReadInvariantApp(app)}
	svc := dbaas.NewWithAPI(a, cfg, "service")
	h := &Handler{dbaasFor: func(*externalservice.ExternalService) (*dbaas.Service, error) { return svc, nil }}
	es := &externalservice.ExternalService{ID: "service", Config: map[string]any{"provider": "dbaas"}}
	// Deliberately contradict actual current policy: actions must not read cache.
	cr := &cloud.CloudResource{ID: "resource", ProjectID: "p1", ServiceID: es.ID, ExternalID: spec.ID, Type: cloud.TypeDatabaseCluster, Data: map[string]any{"database": map[string]any{"engine": dbaas.EngineValkey, "replicas": float64(1), "read_endpoint": !reader}}}
	return h, a, &Project{ID: "p1"}, es, cr
}
func callReadInvariantAction(t *testing.T, h *Handler, p *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	if !h.dbaasAction(w, httptest.NewRequest(http.MethodPost, "/fixture", nil), p, es, cr, action, data) {
		t.Fatal("DBaaS action was not handled")
	}
	return w
}
func assertReadInvariantAppEqual(t *testing.T, before, after map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refused action mutated complete Application")
	}
}

func TestDbaasReadInvariantScaleUsesCurrentValuesAndRefusesBeforeApply(t *testing.T) {
	for _, engine := range []string{dbaas.EnginePostgreSQL, dbaas.EngineMySQL, dbaas.EngineMariaDB} {
		t.Run(engine, func(t *testing.T) {
			h, a, p, es, cr := readInvariantFixture(engine, 3, true)
			before := copyReadInvariantApp(a.app)
			w := callReadInvariantAction(t, h, p, es, cr, "SCALE_REPLICAS", map[string]any{"replicas": float64(1)})
			if w.Code != 409 || !strings.Contains(w.Body.String(), "Disable the read-only endpoint before scaling below two instances") || a.applies != 0 || a.secretWrites != 0 {
				t.Fatal("reader downscale was not refused before Apply", w.Code)
			}
			assertReadInvariantAppEqual(t, before, a.app)
		})
	}
	// First live engine GET says PostgreSQL; callback GET now carries an offered
	// unsupported-reader engine. Neither stale engine nor stale cache is authority.
	for _, mode := range []string{"engine-changed", "unknown-engine", "offer-changed", "reader-changed"} {
		t.Run(mode, func(t *testing.T) {
			h, a, p, es, cr := readInvariantFixture(dbaas.EnginePostgreSQL, 3, false)
			a.beforeSecondRead = func(app map[string]any) {
				v := readInvariantValues(app)
				switch mode {
				case "engine-changed":
					v["engine"] = dbaas.EngineFerretDB
					v["network"].(map[string]any)["readEndpoint"] = true
				case "unknown-engine":
					v["engine"] = "unknown"
				case "offer-changed":
					v["engine"] = dbaas.EngineKafka // capable but absent from current catalog
				case "reader-changed":
					v["network"].(map[string]any)["readEndpoint"] = true
				}
			}
			expected := copyReadInvariantApp(a.app)
			a.beforeSecondRead(expected)
			w := callReadInvariantAction(t, h, p, es, cr, "SCALE_REPLICAS", map[string]any{"replicas": 1})
			want := 400
			if mode == "reader-changed" {
				want = 409
			}
			if w.Code != want || a.applies != 0 || a.secretWrites != 0 {
				t.Fatal("callback revalidation failed", mode, w.Code)
			}
			assertReadInvariantAppEqual(t, expected, a.app)
		})
	}
}

func TestDbaasReadInvariantDisableThenScaleAndPositiveCounts(t *testing.T) {
	for _, engine := range []string{dbaas.EnginePostgreSQL, dbaas.EngineMySQL, dbaas.EngineMariaDB} {
		for _, mode := range []string{"disable-first", "already-disabled", "scale-two", "scale-three"} {
			t.Run(engine+"/"+mode, func(t *testing.T) {
				h, a, p, es, cr := readInvariantFixture(engine, 3, mode != "already-disabled")
				want := copyReadInvariantApp(a.app)
				if mode == "disable-first" {
					w := callReadInvariantAction(t, h, p, es, cr, "SET_READ_ENDPOINT", map[string]any{"enabled": false})
					if w.Code != 200 || a.applies != 1 {
						t.Fatal("explicit disable failed", w.Code)
					}
					delete(readInvariantValues(want)["network"].(map[string]any), "readEndpoint")
				}
				n := 1
				if mode == "scale-two" {
					n = 2
				} else if mode == "scale-three" {
					n = 3
				}
				w := callReadInvariantAction(t, h, p, es, cr, "SCALE_REPLICAS", map[string]any{"replicas": n})
				if w.Code != 200 || a.secretWrites != 0 {
					t.Fatal("valid scale failed", w.Code)
				}
				readInvariantValues(want)["instances"] = float64(n)
				assertReadInvariantAppEqual(t, want, a.app)
				if a.lastApplied["spec"].(map[string]any)["source"].(map[string]any)["targetRevision"] != "exact-existing-pin" {
					t.Fatal("existing chart pin was changed")
				}
			})
		}
	}
}

func TestDbaasReadInvariantSetAndCreateKeep400AndRepairIsNotGlobal(t *testing.T) {
	for _, engine := range []string{dbaas.EnginePostgreSQL, dbaas.EngineMySQL, dbaas.EngineMariaDB} {
		h, a, p, es, cr := readInvariantFixture(engine, 1, false)
		before := copyReadInvariantApp(a.app)
		w := callReadInvariantAction(t, h, p, es, cr, "SET_READ_ENDPOINT", map[string]any{"enabled": true})
		if w.Code != 400 || !strings.Contains(w.Body.String(), "a read-only endpoint needs more than one instance") || a.applies != 0 {
			t.Fatal("SET 400 contract changed", w.Code)
		}
		assertReadInvariantAppEqual(t, before, a.app)
		w = httptest.NewRecorder()
		h.dbaasCreate(w, httptest.NewRequest(http.MethodPost, "/fixture", nil), nil, p, es, providers.CreateRequest{Type: cloud.TypeDatabaseCluster, Data: map[string]any{"name": "fixture", "engine": engine, "version": "fixture", "replicas": float64(1), "cpu": float64(2), "memoryGiB": float64(4), "storageGiB": float64(20), "readEndpoint": true}})
		if w.Code != 400 || !strings.Contains(w.Body.String(), "database: a read-only endpoint needs more than one instance") || a.applies != 0 || a.secretWrites != 0 {
			t.Fatal("CREATE 400 contract changed", w.Code)
		}
	}
	for _, engine := range []string{dbaas.EnginePostgreSQL, dbaas.EngineMySQL, dbaas.EngineMariaDB} {
		for _, n := range []int{2, 3} {
			h, a, p, es, cr := readInvariantFixture(engine, n, false)
			want := copyReadInvariantApp(a.app)
			w := callReadInvariantAction(t, h, p, es, cr, "SET_READ_ENDPOINT", map[string]any{"enabled": true})
			if w.Code != 200 || a.applies != 1 || a.secretWrites != 0 {
				t.Fatal("valid SET reader refused", engine, n, w.Code)
			}
			readInvariantValues(want)["network"].(map[string]any)["readEndpoint"] = true
			assertReadInvariantAppEqual(t, want, a.app)
		}
	}
	for _, malformed := range []any{nil, "3", 2.5} {
		h, a, p, es, cr := readInvariantFixture(dbaas.EnginePostgreSQL, malformed, false)
		w := callReadInvariantAction(t, h, p, es, cr, "SET_READ_ENDPOINT", map[string]any{"enabled": true})
		if w.Code != 400 || a.applies != 0 {
			t.Fatal("SET accepted malformed declared count", w.Code)
		}
	}
	// A current unsupported engine introduced after the initial GET cannot bypass SET.
	h, a, p, es, cr := readInvariantFixture(dbaas.EnginePostgreSQL, 3, false)
	a.beforeSecondRead = func(app map[string]any) { readInvariantValues(app)["engine"] = dbaas.EngineFerretDB }
	if w := callReadInvariantAction(t, h, p, es, cr, "SET_READ_ENDPOINT", map[string]any{"enabled": true}); w.Code != 400 || a.applies != 0 {
		t.Fatal("SET callback used stale engine", w.Code)
	}
	// Keep offered-count and unsupported-action 400 validation, not a conflict waiver.
	h, a, p, es, cr = readInvariantFixture(dbaas.EnginePostgreSQL, 3, true)
	if w := callReadInvariantAction(t, h, p, es, cr, "SCALE_REPLICAS", map[string]any{"replicas": 4}); w.Code != 400 || a.applies != 0 {
		t.Fatal("unoffered scale accepted", w.Code)
	}
	h, a, p, es, cr = readInvariantFixture(dbaas.EngineFerretDB, 3, false)
	if w := callReadInvariantAction(t, h, p, es, cr, "SET_READ_ENDPOINT", map[string]any{"enabled": true}); w.Code != 400 || a.applies != 0 {
		t.Fatal("unsupported reader action accepted", w.Code)
	}
	// An engine change between reads must apply the CURRENT engine's offer, not
	// an earlier PostgreSQL catalog that happened to allow the proposed count.
	h, a, p, es, cr = readInvariantFixture(dbaas.EnginePostgreSQL, 3, false)
	svc, err := h.dbaasFor(es)
	if err != nil {
		t.Fatal(err)
	}
	cfg := svc.Config()
	cfg.Engines[dbaas.EngineMariaDB] = dbaas.EngineOffer{Versions: []string{"fixture"}, Replicas: []int{1, 3}}
	a.beforeSecondRead = func(app map[string]any) { readInvariantValues(app)["engine"] = dbaas.EngineMariaDB }
	if w := callReadInvariantAction(t, h, p, es, cr, "SCALE_REPLICAS", map[string]any{"replicas": 2}); w.Code != 400 || a.applies != 0 || !strings.Contains(w.Body.String(), "engine mariadb offers [1 3] replicas, not 2") {
		t.Fatal("scale used the earlier engine's replica catalog", w.Code)
	}
	// No global full-values validator: resizing unrelated CPU on legacy reader=1
	// remains possible. This neither disables nor repairs its desired reader flag.
	h, a, p, es, cr = readInvariantFixture(dbaas.EnginePostgreSQL, 1, true)
	if w := callReadInvariantAction(t, h, p, es, cr, "RESIZE", map[string]any{"cpu": 2, "memoryGiB": 4}); w.Code != 200 || a.applies != 1 || a.secretWrites != 0 || readInvariantValues(a.app)["network"].(map[string]any)["readEndpoint"] != true {
		t.Fatal("unrelated repair was blocked or reader policy silently changed", w.Code)
	}
}
