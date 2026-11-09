//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/csigw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

func gatewayClient(url string) func(context.Context, string) (*pve.Client, error) {
	return func(context.Context, string) (*pve.Client, error) {
		return pve.New(pve.Config{APIURL: url, TokenID: "csi@pve!gateway", Secret: "x"})
	}
}

func fileVol(name, state string, at time.Time) *csigw.Volume {
	return &csigw.Volume{ID: csigw.VolumeID("svc", "nfs", name), ClusterID: "c1", ProjectID: "p1", ComputeServiceID: "svc",
		Storage: "nfs", Node: "pve1", VolID: "nfs:" + name, SizeBytes: 1 << 30, State: state, CreatedAt: &at}
}

// A create whose PVE answer and confirm read were both lost leaves a creating row (createFailed) while its disk exists.
// Past the grace the reconciler makes it ready and bills it, also on a file store, where PVE lists the disk under
// "<vmid>/" and the row takes that volid. A creating row inside the grace is its create's, and is left alone.
func TestCSIGatewayCreatingWithDiskSettles(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	st := csigw.NewStore(db)
	repo := cloud.NewRepo(db)
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	for _, v := range []*csigw.Volume{
		csiVol("c1", "p1", "vm-9999-pvc-orphan", csigw.StateCreating, 1<<30, old),
		fileVol("vm-9999-pvc-f.raw", csigw.StateCreating, old),
		csiVol("c1", "p1", "vm-9999-pvc-young", csigw.StateCreating, 1<<30, now),
	} {
		if err := st.InsertVolume(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	fs := &fakeStore{disks: map[string]int64{"local-zfs:vm-9999-pvc-orphan": 1 << 30, "nfs:9999/vm-9999-pvc-f.raw": 2 << 30,
		"local-zfs:vm-9999-pvc-young": 1 << 30}}
	srv := fs.server()
	defer srv.Close()
	rows := &csigw.VolumeRows{Reg: st, ServiceID: "svc", Project: "p1", Region: "r", GW: gatewayClient(srv.URL)}
	for i := 0; i < 2; i++ {
		if _, err := providers.Reconcile(ctx, rows, repo, "svc", now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := st.Volume(ctx, csigw.VolumeID("svc", "local-zfs", "vm-9999-pvc-orphan")); v == nil || v.State != csigw.StateReady {
		t.Errorf("creating row an hour old with its disk on PVE: %+v, want ready", v)
	}
	if v, _ := st.Volume(ctx, csigw.VolumeID("svc", "nfs", "vm-9999-pvc-f.raw")); v == nil || v.State != csigw.StateReady ||
		v.VolID != "nfs:9999/vm-9999-pvc-f.raw" || v.SizeBytes != 2<<30 {
		t.Errorf("file store row: %+v, want ready with PVE's volid and size", v)
	}
	if v, _ := st.Volume(ctx, csigw.VolumeID("svc", "local-zfs", "vm-9999-pvc-young")); v == nil || v.State != csigw.StateCreating {
		t.Errorf("creating row inside the grace: %+v, want creating", v)
	}
	cached, _ := repo.FindByServiceProjectAndType(ctx, "svc", "p1", cloud.TypeVolume)
	if len(cached) != 2 {
		t.Errorf("billed rows %d, want 2", len(cached))
	}
}

// On a file store a creating row holds "<s>:<name>" and PVE lists "<s>:9999/<name>": the release must find and delete
// that disk, not close the row and leave it on the store with no owner.
func TestCSIGatewayReleaseFileStoreCreating(t *testing.T) {
	ctx := context.Background()
	st := csigw.NewStore(freshPG(t))
	v := fileVol("vm-9999-pvc-f.raw", csigw.StateCreating, time.Now().UTC().Add(-time.Hour))
	if err := st.InsertVolume(ctx, v); err != nil {
		t.Fatal(err)
	}
	fs := &fakeStore{disks: map[string]int64{"nfs:9999/vm-9999-pvc-f.raw": 1 << 30}}
	srv := fs.server()
	defer srv.Close()
	if err := st.ReleaseCluster(ctx, "c1", gatewayClient(srv.URL)); err != nil {
		t.Fatal(err)
	}
	if _, left := fs.disks["nfs:9999/vm-9999-pvc-f.raw"]; left {
		t.Errorf("release closed the row but the disk stays on the store: deletes %v", fs.deletes)
	}
	if got, _ := st.Volume(ctx, v.ID); got == nil || got.State != csigw.StateDeleted {
		t.Errorf("row after the release: %+v, want deleted", got)
	}
}
