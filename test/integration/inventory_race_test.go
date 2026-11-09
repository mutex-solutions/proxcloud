//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

type racingInventoryProvider struct {
	reconcileProv
	afterProof    func()
	beforeArchive func()
}

func (p *racingInventoryProvider) VerifyInventory(context.Context, []cloud.CloudResource, []cloud.CloudResource) error {
	if p.afterProof != nil {
		p.afterProof()
	}
	return nil
}
func (p *racingInventoryProvider) KeepUnread(*cloud.CloudResource) (bool, bool) {
	if p.beforeArchive != nil {
		p.beforeArchive()
	}
	return false, false
}

func TestInventoryProofRejectsConcurrentRowChanges(t *testing.T) {
	for _, mutation := range []string{"CIDR", "owner", "externalID"} {
		for _, operation := range []string{"update", "archive"} {
			t.Run(operation+mutation, func(t *testing.T) {
				ctx := context.Background()
				db := freshPG(t)
				repo := cloud.NewRepo(db)
				past := time.Now().UTC().Add(-time.Hour)
				old := rcRes("v1", map[string]any{"cidr": "10.0.0.0/24"})
				old.ServiceID = "svc"
				old.CreatedAt, old.UpdatedAt = &past, &past
				saved, err := repo.Insert(ctx, &old)
				if err != nil {
					t.Fatal(err)
				}
				p := &racingInventoryProvider{reconcileProv: reconcileProv{typ: cloud.TypeServer}}
				mutate := func() {
					set := pgdoc.M{}
					switch mutation {
					case "CIDR":
						set["data"] = pgdoc.M{"cidr": "10.0.1.0/24"}
					case "owner":
						set["projectId"] = "other-project"
					case "externalID":
						set["externalId"] = "moved"
					}
					// Deliberately keep updatedAt unchanged: the proof is of a row,
					// not of an ordering heuristic or of the caller's wall clock.
					if _, err := db.C("cloudResource").SetByID(ctx, saved.ID, set, nil); err != nil {
						t.Fatal(err)
					}
				}
				if operation == "update" {
					p.list = []cloud.CloudResource{rcRes("v1", map[string]any{"cidr": "stale-list"})}
					p.afterProof = mutate
				} else {
					p.beforeArchive = mutate
				} // after the vanished scan reread
				st, err := providers.Reconcile(ctx, p, repo, "svc", time.Now().UTC())
				if err != nil || st != (providers.ReconcileStats{}) {
					t.Fatalf("race: %+v %v", st, err)
				}
				row, err := repo.FindByID(ctx, saved.ID)
				if err != nil || row == nil {
					t.Fatalf("lost concurrent row: %+v %v", row, err)
				}
				if mutation == "CIDR" && row.Data["cidr"] != "10.0.1.0/24" || mutation == "owner" && row.ProjectID != "other-project" || mutation == "externalID" && row.ExternalID != "moved" {
					t.Fatalf("clobbered row: %+v", row)
				}
				archive, err := repo.FindArchivedSince(ctx, "proj1", "svc", past.Add(-time.Minute))
				if err != nil || len(archive) != 0 {
					t.Fatalf("false archive: %+v %v", archive, err)
				}
			})
		}
	}
}

func TestInventoryAtomicMutationRechecksAfterRowLock(t *testing.T) {
	for _, operation := range []string{"update", "archive"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			db := freshPG(t)
			repo := cloud.NewRepo(db)
			past := time.Now().UTC().Add(-time.Hour)
			old := rcRes("v1", map[string]any{"cidr": "old"})
			old.ServiceID = "svc"
			old.CreatedAt, old.UpdatedAt = &past, &past
			expected, err := repo.Insert(ctx, &old)
			if err != nil {
				t.Fatal(err)
			}
			locked, release, committed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				committed <- db.WithTx(ctx, func(tc context.Context) error {
					var cur cloud.CloudResource
					if _, err := db.C("cloudResource").GetForUpdate(tc, expected.ID, &cur); err != nil {
						return err
					}
					close(locked)
					<-release
					_, err := db.C("cloudResource").SetByID(tc, expected.ID, pgdoc.M{"data": pgdoc.M{"cidr": "concurrent"}}, nil)
					return err
				})
			}()
			<-locked
			started := make(chan struct{})
			result := make(chan struct {
				changed bool
				err     error
			}, 1)
			go func() {
				close(started)
				var changed bool
				var err error
				if operation == "archive" {
					changed, err = repo.DeleteAndArchiveIfUnchanged(ctx, expected, time.Now().UTC())
				} else {
					fresh := *expected
					fresh.Data = map[string]any{"cidr": "stale"}
					changed, err = repo.UpdateIfUnchanged(ctx, expected, &fresh)
				}
				result <- struct {
					changed bool
					err     error
				}{changed, err}
			}()
			<-started
			close(release)
			if err := <-committed; err != nil {
				t.Fatal(err)
			}
			got := <-result
			if got.err != nil || got.changed {
				t.Fatalf("locked mutation accepted stale proof: %+v", got)
			}
			row, err := repo.FindByID(ctx, expected.ID)
			if err != nil || row == nil || row.Data["cidr"] != "concurrent" {
				t.Fatalf("concurrent update lost: %+v %v", row, err)
			}
		})
	}
}
