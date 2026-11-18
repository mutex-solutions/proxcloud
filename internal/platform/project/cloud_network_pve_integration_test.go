//go:build integration

package project

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// F2 after ADD_SUBNET / REMOVE_SUBNET / SET_SNAT: the row is refreshed from the vnet's live zone. A network made
// before v0.8.1 in a simple zone without a DHCP server has a dhcp-range and a row that says dhcp true; the refresh
// must store false, not write true back until the next sync.
func TestPVESubnetRefreshFollowsTheLiveZone(t *testing.T) {
	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("net"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = testcontainers.TerminateContainer(pgc) }()
	dsn, _ := pgc.ConnectionString(ctx, "sslmode=disable")
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api2/json/cluster/sdn/zones":
			fmt.Fprint(w, `{"data":[{"zone":"pc","type":"simple"}]}`)
		case "GET /api2/json/cluster/sdn/vnets":
			fmt.Fprint(w, `{"data":[{"vnet":"v1","zone":"pc","alias":"st:p1:web"}]}`)
		case "GET /api2/json/cluster/sdn/vnets/v1/subnets":
			fmt.Fprint(w, `{"data":[{"cidr":"10.100.0.0/24","gateway":"10.100.0.1","snat":1,"dhcp-range":[{"start-address":"10.100.0.10","end-address":"10.100.0.254"}]}]}`)
		default:
			t.Errorf("reached PVE: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cloud: cloud.NewRepo(db)}
	cr := &cloud.CloudResource{ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeNetwork, ExternalID: "v1",
		Data: map[string]any{"network": map[string]any{"name": "web", "zone": "pc", "dhcp": true, "snat": false}}}
	if err := h.pveRefreshSubnetRow(ctx, ps, cr); err != nil {
		t.Fatal(err)
	}
	stored, err := h.cloud.FindByID(ctx, cr.ID)
	if err != nil || stored == nil {
		t.Fatalf("stored row: %v, %v", stored, err)
	}
	net, _ := stored.Data["network"].(map[string]any)
	if net["dhcp"] != false || net["snat"] != true || net["cidr"] != "10.100.0.0/24" || net["name"] != "web" {
		t.Fatalf("stored network: %v", net)
	}
}
