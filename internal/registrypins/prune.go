package registrypins

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const (
	Prepared = "prepared"
	Sending  = "sending"
	Settled  = "settled"
)

type PruneIntent struct {
	Operation
	Ref, State, Outcome string
	Epoch               int64
}

// BeginPrune is only a cooperative DB exclusion primitive, NOT deletion
// authorization. Future exact-scope GC still needs complete writer participation,
// legacy bindings and current registry/cluster/metadata proofs. No boolean can
// bypass those requirements or the unconditional production RealDeleteHold.
// Conservatively ANY unreleased pin in the repo (including another digest or an
// unresolved BUILD DEST/tag) prevents an intent; no alias resolution race exists.
func (s *Store) BeginPrune(ctx context.Context, op Operation, digestRef string) (PruneIntent, error) {
	r, err := s.reference(digestRef)
	if err != nil || r.Digest == "" || !op.valid() {
		return PruneIntent{}, ErrInvalid
	}
	if r.Project != op.ProjectID {
		return PruneIntent{}, ErrScope
	}
	out := PruneIntent{Operation: op, Ref: r.String(), State: Prepared, Epoch: 1}
	err = s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if err := s.lockProject(tc, op.ProjectID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(tc, `SELECT EXISTS(SELECT 1 FROM registry_pin WHERE host=$1 AND repo=$2 AND state<>'released')`, s.host, r.Repo).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return hold("reference_pin_active")
		}
		if err := tx.QueryRow(tc, `SELECT EXISTS(SELECT 1 FROM registry_prune_intent WHERE host=$1 AND repo=$2 AND state<>'settled')`, s.host, r.Repo).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return hold("prune_intent_active")
		}
		_, err := tx.Exec(tc, `INSERT INTO registry_prune_intent(id,project_id,owner,generation,host,repo,ref,state,epoch) VALUES($1,$2,$3,$4,$5,$6,$7,'prepared',1)`, op.ID, op.ProjectID, op.Owner, op.Generation, s.host, r.Repo, out.Ref)
		return conflict(err)
	})
	if err != nil {
		return PruneIntent{}, err
	}
	return out, nil
}

// MarkSending commits BEFORE a single caller HTTP attempt (which this package
// never makes). Calling it twice conflicts, even with the same stable operation.
// A process that dies after this commit is UNKNOWN; it must never replay DELETE.
func (s *Store) MarkSending(ctx context.Context, p PruneIntent) (PruneIntent, error) {
	if p.State != Prepared {
		return PruneIntent{}, ErrConflict
	}
	return s.advancePrune(ctx, p, Sending, "")
}

// PruneUnknown permanently retains exclusion after ambiguous send/crash. There
// are no leases, resends or clearHold APIs. HEAD absence, context cancellation or
// a lost pod does not prove that an old DELETE cannot still arrive later.
func (s *Store) PruneUnknown(ctx context.Context, p PruneIntent) (PruneIntent, error) {
	if p.State != Prepared && p.State != Sending {
		return PruneIntent{}, ErrConflict
	}
	return s.advancePrune(ctx, p, Unknown, "")
}

// SettleKnown requires the definitive response to the sole sent attempt AND no
// outstanding/retryable old HTTP operation. It is not crash recovery or replay.
// UNKNOWN is intentionally excluded. Outcomes count deleted vs absent separately.
func (s *Store) SettleKnown(ctx context.Context, p PruneIntent, outcome string) (PruneIntent, error) {
	if p.State != Sending || (outcome != "deleted" && outcome != "already_absent" && outcome != "not_deleted") {
		return PruneIntent{}, ErrConflict
	}
	return s.advancePrune(ctx, p, Settled, outcome)
}
func (s *Store) advancePrune(ctx context.Context, p PruneIntent, state, outcome string) (PruneIntent, error) {
	r, err := s.reference(p.Ref)
	if err != nil || r.Digest == "" || !p.Operation.valid() || p.Epoch < 1 || r.Project != p.ProjectID {
		return PruneIntent{}, ErrInvalid
	}
	out := p
	err = s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		return conflict(tx.QueryRow(tc, `UPDATE registry_prune_intent SET state=$1,outcome=$2,epoch=epoch+1,updated_at=clock_timestamp()
 WHERE id=$3 AND owner=$4 AND generation=$5 AND host=$6 AND repo=$7 AND ref=$8 AND state=$9 AND epoch=$10 AND outcome=$11 AND project_id=$12 RETURNING epoch`, state, outcome, p.ID, p.Owner, p.Generation, s.host, r.Repo, p.Ref, p.State, p.Epoch, p.Outcome, p.ProjectID).Scan(&out.Epoch))
	})
	if err != nil {
		return PruneIntent{}, err
	}
	out.State, out.Outcome = state, outcome
	return out, nil
}
