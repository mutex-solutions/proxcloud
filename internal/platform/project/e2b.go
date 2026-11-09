package project

// e2b.go — the E2B-compatible sandbox API (SB5), so the stock E2B SDKs work against ProxCloud with
// E2B_API_URL=https://<api host>/e2b and E2B_API_KEY=<a sandbox API key of the project>. Sandboxes themselves (envd and
// the customer's ports) are reached at https://<port>-<sandboxID>.<domain> through sandbox-proxy (docs/managed-sandbox.md
// §4): the domain comes back in the create answer, so callers never set it.
//
// The surface follows E2B's openapi.yml (e2b-dev/infra spec/openapi.yml): the same paths, bodies, field names and
// {code, message} errors. What is not here answers 404 "not supported", and docs/e2b-compat.md lists it. Every call runs
// as the key's creator inside the key's project, through the same checks as the native API (cloud_sandbox.go): the
// project permission, the suspension gate, the provider's DISABLED state, the release gates and the project quota.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// E2BRoutes mounts the E2B-compatible API (prefix /e2b, public for pkg/auth: the X-API-Key gate is e2bAuth).
func (h *Handler) E2BRoutes(r chi.Router) {
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(e2bOpenAPI)
	})
	// The SDK uploads a template's COPY context here with the URL GET .../files/{hash} answered (the token is the auth).
	r.Put("/templates/{templateID}/files/{hash}/upload", h.e2bTemplateUpload)
	r.Group(func(r chi.Router) {
		r.Use(h.e2bAuth)
		r.Post("/sandboxes", h.e2bCreate)
		r.Post("/v2/sandboxes", h.e2bCreate)
		r.Get("/sandboxes", h.e2bList)
		r.Get("/v2/sandboxes", h.e2bList)
		r.Get("/sandboxes/metrics", h.e2bMetricsMany)
		r.Get("/sandboxes/{sandboxID}", h.e2bGet)
		r.Delete("/sandboxes/{sandboxID}", h.e2bKill)
		r.Post("/sandboxes/{sandboxID}/timeout", h.e2bTimeout)
		r.Post("/sandboxes/{sandboxID}/refreshes", h.e2bRefresh)
		r.Post("/sandboxes/{sandboxID}/connect", h.e2bConnect)
		r.Post("/v2/sandboxes/{sandboxID}/connect", h.e2bConnect)
		r.Put("/sandboxes/{sandboxID}/network", h.e2bNetwork)
		r.Get("/sandboxes/{sandboxID}/logs", h.e2bLogs)
		r.Get("/v2/sandboxes/{sandboxID}/logs", h.e2bLogs)
		r.Get("/sandboxes/{sandboxID}/metrics", h.e2bMetrics)
		r.Post("/sandboxes/{sandboxID}/pause", h.e2bPause)
		r.Post("/sandboxes/{sandboxID}/resume", h.e2bResume)
		r.Post("/sandboxes/{sandboxID}/snapshots", h.e2bSnapshotCreate)
		r.Post("/sandboxes/{sandboxID}/fork", h.e2bFork)
		r.Get("/snapshots", h.e2bSnapshotList)
		r.Get("/templates", h.e2bTemplates)
		r.Post("/templates/import", h.e2bTemplateImport)
		r.Post("/templates/{templateID}/aliases", h.e2bTemplateAliases)
		r.Delete("/templates/{templateID}/aliases", h.e2bTemplateAliases)
		r.Post("/templates/{templateID}/prepare", h.e2bPrepareTemplate)
		r.Get("/templates/{templateID}/prepare", h.e2bPrepareTemplate)
		r.Get("/templates/aliases/{alias}", h.e2bTemplateAlias)
		r.Get("/templates/{templateID}", h.e2bTemplateGet)
		r.Delete("/templates/{templateID}", h.e2bTemplateDeleteLocked)
		r.Get("/templates/{templateID}/tags", h.e2bTemplateTags)
		r.Post("/templates/tags", h.e2bTemplateTagsAssign)
		r.Delete("/templates/tags", h.e2bTemplateTagsRemove)
		r.Post("/v3/templates", h.e2bTemplateRequestLocked)
		r.Get("/templates/{templateID}/files/{hash}", h.e2bTemplateFile)
		r.Post("/v2/templates/{templateID}/builds/{buildID}", h.e2bTemplateBuildLocked)
		r.Get("/templates/{templateID}/builds/{buildID}/status", h.e2bTemplateBuildStatus)
	})
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		e2bErr(w, http.StatusNotFound, "this endpoint is not supported (see "+h.e2bDocs()+")")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		e2bErr(w, http.StatusNotFound, "this endpoint is not supported (see "+h.e2bDocs()+")")
	})
}

// ---- errors ------------------------------------------------------------------------------------

// e2bErr writes E2B's Error schema: {code, message}.
func e2bErr(w http.ResponseWriter, status int, msg string) {
	e2bJSON(w, status, map[string]any{"code": status, "message": msg})
}

func e2bJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// e2bFail maps a sandbox/platform error to an E2B error response.
func e2bFail(w http.ResponseWriter, err error) {
	status, msg := e2bError(err)
	e2bErr(w, status, msg)
}

// e2bError maps err to the status and message shown to the caller (internals are logged only).
func e2bError(err error) (int, string) {
	if errors.Is(err, errBuildPinState) {
		return http.StatusConflict, "template operation is active, held or requires operator recovery"
	}
	var se e2bStatusErr
	if errors.As(err, &se) {
		return se.code, se.msg
	}
	if status, msg, ok := sandboxHTTPError(err); ok {
		var kerr *kamajik8s.APIError
		switch {
		case status == http.StatusBadRequest && errors.As(err, &kerr):
			// Raw Kubernetes text names namespaces, service accounts and RBAC: log it, do not echo it.
			slog.Warn("e2b: kubernetes rejected a request", "err", err)
			msg = "the sandbox cluster rejected the request"
		case status == http.StatusServiceUnavailable:
			slog.Error("e2b: sandbox cluster error", "err", err)
		}
		return status, msg
	}
	var he *httpx.HTTPError
	if errors.As(err, &he) && he.Status >= 400 && he.Status < 600 {
		return he.Status, he.Msg
	}
	slog.Error("e2b: request failed", "err", err)
	return http.StatusInternalServerError, "internal error"
}

// ---- auth --------------------------------------------------------------------------------------

type e2bCtxKey struct{}

// e2bCaller is the resolved X-API-Key: the creating user, acting in the key's project, on the project's sandbox provider.
type e2bCaller struct {
	user *user.User
	proj *Project
	es   *externalservice.ExternalService
	ss   *sandbox.Service
}

func e2bFrom(r *http.Request) *e2bCaller { return r.Context().Value(e2bCtxKey{}).(*e2bCaller) }

// e2bAuth resolves X-API-Key (or `Authorization: Bearer <key>`) to a caller. The key's creator must still hold
// cloud-resource rights in the project, checked on every call: removing a member stops their keys.
func (h *Handler) e2bAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		k, err := h.lookupSandboxKey(r.Context(), key)
		if err != nil {
			slog.Error("e2b: key lookup", "err", err)
		}
		if err != nil || k == nil {
			e2bErr(w, http.StatusUnauthorized, "Invalid API key")
			return
		}
		ctx := r.Context()
		u, err := h.users.Require(ctx, k.Sub)
		if err != nil {
			e2bErr(w, http.StatusUnauthorized, "Invalid API key")
			return
		}
		proj, err := h.svc.GetProject(ctx, u.Sub, k.ProjectID)
		if err != nil || h.policy.RequireProjectPermission(ctx, u.Sub, proj, rbac.ProjectCloudResourceManage) != nil {
			e2bErr(w, http.StatusForbidden, "the API key's owner can no longer manage sandboxes in this project")
			return
		}
		c, status, msg := h.e2bCallerFor(ctx, u, proj)
		if c == nil {
			e2bErr(w, status, msg)
			return
		}
		h.touchSandboxKey(k)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, e2bCtxKey{}, c)))
	})
}

// e2bCallerFor resolves the project's E2B-capable sandbox provider (nil + status/message if none).
func (h *Handler) e2bCallerFor(ctx context.Context, u *user.User, proj *Project) (*e2bCaller, int, string) {
	svcID := h.sandboxServiceOf(ctx, proj)
	if svcID == "" {
		return nil, http.StatusForbidden, "sandboxes are not enabled for this project"
	}
	es, err := h.esSvc.Get(ctx, svcID)
	if err != nil || es == nil || h.sandboxFor == nil {
		return nil, http.StatusServiceUnavailable, "sandboxes are not available"
	}
	ss, err := h.sandboxFor(es)
	if err != nil {
		slog.Error("e2b: build sandbox service", "service", es.ID, "err", err)
		return nil, http.StatusServiceUnavailable, "sandboxes are not available"
	}
	if cfg := ss.Config(); !cfg.Envd || cfg.Domain == "" {
		return nil, http.StatusServiceUnavailable, "this sandbox location does not offer the E2B API"
	}
	return &e2bCaller{user: u, proj: proj, es: es, ss: ss}, 0, ""
}

// ---- representation ----------------------------------------------------------------------------

func (c *e2bCaller) domain() string { return c.ss.Config().Domain }

// e2bListed renders ListedSandbox (also the base of SandboxDetail) from a sandbox payload.
func e2bListed(id string, d map[string]any) map[string]any {
	tpl, _ := d["template"].(string)
	started, _ := d["runningSince"].(string)
	if started == "" {
		started, _ = d["startedAt"].(string)
	}
	md := map[string]any{}
	switch m := d["metadata"].(type) {
	case map[string]string:
		for k, v := range m {
			md[k] = v
		}
	case map[string]any:
		md = m
	}
	return map[string]any{
		"templateID":  tpl,
		"alias":       tpl,
		"sandboxID":   id,
		"clientID":    "proxcloud",
		"startedAt":   started,
		"endAt":       d["shutdownTime"],
		"cpuCount":    intAny(d["cpu"]),
		"memoryMB":    intAny(d["memoryGiB"]) * 1024,
		"diskSizeMB":  0,
		"metadata":    md,
		"state":       "running",
		"envdVersion": sandbox.EnvdVersion,
	}
}

// e2bSandbox renders the Sandbox schema (create / connect answer).
func (c *e2bCaller) e2bSandbox(id, template string, a *sandbox.Access) map[string]any {
	out := map[string]any{
		"templateID":      template,
		"alias":           template,
		"sandboxID":       id,
		"clientID":        "proxcloud",
		"envdVersion":     sandbox.EnvdVersion,
		"envdAccessToken": a.EnvdAccessToken,
		"domain":          c.domain(),
	}
	if a.TrafficAccessToken != "" {
		out["trafficAccessToken"] = a.TrafficAccessToken
	}
	return out
}

// ---- sandboxes ---------------------------------------------------------------------------------

// e2bNetworkBody is SandboxNetworkConfig (create) / SandboxNetworkUpdateConfig (PUT). Fields we cannot honour are
// decoded so they can be refused instead of silently ignored.
type e2bNetworkBody struct {
	AllowPublicTraffic *bool            `json:"allowPublicTraffic"`
	AllowOut           []string         `json:"allowOut"`
	DenyOut            []string         `json:"denyOut"`
	DNSServers         *[]string        `json:"dnsServers"`
	EgressProxy        json.RawMessage  `json:"egressProxy"`
	MaskRequestHost    string           `json:"maskRequestHost"`
	HTTPSPorts         []int            `json:"httpsPorts"`
	Rules              *json.RawMessage `json:"rules"`
}

func (n *e2bNetworkBody) unsupported() string {
	switch {
	case n == nil:
		return ""
	case len(n.EgressProxy) > 0 && string(n.EgressProxy) != "null":
		return "network.egressProxy is not supported"
	case n.MaskRequestHost != "":
		return "network.maskRequestHost is not supported"
	case len(n.HTTPSPorts) > 0:
		return "network.httpsPorts is not supported"
	case n.Rules != nil && string(*n.Rules) != "null" && string(*n.Rules) != "{}":
		return "network.rules is not supported"
	}
	return ""
}

func (n *e2bNetworkBody) policy(allowInternet *bool) *sandbox.Network {
	var out *sandbox.Network
	if n != nil && (len(n.AllowOut) > 0 || len(n.DenyOut) > 0 || n.DNSServers != nil) {
		out = &sandbox.Network{AllowOut: n.AllowOut, DenyOut: n.DenyOut, DNSServers: n.DNSServers}
	}
	if allowInternet != nil && !*allowInternet {
		if out == nil {
			out = &sandbox.Network{}
		}
		out.DenyOut = append(out.DenyOut, "0.0.0.0/0")
	}
	return out
}

type e2bCreateBody struct {
	TemplateID      string `json:"templateID"`
	Timeout         *int   `json:"timeout"`
	AutoPause       bool   `json:"autoPause"`
	AutoPauseMemory *bool  `json:"autoPauseMemory"`
	AutoResume      *struct {
		Enabled bool `json:"enabled"`
	} `json:"autoResume"`
	Secure              *bool             `json:"secure"`
	AllowInternetAccess *bool             `json:"allow_internet_access"`
	Network             *e2bNetworkBody   `json:"network"`
	Metadata            map[string]string `json:"metadata"`
	EnvVars             map[string]string `json:"envVars"`
	MCP                 json.RawMessage   `json:"mcp"`
	VolumeMounts        []any             `json:"volumeMounts"`
	// ProxCloud extensions: GPUs (E2B has none; usually a GPU template carries one) and size overrides within the
	// provider limits.
	GPU      *sandbox.GPURequest `json:"gpu"`
	CPUCount *int                `json:"cpuCount"`
	MemoryMB *int                `json:"memoryMB"`
}

// e2bReadyTimeout bounds how long create waits for envd. E2B's create returns a usable sandbox, so ours does too; a
// sandbox that does not start in time is removed. A var so tests can shorten it.
var e2bReadyTimeout = 50 * time.Second

// e2bRefusal is what a create asks for that this location does not offer ("" = nothing).
func (b *e2bCreateBody) e2bRefusal() string {
	switch {
	case b.TemplateID == "":
		return "templateID is required"
	case b.AutoPauseMemory != nil && *b.AutoPauseMemory:
		return errMemorySnapshot.msg // a filesystem pause instead would drop the processes the caller asked to keep
	case len(b.VolumeMounts) > 0:
		return "volumeMounts are not supported"
	case len(b.MCP) > 0 && string(b.MCP) != "null" && b.MCP[0] != '{':
		// The SDK starts the gateway inside the sandbox itself (template mcp-gateway); the API only checks the shape.
		// Never log or store it: it holds the tenant's MCP server keys.
		return "mcp must be an object"
	}
	return b.Network.unsupported()
}

func (h *Handler) e2bCreate(w http.ResponseWriter, r *http.Request) {
	c, arrived := e2bFrom(r), time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), SnapshotRequestWait+c.ss.Config().Startup.Duration()+time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	var b e2bCreateBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b); err != nil {
		e2bErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if msg := b.e2bRefusal(); msg != "" {
		e2bErr(w, http.StatusBadRequest, msg)
		return
	}
	// The pause checks (autoResume needs autoPause, both need this location to offer them) are createSandboxResource's,
	// shared with the native create; checking them here first keeps a refused create from waiting on the gates.
	if err := h.pauseRefusal(c.ss.Config(), &sandbox.Spec{AutoPause: b.AutoPause, AutoResume: b.AutoResume != nil && b.AutoResume.Enabled}); err != nil {
		e2bFail(w, err)
		return
	}
	// The native create's checks, in its order: suspension, the provider's state, the release gates, then the quota.
	if err := h.sandboxUsageGate(r.Context(), c.proj, c.es); err != nil {
		e2bFail(w, err)
		return
	}
	if err := h.awaitIfSnapshot(r.Context(), c, b.TemplateID, arrived.Add(SnapshotRequestWait)); err != nil {
		e2bFail(w, err)
		return
	}
	custom, customName, source, err := h.resolveRuntimeCreateTemplate(r.Context(), c.proj.ID, b.TemplateID)
	if errors.Is(err, sandbox.ErrBadTemplate) {
		e2bErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		e2bFail(w, err)
		return
	}
	spec := sandbox.Spec{
		ProjectID:     c.proj.ID,
		Template:      b.TemplateID,
		Custom:        custom,
		RuntimeSource: source,
		Env:           b.EnvVars,
		Metadata:      b.Metadata,
		GPU:           b.GPU,
		Network:       b.Network.policy(b.AllowInternetAccess),
		AutoPause:     b.AutoPause,
		AutoResume:    b.AutoResume != nil && b.AutoResume.Enabled,
	}
	if custom != nil {
		spec.Template = customName
	}
	spec.TimeoutSeconds = 300 // E2B's default; the SDKs always send one
	if b.Timeout != nil {
		spec.TimeoutSeconds = *b.Timeout
	}
	if b.Network != nil {
		spec.AllowPublicTraffic = b.Network.AllowPublicTraffic
	}
	if b.CPUCount != nil {
		spec.CPU = *b.CPUCount
	}
	if b.MemoryMB != nil {
		spec.MemoryGiB = (*b.MemoryMB + 1023) / 1024
	}
	if _, builtin := c.ss.Config().Templates[spec.Template]; custom == nil && !builtin {
		e2bErr(w, http.StatusBadRequest, h.unknownTemplate(r.Context(), c, b.TemplateID))
		return
	}
	q := c.es.SandboxProjectQuota(c.proj.Quota)
	spec.Quota = &q
	if err := spec.Validate(c.ss.Config()); err != nil {
		e2bErr(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := h.createSandboxResource(r.Context(), c.proj, c.es, c.ss, &spec)
	if err != nil {
		e2bFail(w, err)
		return
	}
	h.cloudResourceAudit(c.user, c.proj, "CLOUD_RESOURCE_CREATE", "E2B_CREATE", saved)
	a, err := e2bColdAccess(r.Context(), c.ss.Config().Startup.Duration(), time.Time{}, time.Now,
		func(ctx context.Context) error { return h.waitRuntimeCreateReady(ctx, c, saved) },
		func(ctx context.Context) (*sandbox.Access, error) {
			return committedStartupAccess(ctx, saved.Data, time.Now, func(ctx context.Context) (*sandbox.Access, error) { return c.ss.AccessFor(ctx, c.proj.ID, spec.ID) })
		})
	if err != nil {
		h.runtimeCreateFailure(r.Context(), c, saved.ExternalID)
		h.cleanupFailedStartup(r.Context(), c, saved)
		if errors.Is(err, errRuntimePinState) {
			e2bFail(w, err)
			return
		}
		msg := "sandbox startup failed; check template preparation and location availability"
		var startup *sandbox.StartupError
		if errors.As(err, &startup) {
			msg = startup.Error()
		}
		e2bErr(w, http.StatusServiceUnavailable, msg)
		return
	}
	e2bJSON(w, http.StatusCreated, c.e2bSandbox(spec.ID, spec.Template, a))
}

// e2bWaitReady polls until the sandbox is RUNNING (envd initialised: the pod's startup probe), then stores the live
// payload, so billing starts from runningSince even if nothing lists it.
func (h *Handler) e2bWaitReady(ctx context.Context, c *e2bCaller, cr *cloud.CloudResource) error {
	live, err := c.ss.WaitReady(ctx, c.proj.ID, cr.ExternalID)
	if err != nil {
		return err
	}
	if err := startupResponseAlive(ctx, live); err != nil {
		return err
	}
	cr.Data = live
	now := time.Now().UTC()
	cr.UpdatedAt = &now
	if _, err := h.cloud.Update(ctx, cr); err != nil {
		return err
	}
	return startupResponseAlive(ctx, live)
}

const (
	e2bCustomReadyTimeout = 120 * time.Second
	e2bReadyReturnGrace   = 5 * time.Second
)

// Only normal project builds get a cold-pull allowance. Snapshot/future kinds and
// unknown lifetimes keep the legacy bound; no caller field may select this budget.
func e2bCreateReadyBudget(custom bool, kind string, data map[string]any, now time.Time) (time.Duration, time.Time) {
	if !custom || kind != "" {
		return e2bReadyTimeout, time.Time{}
	}
	end, _ := time.Parse(time.RFC3339, fmt.Sprint(data["shutdownTime"]))
	if end.IsZero() {
		return e2bReadyTimeout, time.Time{}
	}
	return min(e2bCustomReadyTimeout, end.Sub(now)-e2bReadyReturnGrace), end
}

func e2bReadyAlive(ctx context.Context, expires, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !expires.IsZero() && !now.Before(expires) {
		return context.DeadlineExceeded
	}
	return nil
}

// One derived deadline covers readiness, cache work and the final live access read.
func e2bColdAccess(ctx context.Context, budget time.Duration, expires time.Time, now func() time.Time,
	wait func(context.Context) error, access func(context.Context) (*sandbox.Access, error),
) (*sandbox.Access, error) {
	readyBy := now().Add(budget)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if err := wait(ctx); err != nil {
		return nil, err
	}
	if err := e2bReadyAlive(ctx, expires, now()); err != nil {
		return nil, err
	}
	a, err := access(ctx)
	if err != nil {
		return nil, err
	}
	if err := e2bReadyAlive(ctx, expires, now()); err != nil {
		return nil, err
	}
	if !now().Before(readyBy) {
		return nil, context.DeadlineExceeded
	}
	return a, nil
}

func (h *Handler) e2bWaitReadyWithBudget(ctx context.Context, c *e2bCaller, cr *cloud.CloudResource, budget time.Duration, expires time.Time) error {
	return e2bPollReady(ctx, budget, expires, time.Now,
		func(ctx context.Context) (map[string]any, error) {
			return c.ss.GetSandbox(ctx, c.proj.ID, cr.ExternalID)
		},
		func(ctx context.Context, live map[string]any) {
			cr.Data = live
			now := time.Now().UTC()
			cr.UpdatedAt = &now
			if _, err := h.cloud.Update(ctx, cr); err != nil {
				slog.Warn("e2b: cache update after start", "sandbox", cr.ExternalID, "err", err)
			}
		})
}

// The per-call clock/read/store seam keeps deadline tests fast without mutable globals.
func e2bPollReady(ctx context.Context, budget time.Duration, expires time.Time, now func() time.Time,
	read func(context.Context) (map[string]any, error), store func(context.Context, map[string]any),
) error {
	readyBy := now().Add(budget)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	alive := func() error {
		at := now()
		if err := e2bReadyAlive(ctx, expires, at); err != nil {
			return err
		}
		if !at.Before(readyBy) {
			return context.DeadlineExceeded
		}
		return nil
	}
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		if err := alive(); err != nil {
			return err
		}
		live, err := read(ctx)
		if err := alive(); err != nil {
			return err
		}
		if err == nil && live["status"] == sandbox.StatusRunning {
			store(ctx, live)
			return alive()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// e2bList: GET /sandboxes and /v2/sandboxes. Filters: metadata (url-encoded k=v&k2=v2) and state (running, paused).
// Everything fits in one page (a project runs at most its quota of sandboxes).
func (h *Handler) e2bList(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	q := r.URL.Query()
	st := strings.Join(q["state"], ",")
	want, err := url.ParseQuery(q.Get("metadata"))
	if err != nil {
		e2bErr(w, http.StatusBadRequest, "invalid metadata filter")
		return
	}
	out := []any{}
	// A pausing sandbox still runs but lists once, as paused.
	paused := h.projectPaused(r.Context(), c.proj.ID)
	isPaused := make(map[string]bool, len(paused))
	for _, p := range paused {
		isPaused[p.ID] = true
	}
	if st == "" || strings.Contains(st, "running") {
		items, err := c.ss.ListSandboxes(r.Context(), c.proj.ID)
		if err != nil {
			e2bFail(w, err)
			return
		}
		ids := make([]string, 0, len(items))
		for id := range items {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			d := items[id]
			if d["status"] != sandbox.StatusRunning || isPaused[id] || !metadataMatches(d["metadata"], want) {
				continue
			}
			out = append(out, e2bListed(id, d))
		}
	}
	if st == "" || strings.Contains(st, "paused") {
		for i := range paused {
			if metadataMatches(sandbox.PayloadOf(paused[i].Saved)["metadata"], want) {
				out = append(out, pausedListed(&paused[i]))
			}
		}
	}
	w.Header().Set("X-Total-Running", fmt.Sprint(len(out)))
	e2bJSON(w, http.StatusOK, out)
}

func metadataMatches(md any, want url.Values) bool {
	m, _ := md.(map[string]string)
	for k := range want {
		if m == nil || m[k] != want.Get(k) {
			return false
		}
	}
	return true
}

// e2bLive returns the sandbox payload and its access, 404 for anything outside the project.
func (h *Handler) e2bLive(w http.ResponseWriter, r *http.Request) (string, map[string]any, *sandbox.Access, bool) {
	c := e2bFrom(r)
	id := chi.URLParam(r, "sandboxID")
	d, err := c.ss.GetSandbox(r.Context(), c.proj.ID, id)
	if err == nil {
		var a *sandbox.Access
		if a, err = c.ss.AccessFor(r.Context(), c.proj.ID, id); err == nil {
			return id, d, a, true
		}
	}
	e2bFail(w, err)
	return "", nil, nil, false
}

func (h *Handler) e2bGet(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	if p := h.loadPaused(r.Context(), c.proj.ID, chi.URLParam(r, "sandboxID")); p != nil {
		e2bJSON(w, http.StatusOK, pausedListed(p))
		return
	}
	id, d, a, ok := h.e2bLive(w, r)
	if !ok {
		return
	}
	out := e2bListed(id, d)
	out["envdAccessToken"] = a.EnvdAccessToken
	out["domain"] = c.domain()
	out["allowInternetAccess"] = a.Network.AllowsInternet()
	netw := map[string]any{"allowPublicTraffic": a.TrafficAccessToken == ""}
	if a.Network != nil {
		netw["allowOut"] = a.Network.AllowOut
		netw["denyOut"] = a.Network.DenyOut
	}
	out["network"] = netw
	e2bJSON(w, http.StatusOK, out)
}

// e2bRow finds the project's cache row for a sandbox (nil when the row is missing).
func (h *Handler) e2bRow(ctx context.Context, c *e2bCaller, id string) *cloud.CloudResource {
	cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, c.es.ID, id)
	if err != nil || cr == nil || cr.ProjectID != c.proj.ID {
		return nil
	}
	return cr
}

// e2bKill: DELETE /sandboxes/{id}. Open whatever the project's state, like the native delete.
func (h *Handler) e2bKill(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	id := chi.URLParam(r, "sandboxID")
	ctx := r.Context()
	var err error = errSandboxBusy
	for attempt := 0; attempt < 3 && errors.Is(err, errSandboxBusy); attempt++ {
		endedPause := false
		if p := h.loadPaused(ctx, c.proj.ID, id); p != nil {
			if err := h.killPaused(ctx, c, p); err != nil {
				e2bFail(w, err)
				return
			}
			endedPause = true
		}
		if _, gerr := c.ss.GetSandbox(ctx, c.proj.ID, id); gerr != nil {
			if endedPause { // a paused one; a resume that finished meanwhile left a running one, which the kill covers
				w.WriteHeader(http.StatusNoContent)
				return
			}
			e2bFail(w, gerr)
			return
		}
		err = h.killUnpaused(ctx, c, id)
	}
	if err != nil {
		e2bFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// killUnpaused deletes a running sandbox under its pause lock, so a pause whose capture is under way cannot record it
// after the kill (and bring it back as paused); errSandboxBusy when a pause recorded it meanwhile (the caller ends it
// first).
func (h *Handler) killUnpaused(ctx context.Context, c *e2bCaller, id string) error {
	return h.underPauseLock(ctx, c, id, func() error {
		if cr := h.e2bRow(ctx, c, id); cr != nil {
			if err := h.sandboxDelete(ctx, c.es, c.proj, cr); err != nil {
				return err
			}
			h.cloudResourceAudit(c.user, c.proj, "CLOUD_RESOURCE_DELETE", "E2B_KILL", cr)
			return nil
		}
		return c.ss.DeleteSandbox(ctx, c.proj.ID, id)
	})
}

// sandboxKill is the native delete of a live sandbox row: like the E2B kill it ends a pause under way first, then
// deletes the sandbox under its pause lock, so the pause can neither record it after the delete nor finish it as
// paused.
func (h *Handler) sandboxKill(ctx context.Context, u *user.User, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource) error {
	kill := func() error { return h.sandboxDelete(ctx, es, proj, cr) }
	if h.sbPaused == nil || h.sandboxFor == nil || cr.Data["status"] == sandbox.StatusDeleted {
		return kill()
	}
	ss, err := h.sandboxFor(es)
	if err != nil {
		return err
	}
	c := &e2bCaller{user: u, proj: proj, es: es, ss: ss}
	err = errSandboxBusy
	for attempt := 0; attempt < 3 && errors.Is(err, errSandboxBusy); attempt++ {
		if p := h.loadPaused(ctx, proj.ID, cr.ExternalID); p != nil {
			if err = h.killPaused(ctx, c, p); err != nil {
				return err
			}
		}
		err = h.underPauseLock(ctx, c, cr.ExternalID, kill)
	}
	return err
}

// underPauseLock runs kill under the sandbox's pause lock; errSandboxBusy when the project has a paused record for it.
func (h *Handler) underPauseLock(ctx context.Context, c *e2bCaller, id string, kill func() error) error {
	if h.sbPaused == nil {
		return kill()
	}
	return h.sbPaused.DB().WithTx(context.WithoutCancel(ctx), func(tc context.Context) error {
		if err := h.lockPaused(tc, id); err != nil {
			return err
		}
		var p pausedSandbox
		if found, err := h.sbPaused.GetForUpdate(tc, id, &p); err != nil {
			return err
		} else if found && p.ProjectID == c.proj.ID {
			return errSandboxBusy
		}
		return kill()
	})
}

func decodeE2B(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		e2bErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

// e2bSetTimeout moves the sandbox's end to now+seconds. Like the native SET_TIMEOUT it passes the usage gate first:
// a DISABLED project, a suspended profile, a DISABLED provider or a failing release gate refuses it.
func (h *Handler) e2bSetTimeout(w http.ResponseWriter, r *http.Request, seconds int) bool {
	c := e2bFrom(r)
	if err := h.sandboxUsageGate(r.Context(), c.proj, c.es); err != nil {
		e2bFail(w, err)
		return false
	}
	id := chi.URLParam(r, "sandboxID")
	live, err := c.ss.SetTimeout(r.Context(), c.proj.ID, id, seconds)
	if errors.Is(err, sandbox.ErrBadTimeout) {
		e2bErr(w, http.StatusBadRequest, err.Error())
		return false
	}
	if err != nil {
		e2bFail(w, err)
		return false
	}
	if cr := h.e2bRow(r.Context(), c, id); cr != nil {
		cr.Data = live
		now := time.Now().UTC()
		cr.UpdatedAt = &now
		if _, err := h.cloud.Update(r.Context(), cr); err != nil {
			slog.Warn("e2b: cache update after timeout", "sandbox", id, "err", err)
		}
	}
	return true
}

// e2bTimeout: POST /sandboxes/{id}/timeout {timeout}: the sandbox ends timeout seconds from now.
func (h *Handler) e2bTimeout(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Timeout int `json:"timeout"`
	}
	if !decodeE2B(w, r, &b) {
		return
	}
	if h.e2bSetTimeout(w, r, b.Timeout) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// e2bRefresh: POST /sandboxes/{id}/refreshes {duration}: keep-alive, never shortens.
func (h *Handler) e2bRefresh(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Duration int `json:"duration"`
	}
	if !decodeE2B(w, r, &b) {
		return
	}
	if b.Duration <= 0 {
		b.Duration = 60
	}
	if h.e2bExtendTo(w, r, b.Duration) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// e2bExtendTo moves the end to now+seconds only when that is later than the current end (and only then passes the
// usage gate: a call that changes nothing adds no usage).
func (h *Handler) e2bExtendTo(w http.ResponseWriter, r *http.Request, seconds int) bool {
	c := e2bFrom(r)
	d, err := c.ss.GetSandbox(r.Context(), c.proj.ID, chi.URLParam(r, "sandboxID"))
	if err != nil {
		e2bFail(w, err)
		return false
	}
	if end, err := time.Parse(time.RFC3339, fmt.Sprint(d["shutdownTime"])); err == nil &&
		!time.Now().Add(time.Duration(seconds)*time.Second).After(end) {
		return true
	}
	return h.e2bSetTimeout(w, r, seconds)
}

// e2bConnect: POST /sandboxes/{id}/connect {timeout}: the Sandbox (with its tokens), its life extended to at least
// timeout seconds. A paused sandbox is resumed (201), always as a cold boot: memory: false (E2B's reboot) and the
// default restore read the same here, since no pause keeps memory.
func (h *Handler) e2bConnect(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	var b struct {
		Timeout *int `json:"timeout"`
	}
	if !decodeE2B(w, r, &b) {
		return
	}
	if p := h.loadPaused(r.Context(), c.proj.ID, chi.URLParam(r, "sandboxID")); p != nil {
		timeout := 300
		if b.Timeout != nil {
			timeout = *b.Timeout
		}
		sb, err := h.resumePaused(r.Context(), c, p, timeout)
		if err != nil {
			e2bFail(w, err)
			return
		}
		e2bJSON(w, http.StatusCreated, sb)
		return
	}
	if b.Timeout != nil && !h.e2bExtendTo(w, r, *b.Timeout) {
		return
	}
	id, d, a, ok := h.e2bLive(w, r)
	if !ok {
		return
	}
	tpl, _ := d["template"].(string)
	e2bJSON(w, http.StatusOK, c.e2bSandbox(id, tpl, a))
}

// e2bNetwork: PUT /sandboxes/{id}/network: replaces the egress rules (omitted = cleared). Not a read and lowers no
// usage, so a DISABLED or deleting project refuses it, as the native allow-list (allowedWhileDisabled) would.
func (h *Handler) e2bNetwork(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	if err := refuseNewUsage(c.proj); err != nil {
		e2bFail(w, err)
		return
	}
	var b e2bNetworkBody
	if !decodeE2B(w, r, &b) {
		return
	}
	if msg := b.unsupported(); msg != "" {
		e2bErr(w, http.StatusBadRequest, msg)
		return
	}
	if b.AllowPublicTraffic != nil {
		e2bErr(w, http.StatusBadRequest, "allowPublicTraffic can only be set when the sandbox is created")
		return
	}
	n := b.policy(nil)
	if err := n.Validate(); err != nil {
		e2bErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := c.ss.SetNetwork(r.Context(), c.proj.ID, chi.URLParam(r, "sandboxID"), n); err != nil {
		e2bFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// e2bLogs: the sandbox's stdout/stderr (envd and the start command), the newest 1000 lines.
func (h *Handler) e2bLogs(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	text, err := c.ss.Logs(r.Context(), c.proj.ID, chi.URLParam(r, "sandboxID"), 1000)
	if err != nil {
		e2bFail(w, err)
		return
	}
	logs := []any{}
	entries := []any{}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		ts, msg, _ := strings.Cut(line, " ")
		logs = append(logs, map[string]any{"timestamp": ts, "line": msg})
		entries = append(entries, map[string]any{"timestamp": ts, "message": msg, "level": "info", "fields": map[string]any{}})
	}
	e2bJSON(w, http.StatusOK, map[string]any{"logs": logs, "logEntries": entries})
}

// Metrics are not collected per sandbox: empty, which the SDKs treat as "no samples".
func (h *Handler) e2bMetrics(w http.ResponseWriter, r *http.Request) {
	if _, _, _, ok := h.e2bLive(w, r); ok {
		e2bJSON(w, http.StatusOK, []any{})
	}
}

func (h *Handler) e2bMetricsMany(w http.ResponseWriter, _ *http.Request) {
	e2bJSON(w, http.StatusOK, map[string]any{"sandboxes": map[string]any{}})
}

// ---- templates: the project's own (e2b_templates.go) and the built-in ones ------------------------

func e2bTemplate(name string, t sandbox.Template) map[string]any {
	return map[string]any{
		"templateID":  name,
		"buildID":     name,
		"cpuCount":    t.CPU,
		"memoryMB":    t.MemoryGiB * 1024,
		"diskSizeMB":  0,
		"public":      true,
		"aliases":     []string{name},
		"names":       []string{name},
		"envdVersion": sandbox.EnvdVersion,
		"buildStatus": "ready",
		"buildCount":  1,
		"spawnCount":  0,
		"createdAt":   time.Time{}.Format(time.RFC3339),
		"updatedAt":   time.Time{}.Format(time.RFC3339),
		"createdBy":   nil,
	}
}

func (h *Handler) e2bTemplates(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	cfg := c.ss.Config()
	out := []any{}
	for _, t := range h.projectTemplates(r.Context(), c.proj.ID) {
		out = append(out, t.e2bView())
	}
	for _, name := range cfg.TemplateNames() {
		out = append(out, e2bTemplate(name, cfg.Templates[name]))
	}
	e2bJSON(w, http.StatusOK, out)
}

// projectTemplate is the caller's project template by id or name (nil when there is none).
func (h *Handler) projectTemplate(r *http.Request, idOrName string) *sandboxTemplateDoc {
	if h.sbTemplates == nil {
		return nil
	}
	name, _ := splitNameTag(idOrName)
	t, _ := h.loadTemplate(r.Context(), e2bFrom(r).proj.ID, name)
	return t
}

func (h *Handler) e2bTemplateAlias(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "alias")
	if t := h.projectTemplate(r, name); t != nil {
		e2bJSON(w, http.StatusOK, map[string]any{"templateID": t.ID, "public": false})
		return
	}
	if _, ok := e2bFrom(r).ss.Config().Templates[name]; !ok {
		e2bErr(w, http.StatusNotFound, fmt.Sprintf("template %q not found", name))
		return
	}
	e2bJSON(w, http.StatusOK, map[string]any{"templateID": name, "public": true})
}

func (h *Handler) e2bTemplateGet(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "templateID")
	if pt := h.projectTemplate(r, name); pt != nil {
		v := pt.e2bView()
		builds := []any{}
		for _, b := range pt.Builds {
			builds = append(builds, map[string]any{"buildID": b.BuildID, "status": buildPinSDKStatus(&b), "cpuCount": pt.CPU, "memoryMB": pt.MemoryGiB * 1024,
				"createdAt": b.CreatedAt, "finishedAt": b.FinishedAt})
		}
		v["builds"] = builds
		e2bJSON(w, http.StatusOK, v)
		return
	}
	t, ok := e2bFrom(r).ss.Config().Templates[name]
	if !ok {
		e2bErr(w, http.StatusNotFound, fmt.Sprintf("template %q not found", name))
		return
	}
	e2bJSON(w, http.StatusOK, map[string]any{
		"templateID": name, "public": true, "aliases": []string{name}, "names": []string{name},
		"builds": []any{map[string]any{"buildID": name, "status": "ready", "cpuCount": t.CPU, "memoryMB": t.MemoryGiB * 1024}},
	})
}
