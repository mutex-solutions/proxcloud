package project

// Native/E2B CREATE participation only. This durable owner is independent of the
// deletable billing row. There is intentionally no runtime retirement/release API.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/registrypins"
)

const runtimeStartCollection = "sandbox_runtime_start"

var errRuntimePinState = errors.New("sandbox runtime reference held; operator recovery required")

// The only variable public diagnostic is a server-generated operation UUID.
// Neither raw payloads, environment, credentials nor DB errors reach this error.
type runtimePinHold struct{ operation string }

func (e *runtimePinHold) Error() string {
	if e.operation == "" {
		return errRuntimePinState.Error()
	}
	return errRuntimePinState.Error() + " (operation " + e.operation + ")"
}
func (e *runtimePinHold) Unwrap() error { return errRuntimePinState }

type runtimeStartOwner struct {
	ID           string                      `json:"id"`
	Operation    registrypins.Operation      `json:"operation"`
	ProjectID    string                      `json:"projectId"`
	ServiceID    string                      `json:"serviceId"`
	SandboxID    string                      `json:"sandboxId"`
	State        string                      `json:"state"`
	Spec         registrypins.RuntimeSpec    `json:"spec"`
	Source       sandbox.RuntimeCreateSource `json:"source"`
	ProviderHash string                      `json:"providerHash"`
	Pins         []registrypins.Pin          `json:"pins,omitempty"`
	PodUID       string                      `json:"podUID,omitempty"`
	CRUID        string                      `json:"crUID,omitempty"`
}

// SetSandboxRuntimePins wires the explicit PRIMARY database. Missing wiring is
// fail-closed for managed sources, not a flag that disables protection.
func (h *Handler) SetSandboxRuntimePins(db *pgdoc.DB) { h.sbRuntimeDB = db }
func (h *Handler) runtimeOwners() *pgdoc.Store        { return h.sbRuntimeDB.C(runtimeStartCollection) }

// EnsureSandboxRuntimePins runs at startup, outside ANY inherited transaction.
// Helpers also call it before their first transaction for isolated compositions.
func EnsureSandboxRuntimePins(ctx context.Context, db *pgdoc.DB) error {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return err
	}
	if db == nil {
		return errRuntimePinState
	}
	s := db.C(runtimeStartCollection)
	if err := s.Ensure(ctx); err != nil {
		return err
	}
	return s.EnsureIndex(ctx, "identity", true, pgdoc.F("projectId"), pgdoc.F("serviceId"), pgdoc.F("sandboxId"))
}
func runtimeHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func runtimeProviderHash(es *externalservice.ExternalService) string {
	return runtimeHash([]any{es.ID, es.Type, es.Status, es.Config, es.ObservationGeneration})
}
func runtimeSource(t *sandboxTemplateDoc, b *templateBuild, ref string, effective *sandbox.Template) *sandbox.RuntimeCreateSource {
	gen := ""
	if b.Registry != nil {
		gen = b.Registry.Operation.Generation
	}
	if b.Import != nil && b.Import.Registry != nil {
		gen += ":" + b.Import.Registry.Operation.Generation
	}
	return &sandbox.RuntimeCreateSource{RequestedAlias: ref, TemplateID: t.ID, BuildID: b.BuildID, Generation: gen,
		CandidateHash: runtimeHash([]any{t.ID, t.ProjectID, t.Name, t.Kind, t.Internal, t.Deleting, b.BuildID, gen, b.Status, effective})}
}

// resolveRuntimeCreateTemplate preserves the ORIGINAL alias/tag and returns only
// a candidate. It does not resolve a registry tag or grant image-use authority.
func (h *Handler) resolveRuntimeCreateTemplate(ctx context.Context, project, ref string) (*sandbox.Template, string, *sandbox.RuntimeCreateSource, error) {
	if len(ref) == 0 || len(ref) > 128 {
		return nil, "", nil, sandbox.ErrBadTemplate
	}
	custom, name, _, source, err := h.resolveCustomTemplateCandidate(ctx, project, ref)
	if source == nil {
		source = &sandbox.RuntimeCreateSource{RequestedAlias: ref}
	}
	return custom, name, source, err
}

func (h *Handler) runtimeWrite(ctx context.Context, o *runtimeStartOwner, expected string, apply func(*runtimeStartOwner) error) error {
	return h.sbRuntimeDB.WithTx(ctx, func(tc context.Context) error {
		var cur runtimeStartOwner
		found, err := h.runtimeOwners().GetForUpdate(tc, o.ID, &cur)
		if err != nil {
			return err
		}
		if !found || cur.Operation != o.Operation || cur.Spec != o.Spec || cur.Source != o.Source || cur.ProviderHash != o.ProviderHash || cur.State != expected {
			return errRuntimePinState
		}
		if err = apply(&cur); err != nil {
			return err
		}
		ok, err := h.runtimeOwners().Replace(tc, o.ID, &cur)
		if err != nil {
			return err
		}
		if !ok {
			return errRuntimePinState
		}
		return nil
	})
}

// prepareRuntimeCreate commits PREPARING, one exact Pending receipt, PINNED and
// the factory marker in separate standalone transactions BEFORE withSandboxStart.
// A lost response is never retried, reacquired or reminted.
func (h *Handler) prepareRuntimeCreate(ctx context.Context, proj *Project, es *externalservice.ExternalService, ss *sandbox.Service, spec *sandbox.Spec) (*runtimeStartOwner, *registrypins.Store, error) {
	image, protected, err := sandbox.RuntimeCreateImage(ss.Config(), *spec)
	if err != nil {
		return nil, nil, &runtimePinHold{}
	}
	if !protected {
		return nil, nil, nil
	}
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return nil, nil, err
	}
	if h.sbRuntimeDB == nil || h.sbTemplates == nil || h.esSvc == nil {
		return nil, nil, &runtimePinHold{}
	}
	if err = EnsureSandboxRuntimePins(ctx, h.sbRuntimeDB); err != nil {
		return nil, nil, &runtimePinHold{}
	}
	// Never discover a missing collection from inside the revalidation tx.
	for _, table := range []string{"project", "externalService", "sandbox_templates"} {
		if err = h.sbRuntimeDB.C(table).Ensure(ctx); err != nil {
			return nil, nil, &runtimePinHold{}
		}
	}
	host, _, _ := stringsCutRegistry(ss.Config().TemplateRegistry)
	store, err := registrypins.New(h.sbRuntimeDB, host)
	if err != nil {
		return nil, nil, &runtimePinHold{}
	}
	if spec.ID != "" || spec.RuntimePins != nil {
		return nil, nil, &runtimePinHold{}
	}
	spec.ID = sandbox.NewSandboxID()
	if spec.RuntimeSource == nil {
		spec.RuntimeSource = &sandbox.RuntimeCreateSource{RequestedAlias: spec.Template}
	}
	if len(spec.RuntimeSource.RequestedAlias) == 0 || len(spec.RuntimeSource.RequestedAlias) > 128 {
		return nil, nil, &runtimePinHold{}
	}
	envelope, err := sandbox.RuntimeCreateSpec(ss.Config(), es.ID, *spec)
	if err != nil {
		return nil, nil, &runtimePinHold{}
	}
	o := &runtimeStartOwner{ID: uuid.NewString(), ProjectID: proj.ID, ServiceID: es.ID, SandboxID: spec.ID, State: buildPreparing,
		Spec: envelope, Source: *spec.RuntimeSource, ProviderHash: runtimeProviderHash(es)}
	o.Operation = registrypins.Operation{ID: o.ID, ProjectID: proj.ID, Owner: "runtime:create:" + es.ID + ":" + spec.ID, Generation: uuid.NewString()}
	if _, err = h.runtimeOwners().InsertOne(ctx, o); err != nil {
		return o, store, &runtimePinHold{operation: o.ID}
	}
	pins, err := store.BeginPins(ctx, o.Operation, []string{image})
	if err != nil {
		// Read-only exact-operation recovery only; this request must stop.
		h.runtimeUnknownDetached(ctx, store, o)
		return o, store, &runtimePinHold{operation: o.ID}
	}
	o.Pins = pins
	if err = h.validateRuntimeCandidate(ctx, o, ss.Config(), spec); err != nil {
		h.runtimeFailUnused(ctx, store, o)
		return o, store, &runtimePinHold{operation: o.ID}
	}
	o.State = buildPinned
	proof, err := store.AuthorizeRuntimeDispatch(ctx, o.Operation, envelope)
	if err != nil {
		h.runtimeUnknownDetached(ctx, store, o)
		return o, store, &runtimePinHold{operation: o.ID}
	}
	if err = h.runtimeWrite(ctx, o, buildPinned, func(cur *runtimeStartOwner) error { cur.State = buildDispatching; return nil }); err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		// Positive local UNUSED cancellation must precede any no-effect release.
		if store.CancelUnusedRuntimeDispatch(cleanup, proof) == nil {
			h.runtimeFailUnused(cleanup, store, o)
		}
		return o, store, &runtimePinHold{operation: o.ID}
	}
	o.State, spec.RuntimePins = buildDispatching, proof
	return o, store, nil
}

func stringsCutRegistry(reg string) (string, string, bool) { return strings.Cut(reg, "/") }

// Host namespace -> current project -> current provider/template -> owner CAS.
// No K8s/registry/network I/O occurs in this transaction.
func (h *Handler) validateRuntimeCandidate(ctx context.Context, o *runtimeStartOwner, cfg sandbox.Config, spec *sandbox.Spec) error {
	return h.sbRuntimeDB.WithTx(ctx, func(tc context.Context) error {
		tx, err := h.sbRuntimeDB.Tx(tc)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(tc, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
			return err
		}
		host, _, _ := stringsCutRegistry(cfg.TemplateRegistry)
		if err = h.sbRuntimeDB.LockXact(tc, "registry-ref:"+host); err != nil {
			return err
		}
		var project Project
		found, err := h.sbRuntimeDB.C("project").GetForUpdate(tc, o.ProjectID, &project)
		if err != nil {
			return err
		}
		if !found || project.Status != StatusEnabled || refuseNewUsage(&project) != nil {
			return errRuntimePinState
		}
		// Lock the encrypted provider row before reading its current decrypted config.
		var provider externalservice.ExternalService
		found, err = h.sbRuntimeDB.C("externalService").GetForUpdate(tc, o.ServiceID, &provider)
		if err != nil {
			return err
		}
		if !found {
			return errRuntimePinState
		}
		current, err := h.esSvc.Get(tc, o.ServiceID)
		if err != nil {
			return err
		}
		if current.IsDisabled() || runtimeProviderHash(current) != o.ProviderHash {
			return errRuntimePinState
		}
		fresh := sandbox.CloneCreateSpec(*spec)
		if o.Source.TemplateID != "" {
			var parent sandboxTemplateDoc
			found, err = h.sbTemplates.GetForUpdate(tc, o.Source.TemplateID, &parent)
			if err != nil {
				return err
			}
			if !found || parent.ProjectID != o.ProjectID || parent.Deleting {
				return errRuntimePinState
			}
			t, name, source, err := h.resolveRuntimeCreateTemplate(tc, o.ProjectID, o.Source.RequestedAlias)
			if err != nil {
				return err
			}
			if source == nil || *source != o.Source || t == nil {
				return errRuntimePinState
			}
			fresh.Custom, fresh.Template, fresh.RuntimeSource = t, name, source
		}
		q := current.SandboxProjectQuota(project.Quota)
		fresh.Quota = &q
		envelope, err := sandbox.RuntimeCreateSpec(current.SandboxConfig(), current.ID, fresh)
		if err != nil {
			return err
		}
		if envelope != o.Spec {
			return errRuntimePinState
		}
		return h.runtimeWrite(tc, o, buildPreparing, func(cur *runtimeStartOwner) error { cur.State, cur.Pins = buildPinned, o.Pins; return nil })
	})
}

// Only used BEFORE a factory exists, or AFTER positive cancellation of its
// UNUSED proof. Commit a non-reusable tombstone before exact Pending release.
func (h *Handler) runtimeFailUnused(ctx context.Context, store *registrypins.Store, o *runtimeStartOwner) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if h.runtimeWrite(cleanup, o, o.State, func(cur *runtimeStartOwner) error { cur.State, cur.Pins = buildPinFailed, o.Pins; return nil }) != nil {
		return
	}
	for _, p := range o.Pins {
		if p.State == registrypins.Pending && p.BindingUID == "" {
			_, _ = store.ReleaseKnownNoEffect(cleanup, p)
		}
	}
	o.State = buildPinFailed
}
func (h *Handler) runtimeUnknownDetached(ctx context.Context, store *registrypins.Store, o *runtimeStartOwner) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if o == nil || store == nil || o.State == buildPinFailed || o.State == "observed" {
		return
	}
	known, err := store.RuntimeTerminalKnown(cleanup, o.Operation)
	if err != nil || known {
		return
	}
	pins, err := store.PinsForOperation(cleanup, o.Operation)
	if err != nil {
		return
	}
	// The host lock serializes this owner CAS with observation's source guard.
	err = h.sbRuntimeDB.WithTx(cleanup, func(tc context.Context) error {
		host, _, _ := strings.Cut(o.Spec.Image, "/")
		if err := h.sbRuntimeDB.LockXact(tc, "registry-ref:"+host); err != nil {
			return err
		}
		return h.runtimeWrite(tc, o, o.State, func(cur *runtimeStartOwner) error { cur.State, cur.Pins = buildPinUnknown, pins; return nil })
	})
	if err != nil {
		return
	} // no successful owner CAS => no pin mutation
	o.State, o.Pins = buildPinUnknown, pins
	for _, p := range pins {
		if p.State == registrypins.Pending || p.State == registrypins.Bound {
			_, _ = store.MarkUnknown(cleanup, p)
		}
	}
}

// Called only by the TWO CREATE entrypoints. Generic e2bWaitReady is unchanged:
// a saved CR on resume/fork can carry old CREATE annotations but grants no lookup
// or observation authority through this explicitly scoped seam.
func (h *Handler) waitRuntimeCreateReady(ctx context.Context, c *e2bCaller, cr *cloud.CloudResource) error {
	live, err := c.ss.WaitReady(ctx, c.proj.ID, cr.ExternalID)
	if err == nil {
		err = startupResponseAlive(ctx, live)
	}
	if err == nil {
		err = h.observeRuntimeCreate(ctx, c, cr.ExternalID)
	}
	if err != nil {
		h.runtimeCreateFailure(ctx, c, cr.ExternalID)
		return err
	}
	cr.Data = live
	now := time.Now().UTC()
	cr.UpdatedAt = &now
	if _, err = h.cloud.Update(ctx, cr); err != nil {
		h.runtimeCreateFailure(ctx, c, cr.ExternalID)
		return err
	}
	return startupResponseAlive(ctx, live)
}
func (h *Handler) findRuntimeOwner(ctx context.Context, project, service, id string) (*runtimeStartOwner, error) {
	if h.sbRuntimeDB == nil {
		return nil, nil
	} // legacy test composition only; managed creates fail earlier
	var o runtimeStartOwner
	found, err := h.runtimeOwners().FindOne(ctx, pgdoc.M{"projectId": project, "serviceId": service, "sandboxId": id}, &o)
	if err != nil || !found {
		return nil, err
	}
	return &o, nil
}
func (h *Handler) runtimeCreateFailure(ctx context.Context, c *e2bCaller, id string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	o, err := h.findRuntimeOwner(cleanup, c.proj.ID, c.es.ID, id)
	if err != nil || o == nil {
		return
	}
	host, _, _ := strings.Cut(o.Spec.Image, "/")
	store, err := registrypins.New(h.sbRuntimeDB, host)
	if err == nil {
		h.runtimeUnknownDetached(cleanup, store, o)
	}
}
func (h *Handler) observeRuntimeCreate(ctx context.Context, c *e2bCaller, id string) error {
	o, err := h.findRuntimeOwner(ctx, c.proj.ID, c.es.ID, id)
	if err != nil {
		return &runtimePinHold{}
	}
	if o == nil {
		return nil
	}
	hold := &runtimePinHold{operation: o.ID}
	if o.State != buildDispatching && o.State != "observed" {
		return hold
	}
	evidence, err := c.ss.RuntimeCreateEvidence(ctx, c.proj.ID, id)
	if err != nil || evidence.Operation != o.ID || evidence.Nonce != o.Operation.Generation || evidence.Spec != o.Spec || (o.PodUID != "" && (o.PodUID != evidence.UID || o.CRUID != evidence.CRUID)) {
		return hold
	}
	host, _, _ := strings.Cut(o.Spec.Image, "/")
	store, err := registrypins.New(h.sbRuntimeDB, host)
	if err != nil {
		return hold
	}
	// Source guard CAS BEFORE privileged Record; unknown cannot be reopened.
	if err = h.runtimeWrite(ctx, o, o.State, func(cur *runtimeStartOwner) error { cur.PodUID, cur.CRUID = evidence.UID, evidence.CRUID; return nil }); err != nil {
		return hold
	}
	if err = store.RecordRuntimePod(ctx, o.Operation, evidence); err != nil {
		return hold
	}
	pins, err := store.PinsForOperation(ctx, o.Operation)
	if err != nil || len(pins) != 1 {
		return hold
	}
	p := pins[0]
	if p.Operation != o.Operation || p.Ref != o.Spec.Image || p.RequestedRef != o.Spec.Image {
		return hold
	}
	if p.State == registrypins.Pending {
		p, err = store.ObserveRuntimePod(ctx, p, evidence)
	}
	if err != nil || p.State != registrypins.Bound || p.BindingUID != evidence.UID || p.BindingGeneration != evidence.Nonce {
		return hold
	}
	if err = h.runtimeWrite(ctx, o, o.State, func(cur *runtimeStartOwner) error {
		cur.State, cur.Pins = "observed", []registrypins.Pin{p}
		cur.PodUID, cur.CRUID = evidence.UID, evidence.CRUID
		return nil
	}); err != nil {
		return hold
	}
	return nil
}
