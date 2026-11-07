package sandbox

// build.go — custom templates (E2B template builds). A build turns E2B's step list into a
// Dockerfile and runs it as one gVisor pod in BuildNamespace on the sandbox cluster:
//
//	init fetch  (trusted)    downloads the COPY contexts from proxcloud into /workspace/files/<hash>
//	init kaniko (user code)  builds the Dockerfile, --no-push, image tarball to /out; holds NO credentials
//	push        (trusted)    crane pushes the tarball to the template registry; only it mounts the push secret
//
// The customer's RUN steps execute inside the kaniko container, which has finished before the
// push container starts and never sees the registry secret.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	BuildNamespace  = "sbx-build"
	LabelBuild      = "proxcloud.io/sandbox-build"
	LabelTemplateID = "proxcloud.io/sandbox-template"
	buildPushSecret = "registry-push"
	// BuildTemplatePullSecret is a copy of the template registry's pull robot in sbx-build (made
	// by StartBuild), mounted only into the `base` init container and only for refs the server
	// chose (a project's own template). It can read every tenant's templates, so user code must
	// never see it.
	BuildTemplatePullSecret = "template-pull"
	TemplatePullSecret      = "sandbox-template-pull" // source: sandbox-system/<name>; copied into every sb-* namespace
	pullSecretSourceNS      = "sandbox-system"
	BuildTimeout            = 30 * time.Minute
	buildDiskLimit          = "30Gi"

	kanikoImage = "ghcr.io/osscontainertools/kaniko:v1.29.0@sha256:0c0b6c02991bba44090fad918ae177d193e1bd3fd0ccc643e7cc1f9f1a5a373d"
	fetchImage  = "curlimages/curl:8.17.0@sha256:935d9100e9ba842cdb060de42472c7ca90cfe9a7c96e4dacb55e79e560b3ff40"
	craneImage  = "gcr.io/go-containerregistry/crane:v0.20.3@sha256:fc86bcad43a000c2a1ca926a1e167db26c053cebc3fa5d14285c72773fb8c11d"
	// crane's debug variant has a shell: the snapshot push redirects crane's output to the
	// termination log.
	craneDebugImage = "gcr.io/go-containerregistry/crane:debug@sha256:e78770b31258a3846f878036d9c1f63fbe4c871f9f56990bf77fd95c013e3c1b"
)

// BuildStep is one E2B TemplateStep: type RUN|COPY|ENV|WORKDIR|USER.
type BuildStep struct {
	Type      string   `json:"type"`
	Args      []string `json:"args"`
	FilesHash string   `json:"filesHash,omitempty"`
	Force     bool     `json:"force,omitempty"`
}

// BuildResult is what a rendered build implies for the sandboxes started from it.
type BuildResult struct {
	Dockerfile string
	Hashes     []string // COPY contexts to fetch, in order, deduplicated
	User       string   // last USER ("" = image default)
	Workdir    string   // last WORKDIR ("" = image default)
}

var (
	ErrBadBuild    = errors.New("invalid template build")
	ErrBadTemplate = errors.New("template not usable")
	imageRefRe     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]{0,254}$`)
	hashRe         = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,128}$`)
	userRe         = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,31}(:[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,31})?$`)
	modeRe         = regexp.MustCompile(`^[0-7]{3,4}$`)
)

const maxBuildSteps = 200

func badBuild(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadBuild, fmt.Sprintf(format, a...))
}

// noControl rejects characters that would end a Dockerfile instruction early (injection).
func noControl(field, s string) error {
	if strings.ContainsAny(s, "\n\r\x00") {
		return badBuild("%s must not contain line breaks", field)
	}
	return nil
}

func jsonString(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep && and <> readable in the Dockerfile
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// RenderDockerfile turns E2B steps into a Dockerfile on top of from. Every customer string is
// either JSON-encoded (exec-form RUN, COPY sources) or validated against a strict pattern, so no
// step can smuggle in another instruction.
func RenderDockerfile(from string, steps []BuildStep) (*BuildResult, error) {
	if !imageRefRe.MatchString(from) {
		return nil, badBuild("invalid base image %q", from)
	}
	if len(steps) > maxBuildSteps {
		return nil, badBuild("at most %d steps", maxBuildSteps)
	}
	var b strings.Builder
	res := &BuildResult{}
	seen := map[string]bool{}
	fmt.Fprintf(&b, "FROM %s\n", from)
	user := "" // current USER for RUN steps that switch temporarily
	for i, st := range steps {
		where := fmt.Sprintf("step %d (%s)", i+1, st.Type)
		switch st.Type {
		case "RUN":
			if len(st.Args) == 0 || strings.TrimSpace(st.Args[0]) == "" {
				return nil, badBuild("%s: command is required", where)
			}
			as := ""
			if len(st.Args) > 1 && st.Args[1] != "" {
				if !userRe.MatchString(st.Args[1]) {
					return nil, badBuild("%s: invalid user %q", where, st.Args[1])
				}
				as = st.Args[1]
			}
			if as != "" {
				fmt.Fprintf(&b, "USER %s\n", as)
			}
			fmt.Fprintf(&b, "RUN [\"/bin/sh\", \"-c\", %s]\n", jsonString(st.Args[0]))
			if as != "" {
				restore := user
				if restore == "" {
					restore = "root"
				}
				fmt.Fprintf(&b, "USER %s\n", restore)
			}
		case "COPY":
			if len(st.Args) < 2 {
				return nil, badBuild("%s: source and destination are required", where)
			}
			if !hashRe.MatchString(st.FilesHash) {
				return nil, badBuild("%s: filesHash is required", where)
			}
			src, dest := st.Args[0], st.Args[1]
			if src == "" || path.IsAbs(src) || hasDotDot(src) {
				return nil, badBuild("%s: source %q must be a relative path inside the context", where, src)
			}
			for _, f := range []struct{ n, v string }{{"source", src}, {"destination", dest}} {
				if err := noControl(where+" "+f.n, f.v); err != nil {
					return nil, err
				}
			}
			flags := ""
			if len(st.Args) > 2 && st.Args[2] != "" {
				if !userRe.MatchString(st.Args[2]) {
					return nil, badBuild("%s: invalid owner %q", where, st.Args[2])
				}
				owner := st.Args[2]
				if !strings.Contains(owner, ":") {
					owner += ":" + owner
				}
				flags += " --chown=" + owner
			}
			if len(st.Args) > 3 && st.Args[3] != "" {
				if !modeRe.MatchString(st.Args[3]) {
					return nil, badBuild("%s: invalid mode %q", where, st.Args[3])
				}
				flags += " --chmod=" + st.Args[3]
			}
			srcPath := path.Join("files", st.FilesHash, strings.TrimPrefix(path.Clean(src), "./"))
			fmt.Fprintf(&b, "COPY%s [%s, %s]\n", flags, jsonString(srcPath), jsonString(dest))
			if !seen[st.FilesHash] {
				seen[st.FilesHash] = true
				res.Hashes = append(res.Hashes, st.FilesHash)
			}
		case "ENV":
			if len(st.Args) == 0 || len(st.Args)%2 != 0 {
				return nil, badBuild("%s: expects key/value pairs", where)
			}
			var kv []string
			for j := 0; j < len(st.Args); j += 2 {
				k, v := st.Args[j], st.Args[j+1]
				if !envKeyRe.MatchString(k) {
					return nil, badBuild("%s: invalid variable name %q", where, k)
				}
				if err := noControl(where+" "+k, v); err != nil {
					return nil, err
				}
				kv = append(kv, k+"="+jsonString(v))
			}
			fmt.Fprintf(&b, "ENV %s\n", strings.Join(kv, " "))
		case "WORKDIR":
			if len(st.Args) != 1 || st.Args[0] == "" {
				return nil, badBuild("%s: one path is required", where)
			}
			if err := noControl(where, st.Args[0]); err != nil {
				return nil, err
			}
			fmt.Fprintf(&b, "WORKDIR %s\n", st.Args[0])
			res.Workdir = st.Args[0]
		case "USER":
			if len(st.Args) != 1 || !userRe.MatchString(st.Args[0]) {
				return nil, badBuild("%s: invalid user", where)
			}
			fmt.Fprintf(&b, "USER %s\n", st.Args[0])
			user = st.Args[0]
			res.User = strings.SplitN(st.Args[0], ":", 2)[0]
		default:
			return nil, badBuild("%s: unknown step type", where)
		}
	}
	// sandbox-init runs as root (PID 1); the template's USER becomes envd's default user instead.
	if user != "" && user != "root" {
		b.WriteString("USER root\n")
	}
	res.Dockerfile = b.String()
	return res, nil
}

// BuildRequest is one build to run on the cluster.
type BuildRequest struct {
	ProjectID  string
	TemplateID string
	BuildID    string
	Image      string // destination ref, without digest
	Result     *BuildResult
	CPU        int
	MemoryGiB  int
	// FetchURL returns where the fetch init container downloads a COPY context (a proxcloud URL
	// carrying a per-build token).
	FetchURL func(hash string) string
	// Base is the image the Dockerfile's FROM names (the first line RenderDockerfile writes). The
	// `base` init container pulls it into kaniko's cache, so kaniko builds offline and never holds
	// a credential.
	Base string
	// BaseAuth says which credential `base` pulls with; BaseDockerConfig is the user's
	// dockerconfigjson for BaseAuthUser (put in a Secret owned by the pod, never stored).
	BaseAuth         BaseAuth
	BaseDockerConfig []byte
}

// BaseAuth is the credential the `base` init container pulls the base image with.
type BaseAuth int

const (
	BaseAuthNone     BaseAuth = iota // public image
	BaseAuthTemplate                 // a project's own template in the template registry (server-chosen ref)
	BaseAuthUser                     // fromImageRegistry: the user's registry credentials
)

// baseSecretName is the per-build Secret holding the user's registry credentials.
func baseSecretName(buildID string) string { return buildName(buildID) + "-base" }

func buildName(buildID string) string { return "build-" + buildID }

// BuildObjects renders the ConfigMap (Dockerfile + fetch list) and the Pod of a build.
func BuildObjects(cfg Config, req BuildRequest) (cm, pod map[string]any) {
	cfg = cfg.WithDefaults()
	name := buildName(req.BuildID)
	labels := map[string]any{
		LabelManagedBy: ManagedByValue, LabelProject: req.ProjectID,
		LabelBuild: req.BuildID, LabelTemplateID: req.TemplateID,
	}
	var fetch strings.Builder
	for _, h := range req.Result.Hashes {
		fmt.Fprintf(&fetch, "%s %s\n", h, req.FetchURL(h))
	}
	cm = map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": BuildNamespace, "labels": labels},
		"data":     map[string]any{"Dockerfile": req.Result.Dockerfile, "fetch.list": fetch.String()},
	}
	res := map[string]any{"cpu": strconv.Itoa(max(req.CPU, 1)), "memory": fmt.Sprintf("%dGi", max(req.MemoryGiB, 1)), "ephemeral-storage": buildDiskLimit}
	small := map[string]any{"cpu": "100m", "memory": "128Mi"}
	fetchScript := `set -eu
cp /config/Dockerfile /workspace/Dockerfile
while read -r hash url; do
  [ -n "$hash" ] || continue
  mkdir -p "/workspace/files/$hash"
  echo "fetching context $hash"
  curl -fsS --retry 3 -o /tmp/ctx.tar "$url"
  tar -xf /tmp/ctx.tar -C "/workspace/files/$hash"
  rm -f /tmp/ctx.tar
done < /config/fetch.list
`
	// base: pin the base to its linux/amd64 manifest digest, pull it into kaniko's cache (an OCI
	// layout per digest) and write the Dockerfile kaniko builds, FROM that digest. It is the only
	// container that may hold a registry credential; kaniko then builds offline.
	baseScript := `set -eu
case "$SRC" in *@sha256:*) D="${SRC##*@}";; *) D=$(crane digest --platform linux/amd64 "$SRC");; esac
case "$D" in sha256:*) ;; *) echo "base image: bad digest" >&2; exit 1;; esac
H="${D#sha256:}"
[ ${#H} -eq 64 ] && [ -z "$(printf %s "$H" | tr -d 0-9a-f)" ] || { echo "base image: bad digest" >&2; exit 1; }
R="${SRC%@*}"; case "${R##*/}" in *:*) R="${R%:*}";; esac
echo "pulling base image $R@$D"
crane pull --platform linux/amd64 --format=oci "$R@$D" "/cache/$D"
{ echo "FROM $R@$D"; tail -n +2 /config/Dockerfile; } > /pinned/Dockerfile
`
	baseMounts := []any{mount("config", "/config"), mount("base-cache", "/cache"), mount("pinned", "/pinned")}
	volumes := []any{
		map[string]any{"name": "config", "configMap": map[string]any{"name": name}},
		map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": buildDiskLimit}},
		map[string]any{"name": "out", "emptyDir": map[string]any{"sizeLimit": buildDiskLimit}},
		map[string]any{"name": "base-cache", "emptyDir": map[string]any{"sizeLimit": buildDiskLimit}},
		map[string]any{"name": "pinned", "emptyDir": map[string]any{"sizeLimit": "1Mi"}},
		map[string]any{"name": "push-secret", "secret": map[string]any{
			"secretName": buildPushSecret,
			"items":      []any{map[string]any{"key": ".dockerconfigjson", "path": "config.json"}},
		}},
	}
	if secret := map[BaseAuth]string{BaseAuthTemplate: BuildTemplatePullSecret, BaseAuthUser: baseSecretName(req.BuildID)}[req.BaseAuth]; secret != "" {
		baseMounts = append(baseMounts, mount("base-secret", "/docker"))
		volumes = append(volumes, map[string]any{"name": "base-secret", "secret": map[string]any{
			"secretName": secret,
			"items":      []any{map[string]any{"key": ".dockerconfigjson", "path": "config.json"}},
		}})
	}
	ro := func(m map[string]any) map[string]any { m["readOnly"] = true; return m }
	pod = map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": BuildNamespace, "labels": labels},
		"spec": map[string]any{
			"runtimeClassName":              cfg.RuntimeClass,
			"restartPolicy":                 "Never",
			"automountServiceAccountToken":  false,
			"enableServiceLinks":            false,
			"activeDeadlineSeconds":         int64(BuildTimeout.Seconds()),
			"terminationGracePeriodSeconds": int64(5),
			"initContainers": []any{
				map[string]any{
					"name": "fetch", "image": fetchImage, "command": []any{"sh", "-c", fetchScript},
					"resources":    map[string]any{"requests": small, "limits": small},
					"volumeMounts": []any{mount("config", "/config"), mount("workspace", "/workspace")},
				},
				map[string]any{
					"name": "base", "image": craneDebugImage, "command": []any{"sh", "-c", baseScript},
					"env": []any{
						map[string]any{"name": "SRC", "value": req.Base},
						map[string]any{"name": "DOCKER_CONFIG", "value": "/docker"},
					},
					"resources":    map[string]any{"requests": small, "limits": map[string]any{"cpu": "1", "memory": "512Mi"}},
					"volumeMounts": baseMounts,
				},
				map[string]any{
					"name": "kaniko", "image": kanikoImage,
					"args": []any{
						"--context=dir:///workspace", "--dockerfile=/pinned/Dockerfile",
						"--no-push", "--tar-path=/out/image.tar", "--destination=" + req.Image,
						"--snapshot-mode=redo", "--single-snapshot=false", "--cleanup",
						// Offline: the base comes from /cache (filled by `base`); no credential helpers.
						"--cache=true", "--cache-dir=/cache", "--cache-repo=oci:/kaniko/cache-repo",
						"--cache-run-layers=false", "--cache-copy-layers=false", "--credential-helpers=",
					},
					"resources": map[string]any{"requests": res, "limits": res},
					"volumeMounts": []any{mount("workspace", "/workspace"), mount("out", "/out"),
						ro(mount("base-cache", "/cache")), ro(mount("pinned", "/pinned"))},
				},
			},
			"containers": []any{map[string]any{
				"name": "push", "image": craneImage,
				"args":         []any{"push", "/out/image.tar", req.Image, "--image-refs", "/dev/termination-log"},
				"env":          []any{map[string]any{"name": "DOCKER_CONFIG", "value": "/docker"}},
				"resources":    map[string]any{"requests": small, "limits": map[string]any{"cpu": "1", "memory": "512Mi"}},
				"volumeMounts": []any{mount("out", "/out"), mount("push-secret", "/docker")},
			}},
			"volumes": volumes,
		},
	}
	return cm, pod
}

func mount(name, at string) map[string]any { return map[string]any{"name": name, "mountPath": at} }

// StartBuild creates the build's ConfigMap and Pod.
func (s *Service) StartBuild(ctx context.Context, req BuildRequest) error {
	if req.BaseAuth == BaseAuthTemplate {
		// The pull robot copy that only `base` mounts (refreshed on every such build).
		ok, err := s.copyPullSecret(ctx, BuildNamespace, BuildTemplatePullSecret, map[string]any{LabelManagedBy: ManagedByValue})
		if err != nil {
			return fmt.Errorf("sandbox: build pull credentials: %w", err)
		}
		if !ok {
			return fmt.Errorf("%w: building from a project template needs the template registry pull secret (%s/%s)", ErrBadBuild, pullSecretSourceNS, TemplatePullSecret)
		}
	}
	cm, pod := BuildObjects(s.cfg, req)
	if err := s.api.ApplyCore(ctx, "configmaps", cm); err != nil {
		return fmt.Errorf("sandbox: build config: %w", err)
	}
	if err := s.api.ApplyCore(ctx, "pods", pod); err != nil {
		return fmt.Errorf("sandbox: build pod: %w", err)
	}
	if req.BaseAuth != BaseAuthUser {
		return nil
	}
	// The user's credentials go in a Secret created after the pod and owned by it: the pod waits
	// for the volume, and the Secret goes with the pod (proxcloud cannot delete or read secrets).
	// If anything fails here, the pod is removed and nothing is left behind.
	fail := func(err error) error {
		_ = s.DeleteBuild(context.WithoutCancel(ctx), req.BuildID)
		return fmt.Errorf("sandbox: build credentials: %w", err)
	}
	live, err := s.api.GetCore(ctx, "pods", BuildNamespace, buildName(req.BuildID))
	if err != nil || live == nil {
		return fail(fmt.Errorf("build pod not found: %v", err))
	}
	uid := digStr(live, "metadata", "uid")
	if uid == "" {
		return fail(errors.New("build pod has no uid"))
	}
	err = s.api.ApplyCore(ctx, "secrets", map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
		"metadata": map[string]any{
			"name": baseSecretName(req.BuildID), "namespace": BuildNamespace,
			"labels": map[string]any{LabelManagedBy: ManagedByValue, LabelBuild: req.BuildID},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "v1", "kind": "Pod", "name": buildName(req.BuildID), "uid": uid,
			}},
		},
		"data": map[string]any{".dockerconfigjson": base64.StdEncoding.EncodeToString(req.BaseDockerConfig)},
	})
	if err != nil {
		return fail(err)
	}
	return nil
}

// Build states as E2B reports them.
const (
	BuildWaiting  = "waiting"
	BuildBuilding = "building"
	BuildReady    = "ready"
	BuildError    = "error"
)

// BuildState is the live state of a build pod.
type BuildState struct {
	Status     string
	Digest     string // pushed image digest (ready only)
	Reason     string // error only
	Step       string // container that failed
	StartedAt  string
	FinishedAt string
}

// BuildPodState reads the build pod. A missing pod is reported as (nil, nil).
func (s *Service) BuildPodState(ctx context.Context, projectID, buildID string) (*BuildState, error) {
	pod, err := s.api.GetCore(ctx, "pods", BuildNamespace, buildName(buildID))
	if err != nil || pod == nil {
		return nil, err
	}
	if digStr(pod, "metadata", "labels", LabelProject) != projectID {
		return nil, nil
	}
	return buildStateOf(pod), nil
}

func buildStateOf(pod map[string]any) *BuildState {
	st := &BuildState{StartedAt: digStr(pod, "status", "startTime")}
	switch digStr(pod, "status", "phase") {
	case "Pending":
		st.Status = BuildWaiting
		if st.StartedAt != "" {
			st.Status = BuildBuilding
		}
	case "Running":
		st.Status = BuildBuilding
	case "Succeeded":
		st.Status = BuildReady
		for _, cs := range containerStatuses(pod, "containerStatuses") {
			if str(cs["name"]) == "push" {
				st.Digest = digestOf(digStr(cs, "state", "terminated", "message"))
				st.FinishedAt = digStr(cs, "state", "terminated", "finishedAt")
			}
		}
		if st.Digest == "" {
			st.Status, st.Reason = BuildError, "the image was built but its digest was not reported"
		}
	default: // Failed, Unknown
		st.Status = BuildError
		st.Reason = digStr(pod, "status", "message")
		for _, key := range []string{"initContainerStatuses", "containerStatuses"} {
			for _, cs := range containerStatuses(pod, key) {
				if t, ok := dig(cs, "state", "terminated").(map[string]any); ok {
					st.FinishedAt = max(st.FinishedAt, str(t["finishedAt"]))
					if intOf(t["exitCode"]) != 0 && st.Step == "" {
						st.Step = str(cs["name"])
						st.Reason = fmt.Sprintf("%s failed (exit %d)", stepLabel(st.Step), intOf(t["exitCode"]))
					}
				}
			}
		}
		if digStr(pod, "status", "reason") == "DeadlineExceeded" {
			st.Reason = fmt.Sprintf("the build did not finish within %s", BuildTimeout)
		}
		if st.Reason == "" {
			st.Reason = "the build failed"
		}
	}
	return st
}

func stepLabel(container string) string {
	switch container {
	case "fetch":
		return "fetching the build context"
	case "base":
		return "pulling the base image"
	case "kaniko":
		return "a build step"
	case "push":
		return "pushing the image"
	}
	return container
}

// digestOf extracts sha256:… from crane output (repo@sha256:… or a bare digest).
func digestOf(refs string) string {
	for _, line := range strings.Fields(refs) {
		if strings.HasPrefix(line, "sha256:") {
			return line
		}
		if _, d, ok := strings.Cut(line, "@"); ok && strings.HasPrefix(d, "sha256:") {
			return d
		}
	}
	return ""
}

func containerStatuses(pod map[string]any, key string) []map[string]any {
	raw, _ := dig(pod, "status", key).([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// BuildLogs returns the build's output, fetch → kaniko → push, newest maxLines.
func (s *Service) BuildLogs(ctx context.Context, buildID string, maxLines int) []string {
	var lines []string
	for _, c := range []string{"fetch", "base", "kaniko", "push"} {
		text, err := s.api.PodLogs(ctx, BuildNamespace, buildName(buildID), c, maxLines)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			if l != "" {
				lines = append(lines, l)
			}
		}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}

// DeleteBuild removes a build's pod and ConfigMap (absent = success).
func (s *Service) DeleteBuild(ctx context.Context, buildID string) error {
	if err := s.api.DeleteCore(ctx, "pods", BuildNamespace, buildName(buildID)); err != nil {
		return err
	}
	return s.api.DeleteCore(ctx, "configmaps", BuildNamespace, buildName(buildID))
}

// buildData is the cache payload of a running or finished build: billed like a sandbox of the
// build's size (BillingResource "sandbox", template "build:<id>"), hidden from sandbox lists.
func buildData(pod map[string]any) map[string]any {
	st := buildStateOf(pod)
	k := firstInitContainer(pod, "kaniko")
	d := map[string]any{
		"kind":      "build",
		"name":      "build " + digStr(pod, "metadata", "labels", LabelTemplateID),
		"template":  "build:" + digStr(pod, "metadata", "labels", LabelTemplateID),
		"cpu":       intOf(dig(k, "resources", "limits", "cpu")),
		"memoryGiB": memGiB(dig(k, "resources", "limits", "memory")),
		"status":    StatusPending,
	}
	if st.StartedAt != "" {
		d["runningSince"] = st.StartedAt
		d["status"] = StatusRunning
	}
	if st.Status == BuildReady || st.Status == BuildError {
		end := st.FinishedAt
		if end == "" {
			end = time.Now().UTC().Format(time.RFC3339)
		}
		if st.StartedAt != "" {
			d["status"] = StatusDeleted
			d["endedAt"] = end
		}
	}
	return d
}

func firstInitContainer(pod map[string]any, name string) map[string]any {
	cs, _ := dig(pod, "spec", "initContainers").([]any)
	for _, c := range cs {
		if m, ok := c.(map[string]any); ok && str(m["name"]) == name {
			return m
		}
	}
	return map[string]any{}
}

// finishedBuildTTL is how long a finished build pod is kept (its logs; the status endpoint copies
// them into the template earlier when it is polled).
const finishedBuildTTL = 10 * time.Minute

// ListBuilds returns the project's build pods as cache payloads keyed by pod name. Pods finished
// more than finishedBuildTTL ago are deleted here; they are still returned this time, so the cache
// row is written as DELETED with its end before the pod vanishes (then it lingers and archives).
func (s *Service) ListBuilds(ctx context.Context, projectID string) (map[string]map[string]any, error) {
	pods, err := s.api.ListPods(ctx, BuildNamespace, LabelManagedBy+"="+ManagedByValue+","+LabelProject+"="+projectID)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, p := range pods {
		d := buildData(p)
		out[digStr(p, "metadata", "name")] = d
		if end, err := time.Parse(time.RFC3339, str(d["endedAt"])); err == nil && s.now().Sub(end) > finishedBuildTTL {
			_ = s.DeleteBuild(ctx, digStr(p, "metadata", "labels", LabelBuild))
		}
	}
	return out, nil
}

// BuildData returns one build's cache payload (nil when the pod is gone).
func (s *Service) BuildData(ctx context.Context, buildID string) (map[string]any, error) {
	pod, err := s.api.GetCore(ctx, "pods", BuildNamespace, buildName(buildID))
	if err != nil || pod == nil {
		return nil, err
	}
	return buildData(pod), nil
}

// BuildResourceID is the cache external id of a build (its pod name).
func BuildResourceID(buildID string) string { return buildName(buildID) }

func hasDotDot(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// BuildStorageSecret is the context bucket's settings (endpoint, bucket, region, accessKey,
// secretKey), kept as sandbox-system/sandbox-build-s3 on the sandbox cluster.
func (s *Service) BuildStorageSecret(ctx context.Context) (map[string][]byte, error) {
	sec, err := s.api.GetSecretData(ctx, pullSecretSourceNS, "sandbox-build-s3")
	if err == nil && sec == nil {
		err = errors.New("sandbox-system/sandbox-build-s3 is missing")
	}
	return sec, err
}

// SnapshotRequest appends a sandbox's filesystem changes (one gzip tar) as a layer on Base.
type SnapshotRequest struct {
	ProjectID, TemplateID, BuildID string
	Base                           string // the sandbox's image (pinned)
	Image                          string // destination ref, without digest
	LayerURL                       string // where the fetch step downloads the layer (proxcloud, per-build token)
}

// SnapshotObjects renders the pod that turns a snapshot layer into an image: fetch the layer,
// then `crane append` it onto the sandbox's image and push. No customer code runs in it.
func SnapshotObjects(cfg Config, req SnapshotRequest) map[string]any {
	cfg = cfg.WithDefaults()
	labels := map[string]any{
		LabelManagedBy: ManagedByValue, LabelProject: req.ProjectID,
		LabelBuild: req.BuildID, LabelTemplateID: req.TemplateID, "proxcloud.io/sandbox-kind": "snapshot",
	}
	small := map[string]any{"cpu": "100m", "memory": "128Mi"}
	work := map[string]any{"cpu": "1", "memory": "1Gi", "ephemeral-storage": buildDiskLimit}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": buildName(req.BuildID), "namespace": BuildNamespace, "labels": labels},
		"spec": map[string]any{
			"runtimeClassName": cfg.RuntimeClass,
			// A paused sandbox only exists as this snapshot: retry transient fetch/push failures
			// (kubelet backoff) until the deadline instead of failing on the first one.
			"restartPolicy":                 "OnFailure",
			"automountServiceAccountToken":  false,
			"enableServiceLinks":            false,
			"activeDeadlineSeconds":         int64(BuildTimeout.Seconds()),
			"terminationGracePeriodSeconds": int64(5),
			"initContainers": []any{map[string]any{
				"name": "fetch", "image": fetchImage,
				"command":      []any{"sh", "-c", `set -eu; echo "fetching snapshot layer"; curl -fsS --retry 3 -o /work/layer.tar.gz "$LAYER_URL"; ls -l /work/layer.tar.gz`},
				"env":          []any{map[string]any{"name": "LAYER_URL", "value": req.LayerURL}},
				"resources":    map[string]any{"requests": small, "limits": small},
				"volumeMounts": []any{mount("work", "/work")},
			}},
			"containers": []any{map[string]any{
				"name": "push", "image": craneDebugImage,
				"command": []any{"sh", "-c", `set -eu; crane append --platform linux/amd64 -b "$BASE" -f /work/layer.tar.gz -t "$DEST" > /dev/termination-log; cat /dev/termination-log`},
				"env": []any{
					map[string]any{"name": "BASE", "value": req.Base}, map[string]any{"name": "DEST", "value": req.Image},
					map[string]any{"name": "DOCKER_CONFIG", "value": "/docker"},
				},
				"resources":    map[string]any{"requests": small, "limits": work},
				"volumeMounts": []any{mount("work", "/work"), mount("push-secret", "/docker")},
			}},
			"volumes": []any{
				map[string]any{"name": "work", "emptyDir": map[string]any{"sizeLimit": buildDiskLimit}},
				map[string]any{"name": "push-secret", "secret": map[string]any{
					"secretName": buildPushSecret,
					"items":      []any{map[string]any{"key": ".dockerconfigjson", "path": "config.json"}},
				}},
			},
		},
	}
}

// StartSnapshot creates the snapshot pod.
func (s *Service) StartSnapshot(ctx context.Context, req SnapshotRequest) error {
	if err := s.api.ApplyCore(ctx, "pods", SnapshotObjects(s.cfg, req)); err != nil {
		return fmt.Errorf("sandbox: snapshot pod: %w", err)
	}
	return nil
}

// SnapshotFS streams the sandbox's filesystem changes since boot (gzip tar) into w, by running
// `sandbox-init -snapshot` in it.
func (s *Service) SnapshotFS(ctx context.Context, projectID, id string, w io.Writer) error {
	obj, err := s.get(ctx, projectID, id)
	if err != nil {
		return err
	}
	if !conditionTrue(obj, "Ready") {
		return ErrNotRunning
	}
	code, stderr, err := s.api.ExecStream(ctx, NamespaceFor(projectID), id, "main", []string{RuntimeMountPath + "/sandbox-init", "-snapshot"}, w, 4096)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("sandbox: snapshot exited %d: %s", code, strings.TrimSpace(stderr))
	}
	return nil
}

// SavedSandbox is what resuming a paused sandbox needs: its CR as it was, minus server state.
func (s *Service) SavedSandbox(ctx context.Context, projectID, id string) (map[string]any, error) {
	obj, err := s.get(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	meta, _ := obj["metadata"].(map[string]any)
	keep := map[string]any{}
	for _, k := range []string{"name", "namespace", "labels", "annotations"} {
		if v, ok := meta[k]; ok {
			keep[k] = v
		}
	}
	if ann, ok := keep["annotations"].(map[string]any); ok {
		delete(ann, AnnotationRunningSince) // the resumed sandbox starts billing anew
	}
	return map[string]any{"apiVersion": obj["apiVersion"], "kind": obj["kind"], "metadata": keep, "spec": obj["spec"]}, nil
}

// ResumeSandbox recreates a paused sandbox from its saved CR on image (the snapshot), ending at
// shutdown, with its egress policy. The sandbox keeps its id, tokens, env and metadata.
func (s *Service) ResumeSandbox(ctx context.Context, projectID string, saved map[string]any, image string, shutdown time.Time) error {
	cr := saved
	if digStr(cr, "metadata", "namespace") != NamespaceFor(projectID) || digStr(cr, "metadata", "labels", LabelProject) != projectID {
		return ErrNotFound
	}
	id := digStr(cr, "metadata", "name")
	if !ValidID(id) {
		return ErrNotFound
	}
	if err := s.EnsureProjectNamespace(ctx, projectID, nil); err != nil {
		return err
	}
	if n := gpuCount(cr); n > 0 {
		if err := s.checkGPUCapacity(ctx, &GPURequest{Model: digStr(cr, "spec", "podTemplate", "spec", "nodeSelector", LabelGPU), Count: n}); err != nil {
			return err
		}
	}
	if c := firstContainer(cr); len(c) > 0 {
		c["image"] = image
	}
	// The snapshot image is private even when the sandbox started from a public one.
	if ps, ok := dig(cr, "spec", "podTemplate", "spec").(map[string]any); ok && s.cfg.fromTemplateRegistry(image) {
		ps["imagePullSecrets"] = []any{map[string]any{"name": TemplatePullSecret}}
	}
	if spec, ok := cr["spec"].(map[string]any); ok {
		spec["shutdownTime"] = shutdown.UTC().Format(time.RFC3339)
	}
	if err := s.api.ApplyCR(ctx, CRGroup, CRVersion, CRPlural, cr); err != nil {
		return fmt.Errorf("sandbox: resume: %w", err)
	}
	var n *Network
	if raw := digStr(cr, "metadata", "annotations", AnnotationNetwork); raw != "" {
		n = &Network{}
		_ = json.Unmarshal([]byte(raw), n)
	}
	return s.applyEgress(ctx, projectID, id, n)
}

// CloneSaved turns a saved sandbox into a new one (fork): new id, new envd and traffic tokens,
// same template, size, env, metadata and network. The result is ready for ResumeSandbox.
func CloneSaved(saved map[string]any) (clone map[string]any, id string) {
	b, _ := json.Marshal(saved)
	_ = json.Unmarshal(b, &clone)
	id = newSandboxID()
	if meta, ok := clone["metadata"].(map[string]any); ok {
		meta["name"] = id
	}
	if pm, ok := dig(clone, "spec", "podTemplate", "metadata").(map[string]any); ok {
		if l, ok := pm["labels"].(map[string]any); ok {
			l[LabelSandboxID] = id
		}
		if a, ok := pm["annotations"].(map[string]any); ok && a[AnnotationTrafficToken] != nil {
			a[AnnotationTrafficToken] = newToken()
		}
	}
	if env, ok := firstContainer(clone)["env"].([]any); ok {
		for _, e := range env {
			if m, ok := e.(map[string]any); ok && str(m["name"]) == "PROXCLOUD_ENVD_ACCESS_TOKEN" {
				m["value"] = newToken()
			}
		}
	}
	return clone, id
}

// TemplateOfSaved is the custom-template view of a saved sandbox (for snapshot templates): its
// size, GPU and the start/ready/user/workdir settings sandbox-init received.
func TemplateOfSaved(saved map[string]any) Template {
	c := firstContainer(saved)
	t := Template{CPU: intOf(dig(c, "resources", "limits", "cpu")), MemoryGiB: memGiB(dig(c, "resources", "limits", "memory"))}
	if n := gpuCount(saved); n > 0 {
		t.GPU = &GPURequest{Model: digStr(saved, "spec", "podTemplate", "spec", "nodeSelector", LabelGPU), Count: n}
	}
	if env, ok := c["env"].([]any); ok {
		for _, e := range env {
			m, _ := e.(map[string]any)
			switch str(m["name"]) {
			case "PROXCLOUD_START_CMD":
				t.StartCmd = str(m["value"])
			case "PROXCLOUD_READY_CMD":
				t.ReadyCmd = str(m["value"])
			case "PROXCLOUD_DEFAULT_USER":
				t.User = str(m["value"])
			case "PROXCLOUD_DEFAULT_WORKDIR":
				t.Workdir = str(m["value"])
			case "PROXCLOUD_START_AS_ROOT":
				t.StartAsRoot = str(m["value"]) == "1"
			}
		}
	}
	return t
}

// ImageOfSaved is the image a saved sandbox runs.
func ImageOfSaved(saved map[string]any) string { return str(firstContainer(saved)["image"]) }

// PayloadOf is the cache payload of a saved (or any) Sandbox CR.
func PayloadOf(cr map[string]any) map[string]any { return sandboxData(cr) }
