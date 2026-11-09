//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// The interleaving the acceptance pass met on the lab: the move ends, the task poller's sync runs before the retype
// follows the disk (old volid gone, new one listed), then the follow, then the retype's own refresh. One row, the old
// id, nothing archived.
func TestRetypeKeepsOneRowWhenASyncRunsFirst(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := cloud.NewRepo(db)
	past := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	vol := func(ext string) cloud.CloudResource {
		return cloud.CloudResource{ServiceID: "svc", ProjectID: "p", Type: cloud.TypeVolume, ExternalID: ext,
			Data: map[string]any{"volume": map[string]any{"vmid": 101, "slot": "scsi1"}}}
	}
	moved := &reconcileProvScoped{typ: cloud.TypeVolume, projectID: "p", list: []cloud.CloudResource{vol("slow:vm-101-disk-0")}}
	seed := func(t *testing.T) *cloud.CloudResource {
		if _, err := db.C("cloudResource").DeleteMany(ctx, pgdoc.M{}); err != nil {
			t.Fatal(err)
		}
		r := vol("fast:vm-101-disk-1")
		r.CreatedAt, r.UpdatedAt = &past, &past
		row, err := repo.Insert(ctx, &r)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	rows := func(t *testing.T) []cloud.CloudResource {
		got, err := repo.FindByServiceAndType(ctx, "svc", cloud.TypeVolume)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("marked", func(t *testing.T) {
		row := seed(t)
		start := time.Now().UTC()
		if err := repo.MarkMoving(ctx, row, &start); err != nil {
			t.Fatal(err)
		}
		// The poller's sync: the old volid is gone, the new one is listed.
		if _, err := providers.Reconcile(ctx, moved, repo, "svc", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if got := rows(t); len(got) != 2 {
			t.Fatalf("the sync must keep the moving row and may add the new disk: %+v", got)
		}
		if _, err := db.C("cloudResource").SetByID(ctx, row.ID, pgdoc.M{"data": map[string]any{"volume": map[string]any{"vmid": 101, "slot": "scsi1", pve.VolumeLabel: "data"}}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := repo.ChangeExternalID(ctx, row, "slow:vm-101-disk-0"); err != nil {
			t.Fatal(err)
		}
		// A sync that read no row for the new volid before the move committed upserts onto the moved row: the
		// tenant's name for the disk stays (seen live: it was lost that way).
		late := vol("slow:vm-101-disk-0")
		late.UpdatedAt = &start
		if _, err := repo.Upsert(ctx, &late, pve.CarryVolumeLabel); err != nil {
			t.Fatal(err)
		}
		if got, _ := repo.FindByID(ctx, row.ID); got == nil || got.Data["volume"].(map[string]any)[pve.VolumeLabel] != "data" {
			t.Fatalf("the late upsert dropped the name: %+v", got)
		}
		// The retype's refresh, then a later sync.
		for i := 0; i < 2; i++ {
			if _, err := providers.Reconcile(ctx, moved, repo, "svc", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		}
		got := rows(t)
		if len(got) != 1 || got[0].ID != row.ID || got[0].ExternalID != "slow:vm-101-disk-0" || got[0].MovingSince != nil {
			t.Fatalf("want one row %s on the new volid, unmarked: %+v", row.ID, got)
		}
		if n, _ := db.C("cloudResourceHistory").Count(ctx, pgdoc.M{"cloudResourceId": row.ID}); n != 0 {
			t.Fatalf("the disk was archived as deleted (%d)", n)
		}
	})

	// Without the mark (the code before the fix) the sync archives the row first. The follow must then leave the
	// sync's row alone: deleting it is how a third row came about.
	t.Run("unmarked", func(t *testing.T) {
		row := seed(t)
		if _, err := providers.Reconcile(ctx, moved, repo, "svc", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := repo.ChangeExternalID(ctx, row, "slow:vm-101-disk-0"); !errors.Is(err, cloud.ErrRowGone) {
			t.Fatalf("follow onto an archived row: %v", err)
		}
		if got := rows(t); len(got) != 1 || got[0].ID == row.ID {
			t.Fatalf("the sync's row must stay: %+v", got)
		}
	})

	// A mark left by a portal that died mid-move stops protecting the row after MoveKeepFor.
	t.Run("stale mark", func(t *testing.T) {
		row := seed(t)
		old := time.Now().UTC().Add(-cloud.MoveKeepFor - time.Minute)
		if err := repo.MarkMoving(ctx, row, &old); err != nil {
			t.Fatal(err)
		}
		if _, err := providers.Reconcile(ctx, moved, repo, "svc", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if got := rows(t); len(got) != 1 || got[0].ID == row.ID {
			t.Fatalf("a stale mark kept the row: %+v", got)
		}
	})
}
