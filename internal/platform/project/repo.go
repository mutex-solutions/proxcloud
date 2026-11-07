package project

import (
	"context"
	"errors"
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

// Save persists the project's fields other than its status. Callers hold a read that may be minutes old (bootstrap waits on
// providers), so it must not be able to put a status back: status, its deletion fields and disabledByOperator change only
// through the conditional writes below.
func (r *Repo) Save(ctx context.Context, p *Project) error {
	_, err := r.col.SetByID(ctx, p.ID, saveFields(p), nil)
	return err
}

// SaveIfStatus writes a status change, with the rest of the project: it writes only while the stored status is still expect,
// so a change decided on a read cannot undo one that won meanwhile, such as the deletion sweep moving the project on. False
// means it lost.
func (r *Repo) SaveIfStatus(ctx context.Context, p *Project, expect string) (bool, error) {
	set := saveFields(p)
	set["status"] = p.Status
	set["scheduledForDeletionAt"] = p.ScheduledForDeletionAt
	set["statusBeforeDeletion"] = p.StatusBeforeDeletion
	set["disabledByOperator"] = p.DisabledByOperator
	return r.col.SetFieldsOne(ctx, pgdoc.M{"_id": p.ID, "status": expect}, set, nil)
}

// EnableIfDisabled is the enable of a project waiting on billing or on its provisioning: it writes only the status, and only
// from DISABLED. An operator's disable stays, unless the operator is the one asking. False means it lost.
func (r *Repo) EnableIfDisabled(ctx context.Context, p *Project, byOperator bool) (bool, error) {
	filter := pgdoc.M{"_id": p.ID, "status": StatusDisabled}
	if !byOperator {
		filter["disabledByOperator"] = pgdoc.M{"$ne": true}
	}
	now := time.Now().UTC()
	ok, err := r.col.SetFieldsOne(ctx, filter, pgdoc.M{"status": StatusEnabled, "disabledByOperator": false, "updatedAt": now}, nil)
	if err == nil && ok {
		p.Status, p.DisabledByOperator, p.UpdatedAt = StatusEnabled, false, &now
	}
	return ok, err
}

// statusWriteTries bounds ApplyBillingSuspension against writers that keep winning. Each loss is a change that went through,
// so running out takes a project changing status several times within a few milliseconds.
const statusWriteTries = 5

// ErrStatusWriteLost means a billing suspend or resume kept losing to other status changes and was not applied.
var ErrStatusWriteLost = errors.New("project status kept changing, the billing suspension was not applied")

// ApplyBillingSuspension moves the project with its billing profile on a fresh read, retrying when another status change wins
// meanwhile: losing to a member scheduling a deletion must not lose the suspension. It answers the project as it now stands,
// nil when it is gone.
func (r *Repo) ApplyBillingSuspension(ctx context.Context, id, billingProfileID string, suspend bool) (*Project, error) {
	for range statusWriteTries {
		p, err := r.FindByID(ctx, id)
		if err != nil || p == nil {
			return nil, err
		}
		expect := p.Status
		if !p.ApplyBillingSuspension(billingProfileID, suspend) {
			return p, nil
		}
		ok, err := r.SaveIfStatus(ctx, p, expect)
		if err != nil {
			return nil, err
		}
		if ok {
			return p, nil
		}
	}
	return nil, ErrStatusWriteLost
}

func saveFields(p *Project) pgdoc.M {
	now := time.Now().UTC()
	p.UpdatedAt = &now
	return pgdoc.M{
		"name":             p.Name,
		"data":             p.Data,
		"owner":            p.Owner,
		"memberships":      p.Memberships,
		"organizationId":   p.OrganizationID,
		"billingProfileId": p.BillingProfileID,
		"customInfo":       p.CustomInfo,
		"services":         p.Services,
		"updatedAt":        now,
	}
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

// deletionGrace is how long a scheduled project deletion can still be cancelled.
const deletionGrace = 5 * time.Minute

// ScheduledForDeletion returns projects in
// SCHEDULED_FOR_DELETION / DELETE_IN_PROGRESS whose scheduledForDeletionAt is past the 5-minute
// grace window (so a just-scheduled / quickly-canceled deletion isn't acted on immediately).
func (r *Repo) ScheduledForDeletion(ctx context.Context, now time.Time) ([]Project, error) {
	cutoff := now.Add(-deletionGrace)
	out := []Project{}
	if err := r.col.Find(ctx, pgdoc.M{
		"status":                 pgdoc.M{"$in": []string{StatusScheduledForDeletion, StatusDeleteInProgress}},
		"scheduledForDeletionAt": pgdoc.M{"$lt": cutoff},
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AllByBillingProfile returns the projects the billing profile pays for: attached to it directly (billingProfileId == bpID), or
// belonging to an org that references it (organizationId ∈ orgIDs — the caller resolves those via
// org.Repo.FindAllByBillingProfileID) with no profile of their own. A project on its own profile is that profile's alone.
func (r *Repo) AllByBillingProfile(ctx context.Context, bpID string, orgIDs []string) ([]Project, error) {
	or := []pgdoc.M{{"billingProfileId": bpID}}
	if len(orgIDs) > 0 {
		or = append(or, pgdoc.M{"organizationId": pgdoc.M{"$in": orgIDs}, "$or": []pgdoc.M{{"billingProfileId": nil}, {"billingProfileId": ""}}})
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
