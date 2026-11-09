package project

// e2b_template_base.go — what a template build starts FROM: a public image, a private image with the caller's registry
// credentials (fromImageRegistry), a built-in template, or one of the project's own templates (fromTemplate). The build
// pod's `base` init container pulls it; a credential is only ever mounted there (never into kaniko, where the caller's
// RUN steps execute).
//
// fromImageRegistry type "aws" is not offered: E2B exchanges the caller's AWS keys for an ECR token server side, which
// needs the AWS SDK's ECR client for a cloud ProxCloud does not run on. An ECR image still builds with type "registry",
// username "AWS" and the output of `aws ecr get-login-password` (valid 12 h) as the password (docs/e2b-compat.md).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// fromRegistry is E2B's FromImageRegistry (discriminated by type: registry | aws | gcp).
type fromRegistry struct {
	Type               string `json:"type"`
	Username           string `json:"username"`
	Password           string `json:"password"`
	ServiceAccountJSON string `json:"serviceAccountJson"`
}

// buildBase is the resolved base of a build.
type buildBase struct {
	Ref              string
	Auth             sandbox.BaseAuth
	DockerConfig     []byte         // for BaseAuthUser
	Parent           *templateBuild // what the child inherits (a built-in or a project template)
	ParentTemplateID string
}

const dockerHubAuthKey = "https://index.docker.io/v1/"

// errBase is a 400 for the build request.
func errBase(format string, a ...any) error {
	return fmt.Errorf("%w: %s", sandbox.ErrBadBuild, fmt.Sprintf(format, a...))
}

// imageRepoName is the repository part of a ref without host and tag ("acme/app" of "ghcr.io/acme/app:1"). Hosts and
// tags may contain uppercase; repository names may not.
func imageRepoName(ref string) string {
	if h := registryHost(ref); h != "" {
		ref = strings.TrimPrefix(ref, h+"/")
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

// registryHost is the registry of an image ref, as docker config keys it ("" for Docker Hub).
func registryHost(ref string) string {
	first, _, found := strings.Cut(ref, "/")
	if !found || !(strings.ContainsAny(first, ".:") || first == "localhost") {
		return ""
	}
	return first
}

// registryDockerConfig turns fromImageRegistry into a dockerconfigjson for image's registry.
func registryDockerConfig(cfg sandbox.Config, image string, r *fromRegistry) ([]byte, error) {
	host := registryHost(image)
	if own := registryHost(strings.TrimRight(cfg.TemplateRegistry, "/") + "/x"); own != "" && host == own {
		return nil, errBase("fromImageRegistry cannot be used with the sandbox registry: use fromTemplate")
	}
	var auth string
	switch r.Type {
	case "registry":
		if r.Username == "" || r.Password == "" {
			return nil, errBase("fromImageRegistry: username and password are required")
		}
		auth = base64.StdEncoding.EncodeToString([]byte(r.Username + ":" + r.Password))
	case "gcp":
		if strings.TrimSpace(r.ServiceAccountJSON) == "" || !json.Valid([]byte(r.ServiceAccountJSON)) {
			return nil, errBase("fromImageRegistry: serviceAccountJson must be the service account key JSON")
		}
		auth = base64.StdEncoding.EncodeToString([]byte("_json_key:" + r.ServiceAccountJSON))
	case "aws":
		return nil, errBase(`fromImageRegistry: type aws is not supported here; use type registry with username "AWS" and the output of "aws ecr get-login-password" as the password`)
	default:
		return nil, errBase("fromImageRegistry: type must be registry or gcp")
	}
	key := host
	switch key {
	case "", "docker.io", "index.docker.io":
		key = dockerHubAuthKey // the key crane (ggcr) looks Docker Hub up under
	}
	return json.Marshal(map[string]any{"auths": map[string]any{key: map[string]string{"auth": auth}}})
}

// resolveBase decides what a build starts from. Exactly one of fromImage / fromTemplate (none = the default base). With
// fromTemplate, fromImageRegistry is ignored, as in E2B. An image on the sandbox registry (a built-in, or the project's
// own template) is pulled with the project's build account, which reads library/** and t/<project>/** only.
func (h *Handler) resolveBase(ctx context.Context, c *e2bCaller, fromImage, fromTemplate string, rawRegistry json.RawMessage) (*buildBase, error) {
	if fromImage != "" && fromTemplate != "" {
		return nil, errBase("set either fromImage or fromTemplate, not both")
	}
	cfg := c.ss.Config()
	onRegistry := func(ref string) sandbox.BaseAuth {
		own := registryHost(strings.TrimRight(cfg.TemplateRegistry, "/") + "/x")
		if own != "" && registryHost(ref) == own {
			return sandbox.BaseAuthRegistry
		}
		return sandbox.BaseAuthNone
	}
	builtin := func(bt sandbox.Template) *buildBase {
		// A child of a built-in keeps its runtime settings, like a child of a project template.
		return &buildBase{Ref: bt.Image, Auth: onRegistry(bt.Image),
			Parent: &templateBuild{StartCmd: bt.Start(), ReadyCmd: bt.ReadyCmd, User: bt.User, Workdir: bt.Workdir, StartAsRoot: bt.StartAsRoot}}
	}
	if fromTemplate != "" {
		if bt, ok := cfg.Templates[fromTemplate]; ok {
			return builtin(bt), nil
		}
		name, tag := splitNameTag(fromTemplate)
		t, err := h.loadTemplate(ctx, c.proj.ID, name)
		if err != nil {
			return nil, err
		}
		if t == nil || t.Internal {
			return nil, errBase("template '%s' not found", fromTemplate)
		}
		var bl *templateBuild
		if tag == "" || tag == "default" {
			bl = t.defaultBuild()
		} else if id, ok := t.Tags[tag]; ok {
			bl = t.build(id)
		}
		own := strings.TrimRight(cfg.TemplateRegistry, "/") + "/" + c.proj.ID + "/"
		if bl == nil || bl.Status != sandbox.BuildReady || !strings.HasPrefix(bl.Image, own) {
			return nil, errBase("template '%s' has no ready build", fromTemplate)
		}
		return &buildBase{Ref: bl.Image, Auth: sandbox.BaseAuthRegistry, Parent: bl, ParentTemplateID: t.ID}, nil
	}
	// No base given, or E2B's own base image (what the SDK's Template() / from_base_image() sends): the location's
	// "base" template, which is that image plus the ProxCloud conventions.
	noRegistry := len(rawRegistry) == 0 || string(rawRegistry) == "null"
	if fromImage == "" || fromImage == defaultBaseImage || fromImage == "e2bdev/base" {
		if bt, ok := cfg.Templates["base"]; ok && noRegistry {
			return builtin(bt), nil
		}
	}
	if fromImage == "" {
		fromImage = defaultBaseImage
	}
	// Repository names are lowercase (OCI distribution spec): reject now, not in the build pod.
	if repo, _, _ := strings.Cut(fromImage, "@"); strings.ToLower(imageRepoName(repo)) != imageRepoName(repo) {
		return nil, errBase("fromImage %q: the repository name must be lowercase", fromImage)
	}
	if noRegistry {
		if onRegistry(fromImage) == sandbox.BaseAuthRegistry {
			// The registry's own images by name: a built-in through fromTemplate, a project template likewise.
			return nil, errBase("fromImage %q is on the sandbox registry: use fromTemplate", fromImage)
		}
		return &buildBase{Ref: fromImage}, nil
	}
	var r fromRegistry
	if err := json.Unmarshal(rawRegistry, &r); err != nil {
		return nil, errBase("fromImageRegistry is not valid: %v", err)
	}
	dc, err := registryDockerConfig(cfg, fromImage, &r)
	if err != nil {
		return nil, err
	}
	return &buildBase{Ref: fromImage, Auth: sandbox.BaseAuthUser, DockerConfig: dc}, nil
}

// inheritFromParent fills what a child keeps from its parent (E2B semantics): user and workdir unless the child's
// steps set them, and the start/ready pair unless the child sets either. CPU and memory are the child's own.
func inheritFromParent(bl *templateBuild, parent *templateBuild, childStart, childReady string) {
	if parent == nil {
		return
	}
	if bl.User == "" {
		bl.User = parent.User
	}
	if bl.Workdir == "" {
		bl.Workdir = parent.Workdir
	}
	if childStart == "" && childReady == "" {
		bl.StartCmd, bl.ReadyCmd, bl.StartAsRoot = parent.StartCmd, parent.ReadyCmd, parent.StartAsRoot
	}
}
