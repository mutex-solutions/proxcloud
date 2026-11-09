package registrypins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// PinsForOperation only recovers exact already-committed receipts. It does NOT
// authorize dispatch, create pins, change their epochs or clear UNKNOWN.
func (s *Store) PinsForOperation(ctx context.Context, op Operation) ([]Pin, error) {
	if !op.valid() {
		return nil, ErrInvalid
	}
	var out []Pin
	err := s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(tc, `SELECT id::text,ref,requested_ref,state,epoch,binding_uid,binding_generation FROM registry_pin WHERE operation=$1 AND project_id=$2 AND owner=$3 AND generation=$4 AND host=$5 ORDER BY requested_ref`, op.ID, op.ProjectID, op.Owner, op.Generation, s.host)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p := Pin{Operation: op}
			if err = rows.Scan(&p.ID, &p.Ref, &p.RequestedRef, &p.State, &p.Epoch, &p.BindingUID, &p.BindingGeneration); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// BuildProof is an unforgeable, in-process single-use envelope for a caller that
// already won its durable application dispatch CAS. Receipts alone never decide
// whether a build may be redispatched; that decision belongs to the owner record.
type proofState struct{ used atomic.Bool }
type BuildProof struct {
	operation, nonce, project, base, dest string
	state                                 *proofState // shallow copies share the sole use/cancel bit
}

func (s *Store) AuthorizeBuildDispatch(ctx context.Context, op Operation, base, dest string) (*BuildProof, error) {
	d, err := s.reference(dest)
	if err != nil || d.Project != op.ProjectID || d.Tag == "" {
		return nil, ErrScope
	}
	need := map[string]bool{d.String(): true}
	if r, e := s.reference(base); e == nil {
		if r.Project != op.ProjectID {
			return nil, ErrScope
		}
		need[r.String()] = true
	} else if strings.HasPrefix(base, s.host+"/t/") {
		return nil, ErrScope
	}
	err = s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if e := s.lockDispatchOperation(tc, op.ID); e != nil {
			return e
		}
		if e := s.lockProject(tc, op.ProjectID); e != nil {
			return e
		}
		if e := s.rejectImportDispatch(tc, tx, op.ID); e != nil {
			return e
		}
		if e := s.rejectRuntimeDispatch(tc, tx, op.ID); e != nil {
			return e
		}
		rows, e := tx.Query(tc, `SELECT id::text,requested_ref,ref,state,epoch,binding_uid FROM registry_pin WHERE operation=$1 AND project_id=$2 AND owner=$3 AND generation=$4 AND host=$5 ORDER BY requested_ref FOR UPDATE`, op.ID, op.ProjectID, op.Owner, op.Generation, s.host)
		if e != nil {
			return e
		}
		var frozen []any
		for rows.Next() {
			var id, requested, ref, state, uid string
			var epoch int64
			if e = rows.Scan(&id, &requested, &ref, &state, &epoch, &uid); e != nil {
				rows.Close()
				return e
			}
			if state != Pending || uid != "" || ref != requested || !need[requested] {
				rows.Close()
				return ErrConflict
			}
			delete(need, requested)
			frozen = append(frozen, []any{id, requested, epoch})
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(need) != 0 {
			return ErrConflict
		}
		data, _ := json.Marshal(frozen)
		hash := sha256.Sum256(data)
		_, e = tx.Exec(tc, `INSERT INTO registry_build_dispatch(operation,project_id,owner,generation,host,base_ref,destination_ref,pins_hash,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'authorized')`, op.ID, op.ProjectID, op.Owner, op.Generation, s.host, base, dest, hex.EncodeToString(hash[:]))
		return conflict(e)
	})
	if err != nil {
		return nil, err
	}
	return &BuildProof{operation: op.ID, nonce: op.Generation, project: op.ProjectID, base: base, dest: dest, state: &proofState{}}, nil
}

// Consume is intentionally DB/network-free: pins committed before the caller's
// existing project/rotation transaction. It cannot mint or renew a receipt.
func (p *BuildProof) Consume(project, base, dest string) (operation, nonce string, err error) {
	if p == nil || p.state == nil || p.project != project || p.base != base || p.dest != dest || !p.state.used.CompareAndSwap(false, true) {
		return "", "", ErrConflict
	}
	return p.operation, p.nonce, nil
}

// CancelUnusedDispatch is the only positive pre-dispatch cancellation. Invalidating
// the shared opaque envelope precedes the DB CAS; ambiguity retains the hold.
func (s *Store) CancelUnusedDispatch(ctx context.Context, p *BuildProof, op Operation) error {
	if p == nil || p.state == nil || p.operation != op.ID || p.nonce != op.Generation || p.project != op.ProjectID || !p.state.used.CompareAndSwap(false, true) {
		return ErrConflict
	}
	return s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		tag, e := tx.Exec(tc, `UPDATE registry_build_dispatch SET state='known_no_effect',epoch=epoch+1,updated_at=clock_timestamp() WHERE operation=$1 AND project_id=$2 AND owner=$3 AND generation=$4 AND host=$5 AND base_ref=$6 AND destination_ref=$7 AND state='authorized' AND pod_uid=''`, op.ID, op.ProjectID, op.Owner, op.Generation, s.host, p.base, p.dest)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		return nil
	})
}

type dispatchRecord struct{ State, UID, Phase, Base, Dest string }

func (s *Store) dispatch(ctx context.Context, tx pgx.Tx, p Pin) (*dispatchRecord, error) {
	d := &dispatchRecord{}
	err := tx.QueryRow(ctx, `SELECT state,pod_uid,phase,base_ref,destination_ref FROM registry_build_dispatch WHERE operation=$1 AND project_id=$2 AND owner=$3 AND generation=$4 AND host=$5 FOR UPDATE`, p.Operation.ID, p.ProjectID, p.Owner, p.Generation, s.host).Scan(&d.State, &d.UID, &d.Phase, &d.Base, &d.Dest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

// guardBuildPin is called inside the same namespace transaction as every pin
// update: an issued envelope cannot outlive a reference that a caller released.
func (s *Store) guardBuildPin(ctx context.Context, tx pgx.Tx, p Pin, state, ref, uid string) error {
	d, err := s.dispatch(ctx, tx, p)
	if err != nil || d == nil {
		return err
	}
	if state == Unknown {
		if d.State == "terminal" || d.State == "known_no_effect" {
			return ErrConflict
		}
		if d.State != "terminal" && d.State != "known_no_effect" {
			_, err = tx.Exec(ctx, `UPDATE registry_build_dispatch SET state='unknown',epoch=epoch+1 WHERE operation=$1`, p.Operation.ID)
		}
		return err
	}
	if d.State == "unknown" {
		return ErrConflict
	}
	if state == Released {
		if d.State == "known_no_effect" && p.BindingUID == "" {
			return nil
		}
		if d.State != "terminal" {
			return ErrConflict
		}
		if p.State == Pending && p.RequestedRef == d.Dest && d.Phase == "Succeeded" {
			return ErrConflict
		}
		if p.State == Pending && (p.BindingUID != d.UID || p.BindingGeneration != p.Generation) {
			return ErrConflict
		}
	}
	if state == Bound {
		if d.State != "terminal" || d.Phase != "Succeeded" || p.RequestedRef != d.Dest {
			return ErrConflict
		}
	}
	if state == Pending && uid != "" && d.State != "observed" && d.State != "terminal" {
		return ErrConflict
	}
	return nil
}

// RecordBuildPod freezes independently read UID and positive terminal phase in the
// durable one-shot dispatch marker. UNKNOWN is never reopened by later evidence.
func (s *Store) RecordBuildPod(ctx context.Context, op Operation, e BuildPodEvidence) error {
	if !op.valid() || !identityRE.MatchString(e.UID) || e.Operation != op.ID || e.Nonce != op.Generation {
		return ErrConflict
	}
	return s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		p := Pin{Operation: op}
		d, err := s.dispatch(tc, tx, p)
		if err != nil {
			return err
		}
		if d == nil || d.State == "unknown" || d.State == "known_no_effect" || d.Base != e.BaseRef || d.Dest != e.DestinationRef || d.UID != "" && d.UID != e.UID {
			return ErrConflict
		}
		state := "observed"
		phase := ""
		if e.Phase == "Succeeded" || e.Phase == "Failed" {
			state, phase = "terminal", e.Phase
		}
		if d.State == "terminal" {
			// Same-envelope stale Running evidence cannot downgrade or poison
			// a committed positive terminal observation.
			if state == "terminal" && d.Phase != phase {
				return ErrConflict
			}
			return nil
		}
		_, err = tx.Exec(tc, `UPDATE registry_build_dispatch SET state=$1,pod_uid=$2,phase=$3,epoch=epoch+1,updated_at=clock_timestamp() WHERE operation=$4`, state, e.UID, phase, op.ID)
		return err
	})
}

// BuildTerminalKnown prevents stale error handlers from poisoning a positive
// terminal marker. It does not manufacture an image digest or authorize READY.
func (s *Store) BuildTerminalKnown(ctx context.Context, op Operation) (bool, error) {
	if !op.valid() {
		return false, ErrInvalid
	}
	known := false
	err := s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		d, e := s.dispatch(tc, tx, Pin{Operation: op})
		if e != nil {
			return e
		}
		known = d != nil && (d.State == "terminal" || d.State == "known_no_effect")
		return nil
	})
	return known, err
}

// BuildPodEvidence is independently read from a live pod, not synthesized from a
// receipt. Only Succeeded/Failed are terminal; timeout/absence/Unknown are not.
type BuildPodEvidence struct{ UID, Operation, Nonce, BaseRef, DestinationRef, Phase string }

func (s *Store) observePod(p Pin, e BuildPodEvidence) error {
	if !identityRE.MatchString(e.UID) || e.Operation != p.Operation.ID || e.Nonce != p.Generation {
		return ErrConflict
	}
	d, err := s.reference(e.DestinationRef)
	if err != nil || d.Project != p.ProjectID || d.Tag == "" {
		return ErrScope
	}
	if p.RequestedRef != e.BaseRef && p.RequestedRef != d.String() {
		return ErrConflict
	}
	if p.BindingUID != "" && (p.BindingUID != e.UID || p.BindingGeneration != e.Nonce) {
		return ErrConflict
	}
	return nil
}

// ObserveBuildPod retains a pending alias while binding its exact pod UID+nonce.
// It never drops a pre-resolution tag while a pod may still resolve/pull it.
func (s *Store) ObserveBuildPod(ctx context.Context, p Pin, e BuildPodEvidence) (Pin, error) {
	if p.State != Pending {
		return Pin{}, ErrConflict
	}
	if err := s.observePod(p, e); err != nil {
		return Pin{}, err
	}
	r, err := s.reference(p.Ref)
	if err != nil {
		return Pin{}, err
	}
	return s.advancePin(ctx, p, Pending, p.Ref, r.Digest, e.UID, e.Nonce)
}

// ReleaseBuildTerminal releases only a previously observed exact pod binding,
// after the same pod positively reports terminal. It cannot release UNKNOWN or
// replace a known-failure/registry-outcome hold with a guessed no-effect result.
func (s *Store) ReleaseBuildTerminal(ctx context.Context, p Pin, e BuildPodEvidence) (Pin, error) {
	if p.State != Pending || p.BindingUID == "" || (e.Phase != "Succeeded" && e.Phase != "Failed") {
		return Pin{}, ErrConflict
	}
	if err := s.observePod(p, e); err != nil {
		return Pin{}, err
	}
	r, err := s.reference(p.Ref)
	if err != nil {
		return Pin{}, err
	}
	return s.advancePin(ctx, p, Released, p.Ref, r.Digest, p.BindingUID, p.BindingGeneration)
}
