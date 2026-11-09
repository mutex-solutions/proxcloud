package registrypins

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	Pending  = "pending"
	Bound    = "bound"
	Unknown  = "unknown"
	Released = "released"
)

// Pin is a full CAS receipt, not a bearer permission to perform arbitrary I/O.
// Only an exact returned receipt can advance its state; previous epochs stay stale.
type Pin struct {
	ID string
	Operation
	Ref, RequestedRef, State, BindingUID, BindingGeneration string
	Epoch                                                   int64
}

// BeginPins commits all pins before the caller resolves a tag, starts its project
// transaction or dispatches a workload. Any active prune in a repo blocks ALL new
// acquisitions there. No existing operation is silently replayed or redispatched.
func (s *Store) BeginPins(ctx context.Context, op Operation, refs []string) ([]Pin, error) {
	if !op.valid() || len(refs) == 0 || len(refs) > 16 {
		return nil, ErrInvalid
	}
	parsed := make([]Ref, len(refs))
	seen := map[string]bool{}
	for i, raw := range refs {
		r, err := s.reference(raw)
		if err != nil || seen[r.String()] {
			return nil, ErrInvalid
		}
		if r.Project != op.ProjectID {
			return nil, ErrScope
		}
		parsed[i], seen[r.String()] = r, true
	}
	var out []Pin
	err := s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if err := s.lockProject(tc, op.ProjectID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(tc, `SELECT EXISTS(SELECT 1 FROM registry_pin WHERE operation=$1)`, op.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrConflict
		}
		for _, r := range parsed {
			if err := tx.QueryRow(tc, `SELECT EXISTS(SELECT 1 FROM registry_prune_intent WHERE host=$1 AND repo=$2 AND state<>'settled')`, s.host, r.Repo).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return hold("prune_intent_active")
			}
			p := Pin{ID: uuid.NewString(), Operation: op, Ref: r.String(), RequestedRef: r.String(), State: Pending, Epoch: 1}
			_, err := tx.Exec(tc, `INSERT INTO registry_pin(id,operation,project_id,owner,generation,host,repo,requested_ref,ref,digest,state,epoch) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8,$9,'pending',1)`, p.ID, op.ID, op.ProjectID, op.Owner, op.Generation, s.host, r.Repo, p.Ref, r.Digest)
			if err != nil {
				return conflict(err)
			}
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ObserveBinding atomically replaces a pending alias by its resolved digest ONLY
// after the caller positively observed an exact durable DB/CR UID+generation
// binding to that digest. A dispatched build's BASE is NOT releasable on return
// from StartBuild: keep the binding until that exact pod is positively terminal.
// BindingEvidence must come from the observed live object, NOT copied from the
// operation receipt. Generation is its stamped immutable expected owner/spec nonce.
type BindingEvidence struct{ UID, Generation, DigestRef string }

func (s *Store) ObserveBinding(ctx context.Context, p Pin, observed BindingEvidence) (Pin, error) {
	old, err := s.reference(p.Ref)
	if err != nil {
		return Pin{}, err
	}
	r, err := s.reference(observed.DigestRef)
	if err != nil || r.Digest == "" || old.Repo != r.Repo || old.Digest != "" && old.Digest != r.Digest || !identityRE.MatchString(observed.UID) || p.State != Pending || observed.Generation != p.Generation {
		return Pin{}, ErrInvalid
	}
	return s.advancePin(ctx, p, Bound, r.String(), r.Digest, observed.UID, observed.Generation)
}

// ReleaseKnownNoEffect is only for positively known failure before any effect;
// an apply error, cancellation, missing pod or HTTP timeout is NOT such proof.
func (s *Store) ReleaseKnownNoEffect(ctx context.Context, p Pin) (Pin, error) {
	if p.State != Pending || p.BindingUID != "" {
		return Pin{}, ErrConflict
	}
	r, err := s.reference(p.Ref)
	if err != nil {
		return Pin{}, err
	}
	return s.advancePin(ctx, p, Released, p.Ref, r.Digest, "", "")
}

// ReleaseBinding accepts positive removal/termination evidence for the EXACT
// binding UID and owner generation. It cannot release pending/UNKNOWN receipts.
// This DB-only primitive cannot itself prove external disappearance; caller
// integration and observation checks are a separate prerequisite PR.
func (s *Store) ReleaseBinding(ctx context.Context, p Pin, observed BindingEvidence) (Pin, error) {
	rf, err := s.reference(observed.DigestRef)
	if err != nil || p.State != Bound || p.BindingUID == "" || observed.UID != p.BindingUID || observed.Generation != p.BindingGeneration || p.BindingGeneration != p.Generation || rf.String() != p.Ref {
		return Pin{}, ErrConflict
	}
	r, err := s.reference(p.Ref)
	if err != nil {
		return Pin{}, err
	}
	return s.advancePin(ctx, p, Released, p.Ref, r.Digest, p.BindingUID, p.BindingGeneration)
}

// MarkUnknown is a one-way hold. There is deliberately no TTL cleanup, retry,
// takeover or public clear-hold method; later operator recovery needs its own
// reviewed evidence contract, including exclusion of stale external effects.
func (s *Store) MarkUnknown(ctx context.Context, p Pin) (Pin, error) {
	if p.State != Pending && p.State != Bound {
		return Pin{}, ErrConflict
	}
	r, err := s.reference(p.Ref)
	if err != nil {
		return Pin{}, err
	}
	return s.advancePin(ctx, p, Unknown, p.Ref, r.Digest, p.BindingUID, p.BindingGeneration)
}
func (s *Store) advancePin(ctx context.Context, p Pin, state, ref, digest, uid, bindingGeneration string) (Pin, error) {
	r, err := s.reference(p.Ref)
	id, e := uuid.Parse(p.ID)
	if err != nil || e != nil || id == uuid.Nil || id.String() != p.ID || !p.Operation.valid() || p.Epoch < 1 || r.Project != p.ProjectID {
		return Pin{}, ErrInvalid
	}
	requested, err := s.reference(p.RequestedRef)
	if err != nil || requested.Repo != r.Repo {
		return Pin{}, ErrInvalid
	}
	out := p
	err = s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if err := s.guardBuildPin(tc, tx, p, state, ref, uid); err != nil {
			return err
		}
		if err := s.guardImportPin(tc, tx, p, state, ref, uid); err != nil {
			return err
		}
		return conflict(tx.QueryRow(tc, `UPDATE registry_pin SET state=$1,ref=$2,digest=$3,binding_uid=$4,binding_generation=$18,epoch=epoch+1,updated_at=clock_timestamp()
 WHERE id=$5 AND operation=$6 AND owner=$7 AND generation=$8 AND host=$9 AND repo=$10 AND ref=$11 AND state=$12 AND epoch=$13 AND binding_uid=$14 AND project_id=$15 AND binding_generation=$16 AND requested_ref=$17 RETURNING epoch`, state, ref, digest, uid, p.ID, p.Operation.ID, p.Owner, p.Generation, s.host, r.Repo, p.Ref, p.State, p.Epoch, p.BindingUID, p.ProjectID, p.BindingGeneration, p.RequestedRef, bindingGeneration).Scan(&out.Epoch))
	})
	if err != nil {
		return Pin{}, err
	}
	out.State, out.Ref, out.BindingUID, out.BindingGeneration = state, ref, uid, bindingGeneration
	return out, nil
}
