package project

// BUILD/template participation only. Other image writers and legacy adoption
// are not covered; both physical registry-delete paths stay on unconditional HOLD.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/registrypins"
)

const (
	buildPreparing        = "preparing"
	buildPinned           = "pinned"
	buildDispatching      = "dispatching"
	buildPinUnknown       = "unknown"
	buildPinFinished      = "finished"
	buildTerminalObserved = "terminal_observed"
	buildPinFailed        = "pre_dispatch_failed"
)

var errBuildPinState = errors.New("build operation already claimed or requires operator recovery")

type buildRegistryOperation struct {
	Operation                              registrypins.Operation `json:"operation"`
	State                                  string                 `json:"state"`
	Base, Destination, ParentID, Candidate string
	Pins                                   []registrypins.Pin `json:"pins,omitempty"`
	PodUID                                 string             `json:"podUID,omitempty"`
	TerminalPhase                          string             `json:"terminalPhase,omitempty"`
	// No request steps, credentials, env, CR payload, or presigned URLs are stored.
}

// SetSandboxBuildPins enables only BUILD/template participation on this PRIMARY.
// Call EnsureSchema before serving. There is no delete-enabling coverage flag.
func (h *Handler) SetSandboxBuildPins(db *pgdoc.DB) { h.sbBuildPinsDB = db }
func (h *Handler) buildPinStore(cfg sandbox.Config) (*registrypins.Store, error) {
	if h.sbBuildPinsDB == nil {
		return nil, nil
	} // explicit test/legacy handler composition
	parts := strings.Split(strings.TrimRight(cfg.TemplateRegistry, "/"), "/")
	if len(parts) != 2 || parts[1] != "t" {
		return nil, registrypins.ErrInvalid
	}
	return registrypins.New(h.sbBuildPinsDB, parts[0])
}
func parentFingerprint(base *buildBase) string {
	var parent any
	if p := base.Parent; p != nil {
		parent = []any{p.BuildID, p.Status, p.Image, p.StartCmd, p.ReadyCmd, p.User, p.Workdir, p.StartAsRoot}
	}
	b, _ := json.Marshal([]any{base.Ref, base.ParentTemplateID, parent})
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func pinOwner(t *sandboxTemplateDoc, buildID string) string { return "build:" + t.ID + ":" + buildID }

// prepareBuild freezes a unique nonce before BeginPins. It does not resolve a
// registry alias or dispatch anything. Its caller owns no inherited SQL tx.
func (h *Handler) prepareBuild(ctx context.Context, t *sandboxTemplateDoc, b *templateBuild, base *buildBase, cfg sandbox.Config) (*buildRegistryOperation, error) {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return nil, err
	}
	op := &buildRegistryOperation{Operation: registrypins.Operation{ID: uuid.NewString(), ProjectID: t.ProjectID, Owner: pinOwner(t, b.BuildID), Generation: uuid.NewString()}, State: buildPreparing, Base: base.Ref, Destination: templateImage(cfg, t, b.BuildID), ParentID: base.ParentTemplateID, Candidate: parentFingerprint(base)}
	err := h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
		var cur sandboxTemplateDoc
		found, err := h.sbTemplates.GetForUpdate(tc, t.ID, &cur)
		if err != nil {
			return err
		}
		if !found || cur.ProjectID != t.ProjectID || cur.Deleting {
			return errBuildPinState
		}
		cb := cur.build(b.BuildID)
		if cb == nil || cb.Status != sandbox.BuildWaiting || cb.Registry != nil {
			return errBuildPinState
		}
		cb.Registry = op
		_, err = h.sbTemplates.Replace(tc, t.ID, &cur)
		return err
	})
	if err != nil {
		return nil, err
	}
	b.Registry = op
	return op, nil
}
func (h *Handler) buildRegistryWrite(ctx context.Context, t *sandboxTemplateDoc, buildID string, op registrypins.Operation, expect string, apply func(*sandboxTemplateDoc, *templateBuild) error) error {
	return h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
		var cur sandboxTemplateDoc
		found, err := h.sbTemplates.GetForUpdate(tc, t.ID, &cur)
		if err != nil {
			return err
		}
		if !found || cur.ProjectID != t.ProjectID {
			return errBuildPinState
		}
		cb := cur.build(buildID)
		if cb == nil || cb.Registry == nil || cb.Registry.Operation != op || cb.Registry.State != expect {
			return errBuildPinState
		}
		if err = apply(&cur, cb); err != nil {
			return err
		}
		_, err = h.sbTemplates.Replace(tc, t.ID, &cur)
		return err
	})
}

// acquireBuildPins commits BASE+DEST first, then locks namespace -> parent/owned
// template rows and reselects the same parent generation before attaching receipts.
func (h *Handler) acquireBuildPins(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc, b *templateBuild, base *buildBase, fromImage, fromTemplate string, raw json.RawMessage) (*registrypins.Store, error) {
	store, err := h.buildPinStore(c.ss.Config())
	if err != nil || store == nil {
		return store, err
	}
	op, err := h.prepareBuild(ctx, t, b, base, c.ss.Config())
	if err != nil {
		return nil, err
	}
	refs := []string{op.Destination}
	host, _, _ := strings.Cut(c.ss.Config().TemplateRegistry, "/")
	if r, e := registrypins.ParseRef(host, base.Ref); e == nil {
		if r.Project != t.ProjectID {
			return nil, registrypins.ErrScope
		}
		refs = append(refs, r.String())
	} else if strings.HasPrefix(base.Ref, host+"/t/") {
		return nil, registrypins.ErrScope
	}
	pins, err := store.BeginPins(ctx, op.Operation, refs)
	if err != nil {
		// A lost commit response is not permission to reacquire or dispatch. The
		// exact-operation lookup may recover receipts, but this request stops.
		_, _ = store.PinsForOperation(context.WithoutCancel(ctx), op.Operation)
		h.markBuildPinUnknown(context.WithoutCancel(ctx), store, t, b, op)
		return nil, err
	}
	op.Pins = pins
	err = h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
		tx, e := h.sbTemplates.DB().Tx(tc)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(tc, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); e != nil {
			return e
		}
		canonical, e := registrypins.CanonicalHost(host)
		if e != nil {
			return e
		}
		if e = h.sbTemplates.DB().LockXact(tc, "registry-ref:"+canonical); e != nil {
			return e
		}
		ids := []string{t.ID}
		if op.ParentID != "" && op.ParentID != t.ID {
			ids = append(ids, op.ParentID)
		}
		sort.Strings(ids)
		for _, id := range ids {
			var row sandboxTemplateDoc
			found, e := h.sbTemplates.GetForUpdate(tc, id, &row)
			if e != nil {
				return e
			}
			if !found || row.ProjectID != t.ProjectID {
				return errBuildPinState
			}
		}
		fresh, e := h.resolveBase(tc, c, fromImage, fromTemplate, raw)
		if e != nil {
			return e
		}
		if parentFingerprint(fresh) != op.Candidate {
			return errors.New("build parent changed before pin attachment")
		}
		return h.buildRegistryWrite(tc, t, b.BuildID, op.Operation, buildPreparing, func(_ *sandboxTemplateDoc, cb *templateBuild) error {
			cb.Registry.Pins = pins
			cb.Registry.State = buildPinned
			return nil
		})
	})
	if err != nil {
		h.failBuildBeforeDispatch(context.WithoutCancel(ctx), store, t, b, op, err)
		return nil, err
	}
	op.State = buildPinned
	b.Registry = op
	return store, nil
}
func (h *Handler) failBuildBeforeDispatch(ctx context.Context, store *registrypins.Store, t *sandboxTemplateDoc, b *templateBuild, op *buildRegistryOperation, cause error) {
	// Persist the non-dispatch decision BEFORE releasing exact pending receipts.
	if err := h.buildRegistryWrite(ctx, t, b.BuildID, op.Operation, op.State, func(_ *sandboxTemplateDoc, cb *templateBuild) error {
		cb.Registry.State = buildPinFailed
		cb.Registry.Pins = op.Pins
		cb.Status = sandbox.BuildError
		cb.Reason = "build refused before dispatch"
		return nil
	}); err != nil {
		return
	}
	for _, p := range op.Pins {
		if p.State == registrypins.Pending && p.BindingUID == "" {
			_, _ = store.ReleaseKnownNoEffect(ctx, p)
		}
	}
	_ = cause // never persist user/credential-bearing error payloads
}
func (h *Handler) markBuildPinUnknown(ctx context.Context, store *registrypins.Store, t *sandboxTemplateDoc, b *templateBuild, op *buildRegistryOperation) {
	if known, e := store.BuildTerminalKnown(ctx, op.Operation); e != nil || known {
		return
	}
	pins, err := store.PinsForOperation(ctx, op.Operation)
	if err == nil {
		op.Pins = pins
	}
	if op.State == buildTerminalObserved || op.State == buildPinFinished {
		return
	}
	err = h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
		host, _, _ := strings.Cut(op.Destination, "/")
		if e := h.sbTemplates.DB().LockXact(tc, "registry-ref:"+host); e != nil {
			return e
		}
		tx, e := h.sbTemplates.DB().Tx(tc)
		if e != nil {
			return e
		}
		var terminal bool
		if e = tx.QueryRow(tc, `SELECT EXISTS(SELECT 1 FROM registry_build_dispatch WHERE operation=$1 AND project_id=$2 AND owner=$3 AND generation=$4 AND host=$5 AND state IN ('terminal','known_no_effect'))`, op.Operation.ID, op.Operation.ProjectID, op.Operation.Owner, op.Operation.Generation, host).Scan(&terminal); e != nil {
			return e
		}
		if terminal {
			return errBuildPinState
		}
		return h.buildRegistryWrite(tc, t, b.BuildID, op.Operation, op.State, func(_ *sandboxTemplateDoc, cb *templateBuild) error {
			cb.Registry.State = buildPinUnknown
			cb.Registry.Pins = op.Pins
			cb.Status = sandbox.BuildError
			cb.Reason = "registry reference outcome unknown; operator recovery required"
			return nil
		})
	})
	if err != nil {
		return
	} // failed owner CAS authorizes NO pin mutation
	for _, p := range op.Pins {
		if p.State == registrypins.Pending || p.State == registrypins.Bound {
			_, _ = store.MarkUnknown(ctx, p)
		}
	}
	op.State = buildPinUnknown
}
func (h *Handler) authorizeBuildDispatch(ctx context.Context, store *registrypins.Store, t *sandboxTemplateDoc, b *templateBuild) (*registrypins.BuildProof, error) {
	op := b.Registry
	if op == nil {
		return nil, errBuildPinState
	}
	proof, err := store.AuthorizeBuildDispatch(ctx, op.Operation, op.Base, op.Destination)
	if err != nil {
		return nil, err
	}
	err = h.buildRegistryWrite(ctx, t, b.BuildID, op.Operation, buildPinned, func(_ *sandboxTemplateDoc, cb *templateBuild) error {
		cb.Registry.State = buildDispatching
		cb.Status = sandbox.BuildBuilding
		cb.From = b.From
		cb.StartCmd = b.StartCmd
		cb.ReadyCmd = b.ReadyCmd
		cb.User = b.User
		cb.Workdir = b.Workdir
		cb.StartAsRoot = b.StartAsRoot
		return nil
	})
	if err != nil {
		return nil, err
	}
	op.State = buildDispatching
	return proof, nil
}

func (h *Handler) observeBuildPins(ctx context.Context, store *registrypins.Store, t *sandboxTemplateDoc, b *templateBuild, st *sandbox.BuildState) error {
	op := b.Registry
	if op == nil || op.State != buildDispatching {
		return errBuildPinState
	}
	if st == nil || st.Pod.UID == "" || st.Pod.Operation != op.Operation.ID || st.Pod.Nonce != op.Operation.Generation || st.Pod.BaseRef != op.Base || st.Pod.DestinationRef != op.Destination {
		return errBuildPinState
	}
	if op.PodUID != "" && op.PodUID != st.Pod.UID {
		return errBuildPinState
	}
	ps, err := store.PinsForOperation(ctx, op.Operation)
	if err != nil {
		return err
	}
	if err = store.RecordBuildPod(ctx, op.Operation, st.Pod); err != nil {
		return err
	}
	for i, p := range ps {
		if p.State != registrypins.Pending {
			return errBuildPinState
		}
		if p.BindingUID == "" {
			ps[i], err = store.ObserveBuildPod(ctx, p, st.Pod)
			if errors.Is(err, registrypins.ErrConflict) {
				latest, e := store.PinsForOperation(ctx, op.Operation)
				if e != nil {
					return e
				}
				for _, cp := range latest {
					if cp.ID == p.ID && cp.RequestedRef == p.RequestedRef && cp.Ref == p.Ref && cp.Operation == p.Operation && cp.State == registrypins.Pending && cp.BindingUID == st.Pod.UID && cp.BindingGeneration == st.Pod.Nonce {
						ps[i], err = cp, nil
						break
					}
				}
			}
			if err != nil {
				return err
			}
		} else if p.BindingUID != st.Pod.UID || p.BindingGeneration != st.Pod.Nonce {
			return errBuildPinState
		}
	}
	if err = h.buildRegistryWrite(ctx, t, b.BuildID, op.Operation, buildDispatching, func(_ *sandboxTemplateDoc, cb *templateBuild) error {
		cb.Registry.PodUID = st.Pod.UID
		cb.Registry.Pins = ps
		return nil
	}); err != nil {
		return err
	}
	op.PodUID, op.Pins = st.Pod.UID, ps
	return nil
}

// finishPinnedBuild publishes a terminal owner generation before converting DEST
// to a durable template binding. BASE is released only for the exact observed pod.
func (h *Handler) finishPinnedBuild(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc, b *templateBuild, st *sandbox.BuildState, logs []string) error {
	store, err := h.buildPinStore(c.ss.Config())
	if err != nil || store == nil {
		return errBuildPinState
	}
	fresh, err := h.loadTemplate(ctx, t.ProjectID, t.ID)
	if err != nil || fresh == nil {
		return errBuildPinState
	}
	current := fresh.build(b.BuildID)
	if current == nil || current.Registry == nil || current.Registry.Operation != b.Registry.Operation {
		return errBuildPinState
	}
	*b = *current
	if b.Registry.State == buildPinFinished {
		adoptBuild(t, b, fresh)
		return nil
	}
	if b.Registry.State == buildDispatching {
		if err = h.observeBuildPins(ctx, store, t, b, st); err != nil {
			return err
		}
	} else if b.Registry.State != buildTerminalObserved {
		return errBuildPinState
	}
	if st.Pod.Phase != "Succeeded" && st.Pod.Phase != "Failed" {
		return nil
	}
	op := b.Registry
	if op.State == buildDispatching {
		err = h.buildRegistryWrite(context.WithoutCancel(ctx), t, b.BuildID, op.Operation, buildDispatching, func(ft *sandboxTemplateDoc, fb *templateBuild) error {
			fb.Logs = logs
			fb.Status, fb.Reason, fb.Step = st.Status, st.Reason, st.Step
			if st.Status == sandbox.BuildReady {
				if !digestRe.MatchString(st.Digest) {
					return errBuildPinState
				}
				fb.Image = strings.TrimRight(c.ss.Config().TemplateRegistry, "/") + "/" + t.ProjectID + "/" + t.ID + "@" + st.Digest
				if ft.Tags == nil {
					ft.Tags = map[string]string{}
				}
				for _, tag := range fb.Tags {
					ft.Tags[tag] = fb.BuildID
				}
			}
			now := time.Now().UTC()
			fb.FinishedAt = &now
			fb.Registry.TerminalPhase = st.Pod.Phase
			fb.Registry.State = buildTerminalObserved
			return nil
		})
		if err != nil && !errors.Is(err, errBuildPinState) {
			return err
		}
	}
	// Re-observe PRIMARY owner data, not our request copy, before handoff.
	fresh, err = h.loadTemplate(ctx, t.ProjectID, t.ID)
	if err != nil || fresh == nil {
		return errBuildPinState
	}
	fb := fresh.build(b.BuildID)
	if fb == nil || fb.Registry == nil || fb.Registry.Operation != op.Operation || fb.Registry.PodUID != st.Pod.UID || fb.Registry.TerminalPhase != st.Pod.Phase || (fb.Registry.State != buildTerminalObserved && fb.Registry.State != buildPinFinished) {
		return errBuildPinState
	}
	if fb.Registry.State == buildPinFinished {
		adoptBuild(t, b, fresh)
		return nil
	}
	ps, err := store.PinsForOperation(ctx, op.Operation)
	if err != nil {
		return err
	}
	for i, p := range ps {
		if p.Operation != op.Operation || (p.RequestedRef != op.Base && p.RequestedRef != op.Destination) {
			return errBuildPinState
		}
		if p.State == registrypins.Bound {
			if p.RequestedRef != op.Destination || fb.Status != sandbox.BuildReady || p.Ref != fb.Image || p.BindingUID != "template:"+t.ID+":"+b.BuildID || p.BindingGeneration != op.Operation.Generation {
				return errBuildPinState
			}
			continue
		}
		if p.State == registrypins.Released {
			if (p.RequestedRef == op.Destination && fb.Status == sandbox.BuildReady) || p.BindingUID != st.Pod.UID || p.BindingGeneration != op.Operation.Generation || p.Ref != p.RequestedRef {
				return errBuildPinState
			}
			continue
		}
		if p.State != registrypins.Pending {
			return errBuildPinState
		}
		if p.RequestedRef == op.Destination && fb.Status == sandbox.BuildReady {
			ps[i], err = store.ObserveBinding(ctx, p, registrypins.BindingEvidence{UID: "template:" + t.ID + ":" + b.BuildID, Generation: fb.Registry.Operation.Generation, DigestRef: fb.Image})
		} else {
			ps[i], err = store.ReleaseBuildTerminal(ctx, p, st.Pod)
		}
		if err != nil {
			return err
		}
	}
	err = h.buildRegistryWrite(ctx, t, b.BuildID, op.Operation, buildTerminalObserved, func(_ *sandboxTemplateDoc, cb *templateBuild) error {
		cb.Registry.Pins = ps
		cb.Registry.State = buildPinFinished
		return nil
	})
	if err == nil {
		ft, _ := h.loadTemplate(ctx, t.ProjectID, t.ID)
		adoptBuild(t, b, ft)
	}
	return err
}

// recoverPinnedBuild never redispatches. PREPARING/PINNED receipts can be read
// after a lost attach return, but only an explicitly observed dispatch proceeds.
func (h *Handler) recoverPinnedBuild(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc, b *templateBuild) {
	if b.Registry == nil {
		return
	}
	store, err := h.buildPinStore(c.ss.Config())
	if err != nil || store == nil {
		return
	}
	if b.Registry.State == buildPreparing || b.Registry.State == buildPinned {
		pins, e := store.PinsForOperation(ctx, b.Registry.Operation)
		if e == nil {
			b.Registry.Pins = pins
		}
		// Do not race the live preparer's state or dispatch; mere age is not proof.
		return
	}
	if b.Registry.State == buildTerminalObserved {
		// The exact terminal observation was committed before handoff. Its pod
		// may already be gone; replay only DB handoff, never external dispatch.
		op := b.Registry
		st := &sandbox.BuildState{Status: b.Status, Pod: registrypins.BuildPodEvidence{UID: op.PodUID, Operation: op.Operation.ID, Nonce: op.Operation.Generation, BaseRef: op.Base, DestinationRef: op.Destination, Phase: op.TerminalPhase}}
		_ = h.finishPinnedBuild(ctx, c, t, b, st, b.Logs)
		return
	}
	if b.Registry.State != buildDispatching {
		return
	}
	st, err := c.ss.BuildPodState(ctx, t.ProjectID, b.BuildID)
	if err != nil || st == nil {
		h.markBuildPinUnknown(context.WithoutCancel(ctx), store, t, b, b.Registry)
		return
	}
	if st.Pod.Phase == "Succeeded" || st.Pod.Phase == "Failed" {
		_ = h.finishPinnedBuild(ctx, c, t, b, st, c.ss.BuildLogs(ctx, b.BuildID, maxStoredLogLines))
		return
	}
	if err = h.observeBuildPins(ctx, store, t, b, st); err != nil {
		h.markBuildPinUnknown(context.WithoutCancel(ctx), store, t, b, b.Registry)
	}
}

// cleanupRetiredPins keeps removed build generations as tombstones until every
// exact binding release committed. It never releases active/UNKNOWN operations.
func (h *Handler) cleanupRetiredPins(ctx context.Context, ss *sandbox.Service, t *sandboxTemplateDoc) error {
	store, err := h.buildPinStore(ss.Config())
	if err != nil || store == nil {
		return err
	}
	var holds []error
	for _, old := range t.RetiredBuilds {
		// One parent held by a later retired child must not starve independent
		// cleanup. Preserve its tombstone, but attempt every other generation.
		e := func() error {
			var current sandboxTemplateDoc
			found, e := h.sbTemplates.Get(ctx, t.ID, &current)
			if e != nil {
				return e
			}
			if !found || current.ProjectID != t.ProjectID || current.build(old.BuildID) != nil {
				return errBuildPinState
			}
			if old.Registry == nil {
				return nil
			}
			op := old.Registry
			if op.State != buildPinFinished && op.State != buildPinFailed {
				return errBuildPinState
			}
			ps, e := store.PinsForOperation(ctx, op.Operation)
			if e != nil {
				return e
			}
			for _, p := range ps {
				if p.State == registrypins.Released {
					continue
				}
				if p.State == registrypins.Bound && p.RequestedRef == op.Destination && p.Ref == old.Image && p.BindingUID == "template:"+t.ID+":"+old.BuildID && p.BindingGeneration == op.Operation.Generation {
					if held, e := h.retainedBuildReference(ctx, ss.Config(), t.ID, old.BuildID, p.Ref); e != nil {
						return e
					} else if held {
						return errBuildPinState
					}
					_, e = store.ReleaseBinding(ctx, p, registrypins.BindingEvidence{UID: p.BindingUID, Generation: op.Operation.Generation, DigestRef: old.Image})
				} else if p.State == registrypins.Pending && op.State == buildPinFailed {
					_, e = store.ReleaseKnownNoEffect(ctx, p)
				} else {
					return errBuildPinState
				}
				if e != nil {
					return e
				}
			}
			if e = h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
				var current sandboxTemplateDoc
				found, e := h.sbTemplates.GetForUpdate(tc, t.ID, &current)
				if e != nil {
					return e
				}
				if !found || current.ProjectID != t.ProjectID {
					return errBuildPinState
				}
				// A generation present in live Builds must never be released by a stale tombstone.
				if current.build(old.BuildID) != nil {
					return errBuildPinState
				}
				out := current.RetiredBuilds[:0]
				for _, rb := range current.RetiredBuilds {
					if rb.Registry == nil || rb.Registry.Operation != op.Operation {
						out = append(out, rb)
					}
				}
				current.RetiredBuilds = out
				_, e = h.sbTemplates.Replace(tc, t.ID, &current)
				return e
			}); e != nil {
				return e
			}
			return nil
		}()
		if e != nil {
			holds = append(holds, e)
		}
	}
	return errors.Join(holds...)
}

// retainedBuildReference is a conservative CURRENT-DB cleanup HOLD, not a
// race-free provenance pin transfer. Other writers/adoption remain incomplete
// and both physical manifest-delete gates stay unconditional HOLD.
func (h *Handler) retainedBuildReference(ctx context.Context, cfg sandbox.Config, templateID, buildID, digestRef string) (bool, error) {
	host, _, _ := strings.Cut(cfg.TemplateRegistry, "/")
	target, err := registrypins.ParseRef(host, digestRef)
	if err != nil {
		return true, err
	}
	var all []sandboxTemplateDoc
	if err = h.sbTemplates.Find(ctx, pgdoc.M{}, &all); err != nil {
		return true, err
	}
	for _, t := range all {
		for _, list := range [][]templateBuild{t.Builds, t.RetiredBuilds} {
			for _, b := range list {
				if t.ID == templateID && b.BuildID == buildID {
					continue
				}
				for _, ref := range []string{b.From, b.Image} {
					if ref == "" {
						continue
					}
					r, e := registrypins.ParseRef(host, ref)
					if e == nil && r.Repo == target.Repo && (r.Tag != "" || r.Digest == target.Digest) {
						return true, nil
					}
					if e != nil && strings.HasPrefix(ref, target.Host+"/"+target.Repo) {
						return true, nil
					}
				}
			}
		}
	}
	return false, nil
}

func (h *Handler) retireTemplatePins(ctx context.Context, ss *sandbox.Service, t *sandboxTemplateDoc) error {
	if h.sbBuildPinsDB == nil {
		return nil
	}
	if err := h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
		var cur sandboxTemplateDoc
		found, e := h.sbTemplates.GetForUpdate(tc, t.ID, &cur)
		if e != nil {
			return e
		}
		if !found || cur.ProjectID != t.ProjectID {
			return errBuildPinState
		}
		for _, b := range cur.Builds {
			if buildPinActive(b) {
				return errBuildPinState
			}
		}
		cur.Deleting = true
		for _, b := range cur.Builds {
			cur.rememberRetiredBuild(b)
			if b.Registry != nil {
				cur.RetiredBuilds = append(cur.RetiredBuilds, b)
			}
		}
		cur.Builds = nil
		_, e = h.sbTemplates.Replace(tc, t.ID, &cur)
		if e == nil {
			*t = cur
		}
		return e
	}); err != nil {
		return err
	}
	return h.cleanupRetiredPins(ctx, ss, t)
}

// Runtime-ready and template lifecycle use only these identities, never a pin
// receipt's mere existence as proof that an external operation completed.
func buildPinActive(b templateBuild) bool {
	return b.Registry != nil && b.Registry.State != buildPinFinished && b.Registry.State != buildPinFailed
}
func buildPinSDKStatus(b *templateBuild) string {
	if b.Registry != nil {
		switch b.Registry.State {
		case buildPreparing, buildPinned:
			return sandbox.BuildWaiting
		case buildDispatching, buildTerminalObserved:
			return sandbox.BuildBuilding
		case buildPinUnknown, buildPinFailed:
			return sandbox.BuildError
		}
	}
	return b.Status
}
