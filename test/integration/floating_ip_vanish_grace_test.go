//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/syncjob"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// The FLOATING_IP reconcile reads the allocations, then the rows. A balancer's pass that takes an address in between
// has written a row the list does not have, and the reconcile archived it: the address was missing from the tenant's
// list and from billing until the next pass or sync wrote it again (v0.11.1, Known issues). The grace has another
// half: a row archived a moment ago and still listed is not written again. A sync already refused one archived after
// it read its clock, so the row here is archived before that, as by a replica whose clock is behind. Each case has
// its control: past the two minutes the list is believed.
func TestFloatingIPRowVanishGrace(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := cloud.NewRepo(db)
	s := vmTestService(t, &vmInventoryWire{mode: "empty", full: true})
	mustInsert(t, db, "project", pgdoc.M{"_id": "p", "name": "fixture", "status": "ENABLED", "services": []any{pgdoc.M{"serviceId": "s"}}})
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "s", "name": "fixture", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{"provider": "proxmox", "region": "r"}})

	now := time.Now().UTC()
	// alloc is an address a balancer took age ago, with the row its pass wrote for it (lbAddressRows).
	alloc := func(ip string, age time.Duration) (project.PublicIP, *cloud.CloudResource) {
		t.Helper()
		at := now.Add(-age)
		a := project.PublicIP{ID: "s/pub/" + ip, ServiceID: "s", ProjectID: "p", ServerID: project.LBAddressOwner("abc"), NetworkID: "pub", IP: ip, Prefix: 24, NIC: "net1", CreatedAt: &at}
		row := project.FloatingIPRow(a, "r")
		row.UpdatedAt = &at
		out, err := repo.Insert(ctx, &row)
		if err != nil {
			t.Fatal(err)
		}
		return a, out
	}
	// Taken after the list was read: a row, and no line in the list.
	alloc("198.51.100.10", 10*time.Second)
	alloc("198.51.100.11", 3*time.Minute)
	// Given back, and still in the list the sync read: a line in the list, and the row archived.
	var listed []project.PublicIP
	for ip, ago := range map[string]time.Duration{"198.51.100.12": 10 * time.Second, "198.51.100.13": 3 * time.Minute} {
		a, row := alloc(ip, time.Hour)
		if err := repo.DeleteAndArchive(ctx, row, now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
		listed = append(listed, a)
	}

	es := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k"))
	j := syncjob.New(project.NewRepo(db), es, repo, nil).
		WithProxmoxFactory(func(*externalservice.ExternalService) (*pve.Service, error) { return s, nil }).
		WithPublicIPs(func(context.Context, string, string) ([]project.PublicIP, error) { return listed, nil }, func(context.Context, string, string, string) error { return nil })
	if _, err := j.RunPVEState(ctx); err != nil {
		t.Fatal(err)
	}

	fips, err := repo.FindByServiceProjectAndType(ctx, "s", "p", cloud.TypeFloatingIP)
	if err != nil {
		t.Fatal(err)
	}
	has := map[string]bool{}
	for _, r := range fips {
		fip, _ := r.Data["floatingIp"].(map[string]any)
		has[fip["floating_ip_address"].(string)] = true
	}
	for _, c := range []struct {
		ip   string
		want bool
		why  string
	}{
		{"198.51.100.10", true, "a row 10 s old that the list lacks is the pass's: it stays"},
		{"198.51.100.11", false, "control: a row 3 min old that the list lacks is archived"},
		{"198.51.100.12", false, "a row archived 10 s ago and still listed is not written again"},
		{"198.51.100.13", true, "control: a row archived 3 min ago and still listed is written again"},
	} {
		if has[c.ip] != c.want {
			t.Errorf("%s has a row = %v, want %v: %s", c.ip, has[c.ip], c.want, c.why)
		}
	}
	if len(fips) != 2 {
		t.Errorf("FLOATING_IP rows = %d, want 2", len(fips))
	}
}
