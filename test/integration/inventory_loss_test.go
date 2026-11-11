//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// The actual DB + real PVE wire + real TrueNAS export reconciler reproduce the
// incident: a 200 empty (or partial) list is not a deletion authorization.
func TestNetworkInventoryLossPreservesRowsAndExports(t *testing.T) {
	for _, mode := range []string{"empty403", "empty404", "partial404", "omitted-live", "subnet403", "subnet-empty"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newShareAccessFixture(t)
			repo := cloud.NewRepo(f.db)
			past := time.Now().UTC().Add(-time.Hour)
			// Include zone and all CIDRs like the production provider does.
			first, err := repo.FindByServiceIDAndExternalID(ctx, "svc-pve", "v1")
			if err != nil || first == nil {
				t.Fatalf("initial network: %+v %v", first, err)
			}
			first.Data["network"].(map[string]any)["zone"] = "overlay"
			first.CreatedAt, first.UpdatedAt = &past, &past
			if _, err := repo.Insert(ctx, first); err != nil {
				t.Fatal(err)
			}
			second, err := repo.Insert(ctx, &cloud.CloudResource{ServiceID: "svc-pve", ProjectID: f.projectID, ExternalID: "v2", Type: cloud.TypeNetwork,
				CreatedAt: &past, UpdatedAt: &past,
				Data: pve.NetworkData("v2", "second", "overlay", map[string]any{"cidr": "10.100.2.0/24", "subnets": []any{"10.100.2.0/24"}})})
			if err != nil {
				t.Fatal(err)
			}
			f.nas.networks = []string{"10.100.0.0/24", "10.100.1.0/24", "10.100.2.0/24", "10.89.0.10/32"}
			beforeExports := slices.Clone(f.nas.networks)
			phase := "good"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "PVEAPIToken=portal@pve!inventory=key" || r.URL.Query().Get("userid") != "" {
					t.Error("must use configured self credential")
					w.WriteHeader(403)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
				live := func(id string) map[string]any {
					return map[string]any{"vnet": id, "zone": "overlay", "alias": pve.OwnershipAlias(f.projectID, id)}
				}
				path := strings.TrimPrefix(r.URL.Path, "/api2/json")
				switch {
				case path == "/access/acl":
					write([]any{})
				case path == "/access/permissions":
					if r.URL.Query().Get("path") == "/access" {
						write(map[string]any{"/access": map[string]int{"Sys.Audit": 0}})
						return
					}
					privs := map[string]int{"SDN.Audit": 1}
					if phase == "lost" && (mode == "empty403" || mode == "empty404" || mode == "partial404") && strings.HasSuffix(r.URL.Query().Get("path"), "/v2") {
						privs = map[string]int{} // specific NoAccess shadows the still-good namespace
					}
					write(map[string]any{r.URL.Query().Get("path"): privs})
				case path == "/cluster/sdn/vnets":
					list := []any{live("v1"), live("v2")}
					if phase == "deleted" {
						list = []any{}
					}
					if phase == "lost" {
						switch mode {
						case "empty403", "empty404", "omitted-live":
							list = []any{}
						case "partial404":
							list = []any{live("v1")}
						}
					}
					write(list)
				case strings.HasSuffix(path, "/subnets"):
					if phase == "lost" && mode == "subnet403" {
						w.WriteHeader(403)
						return
					}
					var rows []any
					if !(phase == "lost" && mode == "subnet-empty") {
						cidrs := []string{"10.100.2.0/24"}
						if strings.Contains(path, "/v1/") {
							cidrs = []string{"10.100.0.0/24", "10.100.1.0/24"}
						}
						for _, cidr := range cidrs {
							rows = append(rows, map[string]any{"cidr": cidr, "subnet": "overlay-" + strings.ReplaceAll(cidr, "/", "-")})
						}
					}
					if rows == nil {
						rows = []any{}
					}
					write(rows)
				case strings.Contains(path, "/subnets/"):
					write(map[string]any{"cidr": "10.100.1.0/24"}) // omitted, not deleted
				case strings.HasPrefix(path, "/cluster/sdn/vnets/"):
					if phase == "deleted" || phase == "lost" && (mode == "empty404" || mode == "partial404") {
						w.WriteHeader(404)
						return
					}
					if phase == "lost" && mode == "empty403" {
						w.WriteHeader(403)
						return
					}
					write(live(strings.TrimPrefix(path, "/cluster/sdn/vnets/")))
				default:
					t.Errorf("unexpected request: %s", path)
					w.WriteHeader(500)
				}
			}))
			t.Cleanup(srv.Close)
			c, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "portal@pve!inventory", Secret: "key"})
			if err != nil {
				t.Fatal(err)
			}
			ps := &pve.Service{C: c}
			// Two consecutive failed rounds must not mutate ANY network or archive.
			phase = "lost"
			for round := 0; round < 2; round++ {
				st, err := providers.Reconcile(ctx, ps.NetworkSyncProvider("r", f.projectID), repo, "svc-pve", time.Now().UTC())
				var untrusted *cloud.InventoryUntrustedError
				if !errors.As(err, &untrusted) || st != (providers.ReconcileStats{}) {
					t.Fatalf("round%d: %+v %v", round, st, err)
				}
				for _, before := range []*cloud.CloudResource{first, second} {
					row, err := repo.FindByServiceIDAndExternalID(ctx, "svc-pve", before.ExternalID)
					if err != nil || row == nil || row.ID != before.ID || !row.UpdatedAt.Equal(past) {
						t.Fatalf("row changed: %+v %v", row, err)
					}
				}
				if err := f.handler.ReconcileShareAccess(ctx, f.projectID); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(f.nas.current(), beforeExports) || f.nas.updates != 0 {
					t.Fatalf("export changed: %v", f.nas.current())
				}
				archived, err := repo.FindArchivedSince(ctx, f.projectID, "svc-pve", past.Add(-time.Minute))
				if err != nil || len(archived) != 0 {
					t.Fatalf("archive %+v %v", archived, err)
				}
			}
			// Recovered visibility makes the normal pass converge, not recreate rows.
			phase = "good"
			for round := 0; round < 2; round++ {
				st, err := providers.Reconcile(ctx, ps.NetworkSyncProvider("r", f.projectID), repo, "svc-pve", time.Now().UTC())
				if err != nil || st.Created != 0 || st.Deleted != 0 || round == 1 && st.Updated != 0 {
					t.Fatalf("recovered: %+v %v", st, err)
				}
			}
			// A real empty inventory, supported by effective rights + by-id404,
			// does archive and remove access: no permanent billing/resource leak.
			phase = "deleted"
			st, err := providers.Reconcile(ctx, ps.NetworkSyncProvider("r", f.projectID), repo, "svc-pve", time.Now().UTC())
			if err != nil || st.Deleted != 2 {
				t.Fatalf("real delete: %+v %v", st, err)
			}
			if err := f.handler.ReconcileShareAccess(ctx, f.projectID); err != nil {
				t.Fatal(err)
			}
			if got := f.nas.current(); !slices.Equal(got, []string{"10.89.0.10/32"}) {
				t.Fatalf("stale access: %v", got)
			}
		})
	}
}

func TestDuplicateNASParentRefusesExportWritesLocally(t *testing.T) {
	ctx := context.Background()
	f := newShareAccessFixture(t)
	var es pgdoc.M
	if _, err := f.db.C("externalService").FindOne(ctx, pgdoc.M{"_id": "svc-nas"}, &es); err != nil {
		t.Fatal(err)
	}
	es["_id"] = "svc-nas-duplicate"
	es["config"].(map[string]any)["snatSources"] = []any{"192.0.2.8"}
	mustInsert(t, f.db, "externalService", es)
	services := []any{pgdoc.M{"serviceId": "svc-pve"}, pgdoc.M{"serviceId": "svc-nas"}, pgdoc.M{"serviceId": "svc-k8s"}}
	if _, err := f.db.C("project").SetByID(ctx, f.projectID, pgdoc.M{"services": append(slices.Clone(services), pgdoc.M{"serviceId": "svc-nas-duplicate"})}, nil); err != nil {
		t.Fatal(err)
	}
	var conflict *externalservice.TrueNASOwnerConflictError
	if err := f.handler.ReconcileShareAccess(ctx, f.projectID); !errors.As(err, &conflict) {
		t.Fatalf("want operator conflict, got %v", err)
	}
	if f.nas.updates != 0 {
		t.Fatal("duplicate providers changed exports")
	}
	if err := f.ips.ReleaseServer(ctx, "svc-pve", "100"); !errors.As(err, &conflict) || f.held(t) != 1 {
		t.Fatalf("release must fail closed: %v", err)
	}
	// The same global providers do not make an unrelated single-owner project
	// unavailable. Detach the duplicate without deleting its provider or datasets.
	if _, err := f.db.C("project").SetByID(ctx, f.projectID, pgdoc.M{"services": services}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.handler.ReconcileShareAccess(ctx, f.projectID); err != nil {
		t.Fatal(err)
	}
	if f.nas.updates != 1 {
		t.Fatalf("single owner did not recover: %d", f.nas.updates)
	}
}

// The verifier runs before creates and vanished deletion. Old providers retain
// their original behavior by not opting in; concurrent update/archive tests follow.
type rejectedInventoryProvider struct{ reconcileProv }

func (p *rejectedInventoryProvider) VerifyInventory(context.Context, []cloud.CloudResource, []cloud.CloudResource) error {
	return &cloud.InventoryUntrustedError{Type: p.typ, Reason: "test visibility failure"}
}
func TestInventoryVerifierRunsBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	repo := cloud.NewRepo(freshPG(t))
	past := time.Now().UTC().Add(-time.Hour)
	old := rcRes("old", map[string]any{"name": "old"})
	old.ServiceID = "svc"
	old.CreatedAt, old.UpdatedAt = &past, &past
	if _, err := repo.Insert(ctx, &old); err != nil {
		t.Fatal(err)
	}
	p := &rejectedInventoryProvider{reconcileProv{typ: cloud.TypeServer, list: []cloud.CloudResource{rcRes("new", map[string]any{"name": "new"})}}}
	st, err := providers.Reconcile(ctx, p, repo, "svc", time.Now().UTC())
	if err == nil || st != (providers.ReconcileStats{}) {
		t.Fatalf("unexpected reconcile %+v %v", st, err)
	}
	rows, err := repo.FindByServiceAndType(ctx, "svc", cloud.TypeServer)
	if err != nil || len(rows) != 1 || rows[0].ExternalID != "old" {
		t.Fatalf("rows: %+v %v", rows, err)
	}
}
