package admin

// templateupdates.go — the admin "Template updates" surface (docs/template-updates.md):
//
//	GET  /admin/template-updates/status      GitHub integration state   (admin:service:read)
//	GET  /admin/template-updates             discovery (?refresh=1)     (admin:service:read)
//	GET  /admin/template-updates/history     job receipts               (admin:service:read)
//	GET  /admin/template-updates/placement/{serviceId}  node/storage choices (admin:service:read)
//	POST /admin/template-updates/vm          enqueue a VM template import (admin:service:manage)
//	POST /admin/template-updates/sandbox     sandbox pin switch — not yet enabled (501)
//
// The GitHub PAT lives in a thirdPartyIntegration of type "GitHub" (secret.token, encrypted at
// rest, never returned); it is validated with GET /user before it is saved.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const (
	templateUpdatesReadPerm   = "admin:service:read"
	templateUpdatesManagePerm = "admin:service:manage"
)

// SetTemplateUpdates wires the template update service (cmd/api). nil → the surface answers 503.
func (h *Handler) SetTemplateUpdates(s *templateupdates.Service) { h.templateUpdates = s }

func (h *Handler) routeTemplateUpdates(r chi.Router) {
	r.Get("/template-updates", h.templateUpdatesDiscover)
	r.Get("/template-updates/status", h.templateUpdatesStatus)
	r.Get("/template-updates/history", h.templateUpdatesHistory)
	r.Get("/template-updates/placement/{serviceId}", h.templateUpdatesPlacement)
	r.Post("/template-updates/vm", h.templateUpdatesVM)
	r.Post("/template-updates/sandbox", h.templateUpdatesSandbox)
}

func (h *Handler) templateUpdatesReady(w http.ResponseWriter) bool {
	if h.templateUpdates == nil {
		httpx.Err(w, 503, 503, "Template updates are not configured")
		return false
	}
	return true
}

// templateUpdatesErr maps service errors: missing integration → 409 with the fixed instruction,
// rate limits → 429 with the reset time, GitHub auth refusals → 502.
func templateUpdatesErr(w http.ResponseWriter, err error) {
	var rl *templateupdates.RateLimitError
	var he *templateupdates.HTTPError
	switch {
	case errors.Is(err, templateupdates.ErrNotConfigured), errors.Is(err, templateupdates.ErrNoToken):
		httpx.Err(w, http.StatusConflict, http.StatusConflict, templateupdates.ErrNotConfigured.Error())
	case errors.As(err, &rl):
		httpx.Err(w, http.StatusTooManyRequests, http.StatusTooManyRequests, rl.Error())
	case errors.As(err, &he):
		httpx.Err(w, http.StatusBadGateway, http.StatusBadGateway, he.Error())
	case errors.Is(err, templateupdates.ErrDuplicate):
		httpx.Err(w, http.StatusConflict, http.StatusConflict, err.Error())
	default:
		httpx.WriteError(w, httpx.BadRequest(err.Error()))
	}
}

func (h *Handler) templateUpdatesStatus(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesReadPerm) || !h.templateUpdatesReady(w) {
		return
	}
	httpx.OK(w, h.templateUpdates.GetStatus(r.Context()))
}

// providerViews reads every Proxmox provider's pc-templates rows and every sandbox provider's
// template images. An unreachable cluster is reported, never treated as "nothing installed".
func (h *Handler) providerViews(ctx context.Context) ([]templateupdates.ProviderView, []string) {
	out := []templateupdates.ProviderView{}
	names := map[string]bool{}
	if h.esSvc == nil {
		return out, nil
	}
	services, _ := h.esSvc.ListByType(ctx, externalservice.TypeCloud)
	for i := range services {
		es := &services[i]
		if es.IsDisabled() {
			continue
		}
		switch {
		case es.IsProxmox():
			v := templateupdates.ProviderView{ID: es.ID, Name: es.Name}
			if h.pveFor == nil {
				v.Error = "Proxmox access is not wired"
			} else if ps, err := h.pveFor(es); err != nil {
				v.Error = "provider configuration is incomplete"
			} else if rows, err := ps.C.ClusterResources(ctx, "vm"); err != nil {
				v.Error = "the cluster could not be read"
			} else {
				v.Templates = []pve.ClusterResource{}
				for _, row := range rows {
					if row.Template == 1 && row.Pool == pve.TemplatePool {
						v.Templates = append(v.Templates, row)
					}
				}
			}
			out = append(out, v)
		case es.IsSandbox():
			v := templateupdates.ProviderView{ID: es.ID, Name: es.Name, SandboxImages: map[string]string{}}
			for name, t := range es.SandboxConfig().Templates {
				v.SandboxImages[name] = t.Image
				if n, _, ok := templateupdates.SandboxNameOfImage(t.Image); ok {
					names[n] = true
				}
			}
			out = append(out, v)
		}
	}
	// The built-in set is also discovered when no provider pins it yet.
	for _, n := range templateupdates.BuiltinSandboxNames {
		names[n] = true
	}
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	return out, list
}

func (h *Handler) templateUpdatesDiscover(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesReadPerm) || !h.templateUpdatesReady(w) {
		return
	}
	providers, names := h.providerViews(r.Context())
	d, err := h.templateUpdates.Discover(r.Context(), providers, names, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	httpx.OK(w, d)
}

func (h *Handler) templateUpdatesHistory(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesReadPerm) || !h.templateUpdatesReady(w) {
		return
	}
	items, err := h.templateUpdates.History(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	jobs := map[string]any{}
	if h.durableStore != nil {
		for _, it := range items {
			if j, err := h.durableStore.Get(r.Context(), it.ID); err == nil && j != nil {
				jobs[it.ID] = map[string]any{"state": j.State, "attempts": j.Attempts, "maxAttempts": j.MaxAttempts, "lastError": j.LastError}
			}
		}
	}
	httpx.OK(w, map[string]any{"items": items, "jobs": jobs})
}

// PlacementOption is one node with its import-capable and template-capable storages.
type PlacementOption struct {
	Node           string   `json:"node"`
	ImportStorages []string `json:"importStorages"`
	DiskStorages   []string `json:"diskStorages"`
}

// templateUpdatesPlacement lists, per node, the active storages with import content and the
// active storages with images content (where a template disk can live). The provider's
// configured importStorage is listed first; nothing is guessed: the admin picks.
func (h *Handler) templateUpdatesPlacement(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesReadPerm) {
		return
	}
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "serviceId"))
	if !ok {
		return
	}
	opts, err := h.placementOptions(r.Context(), es)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	httpx.OK(w, map[string]any{"configuredImportStorage": es.PVEImportStorage(), "nodes": opts})
}

func (h *Handler) placementOptions(ctx context.Context, es *externalservice.ExternalService) ([]PlacementOption, error) {
	if !es.IsProxmox() || h.pveFor == nil {
		return nil, errors.New("not a Proxmox provider")
	}
	ps, err := h.pveFor(es)
	if err != nil {
		return nil, errors.New("provider configuration is incomplete")
	}
	caps, err := ps.C.DetectStorage(ctx)
	if err != nil {
		return nil, errors.New("the cluster's storage could not be read")
	}
	byNode := map[string]*PlacementOption{}
	for name, c := range caps {
		for node, st := range c.Nodes {
			if !st.Active {
				continue
			}
			o := byNode[node]
			if o == nil {
				o = &PlacementOption{Node: node, ImportStorages: []string{}, DiskStorages: []string{}}
				byNode[node] = o
			}
			if c.HasContent("import") {
				o.ImportStorages = append(o.ImportStorages, name)
			}
			if c.HasContent("images") {
				o.DiskStorages = append(o.DiskStorages, name)
			}
		}
	}
	pref := es.PVEImportStorage()
	out := make([]PlacementOption, 0, len(byNode))
	for _, o := range byNode {
		sort.Slice(o.ImportStorages, func(i, j int) bool {
			if (o.ImportStorages[i] == pref) != (o.ImportStorages[j] == pref) {
				return o.ImportStorages[i] == pref
			}
			return o.ImportStorages[i] < o.ImportStorages[j]
		})
		sort.Strings(o.DiskStorages)
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out, nil
}

func decodeStrictBody(w http.ResponseWriter, r *http.Request, out any) bool {
	return decodeStrictLimited(w, r, out, 8<<10)
}

func decodeStrictLimited(w http.ResponseWriter, r *http.Request, out any, limit int64) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil || d.Decode(new(any)) != io.EOF {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return false
	}
	return true
}

// templateUpdatesVM enqueues a VM template import. The placement must be one the provider
// actually offers (fail closed); the digest is re-verified server-side by the service.
func (h *Handler) templateUpdatesVM(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesManagePerm) || !h.templateUpdatesReady(w) {
		return
	}
	var req templateupdates.VMRequest
	if !decodeStrictBody(w, r, &req) {
		return
	}
	es, ok := h.loadServiceOr(w, r, req.ServiceID)
	if !ok {
		return
	}
	if !es.IsProxmox() || es.IsDisabled() {
		httpx.WriteError(w, httpx.BadRequest("Pick an enabled Proxmox provider"))
		return
	}
	opts, err := h.placementOptions(r.Context(), es)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	if !placementAllowed(opts, req.Node, req.ImportStorage, req.DiskStorage) {
		httpx.WriteError(w, httpx.BadRequest("The node must have the chosen import storage (import content) and disk storage (images content) active"))
		return
	}
	rc := httpx.RC(r.Context())
	actor := rc.Sub
	if actor == "" {
		actor = rc.SigV4KeyID
	}
	rec, err := h.templateUpdates.EnqueueVM(r.Context(), req, actor)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	audit.RecordSnapshots(r.Context(), nil, map[string]any{"package": rec.Payload.Package, "tag": rec.Payload.Tag, "digest": rec.Payload.Digest,
		"serviceId": rec.Payload.ServiceID, "node": rec.Payload.Node, "importStorage": rec.Payload.ImportStorage, "diskStorage": rec.Payload.DiskStorage, "publish": rec.Payload.Publish})
	httpx.OK(w, rec)
}

func placementAllowed(opts []PlacementOption, node, importStorage, diskStorage string) bool {
	for _, o := range opts {
		if o.Node != node {
			continue
		}
		in := func(list []string, v string) bool {
			for _, x := range list {
				if x == v {
					return true
				}
			}
			return false
		}
		return in(o.ImportStorages, importStorage) && in(o.DiskStorages, diskStorage)
	}
	return false
}

// templateUpdatesSandbox is the seam of the sandbox mirror + pin switch: not yet enabled. The
// portal holds no credential that may write the sandbox registry's library/ namespace and no RBAC
// on the prepull DaemonSet; the follow-up design is in docs/template-updates.md. Until then the
// page shows the exact images.yaml pin and the installer path (make upgrade → adoption job).
func (h *Handler) templateUpdatesSandbox(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesManagePerm) {
		return
	}
	httpx.Err(w, http.StatusNotImplemented, http.StatusNotImplemented,
		"Sandbox template updates are not enabled yet: pin the digest in deploy/templates/images.yaml and run make upgrade (docs/template-updates.md)")
}

// ── GitHub integration (thirdPartyIntegration type "GitHub") ─────────────────────────────────

// githubIntegrationCheck validates a GitHub integration body before it is stored. On update an
// ABSENT secret or token key keeps the stored token (write-only secret); a token key that is present
// must be a non-empty string, so an empty or non-string value can never wipe the stored PAT.
// Returns the error to answer with.
func (h *Handler) githubIntegrationCheck(ctx context.Context, secret any, requireToken bool) error {
	if err := githubSecretShape(secret, requireToken); err != nil {
		return err
	}
	m, _ := secret.(map[string]any)
	tok, _ := m["token"].(string)
	if tok == "" {
		return nil // absent on update: keep the stored token
	}
	if _, err := templateupdates.ValidateToken(ctx, tok, h.githubOptions); err != nil {
		var rl *templateupdates.RateLimitError
		if errors.As(err, &rl) {
			return httpx.NewError(http.StatusTooManyRequests, http.StatusTooManyRequests, "GitHub token check: "+rl.Error())
		}
		return httpx.BadRequest("GitHub token check failed: " + strings.TrimPrefix(err.Error(), "github: "))
	}
	return nil
}

// githubCount counts GitHub integrations (overridable in tests).
func (h *Handler) githubCount(ctx context.Context) (int64, error) {
	if h.githubCountFn != nil {
		return h.githubCountFn(ctx)
	}
	return h.repo.GitHubIntegrationCount(ctx)
}

// githubSecretShape checks the secret's shape without I/O.
func githubSecretShape(secret any, requireToken bool) error {
	if secret == nil {
		if requireToken {
			return httpx.BadRequest("A GitHub integration needs a personal access token (classic) with read:packages")
		}
		return nil
	}
	m, ok := secret.(map[string]any)
	if !ok {
		return httpx.BadRequest("A GitHub integration's secret must be an object holding token")
	}
	for k := range m {
		if k != "token" {
			return httpx.BadRequest("A GitHub integration's secret holds only token")
		}
	}
	raw, present := m["token"]
	if !present {
		if requireToken {
			return httpx.BadRequest("A GitHub integration needs a personal access token (classic) with read:packages")
		}
		return nil
	}
	tok, isString := raw.(string)
	if !isString || strings.TrimSpace(tok) == "" {
		return httpx.BadRequest("The GitHub token must be a non-empty string")
	}
	return nil
}

// GitHubToken reads the decrypted PAT of THE GitHub integration ("" when none). Creation refuses a
// second one; should several exist anyway (created before that check), the oldest by createdAt wins,
// deterministically, and the page reports the conflict.
func (r *Repo) GitHubToken(ctx context.Context, decrypt func(any) any) (string, string, error) {
	var docs []pgdoc.M
	if err := r.c(integrationCollection).Find(ctx, pgdoc.M{"thirdParty": templateupdates.ThirdPartyGitHub}, &docs); err != nil {
		return "", "", err
	}
	if len(docs) == 0 {
		return "", "", nil
	}
	sort.SliceStable(docs, func(i, j int) bool {
		ci, _ := docs[i]["createdAt"].(string)
		cj, _ := docs[j]["createdAt"].(string)
		if ci != cj {
			return ci < cj
		}
		return integrationDocID(docs[i]) < integrationDocID(docs[j])
	})
	d := docs[0]
	sec := d["secret"]
	if decrypt != nil {
		sec = decrypt(sec)
	}
	m, _ := sec.(map[string]any)
	tok, _ := m["token"].(string)
	return tok, integrationDocID(d), nil
}

// GitHubIntegrationCount counts GitHub integrations (creation allows only one).
func (r *Repo) GitHubIntegrationCount(ctx context.Context) (int64, error) {
	return r.c(integrationCollection).Count(ctx, pgdoc.M{"thirdParty": templateupdates.ThirdPartyGitHub})
}

func integrationDocID(d pgdoc.M) string {
	if v, ok := d["_id"].(string); ok {
		return v
	}
	v, _ := d["id"].(string)
	return v
}
