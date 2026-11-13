package providerattach

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Collections.
const (
	RunCollection  = "providerAttachRun"
	ItemCollection = "providerAttachItem"
)

// Repo stores runs and items in pgdoc. Inside durablejobs.Store.Checkpoint/Complete the context carries the job
// transaction, so these writes commit together with the fence check, or not at all.
type Repo struct{ DB *pgdoc.DB }

func (r Repo) runs() *pgdoc.Store  { return r.DB.C(RunCollection) }
func (r Repo) items() *pgdoc.Store { return r.DB.C(ItemCollection) }

// Ensure creates both tables and their indexes (startup: the writes run inside transactions, where pgdoc cannot
// auto-create a missing table).
func (r Repo) Ensure(ctx context.Context) error {
	if err := r.runs().Ensure(ctx); err != nil {
		return err
	}
	if err := r.items().Ensure(ctx); err != nil {
		return err
	}
	if err := r.runs().EnsureIndex(ctx, "serviceId", false, pgdoc.F("serviceId")); err != nil {
		return err
	}
	return r.items().EnsureIndex(ctx, "runId_state", false, pgdoc.F("runId"), pgdoc.F("state"))
}

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// q is the job transaction when ctx carries one of this DB, else the pool.
func (r Repo) q(ctx context.Context) querier {
	if tx, err := r.DB.Tx(ctx); err == nil {
		return tx
	}
	return r.DB.Pool
}

// SaveRun inserts or replaces a run.
func (r Repo) SaveRun(ctx context.Context, run *Run) error {
	run.UpdatedAt = time.Now().UTC()
	return r.runs().Upsert(ctx, run.ID, run)
}

// LoadRun reads a run (nil when absent).
func (r Repo) LoadRun(ctx context.Context, id string) (*Run, error) {
	var run Run
	found, err := r.runs().Get(ctx, id, &run)
	if err != nil || !found {
		return nil, err
	}
	return &run, nil
}

// LoadRunForUpdate reads and row-locks a run (inside a transaction).
func (r Repo) LoadRunForUpdate(ctx context.Context, id string) (*Run, error) {
	var run Run
	found, err := r.runs().GetForUpdate(ctx, id, &run)
	if err != nil || !found {
		return nil, err
	}
	return &run, nil
}

// RunsForService lists a provider's runs, newest first.
func (r Repo) RunsForService(ctx context.Context, serviceID string, limit int64) ([]Run, error) {
	out := []Run{}
	err := r.runs().Find(ctx, pgdoc.M{"serviceId": serviceID}, &out, pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime)), pgdoc.Limit(limit))
	return out, err
}

// CountRuns counts a provider's runs (the job key's sequence number).
func (r Repo) CountRuns(ctx context.Context, serviceID string) (int64, error) {
	return r.runs().Count(ctx, pgdoc.M{"serviceId": serviceID})
}

// InsertItems stores new items, keeping any that already exist (a plan re-run after a crash rewrites nothing that ran).
func (r Repo) InsertItems(ctx context.Context, items []Item) error {
	const chunk = 1000
	for start := 0; start < len(items); start += chunk {
		end := min(start+chunk, len(items))
		ids := make([]string, 0, end-start)
		docs := make([]string, 0, end-start)
		for i := start; i < end; i++ {
			body, _, err := pgdoc.Marshal(items[i])
			if err != nil {
				return err
			}
			ids = append(ids, items[i].ID)
			docs = append(docs, string(body))
		}
		if _, err := r.q(ctx).Exec(ctx, `INSERT INTO "`+ItemCollection+`"(id,doc) SELECT * FROM unnest($1::text[],$2::jsonb[]) ON CONFLICT (id) DO NOTHING`, ids, docs); err != nil {
			return err
		}
	}
	return nil
}

// SaveItem replaces one item.
func (r Repo) SaveItem(ctx context.Context, it *Item) error {
	it.UpdatedAt = time.Now().UTC()
	_, err := r.items().Replace(ctx, it.ID, it)
	return err
}

// NextPending row-locks the next pending item of a run (nil when none). SKIP LOCKED: a row another transaction holds is
// not waited on (a wait re-checked against LIMIT 1 could answer "none left" while others are).
func (r Repo) NextPending(ctx context.Context, runID string) (*Item, error) {
	rows, err := r.q(ctx).Query(ctx, `SELECT id,doc FROM "`+ItemCollection+`" WHERE doc->>'runId'=$1 AND doc->>'state'=$2 ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`, runID, ItemPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var id string
	var body []byte
	if err := rows.Scan(&id, &body); err != nil {
		return nil, err
	}
	var it Item
	if err := pgdoc.Unmarshal(body, id, &it); err != nil {
		return nil, err
	}
	return &it, nil
}

// ItemsInState lists a run's items in one of the states (oldest id first).
func (r Repo) ItemsInState(ctx context.Context, runID string, states ...string) ([]Item, error) {
	out := []Item{}
	err := r.items().Find(ctx, pgdoc.M{"runId": runID, "state": pgdoc.M{"$in": states}}, &out, pgdoc.Sort(pgdoc.Asc("_id")))
	return out, err
}

// SetItemsState moves every item of a run in state from to state to with code.
func (r Repo) SetItemsState(ctx context.Context, runID, from, to, code string) (int64, error) {
	return r.items().SetFields(ctx, pgdoc.M{"runId": runID, "state": from}, pgdoc.M{"state": to, "code": code, "updatedAt": time.Now().UTC()}, nil)
}

// ItemPage lists a run's items, optionally of one state, by project id.
func (r Repo) ItemPage(ctx context.Context, runID, state string, limit, offset int64) ([]Item, int64, error) {
	filter := pgdoc.M{"runId": runID}
	if state != "" {
		filter["state"] = state
	}
	out := []Item{}
	if err := r.items().Find(ctx, filter, &out, pgdoc.Sort(pgdoc.Asc("projectId")), pgdoc.Limit(limit), pgdoc.Skip(offset)); err != nil {
		return nil, 0, err
	}
	total, err := r.items().Count(ctx, filter)
	return out, total, err
}

// Counts aggregates a run's items by state (and the skipped and failed ones by code).
func (r Repo) Counts(ctx context.Context, runID string) (Counts, error) {
	c := Counts{ByCode: map[string]int64{}}
	rows, err := r.q(ctx).Query(ctx, `SELECT coalesce(doc->>'state',''),coalesce(doc->>'code',''),count(*) FROM "`+ItemCollection+`" WHERE doc->>'runId'=$1 GROUP BY 1,2`, runID)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var state, code string
		var n int64
		if err := rows.Scan(&state, &code, &n); err != nil {
			return c, err
		}
		c.Total += n
		switch state {
		case ItemPending:
			c.Pending += n
		case ItemRunning:
			c.Running += n
		case ItemAttached:
			c.Attached += n
		case ItemSkipped:
			c.Skipped += n
		case ItemFailed:
			c.Failed += n
		case ItemCancelled:
			c.Cancelled += n
		}
		if (state == ItemSkipped || state == ItemFailed) && code != "" {
			c.ByCode[code] += n
		}
	}
	if len(c.ByCode) == 0 {
		c.ByCode = nil
	}
	return c, rows.Err()
}

// ── the projects a scope selects ─────────────────────────────────────────────────────────────────

// ProjectRef is what a plan reads of a project.
type ProjectRef struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	OrganizationID string `json:"organizationId"`
	Status         string `json:"status"`
	Services       []any  `json:"services"`
}

// Deleting mirrors project.Project.Deleting.
func (p ProjectRef) Deleting() bool {
	return p.Status == "SCHEDULED_FOR_DELETION" || p.Status == "DELETE_IN_PROGRESS" || p.Status == "DELETED"
}

// HasService reports a binding to the service.
func (p ProjectRef) HasService(serviceID string) bool {
	for _, s := range p.Services {
		if m, ok := s.(map[string]any); ok && m["serviceId"] == serviceID {
			return true
		}
	}
	return false
}

// projectFilter is the project filter of a scope (nil scope = nothing).
func projectFilter(s Scope) pgdoc.M {
	switch s.Mode {
	case ScopeAll:
		return pgdoc.M{}
	case ScopeOrganizations:
		return pgdoc.M{"organizationId": pgdoc.M{"$in": s.OrganizationIDs}}
	case ScopeProjects:
		return pgdoc.M{"_id": pgdoc.M{"$in": s.ProjectIDs}}
	}
	return nil
}

// ScopedProjectIDs lists up to limit ids of the projects a scope selects (not ScopeRetry) after the id after, in id order.
func (r Repo) ScopedProjectIDs(ctx context.Context, s Scope, after string, limit int) ([]string, error) {
	f := projectFilter(s)
	if f == nil {
		return nil, fmt.Errorf("providerattach: no project filter for scope %q", s.Mode)
	}
	if after != "" {
		f = pgdoc.M{"$and": []pgdoc.M{f, {"_id": pgdoc.M{"$gt": after}}}}
	}
	var list []struct {
		ID string `json:"id"`
	}
	if err := r.DB.C("project").Find(ctx, f, &list, pgdoc.Sort(pgdoc.Asc("_id")), pgdoc.Limit(int64(limit))); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.ID)
	}
	return out, nil
}

// ItemProjectIDs lists up to limit project ids of a run's items in the states, after the project id after, in order.
func (r Repo) ItemProjectIDs(ctx context.Context, runID string, states []string, after string, limit int) ([]string, error) {
	rows, err := r.q(ctx).Query(ctx, `SELECT doc->>'projectId' FROM "`+ItemCollection+`" WHERE doc->>'runId'=$1 AND doc->>'state'=ANY($2)
	  AND doc->>'projectId' > $3 ORDER BY doc->>'projectId' LIMIT $4`, runID, states, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CountScope counts the projects a scope selects now (not ScopeRetry).
func (r Repo) CountScope(ctx context.Context, s Scope) (int64, error) {
	f := projectFilter(s)
	if f == nil {
		return 0, fmt.Errorf("providerattach: no project filter for scope %q", s.Mode)
	}
	return r.DB.C("project").Count(ctx, f)
}

// FinishedRunsToPrune lists runs finished before cutoff whose items are still stored (oldest first).
func (r Repo) FinishedRunsToPrune(ctx context.Context, cutoff time.Time, limit int64) ([]Run, error) {
	out := []Run{}
	err := r.runs().Find(ctx, pgdoc.M{"finishedAt": pgdoc.M{"$lt": cutoff}, "itemsPruned": pgdoc.M{"$ne": true}}, &out,
		pgdoc.Sort(pgdoc.AscK("finishedAt", pgdoc.KTime)), pgdoc.Limit(limit))
	return out, err
}

// PruneItems deletes a finished run's items and marks it pruned (its counts are frozen on the run).
func (r Repo) PruneItems(ctx context.Context, runID string) error {
	return r.DB.WithTx(ctx, func(tc context.Context) error {
		if _, err := r.items().DeleteMany(tc, pgdoc.M{"runId": runID}); err != nil {
			return err
		}
		_, err := r.runs().SetByID(tc, runID, pgdoc.M{"itemsPruned": true}, nil)
		return err
	})
}

// ProjectsByID reads the given projects (missing ones are absent from the map).
func (r Repo) ProjectsByID(ctx context.Context, ids []string) (map[string]ProjectRef, error) {
	out := map[string]ProjectRef{}
	if len(ids) == 0 {
		return out, nil
	}
	var list []ProjectRef
	if err := r.DB.C("project").Find(ctx, pgdoc.M{"_id": pgdoc.M{"$in": ids}}, &list); err != nil {
		return nil, err
	}
	for _, p := range list {
		out[p.ID] = p
	}
	return out, nil
}

// MissingIDs returns the ids absent from a collection (organization / project checks of a request).
func (r Repo) MissingIDs(ctx context.Context, collection string, ids []string) ([]string, error) {
	rows, err := r.DB.Pool.Query(ctx, `SELECT x FROM unnest($1::text[]) x WHERE NOT EXISTS (SELECT 1 FROM "`+collection+`" c WHERE c.id=x) ORDER BY x`, ids)
	if err != nil {
		var pgErr *pgconn.PgError
		if asPg(err, &pgErr) && pgErr.Code == "42P01" { // no such table yet: every id is missing
			return append([]string(nil), ids...), nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Preview counts what a scope would do now: projects in scope, already attached, being deleted, to attach.
type Preview struct {
	InScope         int64 `json:"inScope"`
	AlreadyAttached int64 `json:"alreadyAttached"`
	Deleting        int64 `json:"deleting"`
	ToAttach        int64 `json:"toAttach"`
}

// Preview counts a scope (not ScopeRetry) against the provider.
func (r Repo) Preview(ctx context.Context, s Scope, serviceID string) (Preview, error) {
	var p Preview
	f := projectFilter(s)
	if f == nil {
		return p, fmt.Errorf("providerattach: no project filter for scope %q", s.Mode)
	}
	col := r.DB.C("project")
	var err error
	if p.InScope, err = col.Count(ctx, f); err != nil {
		return p, err
	}
	attached := cloneM(f)
	attached["services"] = pgdoc.M{"$contains": pgdoc.M{"serviceId": serviceID}}
	if p.AlreadyAttached, err = col.Count(ctx, attached); err != nil {
		return p, err
	}
	// Deleting = on its way out and not attached (an attached one already counts as attached).
	deleting := cloneM(f)
	deleting["status"] = pgdoc.M{"$in": []string{"SCHEDULED_FOR_DELETION", "DELETE_IN_PROGRESS", "DELETED"}}
	deletingAttached := cloneM(deleting)
	deletingAttached["services"] = pgdoc.M{"$contains": pgdoc.M{"serviceId": serviceID}}
	var all, both int64
	if all, err = col.Count(ctx, deleting); err != nil {
		return p, err
	}
	if both, err = col.Count(ctx, deletingAttached); err != nil {
		return p, err
	}
	p.Deleting = all - both
	p.ToAttach = p.InScope - p.AlreadyAttached - p.Deleting
	return p, nil
}

func cloneM(m pgdoc.M) pgdoc.M {
	out := pgdoc.M{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ── candidates for the scope picker ─────────────────────────────────────────────────────────────

// Candidate is one row of the picker.
type Candidate struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	OrganizationID   string `json:"organizationId,omitempty"`
	OrganizationName string `json:"organizationName,omitempty"`
	Status           string `json:"status,omitempty"`
	Attached         bool   `json:"attached,omitempty"`
	Projects         int64  `json:"projects,omitempty"`
}

func searchFilter(search string) pgdoc.M {
	if search == "" {
		return pgdoc.M{}
	}
	return pgdoc.M{"$or": []pgdoc.M{
		{"name": pgdoc.M{"$regex": regexp.QuoteMeta(search), "$options": "i"}},
		{"_id": search},
	}}
}

// ProjectCandidates lists projects matching search (name, case-insensitive, or exact id) with their organization name
// and whether they are attached to the provider.
func (r Repo) ProjectCandidates(ctx context.Context, serviceID, search string, limit int64) ([]Candidate, error) {
	var list []ProjectRef
	if err := r.DB.C("project").Find(ctx, searchFilter(search), &list, pgdoc.Sort(pgdoc.Asc("name")), pgdoc.Limit(limit)); err != nil {
		return nil, err
	}
	orgNames := map[string]string{}
	out := make([]Candidate, 0, len(list))
	for _, p := range list {
		if _, seen := orgNames[p.OrganizationID]; !seen && p.OrganizationID != "" {
			var o struct {
				Name string `json:"name"`
			}
			if found, err := r.DB.C("organization").Get(ctx, p.OrganizationID, &o); err == nil && found {
				orgNames[p.OrganizationID] = o.Name
			} else {
				orgNames[p.OrganizationID] = ""
			}
		}
		out = append(out, Candidate{ID: p.ID, Name: p.Name, OrganizationID: p.OrganizationID, OrganizationName: orgNames[p.OrganizationID],
			Status: p.Status, Attached: p.HasService(serviceID)})
	}
	return out, nil
}

// OrganizationCandidates lists organizations matching search with their project count.
func (r Repo) OrganizationCandidates(ctx context.Context, search string, limit int64) ([]Candidate, error) {
	var list []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := r.DB.C("organization").Find(ctx, searchFilter(search), &list, pgdoc.Sort(pgdoc.Asc("name")), pgdoc.Limit(limit)); err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(list))
	for _, o := range list {
		n, err := r.DB.C("project").Count(ctx, pgdoc.M{"organizationId": o.ID})
		if err != nil {
			return nil, err
		}
		out = append(out, Candidate{ID: o.ID, Name: o.Name, Projects: n})
	}
	return out, nil
}

func asPg(err error, target **pgconn.PgError) bool { return errors.As(err, target) }
