package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GPURequest asks for Count whole GPUs of one model (GPUs are never shared between sandboxes).
type GPURequest struct {
	Model string `json:"model"`
	Count int    `json:"count"`
}

// Spec is one create request, validated and defaulted against the provider config.
type Spec struct {
	ID             string            `json:"-"`
	ProjectID      string            `json:"-"`
	Name           string            `json:"name"`
	Template       string            `json:"template"`
	CPU            int               `json:"cpu"`
	MemoryGiB      int               `json:"memoryGiB"`
	GPU            *GPURequest       `json:"gpu,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	Env            map[string]string `json:"env,omitempty"`
	// Metadata is free-form caller data (E2B `metadata`), returned on reads and usable as a list
	// filter. Not secret: never put credentials here.
	Metadata map[string]string `json:"metadata,omitempty"`
	// Network is the egress policy (nil = open internet; see network.go).
	Network *Network `json:"network,omitempty"`
	// AllowPublicTraffic=false makes every non-envd port require the traffic access token.
	AllowPublicTraffic *bool `json:"allowPublicTraffic,omitempty"`
	// AutoPause pauses the sandbox (filesystem snapshot) at its timeout instead of deleting it.
	AutoPause bool `json:"autoPause,omitempty"`
	// AutoResume (with AutoPause) lets traffic through sandbox-proxy resume the paused sandbox.
	AutoResume bool `json:"autoResume,omitempty"`

	// Custom is a project's own template, resolved by the caller (Template is then its name).
	Custom *Template `json:"-"`
	// Quota is the project's sandbox quota, set by the caller (nil = the provider limits only).
	Quota *ProjectQuota `json:"-"`

	// Minted by CreateSandbox, rendered into the CR by BuildCR; never decoded from callers.
	envdToken    string
	trafficToken string
}

// SpecFromData decodes the free-form create payload (CreateRequest.data).
func SpecFromData(projectID string, data map[string]any) (Spec, error) {
	var s Spec
	raw, err := json.Marshal(data)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("invalid sandbox payload: %w", err)
	}
	s.ProjectID = projectID
	return s, nil
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

const (
	maxEnvVars      = 64
	maxEnvValueSize = 4096
	maxNameLen      = 63
	maxMetadataKeys = 32
	maxMetadataKey  = 128
	maxMetadataVal  = 1024
	// MinTimeoutSeconds matches E2B (timeout >= 1 s). Billing still counts whole minutes.
	MinTimeoutSeconds = 1
)

// template is the spec's template: its custom one, else the provider's.
func (s Spec) template(cfg Config) (Template, bool) {
	if s.Custom != nil {
		return *s.Custom, true
	}
	t, ok := cfg.Templates[s.Template]
	return t, ok
}

// GPUs returns the number of GPUs this spec holds (0 for a CPU sandbox).
func (s Spec) GPUs() int {
	if s.GPU == nil {
		return 0
	}
	return s.GPU.Count
}

// Validate checks the request against the provider config and fills defaults in place
// (cpu/memory from the template, timeout from the provider default, GPU count 1).
func (s *Spec) Validate(cfg Config) error {
	cfg = cfg.WithDefaults()
	l := cfg.Limits
	s.Name = strings.TrimSpace(s.Name)
	if len(s.Name) > maxNameLen {
		return fmt.Errorf("name must be at most %d characters", maxNameLen)
	}
	t, ok := s.template(cfg)
	if !ok {
		return fmt.Errorf("unknown template %q (available: %s)", s.Template, strings.Join(cfg.TemplateNames(), ", "))
	}
	if s.CPU == 0 {
		s.CPU = max(t.CPU, 1)
	}
	if s.MemoryGiB == 0 {
		s.MemoryGiB = max(t.MemoryGiB, 1)
	}
	if s.CPU < 1 || s.CPU > l.MaxCPU {
		return fmt.Errorf("cpu must be between 1 and %d", l.MaxCPU)
	}
	if s.MemoryGiB < 1 || s.MemoryGiB > l.MaxMemoryGiB {
		return fmt.Errorf("memoryGiB must be between 1 and %d", l.MaxMemoryGiB)
	}
	if s.GPU == nil && t.GPU != nil {
		g := *t.GPU
		s.GPU = &g
	}
	if s.GPU != nil {
		if s.GPU.Count == 0 {
			s.GPU.Count = 1
		}
		if !cfg.hasGPUModel(s.GPU.Model) {
			return fmt.Errorf("unknown GPU model %q (available: %s)", s.GPU.Model, strings.Join(cfg.GPUModels, ", "))
		}
		if s.GPU.Count < 1 || s.GPU.Count > l.MaxGPUs {
			return fmt.Errorf("gpu.count must be between 1 and %d", l.MaxGPUs)
		}
	}
	if s.TimeoutSeconds == 0 {
		s.TimeoutSeconds = l.DefaultTimeoutSeconds
	}
	if s.TimeoutSeconds < MinTimeoutSeconds || s.TimeoutSeconds > l.MaxTimeoutSeconds {
		return fmt.Errorf("timeoutSeconds must be between %d and %d", MinTimeoutSeconds, l.MaxTimeoutSeconds)
	}
	if len(s.Env) > maxEnvVars {
		return fmt.Errorf("at most %d environment variables", maxEnvVars)
	}
	for k, v := range s.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("invalid environment variable name %q", k)
		}
		if len(v) > maxEnvValueSize {
			return fmt.Errorf("environment variable %s is longer than %d bytes", k, maxEnvValueSize)
		}
		if strings.HasPrefix(k, "PROXCLOUD_") {
			return fmt.Errorf("environment variable names starting with PROXCLOUD_ are reserved")
		}
	}
	if len(s.Metadata) > maxMetadataKeys {
		return fmt.Errorf("at most %d metadata keys", maxMetadataKeys)
	}
	for k, v := range s.Metadata {
		if k == "" || len(k) > maxMetadataKey || len(v) > maxMetadataVal {
			return fmt.Errorf("metadata keys must be 1-%d bytes and values at most %d bytes", maxMetadataKey, maxMetadataVal)
		}
	}
	return s.Network.Validate()
}

// ErrBadTimeout is returned by ShutdownTimeFor for an out-of-range timeout.
var ErrBadTimeout = errors.New("timeout out of range")

// ShutdownTimeFor is now+timeout, after checking it against the provider maximum.
func ShutdownTimeFor(cfg Config, now time.Time, timeoutSeconds int) (time.Time, error) {
	l := cfg.WithDefaults().Limits
	if timeoutSeconds < MinTimeoutSeconds || timeoutSeconds > l.MaxTimeoutSeconds {
		return time.Time{}, fmt.Errorf("%w: timeoutSeconds must be between %d and %d", ErrBadTimeout, MinTimeoutSeconds, l.MaxTimeoutSeconds)
	}
	return now.Add(time.Duration(timeoutSeconds) * time.Second).UTC().Truncate(time.Second), nil
}

// BuildCR renders the Sandbox custom resource for a validated spec.
//
// Security posture (customer code is untrusted):
//   - runtimeClassName = the provider's gVisor class: every syscall goes to gVisor's user-space
//     kernel; GPU ioctls go through nvproxy.
//   - no service account token, no service links: nothing in the sandbox can talk to the cluster.
//   - egress is fenced by the namespace's LabelTenant (cluster-wide Cilium policy).
//   - requests == limits: a sandbox never bursts into a neighbour's CPU or memory.
func BuildCR(cfg Config, serviceID string, s Spec, shutdown time.Time) map[string]any {
	cfg = cfg.WithDefaults()
	t, _ := s.template(cfg)
	labels := map[string]any{
		LabelProject:   s.ProjectID,
		LabelService:   serviceID,
		LabelManagedBy: ManagedByValue,
	}
	podLabels := map[string]any{LabelSandboxID: s.ID}
	for k, v := range labels {
		podLabels[k] = v
	}
	res := map[string]any{
		"cpu":    strconv.Itoa(s.CPU),
		"memory": fmt.Sprintf("%dGi", s.MemoryGiB),
	}
	podSpec := map[string]any{
		"runtimeClassName":              cfg.RuntimeClass,
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"terminationGracePeriodSeconds": int64(5),
	}
	if s.GPUs() > 0 {
		res[ResourceGPU] = strconv.Itoa(s.GPUs())
		podSpec["nodeSelector"] = map[string]any{LabelGPU: s.GPU.Model}
		podSpec["tolerations"] = []any{map[string]any{"key": ResourceGPU, "operator": "Exists", "effect": "NoSchedule"}}
	}
	cmd := t.Command
	if len(cmd) == 0 {
		// Keep the sandbox alive for exec; the image's own entrypoint often exits at once.
		cmd = []string{"sleep", "infinity"}
	}
	container := map[string]any{
		"name":      "main",
		"image":     t.Image,
		"command":   toAnySlice(cmd),
		"resources": map[string]any{"requests": res, "limits": res},
	}
	var env []any
	for _, k := range sortedKeys(s.Env) {
		env = append(env, map[string]any{"name": k, "value": s.Env[k]})
	}
	if cfg.Envd {
		// sandbox-init (PID 1) creates the default user, starts envd and POSTs envd's /init over
		// localhost, so the control-plane routes never have to be reachable from outside the pod.
		// The template command, if any, becomes envd's start command.
		container["command"] = []any{RuntimeMountPath + "/sandbox-init"}
		start := t.Start()
		for _, kv := range [][2]string{
			{"PROXCLOUD_START_CMD", start}, {"PROXCLOUD_READY_CMD", t.ReadyCmd},
			{"PROXCLOUD_DEFAULT_USER", t.User}, {"PROXCLOUD_DEFAULT_WORKDIR", t.Workdir},
		} {
			if kv[1] != "" {
				env = append(env, map[string]any{"name": kv[0], "value": kv[1]})
			}
		}
		if t.StartAsRoot {
			env = append(env, map[string]any{"name": "PROXCLOUD_START_AS_ROOT", "value": "1"})
		}
		// sandbox-init must be PID 1 as root whatever USER the image ends with.
		container["securityContext"] = map[string]any{"runAsUser": int64(0), "runAsGroup": int64(0)}
		if s.envdToken != "" {
			env = append(env, map[string]any{"name": "PROXCLOUD_ENVD_ACCESS_TOKEN", "value": s.envdToken})
		}
		container["volumeMounts"] = []any{map[string]any{"name": "proxcloud-runtime", "mountPath": RuntimeMountPath, "readOnly": true}}
		// Ready only once envd is initialised: an exec probe, so the kubelet never needs network
		// access to the sandbox (its ingress admits sandbox-proxy only).
		container["startupProbe"] = map[string]any{
			"exec":             map[string]any{"command": []any{RuntimeMountPath + "/sandbox-init", "-ready"}},
			"periodSeconds":    int64(1),
			"failureThreshold": int64(startupBudget(t)),
		}
		// The same marker afterwards: sandbox-init drops it when envd dies and puts it back only once the restarted
		// envd has its access token, so sandbox-proxy (Ready pods only) never routes to an envd without one.
		container["readinessProbe"] = map[string]any{
			"exec":             map[string]any{"command": []any{RuntimeMountPath + "/sandbox-init", "-ready"}},
			"periodSeconds":    int64(1),
			"failureThreshold": int64(1),
		}
		podSpec["volumes"] = []any{map[string]any{
			"name":     "proxcloud-runtime",
			"hostPath": map[string]any{"path": RuntimeHostDir, "type": "Directory"},
		}}
	}
	if len(env) > 0 {
		container["env"] = env
	}
	if cfg.pullsFromRegistry(t.Image) {
		podSpec["imagePullSecrets"] = []any{map[string]any{"name": TemplatePullSecret}}
	}
	podSpec["containers"] = []any{container}
	podMeta := map[string]any{"labels": podLabels}
	if s.trafficToken != "" {
		podMeta["annotations"] = map[string]any{AnnotationTrafficToken: s.trafficToken}
	}
	annotations := map[string]any{
		AnnotationDisplayName: s.Name,
		AnnotationTemplate:    s.Template,
	}
	if len(s.Metadata) > 0 {
		b, _ := json.Marshal(s.Metadata)
		annotations[AnnotationMetadata] = string(b)
	}
	if s.Network != nil {
		b, _ := json.Marshal(s.Network)
		annotations[AnnotationNetwork] = string(b)
	}
	if s.AutoPause {
		annotations[AnnotationAutoPause] = "true"
		annotations[AnnotationEndAt] = shutdown.Format(time.RFC3339)
		shutdown = shutdown.Add(AutoPauseGrace)
		if s.AutoResume {
			annotations[AnnotationAutoResume] = "true"
			annotations[AnnotationTimeoutSeconds] = strconv.Itoa(s.TimeoutSeconds)
		}
	}
	return map[string]any{
		"apiVersion": CRGroup + "/" + CRVersion,
		"kind":       "Sandbox",
		"metadata": map[string]any{
			"name":        s.ID,
			"namespace":   NamespaceFor(s.ProjectID),
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": map[string]any{
			"shutdownTime":   shutdown.Format(time.RFC3339),
			"shutdownPolicy": "Delete",
			"podTemplate": map[string]any{
				"metadata": podMeta,
				"spec":     podSpec,
			},
		},
	}
}

// startupBudget is how many 1-second startup probes a sandbox gets: 2 minutes, or 10 when the
// template has a ready command (it may wait for a server to come up).
func startupBudget(t Template) int {
	if t.ReadyCmd != "" {
		return 600
	}
	return 120
}

// shellJoin quotes argv for `sh -c`.
func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
	}
	return strings.Join(q, " ")
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
