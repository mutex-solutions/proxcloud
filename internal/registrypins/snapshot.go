package registrypins

// Snapshot is a dormant producer action, not content-preserving import and not a
// runtime consumer. This core does no capture, provider, registry or cluster I/O.
import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	distref "github.com/distribution/reference"
	"github.com/jackc/pgx/v5"
)

const snapshotAction = "snapshot/v1"

// SnapshotSpec contains only frozen correlations. Hashes/UID stamps do NOT prove
// current source/provider identity or that capture used them. A future trusted
// caller must independently establish those facts and the fixed assembly spec.
// Never put raw CRs, environment, credentials, tokens or LayerURL in this value.
type SnapshotSpec struct {
	ServiceID, SandboxID, SourceCRUID, SourcePodUID string
	SourceFingerprint, ProviderRevisionHash         string
	Base, Destination, ActionSpecFingerprint        string
}

func (SnapshotSpec) String() string       { return "[snapshot spec redacted]" }
func (SnapshotSpec) GoString() string     { return "[snapshot spec redacted]" }
func (SnapshotSpec) LogValue() slog.Value { return slog.StringValue("[snapshot spec redacted]") }

type snapshotRefs struct {
	base, dest Ref
	need       map[string]bool
}

// Representation validation is NOT external-origin or push-credential policy.
// Same-host non-library references cannot disguise managed scope as external.
func (s *Store) snapshotReferences(project string, spec SnapshotSpec) (snapshotRefs, error) {
	out := snapshotRefs{need: map[string]bool{}}
	if !projectRE.MatchString(project) || !runtimeServiceRE.MatchString(spec.ServiceID) || !runtimeSandboxRE.MatchString(spec.SandboxID) || !identityRE.MatchString(spec.SourceCRUID) || !identityRE.MatchString(spec.SourcePodUID) || !runtimeHashRE.MatchString(spec.SourceFingerprint) || !runtimeHashRE.MatchString(spec.ProviderRevisionHash) || !runtimeHashRE.MatchString(spec.ActionSpecFingerprint) || len(spec.Base) > 512 || len(spec.Destination) > 512 || strings.ContainsAny(spec.Base+spec.Destination, "?#[]%\\\t\r\n ") {
		return out, ErrInvalid
	}
	first, rest, ok := strings.Cut(spec.Base, "/")
	host, err := CanonicalHost(first)
	if !ok || err != nil || host != first {
		return out, ErrInvalid
	}
	parsed, err := distref.Parse(spec.Base)
	canonical, immutable := parsed.(distref.Canonical)
	_, tagged := parsed.(distref.Tagged)
	if err != nil || !immutable || tagged || canonical.String() != spec.Base || !digestRE.MatchString(canonical.Digest().String()) {
		return out, ErrInvalid
	}
	if host == s.host {
		if strings.HasPrefix(rest, "library/") { // positively classified operator library
			if len(strings.TrimSuffix(rest, "@"+canonical.Digest().String())) <= len("library/") {
				return out, ErrScope
			}
		} else {
			r, e := s.reference(spec.Base)
			if e != nil || r.Project != project || r.String() != spec.Base || r.Digest == "" || r.Tag != "" {
				return out, ErrScope
			}
			out.base = r
			out.need[r.String()] = true
		}
	}
	d, err := s.reference(spec.Destination)
	if err != nil || d.Project != project || d.Tag == "" || d.Digest != "" || d.String() != spec.Destination || spec.Base == spec.Destination {
		return out, ErrScope
	}
	out.dest = d
	out.need[d.String()] = true
	return out, nil
}

// SnapshotProof is opaque, single-use and distinct from every other action.
// Copies share the same use/cancel bit. Lost proofs are never reminted.
type SnapshotProof struct {
	op    Operation
	host  string
	spec  SnapshotSpec
	state *proofState
}

func (SnapshotProof) String() string       { return "[snapshot proof redacted]" }
func (SnapshotProof) GoString() string     { return "[snapshot proof redacted]" }
func (SnapshotProof) LogValue() slog.Value { return slog.StringValue("[snapshot proof redacted]") }
func (p *SnapshotProof) Consume(project string, spec SnapshotSpec) (operation, nonce string, err error) {
	if p == nil || p.state == nil || p.op.ProjectID != project || p.spec != spec || !p.state.used.CompareAndSwap(false, true) {
		return "", "", ErrConflict
	}
	return p.op.ID, p.op.Generation, nil
}

const snapshotSchema = `CREATE TABLE IF NOT EXISTS registry_snapshot_dispatch (
 operation uuid PRIMARY KEY,project_id text NOT NULL,owner text NOT NULL,generation text NOT NULL,host text NOT NULL,
 action text NOT NULL DEFAULT 'snapshot/v1' CHECK(action='snapshot/v1'),spec jsonb NOT NULL CHECK(jsonb_typeof(spec)='object'),
 base_pin_id uuid,base_pin_epoch bigint NOT NULL DEFAULT 0 CHECK(base_pin_epoch>=0),
 destination_pin_id uuid NOT NULL,destination_pin_epoch bigint NOT NULL CHECK(destination_pin_epoch>0),
 state text NOT NULL CHECK(state IN ('authorized','observed','terminal','unknown','known_no_effect')),
 pod_uid text NOT NULL DEFAULT '',phase text NOT NULL DEFAULT '',epoch bigint NOT NULL DEFAULT 1 CHECK(epoch>0),
 destination_digest text NOT NULL DEFAULT '',destination_owner_uid text NOT NULL DEFAULT '',destination_owner_generation text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((base_pin_id IS NULL)=(base_pin_epoch=0)),CHECK((state='terminal')=(phase IN ('Succeeded','Failed'))),
 CHECK(state NOT IN ('observed','terminal') OR pod_uid<>''),CHECK(state<>'observed' OR phase IN ('Pending','Running')),
 CHECK(state NOT IN ('authorized','known_no_effect') OR (pod_uid='' AND phase='')),
 CHECK((destination_digest='')=(destination_owner_uid='')),CHECK((destination_digest='')=(destination_owner_generation='')),
 CHECK(destination_digest='' OR (state='terminal' AND phase='Succeeded' AND destination_owner_generation=generation))
);`

func (s *Store) rejectSnapshotDispatch(ctx context.Context, tx pgx.Tx, operation string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM registry_snapshot_dispatch WHERE operation=$1)`, operation).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrConflict
	}
	return nil
}

// AuthorizeSnapshotDispatch commits its marker BEFORE any future owner/project or
// rotation transaction can dispatch. Exactly the entire current pin set is frozen.
func (s *Store) AuthorizeSnapshotDispatch(ctx context.Context, op Operation, spec SnapshotSpec) (*SnapshotProof, error) {
	if !op.valid() {
		return nil, ErrInvalid
	}
	refs, err := s.snapshotReferences(op.ProjectID, spec)
	if err != nil {
		return nil, err
	}
	err = s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if e := s.lockDispatchOperation(tc, op.ID); e != nil {
			return e
		}
		if e := s.lockProject(tc, op.ProjectID); e != nil {
			return e
		}
		var exists bool
		if e := tx.QueryRow(tc, `SELECT EXISTS(SELECT 1 FROM registry_build_dispatch WHERE operation=$1) OR EXISTS(SELECT 1 FROM registry_import_dispatch WHERE operation=$1) OR EXISTS(SELECT 1 FROM registry_runtime_dispatch WHERE operation=$1) OR EXISTS(SELECT 1 FROM registry_snapshot_dispatch WHERE operation=$1)`, op.ID).Scan(&exists); e != nil {
			return e
		}
		if exists {
			return ErrConflict
		}
		rows, e := tx.Query(tc, `SELECT id::text,project_id,owner,generation,host,requested_ref,ref,state,epoch,binding_uid,binding_generation FROM registry_pin WHERE operation=$1 ORDER BY requested_ref FOR UPDATE`, op.ID)
		if e != nil {
			return e
		}
		var baseID, destID string
		var baseEpoch, destEpoch int64
		for rows.Next() {
			var id, project, owner, generation, host, requested, ref, state, uid, binding string
			var epoch int64
			if e = rows.Scan(&id, &project, &owner, &generation, &host, &requested, &ref, &state, &epoch, &uid, &binding); e != nil {
				rows.Close()
				return e
			}
			if project != op.ProjectID || owner != op.Owner || generation != op.Generation || host != s.host || state != Pending || uid != "" || binding != "" || epoch < 1 || ref != requested || !refs.need[requested] {
				rows.Close()
				return ErrConflict
			}
			delete(refs.need, requested)
			if requested == spec.Destination {
				destID, destEpoch = id, epoch
			} else {
				baseID, baseEpoch = id, epoch
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(refs.need) != 0 || destID == "" {
			return ErrConflict
		}
		raw, _ := json.Marshal(spec)
		_, e = tx.Exec(tc, `INSERT INTO registry_snapshot_dispatch(operation,project_id,owner,generation,host,spec,base_pin_id,base_pin_epoch,destination_pin_id,destination_pin_epoch,state) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,'')::uuid,$8,$9,$10,'authorized')`, op.ID, op.ProjectID, op.Owner, op.Generation, s.host, raw, baseID, baseEpoch, destID, destEpoch)
		return conflict(e)
	})
	if err != nil {
		return nil, err
	}
	return &SnapshotProof{op: op, host: s.host, spec: spec, state: &proofState{}}, nil
}

type snapshotDispatchRecord struct {
	Spec                                                                 SnapshotSpec
	State, UID, Phase, BaseID, DestID, Digest, OwnerUID, OwnerGeneration string
	Epoch, BaseEpoch, DestEpoch                                          int64
}

func (s *Store) snapshotDispatch(ctx context.Context, tx pgx.Tx, op Operation) (*snapshotDispatchRecord, error) {
	d := &snapshotDispatchRecord{}
	var project, owner, generation, host, action string
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT project_id,owner,generation,host,action,spec,state,pod_uid,phase,coalesce(base_pin_id::text,''),base_pin_epoch,destination_pin_id::text,destination_pin_epoch,epoch,destination_digest,destination_owner_uid,destination_owner_generation FROM registry_snapshot_dispatch WHERE operation=$1 FOR UPDATE`, op.ID).Scan(&project, &owner, &generation, &host, &action, &raw, &d.State, &d.UID, &d.Phase, &d.BaseID, &d.BaseEpoch, &d.DestID, &d.DestEpoch, &d.Epoch, &d.Digest, &d.OwnerUID, &d.OwnerGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if project != op.ProjectID || owner != op.Owner || generation != op.Generation || host != s.host || action != snapshotAction || json.Unmarshal(raw, &d.Spec) != nil {
		return nil, ErrConflict
	}
	if _, err = s.snapshotReferences(op.ProjectID, d.Spec); err != nil {
		return nil, ErrConflict
	}
	return d, nil
}
func (s *Store) lockSnapshotOwner(ctx context.Context, op Operation) error {
	if e := s.lockDispatchOperation(ctx, op.ID); e != nil {
		return e
	}
	return s.lockProject(ctx, op.ProjectID)
}

// CancelUnusedSnapshotDispatch burns every proof copy BEFORE its DB CAS. If the
// database fails or the marker was observed, no proof remains and holds persist.
func (s *Store) CancelUnusedSnapshotDispatch(ctx context.Context, p *SnapshotProof) error {
	if p == nil || p.state == nil || !p.op.valid() || p.host != s.host || !p.state.used.CompareAndSwap(false, true) {
		return ErrConflict
	}
	return s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if e := s.lockSnapshotOwner(tc, p.op); e != nil {
			return e
		}
		d, e := s.snapshotDispatch(tc, tx, p.op)
		if e != nil {
			return e
		}
		if d == nil || d.Spec != p.spec || d.State != "authorized" || d.UID != "" {
			return ErrConflict
		}
		tag, e := tx.Exec(tc, `UPDATE registry_snapshot_dispatch SET state='known_no_effect',epoch=epoch+1,updated_at=clock_timestamp() WHERE operation=$1 AND epoch=$2 AND state='authorized' AND pod_uid=''`, p.op.ID, d.Epoch)
		if e == nil && tag.RowsAffected() != 1 {
			return ErrConflict
		}
		return e
	})
}

// SnapshotPodEvidence must come from an independently inspected ACTUAL assembly
// pod, its UID, fixed action/image/command/env and exact frozen envelope. Source
// stamps alone NEVER prove capture/provider truth or registry publication.
type SnapshotPodEvidence struct {
	UID, Operation, Nonce string
	Spec                  SnapshotSpec
	Phase                 string
}

func (SnapshotPodEvidence) String() string   { return "[snapshot evidence redacted]" }
func (SnapshotPodEvidence) GoString() string { return "[snapshot evidence redacted]" }
func (SnapshotPodEvidence) LogValue() slog.Value {
	return slog.StringValue("[snapshot evidence redacted]")
}
func (s *Store) validSnapshotPod(op Operation, e SnapshotPodEvidence) bool {
	_, err := s.snapshotReferences(op.ProjectID, e.Spec)
	return err == nil && op.valid() && identityRE.MatchString(e.UID) && e.Operation == op.ID && e.Nonce == op.Generation && (e.Phase == "Pending" || e.Phase == "Running" || runtimeTerminal(e.Phase))
}
func (s *Store) RecordSnapshotPod(ctx context.Context, op Operation, e SnapshotPodEvidence) error {
	if !s.validSnapshotPod(op, e) {
		return ErrConflict
	}
	return s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if err := s.lockSnapshotOwner(tc, op); err != nil {
			return err
		}
		d, err := s.snapshotDispatch(tc, tx, op)
		if err != nil {
			return err
		}
		if d == nil || d.Spec != e.Spec || d.State == "unknown" || d.State == "known_no_effect" || (d.UID != "" && d.UID != e.UID) {
			return ErrConflict
		}
		if d.State == "terminal" {
			if runtimeTerminal(e.Phase) && e.Phase != d.Phase {
				return ErrConflict
			}
			return nil
		}
		state := "observed"
		if runtimeTerminal(e.Phase) {
			state = "terminal"
		}
		tag, err := tx.Exec(tc, `UPDATE registry_snapshot_dispatch SET state=$1,pod_uid=$2,phase=$3,epoch=epoch+1,updated_at=clock_timestamp() WHERE operation=$4 AND epoch=$5`, state, e.UID, e.Phase, op.ID, d.Epoch)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrConflict
		}
		return err
	})
}

// SnapshotTerminalKnown only protects committed terminal evidence from stale
// error handlers. It grants no digest, release, replay or recovery permission.
func (s *Store) SnapshotTerminalKnown(ctx context.Context, op Operation) (bool, error) {
	if !op.valid() {
		return false, ErrInvalid
	}
	known := false
	err := s.transaction(ctx, func(tc context.Context, tx pgx.Tx) error {
		if err := s.lockSnapshotOwner(tc, op); err != nil {
			return err
		}
		d, err := s.snapshotDispatch(tc, tx, op)
		if err != nil {
			return err
		}
		known = d != nil && (d.State == "terminal" || d.State == "known_no_effect")
		return nil
	})
	return known, err
}
func (s *Store) snapshotPinEvidence(p Pin, e SnapshotPodEvidence) error {
	if !s.validSnapshotPod(p.Operation, e) || (p.RequestedRef != e.Spec.Base && p.RequestedRef != e.Spec.Destination) || (p.BindingUID != "" && (p.BindingUID != e.UID || p.BindingGeneration != e.Nonce)) {
		return ErrConflict
	}
	return nil
}

// ObserveSnapshotPod retains PENDING aliases/digests; it cannot bind a result.
func (s *Store) ObserveSnapshotPod(ctx context.Context, p Pin, e SnapshotPodEvidence) (Pin, error) {
	if p.State != Pending || p.Ref != p.RequestedRef || s.snapshotPinEvidence(p, e) != nil {
		return Pin{}, ErrConflict
	}
	r, err := s.reference(p.Ref)
	if err != nil {
		return Pin{}, err
	}
	return s.advancePin(ctx, p, Pending, p.Ref, r.Digest, e.UID, e.Nonce, pinEvidence{snapshotPod: &e})
}

// ReleaseSnapshotTerminal releases ONLY a previously observed managed BASE after
// exact positive assembly termination. Failed DEST may already have published:
// it stays held forever here, as does successful BOUND DEST. No retirement API.
// This never releases an old runtime consumer/source desired CR generation.
func (s *Store) ReleaseSnapshotTerminal(ctx context.Context, p Pin, e SnapshotPodEvidence) (Pin, error) {
	if p.State != Pending || p.BindingUID == "" || p.RequestedRef != e.Spec.Base || !runtimeTerminal(e.Phase) || s.snapshotPinEvidence(p, e) != nil {
		return Pin{}, ErrConflict
	}
	r, err := s.reference(p.Ref)
	if err != nil {
		return Pin{}, err
	}
	return s.advancePin(ctx, p, Released, p.Ref, r.Digest, p.BindingUID, p.BindingGeneration, pinEvidence{snapshotPod: &e})
}

// BindSnapshotDestination transfers only a separately observed, already COMMITTED
// durable publication binding. The future caller must prove its own template owner
// and registry output; this DB core only compares that supplied identity. Output
// digest may differ from BASE. A terminal phase never manufactures that digest.
func (s *Store) BindSnapshotDestination(ctx context.Context, p Pin, e SnapshotPodEvidence, b BindingEvidence) (Pin, error) {
	if p.State != Pending || p.BindingUID == "" || p.RequestedRef != e.Spec.Destination || e.Phase != "Succeeded" || s.snapshotPinEvidence(p, e) != nil || !identityRE.MatchString(b.UID) || b.Generation != p.Generation {
		return Pin{}, ErrConflict
	}
	r, err := s.reference(b.DigestRef)
	old, oldErr := s.reference(p.RequestedRef)
	if err != nil || oldErr != nil || r.Digest == "" || r.Tag != "" || r.String() != b.DigestRef || r.Repo != old.Repo {
		return Pin{}, ErrConflict
	}
	return s.advancePin(ctx, p, Bound, b.DigestRef, r.Digest, b.UID, b.Generation, pinEvidence{snapshotPod: &e, snapshotBinding: &b})
}

// Always composed, even when every other action marker is absent. Marker role
// epoch advancement and the full original pin CAS share one rollback boundary.
func (s *Store) guardSnapshotPin(ctx context.Context, tx pgx.Tx, p Pin, state, ref, uid, generation string, e *SnapshotPodEvidence, b *BindingEvidence) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM registry_snapshot_dispatch WHERE operation=$1)`, p.Operation.ID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if e != nil || b != nil {
			return ErrConflict
		}
		return nil
	}
	if err := s.lockSnapshotOwner(ctx, p.Operation); err != nil {
		return err
	}
	d, err := s.snapshotDispatch(ctx, tx, p.Operation)
	if err != nil {
		return err
	}
	if d == nil {
		return ErrConflict
	}
	base := p.ID == d.BaseID && d.BaseID != ""
	wantID, wantEpoch, wantRef := d.DestID, d.DestEpoch, d.Spec.Destination
	if base {
		wantID, wantEpoch, wantRef = d.BaseID, d.BaseEpoch, d.Spec.Base
	}
	expectedRef := wantRef
	if !base && p.State == Bound {
		expectedRef = d.Digest
	}
	if p.ID != wantID || p.Epoch != wantEpoch || p.RequestedRef != wantRef || p.Ref != expectedRef {
		return ErrConflict
	}
	if e != nil && (s.snapshotPinEvidence(p, *e) != nil || e.Spec != d.Spec || e.UID != d.UID || e.Phase != d.Phase) {
		return ErrConflict
	}
	if d.State == "unknown" {
		return ErrConflict
	}
	switch state {
	case Unknown:
		if e != nil || b != nil || d.State == "terminal" || d.State == "known_no_effect" {
			return ErrConflict
		}
	case Pending:
		if e == nil || b != nil || p.State != Pending || ref != p.Ref || uid != d.UID || generation != p.Generation || (d.State != "observed" && d.State != "terminal") {
			return ErrConflict
		}
	case Released:
		if d.State == "known_no_effect" && p.State == Pending && p.BindingUID == "" && e == nil && b == nil && uid == "" && generation == "" && ref == p.Ref {
			break
		}
		if !base || e == nil || b != nil || d.State != "terminal" || !runtimeTerminal(e.Phase) || p.State != Pending || p.BindingUID != d.UID || p.BindingGeneration != p.Generation || ref != p.Ref || uid != d.UID || generation != p.Generation {
			return ErrConflict
		}
	case Bound:
		if base || e == nil || b == nil || p.State != Pending || d.State != "terminal" || d.Phase != "Succeeded" || p.BindingUID != d.UID || p.BindingGeneration != p.Generation || uid != b.UID || generation != b.Generation || ref != b.DigestRef || d.Digest != "" {
			return ErrConflict
		}
	default:
		return ErrConflict
	}
	column := "destination_pin_epoch"
	if base {
		column = "base_pin_epoch"
	}
	nextState := d.State
	if state == Unknown {
		nextState = "unknown"
	}
	digest, owner, ownerGen := d.Digest, d.OwnerUID, d.OwnerGeneration
	if state == Bound {
		digest, owner, ownerGen = ref, uid, generation
	}
	tag, err := tx.Exec(ctx, `UPDATE registry_snapshot_dispatch SET `+column+`=`+column+`+1,state=$1,destination_digest=$2,destination_owner_uid=$3,destination_owner_generation=$4,epoch=epoch+1,updated_at=clock_timestamp() WHERE operation=$5 AND epoch=$6`, nextState, digest, owner, ownerGen, p.Operation.ID, d.Epoch)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}
