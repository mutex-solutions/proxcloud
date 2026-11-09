package registrypins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	distref "github.com/distribution/reference"
	"github.com/jackc/pgx/v5"
)

// ImportProof is a distinct opaque import envelope, never a BuildProof. Value
// copies share the same private once/cancel bit; there is no caller-supplied gate.
// Only AuthorizeImportDispatch creates one, after committing the durable marker.
type ImportProof struct {
	operation, nonce, project, owner, host, source, destination string
	state                                                       *proofState
}

// Consume is DB/network-free and freezes the exact credential-free image strings.
// A mismatch does not consume the envelope; a successful consume or unused cancel
// consumes every shallow copy. Lost/used envelopes cannot be recovered or reissued.
func (p *ImportProof) Consume(project, source, destination string) (operation, nonce string, err error) {
	if p == nil || p.state == nil || p.project != project || p.source != source || p.destination != destination || !p.state.used.CompareAndSwap(false, true) {
		return "", "", ErrConflict
	}
	return p.operation, p.nonce, nil
}

type importRefs struct {
	kind   string
	source Ref // empty for external/library sources, never a deletable managed ref
	dest   Ref // verify uses the source repository, with its digest retained
	need   map[string]bool
}

// importReferences validates representation, not origin/credential authorization.
// Service.StartImport separately checks operator-configured HTTPS origin policy
// before dispatch. This package never resolves DNS or accesses a registry.
func (s *Store) importReferences(project, source, destination string) (importRefs, error) {
	out := importRefs{need: map[string]bool{}}
	if !projectRE.MatchString(project) || len(source) > 512 || len(destination) > 512 || strings.ContainsAny(source+destination, "?#[]%\\\t\r\n ") {
		return out, ErrInvalid
	}
	first, rest, ok := strings.Cut(source, "/")
	host, err := CanonicalHost(first)
	if !ok || err != nil || host != first {
		return out, ErrInvalid
	}
	parsed, err := distref.Parse(source)
	canonical, immutable := parsed.(distref.Canonical)
	_, tagged := parsed.(distref.Tagged)
	if err != nil || !immutable || tagged || canonical.String() != source || !digestRE.MatchString(canonical.Digest().String()) {
		return out, ErrInvalid
	}
	if host == s.host && (rest == "t" || strings.HasPrefix(rest, "t/")) {
		r, e := s.reference(source)
		if e != nil || r.Project != project {
			return out, ErrScope // managed foreign/malformed refs never become external
		}
		out.source = r
		out.need[r.String()] = true
	}
	if out.source.Host != "" && destination == s.host+"/"+out.source.Repo {
		out.kind, out.dest = "verify", out.source
		return out, nil // metadata-only: exactly ONE source digest pin, no tag
	}
	d, err := s.reference(destination)
	if err != nil || d.Project != project || d.Tag == "" || d.String() != destination {
		return out, ErrScope
	}
	if out.source.Host != "" && out.source.Repo == d.Repo {
		return out, ErrScope // a copy must not overwrite the source template repository
	}
	out.kind, out.dest = "copy", d
	out.need[d.String()] = true
	return out, nil
}

// rejectImportDispatch composes with the existing build authorizer under the
// namespace lock: one operation UUID cannot mint either action after the other.
func (s *Store) rejectImportDispatch(ctx context.Context, tx pgx.Tx, operation string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM registry_import_dispatch WHERE operation=$1)`, operation).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrConflict
	}
	return nil
}

// AuthorizeImportDispatch is PURE database work. The caller must already own its
// durable application operation CAS. Exact current-own pending receipts, project
// validity and the one-shot marker are rechecked on the PRIMARY under host ->
// project locks. Operation reuse after any result (including a crash) conflicts.
func (s *Store) AuthorizeImportDispatch(ctx context.Context, op Operation, source, destination string) (*ImportProof, error) {
	if !op.valid() {
		return nil, ErrInvalid
	}
	refs, err := s.importReferences(op.ProjectID, source, destination)
	if err != nil {
		return nil, err
	}
	err = s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if e := s.lockProject(tc, op.ProjectID); e != nil {
			return e
		}
		var exists bool
		if e := tx.QueryRow(tc, `SELECT EXISTS(SELECT 1 FROM registry_build_dispatch WHERE operation=$1)`, op.ID).Scan(&exists); e != nil {
			return e
		}
		if exists {
			return ErrConflict
		}
		// Select ALL operation receipts, not only matching owner/project rows: a
		// mismatching envelope must never authorize a subset of an operation.
		rows, e := tx.Query(tc, `SELECT id::text,project_id,owner,generation,host,requested_ref,ref,state,epoch,binding_uid,binding_generation FROM registry_pin WHERE operation=$1 ORDER BY requested_ref FOR UPDATE`, op.ID)
		if e != nil {
			return e
		}
		var frozen []any
		for rows.Next() {
			var id, project, owner, generation, host, requested, ref, state, uid, bindingGen string
			var epoch int64
			if e = rows.Scan(&id, &project, &owner, &generation, &host, &requested, &ref, &state, &epoch, &uid, &bindingGen); e != nil {
				rows.Close()
				return e
			}
			if project != op.ProjectID || owner != op.Owner || generation != op.Generation || host != s.host || state != Pending || uid != "" || bindingGen != "" || ref != requested || !refs.need[requested] {
				rows.Close()
				return ErrConflict
			}
			delete(refs.need, requested)
			frozen = append(frozen, []any{id, requested, epoch})
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(refs.need) != 0 {
			return ErrConflict
		}
		data, _ := json.Marshal(frozen)
		hash := sha256.Sum256(data)
		_, e = tx.Exec(tc, `INSERT INTO registry_import_dispatch(operation,project_id,owner,generation,host,source_ref,destination_ref,kind,pins_hash,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'authorized')`, op.ID, op.ProjectID, op.Owner, op.Generation, s.host, source, destination, refs.kind, hex.EncodeToString(hash[:]))
		return conflict(e)
	})
	if err != nil {
		return nil, err
	}
	return &ImportProof{operation: op.ID, nonce: op.Generation, project: op.ProjectID, owner: op.Owner, host: s.host, source: source, destination: destination, state: &proofState{}}, nil
}

// CancelUnusedImportDispatch invalidates the SAME opaque envelope atomically
// BEFORE attempting the DB CAS. A used/lost/ambiguous proof cannot clear a hold.
func (s *Store) CancelUnusedImportDispatch(ctx context.Context, p *ImportProof, op Operation) error {
	if !op.valid() || p == nil || p.state == nil || p.operation != op.ID || p.nonce != op.Generation || p.project != op.ProjectID || p.owner != op.Owner || p.host != s.host || !p.state.used.CompareAndSwap(false, true) {
		return ErrConflict
	}
	return s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if err := s.lockProject(tc, op.ProjectID); err != nil {
			return err
		}
		tag, err := tx.Exec(tc, `UPDATE registry_import_dispatch SET state='known_no_effect',epoch=epoch+1,updated_at=clock_timestamp() WHERE operation=$1 AND project_id=$2 AND owner=$3 AND generation=$4 AND host=$5 AND source_ref=$6 AND destination_ref=$7 AND state='authorized' AND pod_uid=''`, op.ID, op.ProjectID, op.Owner, op.Generation, s.host, p.source, p.destination)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		return nil
	})
}

type importDispatchRecord struct{ State, UID, Phase, Source, Destination, Kind string }

func (s *Store) importDispatch(ctx context.Context, tx pgx.Tx, op Operation) (*importDispatchRecord, error) {
	d := &importDispatchRecord{}
	var project, owner, generation, host string
	err := tx.QueryRow(ctx, `SELECT project_id,owner,generation,host,state,pod_uid,phase,source_ref,destination_ref,kind FROM registry_import_dispatch WHERE operation=$1 FOR UPDATE`, op.ID).Scan(&project, &owner, &generation, &host, &d.State, &d.UID, &d.Phase, &d.Source, &d.Destination, &d.Kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if project != op.ProjectID || owner != op.Owner || generation != op.Generation || host != s.host {
		return nil, ErrConflict
	}
	return d, nil
}

// guardImportPin is always composed with guardBuildPin inside advancePin's SAME
// namespace transaction, including when no build marker exists. It retains aliases
// while authorized/running/UNKNOWN and never infers a result digest from a phase.
func (s *Store) guardImportPin(ctx context.Context, tx pgx.Tx, p Pin, state, ref, uid string, evidence ...ImportPodEvidence) error {
	d, err := s.importDispatch(ctx, tx, p.Operation)
	if err != nil {
		return err
	}
	if d == nil {
		if len(evidence) != 0 {
			return ErrConflict // import-specific methods cannot mutate non-import pins
		}
		return nil
	}
	if len(evidence) > 1 {
		return ErrConflict
	}
	if len(evidence) == 1 {
		e := evidence[0]
		if !validImportPod(p.Operation, e) || e.Source != d.Source || e.Destination != d.Destination || e.UID != d.UID || (state == Released && (d.State != "terminal" || e.Phase != d.Phase)) {
			return ErrConflict
		}
	}
	if state == Unknown {
		if d.State == "terminal" || d.State == "known_no_effect" {
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE registry_import_dispatch SET state='unknown',epoch=epoch+1,updated_at=clock_timestamp() WHERE operation=$1`, p.Operation.ID)
		return err
	}
	if d.State == "unknown" {
		return ErrConflict
	}
	if state == Released {
		if d.State == "known_no_effect" && p.State == Pending && p.BindingUID == "" {
			return nil
		}
		if d.State != "terminal" {
			return ErrConflict
		}
		if p.State == Pending {
			if len(evidence) != 1 || p.BindingUID != d.UID || p.BindingGeneration != p.Generation {
				return ErrConflict
			}
			if d.Phase == "Succeeded" && (d.Kind == "verify" || p.RequestedRef == d.Destination) {
				return ErrConflict // success must transfer its destination to BOUND
			}
		}
	}
	if state == Bound {
		if d.State != "terminal" || d.Phase != "Succeeded" || p.BindingUID != d.UID || p.BindingGeneration != p.Generation || (d.Kind == "verify" && p.RequestedRef != d.Source) || (d.Kind == "copy" && p.RequestedRef != d.Destination) {
			return ErrConflict
		}
		// Import is content-preserving. A terminal phase cannot manufacture a
		// different digest at the tagged copy destination (or verify source).
		frozen, err := s.importReferences(p.ProjectID, d.Source, d.Destination)
		result, parseErr := s.reference(ref)
		_, sourceDigest, ok := strings.Cut(d.Source, "@")
		if err != nil || parseErr != nil || !ok || frozen.kind != d.Kind || result.Repo != frozen.dest.Repo || result.Digest != sourceDigest {
			return ErrConflict
		}
	}
	if state == Pending && (len(evidence) != 1 || p.Ref != ref || uid == "" || uid != d.UID || (d.State != "observed" && d.State != "terminal")) {
		return ErrConflict
	}
	return nil
}

// ImportPodEvidence must be independently read from the actual pod annotations
// and UID. Phase supplies only positive pod termination, never a registry digest.
type ImportPodEvidence struct{ UID, Operation, Nonce, Source, Destination, Phase string }

func validImportPod(op Operation, e ImportPodEvidence) bool {
	return op.valid() && identityRE.MatchString(e.UID) && e.Operation == op.ID && e.Nonce == op.Generation && (e.Phase == "Pending" || e.Phase == "Running" || e.Phase == "Succeeded" || e.Phase == "Failed")
}

// RecordImportPod freezes the exact operation/owner generation, source/destination
// and UID. Repeated same-envelope Running after terminal is a no-op; conflicting
// UID/refs/terminal outcomes fail closed. UNKNOWN cannot be reopened.
func (s *Store) RecordImportPod(ctx context.Context, op Operation, e ImportPodEvidence) error {
	if !validImportPod(op, e) {
		return ErrConflict
	}
	return s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if err := s.lockProject(tc, op.ProjectID); err != nil {
			return err
		}
		d, err := s.importDispatch(tc, tx, op)
		if err != nil {
			return err
		}
		if d == nil || d.State == "unknown" || d.State == "known_no_effect" || d.Source != e.Source || d.Destination != e.Destination || (d.UID != "" && d.UID != e.UID) {
			return ErrConflict
		}
		state, phase := "observed", ""
		if e.Phase == "Succeeded" || e.Phase == "Failed" {
			state, phase = "terminal", e.Phase
		}
		if d.State == "terminal" {
			if state == "terminal" && d.Phase != phase {
				return ErrConflict
			}
			return nil
		}
		_, err = tx.Exec(tc, `UPDATE registry_import_dispatch SET state=$1,pod_uid=$2,phase=$3,epoch=epoch+1,updated_at=clock_timestamp() WHERE operation=$4`, state, e.UID, phase, op.ID)
		return err
	})
}

func (s *Store) observeImportPod(p Pin, e ImportPodEvidence) error {
	if !validImportPod(p.Operation, e) {
		return ErrConflict
	}
	refs, err := s.importReferences(p.ProjectID, e.Source, e.Destination)
	if err != nil {
		return err
	}
	if !refs.need[p.RequestedRef] || (p.BindingUID != "" && (p.BindingUID != e.UID || p.BindingGeneration != e.Nonce)) {
		return ErrConflict
	}
	return nil
}

// ObserveImportPod retains PENDING source/destination aliases with exact pod UID
// and nonce. RecordImportPod must first commit independently observed evidence.
func (s *Store) ObserveImportPod(ctx context.Context, p Pin, e ImportPodEvidence) (Pin, error) {
	if p.State != Pending {
		return Pin{}, ErrConflict
	}
	if err := s.observeImportPod(p, e); err != nil {
		return Pin{}, err
	}
	r, err := s.reference(p.Ref)
	if err != nil {
		return Pin{}, err
	}
	return s.advancePin(ctx, p, Pending, p.Ref, r.Digest, e.UID, e.Nonce, e)
}

// ReleaseImportTerminal releases only exact observed pending pod receipts after
// the durable matching terminal outcome. Success keeps the destination pin (the
// sole source pin for verify) until ObserveBinding transfers it to a durable owner.
func (s *Store) ReleaseImportTerminal(ctx context.Context, p Pin, e ImportPodEvidence) (Pin, error) {
	if p.State != Pending || p.BindingUID == "" || (e.Phase != "Succeeded" && e.Phase != "Failed") {
		return Pin{}, ErrConflict
	}
	if err := s.observeImportPod(p, e); err != nil {
		return Pin{}, err
	}
	r, err := s.reference(p.Ref)
	if err != nil {
		return Pin{}, err
	}
	return s.advancePin(ctx, p, Released, p.Ref, r.Digest, p.BindingUID, p.BindingGeneration, e)
}

// ImportTerminalKnown only prevents stale error handlers from poisoning terminal
// evidence. It grants no digest, recovery, redispatch or release permission.
func (s *Store) ImportTerminalKnown(ctx context.Context, op Operation) (bool, error) {
	if !op.valid() {
		return false, ErrInvalid
	}
	known := false
	err := s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		d, err := s.importDispatch(tc, tx, op)
		if err != nil {
			return err
		}
		known = d != nil && (d.State == "terminal" || d.State == "known_no_effect")
		return nil
	})
	return known, err
}
