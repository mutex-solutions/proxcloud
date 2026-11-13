package sandbox

// mirror.go — the sandbox cluster's side of the built-in template mirror (docs/template-updates.md §5):
//
//   - the trusted sandbox-import worker in copy mode, one pod per job acquisition in sbx-build, with its two
//     credentials (a short-lived GHCR registry token pull-scoped to the one source package — never the GitHub PAT —
//     and the registry's mirror account) as Secrets OWNED by that pod (ownerReference = the pod's observed UID): the
//     Secrets go with the pod, never outlive it;
//   - the prepull DaemonSets (template-prepull, template-prepull-gpu): the job replaces the one init container that
//     pulls <registry>/library/sandbox-<name> with the new digest ref, or adds one; a resourceVersion-guarded merge
//     patch. The portal's identity holds get/patch on exactly those two names, and an admission policy rendered by
//     installer stage 55 lets it change nothing but that image list (installer/playbooks/templates/sandbox-prepull.yml.j2).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

const (
	// LabelMirror marks a mirror worker pod; LabelMirrorJob carries the owning job's short id.
	LabelMirror    = "proxcloud.io/sandbox-mirror"
	LabelMirrorJob = "proxcloud.io/template-update"
	// RegistryMirrorUser is the registry's fixed mirror account (sandbox-proxy registry-users).
	RegistryMirrorUser = "mirror"
	// MirrorPullSecret is the read-only library/** account the kubelet pulls the worker image with in sbx-build
	// (written there by the registry account sync).
	MirrorPullSecret = "sandbox-registry-nodes"
	// PrepullDaemonSet / PrepullGPUDaemonSet are the only DaemonSets the portal may read or patch.
	PrepullDaemonSet    = "template-prepull"
	PrepullGPUDaemonSet = "template-prepull-gpu"
	// MirrorTimeout bounds one worker pod (activeDeadlineSeconds).
	MirrorTimeout = 30 * time.Minute
	// MirrorPendingLimit bounds how long a worker pod may stay Pending (image pull, scheduling, or credential Secrets
	// that were never written) before the job deletes it and starts a fresh one.
	MirrorPendingLimit = 5 * time.Minute
)

// ErrMirrorUnavailable is a missing prerequisite (worker image, registry, mirror account): permanent until an
// operator fixes it.
var ErrMirrorUnavailable = errors.New("sandbox mirror prerequisites are missing")

var mirrorPodRE = regexp.MustCompile(`^tu-mirror-[0-9a-f]{16}-e[0-9]{1,18}$`)

// MirrorPodName is the worker pod of one acquisition: a fresh name per epoch, so a superseded runner's pod never
// collides with the current one.
func MirrorPodName(jobShort string, epoch int64) string {
	return "tu-mirror-" + jobShort + "-e" + strconv.FormatInt(epoch, 10)
}

// RegistryHost is the template registry's host[:port] ("" without a registry).
func (c Config) RegistryHost() string {
	host, _, _ := strings.Cut(strings.TrimRight(c.TemplateRegistry, "/"), "/")
	return host
}

// ErrNoClusterAccess is a provider whose cluster cannot be reached at all: no kubeconfig, or one that does not parse.
var ErrNoClusterAccess = errors.New("sandbox: the provider has no usable cluster kubeconfig (secret.kubeconfig)")

// NewClusterOnly builds a Service from the provider's kubeconfig alone, without the provider document checks New runs
// (templates, limits, policies). It is for the template mirror's cleanup (this job's worker pods and, with them, their
// pod-owned Secrets; a partial prepull switch), which must run whatever else the provider lost since the job started:
// its mirror account, its template registry. Anything that creates sandboxes, builds or mirrors uses New.
func NewClusterOnly(cfg Config, serviceID string) (*Service, error) {
	if strings.TrimSpace(cfg.Kubeconfig) == "" || serviceID == "" {
		return nil, ErrNoClusterAccess
	}
	kc, err := kamajik8s.New(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoClusterAccess, err)
	}
	s := NewWithAPI(kc, cfg, serviceID)
	s.requireBuildPins, s.requireRuntimeCreatePins = true, true
	return s, nil
}

// MirrorPolicy is the worker's operator-derived argument.
func (s *Service) MirrorPolicy() (sandboximport.MirrorPolicy, error) {
	p := sandboximport.MirrorPolicy{Registry: s.cfg.RegistryHost()}
	if err := p.Validate(); err != nil {
		return p, fmt.Errorf("%w: the provider has no template registry (config.sandbox.templateRegistry)", ErrMirrorUnavailable)
	}
	if s.cfg.RegistryMirror.User != RegistryMirrorUser || s.cfg.RegistryMirror.Password == "" {
		return p, fmt.Errorf("%w: the provider has no registry mirror account (secret.registryMirror, installer stage 60)", ErrMirrorUnavailable)
	}
	return p, nil
}

// MirrorWorker is the operator-pinned trusted worker image (the same Secret as digest imports).
func (s *Service) MirrorWorker(ctx context.Context) (string, error) {
	sec, err := s.api.GetSecretData(ctx, PrepullNamespace, "sandbox-import-worker")
	if err != nil || sec == nil {
		return "", fmt.Errorf("%w: sandbox-system/sandbox-import-worker is unreadable", ErrMirrorUnavailable)
	}
	image := strings.TrimSpace(string(sec["image"]))
	repo, d, ok := strings.Cut(image, "@")
	if !ok || repo != s.cfg.RegistryHost()+"/library/proxcloud-sandbox-import" || !digestReImport(d) {
		return "", fmt.Errorf("%w: sandbox-system/sandbox-import-worker must be <registry>/library/proxcloud-sandbox-import@sha256:<digest>", ErrMirrorUnavailable)
	}
	return image, nil
}

// MirrorRequest is one acquisition's worker: fixed source/destination, the job's short id and epoch.
type MirrorRequest struct {
	JobShort    string // 16 lowercase hex
	Epoch       int64
	Source      string // ghcr.io/mutex-solutions/sandbox-<name>@sha256:<digest>
	Destination string // <registry>/library/sandbox-<name>:<tag>
	// SourceToken is a short-lived, anonymous GHCR registry bearer token with pull scope on the (public) source
	// package only, minted by the portal and checked never to be or contain the GitHub PAT; it goes only into the
	// pod-owned source Secret.
	SourceToken string
}

func mirrorSecretName(pod, which string) string { return pod + "-" + which }

// MirrorObjects renders the worker pod: fixed argv, no service-account token, read-only root, the two credential
// Secrets mounted read-only, the read-only nodes account as its image pull Secret.
func MirrorObjects(cfg Config, worker string, policy sandboximport.MirrorPolicy, req MirrorRequest, serviceID string) (map[string]any, error) {
	cfg = cfg.WithDefaults()
	raw, err := policy.JSON()
	if err != nil {
		return nil, err
	}
	if _, _, err = policy.Check(req.Source, req.Destination); err != nil {
		return nil, err
	}
	name := MirrorPodName(req.JobShort, req.Epoch)
	if !mirrorPodRE.MatchString(name) || serviceID == "" || len(serviceID) > 63 {
		return nil, errors.New("invalid mirror identity")
	}
	ro := func(n, path string) map[string]any { m := mount(n, path); m["readOnly"] = true; return m }
	secretVol := func(n, secret string) map[string]any {
		return map[string]any{"name": n, "secret": map[string]any{"secretName": secret, "items": []any{map[string]any{"key": ".dockerconfigjson", "path": "config.json"}}}}
	}
	labels := map[string]any{LabelManagedBy: ManagedByValue, LabelService: serviceID, LabelMirror: "true", LabelMirrorJob: req.JobShort}
	return map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": BuildNamespace, "labels": labels,
			"annotations": map[string]any{AnnotationImportSource: req.Source, AnnotationImportDestination: req.Destination}},
		"spec": map[string]any{
			"runtimeClassName": cfg.RuntimeClass, "nodeSelector": map[string]any{"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64"},
			"restartPolicy": "Never", "automountServiceAccountToken": false, "enableServiceLinks": false,
			"activeDeadlineSeconds": int64(MirrorTimeout.Seconds()), "terminationGracePeriodSeconds": int64(5),
			"imagePullSecrets": []any{map[string]any{"name": MirrorPullSecret}},
			"securityContext":  map[string]any{"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532), "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"containers": []any{map[string]any{"name": "mirror", "image": worker, "command": []any{"/sandbox-import"},
				"args": []any{sandboximport.MirrorMode, req.Source, req.Destination, raw}, "env": []any{registryTrustEnv},
				"volumeMounts": []any{ro("source-secret", "/source"), ro("push-secret", "/push"), ro("registry-ca", "/registry-ca")},
				"resources": map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "16Mi"},
					"limits": map[string]any{"cpu": "1", "memory": "512Mi", "ephemeral-storage": "64Mi"}},
				"securityContext": map[string]any{"readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}}}},
			"volumes": []any{registryTrustVolume, secretVol("source-secret", mirrorSecretName(name, "source")), secretVol("push-secret", mirrorSecretName(name, "push"))},
		}}, nil
}

// MirrorPod is the observed state of a worker pod this provider owns.
type MirrorPod struct {
	Name, UID, Phase string
	Source, Dest     string
	JobShort         string
	Created          string // metadata.creationTimestamp (RFC3339)
}

func (s *Service) ownsMirror(pod map[string]any, jobShort string) bool {
	return digStr(pod, "metadata", "labels", LabelManagedBy) == ManagedByValue && digStr(pod, "metadata", "labels", LabelService) == s.serviceID &&
		digStr(pod, "metadata", "labels", LabelMirror) == "true" && digStr(pod, "metadata", "labels", LabelMirrorJob) == jobShort
}

// GetMirrorPod reads a worker pod; nil when absent or not this job's.
func (s *Service) GetMirrorPod(ctx context.Context, jobShort, name string) (*MirrorPod, error) {
	if !mirrorPodRE.MatchString(name) {
		return nil, errors.New("invalid mirror pod name")
	}
	pod, err := s.api.GetCore(ctx, "pods", BuildNamespace, name)
	if err != nil || pod == nil {
		return nil, err
	}
	if !s.ownsMirror(pod, jobShort) {
		return nil, nil
	}
	return mirrorPodOf(pod), nil
}

func mirrorPodOf(pod map[string]any) *MirrorPod {
	return &MirrorPod{Name: digStr(pod, "metadata", "name"), UID: digStr(pod, "metadata", "uid"), Phase: digStr(pod, "status", "phase"),
		Source: digStr(pod, "metadata", "annotations", AnnotationImportSource), Dest: digStr(pod, "metadata", "annotations", AnnotationImportDestination),
		JobShort: digStr(pod, "metadata", "labels", LabelMirrorJob), Created: digStr(pod, "metadata", "creationTimestamp")}
}

// StartMirror creates the worker pod, reads back its UID, then writes the two credential Secrets owned by that UID.
// Returns the UID. The source token exists only in the source Secret's data (deleted with the pod by the garbage
// collector); the GitHub PAT never reaches the cluster.
func (s *Service) StartMirror(ctx context.Context, req MirrorRequest) (string, error) {
	policy, err := s.MirrorPolicy()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(req.SourceToken) == "" || strings.ContainsAny(req.SourceToken, " \t\r\n") || len(req.SourceToken) > 8192 {
		return "", errors.New("sandbox: no usable GHCR registry token for the mirror source")
	}
	worker, err := s.MirrorWorker(ctx)
	if err != nil {
		return "", err
	}
	pod, err := MirrorObjects(s.cfg, worker, policy, req, s.serviceID)
	if err != nil {
		return "", err
	}
	name := MirrorPodName(req.JobShort, req.Epoch)
	if err = s.api.ApplyCore(ctx, "pods", pod); err != nil {
		return "", fmt.Errorf("sandbox: mirror worker dispatch outcome is uncertain: %w", err)
	}
	live, err := s.GetMirrorPod(ctx, req.JobShort, name)
	if err != nil || live == nil || live.UID == "" || live.Source != req.Source || live.Dest != req.Destination {
		return "", errors.New("sandbox: mirror worker identity is unavailable")
	}
	owner := []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "name": name, "uid": live.UID}}
	labels := map[string]any{LabelManagedBy: ManagedByValue, LabelService: s.serviceID, LabelMirror: "true", LabelMirrorJob: req.JobShort}
	for _, sec := range []struct {
		which, host string
		auth        map[string]any
	}{
		{"source", sandboximport.MirrorSourceHost, map[string]any{"registrytoken": req.SourceToken}},
		{"push", policy.Registry, map[string]any{"username": s.cfg.RegistryMirror.User, "password": s.cfg.RegistryMirror.Password,
			"auth": base64.StdEncoding.EncodeToString([]byte(s.cfg.RegistryMirror.User + ":" + s.cfg.RegistryMirror.Password))}},
	} {
		doc, _ := json.Marshal(map[string]any{"auths": map[string]any{sec.host: sec.auth}})
		err = s.api.ApplyCore(ctx, "secrets", map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
			"metadata": map[string]any{"name": mirrorSecretName(name, sec.which), "namespace": BuildNamespace, "labels": labels, "ownerReferences": owner},
			"data":     map[string]any{".dockerconfigjson": base64.StdEncoding.EncodeToString(doc)}})
		if err != nil {
			return live.UID, fmt.Errorf("sandbox: mirror credential outcome is uncertain: %w", errors.New("secret write failed"))
		}
	}
	return live.UID, ctx.Err()
}

// MirrorResult reads a Succeeded worker's single result line and validates it for exactly its source/destination.
func (s *Service) MirrorResult(ctx context.Context, p *MirrorPod) (*sandboximport.MirrorResult, error) {
	text, err := s.api.PodLogs(ctx, BuildNamespace, p.Name, "mirror", 1)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if stamp, body, ok := strings.Cut(text, " "); ok {
		if _, e := time.Parse(time.RFC3339Nano, stamp); e == nil {
			text = body
		}
	}
	var r sandboximport.MirrorResult
	if len(text) > 4096 || json.Unmarshal([]byte(text), &r) != nil || sandboximport.ValidateMirrorResult(r, p.Source, p.Dest) != nil {
		return nil, errors.New("invalid mirror verification output")
	}
	return &r, nil
}

// DeleteMirrorPod deletes one observed worker instance (UID precondition); its Secrets follow by ownerReference.
func (s *Service) DeleteMirrorPod(ctx context.Context, name, uid string) error {
	api, ok := s.api.(coreUIDDeleter)
	if !ok || uid == "" {
		return errors.New("sandbox: mirror cleanup requires conditional UID deletion")
	}
	return api.DeleteCoreUID(ctx, "pods", BuildNamespace, name, uid)
}

// ListMirrorPods lists this job's worker pods (every epoch).
func (s *Service) ListMirrorPods(ctx context.Context, jobShort string) ([]MirrorPod, error) {
	pods, err := s.api.ListPods(ctx, BuildNamespace, LabelMirror+"=true,"+LabelMirrorJob+"="+jobShort)
	if err != nil {
		return nil, err
	}
	var out []MirrorPod
	for _, p := range pods {
		if !s.ownsMirror(p, jobShort) {
			continue
		}
		out = append(out, *mirrorPodOf(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListAllMirrorPods lists every mirror worker pod of this provider (any job): the orphan sweep.
func (s *Service) ListAllMirrorPods(ctx context.Context) ([]MirrorPod, error) {
	pods, err := s.api.ListPods(ctx, BuildNamespace, LabelMirror+"=true,"+LabelService+"="+s.serviceID)
	if err != nil {
		return nil, err
	}
	var out []MirrorPod
	for _, p := range pods {
		short := digStr(p, "metadata", "labels", LabelMirrorJob)
		if !s.ownsMirror(p, short) || !mirrorPodRE.MatchString(digStr(p, "metadata", "name")) {
			continue
		}
		out = append(out, *mirrorPodOf(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ── prepull DaemonSets ───────────────────────────────────────────────────────────────────────────

// DaemonSetAPI is the optional surface of the sandbox cluster client the prepull patch needs.
type DaemonSetAPI interface {
	GetDaemonSet(ctx context.Context, ns, name string) (map[string]any, error)
	PatchDaemonSet(ctx context.Context, ns, name string, patch map[string]any) (map[string]any, error)
}

var builtinRepoRE = regexp.MustCompile(`^[^/@]+/library/sandbox-[a-z0-9][a-z0-9-]{0,62}$`)
var prepullNameRE = regexp.MustCompile(`^p([0-9]{1,4})$`)

// PrepullImage is what the prepull DaemonSets pull for one repository.
type PrepullImage struct {
	DaemonSet, Container, Image string
}

// PrepullImages lists the init containers of both prepull DaemonSets pulling repo (<registry>/library/sandbox-<name>).
// A missing template-prepull-gpu is normal (no GPU nodes).
func (s *Service) PrepullImages(ctx context.Context, repo string) ([]PrepullImage, error) {
	api, ok := s.api.(DaemonSetAPI)
	if !ok {
		return nil, errors.New("sandbox: the cluster client cannot read DaemonSets")
	}
	var out []PrepullImage
	for _, ds := range []string{PrepullDaemonSet, PrepullGPUDaemonSet} {
		obj, err := api.GetDaemonSet(ctx, PrepullNamespace, ds)
		if err != nil {
			return nil, err
		}
		if obj == nil {
			continue
		}
		for _, c := range initContainers(obj) {
			if r, _ := splitRef(str(c["image"])); r == repo {
				out = append(out, PrepullImage{DaemonSet: ds, Container: str(c["name"]), Image: str(c["image"])})
			}
		}
	}
	return out, nil
}

func initContainers(ds map[string]any) []map[string]any {
	cs, _ := dig(ds, "spec", "template", "spec", "initContainers").([]any)
	out := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// SetPrepullImage points the prepull DaemonSets at ref (<registry>/library/sandbox-<name>:<tag>@sha256:<digest>):
// every init container pulling that repository gets the new image; when none does, one init container is added to
// template-prepull, shaped like its first one (the installer's p<N> naming). Nothing else changes. Each write is a
// merge patch guarded by the resourceVersion read just before (409 when the DaemonSet moved meanwhile). Returns the
// images replaced (empty when added) and whether anything was written.
func (s *Service) SetPrepullImage(ctx context.Context, ref string) (previous []PrepullImage, changed bool, err error) {
	api, ok := s.api.(DaemonSetAPI)
	if !ok {
		return nil, false, errors.New("sandbox: the cluster client cannot patch DaemonSets")
	}
	repo, d := splitRef(ref)
	host := s.cfg.RegistryHost()
	if !builtinRepoRE.MatchString(repo) || !strings.HasPrefix(repo, host+"/library/sandbox-") || !digestReImport(d) {
		return nil, false, errors.New("sandbox: a prepull image must be <registry>/library/sandbox-<name>[:tag]@sha256:<digest>")
	}
	found := false
	for _, dsName := range []string{PrepullDaemonSet, PrepullGPUDaemonSet} {
		obj, err := api.GetDaemonSet(ctx, PrepullNamespace, dsName)
		if err != nil {
			return previous, changed, err
		}
		if obj == nil {
			continue
		}
		cs := initContainers(obj)
		list, hit := make([]any, 0, len(cs)+1), false
		for _, c := range cs {
			cp := map[string]any{}
			for k, v := range c {
				cp[k] = v
			}
			if r, _ := splitRef(str(c["image"])); r == repo {
				hit = true
				if str(c["image"]) != ref {
					previous = append(previous, PrepullImage{DaemonSet: dsName, Container: str(c["name"]), Image: str(c["image"])})
					cp["image"] = ref
				}
			}
			list = append(list, cp)
		}
		if !hit {
			continue
		}
		found = true
		if eqJSON(list, toAny(cs)) {
			continue
		}
		if err = s.patchInit(ctx, api, obj, dsName, list); err != nil {
			return previous, changed, err
		}
		changed = true
	}
	if found {
		return previous, changed, nil
	}
	// No DaemonSet pulls this repository yet: add one init container to template-prepull.
	obj, err := api.GetDaemonSet(ctx, PrepullNamespace, PrepullDaemonSet)
	if err != nil {
		return nil, false, err
	}
	if obj == nil {
		return nil, false, errors.New("sandbox: sandbox-system/template-prepull does not exist (installer stage 55)")
	}
	cs := initContainers(obj)
	if len(cs) == 0 {
		return nil, false, errors.New("sandbox: template-prepull has no init container to shape a new one on")
	}
	next := 0
	list := make([]any, 0, len(cs)+1)
	for _, c := range cs {
		if m := prepullNameRE.FindStringSubmatch(str(c["name"])); m != nil {
			if n, _ := strconv.Atoi(m[1]); n >= next {
				next = n + 1
			}
		}
		list = append(list, c)
	}
	added := map[string]any{}
	for k, v := range cs[0] {
		added[k] = v
	}
	added["name"], added["image"] = "p"+strconv.Itoa(next), ref
	list = append(list, added)
	if err = s.patchInit(ctx, api, obj, PrepullDaemonSet, list); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

// RestorePrepullImages points named init containers back at their recorded images, but only a container that still
// runs `current` (what a failed job put there): a partial prepull switch is undone instead of leaving the two
// DaemonSets on different digests. Each DaemonSet is one resourceVersion-guarded patch.
func (s *Service) RestorePrepullImages(ctx context.Context, current string, restore []PrepullImage) (int, error) {
	api, ok := s.api.(DaemonSetAPI)
	if !ok {
		return 0, errors.New("sandbox: the cluster client cannot patch DaemonSets")
	}
	byDS := map[string]map[string]string{}
	for _, r := range restore {
		if r.DaemonSet != PrepullDaemonSet && r.DaemonSet != PrepullGPUDaemonSet {
			continue
		}
		if byDS[r.DaemonSet] == nil {
			byDS[r.DaemonSet] = map[string]string{}
		}
		byDS[r.DaemonSet][r.Container] = r.Image
	}
	n := 0
	for _, dsName := range []string{PrepullDaemonSet, PrepullGPUDaemonSet} {
		want := byDS[dsName]
		if len(want) == 0 {
			continue
		}
		obj, err := api.GetDaemonSet(ctx, PrepullNamespace, dsName)
		if err != nil {
			return n, err
		}
		if obj == nil {
			continue
		}
		cs := initContainers(obj)
		list, changed := make([]any, 0, len(cs)), false
		for _, c := range cs {
			cp := map[string]any{}
			for k, v := range c {
				cp[k] = v
			}
			if img, ok := want[str(c["name"])]; ok && str(c["image"]) == current && img != current {
				cp["image"], changed = img, true
				n++
			}
			list = append(list, cp)
		}
		if changed {
			if err := s.patchInit(ctx, api, obj, dsName, list); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (s *Service) patchInit(ctx context.Context, api DaemonSetAPI, obj map[string]any, name string, list []any) error {
	rv := digStr(obj, "metadata", "resourceVersion")
	if rv == "" {
		return errors.New("sandbox: the prepull DaemonSet has no resourceVersion")
	}
	_, err := api.PatchDaemonSet(ctx, PrepullNamespace, name, map[string]any{
		"metadata": map[string]any{"resourceVersion": rv},
		"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{"initContainers": list}}},
	})
	return err
}

func toAny(cs []map[string]any) []any {
	out := make([]any, len(cs))
	for i, c := range cs {
		out[i] = c
	}
	return out
}

func eqJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// PrepullProgress is how far the prepull pods are with ref: pulled/total counts every prepull pod on a Ready node
// that lists the repository (the adoption job's own measure, Adoptions).
func (s *Service) PrepullProgress(ctx context.Context, ref string) (pulled, total int, err error) {
	repo, d := splitRef(ref)
	pods, err := s.api.ListPods(ctx, PrepullNamespace, PrepullSelector)
	if err != nil {
		return 0, 0, err
	}
	nodes, err := s.api.ListNodes(ctx)
	if err != nil {
		return 0, 0, err
	}
	notReady := map[string]bool{}
	for _, n := range nodes {
		if !conditionTrue(n, "Ready") {
			notReady[digStr(n, "metadata", "name")] = true
		}
	}
	for _, p := range pods {
		if notReady[digStr(p, "spec", "nodeName")] || !ownedByDaemonSet(p) {
			continue
		}
		running := digStr(p, "status", "phase") == "Running"
		imageID := map[string]string{}
		statuses, _ := dig(p, "status", "initContainerStatuses").([]any)
		for _, st := range statuses {
			if m, ok := st.(map[string]any); ok {
				imageID[str(m["name"])] = str(m["imageID"])
			}
		}
		podInits, _ := dig(p, "spec", "initContainers").([]any)
		for _, raw := range podInits {
			c, _ := raw.(map[string]any)
			if r, _ := splitRef(str(c["image"])); r != repo {
				continue
			}
			total++
			if running && str(c["image"]) == ref && strings.HasSuffix(imageID[str(c["name"])], "@"+d) {
				pulled++
			}
		}
	}
	return pulled, total, nil
}
