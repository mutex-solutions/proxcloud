package pveincident

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
)

// Repo is DB-only; it has no cloud client, notifier or cache/billing repository.
// Constructing it does not enable collection: a future inventory adapter must explicitly call it.
type Repo struct {
	db                       *pgdoc.DB
	states, events, captures *pgdoc.Store
	audit                    *audit.Service
	clock                    func(context.Context) (time.Time, error) // transaction DB clock; replaceable only by same-package tests
}
type captureState struct {
	Capture
	Consumed   bool      `json:"consumed"`
	AcceptedAt time.Time `json:"acceptedAt"`
}

func New(db *pgdoc.DB, a *audit.Service) *Repo {
	r := &Repo{db: db, states: db.C("pveIncidentState"), events: db.C("pveIncidentEvent"), captures: db.C("pveIncidentCapture"), audit: a}
	r.clock = func(ctx context.Context) (time.Time, error) {
		tx, err := db.Tx(ctx)
		if err != nil {
			return time.Time{}, err
		}
		var now time.Time
		err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now)
		return now.UTC(), err
	}
	return r
}
func (r *Repo) Ensure(ctx context.Context) error {
	for _, s := range []*pgdoc.Store{r.states, r.events, r.captures, r.db.C("auditEvent")} {
		if err := s.Ensure(ctx); err != nil {
			return err
		}
	}
	if err := r.states.EnsureIndex(ctx, "provider", false, pgdoc.F("providerId")); err != nil {
		return err
	}
	return r.events.EnsureIndex(ctx, "version", true, pgdoc.F("incidentId"), pgdoc.IndexField{Field: "episode", Kind: pgdoc.KNum}, pgdoc.IndexField{Field: "version", Kind: pgdoc.KNum})
}
func key(parts ...string) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func stateID(c Capture, kind, subject string) string {
	return key(c.ProviderID, c.Generation, c.Region, kind, subject)
}

// BeginCapture issues a monotonic provider epoch before any external GET. The global
// provider lock/throttle covers all pods, regions and credential generations.
func (r *Repo) BeginCapture(ctx context.Context, provider, generation, region string) (Capture, error) {
	if !validID(provider) || !validID(generation) || !validID(region) {
		return Capture{}, ErrInvalid
	}
	var result Capture
	err := r.db.WithTx(ctx, func(tc context.Context) error {
		if err := r.db.LockXact(tc, "pve-incident:"+provider); err != nil {
			return err
		}
		now, err := r.clock(tc)
		if err != nil {
			return err
		}
		var old captureState
		if _, err = r.captures.GetForUpdate(tc, provider, &old); err != nil {
			return err
		}
		if !old.StartedAt.IsZero() && (now.Before(old.StartedAt) || now.Sub(old.StartedAt) < ObservationInterval) {
			return ErrThrottled
		}
		if !old.AcceptedAt.IsZero() && now.Sub(old.AcceptedAt) < ObservationInterval {
			return ErrThrottled
		}
		result = Capture{provider, generation, region, old.Epoch + 1, now}
		return r.captures.Upsert(tc, provider, captureState{Capture: result, AcceptedAt: old.AcceptedAt})
	})
	return result, err
}

// Observe validates the DB-issued capture and commits evidence, transitions and audit
// in ONE transaction. Every accepted epoch is consumed even when visibility is unknown.
func (r *Repo) Observe(ctx context.Context, x SnapshotResult) (ObserveResult, error) {
	var result ObserveResult
	if r.audit == nil {
		return result, fmt.Errorf("incident audit unavailable")
	}
	c := x.Capture
	if !validID(c.ProviderID) || !validID(c.Generation) || !validID(c.Region) || c.Epoch < 1 {
		return result, ErrInvalid
	}
	err := r.db.WithTx(ctx, func(tc context.Context) error {
		if err := r.db.LockXact(tc, "pve-incident:"+c.ProviderID); err != nil {
			return err
		}
		now, err := r.clock(tc)
		if err != nil {
			return err
		}
		var issued captureState
		found, err := r.captures.GetForUpdate(tc, c.ProviderID, &issued)
		if err != nil {
			return err
		}
		if !found || issued.Epoch != c.Epoch || issued.Generation != c.Generation || issued.Region != c.Region || !issued.StartedAt.Equal(c.StartedAt) {
			return ErrInvalid
		}
		if issued.Consumed {
			return nil
		}
		if now.Before(c.StartedAt) || now.Sub(c.StartedAt) > CaptureTTL {
			return ErrInvalid
		}
		if !issued.AcceptedAt.IsZero() && now.Sub(issued.AcceptedAt) < ObservationInterval {
			return ErrThrottled
		}
		observations, err := classify(x, now)
		if err != nil {
			return err
		}
		var existing []state
		if err = r.states.Find(tc, pgdoc.M{"providerId": c.ProviderID, "generation": c.Generation, "region": c.Region}, &existing); err != nil {
			return err
		}
		byID := map[string]state{}
		for _, s := range existing {
			byID[s.ID] = s
		}
		seen := map[string]bool{}
		for _, o := range observations {
			seen[stateID(c, o.kind, o.subject)] = true
		}
		// Absence is unknown, never an implicit healthy observation or deletion.
		missingSubject := false
		for _, s := range existing {
			if !seen[s.ID] {
				observations = append(observations, observation{s.Kind, s.Subject, "subject_unobserved", 0, Untrusted})
				if s.Kind == KindNode || s.Kind == KindProtection {
					missingSubject = true
				}
			}
		}
		if missingSubject {
			for i := range observations {
				o := &observations[i]
				if o.kind == KindVisibility && o.health > 0 {
					o.health, o.quality, o.reason = 0, Untrusted, "subject_unobserved"
				}
			}
		}
		for _, o := range observations {
			id := stateID(c, o.kind, o.subject)
			s, exists := byID[id]
			if !exists {
				s.Incident = Incident{ID: id, ProviderID: c.ProviderID, Generation: c.Generation, Region: c.Region, Kind: o.kind, Subject: o.subject, State: StatePending}
			}
			priorVersion := s.Version
			s, action := advance(s, o, c.Epoch, now)
			updateAffected(&s, x)
			if err = r.save(tc, s, exists, priorVersion); err != nil {
				return err
			}
			if action != "" {
				if err = r.record(tc, s, action, now); err != nil {
					return err
				}
				result.Events++
			}
		}
		issued.Consumed, issued.AcceptedAt = true, now
		if err = r.captures.Upsert(tc, c.ProviderID, issued); err != nil {
			return err
		}
		result.Accepted = true
		return nil
	})
	if err != nil {
		return ObserveResult{}, err
	}
	return result, nil
}
func (r *Repo) save(ctx context.Context, s state, exists bool, version int64) error {
	if !exists {
		_, err := r.states.InsertOne(ctx, s)
		return err
	}
	body, _, err := pgdoc.Marshal(s)
	if err != nil {
		return err
	}
	tx, err := r.db.Tx(ctx)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE "pveIncidentState" SET doc=$1 WHERE id=$2 AND (doc->>'version')::bigint=$3`, body, s.ID, version)
	if err == nil && tag.RowsAffected() != 1 {
		return fmt.Errorf("incident version conflict")
	}
	return err
}
func updateAffected(s *state, x SnapshotResult) {
	s.AffectedComplete = x.Trust.VMs == Verified && x.LinksComplete
	if x.Trust.VMs != Verified {
		return
	}
	links := map[string]Link{}
	if !x.LinksComplete {
		for _, l := range s.Affected {
			links[l.ServerID] = l
		}
	}
	for _, l := range x.Links {
		delete(links, l.ServerID) // positive migration/reuse evidence supersedes a last-known link
		if s.Kind == KindNode && l.NodeID != s.Subject {
			continue
		}
		if s.Kind == KindProtection && l.ServerID+":"+l.InstanceID != s.Subject {
			continue
		}
		links[l.ServerID] = l
	}
	s.Affected = nil
	for _, l := range links {
		s.Affected = append(s.Affected, l)
	}
	sort.Slice(s.Affected, func(i, j int) bool { return s.Affected[i].ServerID < s.Affected[j].ServerID })
}
func (r *Repo) record(ctx context.Context, s state, action string, now time.Time) error {
	e := Event{ID: fmt.Sprintf("%s.%d.%d", s.ID, s.Episode, s.Version), IncidentID: s.ID, ProviderID: s.ProviderID, Episode: s.Episode, Version: s.Version, Action: action, ObservedAt: now, Reason: s.Reason, Affected: s.Affected, EvidenceQuality: s.EvidenceQuality, AffectedComplete: s.AffectedComplete}
	if _, err := r.events.InsertOne(ctx, e); err != nil {
		return err
	}
	ev := audit.SystemEvent()
	ev.Action, ev.ResourceType, ev.ResourceID, ev.Outcome = action, "PVE_INCIDENT", s.ID, audit.OutcomeSuccess
	ev.ResourceMetadata = map[string]any{"providerId": s.ProviderID, "region": s.Region, "kind": s.Kind, "subject": s.Subject, "episode": s.Episode, "eventVersion": s.Version, "observedAt": now, "reason": s.Reason, "affectedSelector": s.ID, "evidenceQuality": s.EvidenceQuality, "affectedComplete": s.AffectedComplete}
	return r.audit.Log(ctx, ev) // synchronous: any failure rolls back state + event + capture consumption
}

func pageLimit(n int) (int, error) {
	if n == 0 {
		return 50, nil
	}
	if n < 1 || n > 100 {
		return 0, ErrInvalid
	}
	return n, nil
}
func (r *Repo) List(ctx context.Context, provider string, f Filter) (Page[Incident], error) {
	out := Page[Incident]{Data: []Incident{}}
	n, err := pageLimit(f.Limit)
	if err != nil || !validID(provider) || (f.Cursor != "" && !validID(f.Cursor)) || (f.Kind != "" && !validKind(f.Kind)) || (f.State != "" && f.State != StatePending && f.State != StateOpen && f.State != StateRecovered) {
		return out, ErrInvalid
	}
	q := pgdoc.M{"providerId": provider}
	if f.Cursor != "" {
		q["_id"] = pgdoc.M{"$gt": f.Cursor}
	}
	if f.State != "" {
		q["state"] = f.State
	}
	if f.Kind != "" {
		q["kind"] = f.Kind
	}
	if err = r.states.Find(ctx, q, &out.Data, pgdoc.Sort(pgdoc.Asc("_id")), pgdoc.Limit(int64(n+1))); err != nil {
		return out, err
	}
	if len(out.Data) > n {
		out.Data = out.Data[:n]
		out.Next = out.Data[n-1].ID
	}
	return out, nil
}
func (r *Repo) Affected(ctx context.Context, provider, id, project, cursor string, limit int) (Page[Affected], error) {
	out := Page[Affected]{Data: []Affected{}}
	n, err := pageLimit(limit)
	if err != nil || !validID(provider) || !validID(id) || (project != "" && !validID(project)) || (cursor != "" && !validID(cursor)) {
		return out, ErrInvalid
	}
	var s state
	found, err := r.states.FindOne(ctx, pgdoc.M{"_id": id, "providerId": provider}, &s)
	if err != nil {
		return out, err
	}
	if !found {
		return out, ErrNotFound
	}
	for _, l := range s.Affected { // project scope BEFORE cursor/page, no foreign totals
		if (project == "" || l.ProjectID == project) && l.ServerID > cursor {
			out.Data = append(out.Data, Affected{l.ServerID, l.ProjectID})
		}
		if len(out.Data) > n {
			out.Data = out.Data[:n]
			out.Next = out.Data[n-1].ServerID
			break
		}
	}
	return out, nil
}
