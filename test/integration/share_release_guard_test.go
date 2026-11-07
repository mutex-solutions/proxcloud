//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

// A cluster address on a project network is on no export, so a broken NAS must not hold it back. One on a shared worker
// pool or a public address is on every export, so the same NAS must: freeing it would hand that access to the
// next tenant to get the address.
func TestAddressReleaseGuardsOnlyWhatAnExportCanList(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	const compute, nas, kam = "svccompute", "svcnas", "svckamaji"
	seed := func(id string, cfg pgdoc.M) {
		if _, err := db.C("externalService").InsertOne(ctx, pgdoc.M{"_id": id, "name": id, "config": cfg}); err != nil {
			t.Fatal(err)
		}
	}
	seed(compute, pgdoc.M{"provider": "proxmox"})
	seed(nas, pgdoc.M{"provider": "truenas", "apiUrl": "ws://127.0.0.1:1/api/current", "username": "x", "parentDataset": "tank/shares", "mountHost": "nas.invalid"})
	seed(kam, pgdoc.M{"provider": "kamaji", "cluster": pgdoc.M{"sharedWorkers": pgdoc.M{"bridge": "vmbr-shared", "cidr": "198.51.100.0/24", "gateway": "198.51.100.1"}}})
	if _, err := db.C("project").InsertOne(ctx, pgdoc.M{"_id": "p1", "status": "ENABLED", "services": []pgdoc.M{
		{"serviceId": compute}, {"serviceId": nas}, {"serviceId": kam},
	}}); err != nil {
		t.Fatal(err)
	}
	es := externalservice.NewService(externalservice.NewRepo(db), nil)
	h := project.NewHandler(project.NewService(project.NewRepo(db), nil, nil, nil, nil), nil, nil, nil, nil, nil, cloud.NewRepo(db), es, nil, nil, "")
	ips := project.NewPublicIPRepo(db)
	h.SetPublicIPs(ips)

	row := func(id, network, nic, owner string) string {
		if _, err := db.C("pvePublicIp").InsertOne(ctx, pgdoc.M{"_id": id, "serviceId": compute, "networkId": network, "ip": id, "projectId": "p1", "serverId": owner, "nic": nic}); err != nil {
			t.Fatal(err)
		}
		return owner
	}
	left := func(owner string) int {
		rows, err := ips.ForServer(ctx, compute, owner)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}

	onNet := row("10.1.0.5", "vnet1", project.ReservedNIC, "cluster-a")
	if err := ips.ReleaseServer(ctx, compute, onNet); err != nil || left(onNet) != 0 {
		t.Fatalf("project network: err %v, rows left %d", err, left(onNet))
	}
	onShared := row("198.51.100.5", "vmbr-shared", project.ReservedNIC, "cluster-b")
	if err := ips.ReleaseServer(ctx, compute, onShared); err == nil || left(onShared) != 1 {
		t.Fatalf("shared pool must fail closed: err %v, rows left %d", err, left(onShared))
	}
	public := row("203.0.113.5", "vmbr1", "net1", "server-a")
	if err := ips.ReleaseServer(ctx, compute, public); err == nil || left(public) != 1 {
		t.Fatalf("public address must fail closed: err %v, rows left %d", err, left(public))
	}
}
