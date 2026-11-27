package project

// cloud_function.go — serverless functions: a customer's container image behind a stable internal HTTPS URL. They run
// on a sandbox provider whose config.sandbox.functions block is enabled (internal/cloud/function drives the cluster).
//
//	POST   /api/v1/project/{id}/functions                    create (and deploy)
//	GET    /api/v1/project/{id}/functions                    list
//	GET    /api/v1/project/{id}/functions/{fn}               one, with the rollout reason
//	PATCH  /api/v1/project/{id}/functions/{fn}               change it; image, size, env, secrets, registryAuth, port, command: a new deployment
//	DELETE /api/v1/project/{id}/functions/{fn}
//	GET    /api/v1/project/{id}/functions/{fn}/deployments   newest first
//	POST   /api/v1/project/{id}/functions/{fn}/rollback      {deploymentId, registryAuth?}
//	POST   /api/v1/project/{id}/functions/{fn}/action        {action: suspend|resume}
//	GET    /api/v1/project/{id}/functions/{fn}/logs?tail=
//
// A function runs an image the customer names. Nothing else is offered: a request that names a GPU, a source to build
// from, a public exposure, a rate limit or a hostname of its own is refused (400), never taken with the field ignored.
//
// Records: `functions` (one per function: config, host, scale, activity), `function_deployments` (one per rollout,
// immutable but for its state and the digest it ran) and `function_hosts` (every host ever issued, never deleted: a
// URL is not handed out twice). Every function also has a FUNCTION cloud resource row, a projection of its record:
// what the generic resource list shows and what billing reads. The record and its row change in one transaction.
//
// Secrets are env the API takes and never gives back: sealed in the function record (functionDoc.Secrets) with the
// portal's encryption key, handed to the instances through a Secret on the cluster, and visible everywhere else
// (answers, deployment records, cloud rows, audit events, logs) by name only. The password of a private registry
// (registryAuth) is kept the same way and written to one place only, the function's image pull secret on the cluster.
//
// Where functions are off (the provider's block is off, or the provider is disabled) reads and DELETE stay open, so
// what is left can be seen and removed; everything else answers 403.
//
// The deploy flow is in cloud_function_deploy.go; the scale, the cloud row and teardown are in
// cloud_function_scale.go.

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/function"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// SetFunctions wires the function service factory and the functions, function_deployments and function_hosts
// collections (unset: every function route answers 503). keys seals the secret values and registry passwords in the
// functions records; nil (the portal has no encryption key) refuses every write of one and stores nothing.
func (h *Handler) SetFunctions(f func(es *externalservice.ExternalService) (*function.Service, error), fns, deployments, hosts *pgdoc.Store, keys *textcrypt.BlobKeys) {
	h.functionFor, h.fns, h.fnDeps, h.fnHosts, h.fnKeys = f, fns, deployments, hosts, keys
}

// EnsureFunctionIndexes creates the collections and their indexes (startup; idempotent). The unique ones are the
// backstop behind the create checks: a name is unique in its project and a host is never handed to two functions.
func (h *Handler) EnsureFunctionIndexes(ctx context.Context) error {
	for _, s := range []*pgdoc.Store{h.fns, h.fnDeps, h.fnHosts} {
		if err := s.Ensure(ctx); err != nil {
			return err
		}
	}
	if err := h.fns.EnsureIndex(ctx, "project_name_unique", true, pgdoc.F("projectId"), pgdoc.F("name")); err != nil {
		return err
	}
	if err := h.fns.EnsureIndex(ctx, "host_unique", true, pgdoc.F("host")); err != nil {
		return err
	}
	return h.fnDeps.EnsureIndex(ctx, "functionId", false, pgdoc.F("functionId"))
}

// functionDoc is a `functions` record. The embedded spec is what is serving: its snapshot part changes only when a
// deployment becomes active, so it is also what a failed rollout goes back to.
type functionDoc struct {
	ID        string `json:"id"` // fn-<16 hex>: record id and Deployment name
	ProjectID string `json:"projectId"`
	ServiceID string `json:"serviceId"`
	Region    string `json:"region"`
	Host      string `json:"host"` // <name>-<6 hex>.<domain>
	function.Spec
	// Secrets are the secret values that are serving, by name; Spec.SecretNames and Spec.SecretsVersion describe
	// them. PendingSecrets is the whole set a rollout in progress switches to, when it changes them: adopted with its
	// snapshot, dropped when it fails. Neither is ever rendered (functionView has no field for them).
	Secrets        map[string]fnSecret `json:"secrets,omitempty"`
	PendingSecrets map[string]fnSecret `json:"pendingSecrets,omitempty"`
	// RegistryPassword is the password of the registry credentials that are serving (Spec.RegistryAuth describes
	// them), PendingRegistryPassword the one a rollout in progress switches to: the same life as Secrets and
	// PendingSecrets, and as little ever rendered.
	RegistryPassword        *fnSecret `json:"registryPassword,omitempty"`
	PendingRegistryPassword *fnSecret `json:"pendingRegistryPassword,omitempty"`
	// CurrentDeploymentID is the deployment that is serving ("" until the first one succeeds).
	CurrentDeploymentID string     `json:"currentDeploymentId,omitempty"`
	CurrentSeq          int        `json:"currentSeq,omitempty"`
	DeployedAt          *time.Time `json:"deployedAt,omitempty"`
	// DeployingID is the deployment being rolled out. It is also the claim that keeps rollouts one at a time; absent
	// when none is in progress.
	DeployingID string `json:"deployingId,omitempty"`
	DeploySeq   int    `json:"deploySeq"`           // sequence number of the newest deployment
	LastError   string `json:"lastError,omitempty"` // why the newest deployment failed
	Suspended   bool   `json:"suspended"`
	// Instances is the desired instance count as the portal last set it (scaleFunction is the only writer).
	// RunningSince / StoppedAt bound the current or last running interval, for billing; LastInstances is the count it
	// last ran with, which the end of an interval is billed at. Ended are the intervals that ended lately, the last
	// one included (endedIntervals).
	Instances     int        `json:"instances"`
	LastInstances int        `json:"lastInstances,omitempty"`
	LastActiveAt  *time.Time `json:"lastActiveAt,omitempty"`
	RunningSince  *time.Time `json:"runningSince,omitempty"`
	StoppedAt     *time.Time `json:"stoppedAt,omitempty"`
	Ended         []fnEnded  `json:"endedIntervals,omitempty"`
	CreatedAt     *time.Time `json:"createdAt,omitempty"`
	UpdatedAt     *time.Time `json:"updatedAt,omitempty"`
}

// fnEnded is a running interval that has ended, and the instance count and size it ran with last: what it is billed
// at (billedSize, at the stop), whatever is deployed after it.
type fnEnded struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Instances int       `json:"instances"`
	CPUMillis int       `json:"cpuMillis,omitempty"`
	MemoryMi  int       `json:"memoryMi,omitempty"`
}

// fnSecret is one stored secret: its value sealed with the portal's key (KeyID names it; Sealed is base64 of the
// blob), and when it was last set. The blob opens only as what it was sealed for: this project, this function, this
// name (fnSecretAAD).
type fnSecret struct {
	KeyID  string    `json:"keyId"`
	Sealed string    `json:"sealed"`
	SetAt  time.Time `json:"setAt"`
}

// fnRegistryPasswordName is what a registry password is sealed as. It is no name a secret can have (a secret's name
// is an environment variable's: letters, digits and '_'), so the two never open as each other.
const fnRegistryPasswordName = "registry-password"

// fnSecretAAD binds a sealed value to its place: a blob copied to another project, function or name does not open.
func fnSecretAAD(projectID, functionID, name string) []byte {
	return []byte("function/" + projectID + "/" + functionID + "/" + name)
}

// errNoFunctionKey refuses every write of a secret or a registry password where the portal has no encryption key:
// the value would be stored as it was sent.
var errNoFunctionKey = httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable,
	"functions are not configured: the portal has no encryption key to protect secrets and registry passwords with. Ask the operator to set an encryption key of 32 characters or more")

// errFunctionSealed: a stored value does not open with the portal's keys (the key was changed or removed, or the
// record was tampered with). Nothing is deployed without the values it was configured with.
var errFunctionSealed = httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable,
	"the function's secrets cannot be opened with the portal's encryption key: set them again, or ask the operator")

// sealValue seals one value for the function's record.
func (h *Handler) sealValue(fn *functionDoc, name, value string, now time.Time) (fnSecret, error) {
	if h.fnKeys == nil {
		return fnSecret{}, errNoFunctionKey
	}
	keyID, blob, err := h.fnKeys.Seal([]byte(value), fnSecretAAD(fn.ProjectID, fn.ID, name))
	if err != nil {
		return fnSecret{}, errNoFunctionKey
	}
	return fnSecret{KeyID: keyID, Sealed: base64.StdEncoding.EncodeToString(blob), SetAt: now}, nil
}

// openValue is the value of one sealed record of the function. It fails closed: without the key, or for a blob that
// is not this project's, this function's and this name's, there is no value.
func (h *Handler) openValue(fn *functionDoc, name string, s fnSecret) (string, error) {
	blob, err := base64.StdEncoding.DecodeString(s.Sealed)
	if err != nil || h.fnKeys == nil {
		return "", errFunctionSealed
	}
	plain, err := h.fnKeys.Open(s.KeyID, blob, fnSecretAAD(fn.ProjectID, fn.ID, name))
	if err != nil {
		return "", errFunctionSealed
	}
	return string(plain), nil
}

// mergeSecrets applies the secrets of a request to a function's (cur, in the clear): a string sets or replaces that
// name, null removes it, names not given stay. It reports whether the request changed anything. Setting a name is
// always a change, also to the value it has: comparing would tell the caller whether a guess is right. The result is
// for validating and never kept.
func mergeSecrets(cur map[string]string, given map[string]*string) (map[string]string, bool) {
	out, changed := maps.Clone(cur), false
	if out == nil {
		out = map[string]string{}
	}
	for name, v := range given {
		if _, has := out[name]; v == nil {
			changed = changed || has
			delete(out, name)
			continue
		}
		out[name], changed = *v, true
	}
	return out, changed
}

// sealSecrets is the stored form of the same merge: names that stay keep their record, names that are set are sealed
// and stamped. Called once the merged set is known to be valid.
func (h *Handler) sealSecrets(fn *functionDoc, given map[string]*string, now time.Time) (map[string]fnSecret, error) {
	out := maps.Clone(fn.Secrets)
	if out == nil {
		out = map[string]fnSecret{}
	}
	for name, v := range given {
		if v == nil {
			delete(out, name)
			continue
		}
		s, err := h.sealValue(fn, name, *v, now)
		if err != nil {
			return nil, err
		}
		out[name] = s
	}
	return out, nil
}

// openSecrets are the values of a stored secret set, in the clear.
func (h *Handler) openSecrets(fn *functionDoc, set map[string]fnSecret) (map[string]string, error) {
	out := make(map[string]string, len(set))
	for name, s := range set {
		v, err := h.openValue(fn, name, s)
		if err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, nil
}

// describeSecrets writes onto a snapshot what it may know of a new secret set: the names, and a new version.
func describeSecrets(snap *function.Snapshot, secrets map[string]string) {
	snap.SecretNames, snap.SecretsVersion = nil, ""
	if len(secrets) > 0 {
		snap.SecretNames, snap.SecretsVersion = slices.Sorted(maps.Keys(secrets)), pgdoc.NewID()
	}
}

// registryAuthReq is registryAuth as a request gives it. The password is required whenever the credentials are set:
// it cannot be read back, so there is nothing to keep it from.
type registryAuthReq struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// maxRegistryPassword fits the longest registry passwords in use (a cloud provider's JSON key).
const maxRegistryPassword = 16 << 10

// describe is what a snapshot may know of the credentials (a new version: setting them is always a change, as for a
// secret), or the reason they are refused. Never the password.
func (r *registryAuthReq) describe() (*function.RegistryAuth, error) {
	if r.Password == "" || len(r.Password) > maxRegistryPassword {
		return nil, errors.New("registryAuth.password is required whenever registryAuth is set (it is write-only and cannot be read back)")
	}
	return &function.RegistryAuth{Server: strings.TrimSpace(r.Server), Username: r.Username, Version: pgdoc.NewID()}, nil
}

// registryAuthChange applies what a request says of registryAuth to the snapshot it rolls out, which has the
// function's credentials: not given keeps them, null (req nil) removes them, an object sets or replaces them. It
// returns the password of new ones, sealed for the record (nil: the rollout runs with the serving one).
func (h *Handler) registryAuthChange(fn *functionDoc, snap *function.Snapshot, given bool, req *registryAuthReq) (*fnSecret, error) {
	if !given {
		return nil, nil
	}
	if snap.RegistryAuth = nil; req == nil {
		return nil, nil
	}
	ra, err := req.describe()
	if err != nil {
		return nil, httpx.BadRequest(err.Error())
	}
	sealed, err := h.sealValue(fn, fnRegistryPasswordName, req.Password, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	snap.RegistryAuth = ra
	return &sealed, nil
}

// registryVersion is the version marker of a snapshot's registry credentials, "" without any.
func registryVersion(ra *function.RegistryAuth) string {
	if ra == nil {
		return ""
	}
	return ra.Version
}

// identity is the function as its Deployment carries it.
func (f *functionDoc) identity() function.Function {
	return function.Function{ID: f.ID, ProjectID: f.ProjectID, Host: f.Host, Suspended: f.Suspended}
}

// Deployment states.
const (
	depDeploying  = "DEPLOYING"
	depActive     = "ACTIVE"
	depSuperseded = "SUPERSEDED"
	depFailed     = "FAILED"
)

// functionDeployment is a `function_deployments` record: one rollout of one config snapshot.
type functionDeployment struct {
	ID         string `json:"id"`
	FunctionID string `json:"functionId"`
	ProjectID  string `json:"projectId"`
	Seq        int    `json:"seq"`
	function.Snapshot
	// ImageDigest is the digest ("sha256:…") the deployment's first ready instance ran, recorded when it became
	// ACTIVE: what a rollback to it deploys, so that a rollback runs the bits that ran and not what a tag has moved
	// to since. Absent when the runtime told none.
	ImageDigest string     `json:"imageDigest,omitempty"`
	RollbackOf  int        `json:"rollbackOf,omitempty"` // seq of the deployment this one re-applies
	State       string     `json:"state"`
	Message     string     `json:"message,omitempty"` // why it failed: one of function.Reasons, or the portal's own words
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
}

// Function statuses of the API.
const (
	fnDeploying = "DEPLOYING"
	fnReady     = "READY"
	fnIdle      = "IDLE"
	fnSuspended = "SUSPENDED"
	fnFailed    = "FAILED"
)

// functionStatus derives the API status. ready is the live number of ready instances. FAILED means nothing healthy
// was ever deployed; a failed later deployment leaves the function on its previous one (READY/IDLE) with lastError
// set.
func functionStatus(fn *functionDoc, ready int) string {
	switch {
	case fn.Suspended:
		return fnSuspended
	case fn.DeployingID != "":
		return fnDeploying
	case fn.CurrentDeploymentID == "":
		return fnFailed
	case ready > 0:
		return fnReady
	}
	return fnIdle
}

type functionDeploymentRef struct {
	ID         string     `json:"id"`
	Seq        int        `json:"seq"`
	Image      string     `json:"image"`
	DeployedAt *time.Time `json:"deployedAt,omitempty"`
}

// registryAuthView is a function's registry credentials as the API shows them: no password.
type registryAuthView struct {
	Server   string    `json:"server"`
	Username string    `json:"username"`
	SetAt    time.Time `json:"setAt"`
}

// functionView is a function as the API answers it. It is a type of its own with no field for a sealed value: what
// the record holds of secrets and passwords cannot be rendered by mistake.
type functionView struct {
	ID string `json:"id"`
	function.Spec
	Host      string `json:"host"`
	URL       string `json:"url"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"` // what an unfinished rollout or start is waiting for
	Instances int    `json:"instances"`        // ready instances, live
	Suspended bool   `json:"suspended"`
	LastError string `json:"lastError,omitempty"`
	// SecretsSetAt says when each secret (Spec.SecretNames) was last set. Values are never shown.
	SecretsSetAt map[string]time.Time `json:"secretsSetAt,omitempty"`
	// RegistryAuth takes the place of the spec's: it says when the password was set, and has no use for the version
	// marker.
	RegistryAuth *registryAuthView `json:"registryAuth,omitempty"`
	// CurrentDeployment is the deployment that is serving; absent until the first one succeeds.
	CurrentDeployment *functionDeploymentRef `json:"currentDeployment,omitempty"`
	LastActiveAt      *time.Time             `json:"lastActiveAt,omitempty"`
	CreatedAt         *time.Time             `json:"createdAt,omitempty"`
	UpdatedAt         *time.Time             `json:"updatedAt,omitempty"`
}

// fnCaller is an authenticated function request: who, which project, and the location the request is about (the
// project's functions location, or the one the function named in the URL lives on).
type fnCaller struct {
	u    *user.User
	proj *Project
	es   *externalservice.ExternalService
	fs   *function.Service
	// on: the location offers functions (functionsOffered). Off, only reads and DELETE are answered.
	on bool
	// manage: the caller may change functions, so may see env values. Others get the names only (env is where
	// customers put what should have been a secret).
	manage bool
}

func (c *fnCaller) env(env map[string]string) map[string]string {
	if c.manage || env == nil {
		return env
	}
	out := make(map[string]string, len(env))
	for k := range env {
		out[k] = ""
	}
	return out
}

func (c *fnCaller) view(fn *functionDoc, ready int) functionView {
	v := functionView{
		ID: fn.ID, Spec: fn.Spec, Host: fn.Host, URL: "https://" + fn.Host,
		Status: functionStatus(fn, ready), Instances: ready, Suspended: fn.Suspended, LastError: fn.LastError,
		LastActiveAt: fn.LastActiveAt, CreatedAt: fn.CreatedAt, UpdatedAt: fn.UpdatedAt,
	}
	v.Env = c.env(fn.Env)
	v.MaxInstances, v.Concurrency = max(v.MaxInstances, 1), cmp.Or(v.Concurrency, function.DefaultConcurrency)
	for name, s := range fn.Secrets {
		if v.SecretsSetAt == nil {
			v.SecretsSetAt = map[string]time.Time{}
		}
		v.SecretsSetAt[name] = s.SetAt
	}
	if fn.RegistryAuth != nil && fn.RegistryPassword != nil {
		v.RegistryAuth = &registryAuthView{Server: fn.RegistryAuth.Server, Username: fn.RegistryAuth.Username, SetAt: fn.RegistryPassword.SetAt}
	}
	if fn.CurrentDeploymentID != "" {
		v.CurrentDeployment = &functionDeploymentRef{ID: fn.CurrentDeploymentID, Seq: fn.CurrentSeq, Image: fn.Image, DeployedAt: fn.DeployedAt}
	}
	return v
}

// functionSvc builds the function service of one location (a sandbox provider), whether or not the location offers
// functions (functionsOffered): what already runs there must stay stoppable and deletable. ErrDisabled when the
// service is not a sandbox provider.
func (h *Handler) functionSvc(ctx context.Context, serviceID string) (*externalservice.ExternalService, *function.Service, error) {
	es, err := h.esSvc.Get(ctx, serviceID)
	if err != nil {
		return nil, nil, err
	}
	if es == nil || !es.IsSandbox() {
		return nil, nil, function.ErrDisabled
	}
	fs, err := h.functionFor(es)
	return es, fs, err
}

// functionsOffered reports whether a location takes function work: its provider is live and its functions block is
// on and valid. Where it is not, reads and DELETE stay open and everything else is refused (403); the functions
// themselves stay, and come back when the location offers functions again.
func functionsOffered(es *externalservice.ExternalService, fs *function.Service) bool {
	return es != nil && fs != nil && !es.IsDisabled() && fs.Enabled()
}

// errFunctionsOff is the off rule's answer to everything but a read and a DELETE.
var errFunctionsOff = httpx.NewError(http.StatusForbidden, http.StatusForbidden, "functions are not offered here")

// functionCaller authenticates the request, checks the project permission and resolves the project's functions
// location: its first sandbox provider that offers functions (none: c.on stays false and c.fs nil). 503 where the
// portal has no function service at all.
func (h *Handler) functionCaller(w http.ResponseWriter, r *http.Request, perm string) (*fnCaller, bool) {
	u, ok := h.principal(w, r)
	if !ok {
		return nil, false
	}
	proj, ok := h.project(w, r, u, perm)
	if !ok {
		return nil, false
	}
	if h.functionFor == nil || h.fns == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "functions are not configured")
		return nil, false
	}
	c := &fnCaller{u: u, proj: proj}
	c.manage = perm == rbac.ProjectCloudResourceManage ||
		h.policy.RequireProjectPermission(r.Context(), u.Sub, proj, rbac.ProjectCloudResourceManage) == nil
	for _, id := range proj.ServiceIDs() {
		es, fs, err := h.functionSvc(r.Context(), id)
		if err == nil && functionsOffered(es, fs) {
			c.es, c.fs, c.on = es, fs, true
			break
		}
		if err != nil && !errors.Is(err, function.ErrDisabled) {
			slog.Error("function: build service", "service", id, "err", err)
		}
	}
	return c, true
}

// offered answers 403 unless the caller's location offers functions.
func (c *fnCaller) offered(w http.ResponseWriter) bool {
	if !c.on {
		httpx.WriteError(w, errFunctionsOff)
	}
	return c.on
}

// functionOf loads the function named in the URL, 404 unless it is this project's, and switches the caller to the
// location the function lives on, offered or not.
func (h *Handler) functionOf(w http.ResponseWriter, r *http.Request, c *fnCaller) (*functionDoc, bool) {
	var fn functionDoc
	id := chi.URLParam(r, "fn")
	if function.ValidID(id) {
		found, err := h.fns.Get(r.Context(), id, &fn)
		if err != nil {
			h.fail(w, err)
			return nil, false
		}
		if found && fn.ProjectID == c.proj.ID {
			es, fs, err := h.functionSvc(r.Context(), fn.ServiceID)
			if err != nil {
				slog.Error("function: build service", "service", fn.ServiceID, "function", fn.ID, "err", err)
				httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "functions are not available")
				return nil, false
			}
			c.es, c.fs, c.on = es, fs, functionsOffered(es, fs)
			return &fn, true
		}
	}
	httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "function not found")
	return nil, false
}

// errFunctionBusy: one rollout at a time.
var errFunctionBusy = errors.New("a deployment of this function is already in progress")

// errFunctionsUnavailable is what a tenant sees while a release gate of the location does not hold: the reason is
// the operator's.
var errFunctionsUnavailable = httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable,
	"Functions are not available on this location right now. Try again later or contact your platform operator.")

// functionUsageGate is what everything that adds usage passes (create, a deployment, a rollback, a resume): the
// project can be billed (sandboxBillingGate) and the location's release gates hold, as for a sandbox.
func (h *Handler) functionUsageGate(ctx context.Context, c *fnCaller) error {
	if err := h.sandboxBillingGate(ctx, c.proj); err != nil {
		return err
	}
	if err := h.sandboxReleaseGate(ctx, c.es); err != nil {
		slog.Error("function: release gates do not hold, usage refused", "service", c.es.ID, "project", c.proj.ID, "err", err)
		return errFunctionsUnavailable
	}
	return nil
}

func (h *Handler) functionFail(w http.ResponseWriter, err error) {
	if status, msg, ok := functionHTTPError(err); ok {
		if status == http.StatusServiceUnavailable {
			slog.Error("function: cluster error", "err", err)
		}
		httpx.Err(w, status, status, msg)
		return
	}
	h.fail(w, err)
}

// errFunctionQuota is a refusal by the project's function quota (429).
type errFunctionQuota string

func (e errFunctionQuota) Error() string { return string(e) }

// functionHTTPError is the function error contract, in the style of sandboxHTTPError: billing refusals 403, a
// missing function 404, a rollout in progress 409, the project's quota 429, and the cluster itself 503. Kubernetes'
// own words never reach the caller: they name namespaces, service accounts and nodes.
func functionHTTPError(err error) (status int, msg string, ok bool) {
	var forbidden sandboxForbidden
	var quota errFunctionQuota
	var kerr *kamajik8s.APIError
	switch {
	case errors.As(err, &forbidden):
		return http.StatusForbidden, "Cannot run the function: " + err.Error(), true
	case errors.As(err, &quota):
		return http.StatusTooManyRequests, err.Error(), true
	case errors.Is(err, function.ErrNotFound):
		return http.StatusNotFound, "function not found", true
	case errors.Is(err, function.ErrDisabled):
		return http.StatusForbidden, errFunctionsOff.Msg, true
	case errors.Is(err, errFunctionBusy):
		return http.StatusConflict, err.Error(), true
	case errors.Is(err, kamajik8s.ErrUnreachable):
		return http.StatusServiceUnavailable, "functions are not available", true
	case errors.As(err, &kerr):
		switch {
		case kerr.Status == http.StatusForbidden && strings.Contains(kerr.Message, "exceeded quota"):
			return http.StatusTooManyRequests, "the project's function resources are in use", true
		case kerr.Status == http.StatusBadRequest || kerr.Status == http.StatusUnprocessableEntity:
			slog.Warn("function: kubernetes rejected a request", "err", err)
			return http.StatusBadRequest, "the cluster rejected the function's configuration", true
		default:
			return http.StatusServiceUnavailable, "functions are not available", true
		}
	}
	return 0, "", false
}

func sameSnapshot(a, b function.Snapshot) bool {
	return a.Image == b.Image && a.Port == b.Port && a.CPUMillis == b.CPUMillis && a.MemoryMi == b.MemoryMi &&
		maps.Equal(a.Env, b.Env) && slices.Equal(a.Command, b.Command) && slices.Equal(a.Args, b.Args) &&
		a.SecretsVersion == b.SecretsVersion && registryVersion(a.RegistryAuth) == registryVersion(b.RegistryAuth)
}

// errBadFunctionBody is all a caller is told of a body that does not decode. The decoder's own words quote the body,
// and the body is where secrets and registry passwords are sent.
var errBadFunctionBody = httpx.BadRequest("invalid request body")

// maxFunctionBody caps a function request: env and secrets (128 KiB each at most), a registry password, the rest.
const maxFunctionBody = 1 << 20

// laterPhaseKeys are the keys of a create or update that name what is not offered here and that function.Spec does
// not read (it refuses gpu, source and a public exposure itself). The decoder is not strict, so without this they
// would be taken and ignored.
var laterPhaseKeys = map[string]string{
	"domains":         "custom hostnames are not offered here",
	"publicRateLimit": "a public endpoint and its rate limit are not offered here",
}

// notOffered refuses a body that names a later-phase key with a value (null is no value).
func notOffered(given map[string]json.RawMessage) error {
	for _, key := range slices.Sorted(maps.Keys(laterPhaseKeys)) {
		if raw := given[key]; len(raw) > 0 && string(raw) != "null" {
			return httpx.BadRequest(key + ": " + laterPhaseKeys[key])
		}
	}
	return nil
}

func (h *Handler) functionList(w http.ResponseWriter, r *http.Request) {
	c, ok := h.functionCaller(w, r, rbac.ProjectCloudResourceRead)
	if !ok {
		return
	}
	fns := []functionDoc{}
	if err := h.fns.Find(r.Context(), pgdoc.M{"projectId": c.proj.ID}, &fns, pgdoc.Sort(pgdoc.Asc("name"))); err != nil {
		h.fail(w, err)
		return
	}
	// One cluster list per location for the live instance counts; without it the recorded count stands in.
	live := map[string]map[string]function.Status{}
	out := make([]functionView, 0, len(fns))
	for i := range fns {
		fn := &fns[i]
		counts, listed := live[fn.ServiceID]
		if !listed {
			if _, fs, err := h.functionSvc(r.Context(), fn.ServiceID); err == nil {
				if counts, err = fs.List(r.Context(), c.proj.ID); err != nil {
					slog.Warn("function: live list", "project", c.proj.ID, "service", fn.ServiceID, "err", err)
					counts = nil
				}
			}
			live[fn.ServiceID] = counts
		}
		ready := fn.Instances
		if counts != nil {
			ready = counts[fn.ID].Ready
		}
		out = append(out, c.view(fn, ready))
	}
	httpx.OK(w, out)
}

// functionLive renders one function with its live state off the cluster.
func (h *Handler) functionLive(ctx context.Context, c *fnCaller, fn *functionDoc) functionView {
	if fn.CurrentDeploymentID == "" && fn.DeployingID == "" { // nothing of it runs
		return c.view(fn, 0)
	}
	st, err := c.fs.Status(ctx, c.proj.ID, fn.ID)
	if err != nil {
		slog.Warn("function: live status", "function", fn.ID, "err", err)
		return c.view(fn, fn.Instances)
	}
	v := c.view(fn, st.Ready)
	switch {
	case fn.DeployingID != "":
		v.Reason = st.Reason
	case v.Status == fnIdle && st.Desired > 0:
		// An instance was asked for and is not ready yet: IDLE with zero instances alone would say nothing is
		// happening. It is a start and no rollout, so it is not called one.
		if v.Reason = st.Reason; v.Reason == function.ReasonRollout {
			v.Reason = function.ReasonStarting
		}
	}
	return v
}

func (h *Handler) functionGet(w http.ResponseWriter, r *http.Request) {
	c, ok := h.functionCaller(w, r, rbac.ProjectCloudResourceRead)
	if !ok {
		return
	}
	fn, ok := h.functionOf(w, r, c)
	if !ok {
		return
	}
	httpx.OK(w, h.functionLive(r.Context(), c, fn))
}

func (h *Handler) functionCreate(w http.ResponseWriter, r *http.Request) {
	c, ok := h.functionCaller(w, r, rbac.ProjectCloudResourceManage)
	if !ok || !c.offered(w) {
		return
	}
	ctx := r.Context()
	var req struct {
		function.Spec
		Secrets map[string]*string `json:"secrets"`
		// RegistryAuth takes the place of the spec's: nothing of the body reaches that one.
		RegistryAuth *registryAuthReq `json:"registryAuth"`
	}
	var given map[string]json.RawMessage
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxFunctionBody))
	if err := errors.Join(json.Unmarshal(body, &given), json.Unmarshal(body, &req)); err != nil {
		h.fail(w, errBadFunctionBody)
		return
	}
	if err := notOffered(given); err != nil {
		h.fail(w, err)
		return
	}
	now := time.Now().UTC()
	region := c.proj.ServiceRegion(c.es.ID)
	if region == "" {
		region = c.es.SandboxRegion()
	}
	fn := &functionDoc{ID: function.NewID(), ProjectID: c.proj.ID, ServiceID: c.es.ID, Region: region, CreatedAt: &now, UpdatedAt: &now}
	spec := req.Spec
	spec.ProjectID = c.proj.ID
	secrets, _ := mergeSecrets(nil, req.Secrets)
	describeSecrets(&spec.Snapshot, secrets) // never what the body says
	spec.RegistryAuth = nil                  // nor of the credentials
	fc := c.fs.Config()
	// Validated before anything is sealed: with the password missing or the limits broken nothing is.
	if req.RegistryAuth != nil {
		ra, err := req.RegistryAuth.describe()
		if err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return
		}
		spec.RegistryAuth = ra
	}
	err := spec.Validate(fc)
	if err == nil {
		err = function.ValidateSecrets(spec.Env, secrets)
	}
	if err != nil {
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	if err := h.functionUsageGate(ctx, c); err != nil {
		h.functionFail(w, err)
		return
	}
	// Sealed now, or nothing is stored: without a key a create that carries a secret or a password ends here.
	var password *fnSecret
	if req.RegistryAuth != nil {
		sealed, err := h.sealValue(fn, fnRegistryPasswordName, req.RegistryAuth.Password, now)
		if err != nil {
			h.fail(w, err)
			return
		}
		password = &sealed
	}
	if fn.Secrets, err = h.sealSecrets(fn, req.Secrets, now); err != nil {
		h.fail(w, err)
		return
	}
	if len(fn.Secrets) == 0 {
		fn.Secrets = nil
	}
	fn.Spec, fn.RegistryPassword = spec, password
	hold := fnHold{CPUMillis: spec.CPUMillis, MemoryMi: spec.MemoryMi, Instances: spec.MaxInstances}
	errNameTaken := httpx.NewError(http.StatusConflict, http.StatusConflict, "a function named "+spec.Name+" already exists in this project")
	// fits is the project's side of a create: room in its quota, and the name free. It holds the project's quota
	// lock until the transaction it runs in ends.
	fits := func(tc context.Context) error {
		if err := h.reserveFunction(tc, c, nil, hold); err != nil {
			return err
		}
		if taken, err := h.fns.Exists(tc, pgdoc.M{"projectId": c.proj.ID, "name": spec.Name}); err != nil {
			return err
		} else if taken {
			return errNameTaken
		}
		return nil
	}
	// Asked once before anything is made on the cluster (a refused create leaves no namespace), and again below
	// with the record.
	if err = h.fns.DB().WithTx(ctx, fits); err == nil {
		err = h.ensureFunctionNamespace(ctx, c)
	}
	if err != nil {
		h.functionFail(w, err)
		return
	}
	// The quota check, the host, the record and its row, or none of them: of two creates at the project's limit one
	// is refused. Every host ever issued stays in function_hosts, also after its function is deleted: a URL is never
	// handed out again (clients of the old function would reach the new one). A host that exists already (1 in 16M
	// per name) fails the transaction, and the next try takes another.
	var row *cloud.CloudResource
	for try := 0; try < 3; try++ {
		fn.Host = function.NewHost(spec.Name, c.fs.Domain())
		err = h.fns.DB().WithTx(ctx, func(tc context.Context) error {
			if err := fits(tc); err != nil {
				return err
			}
			if _, err := h.fnHosts.InsertOne(tc, pgdoc.M{"_id": fn.Host, "functionId": fn.ID, "projectId": fn.ProjectID, "createdAt": now}); err != nil {
				return err
			}
			if _, err := h.fns.InsertOne(tc, fn); err != nil {
				return err
			}
			var err error
			row, err = h.syncFunctionRow(tc, fn, now)
			return err
		})
		if !pgdoc.IsDup(err) {
			break
		}
	}
	if err != nil {
		h.functionFail(w, err)
		return
	}
	if err = h.startDeployment(ctx, c.fs, fn, spec.Snapshot, 0, nil, nil, nil); err != nil {
		// Nothing of a function that could not be started is left behind, least of all running.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if derr := c.fs.Delete(cleanup, c.proj.ID, fn.ID); derr != nil && !kamajik8s.NotFound(derr) {
			// The function stays, FAILED: its Secret may still hold the values, and only a function that has a
			// record is ever cleaned up again (its delete, or a deploy).
			slog.Error("function: delete after failed create, the function is kept", "function", fn.ID, "err", derr)
		} else if derr := h.dropFunctionRecords(cleanup, pgdoc.M{"_id": fn.ID}, now, true); derr != nil {
			slog.Error("function: drop the records of a failed create", "function", fn.ID, "err", derr)
		}
		h.functionFail(w, err)
		return
	}
	h.functionAudit(c, fn, row, "CLOUD_RESOURCE_CREATE", "")
	httpx.OK(w, c.view(fn, 0))
}

// functionUpdate applies a partial spec. minInstances, idleSeconds, maxInstances and concurrency take effect without
// a deployment; anything in the snapshot (image, size, env, port, command, args), any secret set or removed and
// registry credentials set, replaced or removed roll out as a new deployment. The name is the function's identity (it
// is in the host) and cannot change.
//
// secrets are merged, unlike env, which is replaced as a whole: their values cannot be read back, so a replace would
// mean entering every one of them again (mergeSecrets).
//
// registryAuth is one thing: an object sets or replaces the credentials (with the password, every time), null
// removes them, absent keeps them.
func (h *Handler) functionUpdate(w http.ResponseWriter, r *http.Request) {
	c, ok := h.functionCaller(w, r, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	fn, ok := h.functionOf(w, r, c)
	if !ok || !c.offered(w) {
		return
	}
	ctx := r.Context()
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxFunctionBody))
	var given map[string]json.RawMessage
	if err := json.Unmarshal(body, &given); err != nil {
		h.fail(w, errBadFunctionBody)
		return
	}
	if err := notOffered(given); err != nil {
		h.fail(w, err)
		return
	}
	spec := fn.Spec
	spec.ProjectID = c.proj.ID
	// A given map or list replaces the stored one; decoding onto it would merge into (and write through to) the
	// function's own.
	if _, ok := given["env"]; ok {
		spec.Env = nil
	}
	if _, ok := given["command"]; ok {
		spec.Command = nil
	}
	if _, ok := given["args"]; ok {
		spec.Args = nil
	}
	spec.SecretNames, spec.RegistryAuth = nil, nil // what the body says of them is dropped below: not into the function's own
	var req struct {
		Secrets      map[string]*string `json:"secrets"`
		RegistryAuth *registryAuthReq   `json:"registryAuth"`
	}
	if err := errors.Join(json.Unmarshal(body, &spec), json.Unmarshal(body, &req)); err != nil {
		h.fail(w, errBadFunctionBody)
		return
	}
	spec.SecretNames, spec.SecretsVersion = fn.SecretNames, fn.SecretsVersion // never what the body says
	spec.RegistryAuth = fn.RegistryAuth
	if spec.Name != fn.Name {
		h.fail(w, httpx.BadRequest("a function cannot be renamed"))
		return
	}
	_, registryGiven := given["registryAuth"]
	if registryGiven && req.RegistryAuth != nil {
		ra, err := req.RegistryAuth.describe()
		if err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return
		}
		spec.RegistryAuth = ra
	} else if registryGiven {
		spec.RegistryAuth = nil
	}
	var secrets map[string]string
	changed := false
	if len(req.Secrets) > 0 {
		cur, err := h.openSecrets(fn, fn.Secrets)
		if err != nil {
			h.fail(w, err)
			return
		}
		if secrets, changed = mergeSecrets(cur, req.Secrets); changed {
			describeSecrets(&spec.Snapshot, secrets)
		}
	}
	// Scale settings the platform has narrowed since they were stored are clamped at run time anyway: an update
	// that does not touch them must not fail on them.
	fc := c.fs.Config()
	if _, given := given["minInstances"]; !given {
		spec.MinInstances = min(spec.MinInstances, fc.MaxMinInstances)
	}
	if _, given := given["maxInstances"]; !given {
		spec.MaxInstances = min(spec.MaxInstances, fc.MaxInstances)
	}
	if _, given := given["idleSeconds"]; !given {
		spec.IdleSeconds = min(max(spec.IdleSeconds, fc.MinIdleSeconds), fc.MaxIdleSeconds)
	}
	// A size the platform has lowered its limits under since stays valid until it is rolled out again: an update of
	// the scale settings alone must not fail on it.
	stored := fc
	stored.MaxCPUMillis, stored.MaxMemoryMi = max(stored.MaxCPUMillis, fn.CPUMillis), max(stored.MaxMemoryMi, fn.MemoryMi)
	err := spec.Validate(stored)
	// A function that never had a working deployment (its first one failed) is deployed again even with nothing
	// changed: it has nothing to roll back to and no other way to be retried.
	deploy := err != nil || !sameSnapshot(spec.Snapshot, fn.Snapshot) || (fn.CurrentDeploymentID == "" && fn.DeployingID == "")
	if deploy {
		err = spec.Validate(fc) // a rollout is held to the limits of today
	}
	if err == nil && changed {
		err = function.ValidateSecrets(spec.Env, secrets)
	} else if err == nil {
		for _, name := range spec.SecretNames { // the secrets stay: only their names can clash with a new env
			if _, both := spec.Env[name]; both {
				err = errors.New(name + " is both an environment variable and a secret")
			}
		}
	}
	if err != nil {
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	now := time.Now().UTC()
	scale := spec.MinInstances != fn.MinInstances || spec.IdleSeconds != fn.IdleSeconds || spec.MaxInstances != fn.MaxInstances || spec.Concurrency != fn.Concurrency
	// What the function holds of the project's quota from now on: every instance it may run, and while the new
	// snapshot rolls out the larger of the two sizes.
	hold := fnHold{CPUMillis: fn.CPUMillis, MemoryMi: fn.MemoryMi, Instances: spec.MaxInstances}
	if deploy {
		hold.CPUMillis, hold.MemoryMi = max(hold.CPUMillis, spec.CPUMillis), max(hold.MemoryMi, spec.MemoryMi)
	}
	// The quota, the scale settings and (in startDeployment) the claim of the rollout: one transaction.
	settle := func(tc context.Context) error {
		if err := h.reserveFunction(tc, c, fn, hold); err != nil {
			return err
		}
		if !scale {
			return nil
		}
		_, err := h.fns.SetByID(tc, fn.ID, pgdoc.M{"minInstances": spec.MinInstances, "idleSeconds": spec.IdleSeconds, "maxInstances": spec.MaxInstances, "concurrency": spec.Concurrency, "updatedAt": now}, nil)
		return err
	}
	verb := "UPDATE"
	// What adds usage passes the usage gate: a deployment, and a floor or a cap that is raised.
	if deploy || spec.MinInstances > fn.MinInstances || spec.MaxInstances > fn.MaxInstances {
		if err := h.functionUsageGate(ctx, c); err != nil {
			h.functionFail(w, err)
			return
		}
	}
	if deploy {
		var password *fnSecret
		if registryGiven && req.RegistryAuth != nil {
			sealed, err := h.sealValue(fn, fnRegistryPasswordName, req.RegistryAuth.Password, now)
			if err != nil {
				h.fail(w, err)
				return
			}
			password = &sealed
		}
		var sealed map[string]fnSecret
		if changed {
			if sealed, err = h.sealSecrets(fn, req.Secrets, now); err != nil {
				h.fail(w, err)
				return
			}
		}
		if err := h.ensureFunctionNamespace(ctx, c); err != nil {
			h.functionFail(w, err)
			return
		}
		if err := h.startDeployment(ctx, c.fs, fn, spec.Snapshot, 0, sealed, password, settle); err != nil {
			h.functionFail(w, err)
			return
		}
		verb = "DEPLOY"
	} else if err := h.fns.DB().WithTx(ctx, settle); err != nil {
		h.functionFail(w, err)
		return
	}
	if scale {
		fn.MinInstances, fn.IdleSeconds, fn.MaxInstances, fn.Concurrency, fn.UpdatedAt = spec.MinInstances, spec.IdleSeconds, spec.MaxInstances, spec.Concurrency, &now
		// A raised maxInstances must find room in the namespace's own quota, which is written from the project's.
		if err := h.ensureFunctionNamespace(ctx, c); err != nil {
			slog.Warn("function: namespace quota after an update", "function", fn.ID, "err", err)
		}
	}
	h.functionAudit(c, fn, nil, "CLOUD_RESOURCE_ACTION", verb)
	httpx.OK(w, h.functionLive(ctx, c, fn))
}

// dropFunctionRecords deletes the functions that match filter with their deployment records, and ends their cloud
// rows: archived with stoppedAt set, so the last running interval is billed and nothing after it. One transaction:
// no row outlives its record, and no record its row. void is for a function that never came to be (a create that
// could not be started): its row is archived with no running interval at all, so nothing of it is ever billed.
func (h *Handler) dropFunctionRecords(ctx context.Context, filter pgdoc.M, now time.Time, void bool) error {
	return h.fns.DB().WithTx(ctx, func(tc context.Context) error {
		var fns []functionDoc
		if err := h.fns.Find(tc, filter, &fns); err != nil {
			return err
		}
		for i := range fns {
			fn := &fns[i]
			if _, err := h.fnDeps.DeleteMany(tc, pgdoc.M{"functionId": fn.ID}); err != nil {
				return err
			}
			if _, err := h.fns.DeleteByID(tc, fn.ID); err != nil {
				return err
			}
			if err := h.endFunctionRow(tc, fn, now, void); err != nil {
				return err
			}
		}
		return nil
	})
}

// functionDelete stops the function (its Deployment and pods), then drops its records and archives its cloud row
// with the end of its last running interval. It is answered also where functions are off: what is left there can
// always be removed.
func (h *Handler) functionDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := h.functionCaller(w, r, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	fn, ok := h.functionOf(w, r, c)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := c.fs.Delete(ctx, c.proj.ID, fn.ID); err != nil {
		h.functionFail(w, err)
		return
	}
	row, _ := h.cloud.FindByServiceIDAndExternalID(ctx, fn.ServiceID, fn.ID) // for the audit event: gone after the drop
	// The record goes with its row: every writer of the row first writes the record, so none can bring the row back
	// afterwards.
	if err := h.dropFunctionRecords(ctx, pgdoc.M{"_id": fn.ID}, time.Now().UTC(), false); err != nil {
		h.fail(w, err)
		return
	}
	// A deploy or rollback racing this delete may have applied the Deployment again after it was removed above. One
	// that applies from here on finds the record gone and removes it itself (applyFunction); one that applied before
	// is removed now.
	if err := c.fs.Delete(ctx, c.proj.ID, fn.ID); err != nil {
		slog.Warn("function: delete after the record", "function", fn.ID, "err", err)
	}
	h.functionAudit(c, fn, row, "CLOUD_RESOURCE_DELETE", "")
	httpx.Accepted(w)
}

func (h *Handler) functionDeployments(w http.ResponseWriter, r *http.Request) {
	c, ok := h.functionCaller(w, r, rbac.ProjectCloudResourceRead)
	if !ok {
		return
	}
	fn, ok := h.functionOf(w, r, c)
	if !ok {
		return
	}
	deps := []functionDeployment{}
	if err := h.fnDeps.Find(r.Context(), pgdoc.M{"functionId": fn.ID}, &deps, pgdoc.Sort(pgdoc.DescK("seq", pgdoc.KNum))); err != nil {
		h.fail(w, err)
		return
	}
	for i := range deps {
		deps[i].Env = c.env(deps[i].Env)
	}
	httpx.OK(w, deps)
}

// rollbackImage is the image a rollback to dep deploys: the digest dep ran, when it was deployed by tag and the
// digest was recorded, so that the rollback runs the bits that ran. A reference that names a digest already, and one
// whose digest was never told, stay as they are.
func rollbackImage(dep *functionDeployment) string {
	if dep.ImageDigest == "" || strings.Contains(dep.Image, "@") {
		return dep.Image
	}
	repo := dep.Image
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") { // a tag, not the port of the registry
		repo = repo[:i]
	}
	return repo + "@" + dep.ImageDigest
}

// functionRollback rolls the function to the snapshot of an earlier deployment that once served. It is recorded as a
// new deployment (rollbackOf names the old one): a record is one rollout with one outcome.
//
// Secrets and registry credentials are not versioned: the image, config and env are the old deployment's, the
// secrets and the credentials are the ones the function has now. The credentials can be changed with it, as in an
// update (registryAuth: an object or null): an image in another registry than they are for could not be rolled back
// to otherwise.
func (h *Handler) functionRollback(w http.ResponseWriter, r *http.Request) {
	c, ok := h.functionCaller(w, r, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	fn, ok := h.functionOf(w, r, c)
	if !ok || !c.offered(w) {
		return
	}
	ctx := r.Context()
	var req struct {
		DeploymentID string           `json:"deploymentId"`
		RegistryAuth *registryAuthReq `json:"registryAuth"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxFunctionBody))
	var given map[string]json.RawMessage
	// Not ignored: a registryAuth that cannot be read must not pass for null.
	if err := errors.Join(json.Unmarshal(body, &given), json.Unmarshal(body, &req)); err != nil {
		h.fail(w, errBadFunctionBody)
		return
	}
	var dep functionDeployment
	if found, err := h.fnDeps.Get(ctx, req.DeploymentID, &dep); err != nil {
		h.fail(w, err)
		return
	} else if !found || dep.FunctionID != fn.ID {
		httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "deployment not found")
		return
	}
	if dep.State != depSuperseded {
		h.fail(w, httpx.BadRequest("only a superseded deployment can be rolled back to"))
		return
	}
	if err := h.functionUsageGate(ctx, c); err != nil {
		h.functionFail(w, err)
		return
	}
	snap := dep.Snapshot
	snap.Image = rollbackImage(&dep)
	snap.SecretNames, snap.SecretsVersion = fn.SecretNames, fn.SecretsVersion
	for _, name := range snap.SecretNames {
		if _, both := snap.Env[name]; both {
			h.fail(w, httpx.BadRequest(name+" is an environment variable of that deployment and a secret now: remove the secret first"))
			return
		}
	}
	snap.RegistryAuth = fn.RegistryAuth
	_, registryGiven := given["registryAuth"]
	password, err := h.registryAuthChange(fn, &snap, registryGiven, req.RegistryAuth)
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := snap.CheckRegistryAuth(); err != nil { // not least: that deployment's image is in another registry
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	hold := fnHold{CPUMillis: max(fn.CPUMillis, snap.CPUMillis), MemoryMi: max(fn.MemoryMi, snap.MemoryMi), Instances: fn.MaxInstances}
	reserve := func(tc context.Context) error { return h.reserveFunction(tc, c, fn, hold) }
	if err := h.ensureFunctionNamespace(ctx, c); err != nil {
		h.functionFail(w, err)
		return
	}
	if err := h.startDeployment(ctx, c.fs, fn, snap, dep.Seq, nil, password, reserve); err != nil {
		h.functionFail(w, err)
		return
	}
	h.functionAudit(c, fn, nil, "CLOUD_RESOURCE_ACTION", "ROLLBACK")
	httpx.OK(w, h.functionLive(ctx, c, fn))
}

// functionAction: suspend stops the function and keeps it stopped (the proxy answers 503, a wake is refused); resume
// lets traffic wake it again.
func (h *Handler) functionAction(w http.ResponseWriter, r *http.Request) {
	c, ok := h.functionCaller(w, r, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	fn, ok := h.functionOf(w, r, c)
	if !ok || !c.offered(w) {
		return
	}
	ctx := r.Context()
	var req struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, maxFunctionBody)).Decode(&req)
	suspend := req.Action == "suspend"
	if !suspend && req.Action != "resume" {
		h.fail(w, httpx.BadRequest("action must be suspend or resume"))
		return
	}
	// A rollout at zero instances proves nothing and would be recorded as the working deployment.
	if suspend && fn.DeployingID != "" {
		h.functionFail(w, errFunctionBusy)
		return
	}
	if !suspend {
		// A resume adds usage: the project can be billed, the location's gates hold, and the project is within its
		// quota (which may have been lowered since the function was suspended).
		err := h.functionUsageGate(ctx, c)
		if err == nil {
			err = h.fns.DB().WithTx(ctx, func(tc context.Context) error { return h.reserveFunction(tc, c, fn, fnHold{}) })
		}
		if err == nil {
			err = h.ensureFunctionNamespace(ctx, c)
		}
		if err != nil {
			h.functionFail(w, err)
			return
		}
	}
	// A function that was never deployed has no Deployment to carry the proxy's mark: its first Apply renders the
	// mark from the record.
	mark := func() error {
		err := c.fs.SetSuspended(ctx, c.proj.ID, fn.ID, suspend)
		if fn.CurrentDeploymentID == "" && errors.Is(err, function.ErrNotFound) {
			return nil
		}
		return err
	}
	now := time.Now().UTC()
	var row *cloud.CloudResource
	// The mark in the record and the row's status, or neither.
	record := func() error {
		return h.fns.DB().WithTx(ctx, func(tc context.Context) error {
			if _, err := h.fns.SetByID(tc, fn.ID, pgdoc.M{"suspended": suspend, "updatedAt": now}, nil); err != nil {
				return err
			}
			var cur functionDoc
			if found, err := h.fns.Get(tc, fn.ID, &cur); err != nil || !found {
				return cmp.Or(err, function.ErrNotFound)
			}
			*fn = cur
			var err error
			row, err = h.syncFunctionRow(tc, fn, now)
			return err
		})
	}
	var err error
	if suspend {
		// The record first, so a wake refuses from here on; then the proxy's mark; then the instances.
		if err = record(); err == nil {
			err = mark()
		}
		if err == nil {
			err = h.scaleFunction(ctx, c.fs, fn, 0)
		}
	} else if err = mark(); err == nil {
		if err = record(); err == nil {
			h.raiseToFloor(ctx, c, fn)
		}
	}
	if err != nil {
		h.functionFail(w, err)
		return
	}
	h.functionAudit(c, fn, row, "CLOUD_RESOURCE_ACTION", strings.ToUpper(req.Action))
	httpx.OK(w, h.functionLive(ctx, c, fn))
}

// raiseToFloor starts the instance a resumed function's floor asks for (no rollout in progress; the caller passed
// the usage gate). A failure is not a failed resume: the next request wakes the function.
func (h *Handler) raiseToFloor(ctx context.Context, c *fnCaller, fn *functionDoc) {
	floor := functionFloor(fn, c.fs.Config())
	if fn.DeployingID != "" || fn.Instances >= floor {
		return
	}
	if err := h.scaleFunction(ctx, c.fs, fn, floor); err != nil {
		slog.Warn("function: raise to the floor after resume", "function", fn.ID, "err", err)
	}
}

// functionLogs returns the newest log lines of the function's current instances. Whoever may read the project's
// resources may read them: a function that prints its secrets shows them to every member.
func (h *Handler) functionLogs(w http.ResponseWriter, r *http.Request) {
	c, ok := h.functionCaller(w, r, rbac.ProjectCloudResourceRead)
	if !ok {
		return
	}
	fn, ok := h.functionOf(w, r, c)
	if !ok {
		return
	}
	tail := 200
	if t, err := strconv.Atoi(r.URL.Query().Get("tail")); err == nil && t > 0 {
		tail = min(t, 5000)
	}
	var logs string
	var err error
	if fn.CurrentDeploymentID != "" || fn.DeployingID != "" { // otherwise nothing of it runs: no instance has logged
		if logs, err = c.fs.Logs(r.Context(), c.proj.ID, fn.ID, tail); errors.Is(err, function.ErrNotFound) {
			logs, err = "", nil
		}
	}
	if err != nil {
		h.functionFail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"logs": logs})
}

// functionAudit emits the cloud-resource audit event of a function change, carrying the function's cloud row (read
// here when the caller has none; a missing row degrades to a project event, never to no event). Its metadata names
// the function, its image, the deployment the change made and the NAMES of its secrets. Never a value: neither of a
// secret, nor of env, nor a registry password.
func (h *Handler) functionAudit(c *fnCaller, fn *functionDoc, row *cloud.CloudResource, action, verb string) {
	if row == nil {
		row, _ = h.cloud.FindByServiceIDAndExternalID(context.Background(), fn.ServiceID, fn.ID)
	}
	if row == nil {
		h.projectAudit(c.u, c.proj, action)
		return
	}
	ev := newCloudResourceEvent(c.u, c.proj, action, verb, row)
	ev.ResourceMetadata["functionId"], ev.ResourceMetadata["name"], ev.ResourceMetadata["image"] = fn.ID, fn.Name, fn.Image
	if fn.DeploySeq > 0 {
		ev.ResourceMetadata["deploymentSeq"] = fn.DeploySeq
	}
	if len(fn.SecretNames) > 0 {
		ev.ResourceMetadata["secretNames"] = fn.SecretNames
	}
	h.audit.LogAsync(ev)
}

// fnHold is what one function holds of its project's function quota: every instance it may run (its maxInstances),
// each at its size. The figure is reserved, not measured: a function at zero instances holds as much as one at its
// cap, so a wake the portal allows is never one the namespace's quota refuses.
type fnHold struct {
	CPUMillis, MemoryMi, Instances int
}

func (f fnHold) cpu() int    { return f.CPUMillis * max(f.Instances, 1) }
func (f fnHold) memory() int { return f.MemoryMi * max(f.Instances, 1) }

// fnUsage is what a project holds on one location: its functions, and what they reserve.
type fnUsage struct {
	Functions int `json:"functions"`
	CPUMillis int `json:"cpuMillis"`
	MemoryMi  int `json:"memoryMi"`
}

// functionUsage is what the project's functions on a location reserve, the function except left out. A function
// whose rollout is in progress holds the larger of its two sizes: both snapshots run until it settles.
func (h *Handler) functionUsage(ctx context.Context, projectID, serviceID, except string) (fnUsage, error) {
	var fns []functionDoc
	if err := h.fns.Find(ctx, pgdoc.M{"projectId": projectID, "serviceId": serviceID}, &fns); err != nil {
		return fnUsage{}, err
	}
	var u fnUsage
	for i := range fns {
		fn := &fns[i]
		if fn.ID == except {
			continue
		}
		cpu, memory := h.billedSize(ctx, fn)
		hold := fnHold{CPUMillis: cpu, MemoryMi: memory, Instances: fn.MaxInstances}
		u.Functions, u.CPUMillis, u.MemoryMi = u.Functions+1, u.CPUMillis+hold.cpu(), u.MemoryMi+hold.memory()
	}
	return u, nil
}

// functionQuotaOf is the project's function quota on a location: its own functionQuota, else the provider's
// functions.projectQuota, else the built-in default. Never unlimited. The count is also held to the provider's
// maxFunctions, whatever the quota says.
func functionQuotaOf(c *fnCaller) sandbox.FunctionQuota {
	q := c.es.FunctionProjectQuota(c.proj.FunctionQuota)
	q.Functions = min(q.Functions, c.fs.Config().MaxFunctions)
	return q
}

// reserveFunction checks, inside the caller's transaction, that the project's function quota has room for want, and
// holds the project's quota lock until that transaction ends: what the caller then writes (a new record, a claim, a
// maxInstances) is what the next check counts. fn nil is a new function. A change that does not raise what fn holds
// is never refused, so a project over a quota that was lowered can still make its functions smaller; a zero want
// (a resume) checks the project as it is. What fn holds is read here, under the lock: want is its size and its
// maxInstances from now on, and never a smaller size than the one it holds.
func (h *Handler) reserveFunction(tc context.Context, c *fnCaller, fn *functionDoc, want fnHold) error {
	if err := h.fns.DB().LockXact(tc, "function-quota:"+c.proj.ID); err != nil {
		return err
	}
	q := functionQuotaOf(c)
	except := ""
	if fn != nil && want != (fnHold{}) {
		except = fn.ID
		// The function as it is under the lock, not the caller's copy read before it. What it holds is counted at
		// the size it holds now, and while a rollout is in progress that is the larger of its two sizes: a cap raised
		// at the small size that still serves would hold the large one at that cap once the rollout settles.
		var cur functionDoc
		if found, err := h.fns.Get(tc, fn.ID, &cur); err != nil || !found {
			return cmp.Or(err, function.ErrNotFound)
		}
		cpu, memory := h.billedSize(tc, &cur)
		want.CPUMillis, want.MemoryMi = max(want.CPUMillis, cpu), max(want.MemoryMi, memory)
		if has := (fnHold{CPUMillis: cpu, MemoryMi: memory, Instances: cur.MaxInstances}); want.cpu() <= has.cpu() && want.memory() <= has.memory() {
			return nil
		}
	}
	used, err := h.functionUsage(tc, c.proj.ID, c.es.ID, except)
	if err != nil {
		return err
	}
	if want != (fnHold{}) {
		used.CPUMillis, used.MemoryMi = used.CPUMillis+want.cpu(), used.MemoryMi+want.memory()
	}
	switch {
	case fn == nil && used.Functions >= q.Functions:
		return errFunctionQuota("the project already has " + strconv.Itoa(q.Functions) + " functions, the most it may have")
	case used.CPUMillis > q.VCPUs*1000:
		return errFunctionQuota("the project's function quota of " + strconv.Itoa(q.VCPUs) + " vCPUs would be exceeded: every function holds cpuMillis times its maxInstances")
	case used.MemoryMi > q.RAMMb:
		return errFunctionQuota("the project's function quota of " + strconv.Itoa(q.RAMMb) + " MiB of memory would be exceeded: every function holds memoryMi times its maxInstances")
	}
	return nil
}

// ensureFunctionNamespace writes the project's function namespace and its ResourceQuota from the quota as it is now
// (idempotent). Run before anything may start an instance: a quota an operator raised must be in the namespace before
// the portal allows what it permits. The namespace's quota is never under what the project's functions hold already:
// a quota lowered under that refuses what is new here, in the portal, and the cluster goes on starting the instances
// of what was admitted before (it follows down as the functions get smaller).
func (h *Handler) ensureFunctionNamespace(ctx context.Context, c *fnCaller) error {
	q := functionQuotaOf(c)
	used, err := h.functionUsage(ctx, c.proj.ID, c.es.ID, "")
	if err != nil {
		return err
	}
	q.Functions, q.VCPUs, q.RAMMb = max(q.Functions, used.Functions), max(q.VCPUs, (used.CPUMillis+999)/1000), max(q.RAMMb, used.MemoryMi)
	return c.fs.EnsureNamespace(ctx, c.proj.ID, q)
}

// functionQuotaUsage is the functions part of the project's quota usage answer for a location: the limits in force
// and what the project's functions reserve. nil when the service is no sandbox provider, or holds no function of the
// project and offers none.
func (h *Handler) functionQuotaUsage(ctx context.Context, proj *Project, serviceID string) map[string]any {
	if h.functionFor == nil || h.fns == nil || serviceID == "" {
		return nil
	}
	es, fs, err := h.functionSvc(ctx, serviceID)
	if err != nil {
		return nil
	}
	used, err := h.functionUsage(ctx, proj.ID, serviceID, "")
	if err != nil || (used.Functions == 0 && !functionsOffered(es, fs)) {
		return nil
	}
	q := functionQuotaOf(&fnCaller{proj: proj, es: es, fs: fs})
	return map[string]any{
		"limits": map[string]any{"functions": q.Functions, "vcpus": q.VCPUs, "ramMb": q.RAMMb},
		// Reserved, not measured: cpuMillis and memoryMi times maxInstances of every function.
		"usage": map[string]any{"functions": used.Functions, "vcpus": float64(used.CPUMillis) / 1000, "ramMb": used.MemoryMi},
	}
}
