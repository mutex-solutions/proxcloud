package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

// K8sAPI is the sandbox-cluster surface Service consumes: implemented by *kamajik8s.Client,
// fake-able in tests.
type K8sAPI interface {
	EnsureNamespace(ctx context.Context, name string, labels map[string]string) error
	GetNamespace(ctx context.Context, name string) (map[string]any, error)
	DeleteNamespace(ctx context.Context, name string) error
	ApplyResourceQuota(ctx context.Context, rq map[string]any) error
	ApplyCR(ctx context.Context, group, version, plural string, obj map[string]any) error
	GetCR(ctx context.Context, group, version, plural, ns, name string) (map[string]any, error)
	DeleteCR(ctx context.Context, group, version, plural, ns, name string) error
	MergePatchCR(ctx context.Context, group, version, plural, ns, name string, patch map[string]any) (map[string]any, error)
	ListCRs(ctx context.Context, group, version, plural, ns, labelSelector string) ([]map[string]any, error)
	Exec(ctx context.Context, ns, pod, container string, cmd []string, stdin []byte, maxOutput int) (*kamajik8s.ExecResult, error)
	PodLogs(ctx context.Context, ns, pod, container string, tailLines int) (string, error)
	ApplyCore(ctx context.Context, plural string, obj map[string]any) error
	GetCore(ctx context.Context, plural, ns, name string) (map[string]any, error)
	DeleteCore(ctx context.Context, plural, ns, name string) error
	ListPods(ctx context.Context, ns, labelSelector string) ([]map[string]any, error)
	ListPodsAllNamespaces(ctx context.Context, labelSelector string) ([]map[string]any, error)
	ListNodes(ctx context.Context) ([]map[string]any, error)
	GetSecretData(ctx context.Context, ns, name string) (map[string][]byte, error)
	ExecStream(ctx context.Context, ns, pod, container string, cmd []string, w io.Writer, maxStderr int) (int, string, error)
	ListCRsAllNamespaces(ctx context.Context, group, version, plural, labelSelector string) ([]map[string]any, error)
}

// Logs returns the newest lines of the sandbox's output (envd + start command), timestamped.
func (s *Service) Logs(ctx context.Context, projectID, id string, tailLines int) (string, error) {
	if _, err := s.get(ctx, projectID, id); err != nil {
		return "", err
	}
	return s.api.PodLogs(ctx, NamespaceFor(projectID), id, "main", tailLines)
}

// Errors the HTTP layer maps to 4xx.
var (
	ErrNotFound      = errors.New("sandbox not found")
	ErrQuotaExceeded = errors.New("sandbox quota exceeded")
)

// Service drives one sandbox provider (one sandbox cluster).
type Service struct {
	api       K8sAPI
	cfg       Config
	serviceID string
	now       func() time.Time
}

// New builds a live Service from the provider config.
func New(cfg Config, serviceID string) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	kc, err := kamajik8s.New(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("sandbox: cluster kubeconfig: %w", err)
	}
	return NewWithAPI(kc, cfg, serviceID), nil
}

// NewWithAPI builds a Service over any K8sAPI (tests).
func NewWithAPI(api K8sAPI, cfg Config, serviceID string) *Service {
	return &Service{api: api, cfg: cfg.WithDefaults(), serviceID: serviceID, now: time.Now}
}

// Config exposes the defaulted provider config (catalog, limits).
func (s *Service) Config() Config { return s.cfg }

// ServiceID is the sandbox provider this service belongs to.
func (s *Service) ServiceID() string { return s.serviceID }

func (s *Service) ownerSelector(projectID string) string {
	return LabelProject + "=" + projectID + "," + LabelManagedBy + "=" + ManagedByValue + "," + LabelService + "=" + s.serviceID
}

// ensureTenantNamespace creates/labels sb-<projectId> and leaves its quota alone.
func (s *Service) ensureTenantNamespace(ctx context.Context, projectID string) error {
	return s.api.EnsureNamespace(ctx, NamespaceFor(projectID), map[string]string{
		LabelProject:   projectID,
		LabelService:   s.serviceID,
		LabelManagedBy: ManagedByValue,
		LabelTenant:    "true",
	})
}

// EnsureProjectNamespace creates/labels sb-<projectId> and its ResourceQuota (the bootstrap
// leg). LabelTenant is load-bearing: it is what puts the namespace under the cluster-wide egress
// guardrail. The quota is the hard backstop behind the create-time checks in CreateSandbox
// (which can race between two concurrent creates): the provider limits, lowered to the project's
// quota when q is set.
func (s *Service) EnsureProjectNamespace(ctx context.Context, projectID string, q *ProjectQuota) error {
	ns := NamespaceFor(projectID)
	if err := s.ensureTenantNamespace(ctx, projectID); err != nil {
		return err
	}
	l := s.cfg.Limits
	pods, cpu, memMb, gpus := l.MaxConcurrent, l.MaxConcurrent*l.MaxCPU, l.MaxConcurrent*l.MaxMemoryGiB*1024, l.MaxConcurrentGPUs
	if q != nil {
		pods, cpu, memMb, gpus = min(pods, q.Sandboxes), min(cpu, q.VCPUs), min(memMb, q.RAMMb), min(gpus, q.GPUs)
	}
	return s.api.ApplyResourceQuota(ctx, map[string]any{
		"apiVersion": "v1",
		"kind":       "ResourceQuota",
		"metadata": map[string]any{
			"name":      "proxcloud-sandbox",
			"namespace": ns,
			"labels":    map[string]any{LabelProject: projectID, LabelService: s.serviceID, LabelManagedBy: ManagedByValue},
		},
		"spec": map[string]any{
			"hard": map[string]any{
				"pods":                       strconv.Itoa(pods),
				"requests.cpu":               strconv.Itoa(cpu),
				"requests.memory":            fmt.Sprintf("%dMi", memMb),
				"requests." + ResourceGPU:    strconv.Itoa(gpus),
				"count/sandboxes." + CRGroup: strconv.Itoa(pods),
			},
		},
	})
}

// The project's pull Secret (TemplatePullSecret in sb-<project>) and build Secret (BuildSecretFor in sbx-build) are
// not written here: the registry's account sync writes them with that project's own accounts
// (docs/managed-sandbox.md §6). Copying one shared account into every namespace, as before, let a node escape read
// every tenant's templates.

// DeleteProjectNamespace removes sb-<projectId> and everything left in it (project teardown).
// Refuses a namespace proxcloud does not own for this service.
func (s *Service) DeleteProjectNamespace(ctx context.Context, projectID string) error {
	ns := NamespaceFor(projectID)
	obj, err := s.api.GetNamespace(ctx, ns)
	if err != nil || obj == nil {
		return err
	}
	if !s.owns(obj, projectID) {
		return fmt.Errorf("sandbox: namespace %s is not managed by this service", ns)
	}
	return s.api.DeleteNamespace(ctx, ns)
}

// live lists the project's sandboxes on the cluster, skipping terminating ones.
func (s *Service) live(ctx context.Context, projectID string) ([]map[string]any, error) {
	items, err := s.api.ListCRs(ctx, CRGroup, CRVersion, CRPlural, NamespaceFor(projectID), s.ownerSelector(projectID))
	if err != nil {
		return nil, err
	}
	out := items[:0]
	for _, it := range items {
		if dig(it, "metadata", "deletionTimestamp") == nil {
			out = append(out, it)
		}
	}
	return out, nil
}

// CreateSandbox validates the spec, enforces the per-project concurrency limits against the
// LIVE cluster (not the cache, which lags), and applies the Sandbox CR. Returns the cache row's
// data payload. spec.ID is minted here.
func (s *Service) CreateSandbox(ctx context.Context, spec *Spec) (map[string]any, error) {
	if err := spec.Validate(s.cfg); err != nil {
		return nil, err
	}
	if err := s.EnsureProjectNamespace(ctx, spec.ProjectID, spec.Quota); err != nil {
		return nil, fmt.Errorf("sandbox: ensure namespace: %w", err)
	}
	live, err := s.live(ctx, spec.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("sandbox: list: %w", err)
	}
	l := s.cfg.Limits
	if len(live)+1 > l.MaxConcurrent {
		return nil, fmt.Errorf("%w: at most %d sandboxes may run at once", ErrQuotaExceeded, l.MaxConcurrent)
	}
	if spec.Quota != nil {
		if err := spec.Quota.check(live, spec); err != nil {
			return nil, err
		}
	}
	if spec.GPUs() > 0 {
		held := 0
		for _, it := range live {
			held += gpuCount(it)
		}
		if held+spec.GPUs() > l.MaxConcurrentGPUs {
			return nil, fmt.Errorf("%w: at most %d GPUs may be held at once (%d in use)", ErrQuotaExceeded, l.MaxConcurrentGPUs, held)
		}
		if err := s.checkGPUCapacity(ctx, spec.GPU); err != nil {
			return nil, err
		}
	}
	shutdown, err := ShutdownTimeFor(s.cfg, s.now(), spec.TimeoutSeconds)
	if err != nil {
		return nil, err
	}
	spec.ID = newSandboxID()
	if s.cfg.Envd {
		// envd always requires its access token (E2B's `secure` is always on).
		spec.envdToken = newToken()
	}
	if spec.AllowPublicTraffic != nil && !*spec.AllowPublicTraffic {
		spec.trafficToken = newToken()
	}
	cr := BuildCR(s.cfg, s.serviceID, *spec, shutdown)
	if err := s.api.ApplyCR(ctx, CRGroup, CRVersion, CRPlural, cr); err != nil {
		return nil, fmt.Errorf("sandbox: create: %w", err)
	}
	// The egress policy is owned by the CR, so it needs the CR's uid. Without it the sandbox has
	// no internet (fail closed); roll the sandbox back rather than hand out a crippled one.
	if err := s.applyEgress(ctx, spec.ProjectID, spec.ID, spec.Network); err != nil {
		_ = s.api.DeleteCR(context.WithoutCancel(ctx), CRGroup, CRVersion, CRPlural, NamespaceFor(spec.ProjectID), spec.ID)
		return nil, fmt.Errorf("sandbox: egress policy: %w", err)
	}
	return sandboxData(cr), nil
}

// applyEgress (re)writes the sandbox's CiliumNetworkPolicy.
func (s *Service) applyEgress(ctx context.Context, projectID, id string, n *Network) error {
	obj, err := s.get(ctx, projectID, id)
	if err != nil {
		return err
	}
	pol, err := BuildEgressPolicy(NamespaceFor(projectID), id, digStr(obj, "metadata", "uid"), n)
	if err != nil {
		return err
	}
	return s.api.ApplyCR(ctx, CNPGroup, CNPVersion, CNPPlural, pol)
}

// SetNetwork replaces a running sandbox's egress rules (E2B PUT /sandboxes/{id}/network).
func (s *Service) SetNetwork(ctx context.Context, projectID, id string, n *Network) error {
	if err := n.Validate(); err != nil {
		return err
	}
	if err := s.applyEgress(ctx, projectID, id, n); err != nil {
		return err
	}
	var ann any // nil removes the annotation (merge patch)
	if n != nil {
		b, _ := json.Marshal(n)
		ann = string(b)
	}
	_, err := s.api.MergePatchCR(ctx, CRGroup, CRVersion, CRPlural, NamespaceFor(projectID), id,
		map[string]any{"metadata": map[string]any{"annotations": map[string]any{AnnotationNetwork: ann}}})
	return err
}

// Access is what a client needs to talk to a sandbox's envd and ports. Read from the live CR,
// never cached (the cache payload must not carry credentials).
type Access struct {
	EnvdAccessToken    string
	TrafficAccessToken string
	Network            *Network
	Metadata           map[string]string
}

// AccessFor returns the sandbox's tokens and network policy.
func (s *Service) AccessFor(ctx context.Context, projectID, id string) (*Access, error) {
	obj, err := s.get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	return AccessOf(obj), nil
}

// AccessOf reads the tokens, metadata and network rules from a Sandbox CR (live or saved).
func AccessOf(obj map[string]any) *Access {
	a := &Access{
		TrafficAccessToken: digStr(obj, "spec", "podTemplate", "metadata", "annotations", AnnotationTrafficToken),
		Metadata:           metadataOf(obj),
	}
	if env, ok := firstContainer(obj)["env"].([]any); ok {
		for _, e := range env {
			if m, ok := e.(map[string]any); ok && str(m["name"]) == "PROXCLOUD_ENVD_ACCESS_TOKEN" {
				a.EnvdAccessToken = str(m["value"])
			}
		}
	}
	if raw := digStr(obj, "metadata", "annotations", AnnotationNetwork); raw != "" {
		var n Network
		if json.Unmarshal([]byte(raw), &n) == nil {
			a.Network = &n
		}
	}
	return a
}

func metadataOf(obj map[string]any) map[string]string {
	raw := digStr(obj, "metadata", "annotations", AnnotationMetadata)
	if raw == "" {
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal([]byte(raw), &m)
	return m
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// get returns the project's sandbox, refusing objects proxcloud does not own for this
// service+project (a customer can only ever address their own namespace, but the guard keeps a
// mislabelled or hand-made object out of reach).
func (s *Service) get(ctx context.Context, projectID, id string) (map[string]any, error) {
	// IDs come from URLs: anything that is not an sbx- ID never reaches a Kubernetes API path.
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	obj, err := s.api.GetCR(ctx, CRGroup, CRVersion, CRPlural, NamespaceFor(projectID), id)
	if err != nil {
		return nil, err
	}
	if obj == nil || !s.owns(obj, projectID) {
		return nil, ErrNotFound
	}
	return obj, nil
}

// Alive is nil when the sandbox exists and is not being deleted (ErrNotFound otherwise): a sandbox a kill is removing
// still reads with GetSandbox until its pod is gone.
func (s *Service) Alive(ctx context.Context, projectID, id string) error {
	obj, err := s.get(ctx, projectID, id)
	if err == nil && dig(obj, "metadata", "deletionTimestamp") != nil {
		return ErrNotFound
	}
	return err
}

// GetSandbox returns the live data payload of one sandbox.
func (s *Service) GetSandbox(ctx context.Context, projectID, id string) (map[string]any, error) {
	obj, err := s.get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	s.pinRunningSince(ctx, obj)
	return s.observed(sandboxData(obj)), nil
}

// observed stamps a PENDING payload with when it was read (seenPendingAt): the latest moment the sandbox is known not to
// have been running, which bills a sandbox that ends before any read sees it running (MarkVanished).
func (s *Service) observed(d map[string]any) map[string]any {
	if d["status"] == StatusPending {
		d["seenPendingAt"] = s.now().UTC().Format(time.RFC3339)
	}
	return d
}

// pinRunningSince records, once, when the sandbox first became Ready (AnnotationRunningSince).
// The Ready condition's lastTransitionTime moves on every container restart, which customer code
// can trigger; billing must keep counting from the first start. Best-effort: on a failed patch the
// next read retries, and sandboxData falls back to the condition.
func (s *Service) pinRunningSince(ctx context.Context, obj map[string]any) {
	if digStr(obj, "metadata", "annotations", AnnotationRunningSince) != "" || !conditionTrue(obj, "Ready") {
		return
	}
	at := conditionTransition(obj, "Ready")
	if at == "" {
		return
	}
	ns, name := digStr(obj, "metadata", "namespace"), digStr(obj, "metadata", "name")
	if _, err := s.api.MergePatchCR(ctx, CRGroup, CRVersion, CRPlural, ns, name,
		map[string]any{"metadata": map[string]any{"annotations": map[string]any{AnnotationRunningSince: at}}}); err != nil {
		return
	}
	meta, _ := obj["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
		meta["annotations"] = ann
	}
	ann[AnnotationRunningSince] = at
}

// DeleteSandbox deletes one sandbox. Already gone = success.
func (s *Service) DeleteSandbox(ctx context.Context, projectID, id string) error {
	if _, err := s.get(ctx, projectID, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return s.api.DeleteCR(ctx, CRGroup, CRVersion, CRPlural, NamespaceFor(projectID), id)
}

// SetTimeout moves the sandbox's end to now+timeoutSeconds (keepalive / shorten). An auto-pause
// sandbox moves its end-at, and its shutdownTime to end-at + AutoPauseGrace.
func (s *Service) SetTimeout(ctx context.Context, projectID, id string, timeoutSeconds int) (map[string]any, error) {
	cur, err := s.get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	shutdown, err := ShutdownTimeFor(s.cfg, s.now(), timeoutSeconds)
	if err != nil {
		return nil, err
	}
	patch := map[string]any{"spec": map[string]any{"shutdownTime": shutdown.Format(time.RFC3339)}}
	if digStr(cur, "metadata", "annotations", AnnotationAutoPause) == "true" {
		patch["metadata"] = map[string]any{"annotations": map[string]any{AnnotationEndAt: shutdown.Format(time.RFC3339)}}
		patch["spec"] = map[string]any{"shutdownTime": shutdown.Add(AutoPauseGrace).Format(time.RFC3339)}
	}
	obj, err := s.api.MergePatchCR(ctx, CRGroup, CRVersion, CRPlural, NamespaceFor(projectID), id, patch)
	if err != nil {
		return nil, err
	}
	return sandboxData(obj), nil
}

// ShutdownAt sets the time the cluster removes the sandbox (spec.shutdownTime), outside the timeout limits: a pause
// holds a sandbox past its end until its image is ready, and puts the old time back if the pause fails. Its end-at
// (autoPause) stays as it is.
func (s *Service) ShutdownAt(ctx context.Context, projectID, id string, t time.Time) error {
	if _, err := s.get(ctx, projectID, id); err != nil {
		return err
	}
	_, err := s.api.MergePatchCR(ctx, CRGroup, CRVersion, CRPlural, NamespaceFor(projectID), id,
		map[string]any{"spec": map[string]any{"shutdownTime": t.UTC().Format(time.RFC3339)}})
	return err
}

// Exec limits. The output cap keeps one call's response bounded; long-running work belongs in
// a background process the agent polls.
const (
	DefaultExecTimeout = 60 * time.Second
	MaxExecTimeout     = 10 * time.Minute
	MaxExecOutput      = 1 << 20 // per stream
	maxExecStdin       = 1 << 20
)

// ErrNotRunning: exec on a sandbox that is not Ready yet.
var ErrNotRunning = errors.New("sandbox is not running yet")

// Exec runs one command in the sandbox and waits for it (bounded by timeout). stdin may be nil.
// The command runs inside gVisor as the image's user; nothing here reaches the node.
func (s *Service) Exec(ctx context.Context, projectID, id string, cmd []string, stdin []byte, timeout time.Duration) (*kamajik8s.ExecResult, error) {
	if len(cmd) == 0 {
		return nil, fmt.Errorf("%w: command is required", ErrBadExec)
	}
	if len(stdin) > maxExecStdin {
		return nil, fmt.Errorf("%w: stdin is larger than %d bytes", ErrBadExec, maxExecStdin)
	}
	if timeout <= 0 {
		timeout = DefaultExecTimeout
	}
	if timeout > MaxExecTimeout {
		return nil, fmt.Errorf("%w: timeoutSeconds must be at most %d", ErrBadExec, int(MaxExecTimeout.Seconds()))
	}
	obj, err := s.get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	if !conditionTrue(obj, "Ready") {
		return nil, ErrNotRunning
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// agent-sandbox names the pod after the Sandbox; "main" is the container BuildCR renders.
	return s.api.Exec(ctx, NamespaceFor(projectID), id, "main", cmd, stdin, MaxExecOutput)
}

// ErrBadExec: an invalid exec request (400).
var ErrBadExec = errors.New("invalid exec request")

// ListSandboxes returns the project's live sandboxes as cache payloads keyed by id.
func (s *Service) ListSandboxes(ctx context.Context, projectID string) (map[string]map[string]any, error) {
	items, err := s.live(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]any, len(items))
	for _, it := range items {
		if id := digStr(it, "metadata", "name"); id != "" {
			s.pinRunningSince(ctx, it)
			out[id] = s.observed(sandboxData(it))
		}
	}
	return out, nil
}

func (s *Service) owns(obj map[string]any, projectID string) bool {
	return digStr(obj, "metadata", "labels", LabelManagedBy) == ManagedByValue &&
		digStr(obj, "metadata", "labels", LabelService) == s.serviceID &&
		digStr(obj, "metadata", "labels", LabelProject) == projectID
}

// Status values in the cached payload. A DELETED row carries its end (endedAt), which the charge bills up to (see
// MarkDeleted and MarkVanished).
const (
	StatusPending = "PENDING"
	StatusRunning = "RUNNING"
	StatusDeleted = "DELETED"
)

// MarkDeleted turns a cached payload into its final state: status DELETED, endedAt = end.
func MarkDeleted(d map[string]any, end time.Time) map[string]any {
	out := make(map[string]any, len(d)+2)
	for k, v := range d {
		out[k] = v
	}
	out["status"] = StatusDeleted
	out["endedAt"] = end.UTC().Format(time.RFC3339)
	return out
}

// sandboxData maps a Sandbox CR to the cached `data` payload. Environment VALUES are never
// copied (customers pass secrets that way); only the variable names are kept.
func sandboxData(obj map[string]any) map[string]any {
	c := firstContainer(obj)
	status := StatusPending
	runningSince := digStr(obj, "metadata", "annotations", AnnotationRunningSince)
	if runningSince != "" {
		// Started once: it stays billable while its pod exists, even through a container restart.
		status = StatusRunning
	} else if conditionTrue(obj, "Ready") {
		status = StatusRunning
		runningSince = conditionTransition(obj, "Ready")
	}
	d := map[string]any{
		"name":         digStr(obj, "metadata", "annotations", AnnotationDisplayName),
		"template":     digStr(obj, "metadata", "annotations", AnnotationTemplate),
		"image":        str(c["image"]),
		"cpu":          intOf(dig(c, "resources", "limits", "cpu")),
		"memoryGiB":    memGiB(dig(c, "resources", "limits", "memory")),
		"gpuCount":     intOf(dig(c, "resources", "limits", ResourceGPU)),
		"gpuModel":     digStr(obj, "spec", "podTemplate", "spec", "nodeSelector", LabelGPU),
		"status":       status,
		"shutdownTime": endOf(obj),
	}
	if created := digStr(obj, "metadata", "creationTimestamp"); created != "" {
		d["startedAt"] = created
	}
	// runningSince is when the sandbox became Ready: billing starts here, not at create.
	if runningSince != "" {
		d["runningSince"] = runningSince
	}
	var envNames []any
	if env, ok := c["env"].([]any); ok {
		for _, e := range env {
			if m, ok := e.(map[string]any); ok && !strings.HasPrefix(str(m["name"]), "PROXCLOUD_") {
				envNames = append(envNames, str(m["name"]))
			}
		}
	}
	if md := metadataOf(obj); len(md) > 0 {
		d["metadata"] = md
	}
	d["publicTraffic"] = digStr(obj, "spec", "podTemplate", "metadata", "annotations", AnnotationTrafficToken) == ""
	if len(envNames) > 0 {
		d["envNames"] = envNames
	}
	return d
}

// endOf is when the sandbox ends for the customer (and billing): end-at for auto-pause
// sandboxes, else shutdownTime.
func endOf(obj map[string]any) string {
	if e := digStr(obj, "metadata", "annotations", AnnotationEndAt); e != "" {
		return e
	}
	return digStr(obj, "spec", "shutdownTime")
}

// Due is one auto-pause sandbox whose end has passed.
type Due struct{ ProjectID, ID string }

// DueForAutoPause lists this service's auto-pause sandboxes past their end-at.
func (s *Service) DueForAutoPause(ctx context.Context) ([]Due, error) {
	items, err := s.api.ListCRsAllNamespaces(ctx, CRGroup, CRVersion, CRPlural, LabelManagedBy+"="+ManagedByValue+","+LabelService+"="+s.serviceID)
	if err != nil {
		return nil, err
	}
	var out []Due
	for _, it := range items {
		if digStr(it, "metadata", "annotations", AnnotationAutoPause) != "true" || dig(it, "metadata", "deletionTimestamp") != nil {
			continue
		}
		end, err := time.Parse(time.RFC3339, digStr(it, "metadata", "annotations", AnnotationEndAt))
		if err == nil && !end.After(s.now()) {
			out = append(out, Due{ProjectID: digStr(it, "metadata", "labels", LabelProject), ID: digStr(it, "metadata", "name")})
		}
	}
	return out, nil
}

// RegistryRefsInUse lists every image the cluster references right now: the containers of every
// Sandbox CR (any labels, terminating ones too) and the source/destination refs of build and
// snapshot pods in sbx-build. Registry pruning must keep all of them.
func (s *Service) RegistryRefsInUse(ctx context.Context) ([]string, error) {
	crs, err := s.api.ListCRsAllNamespaces(ctx, CRGroup, CRVersion, CRPlural, "")
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, cr := range crs {
		cs, _ := dig(cr, "spec", "podTemplate", "spec", "containers").([]any)
		for _, c := range cs {
			if m, ok := c.(map[string]any); ok {
				refs = append(refs, str(m["image"]))
			}
		}
	}
	pods, err := s.api.ListPods(ctx, BuildNamespace, "")
	if err != nil {
		return nil, err
	}
	for _, p := range pods {
		for _, k := range []string{"initContainers", "containers"} {
			cs, _ := dig(p, "spec", k).([]any)
			for _, c := range cs {
				m, _ := c.(map[string]any)
				env, _ := m["env"].([]any)
				for _, e := range env {
					em, _ := e.(map[string]any)
					switch str(em["name"]) {
					case "SRC", "BASE", "DEST":
						refs = append(refs, str(em["value"]))
					}
				}
				args, _ := m["args"].([]any)
				for _, a := range args {
					if v := str(a); s.cfg.fromTemplateRegistry(v) {
						refs = append(refs, v)
					} else if d, ok := strings.CutPrefix(v, "--destination="); ok {
						refs = append(refs, d)
					}
				}
			}
		}
	}
	return refs, nil
}

func gpuCount(obj map[string]any) int {
	return intOf(dig(firstContainer(obj), "resources", "limits", ResourceGPU))
}

func firstContainer(obj map[string]any) map[string]any {
	cs, _ := dig(obj, "spec", "podTemplate", "spec", "containers").([]any)
	if len(cs) == 0 {
		return map[string]any{}
	}
	c, _ := cs[0].(map[string]any)
	if c == nil {
		return map[string]any{}
	}
	return c
}

func conditionTrue(obj map[string]any, typ string) bool {
	conds, _ := dig(obj, "status", "conditions").([]any)
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if str(m["type"]) == typ {
			return str(m["status"]) == "True"
		}
	}
	return false
}

// conditionTransition is the condition's lastTransitionTime ("" when absent).
func conditionTransition(obj map[string]any, typ string) string {
	conds, _ := dig(obj, "status", "conditions").([]any)
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if str(m["type"]) == typ {
			return str(m["lastTransitionTime"])
		}
	}
	return ""
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func digStr(m map[string]any, path ...string) string { return str(dig(m, path...)) }

func str(v any) string {
	s, _ := v.(string)
	return s
}

// intOf reads a quantity that proxcloud itself wrote: a plain integer, as string or number.
func intOf(v any) int {
	switch x := v.(type) {
	case string:
		n, _ := strconv.Atoi(x)
		return n
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return 0
}

// memGiB reads "<n>Gi" (the only memory form BuildCR writes).
func memGiB(v any) int {
	s := str(v)
	if len(s) > 2 && s[len(s)-2:] == "Gi" {
		n, _ := strconv.Atoi(s[:len(s)-2])
		return n
	}
	return 0
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
