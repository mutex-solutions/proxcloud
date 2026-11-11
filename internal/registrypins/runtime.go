package registrypins

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// RuntimeMode identifies a new consumer incarnation, not a producer workflow.
// Resume/fork require a fresh operation UUID and spec nonce even if a CR is reused.
type RuntimeMode string

const (
	RuntimeCreate RuntimeMode = "create"
	RuntimeResume RuntimeMode = "resume"
	RuntimeFork   RuntimeMode = "fork"
)

// RuntimeSpec is the credential-free, immutable dispatch envelope. Fingerprint is
// the caller's SHA-256 of its full canonical runtime spec; credentials, environment
// and the raw spec must never be persisted in a registry marker. A managed runtime
// has exactly one immutable image. Builtin/library/external skips belong OUTSIDE
// this engine and cannot manufacture a RuntimeProof.
type RuntimeSpec struct {
	ServiceID, SandboxID string
	Mode                 RuntimeMode
	Image, Fingerprint   string
}

var (
	runtimeServiceRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,127}$`)
	runtimeSandboxRE = regexp.MustCompile(`^sbx-([0-9a-f]{8}|[0-9a-f]{16})$`)
	runtimeHashRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func (s *Store) runtimeReference(project string, spec RuntimeSpec) (Ref, error) {
	if !projectRE.MatchString(project) || !runtimeServiceRE.MatchString(spec.ServiceID) || !runtimeSandboxRE.MatchString(spec.SandboxID) || !runtimeHashRE.MatchString(spec.Fingerprint) || (spec.Mode != RuntimeCreate && spec.Mode != RuntimeResume && spec.Mode != RuntimeFork) {
		return Ref{}, ErrInvalid
	}
	r, err := s.reference(spec.Image)
	if err != nil || r.Project != project || r.Digest == "" || r.Tag != "" || r.String() != spec.Image {
		return Ref{}, ErrScope
	}
	return r, nil
}

// RuntimeProof is distinct from BuildProof/ImportProof. No receipt, boolean or
// public constructor grants dispatch. Every value copy shares the same once bit;
// the durable operation marker forbids reminting from another Store or restart.
type RuntimeProof struct {
	op    Operation
	host  string
	spec  RuntimeSpec
	state *proofState
}

// Consume is DB/network-free so it can run inside the caller's existing project
// transaction immediately before the one external effect. Mismatches do not use
// the envelope. A lost/used/canceled proof can never authorize redispatch.
func (p *RuntimeProof) Consume(project string, spec RuntimeSpec) (operation, nonce string, err error) {
	if p == nil || p.state == nil || p.op.ProjectID != project || p.spec != spec || !p.state.used.CompareAndSwap(false, true) {
		return "", "", ErrConflict
	}
	return p.op.ID, p.op.Generation, nil
}

// rejectRuntimeDispatch runs under the existing host -> global operation locks
// in BOTH producer factories. A UUID belongs to at most one action, on any host.
func (s *Store) rejectRuntimeDispatch(ctx context.Context, tx pgx.Tx, operation string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM registry_runtime_dispatch WHERE operation=$1)`, operation).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrConflict
	}
	return nil
}

// AuthorizeRuntimeDispatch is PURE PRIMARY database work, after the caller wins
// its own durable billing/runtime owner CAS. The core cannot replace that protocol.
// It verifies the ENTIRE current operation receipt set, not a matching subset or
// an alias resolved at an earlier epoch. All locks precede the external effect.
func (s *Store) AuthorizeRuntimeDispatch(ctx context.Context, op Operation, spec RuntimeSpec) (*RuntimeProof, error) {
	if !op.valid() {
		return nil, ErrInvalid
	}
	if _, err := s.runtimeReference(op.ProjectID, spec); err != nil {
		return nil, err
	}
	err := s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if err := s.lockDispatchOperation(tc, op.ID); err != nil {
			return err
		}
		if err := s.lockProject(tc, op.ProjectID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(tc, `SELECT EXISTS(SELECT 1 FROM registry_build_dispatch WHERE operation=$1) OR EXISTS(SELECT 1 FROM registry_import_dispatch WHERE operation=$1) OR EXISTS(SELECT 1 FROM registry_runtime_dispatch WHERE operation=$1)`, op.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrConflict
		}
		if err := s.rejectSnapshotDispatch(tc, tx, op.ID); err != nil {
			return err
		}
		rows, err := tx.Query(tc, `SELECT id::text,project_id,owner,generation,host,requested_ref,ref,state,epoch,binding_uid,binding_generation FROM registry_pin WHERE operation=$1 ORDER BY requested_ref FOR UPDATE`, op.ID)
		if err != nil {
			return err
		}
		count := 0
		var pinID string
		var pinEpoch int64
		for rows.Next() {
			var project, owner, generation, host, requested, ref, state, uid, bindingGen string
			if err = rows.Scan(&pinID, &project, &owner, &generation, &host, &requested, &ref, &state, &pinEpoch, &uid, &bindingGen); err != nil {
				rows.Close()
				return err
			}
			count++
			if count != 1 || project != op.ProjectID || owner != op.Owner || generation != op.Generation || host != s.host || state != Pending || uid != "" || bindingGen != "" || requested != spec.Image || ref != requested || pinEpoch < 1 {
				rows.Close()
				return ErrConflict
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrConflict
		}
		_, err = tx.Exec(tc, `INSERT INTO registry_runtime_dispatch(operation,project_id,owner,generation,host,service_id,sandbox_id,mode,image_ref,spec_fingerprint,pin_id,pin_epoch,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'authorized')`, op.ID, op.ProjectID, op.Owner, op.Generation, s.host, spec.ServiceID, spec.SandboxID, string(spec.Mode), spec.Image, spec.Fingerprint, pinID, pinEpoch)
		return conflict(err)
	})
	if err != nil {
		return nil, err
	}
	return &RuntimeProof{op: op, host: s.host, spec: spec, state: &proofState{}}, nil
}

// CancelUnusedRuntimeDispatch invalidates all local proof copies BEFORE the DB
// CAS. Only an exact never-observed authorized marker becomes known_no_effect.
// DB errors/ambiguity burn the envelope but retain the nonexpiring reference hold.
func (s *Store) CancelUnusedRuntimeDispatch(ctx context.Context, p *RuntimeProof) error {
	if p == nil || p.state == nil || !p.op.valid() || p.host != s.host || !p.state.used.CompareAndSwap(false, true) {
		return ErrConflict
	}
	return s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if err := s.lockProject(tc, p.op.ProjectID); err != nil {
			return err
		}
		d, err := s.runtimeDispatch(tc, tx, p.op)
		if err != nil {
			return err
		}
		if d == nil || d.Spec != p.spec || d.State != "authorized" || d.UID != "" || d.CRUID != "" {
			return ErrConflict
		}
		tag, err := tx.Exec(tc, `UPDATE registry_runtime_dispatch SET state='known_no_effect',epoch=epoch+1,updated_at=clock_timestamp() WHERE operation=$1 AND epoch=$2 AND state='authorized' AND pod_uid='' AND cr_uid=''`, p.op.ID, d.Epoch)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		return nil
	})
}

type runtimeDispatchRecord struct {
	State, UID, CRUID, Phase, PinID string
	Spec                            RuntimeSpec
	Epoch, PinEpoch                 int64
}

func (s *Store) runtimeDispatch(ctx context.Context, tx pgx.Tx, op Operation) (*runtimeDispatchRecord, error) {
	d := &runtimeDispatchRecord{}
	var project, owner, generation, host string
	err := tx.QueryRow(ctx, `SELECT project_id,owner,generation,host,state,pod_uid,cr_uid,phase,service_id,sandbox_id,mode,image_ref,spec_fingerprint,pin_id::text,pin_epoch,epoch FROM registry_runtime_dispatch WHERE operation=$1 FOR UPDATE`, op.ID).Scan(&project, &owner, &generation, &host, &d.State, &d.UID, &d.CRUID, &d.Phase, &d.Spec.ServiceID, &d.Spec.SandboxID, &d.Spec.Mode, &d.Spec.Image, &d.Spec.Fingerprint, &d.PinID, &d.PinEpoch, &d.Epoch)
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

// RuntimePodEvidence must be independently observed from the ACTUAL pod and its
// owning CR, including their UID, immutable runtime operation/spec nonce and exact
// image/spec envelope. A CR ReadyCandidate, copied receipt or NotFound is not this
// evidence. CRUID alone is insufficient: a resume can reuse that CR with a new spec.
type RuntimePodEvidence struct {
	UID, CRUID, Operation, Nonce string
	Spec                         RuntimeSpec
	Phase                        string
}

func (s *Store) validRuntimePod(op Operation, e RuntimePodEvidence) bool {
	_, err := s.runtimeReference(op.ProjectID, e.Spec)
	return err == nil && op.valid() && identityRE.MatchString(e.UID) && identityRE.MatchString(e.CRUID) && e.Operation == op.ID && e.Nonce == op.Generation && (e.Phase == "Running" || e.Phase == "Succeeded" || e.Phase == "Failed")
}

func runtimeTerminal(phase string) bool { return phase == "Succeeded" || phase == "Failed" }

// RecordRuntimePod freezes independently observed live identities and positive
// terminal phase on the PRIMARY. UNKNOWN never reopens; same-envelope stale Running
// after terminal is a no-op and cannot revive a released pin or poison newer owners.
func (s *Store) RecordRuntimePod(ctx context.Context, op Operation, e RuntimePodEvidence) error {
	if !s.validRuntimePod(op, e) {
		return ErrConflict
	}
	return s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if err := s.lockProject(tc, op.ProjectID); err != nil {
			return err
		}
		d, err := s.runtimeDispatch(tc, tx, op)
		if err != nil {
			return err
		}
		if d == nil || d.Spec != e.Spec || d.State == "unknown" || d.State == "known_no_effect" || (d.UID != "" && (d.UID != e.UID || d.CRUID != e.CRUID)) {
			return ErrConflict
		}
		if d.State == "terminal" {
			if runtimeTerminal(e.Phase) && d.Phase != e.Phase {
				return ErrConflict
			}
			return nil
		}
		state := "observed"
		if runtimeTerminal(e.Phase) {
			state = "terminal"
		}
		tag, err := tx.Exec(tc, `UPDATE registry_runtime_dispatch SET state=$1,pod_uid=$2,cr_uid=$3,phase=$4,epoch=epoch+1,updated_at=clock_timestamp() WHERE operation=$5 AND epoch=$6`, state, e.UID, e.CRUID, e.Phase, op.ID, d.Epoch)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		return nil
	})
}

// RuntimeTerminalKnown only protects monotone terminal evidence from stale error
// handlers. It grants no dispatch, release, recovery or image provenance authority.
func (s *Store) RuntimeTerminalKnown(ctx context.Context, op Operation) (bool, error) {
	if !op.valid() {
		return false, ErrInvalid
	}
	known := false
	err := s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		d, err := s.runtimeDispatch(tc, tx, op)
		if err != nil {
			return err
		}
		known = d != nil && (d.State == "terminal" || d.State == "known_no_effect")
		return nil
	})
	return known, err
}

func (s *Store) runtimePinEvidence(p Pin, e RuntimePodEvidence) error {
	if !s.validRuntimePod(p.Operation, e) || p.Ref != e.Spec.Image || p.RequestedRef != e.Spec.Image || (p.BindingUID != "" && (p.BindingUID != e.UID || p.BindingGeneration != e.Nonce)) {
		return ErrConflict
	}
	return nil
}

// ObserveRuntimePod binds an immutable CONSUMER reference while its worker is
// Running (unlike a producer destination). RecordRuntimePod must first commit the
// independently read evidence. BindingUID is the POD UID, never the template/CR UID.
func (s *Store) ObserveRuntimePod(ctx context.Context, p Pin, e RuntimePodEvidence) (Pin, error) {
	if p.State != Pending || s.runtimePinEvidence(p, e) != nil {
		return Pin{}, ErrConflict
	}
	r, _ := s.reference(p.Ref)
	return s.advancePin(ctx, p, Bound, p.Ref, r.Digest, e.UID, e.Nonce, pinEvidence{runtimePod: &e})
}

// ReleaseRuntimeTerminal requires previously observed exact pod/CR/spec evidence
// AND the same durable positive terminal outcome. Ready, deletion responses,
// deadlines, expiry and missing objects are never release authority. Future callers
// must ALSO retire the desired owner generation and exclude stale Apply/controller
// recreation before using this trusted observation API; this core cannot prove it.
func (s *Store) ReleaseRuntimeTerminal(ctx context.Context, p Pin, e RuntimePodEvidence) (Pin, error) {
	if (p.State != Pending && p.State != Bound) || p.BindingUID == "" || !runtimeTerminal(e.Phase) || s.runtimePinEvidence(p, e) != nil {
		return Pin{}, ErrConflict
	}
	r, _ := s.reference(p.Ref)
	return s.advancePin(ctx, p, Released, p.Ref, r.Digest, e.UID, e.Nonce, pinEvidence{runtimePod: &e})
}

// guardRuntimePin is composed AFTER both producer guards even when their markers
// are absent. The private typed evidence is the sole runtime bind/terminal grant;
// generic receipts, Build/Import evidence and caller booleans cannot supply it.
func (s *Store) guardRuntimePin(ctx context.Context, tx pgx.Tx, p Pin, state, ref, uid, generation string, e *RuntimePodEvidence) error {
	d, err := s.runtimeDispatch(ctx, tx, p.Operation)
	if err != nil {
		return err
	}
	if d == nil {
		if e != nil {
			return ErrConflict
		}
		return nil
	}
	if d.PinID != p.ID || d.PinEpoch != p.Epoch || d.Spec.Image != p.Ref || p.RequestedRef != p.Ref || ref != p.Ref {
		return ErrConflict
	}
	if e != nil && (s.runtimePinEvidence(p, *e) != nil || e.Spec != d.Spec || e.UID != d.UID || e.CRUID != d.CRUID || uid != e.UID || generation != e.Nonce) {
		return ErrConflict
	}
	if state == Unknown {
		if e != nil || d.State == "terminal" || d.State == "known_no_effect" {
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE registry_runtime_dispatch SET state='unknown',pin_epoch=pin_epoch+1,epoch=epoch+1,updated_at=clock_timestamp() WHERE operation=$1 AND epoch=$2`, p.Operation.ID, d.Epoch)
		return err
	}
	if d.State == "unknown" {
		return ErrConflict
	}
	switch state {
	case Bound:
		if e == nil || (d.State != "observed" && d.State != "terminal") || p.State != Pending || (d.State == "terminal" && e.Phase != d.Phase) || (d.State == "observed" && e.Phase != "Running") {
			return ErrConflict
		}
	case Released:
		if d.State == "known_no_effect" && p.State == Pending && p.BindingUID == "" && e == nil && uid == "" && generation == "" {
			break
		}
		if e == nil || d.State != "terminal" || e.Phase != d.Phase || !runtimeTerminal(e.Phase) || p.BindingUID != d.UID || p.BindingGeneration != p.Generation || (p.State != Pending && p.State != Bound) {
			return ErrConflict
		}
	default:
		return ErrConflict
	}
	// The marker and pin advance in the SAME transaction. A forged/stale receipt
	// rolls this write back when advancePin's complete pin CAS loses.
	_, err = tx.Exec(ctx, `UPDATE registry_runtime_dispatch SET pin_epoch=pin_epoch+1,epoch=epoch+1,updated_at=clock_timestamp() WHERE operation=$1 AND epoch=$2`, p.Operation.ID, d.Epoch)
	return err
}
