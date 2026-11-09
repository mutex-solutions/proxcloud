//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/csigw"
)

// The migration reads who holds each name on a store from StorageVolumes: every cluster's rows on that provider and store,
// none of another provider's or store's, none deleted.
func TestCSIGatewayStorageVolumes(t *testing.T) {
	ctx := context.Background()
	st := csigw.NewStore(freshPG(t))
	rows := []csigw.Volume{
		{ClusterID: "pc-c-a", ComputeServiceID: "svc", Storage: "local-zfs", VolID: "local-zfs:vm-9999-pvc-a", State: csigw.StateReady},
		{ClusterID: "pc-c-b", ComputeServiceID: "svc", Storage: "local-zfs", VolID: "local-zfs:vm-9999-pvc-b", State: csigw.StateCreating},
		{ClusterID: "pc-c-c", ComputeServiceID: "svc", Storage: "local-zfs", VolID: "local-zfs:vm-9999-pvc-c", State: csigw.StateDeleted},
		{ClusterID: "pc-c-d", ComputeServiceID: "svc", Storage: "nas", VolID: "nas:9999/vm-9999-pvc-d.raw", State: csigw.StateReady},
		{ClusterID: "pc-c-e", ComputeServiceID: "other", Storage: "local-zfs", VolID: "local-zfs:vm-9999-pvc-e", State: csigw.StateReady},
	}
	for i := range rows {
		v := rows[i]
		v.ID = csigw.VolumeID(v.ComputeServiceID, v.Storage, v.VolID[strings.LastIndexAny(v.VolID, ":/")+1:])
		if err := st.InsertVolume(ctx, &v); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.StorageVolumes(ctx, "svc", "local-zfs")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, v := range got {
		seen[v.ClusterID] = true
	}
	if len(got) != 2 || !seen["pc-c-a"] || !seen["pc-c-b"] {
		t.Fatalf("StorageVolumes = %+v", got)
	}
}
