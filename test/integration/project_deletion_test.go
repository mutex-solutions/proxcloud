//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

type fakeDeleter struct {
	called []string
	err    error
}

func (f *fakeDeleter) DeleteProjectResources(_ context.Context, id string) error {
	f.called = append(f.called, id)
	return f.err
}

// TestExecuteProjectDeletion exercises project.DeletionJob against real Postgres with a fake cloud
// deleter (NO live cloud): only projects past the 5-min grace window in SCHEDULED_FOR_DELETION /
// DELETE_IN_PROGRESS are deleted; ENABLED + within-grace are left; a deleter error rolls back to ENABLED.
func TestExecuteProjectDeletion(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := project.NewRepo(db)
	now := time.Now().UTC()
	past := now.Add(-10 * time.Minute)
	within := now.Add(-1 * time.Minute)

	mk := func(name, status string, sched *time.Time) *project.Project {
		p, err := repo.Insert(ctx, &project.Project{Name: name, Status: status, OrganizationID: "o"})
		if err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		if sched != nil {
			p.ScheduledForDeletionAt = sched
			if ok, err := repo.SaveIfStatus(ctx, p, status); err != nil || !ok {
				t.Fatalf("save %s: %v %v", name, ok, err)
			}
		}
		return p
	}

	a := mk("a", project.StatusScheduledForDeletion, &past)   // past grace → delete
	b := mk("b", project.StatusEnabled, nil)                  // enabled → untouched
	c := mk("c", project.StatusScheduledForDeletion, &within) // within grace → skip

	fd := &fakeDeleter{}
	n, err := project.NewDeletionJob(repo, fd, nil).ExecuteAll(ctx)
	if err != nil || n != 1 {
		t.Fatalf("deleted=%d err=%v, want 1", n, err)
	}
	if len(fd.called) != 1 || fd.called[0] != a.ID {
		t.Fatalf("cascade-deleted projects = %v, want [%s]", fd.called, a.ID)
	}
	if got, _ := repo.FindByID(ctx, a.ID); got != nil {
		t.Fatal("A should be deleted")
	}
	if got, _ := repo.FindByID(ctx, b.ID); got == nil {
		t.Fatal("B (enabled) should remain")
	}
	if got, _ := repo.FindByID(ctx, c.ID); got == nil {
		t.Fatal("C (within grace) should remain")
	}

	// a deleter error keeps the project in progress, so the next sweep retries.
	d := mk("d", project.StatusScheduledForDeletion, &past)
	n2, _ := project.NewDeletionJob(repo, &fakeDeleter{err: errors.New("cloud down")}, nil).ExecuteAll(ctx)
	if n2 != 0 {
		t.Fatalf("failed run deleted=%d, want 0", n2)
	}
	got, _ := repo.FindByID(ctx, d.ID)
	if got == nil || got.Status != project.StatusDeleteInProgress {
		t.Fatalf("D should stay DELETE_IN_PROGRESS, got %+v", got)
	}
}
