package project

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
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

// saveFields is what Save writes. Not the bindings (services) nor the provisioning failures: bindings only grow through
// AppendBinding (an atomic append) and failures are written by the bootstrap under the project's attach lock
// (SetProvisionFailures), so a save from a read taken before an attach (a member change, a rename, an activation)
// cannot drop a binding or a failure recorded meanwhile.
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
		"updatedAt":        now,
	}
}

// SetProvisionFailures writes the project's provisioning failures alone (the bootstrap, under the attach lock).
func (r *Repo) SetProvisionFailures(ctx context.Context, projectID string, failures map[string]ProvisionFailure) error {
	if len(failures) == 0 {
		_, err := r.col.SetByID(ctx, projectID, nil, []string{"provisionFailures"})
		return err
	}
	_, err := r.col.SetByID(ctx, projectID, pgdoc.M{"provisionFailures": failures}, nil)
	return err
}

// ErrAttachLost refuses a binding write whose attach lock is no longer held (its lock session ended, or it does not
// hold the lock any more): another writer may own the project's bindings now.
var ErrAttachLost = errors.New("project attach: the attach lock was lost")

// LockedExec runs a binding write on the session that holds the project's attach lock, the ownership check inside the
// statement itself: the write happens on that session only while it still holds the lock (no check-then-write gap),
// and a session that is gone fails it. nil = no lock (the pool).
type LockedExec struct {
	Session pgdoc.LockSession
	Key     string
}

// held asks the lock's session whether it still holds the lock (false on any error: a dead session holds nothing).
func (lx *LockedExec) held() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ok bool
	return lx.Session.QueryRow(ctx, `SELECT `+lockHeldSQL("$1::text"), lx.Key).Scan(&ok) == nil && ok
}

// lockHeldSQL is true while the statement's own session holds the advisory lock on hashtext($key). pg_advisory_lock
// (bigint) shows in pg_locks as classid = high 32 bits, objid = low 32 bits, objsubid = 1.
func lockHeldSQL(keyArg string) string {
	return `EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid() AND granted
	  AND classid=((hashtext(` + keyArg + `)::bigint >> 32) & 4294967295)::oid AND objid=(hashtext(` + keyArg + `)::bigint & 4294967295)::oid AND objsubid=1)`
}

// execLocked runs one write statement holding the placeholder {{LOCK}}. With lx it runs on the lock's session with the
// ownership check (lockHeldSQL) in place of the placeholder, the key appended as the last argument: no gap between the
// check and the write, which are one statement on the lock's own session. A refusal or a session that is gone is
// ErrAttachLost. Without lx it runs on the pool with the check left out. Returns rows affected.
func (r *Repo) execLocked(ctx context.Context, lx *LockedExec, sql string, args ...any) (rows int64, err error) {
	if lx == nil {
		tag, err := r.col.DB().Pool.Exec(ctx, strings.ReplaceAll(sql, "{{LOCK}}", "TRUE"), args...)
		return tag.RowsAffected(), err
	}
	keyArg := "$" + strconv.Itoa(len(args)+1)
	tag, err := lx.Session.Exec(ctx, strings.ReplaceAll(sql, "{{LOCK}}", lockHeldSQL(keyArg+"::text")), append(args, lx.Key)...)
	if err != nil {
		if ctx.Err() != nil || !lx.held() {
			return 0, errors.Join(ErrAttachLost, err) // the session, and with it the lock, is gone (or unusable)
		}
		return 0, err
	}
	// Nothing written: not held (refused by the statement's own check) or nothing to write. Tell which.
	if tag.RowsAffected() == 0 && !lx.held() {
		return 0, ErrAttachLost
	}
	return tag.RowsAffected(), nil
}

// AppendBinding appends binding to the project's services in ONE statement, only when no binding of the same serviceId
// is there yet. False = already bound (nothing written). With lx, the statement runs on the attach lock's session and
// writes only while that session holds the lock; otherwise ErrAttachLost and nothing is written.
func (r *Repo) AppendBinding(ctx context.Context, projectID string, binding map[string]any, lx *LockedExec) (bool, error) {
	serviceID, _ := binding["serviceId"].(string)
	if serviceID == "" {
		return false, errors.New("project binding: serviceId required")
	}
	b, err := json.Marshal(binding)
	if err != nil {
		return false, err
	}
	probe, _ := json.Marshal([]map[string]string{{"serviceId": serviceID}})
	now, _ := json.Marshal(time.Now().UTC())
	n, err := r.execLocked(ctx, lx, `UPDATE "project" SET doc = jsonb_set(doc,
	    '{services}', COALESCE(CASE WHEN jsonb_typeof(doc->'services')='array' THEN doc->'services' END,'[]'::jsonb) || jsonb_build_array($2::jsonb), true)
	    || jsonb_build_object('updatedAt',$4::jsonb)
	  WHERE id=$1 AND NOT (COALESCE(CASE WHEN jsonb_typeof(doc->'services')='array' THEN doc->'services' END,'[]'::jsonb) @> $3::jsonb)
	    AND {{LOCK}}`, projectID, string(b), string(probe), string(now))
	return n == 1, err
}

// ClearBulkMarks drops the operator attach marks (attachedBy) from every binding of the project, in one statement that
// keeps whatever bindings are stored at that moment. False = nothing to clear. lx as in AppendBinding.
func (r *Repo) ClearBulkMarks(ctx context.Context, projectID string, lx *LockedExec) (bool, error) {
	n, err := r.execLocked(ctx, lx, `UPDATE "project" SET doc = jsonb_set(doc,'{services}',
	    (SELECT COALESCE(jsonb_agg(e - 'attachedBy' ORDER BY i),'[]'::jsonb) FROM jsonb_array_elements(doc->'services') WITH ORDINALITY AS t(e,i)), false)
	  WHERE id=$1 AND jsonb_typeof(doc->'services')='array' AND EXISTS (SELECT 1 FROM jsonb_array_elements(doc->'services') e WHERE e ? 'attachedBy')
	    AND {{LOCK}}`, projectID)
	return n == 1, err
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
