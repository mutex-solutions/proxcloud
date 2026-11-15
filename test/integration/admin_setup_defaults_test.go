//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type setupDefaultsItem struct {
	ID, State, Message string
	Preview            json.RawMessage
}

// setupDefaultsEnv is a database as the Compose seed leaves it (the two configuration documents, no table for
// flavors or price plans yet) plus one Proxmox provider whose default quota holds per-model GPU limits.
func setupDefaultsEnv(t *testing.T) (*pgdoc.DB, func(method, body string) (int, []setupDefaultsItem)) {
	t.Helper()
	ctx := context.Background()
	db := freshPG(t)
	for table, doc := range map[string]pgdoc.M{
		"billingConfiguration":  {"_id": "seed", "baseCurrency": "USD", "defaultConfiguration": true},
		"platformConfiguration": {"_id": "seed", "name": "ProxCloud", "defaultConfiguration": true, "regions": []any{}},
		"externalService": {"_id": "svc-pve", "name": "lab", "type": "CLOUD", "status": "PUBLIC", "secret": "sealed", "config": pgdoc.M{
			"provider": "proxmox", "region": "lab", "apiUrl": "https://pve.invalid:8006",
			"provisioning": pgdoc.M{"quota": pgdoc.M{"gpu": pgdoc.M{"nvidia-4090": 2}, "servers": 50}}}},
	} {
		if _, err := db.C(table).InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	const iss, cid = "test-iss", "test-cid"
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, nil, "", iss, cid)
	h.SetPVEFlavors(pveflavor.New(db))
	r := chi.NewRouter()
	h.Routes(r)
	return db, func(method, body string) (int, []setupDefaultsItem) {
		req := httptest.NewRequest(method, "/admin/setup/defaults?serviceId=svc-pve", strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var env struct {
			Data struct {
				Items []setupDefaultsItem `json:"items"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		return w.Code, env.Data.Items
	}
}

const applyAllDefaults = `{"serviceId":"svc-pve","items":["flavors","pricePlan","quota","region"],"confirmRates":true}`

func setupDefaultsCounts(t *testing.T, db *pgdoc.DB) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, c := range []string{"flavor", "pricePlan", "pricePlanRule", "platformConfiguration", "externalService"} {
		n, err := db.C(c).Count(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		out[c] = n
	}
	return out
}

// POST /admin/setup/defaults creates what is missing, once: a second apply answers skipped everywhere and leaves the
// same number of documents, the stored quota keeps its GPU limits and its own numbers, and the region is appended.
func TestAdminSetupDefaults_ApplyTwice(t *testing.T) {
	ctx := context.Background()
	db, call := setupDefaultsEnv(t)

	code, items := call(http.MethodGet, "")
	if code != 200 || len(items) != 4 {
		t.Fatalf("preview: %d %+v", code, items)
	}
	for _, it := range items {
		if it.State != "todo" || len(it.Preview) == 0 {
			t.Fatalf("preview of %s: %s, preview %s", it.ID, it.State, it.Preview)
		}
	}
	if n, _ := db.C("pricePlan").Count(ctx, nil); n != 0 {
		t.Fatal("the preview wrote a price plan")
	}

	code, items = call(http.MethodPost, applyAllDefaults)
	if code != 200 || len(items) != 4 {
		t.Fatalf("apply: %d %+v", code, items)
	}
	for _, it := range items {
		if it.State != "created" {
			t.Fatalf("first apply, %s: %s (%s)", it.ID, it.State, it.Message)
		}
	}
	first := setupDefaultsCounts(t, db)
	if first["flavor"] != 3 || first["pricePlan"] != 1 || first["pricePlanRule"] != 7 || first["platformConfiguration"] != 1 {
		t.Fatalf("after the first apply: %v", first)
	}
	var plan struct {
		ID         string `json:"id"`
		Enabled    bool   `json:"enabled"`
		AccessMode string `json:"accessMode"`
	}
	if _, err := db.C("pricePlan").FindOne(ctx, pgdoc.M{}, &plan); err != nil || !plan.Enabled || plan.AccessMode != "PUBLIC" {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if n, _ := db.C("pricePlanRule").Count(ctx, pgdoc.M{"pricePlanId": plan.ID}); n != 7 {
		t.Fatalf("%d rules point at the plan", n)
	}
	var svc struct {
		Secret string `json:"secret"`
		Config struct {
			APIURL       string `json:"apiUrl"`
			Provisioning struct {
				Quota map[string]any `json:"quota"`
			} `json:"provisioning"`
		} `json:"config"`
	}
	if _, err := db.C("externalService").Get(ctx, "svc-pve", &svc); err != nil {
		t.Fatal(err)
	}
	q := svc.Config.Provisioning.Quota
	if gpu, _ := q["gpu"].(map[string]any); gpu["nvidia-4090"] != float64(2) || q["servers"] != float64(50) || q["vcpus"] != float64(80) || q["floatingIps"] != float64(5) {
		t.Fatalf("merged quota: %v", q)
	}
	if svc.Secret != "sealed" || svc.Config.APIURL == "" {
		t.Fatalf("the quota write touched the rest of the provider: %+v", svc)
	}
	var plat struct {
		Name    string `json:"name"`
		Regions []struct {
			ServiceID string `json:"serviceId"`
			Region    string `json:"region"`
		} `json:"regions"`
	}
	if _, err := db.C("platformConfiguration").Get(ctx, "seed", &plat); err != nil || plat.Name != "ProxCloud" || len(plat.Regions) != 1 || plat.Regions[0].ServiceID != "svc-pve" || plat.Regions[0].Region != "lab" {
		t.Fatalf("platform configuration: %+v %v", plat, err)
	}

	code, items = call(http.MethodPost, applyAllDefaults)
	if code != 200 {
		t.Fatalf("second apply: %d", code)
	}
	for _, it := range items {
		if it.State != "skipped" {
			t.Fatalf("second apply, %s: %s (%s)", it.ID, it.State, it.Message)
		}
	}
	if second := setupDefaultsCounts(t, db); len(second) != len(first) || second["flavor"] != first["flavor"] || second["pricePlan"] != first["pricePlan"] ||
		second["pricePlanRule"] != first["pricePlanRule"] || second["platformConfiguration"] != first["platformConfiguration"] {
		t.Fatalf("counts changed: %v then %v", first, second)
	}
	if code, items = call(http.MethodGet, ""); code != 200 {
		t.Fatalf("preview after apply: %d", code)
	}
	for _, it := range items {
		if it.State != "done" || len(it.Preview) != 0 {
			t.Fatalf("preview after apply, %s: %s", it.ID, it.State)
		}
	}
}

// Two operators applying at the same moment get one price plan: every enabled public plan is billed, so two would
// charge every resource twice.
func TestAdminSetupDefaults_ConcurrentAppliesGiveOnePlan(t *testing.T) {
	db, call := setupDefaultsEnv(t)
	const n = 8
	var wg sync.WaitGroup
	created := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, items := call(http.MethodPost, applyAllDefaults)
			if code != 200 {
				created[i] = -1
				return
			}
			for _, it := range items {
				if it.ID == "pricePlan" && it.State == "created" {
					created[i] = 1
				}
			}
		}(i)
	}
	wg.Wait()
	winners := 0
	for _, c := range created {
		if c < 0 {
			t.Fatal("an apply failed")
		}
		winners += c
	}
	if got := setupDefaultsCounts(t, db); winners != 1 || got["pricePlan"] != 1 || got["pricePlanRule"] != 7 || got["flavor"] != 3 {
		t.Fatalf("%d applies created the plan; counts %v", winners, got)
	}
}

// An enabled public plan of the operator's own is left alone and no default is added next to it; a failed apply
// (unknown provider) writes nothing at all.
func TestAdminSetupDefaults_ExistingPlanAndUnknownProvider(t *testing.T) {
	ctx := context.Background()
	db, call := setupDefaultsEnv(t)
	if _, err := db.C("pricePlan").InsertOne(ctx, pgdoc.M{"_id": "mine", "name": "Mine", "enabled": true, "accessMode": "PUBLIC"}); err != nil {
		t.Fatal(err)
	}
	code, items := call(http.MethodPost, `{"serviceId":"svc-pve","items":["pricePlan","flavors"],"confirmRates":true}`)
	if code != 200 || len(items) != 2 || items[0].ID != "flavors" || items[0].State != "created" || items[1].State != "skipped" || !strings.Contains(items[1].Message, "Mine") {
		t.Fatalf("apply: %d %+v", code, items)
	}
	if got := setupDefaultsCounts(t, db); got["pricePlan"] != 1 || got["pricePlanRule"] != 0 {
		t.Fatalf("counts: %v", got)
	}
	if code, _ := call(http.MethodPost, `{"serviceId":"gone","items":["quota"]}`); code != 404 {
		t.Fatalf("unknown provider: %d", code)
	}
}
