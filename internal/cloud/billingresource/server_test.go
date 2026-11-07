package billingresource

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

func TestVolumeBackedServerDoesNotBillFlavorRootDisk(t *testing.T) {
	cr := &cloud.CloudResource{
		ID:        "server-1",
		ProjectID: "project-1",
		Data: map[string]any{
			"volumeBacked": true,
			"server": map[string]any{
				"flavor": map[string]any{"disk": 80, "ram": 4096, "vcpus": 2},
			},
		},
	}

	got := (&ServerProvider{}).instanceBR(cr, false)
	if got == nil || got.Values["root_disk_gb"] != 0 {
		t.Fatalf("root_disk_gb = %#v, want 0 for a volume-backed root", got)
	}
}

func TestSnapshotBillingResources(t *testing.T) {
	row := func(sn any) *cloud.CloudResource {
		srv := map[string]any{"name": "web", "flavor": map[string]any{}}
		if sn != nil {
			srv["snapshots"] = sn
		}
		old := time.Unix(1000, 0).UTC()
		return &cloud.CloudResource{ID: "s1", ProjectID: "p1", CreatedAt: &old, Data: map[string]any{"server": srv}}
	}
	if brs := snapshotBRs(row(nil)); len(brs) != 0 {
		t.Fatalf("a server without snapshots must not bill any: %+v", brs)
	}
	at := func(sec int64) string { return time.Unix(sec, 0).UTC().Format(time.RFC3339) }
	// A stored row comes back from JSON as float64, a fresh one as int.
	for _, sn := range []map[string]any{
		{"diskGb": 40, "items": []any{map[string]any{"name": "a", "createdAt": at(5000)}, map[string]any{"name": "b", "createdAt": at(6000)}}},
		{"diskGb": float64(40), "items": []any{map[string]any{"name": "a", "createdAt": at(5000)}, map[string]any{"name": "b", "createdAt": at(6000)}}},
	} {
		cr := row(sn)
		brs := snapshotBRs(cr)
		if len(brs) != 2 || brs[0].ResourceType != "server_snapshot" || brs[0].ResourceID != "server_snapshot-s1-a-5000" {
			t.Fatalf("%+v", brs)
		}
		if brs[1].Values["size_gb"] != float64(40) || brs[1].Values["display_name"] != "web / b" {
			t.Fatalf("%+v", brs[1].Values)
		}
		stampResourceValues(brs, cr)
		// The first watermark of a bill item is CreatedAt: it must be the snapshot's, not the server's,
		// or a snapshot on an old server is charged back to the start of the month.
		if !brs[0].CreatedAt.Equal(time.Unix(5000, 0)) || !brs[1].CreatedAt.Equal(time.Unix(6000, 0)) {
			t.Fatalf("%v %v", brs[0].CreatedAt, brs[1].CreatedAt)
		}
		if brs[0].Values["createdAt"] != time.Unix(5000, 0).UTC() {
			t.Fatalf("%+v", brs[0].Values)
		}
	}
	brs, err := (&ServerProvider{}).GetBillingInformation(context.Background(), billingapi.BillingContext{}, &cloud.CloudResource{
		Data: map[string]any{"server": map[string]any{"status": "DELETED", "name": "web", "snapshots": map[string]any{"diskGb": 8, "items": []any{map[string]any{"name": "a", "createdAt": at(1)}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(brs, func(b *billingapi.BillingResource) bool { return b.ResourceType == "server_snapshot" }) {
		t.Fatalf("snapshots still occupy space on a server not billed for compute: %+v", brs)
	}
	if !slices.ContainsFunc(Catalog(), func(b *billingapi.BillingResourceType) bool { return b.ResourceType == "server_snapshot" }) {
		t.Fatal("the price plan form must offer server_snapshot")
	}
}

func TestEmptyImageVolumeBackedShapeDoesNotBillFlavorRootDiskWithoutMarker(t *testing.T) {
	cr := &cloud.CloudResource{Data: map[string]any{
		"server": map[string]any{
			"image":  "",
			"flavor": map[string]any{"disk": 80},
		},
	}}
	got := (&ServerProvider{}).instanceBR(cr, false)
	if got == nil || got.Values["root_disk_gb"] != 0 {
		t.Fatalf("root_disk_gb = %#v, want 0 from an empty image reference", got)
	}
}

func TestProtectionBilledFromItsEnable(t *testing.T) {
	cr := &cloud.CloudResource{ID: "r1", ProjectID: "p", Data: map[string]any{"server": map[string]any{
		"name": "a", "protection": map[string]any{"state": "active", "diskGb": 10.0, "since": "2026-11-07T01:00:00Z"},
	}}}
	br := protectionBR(cr)
	if br == nil || br.CreatedAt == nil || br.CreatedAt.Format(time.RFC3339) != "2026-11-07T01:00:00Z" {
		t.Fatalf("got %+v", br)
	}
	if br.ResourceID != fmt.Sprintf("server_protection-r1-%d", br.CreatedAt.Unix()) {
		t.Fatalf("each enable is its own line: %s", br.ResourceID)
	}
	delete(cr.Data["server"].(map[string]any)["protection"].(map[string]any), "since")
	if br := protectionBR(cr); br.ResourceID != "server_protection-r1" || br.CreatedAt != nil {
		t.Fatalf("a server protected before the stamp keeps its line: %+v", br)
	}
}
