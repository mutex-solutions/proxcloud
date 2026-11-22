//go:build integration

package project

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
)

// One apply of a pair: the vrrp rule and the VIP in both appliances' sets before keepalived anywhere (C14), the
// standby before the appliance holding the address, ACTIVE when one of two answering appliances holds it; nothing
// written when nothing changed; DEGRADED with the split named when both hold it.
func TestLBPairApplyOrderAndStatus(t *testing.T) {
	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("lb"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"), tcpostgres.BasicWaitStrategies())
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

	f := newPairWire(t, "")
	srv := httptest.NewServer(f)
	defer srv.Close()
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cloud: cloud.NewRepo(db), sdnLock: lock.New(db), publicIPs: NewPublicIPRepo(db),
		pveFor: func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil }}
	es := &externalservice.ExternalService{ID: "svc", Config: map[string]any{"network": map[string]any{"publicNetworks": []any{
		map[string]any{"id": "vmbr1", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1", "ranges": []any{"203.0.113.10-203.0.113.20"}}}}}}
	proj := &Project{ID: "p1"}
	lb := &loadBalancer{ID: "abc", Name: "web", Status: lbStatusBuild, NetworkID: "v1", PublicNetworkID: "vmbr1", HA: true,
		ServerID: "301", PrivateIP: "10.0.0.31", PrivatePrefix: 24, PublicIP: "203.0.113.10", VIPs: []string{"203.0.113.10"},
		Appliances: []lbAppliance{{ServerID: "301", PrivateIP: "10.0.0.31"}, {ServerID: "302", PrivateIP: "10.0.0.32", HoldsVIP: true}},
		LBSpec:     pve.LBSpec{Listeners: []pve.LBListener{{Port: 80, Pool: "web"}}, Pools: []pve.LBPool{{Name: "web", Protocol: "http", Algorithm: "roundrobin"}}}}
	row := &cloud.CloudResource{ExternalID: "lb-abc", ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(row, lb); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cloud.Insert(ctx, row); err != nil {
		t.Fatal(err)
	}

	status, err := h.pveLBApply(ctx, es, proj, "lb-abc")
	if err != nil || status != lbStatusActive {
		t.Fatalf("apply: %s %v\n%s", status, err, strings.Join(f.log, "\n"))
	}
	t.Logf("writes of the first apply:\n%s", strings.Join(f.log, "\n"))
	firstKeepalived := min(f.first("301 write /etc/keepalived/keepalived.conf.new"), f.first("302 write /etc/keepalived/keepalived.conf.new"))
	for _, before := range []string{"301 rule vrrp", "302 rule vrrp", "301 ipset 203.0.113.10", "302 ipset 203.0.113.10"} {
		if i := f.first(before); i < 0 || i > firstKeepalived {
			t.Errorf("%q at %d, keepalived first written at %d:\n%s", before, i, firstKeepalived, strings.Join(f.log, "\n"))
		}
	}
	if f.first("301 keepalived reload") > f.first("302 keepalived reload") || f.first("301 haproxy reload") > f.first("302 haproxy reload") {
		t.Errorf("the appliance holding the address was pushed before the standby:\n%s", strings.Join(f.log, "\n"))
	}
	for _, vm := range []string{"301", "302"} {
		peer := map[string]string{"301": "10.0.0.32", "302": "10.0.0.31"}[vm]
		if !slices.ContainsFunc(f.rules[vm], func(r pve.FirewallRule) bool { return r.Proto == "vrrp" && r.Source == peer }) ||
			!slices.ContainsFunc(f.rules[vm], func(r pve.FirewallRule) bool { return r.Dest == "203.0.113.10" && r.DPort == "80" }) {
			t.Errorf("%s rules: %+v", vm, f.rules[vm])
		}
	}
	saved, _ := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", "lb-abc")
	got, _ := lbOfRow(saved)
	for _, a := range got.Appliances {
		if a.State != lbAppUp || a.HAProxyHash == "" || a.KeepalivedHash == "" || a.TenantIface != "ens18" || a.PublicIface != "ens19" {
			t.Errorf("appliance after apply: %+v", a)
		}
	}

	// Nothing changed: nothing is written.
	f.mu.Lock()
	writes := len(f.log)
	f.mu.Unlock()
	if status, err := h.pveLBApply(ctx, es, proj, "lb-abc"); err != nil || status != lbStatusActive || len(f.log) != writes {
		t.Fatalf("second apply: %s %v, writes %v", status, err, f.log[writes:])
	}

	// Both hold the address: DEGRADED, said so, nothing done about it.
	f.mu.Lock()
	f.guests["301"].role, f.guests["301"].holds = "MASTER", true
	f.mu.Unlock()
	status, _ = h.pveLBApply(ctx, es, proj, "lb-abc")
	saved, _ = h.cloud.FindByServiceIDAndExternalID(ctx, "svc", "lb-abc")
	got, _ = lbOfRow(saved)
	if status != lbStatusDegraded || !strings.Contains(got.StatusMessage, "Both appliances hold the address") || len(f.log) != writes {
		t.Fatalf("split: %s %q, writes %v", status, got.StatusMessage, f.log[writes:])
	}

	// A replace is refused while one runs, and so is a delete, which would leave the VM being built to nobody; a
	// mark from a portal that died long ago holds nothing.
	got.Replacing = time.Now().UTC().Format(time.RFC3339)
	if err := h.lbSave(ctx, saved, got); err != nil {
		t.Fatal(err)
	}
	if herr := h.pveLBReplace(ctx, nil, es, proj, saved); herr == nil || herr.Status != http.StatusConflict || !strings.Contains(herr.Msg, "already being replaced") {
		t.Fatalf("second replace: %v", herr)
	}
	if err := h.pveDeleteLoadBalancer(ctx, es, proj, saved); err == nil || !strings.Contains(err.Error(), "being replaced") {
		t.Fatalf("delete during a replace: %v", err)
	}
	// A single appliance has nothing to replace.
	single := &cloud.CloudResource{ExternalID: "lb-def", ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(single, &loadBalancer{ID: "def", Name: "one", Status: lbStatusActive, ServerID: "401"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cloud.Insert(ctx, single); err != nil {
		t.Fatal(err)
	}
	if herr := h.pveLBReplace(ctx, nil, es, proj, single); herr == nil || herr.Status != http.StatusConflict || !strings.Contains(herr.Msg, "one VM") {
		t.Fatalf("replace on a single appliance: %v", herr)
	}
	// UDP through UPDATE_CONFIG. A balancer on one VM refuses it by name and its row stays as it was (it has no
	// keepalived to fill the kernel's table); a pair takes the listener into its row.
	udp := map[string]any{
		"listeners": []any{map[string]any{"port": float64(53), "protocol": "udp", "pool": "dns"}},
		"pools":     []any{map[string]any{"name": "dns", "members": []any{}}}}
	oneVM := &cloud.CloudResource{ExternalID: "lb-one", ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
	if err := setLBRow(oneVM, &loadBalancer{ID: "one", Name: "one", Status: lbStatusActive, NetworkID: "v1", PublicNetworkID: "vmbr1", ServerID: "401",
		PrivateIP: "10.0.0.41", PrivatePrefix: 24, PublicIP: "203.0.113.11", VIPs: []string{"203.0.113.11"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cloud.Insert(ctx, oneVM); err != nil {
		t.Fatal(err)
	}
	if err := h.pveLBUpdateConfig(ctx, es, proj, oneVM, udp); err == nil || !strings.Contains(err.Error(), "UDP needs a high-availability load balancer") {
		t.Fatalf("a UDP listener on a load balancer on one VM: %v", err)
	}
	for id, want := range map[string]bool{"lb-one": false, "lb-abc": true} {
		if want {
			if err := h.pveLBUpdateConfig(ctx, es, proj, &cloud.CloudResource{ExternalID: id}, udp); err != nil {
				t.Fatalf("a UDP listener on a pair: %v", err)
			}
		}
		kept, err := h.cloud.FindByServiceIDAndExternalID(ctx, "svc", id)
		if err != nil || kept == nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got, err := lbOfRow(kept); err != nil || got.HasUDP() != want {
			t.Fatalf("%s holds a UDP listener: %v, want %v (%v)", id, got.HasUDP(), want, err)
		}
	}
}
