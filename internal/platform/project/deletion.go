package project

import (
	"context"
	"log/slog"
	"time"
)

// deletion.go runs the executeProjectDeletion cron: projects past their deletion grace window get
// their cloud resources cascade-deleted, then the project doc is removed. A failure rolls the
// project back to ENABLED (so it's retried/visible).

// ResourceDeleter cascades a project's cloud-resource deletion (per-provider delete). The
// production impl deletes each CloudResource via the cloud WriteService; tests use a fake.
// ⚠ This performs LIVE cloud DELETEs.
type ResourceDeleter interface {
	DeleteProjectResources(ctx context.Context, projectID string) error
}

// DeletionJob runs the scheduled project deletions.
type DeletionJob struct {
	repo    *Repo
	deleter ResourceDeleter
	log     *slog.Logger
	now     func() time.Time
}

func NewDeletionJob(repo *Repo, deleter ResourceDeleter, log *slog.Logger) *DeletionJob {
	if log == nil {
		log = slog.Default()
	}
	return &DeletionJob{repo: repo, deleter: deleter, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// ExecuteAll deletes every project past its grace window. Best-effort
// per project (a single failure doesn't abort the batch). Returns the number deleted.
func (j *DeletionJob) ExecuteAll(ctx context.Context) (int, error) {
	projects, err := j.repo.ScheduledForDeletion(ctx, j.now())
	if err != nil {
		return 0, err
	}
	deleted := 0
	for i := range projects {
		if j.deleteProject(ctx, &projects[i]) {
			deleted++
		}
	}
	return deleted, nil
}

// deleteProject: ENABLED → skip (deletion was canceled);
// SCHEDULED_FOR_DELETION → DELETE_IN_PROGRESS; cascade-delete the cloud resources; on success
// remove the doc; on error roll back to ENABLED. Returns whether the project was deleted.
func (j *DeletionJob) deleteProject(ctx context.Context, p *Project) bool {
	if p.Status == StatusEnabled {
		j.log.Debug("project no longer scheduled for deletion (canceled)", "id", p.ID)
		return false
	}
	if p.Status == StatusScheduledForDeletion {
		p.Status = StatusDeleteInProgress
		if err := j.repo.Save(ctx, p); err != nil {
			j.log.Error("project deletion: mark in-progress", "id", p.ID, "err", err)
			return false
		}
	}
	if err := j.deleter.DeleteProjectResources(ctx, p.ID); err != nil {
		j.log.Error("project deletion: cascade cloud delete failed, rolling back to ENABLED", "id", p.ID, "err", err)
		p.Status = StatusEnabled
		_ = j.repo.Save(ctx, p)
		return false
	}
	if err := j.repo.Delete(ctx, p.ID); err != nil {
		j.log.Error("project deletion: remove doc", "id", p.ID, "err", err)
		return false
	}
	j.log.Info("project deleted", "id", p.ID)
	return true
}
