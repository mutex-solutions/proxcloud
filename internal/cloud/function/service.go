package function

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// K8sAPI is the cluster surface Service consumes: implemented by *kamajik8s.Client, fake-able in
// tests. Deployments go through the generic CR helpers (group apps).
//
// What is NOT here is the point: no read of a Secret, no delete of one, no watch, no read of a scale. The portal's
// identity holds none of those rights in a function namespace, and a call this interface cannot make is one the
// service cannot come to depend on (TestClusterCallsAreTheRolesVerbs).
type K8sAPI interface {
	EnsureNamespace(ctx context.Context, name string, labels, annotations map[string]string) error
	GetNamespace(ctx context.Context, name string) (map[string]any, error)
	DeleteNamespace(ctx context.Context, name string) error
	ListNamespaces(ctx context.Context, labelSelector string) ([]map[string]any, error)
	ApplyResourceQuota(ctx context.Context, rq map[string]any) error
	ApplyCR(ctx context.Context, group, version, plural string, obj map[string]any) error
	GetCR(ctx context.Context, group, version, plural, ns, name string) (map[string]any, error)
	DeleteCR(ctx context.Context, group, version, plural, ns, name string) error
	MergePatchCR(ctx context.Context, group, version, plural, ns, name string, patch map[string]any) (map[string]any, error)
	ListCRs(ctx context.Context, group, version, plural, ns, labelSelector string) ([]map[string]any, error)
	ListCRsAllNamespaces(ctx context.Context, group, version, plural, labelSelector string) ([]map[string]any, error)
	SetScale(ctx context.Context, group, version, plural, ns, name string, replicas int) error
	ListPods(ctx context.Context, ns, labelSelector string) ([]map[string]any, error)
	PodLogs(ctx context.Context, ns, pod, container string, tailLines int) (string, error)
	ApplyCore(ctx context.Context, plural string, obj map[string]any) error
	MergePatchCore(ctx context.Context, plural, ns, name string, patch map[string]any) error
}

// Errors the HTTP layer maps to 4xx.
var (
	ErrNotFound = errors.New("function not found")
	// ErrDisabled: the provider has no (enabled) functions block.
	ErrDisabled = errors.New("functions are not enabled on this provider")
)

// Service drives the functions of one sandbox provider (one cluster).
type Service struct {
	api       K8sAPI
	cfg       sandbox.Config
	serviceID string
}

// New builds a live Service from the sandbox provider config, whether or not the provider offers
// functions (see Enabled): what already runs there must stay stoppable and deletable.
func New(cfg sandbox.Config, serviceID string) (*Service, error) {
	kc, err := kamajik8s.New(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("function: cluster kubeconfig: %w", err)
	}
	return NewWithAPI(kc, cfg, serviceID), nil
}

// NewWithAPI builds a Service over any K8sAPI (tests).
func NewWithAPI(api K8sAPI, cfg sandbox.Config, serviceID string) *Service {
	return &Service{api: api, cfg: cfg.WithDefaults(), serviceID: serviceID}
}

// Config exposes the defaulted functions config (limits, floor, the default quota of a project).
func (s *Service) Config() sandbox.FunctionsConfig { return s.cfg.Functions }

// Domain is the domain functions are served under: the sandbox domain of the provider (NewHost).
func (s *Service) Domain() string { return s.cfg.Domain }

// Enabled reports whether the provider offers functions: the block is on and valid, and the provider has a domain
// to serve them under. When it is not, nothing new may be created, deployed or woken, and what runs is scaled to
// zero.
func (s *Service) Enabled() bool {
	return s.cfg.Functions.Enabled && s.cfg.ValidateFunctions() == nil
}

func (s *Service) owns(obj map[string]any, projectID string) bool {
	return digStr(obj, "metadata", "labels", sandbox.LabelManagedBy) == sandbox.ManagedByValue &&
		digStr(obj, "metadata", "labels", sandbox.LabelService) == s.serviceID &&
		digStr(obj, "metadata", "labels", sandbox.LabelProject) == projectID
}

// EnsureNamespace creates/labels fn-<projectId> and applies its ResourceQuota with the project's limits q.
// Idempotent; run before a project's first function and whenever its limits change. The namespace carries the tenant
// label every tenant namespace carries (sandbox.LabelTenant: the pod policy and the egress guardrail) and
// LabelTenant (the function network policy). On a shared cluster the portal's identity is bound to
// sandbox.FunctionRole there, the only role the cluster lets it bind in an fn-* namespace, and never to the sandbox
// role: its rights in a function namespace hold no exec and no sandbox kind.
//
// No registry credential is copied into the namespace: a function pulls with its own (BuildPullSecret) or with none.
func (s *Service) EnsureNamespace(ctx context.Context, projectID string, q sandbox.FunctionQuota) error {
	ns := NamespaceFor(projectID)
	if err := sandbox.EnsureTenantNamespace(ctx, s.api, s.cfg.Shared, s.serviceID, projectID, ns, sandbox.FunctionRole, map[string]string{LabelTenant: "true"}); err != nil {
		return err
	}
	return sandbox.AfterBinding(ctx, s.cfg.Shared, func() error {
		return s.api.ApplyResourceQuota(ctx, BuildQuota(s.cfg, s.serviceID, projectID, q))
	})
}

// DeleteNamespace removes fn-<projectId> and everything left in it (project teardown). Refuses a
// namespace proxcloud does not own for this service.
func (s *Service) DeleteNamespace(ctx context.Context, projectID string) error {
	ns := NamespaceFor(projectID)
	obj, err := s.api.GetNamespace(ctx, ns)
	if err != nil || obj == nil {
		return err
	}
	if !s.owns(obj, projectID) {
		return fmt.Errorf("function: namespace %s is not managed by this service", ns)
	}
	return s.api.DeleteNamespace(ctx, ns)
}

// Apply creates the function's Deployment or rolls it to snap (a deploy, a rollback, or the same
// snapshot again). It never changes the scale: a new Deployment starts with one instance, an
// existing one keeps what it has — a function at zero must be scaled up by the caller if the
// rollout is to be proven by a ready instance.
//
// secrets are the values snap runs with (snap.SecretsVersion says whether there are any) and
// registryPassword the password of snap.RegistryAuth. They go into the function's Secret and its
// pull secret, applied before the Deployment so that no instance starts without them, and owned by
// the Deployment. Each apply carries every value in data: a name that is no longer among the secrets is
// gone from the Secret with it. What a snapshot does not have at all is emptied instead, after the Deployment
// stopped naming it.
//
// ponytail: one Secret of each kind per function, changed in place: an instance of the serving
// snapshot that restarts while a rollout is in progress reads the new values. A Secret per
// version when that window matters.
func (s *Service) Apply(ctx context.Context, f Function, snap Snapshot, secrets map[string]string, registryPassword string) error {
	apply := func(uid string) error {
		if snap.SecretsVersion != "" {
			if err := s.api.ApplyCore(ctx, "secrets", BuildSecret(s.serviceID, f, uid, secrets)); err != nil {
				return err
			}
		}
		if snap.RegistryAuth != nil {
			return s.api.ApplyCore(ctx, "secrets", BuildPullSecret(s.serviceID, f, uid, *snap.RegistryAuth, registryPassword))
		}
		return nil
	}
	owned, uid := snap.SecretsVersion != "" || snap.RegistryAuth != nil, ""
	if owned {
		var err error
		if uid, err = s.deploymentUID(ctx, f); err == nil {
			err = apply(uid)
		}
		if err != nil {
			return err
		}
	}
	if err := s.api.ApplyCR(ctx, depGroup, depVersion, depPlural, BuildDeployment(s.cfg, s.serviceID, f, snap)); err != nil {
		return err
	}
	if owned && uid == "" {
		// A new Deployment has its uid only now: its Secrets are applied again, owned.
		uid, err := s.deploymentUID(ctx, f)
		if err == nil {
			err = apply(uid)
		}
		if err != nil {
			return err
		}
	}
	if snap.SecretsVersion == "" {
		if err := s.clearSecret(ctx, f.ProjectID, f.ID); err != nil {
			return err
		}
	}
	if snap.RegistryAuth == nil {
		return s.clearPullSecret(ctx, f.ProjectID, f.ID)
	}
	return nil
}

// deploymentUID is the uid of the function's Deployment, "" when there is none.
func (s *Service) deploymentUID(ctx context.Context, f Function) (string, error) {
	dep, err := s.api.GetCR(ctx, depGroup, depVersion, depPlural, NamespaceFor(f.ProjectID), f.ID)
	return digStr(dep, "metadata", "uid"), err
}

// clearSecret empties the function's Secret, if it has one: no secret value stays on the cluster
// for a function that has none or is being deleted. A patch, because proxcloud may neither read nor
// delete Secrets here; the emptied object goes with its owner, the Deployment.
func (s *Service) clearSecret(ctx context.Context, projectID, id string) error {
	return s.patchSecret(ctx, projectID, id, map[string]any{"data": nil})
}

// emptyDockerConfig is what a pull secret is emptied to: its type demands a docker config.
var emptyDockerConfig = base64.StdEncoding.EncodeToString([]byte(`{"auths":{}}`))

// clearPullSecret is clearSecret for the function's registry credentials.
func (s *Service) clearPullSecret(ctx context.Context, projectID, id string) error {
	return s.patchSecret(ctx, projectID, PullSecretName(id), map[string]any{"data": map[string]any{".dockerconfigjson": emptyDockerConfig}})
}

func (s *Service) patchSecret(ctx context.Context, projectID, name string, patch map[string]any) error {
	err := s.api.MergePatchCore(ctx, "secrets", NamespaceFor(projectID), name, patch)
	if kamajik8s.NotFound(err) {
		return nil
	}
	return err
}

// get returns the function's Deployment after checking proxcloud owns it.
func (s *Service) get(ctx context.Context, projectID, id string) (map[string]any, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	obj, err := s.api.GetCR(ctx, depGroup, depVersion, depPlural, NamespaceFor(projectID), id)
	if err != nil {
		return nil, err
	}
	if obj == nil || !s.owns(obj, projectID) {
		return nil, ErrNotFound
	}
	return obj, nil
}

// SetScale sets the desired instance count (0..sandbox.MaxFunctionInstances; the function's own
// cap is the platform's to apply) through the scale subresource. It
// skips the ownership read that get does (the wake path must stay one round trip): the id shape
// and the fn- namespace, which only proxcloud creates, are the guard.
func (s *Service) SetScale(ctx context.Context, projectID, id string, instances int) error {
	if !ValidID(id) {
		return ErrNotFound
	}
	if instances < 0 || instances > sandbox.MaxFunctionInstances {
		return fmt.Errorf("function: instances must be between 0 and %d", sandbox.MaxFunctionInstances)
	}
	return notFound(s.api.SetScale(ctx, depGroup, depVersion, depPlural, NamespaceFor(projectID), id, instances))
}

func notFound(err error) error {
	if kamajik8s.NotFound(err) {
		return ErrNotFound
	}
	return err
}

// SetSuspended sets or clears the suspended mark sandbox-proxy reads (a suspended function answers
// 503 and is never woken). A merge patch, so it needs no snapshot and cannot disturb a rollout.
// Apply renders the mark from Function.Suspended too, but an Apply without it does not remove a
// mark set here (another field owner): resume is always SetSuspended(false). Scaling to zero is
// the caller's step.
func (s *Service) SetSuspended(ctx context.Context, projectID, id string, suspended bool) error {
	if _, err := s.get(ctx, projectID, id); err != nil {
		return err
	}
	var v any // nil removes the annotation (merge patch)
	if suspended {
		v = "true"
	}
	_, err := s.api.MergePatchCR(ctx, depGroup, depVersion, depPlural, NamespaceFor(projectID), id,
		map[string]any{"metadata": map[string]any{"annotations": map[string]any{AnnotationSuspended: v}}})
	return notFound(err)
}

// Status is a function's Deployment as observed on the cluster.
type Status struct {
	Desired int // spec.replicas
	Ready   int // ready instances
	// RolloutDone: every desired instance runs the applied snapshot and is available. Trivially
	// true at zero instances.
	RolloutDone bool
	// Failed: the rollout made no progress within its deadline (120 s).
	Failed bool
	// Reason says what an unfinished rollout is waiting for or why it failed: one of Reasons, and nothing else.
	// Empty when RolloutDone. It is what a customer is shown.
	Reason string
	// Detail is what the cluster itself said about Reason, word for word: the kubelet's message of a failed pull,
	// the scheduler's of a pod without a node, the controller's of a pod it could not create. It can name machines,
	// internal addresses and other tenants' quota, so it is for the operator's log alone and never part of an answer
	// (it is not marshalled). Empty when the cluster said nothing.
	Detail string `json:"-"`
}

// What Status.Reason can be. Fixed words, never a Kubernetes message: those can carry registry
// credentials, node names, internal addresses or the customer's command line (Status.Detail keeps them for the
// operator).
const (
	// ReasonRollout: a rollout nothing more specific is known of yet.
	ReasonRollout = "rollout in progress"
	// ReasonStarting: a normal start.
	ReasonStarting = "pulling the image and starting the container"
	// ReasonUnplaced: the instance has no machine yet.
	ReasonUnplaced = "waiting for a node with free capacity"
	// ReasonNotCreated: the cluster refuses to create the pod.
	ReasonNotCreated = "the instance could not be created (project quota or cluster admission)"
	// ReasonDeadline: the Deployment's progress deadline passed.
	ReasonDeadline = "no instance became ready within 120 seconds"
	// ReasonPull / ReasonPullPrivate: the image could not be pulled, without and with credentials of the function.
	ReasonPull        = "image pull failed: check the image reference and that the registry allows anonymous pulls"
	ReasonPullPrivate = "image pull failed: the registry refused the credentials or does not have the image: check registryAuth (server, username, password) and the image reference"
	// ReasonCrash: the container does not stay up.
	ReasonCrash = "the container failed to start or keeps exiting: check the logs, command and args"
	// ReasonNotListening: the container runs and the readiness check finds nothing on the port.
	ReasonNotListening = "the container is running but nothing accepts connections on the function's port"
)

// Reasons is every value Status.Reason can have besides "".
var Reasons = []string{ReasonRollout, ReasonStarting, ReasonUnplaced, ReasonNotCreated, ReasonDeadline, ReasonPull, ReasonPullPrivate, ReasonCrash, ReasonNotListening}

// statusOf reads the Deployment alone (no pod lookup).
//
// Right after re-applying a snapshot whose ReplicaSet already exists (a rollback) on top of a
// rollout that timed out, the controller keeps the old ProgressDeadlineExceeded condition for
// a second or two, until it makes progress: Failed can be stale that long.
func statusOf(dep map[string]any) Status {
	st := Status{
		Desired: intOf(dig(dep, "spec", "replicas")),
		Ready:   intOf(dig(dep, "status", "readyReplicas")),
		Reason:  ReasonRollout,
	}
	// Until the controller has seen the applied spec, status and conditions describe the old one.
	if intOf(dig(dep, "status", "observedGeneration")) < intOf(dig(dep, "metadata", "generation")) {
		return st
	}
	conds, _ := dig(dep, "status", "conditions").([]any)
	for _, c := range conds {
		m, _ := c.(map[string]any)
		switch {
		case str(m["type"]) == "Progressing" && str(m["reason"]) == "ProgressDeadlineExceeded":
			st.Failed = true
			if st.Reason == ReasonRollout {
				st.Reason = ReasonDeadline
			}
		case str(m["type"]) == "ReplicaFailure" && str(m["status"]) == "True":
			// The most specific cause there is: no pod exists to tell more.
			st.Reason, st.Detail = ReasonNotCreated, str(m["message"])
		}
	}
	status := func(k string) int { return intOf(dig(dep, "status", k)) }
	if !st.Failed && status("updatedReplicas") == st.Desired && status("replicas") == st.Desired && status("availableReplicas") == st.Desired {
		st.RolloutDone, st.Reason, st.Detail = true, "", ""
	}
	return st
}

// podReason explains why a function's instance is not ready ("" when no pod tells), in the fixed words of Reasons,
// and returns beside it what the cluster said (Status.Detail).
//
// private says the function pulls with its own registry credentials: a failed pull is then most
// likely about them.
func podReason(pods []map[string]any, private bool) (reason, detail string) {
	for _, p := range pods {
		if dig(p, "metadata", "deletionTimestamp") != nil || podReady(p) {
			continue
		}
		if digStr(p, "spec", "nodeName") == "" {
			reason, detail = ReasonUnplaced, podCondition(p, "PodScheduled")
			continue
		}
		reason, detail = ReasonStarting, ""
		css, _ := dig(p, "status", "containerStatuses").([]any)
		for _, c := range css {
			cs, _ := c.(map[string]any)
			waiting := digStr(cs, "state", "waiting", "message")
			switch digStr(cs, "state", "waiting", "reason") {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
				if private {
					return ReasonPullPrivate, waiting
				}
				return ReasonPull, waiting
			case "CrashLoopBackOff", "RunContainerError", "CreateContainerError", "CreateContainerConfigError":
				return ReasonCrash, waiting
			}
			if dig(cs, "state", "terminated") != nil {
				return ReasonCrash, digStr(cs, "state", "terminated", "message")
			}
			if dig(cs, "state", "running") != nil {
				reason = ReasonNotListening
			}
		}
	}
	return reason, detail
}

// podCondition is the message of one condition of a pod.
func podCondition(pod map[string]any, typ string) string {
	conds, _ := dig(pod, "status", "conditions").([]any)
	for _, c := range conds {
		if m, _ := c.(map[string]any); str(m["type"]) == typ {
			return str(m["message"])
		}
	}
	return ""
}

func podReady(pod map[string]any) bool {
	conds, _ := dig(pod, "status", "conditions").([]any)
	for _, c := range conds {
		if m, _ := c.(map[string]any); str(m["type"]) == "Ready" {
			return str(m["status"]) == "True"
		}
	}
	return false
}

// Status returns the function's instance counts and rollout state. While a rollout is unfinished
// the reason comes from the function's pods.
//
// ponytail: a rollout is declared failed only at the Deployment's 120 s progress deadline, even
// for an image that can never be pulled; fail fast on terminal pod states when deploy latency
// on bad images matters.
func (s *Service) Status(ctx context.Context, projectID, id string) (Status, error) {
	dep, err := s.get(ctx, projectID, id)
	if err != nil {
		return Status{}, err
	}
	st := statusOf(dep)
	if !st.RolloutDone {
		pods, err := s.api.ListPods(ctx, NamespaceFor(projectID), LabelID+"="+id)
		if err != nil {
			return Status{}, err
		}
		private := digStr(dep, "spec", "template", "metadata", "annotations", AnnotationRegistryAuth) != ""
		if r, detail := podReason(pods, private); r != "" {
			st.Reason, st.Detail = r, detail
		}
	}
	return st, nil
}

// digestRe takes the manifest digest out of an imageID of the form <repository>@sha256:<64 hex>. A bare sha256:… is
// the id of the image's config, which no registry serves a manifest under, and is not taken.
var digestRe = regexp.MustCompile(`@(sha256:[0-9a-f]{64})$`)

// ImageDigest is the digest ("sha256:…") of the image the function's ready instances run, as the container
// runtime resolved it at their start, or "" while no ready instance tells. Asked once a rollout is done, when every
// instance runs the applied snapshot, it is what that snapshot's tag pointed at: the reference a later rollback
// deploys, so that it runs the bits that ran and not what the tag has moved to since.
func (s *Service) ImageDigest(ctx context.Context, projectID, id string) (string, error) {
	if !ValidID(id) {
		return "", ErrNotFound
	}
	pods, err := s.api.ListPods(ctx, NamespaceFor(projectID), LabelID+"="+id)
	if err != nil {
		return "", err
	}
	for _, p := range pods {
		if dig(p, "metadata", "deletionTimestamp") != nil || !podReady(p) {
			continue
		}
		css, _ := dig(p, "status", "containerStatuses").([]any)
		for _, c := range css {
			if cs, _ := c.(map[string]any); str(cs["name"]) == "main" {
				if m := digestRe.FindStringSubmatch(str(cs["imageID"])); m != nil {
					return m[1], nil
				}
			}
		}
	}
	return "", nil
}

// ownerSelector selects what this service created for one project.
func (s *Service) ownerSelector(projectID string) string {
	return sandbox.LabelProject + "=" + projectID + "," + s.serviceSelector()
}

func (s *Service) serviceSelector() string {
	return sandbox.LabelManagedBy + "=" + sandbox.ManagedByValue + "," + sandbox.LabelService + "=" + s.serviceID
}

// List returns the status of every function Deployment of the project, by function id (no pod
// lookups: Reason is the Deployment-level one).
func (s *Service) List(ctx context.Context, projectID string) (map[string]Status, error) {
	items, err := s.api.ListCRs(ctx, depGroup, depVersion, depPlural, NamespaceFor(projectID), s.ownerSelector(projectID))
	if err != nil {
		return nil, err
	}
	out := make(map[string]Status, len(items))
	for _, it := range items {
		if id := digStr(it, "metadata", "name"); ValidID(id) && dig(it, "metadata", "deletionTimestamp") == nil {
			out[id] = statusOf(it)
		}
	}
	return out, nil
}

// Deployed is one function Deployment found on the cluster.
type Deployed struct {
	ProjectID string
	Status
}

// ListAll returns every function Deployment of this service on the cluster, by function id: what
// the platform's records are reconciled against (scale drift, Deployments without a record).
//
// A sandbox cluster of its own answers that in one call. On a shared cluster the portal's identity has no
// cluster-wide read of Deployments (it would see every Deployment of the platform), so the list is made namespace
// by namespace over this service's fn-* namespaces, where it is bound.
// ponytail: one call per project namespace; a paged fan-out if a shared cluster ever holds hundreds of projects.
func (s *Service) ListAll(ctx context.Context) (map[string]Deployed, error) {
	var items []map[string]any
	if !s.cfg.Shared {
		var err error
		if items, err = s.api.ListCRsAllNamespaces(ctx, depGroup, depVersion, depPlural, s.serviceSelector()); err != nil {
			return nil, err
		}
	} else {
		nss, err := sandbox.TenantNamespaces(ctx, s.api, s.serviceID, "fn-")
		if err != nil {
			return nil, err
		}
		for _, ns := range nss {
			in, err := s.api.ListCRs(ctx, depGroup, depVersion, depPlural, ns, s.serviceSelector())
			if kamajik8s.NotFound(err) { // deleted since the list
				continue
			}
			if err != nil {
				return nil, err
			}
			items = append(items, in...)
		}
	}
	out := make(map[string]Deployed, len(items))
	for _, it := range items {
		id, project := digStr(it, "metadata", "name"), digStr(it, "metadata", "labels", sandbox.LabelProject)
		if ValidID(id) && digStr(it, "metadata", "namespace") == NamespaceFor(project) && s.owns(it, project) && dig(it, "metadata", "deletionTimestamp") == nil {
			out[id] = Deployed{ProjectID: project, Status: statusOf(it)}
		}
	}
	return out, nil
}

// Namespace is one function namespace of this service found on the cluster.
type Namespace struct {
	ProjectID string
	// Created is when the cluster made it; zero when the cluster told none.
	Created time.Time
}

// Namespaces returns the function namespaces of this service on the cluster (fn-<projectId>, owned by this service,
// not being deleted): what the platform's records are swept against, since a namespace whose project and functions
// are gone holds nothing ListAll would return.
func (s *Service) Namespaces(ctx context.Context) ([]Namespace, error) {
	items, err := s.api.ListNamespaces(ctx, sandbox.LabelTenant+"=true,"+sandbox.LabelService+"="+s.serviceID)
	if err != nil {
		return nil, err
	}
	var out []Namespace
	for _, it := range items {
		project := digStr(it, "metadata", "labels", sandbox.LabelProject)
		if project == "" || digStr(it, "metadata", "name") != NamespaceFor(project) || !s.owns(it, project) || dig(it, "metadata", "deletionTimestamp") != nil {
			continue
		}
		created, _ := time.Parse(time.RFC3339, digStr(it, "metadata", "creationTimestamp"))
		out = append(out, Namespace{ProjectID: project, Created: created})
	}
	return out, nil
}

// Logs returns the newest lines of the function's current instances, timestamped. A pod whose
// container has not started yet has no log and is skipped.
func (s *Service) Logs(ctx context.Context, projectID, id string, tailLines int) (string, error) {
	if _, err := s.get(ctx, projectID, id); err != nil {
		return "", err
	}
	ns := NamespaceFor(projectID)
	pods, err := s.api.ListPods(ctx, ns, LabelID+"="+id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range pods {
		name := digStr(p, "metadata", "name")
		logs, err := s.api.PodLogs(ctx, ns, name, "main", tailLines)
		var ae *kamajik8s.APIError
		if errors.As(err, &ae) && (ae.Status == 400 || ae.Status == 404) {
			continue // container not started yet, or the pod just went away
		}
		if err != nil {
			return "", err
		}
		if len(pods) > 1 {
			b.WriteString("==> " + name + " <==\n")
		}
		b.WriteString(logs)
	}
	return b.String(), nil
}

// Delete removes the function's Deployment and, through it, its pods and its Secrets (garbage
// collection). The Secrets are emptied as well, so their values are gone at once and also where a
// Secret never got its owner (an Apply that failed half way) — but only once the Deployment is
// gone: a delete that fails must leave the function as it was, not one whose next instance starts
// without its secrets or cannot pull its image. Absent = success.
func (s *Service) Delete(ctx context.Context, projectID, id string) error {
	if !ValidID(id) {
		return nil
	}
	_, err := s.get(ctx, projectID, id)
	if err == nil {
		err = s.api.DeleteCR(ctx, depGroup, depVersion, depPlural, NamespaceFor(projectID), id)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := s.clearSecret(ctx, projectID, id); err != nil {
		return err
	}
	return s.clearPullSecret(ctx, projectID, id)
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

// intOf reads a JSON number (float64 off the wire, int in manifests this package built).
func intOf(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}
