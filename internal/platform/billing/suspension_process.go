package billing

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// SuspensionProcessStatus values.
const (
	SuspensionInProgress = "IN_PROGRESS"
	SuspensionSuspended  = "SUSPENDED"
	SuspensionResolved   = "RESOLVED"
)

// SuspensionProcess is the "suspension" document tracking record for a billing
// profile's dunning/auto-suspension lifecycle.
type SuspensionProcess struct {
	ID               string                           `json:"id,omitempty"`
	Status           string                           `json:"status,omitempty"`
	BillingProfileID string                           `json:"billingProfileId,omitempty"`
	Notifications    []pricing.SuspensionNotification `json:"notifications,omitempty"`
	CreatedAt        *time.Time                       `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time                       `json:"updatedAt,omitempty"`
}

// FindFirstSuspensionByStatus finds the first suspension by billing profile + status.
func (r *Repo) FindFirstSuspensionByStatus(ctx context.Context, bpID, status string) (*SuspensionProcess, error) {
	return r.findFirstSuspension(ctx, pgdoc.M{"billingProfileId": bpID, "status": status})
}

// FindFirstSuspensionByStatusIn finds the first suspension by billing profile + status-in.
func (r *Repo) FindFirstSuspensionByStatusIn(ctx context.Context, bpID string, statuses ...string) (*SuspensionProcess, error) {
	return r.findFirstSuspension(ctx, pgdoc.M{"billingProfileId": bpID, "status": pgdoc.M{"$in": statuses}})
}

func (r *Repo) findFirstSuspension(ctx context.Context, filter pgdoc.M) (*SuspensionProcess, error) {
	var sp SuspensionProcess
	found, err := r.suspensions.FindOne(ctx, filter, &sp)
	if err != nil || !found {
		return nil, err
	}
	return &sp, nil
}

// ExistsSuspensionByStatusIn reports whether a suspension exists by billing profile + status-in.
func (r *Repo) ExistsSuspensionByStatusIn(ctx context.Context, bpID string, statuses ...string) (bool, error) {
	return r.suspensions.Exists(ctx, pgdoc.M{"billingProfileId": bpID, "status": pgdoc.M{"$in": statuses}})
}

// AllSuspensionsByBillingProfile returns all suspensions for a billing profile.
func (r *Repo) AllSuspensionsByBillingProfile(ctx context.Context, bpID string) ([]SuspensionProcess, error) {
	return findTyped[SuspensionProcess](ctx, r.suspensions, pgdoc.M{"billingProfileId": bpID})
}

// SaveSuspensionProcess inserts a new process (no ID → generated id, createdAt set)
// or full-replaces an existing one by _id (bumps updatedAt).
func (r *Repo) SaveSuspensionProcess(ctx context.Context, sp *SuspensionProcess) error {
	now := time.Now().UTC()
	sp.UpdatedAt = &now
	if sp.ID == "" {
		sp.CreatedAt = &now
		id, err := r.suspensions.InsertOne(ctx, sp)
		if err != nil {
			return err
		}
		sp.ID = id
		return nil
	}
	_, err := r.suspensions.Replace(ctx, sp.ID, sp)
	return err
}
