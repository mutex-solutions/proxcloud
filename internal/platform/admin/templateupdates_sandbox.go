package admin

// templateupdates_sandbox.go — the built-in sandbox image mirror (docs/template-updates.md §4):
//
//	POST /admin/template-updates/sandbox            mirror a published image + prepull switch (admin:service:manage)
//	POST /admin/template-updates/sandbox/rollback   re-point the prepull at the previous pin   (admin:service:manage)
//	GET  /admin/template-updates/sandbox/state      receipts, pins and prepull progress per provider (admin:service:read)
//	GET  /admin/template-updates/sandbox/pins       sandboxBuiltinPin records (admin:service:read; installer stage 55)
//
// Mirror and rollback answer 501 unless PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED is set. The pin list is read-only
// and always available: installer stage 55 must be able to read it whether or not the feature is on.

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// SandboxCluster is what the sandbox surface needs from a provider's cluster (a fake in tests).
type SandboxCluster interface {
	PrepullImages(ctx context.Context, repo string) ([]sandbox.PrepullImage, error)
	PrepullProgress(ctx context.Context, ref string) (int, int, error)
}

// SetSandboxCluster wires the sandbox provider's cluster for the sandbox template updates (cmd/api).
func (h *Handler) SetSandboxCluster(f func(es *externalservice.ExternalService) (SandboxCluster, error)) {
	h.sandboxClusterFor = f
}

const sandboxUpdatesDisabled = "Sandbox template updates are not enabled on this site (PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED): pin the digest in deploy/templates/images.yaml and run make upgrade (docs/template-updates.md)"

func (h *Handler) sandboxUpdatesEnabled(w http.ResponseWriter) bool {
	if h.templateUpdates == nil || !h.templateUpdates.SandboxEnabled {
		httpx.Err(w, http.StatusNotImplemented, http.StatusNotImplemented, sandboxUpdatesDisabled)
		return false
	}
	return true
}

// sandboxTarget resolves an enabled sandbox provider with a template registry, and what its prepull DaemonSets pull
// for the image now (read live: a cluster that cannot be read refuses the request).
func (h *Handler) sandboxTarget(ctx context.Context, w http.ResponseWriter, r *http.Request, serviceID, name string) (templateupdates.SandboxTarget, bool) {
	es, ok := h.loadServiceOr(w, r, serviceID)
	if !ok {
		return templateupdates.SandboxTarget{}, false
	}
	if !es.IsSandbox() || es.IsDisabled() {
		httpx.WriteError(w, httpx.BadRequest("Pick an enabled sandbox provider"))
		return templateupdates.SandboxTarget{}, false
	}
	cfg := es.SandboxConfig()
	host := cfg.RegistryHost()
	if host == "" {
		httpx.WriteError(w, httpx.BadRequest("This sandbox provider has no template registry (config.sandbox.templateRegistry)"))
		return templateupdates.SandboxTarget{}, false
	}
	if cfg.RegistryMirror.Password == "" {
		httpx.Err(w, http.StatusConflict, http.StatusConflict, "This sandbox provider has no registry mirror account yet (secret.registryMirror): run make upgrade (installer stages 55 and 60)")
		return templateupdates.SandboxTarget{}, false
	}
	if h.sandboxClusterFor == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "Sandbox cluster access is not wired")
		return templateupdates.SandboxTarget{}, false
	}
	cl, err := h.sandboxClusterFor(es)
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "The sandbox provider's cluster configuration is incomplete")
		return templateupdates.SandboxTarget{}, false
	}
	cur, err := cl.PrepullImages(ctx, host+"/library/sandbox-"+name)
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "The prepull DaemonSets could not be read (sandbox-system get daemonsets)")
		return templateupdates.SandboxTarget{}, false
	}
	t := templateupdates.SandboxTarget{ServiceID: es.ID, Registry: host}
	for _, c := range cur {
		t.Current = append(t.Current, c.Image)
	}
	return t, true
}

func templateUpdatesActor(r *http.Request) string {
	rc := httpx.RC(r.Context())
	if rc.Sub != "" {
		return rc.Sub
	}
	return rc.SigV4KeyID
}

func sandboxErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, templateupdates.ErrSandboxDisabled):
		httpx.Err(w, http.StatusNotImplemented, http.StatusNotImplemented, sandboxUpdatesDisabled)
	case errors.Is(err, templateupdates.ErrSandboxSameDigest), errors.Is(err, templateupdates.ErrSandboxBusy), errors.Is(err, templateupdates.ErrSandboxNoRollback):
		httpx.Err(w, http.StatusConflict, http.StatusConflict, err.Error())
	default:
		templateUpdatesErr(w, err)
	}
}

// templateUpdatesSandbox enqueues a mirror of a built-in image (the digest discovery offered, confirmed).
func (h *Handler) templateUpdatesSandbox(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesManagePerm) || !h.sandboxUpdatesEnabled(w) {
		return
	}
	var req templateupdates.SandboxRequest
	if !decodeStrictBody(w, r, &req) {
		return
	}
	if req.ConfirmDigest != req.Digest {
		httpx.WriteError(w, httpx.BadRequest("confirm the exact digest to mirror"))
		return
	}
	t, ok := h.sandboxTarget(r.Context(), w, r, req.ServiceID, req.Name)
	if !ok {
		return
	}
	rec, err := h.templateUpdates.EnqueueSandbox(r.Context(), req, t, templateUpdatesActor(r))
	if err != nil {
		sandboxErr(w, err)
		return
	}
	p := rec.Payload
	audit.RecordSnapshots(r.Context(), map[string]any{"prepull": strings.Join(t.Current, ",")},
		map[string]any{"action": p.Action, "image": "sandbox-" + p.Name, "tag": p.Tag, "digest": p.Digest, "serviceId": p.ServiceID, "registry": p.Registry, "prepull": p.Ref(), "job": rec.ID})
	httpx.OK(w, rec)
}

// templateUpdatesSandboxRollback enqueues a rollback to the pin record's previous ref.
func (h *Handler) templateUpdatesSandboxRollback(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesManagePerm) || !h.sandboxUpdatesEnabled(w) {
		return
	}
	var req templateupdates.SandboxRollbackRequest
	if !decodeStrictBody(w, r, &req) {
		return
	}
	t, ok := h.sandboxTarget(r.Context(), w, r, req.ServiceID, req.Name)
	if !ok {
		return
	}
	rec, err := h.templateUpdates.EnqueueSandboxRollback(r.Context(), req, t, templateUpdatesActor(r))
	if err != nil {
		sandboxErr(w, err)
		return
	}
	p := rec.Payload
	audit.RecordSnapshots(r.Context(), map[string]any{"prepull": strings.Join(t.Current, ",")},
		map[string]any{"action": p.Action, "image": "sandbox-" + p.Name, "tag": p.Tag, "digest": p.Digest, "serviceId": p.ServiceID, "registry": p.Registry, "prepull": p.Ref(), "job": rec.ID})
	httpx.OK(w, rec)
}

// SandboxImageState is one built-in image's live state on one sandbox provider (for the progress panel).
type SandboxImageState struct {
	ServiceID   string                      `json:"serviceId"`
	ServiceName string                      `json:"serviceName"`
	Name        string                      `json:"name"`
	Prepull     []string                    `json:"prepull"`
	Pin         *templateupdates.SandboxPin `json:"pin,omitempty"`
	// Pulled/Total: prepull pods on Ready nodes that run the pinned ref / that list the image.
	Pulled int `json:"pulled"`
	Total  int `json:"total"`
	// Templates: the provider's templates on this image and whether they already run the pinned digest.
	Templates []SandboxTemplateState `json:"templates"`
	// ImagesYAML is the deploy/templates/images.yaml line to commit for the pin.
	ImagesYAML string `json:"imagesYaml,omitempty"`
	Error      string `json:"error,omitempty"`
}

// SandboxTemplateState is one template of a provider on the image.
type SandboxTemplateState struct {
	Template string `json:"template"`
	Image    string `json:"image"`
	Switched bool   `json:"switched"`
}

// templateUpdatesSandboxState returns the sandbox receipts plus, per sandbox provider and pinned image, the prepull
// and adoption progress: "waiting for prepull on N/M nodes" until every Ready node pulled, then "switched" once the
// adoption job moved the templates.
func (h *Handler) templateUpdatesSandboxState(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesReadPerm) || !h.templateUpdatesReady(w) {
		return
	}
	ctx := r.Context()
	items, err := h.templateUpdates.SandboxHistory(ctx)
	if httpx.WriteError(w, err) {
		return
	}
	jobs := map[string]any{}
	if h.durableStore != nil {
		for _, it := range items {
			if j, err := h.durableStore.Get(ctx, it.ID); err == nil && j != nil {
				jobs[it.ID] = map[string]any{"state": j.State, "attempts": j.Attempts, "maxAttempts": j.MaxAttempts, "lastError": j.LastError}
			}
		}
	}
	pins, err := h.templateUpdates.SandboxPinList(ctx, "")
	if httpx.WriteError(w, err) {
		return
	}
	states := []SandboxImageState{}
	if h.esSvc != nil {
		services, _ := h.esSvc.ListByType(ctx, externalservice.TypeCloud)
		for i := range services {
			es := &services[i]
			if !es.IsSandbox() || es.IsDisabled() {
				continue
			}
			cfg := es.SandboxConfig()
			host := cfg.RegistryHost()
			for j := range pins {
				pin := pins[j]
				if host == "" || pin.Registry != host {
					continue
				}
				st := SandboxImageState{ServiceID: es.ID, ServiceName: es.Name, Name: pin.Template, Pin: &pin, Prepull: []string{}, Templates: []SandboxTemplateState{},
					ImagesYAML: templateupdates.ImagesYAMLLine(pin.Template, pin.Tag, pin.Digest)}
				for tn, t := range cfg.Templates {
					if n, d, ok := templateupdates.SandboxNameOfImage(t.Image); ok && n == pin.Template {
						st.Templates = append(st.Templates, SandboxTemplateState{Template: tn, Image: t.Image, Switched: d == pin.Digest})
					}
				}
				sort.Slice(st.Templates, func(a, b int) bool { return st.Templates[a].Template < st.Templates[b].Template })
				if h.sandboxClusterFor == nil {
					st.Error = "sandbox cluster access is not wired"
				} else if cl, err := h.sandboxClusterFor(es); err != nil {
					st.Error = "the provider's cluster configuration is incomplete"
				} else {
					if cur, err := cl.PrepullImages(ctx, pin.Registry+"/library/sandbox-"+pin.Template); err == nil {
						for _, c := range cur {
							st.Prepull = append(st.Prepull, c.Image)
						}
					} else {
						st.Error = "the prepull DaemonSets could not be read"
					}
					if st.Error == "" {
						if st.Pulled, st.Total, err = cl.PrepullProgress(ctx, pin.Ref); err != nil {
							st.Error = "the prepull pods could not be read"
						}
					}
				}
				states = append(states, st)
			}
		}
	}
	httpx.OK(w, map[string]any{"enabled": h.templateUpdates.SandboxEnabled, "items": items, "jobs": jobs, "pins": pins, "states": states})
}

// templateUpdatesSandboxPins lists the sandboxBuiltinPin records, optionally of one registry (?registry=host[:port]).
// Read-only (admin:service:read), no secret: installer stage 55 reads it with its admin API key so a make upgrade
// renders the prepull with the newer of images.yaml and the recorded pin (docs/template-updates.md §4).
func (h *Handler) templateUpdatesSandboxPins(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesReadPerm) || !h.templateUpdatesReady(w) {
		return
	}
	pins, err := h.templateUpdates.SandboxPinList(r.Context(), r.URL.Query().Get("registry"))
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "The sandbox pin records could not be read")
		return
	}
	httpx.OK(w, map[string]any{"pins": pins})
}
