package project

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Repo backs the `project` collection. Memberships are embedded in the document
// (stored inline), so member queries use jsonb containment on `memberships`.
type Repo struct {
	col *pgdoc.Store
}

func NewRepo(db *pgdoc.DB) *Repo {
	return &Repo{col: db.C("project")}
}

func (r *Repo) EnsureIndexes(ctx context.Context) error {
	if err := r.col.Ensure(ctx); err != nil {
		return err
	}
	// memberships.sub points into an array of objects, which the expression
	// indexes can't address — member lookups rely on jsonb containment instead.
	return r.col.EnsureIndex(ctx, "organizationId", false, pgdoc.F("organizationId"))
}

func (r *Repo) Insert(ctx context.Context, p *Project) (*Project, error) {
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = &now, &now
	id, err := r.col.InsertOne(ctx, p)
	if err != nil {
		return nil, err
	}
	p.ID = id
	return p, nil
}

// Save persists the mutable project fields.
func (r *Repo) Save(ctx context.Context, p *Project) error {
	now := time.Now().UTC()
	p.UpdatedAt = &now
	_, err := r.col.SetByID(ctx, p.ID, pgdoc.M{
		"name":                   p.Name,
		"status":                 p.Status,
		"data":                   p.Data,
		"owner":                  p.Owner,
		"memberships":            p.Memberships,
		"organizationId":         p.OrganizationID,
		"billingProfileId":       p.BillingProfileID,
		"customInfo":             p.CustomInfo,
		"services":               p.Services,
		"scheduledForDeletionAt": p.ScheduledForDeletionAt,
		"updatedAt":              now,
	}, nil)
	return err
}

func (r *Repo) FindByID(ctx context.Context, id string) (*Project, error) {
	var p Project
	found, err := r.col.Get(ctx, id, &p) // unknown/malformed id => not found
	if err != nil || !found {
		return nil, err
	}
	return &p, nil
}

// FindForMember loads a project only if sub is one of its members
// (_id == id AND some memberships element has that sub).
func (r *Repo) FindForMember(ctx context.Context, id, sub string) (*Project, error) {
	var p Project
	found, err := r.col.FindOne(ctx, pgdoc.M{
		"_id":         id,
		"memberships": pgdoc.M{"$contains": pgdoc.M{"sub": sub}},
	}, &p)
	if err != nil || !found {
		return nil, err
	}
	return &p, nil
}

// ListForMember returns the projects sub can see: ones it is a member of, plus
// (if orgIDs given) any project under an org where sub is OWNER/ADMIN (an $or
// filter).
func (r *Repo) ListForMember(ctx context.Context, sub string, orgIDs []string) ([]Project, error) {
	member := pgdoc.M{"memberships": pgdoc.M{"$contains": pgdoc.M{"sub": sub}}}
	filter := member
	if len(orgIDs) > 0 {
		filter = pgdoc.M{"$or": []pgdoc.M{
			member,
			{"organizationId": pgdoc.M{"$in": orgIDs}},
		}}
	}
	var out []Project
	if err := r.col.Find(ctx, filter, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListByOrganizationID returns every project owned by an organization — used to propagate a new
// org member onto the org's projects.
func (r *Repo) ListByOrganizationID(ctx context.Context, orgID string) ([]Project, error) {
	out := []Project{}
	if err := r.col.Find(ctx, pgdoc.M{"organizationId": orgID}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AllEnabled returns every ENABLED project — the set the cloud metrics job walks.
func (r *Repo) AllEnabled(ctx context.Context) ([]Project, error) {
	out := []Project{}
	if err := r.col.Find(ctx, pgdoc.M{"status": StatusEnabled}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ScheduledForDeletion returns projects in
// SCHEDULED_FOR_DELETION / DELETE_IN_PROGRESS whose scheduledForDeletionAt is past the 5-minute
// grace window (so a just-scheduled / quickly-canceled deletion isn't acted on immediately).
func (r *Repo) ScheduledForDeletion(ctx context.Context, now time.Time) ([]Project, error) {
	cutoff := now.Add(-5 * time.Minute)
	out := []Project{}
	if err := r.col.Find(ctx, pgdoc.M{
		"status":                 pgdoc.M{"$in": []string{StatusScheduledForDeletion, StatusDeleteInProgress}},
		"scheduledForDeletionAt": pgdoc.M{"$lt": cutoff},
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AllByBillingProfile returns projects attached to
// the billing profile directly (billingProfileId == bpID) OR belonging to an org that
// references it (organizationId ∈ orgIDs — the caller resolves those via
// org.Repo.FindAllByBillingProfileID). Deduped by the $or.
func (r *Repo) AllByBillingProfile(ctx context.Context, bpID string, orgIDs []string) ([]Project, error) {
	or := []pgdoc.M{{"billingProfileId": bpID}}
	if len(orgIDs) > 0 {
		or = append(or, pgdoc.M{"organizationId": pgdoc.M{"$in": orgIDs}})
	}
	out := []Project{}
	if err := r.col.Find(ctx, pgdoc.M{"$or": or}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repo) Delete(ctx context.Context, id string) error {
	_, err := r.col.DeleteByID(ctx, id) // unknown/malformed id => no-op
	return err
}
