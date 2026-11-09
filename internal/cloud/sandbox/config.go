// Package sandbox drives the `sandbox` provider: short-lived, isolated customer environments
// (CPU or GPU) that run as gVisor pods on a dedicated Kubernetes cluster. One sandbox is one
// agent-sandbox `Sandbox` CR (agents.x-k8s.io/v1beta1) in the project's sb-<projectId>
// namespace. The cluster-side contract (runtime classes, device plugin, egress guardrail, the
// proxcloud service account) is deploy/sandbox-cluster/ (added in SB2a) and docs/managed-sandbox.md.
package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Sandbox CR coordinates (kubernetes-sigs/agent-sandbox).
const (
	CRGroup   = "agents.x-k8s.io"
	CRVersion = "v1beta1"
	CRPlural  = "sandboxes"
)

// Labels and annotations proxcloud stamps on everything it creates. The sync lists ONLY objects
// carrying the managed-by marker and this service's label; delete/patch refuse anything else.
const (
	LabelProject          = "proxcloud.io/project"
	LabelService          = "proxcloud.io/service"
	LabelManagedBy        = "app.kubernetes.io/managed-by"
	ManagedByValue        = "proxcloud"
	AnnotationDisplayName = "proxcloud.io/display-name"
	AnnotationTemplate    = "proxcloud.io/template"
	// LabelTenant selects the namespace into the cluster-wide egress guardrail (SB0 spike policy;
	// deploy/sandbox-cluster/network-policy.yaml from SB2a). Without it a sandbox has unrestricted egress.
	LabelTenant = "proxcloud.io/tenant"
	// LabelGPU is the node label carrying a GPU node group's model alias (e.g. nvidia-3090). GPU
	// sandboxes select on it. node-restriction.kubernetes.io/ is the one prefix a kubelet cannot
	// set on its own Node once the NodeRestriction admission plugin is on, so a compromised node
	// cannot claim the label and draw other tenants' GPU sandboxes to itself.
	LabelGPU = "node-restriction.kubernetes.io/proxcloud-gpu"
	// ResourceGPU is the device-plugin resource name.
	ResourceGPU = "nvidia.com/gpu"
)

// NamespaceFor is the project's namespace on the sandbox cluster. Distinct from kamaji's pc-
// and dbaas's pc-db- so no prefix glob of one provider can ever match another's namespace.
func NamespaceFor(projectID string) string { return "sb-" + projectID }

// newSandboxID mints the platform identifier (CR name, cache external id). The sbx- prefix keeps
// billing rows and audit logs unambiguous next to kamaji's pc-c- and dbaas's pc-d-. 64 random bits:
// the ID is also the sandbox's host name (<port>-<id>.<domain>), so it must not be guessable.
func newSandboxID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "sbx-" + hex.EncodeToString(b)
}

var sandboxIDRe = regexp.MustCompile(`^sbx-[0-9a-f]{8}([0-9a-f]{8})?$`)

// ValidID reports a well-formed sandbox ID (8 hex digits before 0.9, 16 since).
func ValidID(id string) bool { return sandboxIDRe.MatchString(id) }

// Template is one curated environment customers can start a sandbox from.
type Template struct {
	// EgressMbps overrides the provider default (0 inherits); operator-curated templates only.
	EgressMbps  int      `json:"egressMbps,omitempty"`
	Image       string   `json:"image"`
	Description string   `json:"description,omitempty"`
	CPU         int      `json:"cpu"`       // default vCPUs
	MemoryGiB   int      `json:"memoryGiB"` // default memory
	Command     []string `json:"command,omitempty"`
	// GPU, when set, is the default GPU of sandboxes from this template (how E2B callers, whose
	// API has no GPU field, get one: pick a GPU template such as "pytorch-gpu").
	GPU *GPURequest `json:"gpu,omitempty"`
	// Run at boot by sandbox-init, as the template's user in its workdir (StartAsRoot: as root, for
	// services such as code-interpreter's Jupyter server); the sandbox is reported ready once
	// ReadyCmd exits 0. Custom templates get them from their build; built-in ones from the
	// provider config.
	StartCmd    string `json:"startCmd,omitempty"`
	ReadyCmd    string `json:"readyCmd,omitempty"`
	User        string `json:"user,omitempty"`
	Workdir     string `json:"workdir,omitempty"`
	StartAsRoot bool   `json:"startAsRoot,omitempty"`
}

// Start is the command sandbox-init runs at boot: StartCmd, else the legacy Command.
func (t Template) Start() string {
	if t.StartCmd == "" && len(t.Command) > 0 {
		return shellJoin(t.Command)
	}
	return t.StartCmd
}

// Limits caps what one sandbox and one project may ask for.
type Limits struct {
	MaxCPU                int `json:"maxCpu"`
	MaxMemoryGiB          int `json:"maxMemoryGiB"`
	MaxGPUs               int `json:"maxGpus"` // per sandbox
	MaxTimeoutSeconds     int `json:"maxTimeoutSeconds"`
	DefaultTimeoutSeconds int `json:"defaultTimeoutSeconds"`
	MaxConcurrent         int `json:"maxConcurrent"`     // live sandboxes per project
	MaxConcurrentGPUs     int `json:"maxConcurrentGpus"` // GPUs held at once per project
}

// Defaults applied when the provider document leaves a limit unset (0).
var defaultLimits = Limits{
	MaxCPU:                8,
	MaxMemoryGiB:          32,
	MaxGPUs:               1,
	MaxTimeoutSeconds:     24 * 3600,
	DefaultTimeoutSeconds: 3600,
	MaxConcurrent:         10,
	MaxConcurrentGPUs:     1,
}

// Config is one sandbox provider (one sandbox cluster).
type Config struct {
	EgressMbps   int
	Abuse        AbusePolicy
	Rotation     RotationPolicy
	Kubeconfig   string
	Region       string
	RuntimeClass string
	Templates    map[string]Template
	GPUModels    []string // offered GPU model aliases; each must exist as a LabelGPU node label
	Limits       Limits
	Startup      StartupConfig
	Build        BuildConfig
	// Envd runs E2B's envd in every sandbox (via sandbox-init from the node runtime dir), which is
	// what the E2B-compatible API and SDKs talk to. Requires the node runtime installed by
	// deploy/sandbox-cluster (proxcloud-sandbox-runtime, SB2a) and sandbox-proxy serving Domain.
	Envd bool
	// Domain is the E2B_DOMAIN of this provider: sandboxes are reached at
	// <port>-<sandboxID>.<Domain> through sandbox-proxy.
	Domain string
	// RegistryPrune turns on deleting unreferenced images from the template registry:
	// "" or "off", "dry-run" (log only), "on". RegistryGC holds the registry account it uses
	// (secret.registryGcUser / registryGcPassword: artifact list/read/delete only).
	RegistryPrune string
	RegistryGC    RegistryCredential
	// RegistryCA is the PEM CA the template registry's certificate chains to (config.sandbox.registryCa; empty: the
	// system roots). The portal's pruning reaches the registry with it.
	RegistryCA string
	// TemplateRegistry is the repository prefix custom templates and snapshots are pushed under,
	// e.g. registry.example.org/sandbox-templates (config.sandbox.templateRegistry). An image
	// under it gets the template pull secret. Empty: no template registry, so no image counts as
	// one of its own.
	TemplateRegistry string
	// ImageAdoption moves a built-in template to the image digest the prepull DaemonSets run once
	// every node has pulled it (Adoptions): "" (off), "dry-run" (log only) or "on".
	ImageAdoption string
	// ProxyWakeToken is the bearer sandbox-proxy sends to wake a paused sandbox (secret
	// proxyWakeToken). Empty = autoResume is refused.
	ProxyWakeToken string
	// BuildStorage is the bucket custom template builds keep their COPY contexts in (config.sandbox.buildStorage,
	// keys secret.buildStorageAccessKey / buildStorageSecretKey). Empty Bucket: custom template builds are off.
	BuildStorage BuildStorage
}

// BuildStorage is an S3 bucket the portal writes build contexts to and build pods read them from (presigned GETs).
type BuildStorage struct {
	Endpoint string // as the portal reaches it
	// PodEndpoint is the endpoint as the sandbox nodes reach it, which presigned URLs name (default Endpoint).
	PodEndpoint          string
	Bucket, Region       string
	AccessKey, SecretKey string
}

// RegistryCredential is a template registry robot (never logged, never mounted into pods).
type RegistryCredential struct{ User, Password string }

// AutoPauseGrace is how long an auto-pause sandbox outlives its end if pausing it fails.
const AutoPauseGrace = 15 * time.Minute

// Node runtime contract (deploy/sandbox-cluster/README.md, added in SB2a).
const (
	RuntimeHostDir   = "/opt/proxcloud-sandbox/bin"
	RuntimeMountPath = "/.proxcloud"
	EnvdPort         = 49983
	// EnvdVersion is the envd release vendored by deploy/sandbox-runtime.Dockerfile (ported later). The E2B SDK
	// refuses sandboxes whose envd is older than it supports, so this must track that pin.
	EnvdVersion = "0.10.2"
	// AnnotationTrafficToken on the pod: when set, sandbox-proxy requires it on every port but envd.
	AnnotationTrafficToken = "proxcloud.io/sandbox-traffic-token"
	AnnotationMetadata     = "proxcloud.io/metadata"
	AnnotationNetwork      = "proxcloud.io/network"
	// AnnotationRunningSince pins the first time proxcloud saw the sandbox Ready. Billing starts
	// there; a later container restart (Ready flapping) must not move it.
	AnnotationRunningSince = "proxcloud.io/running-since"
	// AnnotationAutoPause / AnnotationEndAt: E2B autoPause. The sandbox should end at end-at;
	// proxcloud pauses it then (snapshot + delete). shutdownTime is end-at + AutoPauseGrace, so the
	// controller only deletes it if pausing failed for that long.
	AnnotationAutoPause = "proxcloud.io/auto-pause"
	AnnotationEndAt     = "proxcloud.io/end-at"
	// AnnotationAutoResume: E2B autoResume. Traffic to the paused sandbox through sandbox-proxy
	// resumes it, for AnnotationTimeoutSeconds (the create timeout).
	AnnotationAutoResume     = "proxcloud.io/auto-resume"
	AnnotationTimeoutSeconds = "proxcloud.io/timeout-seconds"
)

// WithDefaults fills unset fields.
func (c Config) WithDefaults() Config {
	c.Startup = c.Startup.withDefaults()
	c.Build = c.Build.withDefaults()
	if c.EgressMbps == 0 {
		c.EgressMbps = 100
	}
	c.Abuse = c.Abuse.WithDefaults()
	if c.RuntimeClass == "" {
		c.RuntimeClass = "gvisor"
	}
	d := defaultLimits
	l := &c.Limits
	for _, f := range []struct {
		v   *int
		def int
	}{
		{&l.MaxCPU, d.MaxCPU}, {&l.MaxMemoryGiB, d.MaxMemoryGiB}, {&l.MaxGPUs, d.MaxGPUs},
		{&l.MaxTimeoutSeconds, d.MaxTimeoutSeconds}, {&l.DefaultTimeoutSeconds, d.DefaultTimeoutSeconds},
		{&l.MaxConcurrent, d.MaxConcurrent}, {&l.MaxConcurrentGPUs, d.MaxConcurrentGPUs},
	} {
		if *f.v <= 0 {
			*f.v = f.def
		}
	}
	if l.DefaultTimeoutSeconds > l.MaxTimeoutSeconds {
		l.DefaultTimeoutSeconds = l.MaxTimeoutSeconds
	}
	return c
}

// Validate checks the provider document. Run by New, so a broken provider is reported as
// "not available" instead of half-creating sandboxes.
func (c Config) Validate() error {
	var errs []string
	if c.EgressMbps < 0 || c.EgressMbps > 100000 {
		errs = append(errs, "sandbox.egressMbps must be 1-100000 (0 defaults to 100)")
	}
	if err := c.Abuse.Validate(); err != nil {
		errs = append(errs, err.Error())
	}
	if err := c.ValidateAutomaticActions(); err != nil {
		errs = append(errs, err.Error())
	}
	if err := c.Rotation.Validate(); err != nil {
		errs = append(errs, err.Error())
	}
	if strings.TrimSpace(c.Kubeconfig) == "" {
		errs = append(errs, "secret.kubeconfig is required")
	}
	if len(c.Templates) == 0 {
		errs = append(errs, "sandbox.templates: at least one template is required")
	}
	for name, t := range c.Templates {
		if t.EgressMbps < 0 || t.EgressMbps > 100000 {
			errs = append(errs, "template egressMbps must be 0-100000")
		}
		if !validName(name) {
			errs = append(errs, fmt.Sprintf("sandbox.templates: invalid template name %q", name))
		}
		if strings.TrimSpace(t.Image) == "" {
			errs = append(errs, fmt.Sprintf("sandbox.templates.%s.image is required", name))
		}
	}
	for _, m := range c.GPUModels {
		if !validName(m) {
			errs = append(errs, fmt.Sprintf("sandbox.gpuModels: invalid model alias %q", m))
		}
	}
	if len(errs) > 0 {
		return errors.New("sandbox provider: " + strings.Join(errs, "; "))
	}
	return nil
}

// TemplateNames returns the template names sorted (stable catalog order for the API).
func (c Config) TemplateNames() []string {
	out := make([]string, 0, len(c.Templates))
	for n := range c.Templates {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// fromTemplateRegistry reports an image under this provider's template registry.
func (c Config) fromTemplateRegistry(image string) bool {
	return c.TemplateRegistry != "" && strings.HasPrefix(image, strings.TrimRight(c.TemplateRegistry, "/")+"/")
}

// pullsFromRegistry reports an image on the template registry's host: the built-in library/** images as well as the
// project's own t/<project>/**. The pod then names the project's pull Secret. The kubelet checks that Secret even for an
// image a prepull pod already put on the node (KubeletEnsureSecretPulledImages), so a library image needs it too.
func (c Config) pullsFromRegistry(image string) bool {
	reg := strings.TrimRight(c.TemplateRegistry, "/")
	if reg == "" {
		return false
	}
	host, _, _ := strings.Cut(reg, "/")
	return strings.HasPrefix(image, host+"/")
}

func (c Config) hasGPUModel(m string) bool {
	for _, g := range c.GPUModels {
		if g == m {
			return true
		}
	}
	return false
}

// validName: a DNS-label-safe identifier (lowercase alphanumerics and '-', 1–63, no edge dash).
func validName(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
