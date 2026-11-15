//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

type setupStatusStep struct {
	ID, State, Summary string
	Items              []struct{ ID, State, Message string }
}

// setupStatusEnv is a database as the Compose seed leaves it (the billing and platform configuration documents), plus
// the given documents, behind the real admin routes with the database-backed repositories.
func setupStatusEnv(t *testing.T, docs map[string][]pgdoc.M) (*pgdoc.DB, func(method, path, body string) (int, []setupStatusStep)) {
	t.Helper()
	ctx := context.Background()
	db := freshPG(t)
	docs["billingConfiguration"] = append(docs["billingConfiguration"], pgdoc.M{"_id": "seed", "baseCurrency": "USD", "defaultConfiguration": true, "promotionCodesEnabled": true})
	docs["platformConfiguration"] = append(docs["platformConfiguration"], pgdoc.M{"_id": "seed", "name": "ProxCloud", "defaultConfiguration": true, "regions": []any{}})
	for table, list := range docs {
		for _, doc := range list {
			if _, err := db.C(table).InsertOne(ctx, doc); err != nil {
				t.Fatal(err)
			}
		}
	}
	const iss, cid = "test-iss", "test-cid"
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key"))
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, pricing.NewRepo(db), billing.NewRepo(db), nil, nil, nil, esSvc, "", iss, cid)
	h.SetPVEFlavors(pveflavor.New(db))
	r := chi.NewRouter()
	h.Routes(r)
	return db, func(method, path, body string) (int, []setupStatusStep) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var env struct {
			Data struct {
				Steps []setupStatusStep `json:"steps"`
			} `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		return w.Code, env.Data.Steps
	}
}

func setupStatusStates(steps []setupStatusStep) map[string]string {
	out := map[string]string{}
	for _, st := range steps {
		out[st.ID] = st.State
		for _, it := range st.Items {
			out[st.ID+"/"+it.ID] = it.State
		}
	}
	return out
}

// A fresh Compose install shows nothing done: the seeded billing and platform rows are not setup work. Seven steps:
// the six of the wizard and the optional managed step.
func TestAdminSetupStatus_FreshCompose(t *testing.T) {
	_, call := setupStatusEnv(t, map[string][]pgdoc.M{})
	code, steps := call(http.MethodGet, "/admin/setup/status", "")
	if code != 200 || len(steps) != 7 {
		t.Fatalf("status: %d %+v", code, steps)
	}
	for id, state := range setupStatusStates(steps) {
		if state == "done" {
			t.Errorf("fresh install: %s is done", id)
		}
	}
	if code, _ := call(http.MethodGet, "/admin/setup/status?serviceId=gone", ""); code != 404 {
		t.Fatalf("unknown provider: %d", code)
	}
}

// The catalog step reads the database: a public plan scoped to another provider does not price this one, the defaults
// apply turns flavors, quota and region done, and an unscoped public plan with a server rule turns the price plan done.
func TestAdminSetupStatus_CatalogAfterDefaults(t *testing.T) {
	ctx := context.Background()
	db, call := setupStatusEnv(t, map[string][]pgdoc.M{
		"externalService": {
			{"_id": "svc-pve", "name": "lab", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{"provider": "proxmox", "region": "lab", "apiUrl": "https://pve.invalid:8006", "attachToNewProjects": false}},
		},
		"pricePlan":     {{"_id": "other", "name": "Other site", "enabled": true, "accessMode": "PUBLIC", "serviceProviders": []any{pgdoc.M{"serviceId": "svc-other"}}}},
		"pricePlanRule": {{"_id": "other-cpu", "pricePlanId": "other", "name": "cpu", "resourceType": "instance", "timeUnit": "hour"}},
	})
	const path = "/admin/setup/status?serviceId=svc-pve"
	code, steps := call(http.MethodGet, path, "")
	got := setupStatusStates(steps)
	if code != 200 || got["catalog/pricePlan"] != "todo" || got["catalog/flavors"] != "todo" || got["connect"] != "done" || got["golive"] != "blocked" {
		t.Fatalf("before the defaults: %d %v", code, got)
	}
	if code, _ := call(http.MethodPost, "/admin/setup/defaults", `{"serviceId":"svc-pve","items":["flavors","quota","region"]}`); code != 200 {
		t.Fatalf("apply: %d", code)
	}
	code, steps = call(http.MethodGet, path, "")
	got = setupStatusStates(steps)
	for _, id := range []string{"catalog/flavors", "catalog/quota", "catalog/region"} {
		if got[id] != "done" {
			t.Errorf("after the defaults, %s: %s", id, got[id])
		}
	}
	if code != 200 || got["catalog/pricePlan"] != "todo" || got["catalog/billingDetails"] != "attention" {
		t.Fatalf("after the defaults: %d %v", code, got)
	}
	for table, doc := range map[string]pgdoc.M{
		"pricePlan":     {"_id": "all", "name": "Everywhere", "enabled": true, "accessMode": "PUBLIC"},
		"pricePlanRule": {"_id": "all-cpu", "pricePlanId": "all", "name": "cpu", "resourceType": "instance", "timeUnit": "hour"},
	} {
		if _, err := db.C(table).InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	if _, steps = call(http.MethodGet, path, ""); setupStatusStates(steps)["catalog/pricePlan"] != "done" {
		t.Fatalf("with an unscoped plan: %v", setupStatusStates(steps))
	}
}
