package project

// Import participation is a new, partial writer integration, not registry-wide
// coverage. Both physical manifest-delete paths remain unconditional HOLD.
import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/registrypins"
	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

type importRegistryOperation struct {
	Operation registrypins.Operation          `json:"operation"`
	State     string                          `json:"state"`
	Pins      []registrypins.Pin              `json:"pins,omitempty"`
	Pod       *registrypins.ImportPodEvidence `json:"pod,omitempty"`
}

func importPinActive(b templateBuild) bool {
	return b.Import != nil && b.Import.Registry != nil && b.Import.Registry.State != buildPinFinished && b.Import.Registry.State != buildPinFailed
}

func (h *Handler) importOperationWrite(ctx context.Context, t *sandboxTemplateDoc, b *templateBuild, expect string, apply func(*sandboxTemplateDoc, *templateBuild) error) error {
	op := b.Import.Registry.Operation
	return h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
		var cur sandboxTemplateDoc
		found, err := h.sbTemplates.GetForUpdate(tc, t.ID, &cur)
		if err != nil {
			return err
		}
		if !found || cur.ProjectID != t.ProjectID || cur.Deleting {
			return errBuildPinState
		}
		cb := cur.build(b.BuildID)
		if cb == nil || cb.Import == nil || cb.Import.Registry == nil || cb.Import.Registry.Operation != op || cb.Import.Registry.State != expect {
			return errBuildPinState
		}
		if err := apply(&cur, cb); err != nil {
			return err
		}
		_, err = h.sbTemplates.Replace(tc, cur.ID, &cur)
		if err == nil {
			adoptBuild(t, b, &cur)
		}
		return err
	})
}

// acquireImportDispatch runs outside every inherited project/rotation transaction.
// The owner nonce was reserved before this call; BeginPins commits before any
// source resolution or worker use, then owner attachment and authorization CAS.
func (h *Handler) acquireImportDispatch(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc, b *templateBuild) (*registrypins.Store, *registrypins.ImportProof, error) {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return nil, nil, err
	}
	store, err := h.buildPinStore(c.ss.Config())
	if err != nil {
		return nil, nil, err
	}
	if store == nil {
		return nil, nil, errBuildPinState
	}
	op := b.Import.Registry
	host, _, _ := strings.Cut(c.ss.Config().TemplateRegistry, "/")
	refs := []string{b.Import.Destination}
	if ref, err := registrypins.ParseRef(host, b.Import.Source); err == nil {
		if ref.Project != t.ProjectID {
			return nil, nil, registrypins.ErrScope
		}
		refs = []string{ref.String()} // managed metadata-only verification
	}
	pins, err := store.BeginPins(ctx, op.Operation, refs)
	if err != nil {
		h.markImportUnknown(ctx, store, t, b)
		return nil, nil, err
	}
	err = h.importOperationWrite(ctx, t, b, buildPreparing, func(_ *sandboxTemplateDoc, cb *templateBuild) error {
		cb.Import.Registry.Pins, cb.Import.Registry.State = pins, buildPinned
		return nil
	})
	if err != nil {
		h.markImportUnknown(ctx, store, t, b)
		return nil, nil, err
	}
	proof, err := store.AuthorizeImportDispatch(ctx, op.Operation, b.Import.Source, b.Import.Destination)
	if err != nil {
		h.markImportUnknown(ctx, store, t, b)
		return nil, nil, err
	}
	err = h.importOperationWrite(ctx, t, b, buildPinned, func(_ *sandboxTemplateDoc, cb *templateBuild) error {
		cb.Import.Registry.State, cb.Status = buildDispatching, sandbox.BuildBuilding
		now := time.Now().UTC()
		cb.LaunchStartedAt = &now
		return nil
	})
	if err != nil {
		// The owner write could have committed. Invalidating the exact unused
		// proof is the only pre-dispatch compensation, never a new claim.
		h.compensateImportLaunch(ctx, store, proof, t, b)
		return nil, nil, err
	}
	return store, proof, nil
}

func (h *Handler) markImportUnknown(ctx context.Context, store *registrypins.Store, t *sandboxTemplateDoc, b *templateBuild) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildCompensationTimeout)
	defer cancel()
	op := b.Import.Registry
	if op == nil || op.State == buildPinFinished || op.State == buildTerminalObserved {
		return
	}
	known, err := store.ImportTerminalKnown(cleanup, op.Operation)
	if err != nil || known {
		return
	}
	pins, err := store.PinsForOperation(cleanup, op.Operation)
	if err != nil {
		return
	}
	err = h.importOperationWrite(cleanup, t, b, op.State, func(_ *sandboxTemplateDoc, cb *templateBuild) error {
		cb.Import.Registry.State, cb.Import.Registry.Pins = buildPinUnknown, pins
		cb.Status, cb.Reason = sandbox.BuildError, "import outcome unknown; operator recovery required"
		return nil
	})
	if err != nil {
		return
	}
	for _, p := range pins {
		if p.State == registrypins.Pending || p.State == registrypins.Bound {
			_, _ = store.MarkUnknown(cleanup, p)
		}
	}
}

func (h *Handler) compensateImportLaunch(ctx context.Context, store *registrypins.Store, proof *registrypins.ImportProof, t *sandboxTemplateDoc, b *templateBuild) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildCompensationTimeout)
	defer cancel()
	op := b.Import.Registry
	if err := store.CancelUnusedImportDispatch(cleanup, proof, op.Operation); err != nil {
		h.markImportUnknown(cleanup, store, t, b)
		return
	}
	if err := h.importOperationWrite(cleanup, t, b, op.State, func(_ *sandboxTemplateDoc, cb *templateBuild) error {
		cb.Import.Registry.State, cb.Status, cb.Reason = buildPinFailed, sandbox.BuildError, "import refused before worker dispatch"
		now := time.Now().UTC()
		cb.FinishedAt = &now
		return nil
	}); err != nil {
		return
	}
	pins, err := store.PinsForOperation(cleanup, op.Operation)
	if err != nil {
		return
	}
	for _, p := range pins {
		if p.State == registrypins.Pending && p.BindingUID == "" {
			_, _ = store.ReleaseKnownNoEffect(cleanup, p)
		}
	}
}

// finishImportPins publishes usable success only after positive exact worker
// terminal evidence and the immutable output identity are durable. UNKNOWN is
// deliberately not reopened, even if a pod appears later.
func (h *Handler) finishImportPins(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc, b *templateBuild, status string, result *sandboximport.Result, reason string) error {
	store, err := h.buildPinStore(c.ss.Config())
	if err != nil || store == nil {
		return errBuildPinState
	}
	op := b.Import.Registry
	if op == nil {
		return errBuildPinState
	}
	if op.State == buildPinFinished {
		return nil
	}
	if op.State != buildDispatching && op.State != buildTerminalObserved {
		return errBuildPinState
	}
	var evidence registrypins.ImportPodEvidence
	if op.State == buildDispatching {
		state, err := c.ss.ImportPodState(ctx, t.ProjectID, b.BuildID)
		if err != nil || state == nil {
			return errBuildPinState
		}
		evidence = state.Pod
		if err != nil {
			return err
		}
		if evidence.UID == "" || evidence.Operation != op.Operation.ID || evidence.Nonce != op.Operation.Generation || evidence.Source != b.Import.Source || evidence.Destination != b.Import.Destination || (evidence.Phase != "Succeeded" && evidence.Phase != "Failed") {
			return errBuildPinState
		}
		if err := store.RecordImportPod(ctx, op.Operation, evidence); err != nil {
			return err
		}
		pins, err := store.PinsForOperation(ctx, op.Operation)
		if err != nil {
			return err
		}
		for _, p := range pins {
			if p.State != registrypins.Pending {
				return errBuildPinState
			}
			if p.BindingUID == "" {
				if _, err := store.ObserveImportPod(ctx, p, evidence); err != nil {
					return err
				}
			} else if p.BindingUID != evidence.UID || p.BindingGeneration != evidence.Nonce {
				return errBuildPinState
			}
		}
		if status == sandbox.BuildReady && (result == nil || evidence.Phase != "Succeeded" || sandboximport.ValidateResult(*result, b.Import.Source, b.Import.Destination) != nil) {
			return errBuildPinState
		}
		err = h.importOperationWrite(ctx, t, b, buildDispatching, func(_ *sandboxTemplateDoc, cb *templateBuild) error {
			cb.Import.Registry.State, cb.Import.Registry.Pod = buildTerminalObserved, &evidence
			cb.Status, cb.Reason, cb.Import.Result = status, sandbox.RedactBuildText(reason), result
			if status == sandbox.BuildReady {
				cb.Image = result.Image
				if cb.User == "" {
					cb.User = result.User
				}
				if cb.Workdir == "" {
					cb.Workdir = result.Workdir
				}
			}
			now := time.Now().UTC()
			cb.FinishedAt = &now
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		if op.Pod == nil {
			return errBuildPinState
		}
		evidence = *op.Pod
	}
	pins, err := store.PinsForOperation(ctx, op.Operation)
	if err != nil {
		return err
	}
	for _, p := range pins {
		if p.State == registrypins.Bound {
			if p.Ref != b.Image || p.BindingUID != "template:"+t.ID+":"+b.BuildID || p.BindingGeneration != op.Operation.Generation {
				return errBuildPinState
			}
			continue
		}
		if p.State == registrypins.Released && b.Status == sandbox.BuildError {
			continue
		}
		if p.State != registrypins.Pending {
			return errBuildPinState
		}
		if b.Status == sandbox.BuildReady {
			_, err = store.ObserveBinding(ctx, p, registrypins.BindingEvidence{UID: "template:" + t.ID + ":" + b.BuildID, Generation: op.Operation.Generation, DigestRef: b.Image})
		} else {
			_, err = store.ReleaseImportTerminal(ctx, p, evidence)
		}
		if err != nil {
			return err
		}
	}
	return h.importOperationWrite(ctx, t, b, buildTerminalObserved, func(cur *sandboxTemplateDoc, cb *templateBuild) error {
		cb.Import.Registry.State = buildPinFinished
		if cb.Status == sandbox.BuildReady {
			if cur.Tags == nil {
				cur.Tags = map[string]string{}
			}
			for _, tag := range cb.Tags {
				cur.Tags[tag] = cb.BuildID
			}
		}
		return nil
	})
}

func (h *Handler) retireImportPins(ctx context.Context, ss *sandbox.Service, t *sandboxTemplateDoc) error {
	store, err := h.buildPinStore(ss.Config())
	if err != nil {
		return err
	}
	for _, b := range t.Builds {
		if b.Import == nil {
			continue
		}
		if importPinActive(b) || b.Import.Registry == nil || store == nil {
			return errBuildPinState
		}
		pins, err := store.PinsForOperation(ctx, b.Import.Registry.Operation)
		if err != nil {
			return err
		}
		for _, p := range pins {
			if p.State == registrypins.Released {
				continue
			}
			if p.State != registrypins.Bound {
				return errBuildPinState
			}
			if held, err := h.retainedBuildReference(ctx, ss.Config(), t.ID, b.BuildID, p.Ref); err != nil || held {
				return errors.Join(errBuildPinState, err)
			}
			if _, err := store.ReleaseBinding(ctx, p, registrypins.BindingEvidence{UID: p.BindingUID, Generation: p.BindingGeneration, DigestRef: p.Ref}); err != nil {
				return err
			}
		}
	}
	return nil
}
