package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/registrypins"
	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

const (
	LabelImport                 = "proxcloud.io/sandbox-import"
	AnnotationImportOperation   = "proxcloud.io/import-operation"
	AnnotationImportNonce       = "proxcloud.io/import-spec-nonce"
	AnnotationImportSource      = "proxcloud.io/import-source"
	AnnotationImportDestination = "proxcloud.io/import-destination"
)

type ImportRequest struct {
	ProjectID, TemplateID, BuildID, Source, Destination string
	DockerConfig                                        []byte
	Pins                                                *registrypins.ImportProof
	operation, nonce                                    string
}

func importPullSecret(project string) string { return "registry-import-pull-" + project }

// ImportObjects renders only the fixed trusted worker, never customer argv/env.
// Invalid policy yields no pod; StartImport validates and reports the error first.
func ImportObjects(cfg Config, worker string, req ImportRequest) map[string]any {
	cfg = cfg.WithDefaults()
	policy := sandboximport.Policy{TemplateRegistry: cfg.TemplateRegistry, SourceRepositories: cfg.Import.SourceRepositories, AllowAnonymous: cfg.Import.AllowAnonymous}
	raw, err := policy.JSON()
	if err != nil {
		return nil
	}
	managed, err := policy.Source(req.Source, req.ProjectID)
	if err != nil || policy.Destination(req.Source, req.ProjectID, req.Destination) != nil {
		return nil
	}
	volumes := []any{registryTrustVolume, map[string]any{"name": "pull-secret", "secret": map[string]any{"secretName": importPullSecret(req.ProjectID), "items": []any{map[string]any{"key": ".dockerconfigjson", "path": "config.json"}}}}}
	ro := func(name, path string) map[string]any { m := mount(name, path); m["readOnly"] = true; return m }
	mounts := []any{ro("pull-secret", "/pull"), ro("registry-ca", "/registry-ca")}
	if !managed {
		for _, v := range []struct{ name, secret, path string }{{"source-secret", baseSecretName(req.BuildID), "/source"}, {"push-secret", BuildSecretFor(req.ProjectID), "/push"}} {
			volumes = append(volumes, map[string]any{"name": v.name, "secret": map[string]any{"secretName": v.secret, "items": []any{map[string]any{"key": ".dockerconfigjson", "path": "config.json"}}}})
			mounts = append(mounts, ro(v.name, v.path))
		}
	}
	labels := map[string]any{LabelManagedBy: ManagedByValue, LabelService: cfg.Region, LabelProject: req.ProjectID, LabelTemplateID: req.TemplateID, LabelBuild: req.BuildID, LabelImport: "true"}
	// service identity is stamped by StartImport; region is not an ownership authority.
	delete(labels, LabelService)
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": buildName(req.BuildID), "namespace": BuildNamespace, "labels": labels, "annotations": map[string]any{AnnotationImportOperation: req.operation, AnnotationImportNonce: req.nonce, AnnotationImportSource: req.Source, AnnotationImportDestination: req.Destination}}, "spec": map[string]any{
		"runtimeClassName": cfg.RuntimeClass, "nodeSelector": map[string]any{"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64"}, "restartPolicy": "Never", "automountServiceAccountToken": false, "enableServiceLinks": false, "activeDeadlineSeconds": int64(BuildTimeout.Seconds()), "terminationGracePeriodSeconds": int64(5),
		"imagePullSecrets": []any{map[string]any{"name": importPullSecret(req.ProjectID)}},
		"securityContext":  map[string]any{"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532), "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"containers":       []any{map[string]any{"name": "import", "image": worker, "command": []any{"/sandbox-import"}, "args": []any{req.ProjectID, req.Source, req.Destination, raw}, "env": []any{registryTrustEnv}, "volumeMounts": mounts, "resources": map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "16Mi"}, "limits": map[string]any{"cpu": "1", "memory": "512Mi", "ephemeral-storage": "64Mi"}}, "securityContext": map[string]any{"readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}}}}, "volumes": volumes,
	}}
}

func (s *Service) ImportWorker(ctx context.Context) (string, error) {
	if err := s.importPolicy().Validate(); err != nil {
		return "", badBuild("%s", err)
	}
	sec, err := s.api.GetSecretData(ctx, PrepullNamespace, "sandbox-import-worker")
	if err != nil {
		return "", fmt.Errorf("trusted import worker is unavailable")
	}
	image := strings.TrimSpace(string(sec["image"]))
	repo, d, ok := strings.Cut(image, "@")
	host, _, _ := strings.Cut(s.cfg.TemplateRegistry, "/")
	if !ok || repo != host+"/library/proxcloud-sandbox-import" || !digestReImport(d) {
		return "", badBuild("digest imports require an operator-owned digest-pinned sandbox-system/sandbox-import-worker image")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return image, nil
}
func digestReImport(d string) bool {
	return len(d) == 71 && strings.HasPrefix(d, "sha256:") && strings.Trim(d[7:], "0123456789abcdef") == ""
}

// StartImport rechecks operator policy, consumes a distinct one-shot import proof,
// then dispatches exactly once. An uncertain write never deletes workers or pins.
func (s *Service) StartImport(ctx context.Context, req ImportRequest) error {
	managed, err := s.ValidateImportSource(req.Source, req.ProjectID, len(req.DockerConfig) > 0)
	if err != nil {
		return err
	}
	if err = s.importPolicy().Destination(req.Source, req.ProjectID, req.Destination); err != nil {
		return badBuild("%s", err)
	}
	if !validName(req.TemplateID) || !validName(req.BuildID) {
		return badBuild("invalid import identity")
	}
	host, _, _ := strings.Cut(req.Source, "/")
	if !managed {
		// No omitted credential file may fall back to a platform robot.
		if len(req.DockerConfig) == 0 {
			return badBuild("external import requires an explicit source auth document")
		}
		auth, e := sandboximport.SourceCredential(req.DockerConfig, host)
		if e != nil {
			return badBuild("%s", e)
		}
		if auth.Username == "" && !s.cfg.Import.AllowAnonymous {
			return badBuild("anonymous source access is not enabled")
		}
	}
	worker, err := s.ImportWorker(ctx)
	if err != nil {
		return err
	}
	if s.requireBuildPins || req.Pins != nil {
		req.operation, req.nonce, err = req.Pins.Consume(req.ProjectID, req.Source, req.Destination)
		if err != nil {
			return fmt.Errorf("sandbox: committed import pins required: %w", err)
		}
	}
	if err = s.ensureTenantNamespace(ctx, req.ProjectID); err != nil {
		return err
	}
	if _, err = s.copyProjectPullSecret(ctx, req.ProjectID, BuildNamespace, importPullSecret(req.ProjectID)); err != nil {
		return err
	}
	pod := ImportObjects(s.cfg, worker, req)
	if pod == nil {
		return badBuild("invalid import worker configuration")
	}
	pod["metadata"].(map[string]any)["labels"].(map[string]any)[LabelService] = s.serviceID
	if err = s.api.ApplyCore(ctx, "pods", pod); err != nil {
		return fmt.Errorf("sandbox: import dispatch outcome is uncertain: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if managed {
		return nil
	}
	live, err := s.api.GetCore(ctx, "pods", BuildNamespace, buildName(req.BuildID))
	if err != nil || !s.ownsImport(live, req.ProjectID, req.BuildID) || digStr(live, "metadata", "annotations", AnnotationImportOperation) != req.operation || digStr(live, "metadata", "annotations", AnnotationImportNonce) != req.nonce || digStr(live, "metadata", "annotations", AnnotationImportSource) != req.Source || digStr(live, "metadata", "annotations", AnnotationImportDestination) != req.Destination {
		return fmt.Errorf("sandbox: import worker identity is unavailable")
	}
	uid := digStr(live, "metadata", "uid")
	if uid == "" {
		return fmt.Errorf("sandbox: import worker UID is unavailable")
	}
	err = s.api.ApplyCore(ctx, "secrets", map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson", "metadata": map[string]any{"name": baseSecretName(req.BuildID), "namespace": BuildNamespace, "labels": map[string]any{LabelManagedBy: ManagedByValue, LabelService: s.serviceID, LabelProject: req.ProjectID, LabelBuild: req.BuildID}, "ownerReferences": []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "name": buildName(req.BuildID), "uid": uid}}}, "data": map[string]any{".dockerconfigjson": base64.StdEncoding.EncodeToString(req.DockerConfig)}})
	if err != nil {
		return fmt.Errorf("sandbox: import source credential outcome is uncertain: %w", err)
	}
	return ctx.Err()
}

func (s *Service) ownsImport(pod map[string]any, project, build string) bool {
	return s.ownsBuild(pod, project, build) && digStr(pod, "metadata", "labels", LabelService) == s.serviceID && digStr(pod, "metadata", "labels", LabelImport) == "true"
}

// ImportEvidence is a pure ownership-checked live read. Only positive terminal
// phase/UID/op/nonce is evidence; absence, timeout and Unknown are never terminal.
type ImportPodState struct {
	Status string
	Pod    registrypins.ImportPodEvidence
}

func (s *Service) ImportPodState(ctx context.Context, project, build string) (*ImportPodState, error) {
	pod, err := s.api.GetCore(ctx, "pods", BuildNamespace, buildName(build))
	if err != nil {
		return nil, err
	}
	if pod == nil {
		return nil, nil
	}
	if !s.ownsImport(pod, project, build) {
		return nil, ErrNotFound
	}
	e := registrypins.ImportPodEvidence{UID: digStr(pod, "metadata", "uid"), Operation: digStr(pod, "metadata", "annotations", AnnotationImportOperation), Nonce: digStr(pod, "metadata", "annotations", AnnotationImportNonce), Source: digStr(pod, "metadata", "annotations", AnnotationImportSource), Destination: digStr(pod, "metadata", "annotations", AnnotationImportDestination), Phase: digStr(pod, "status", "phase")}
	status := BuildWaiting
	switch e.Phase {
	case "Running":
		status = BuildBuilding
	case "Succeeded":
		status = BuildReady
	case "Failed":
		status = BuildError
	}
	return &ImportPodState{Status: status, Pod: e}, ctx.Err()
}

// DeleteImport only cleans a positively terminal, currently owned worker. Callers
// archive/settle operation evidence first; active/unknown workers retain their holds.
func (s *Service) DeleteImport(ctx context.Context, build string) error {
	pod, err := s.api.GetCore(ctx, "pods", BuildNamespace, buildName(build))
	if err != nil {
		return err
	}
	if pod == nil {
		return nil
	}
	project := digStr(pod, "metadata", "labels", LabelProject)
	if !s.ownsImport(pod, project, build) {
		return ErrNotFound
	}
	phase := digStr(pod, "status", "phase")
	if phase != "Succeeded" && phase != "Failed" {
		return fmt.Errorf("sandbox: import is active or unknown; retain operation pins")
	}
	if err = s.api.DeleteCore(ctx, "pods", BuildNamespace, buildName(build)); err != nil {
		return err
	}
	// Caller credentials are pod-owned. Avoid broad secret deletion: GC follows the
	// exact observed UID, and a replacement worker's credentials must never be removed.
	return nil
}

func importPendingReason(pod map[string]any) string {
	for _, key := range []string{"initContainerStatuses", "containerStatuses"} {
		for _, cs := range containerStatuses(pod, key) {
			switch digStr(cs, "state", "waiting", "reason") {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
				return "import worker image is unavailable"
			case "CreateContainerConfigError", "CreateContainerError":
				return "import worker configuration is unavailable"
			}
		}
	}
	return "import worker is pending eligible capacity or startup"
}

func (s *Service) ImportState(ctx context.Context, project, build string) (string, *sandboximport.Result, string, error) {
	pod, err := s.api.GetCore(ctx, "pods", BuildNamespace, buildName(build))
	if err != nil {
		return "", nil, "", err
	}
	if !s.ownsImport(pod, project, build) {
		return BuildWaiting, nil, "import worker is unavailable", nil
	}
	switch digStr(pod, "status", "phase") {
	case "Pending", "":
		return BuildWaiting, nil, importPendingReason(pod), nil
	case "Running":
		return BuildBuilding, nil, "", nil
	case "Succeeded":
		text, err := s.api.PodLogs(ctx, BuildNamespace, buildName(build), "import", 1)
		if err != nil {
			return "", nil, "", err
		}
		text = strings.TrimSpace(text)
		if stamp, body, ok := strings.Cut(text, " "); ok {
			if _, e := time.Parse(time.RFC3339Nano, stamp); e == nil {
				text = body
			}
		}
		var result sandboximport.Result
		if len(text) > 4096 || json.Unmarshal([]byte(text), &result) != nil || sandboximport.ValidateResult(result, digStr(pod, "metadata", "annotations", AnnotationImportSource), digStr(pod, "metadata", "annotations", AnnotationImportDestination)) != nil {
			return BuildError, nil, "invalid import verification output", nil
		}
		if err = ctx.Err(); err != nil {
			return "", nil, "", err
		}
		return BuildReady, &result, "", nil
	case "Failed":
		return BuildError, nil, "image import verification failed; inspect the sanitized worker diagnostics", nil
	default:
		return BuildBuilding, nil, "import worker outcome is unknown; operation is retained", nil
	}
}
