// Package function drives serverless functions: a customer's container image run as a gVisor
// Deployment that scales from zero to maxInstances on traffic, on the sandbox provider's cluster
// (config.sandbox.functions).
// One function is one apps/v1 Deployment named by the function id in the project's fn-<projectId>
// namespace. The package knows the cluster only; records (functions, function_deployments), the
// wake/reap control flow and billing live in the platform layer.
package function

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/sandboxenv"
)

// Labels and annotations on a function's Deployment. The project/service/managed-by labels are
// the sandbox ones; sandbox.LabelSandboxID is deliberately never set, so no sandbox selector, sandbox
// network policy or network guard can match a function.
const (
	// LabelID is on the Deployment and its pods (it is also the Deployment's selector).
	LabelID = sandbox.LabelFunctionID
	// LabelHost (Deployment only) is the first label of the function's host: how sandbox-proxy finds
	// the function for a request.
	LabelHost = "proxcloud.io/function-host"
	// LabelTenant marks the namespace for the function network policy, beside sandbox.LabelTenant.
	LabelTenant = sandbox.LabelFunctionTenant
	// AnnotationPort / AnnotationSuspended are read by sandbox-proxy. The port is on the Deployment and
	// on every pod: during a rollout that changes it, each pod is reached on the port of its own
	// snapshot. Suspended (Deployment only) is "true" or absent.
	AnnotationPort      = "proxcloud.io/function-port"
	AnnotationSuspended = "proxcloud.io/function-suspended"
	// AnnotationSecrets (pod template only) carries Snapshot.SecretsVersion: a change of the
	// function's secrets changes the template, so it rolls out like any other change.
	AnnotationSecrets = "proxcloud.io/function-secrets-version"
	// AnnotationRegistryAuth (pod template only) carries RegistryAuth.Version, for the same
	// reason: new registry credentials roll out, and the rollout proves them with a pull (of an
	// image that cannot be pulled without them: a mirror in front of the registry serves a
	// public image without looking at them, and nothing else checks them).
	AnnotationRegistryAuth = "proxcloud.io/function-registry-auth-version"
	// QuotaName is the ResourceQuota of a project's function namespace.
	QuotaName = "proxcloud-function"
)

// ExposurePrivate is the only exposure a function has: it answers at its host on the sandbox proxy's
// address, as far as the site routes that address. A public endpoint is not offered (ErrNotOffered).
const ExposurePrivate = "private"

// DefaultConcurrency is the target of requests in flight per instance of a function that sets
// none, and MaxConcurrency the highest a function may set.
const (
	DefaultConcurrency = 50
	MaxConcurrency     = 1000
)

// NamespaceFor is the project's function namespace on the sandbox cluster. Not the sandbox
// namespace (sb-): functions get their own quota, their own role and their own network policy.
func NamespaceFor(projectID string) string { return sandbox.FunctionNamespace(projectID) }

// NewID mints a function id (Deployment name, record id).
func NewID() string { return "fn-" + randHex(8) }

var idRe = regexp.MustCompile(`^fn-[0-9a-f]{16}$`)

// ValidID reports a well-formed function id. IDs come from URLs: anything else must never reach
// a Kubernetes API path.
func ValidID(id string) bool { return idRe.MatchString(id) }

// NewHost mints a function's host, <name>-<6 hex>.<domain>. The caller stores it and
// retries on a collision: a host is never handed out twice.
func NewHost(name, domain string) string { return name + "-" + randHex(3) + "." + domain }

// HostLabel is the first DNS label of a host (the LabelHost value).
func HostLabel(host string) string {
	l, _, _ := strings.Cut(host, ".")
	return l
}

var hostLabelRe = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?-[0-9a-f]{6}$`)

// ValidHostLabel reports the first label of a function host: a function name, a dash and six hex digits. It starts
// with a letter and a sandbox's host label (<port>-<sandbox id>) with a digit, which is how one proxy on one domain
// tells the two apart. Anything built from a request's Host header is matched against it before it reaches a selector.
func ValidHostLabel(label string) bool { return hostLabelRe.MatchString(label) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Snapshot is what one deployment runs: the immutable part of a function_deployments record.
type Snapshot struct {
	Image     string `json:"image"`
	Port      int    `json:"port"`
	CPUMillis int    `json:"cpuMillis"`
	MemoryMi  int    `json:"memoryMi"`
	// Env values, Command and Args reach the container as they are written: the manifest escapes what the kubelet
	// would expand ($(NAME), $$).
	Env     map[string]string `json:"env,omitempty"`
	Command []string          `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	// SecretNames (sorted) and SecretsVersion are all a snapshot knows of the function's secrets:
	// which names it runs with, and an opaque marker that changes whenever a secret is set or
	// removed ("" = no secrets). The values are never part of a snapshot. Both are set by the
	// platform, never taken from a request.
	SecretNames    []string `json:"secretNames,omitempty"`
	SecretsVersion string   `json:"secretsVersion,omitempty"`
	// RegistryAuth says which credentials Image is pulled with, when its registry is private.
	// Absent for a public image.
	RegistryAuth *RegistryAuth `json:"registryAuth,omitempty"`
}

// RegistryAuth is all a snapshot knows of the credentials its image is pulled with: the registry
// they are for, the user, and an opaque marker that changes whenever they are set. The password
// is never part of a snapshot. Version is set by the platform, never taken from a request.
type RegistryAuth struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Version  string `json:"version,omitempty"`
}

// DockerHubServer is the server the kubelet looks Docker Hub credentials up by.
const DockerHubServer = "https://index.docker.io/v1/"

// serverRe is a credentials server: host[:port][/path], with or without a scheme.
var serverRe = regexp.MustCompile(`^(https?://)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9._/-]*)?$`)

// registryHost is a registry host as the kubelet compares it with the server of credentials: as
// written (it matches case-sensitively), with Docker Hub's two names made one. registry-1.docker.io
// is not one of them: to the kubelet it is a registry of its own.
func registryHost(host string) string {
	if host == "index.docker.io" {
		return "docker.io"
	}
	return host
}

// RegistryHost is the registry an image reference is pulled from, by Docker's rule: the first
// component is a host when it has a '.' or a ':', is localhost, or has an upper-case letter (a
// repository path is lower case, so the kubelet and containerd read "Registry/app" as host
// "Registry"); anything else is on Docker Hub. It must read a reference as they do: the check of
// the registry's name (checkRegistryHost) is made on what this returns.
func RegistryHost(image string) string {
	host, _, found := strings.Cut(image, "/")
	if !found || (!strings.ContainsAny(host, ".:") && host != "localhost" && strings.ToLower(host) == host) {
		return "docker.io"
	}
	return registryHost(host)
}

// dnsLabelRe is one label of a registry's host name.
var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// checkRegistryHost refuses an image whose registry is not named by a DNS name. The pull is made by containerd on a
// sandbox machine, outside every network policy of the pod, to whatever host the customer writes: an address
// (10.0.0.1, 127.0.0.1:5000, any numeric spelling of one), localhost, or a single-label name the machine resolves
// from its own hosts file or search domain would point that pull at the machine itself or at the platform network
// behind it. A name must have two labels or more and end in one that starts with a letter, which no spelling of an
// address does. What such a name resolves to is not checked here.
func checkRegistryHost(image string) error {
	host := strings.ToLower(RegistryHost(image))
	if h, port, ok := strings.Cut(host, ":"); ok {
		if port == "" || len(port) > 5 || strings.ContainsFunc(port, func(r rune) bool { return r < '0' || r > '9' }) {
			return fmt.Errorf("image must be a container image reference")
		}
		host = h
	}
	labels := strings.Split(host, ".")
	bad := len(host) > 253 || len(labels) < 2 || slices.ContainsFunc(labels, func(l string) bool { return !dnsLabelRe.MatchString(l) })
	if !bad {
		last := labels[len(labels)-1]
		_, err := netip.ParseAddr(host)
		bad = err == nil || last[0] < 'a' || last[0] > 'z'
	}
	if bad {
		return fmt.Errorf("image must be pulled from a registry with a DNS name (not an IP address, localhost or a single-label host)")
	}
	return nil
}

// ErrNotOffered refuses what a function cannot have here: a GPU, a build from source, a public endpoint. A request
// that names one is refused, never taken with the field ignored.
var ErrNotOffered = errors.New("not offered")

// CheckRegistryAuth checks the snapshot's registry credentials, if it has any, and fills in the
// server they are for when none is named: the registry of the image, as the kubelet knows it.
//
// Credentials that are for another registry than the one the image is pulled from are refused.
// The kubelet offers credentials only to the server they are stored for, so such a snapshot could
// never pull with them: it fails here, with words, instead of in a rollout.
func (s *Snapshot) CheckRegistryAuth() error {
	ra := s.RegistryAuth
	if ra == nil {
		return nil
	}
	if ra.Server == "" {
		if ra.Server = RegistryHost(s.Image); ra.Server == "docker.io" {
			ra.Server = DockerHubServer
		}
	}
	if len(ra.Server) > 255 || !serverRe.MatchString(ra.Server) {
		return fmt.Errorf("registryAuth.server must be the registry of the image, as host[:port]")
	}
	if ra.Username == "" || len(ra.Username) > 255 || strings.ContainsFunc(ra.Username, unicode.IsControl) {
		return fmt.Errorf("registryAuth.username is required (at most 255 characters)")
	}
	_, server, found := strings.Cut(ra.Server, "://")
	if !found {
		server = ra.Server
	}
	server, _, _ = strings.Cut(server, "/")
	if got, want := registryHost(server), RegistryHost(s.Image); got != want {
		return fmt.Errorf("the registry credentials are for %s but the image is pulled from %s: send registryAuth for that registry, or null to remove the credentials", got, want)
	}
	return nil
}

// Spec is a function as the customer configures it. Changing anything in Snapshot makes a new
// deployment; the rest applies without one.
type Spec struct {
	ProjectID string `json:"-"`
	Name      string `json:"name"`
	Snapshot
	MinInstances int `json:"minInstances"`
	IdleSeconds  int `json:"idleSeconds"`
	// MaxInstances is the most instances the function is scaled to on load (default 1: scale
	// 0↔1). Concurrency is the requests in flight one instance is meant to take (default 50): the
	// load is divided by it. It is a target, never a limit: an instance gets whatever is sent.
	MaxInstances int `json:"maxInstances"`
	Concurrency  int `json:"concurrency"`
	// Exposure is ExposurePrivate, the default and the only one offered.
	Exposure string `json:"exposure"`
	// GPU and Source are read only to be refused (ErrNotOffered): a request for a GPU function or for one built
	// from a repository must not come back as a plain function from an image.
	GPU    json.RawMessage `json:"gpu,omitempty"`
	Source json.RawMessage `json:"source,omitempty"`
}

const (
	// maxNameLen keeps <name>-<6 hex> within one 63-character DNS label, with room to spare.
	maxNameLen = 40
	maxArgs    = 64
	maxArgLen  = 4096
)

var (
	// nameRe: a function name starts with a letter. Its host label then does too, and never looks like a
	// sandbox's (ValidHostLabel).
	nameRe = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)
	// imageRe is the character set of an image reference (host[:port]/path[:tag][@digest]).
	// Whether it can be pulled is found out by the rollout.
	imageRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,511}$`)
	cpuMillis = []int{250, 500, 1000, 2000}
)

// cpuSizes are the cpuMillis values a function may have under a limit: the fractions and, above
// two vCPUs, whole vCPUs.
func cpuSizes(limit int) []int {
	var out []int
	for _, c := range cpuMillis {
		if c <= limit {
			out = append(out, c)
		}
	}
	for c := 3000; c <= limit; c += 1000 {
		out = append(out, c)
	}
	return out
}

func given(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

// Validate checks the spec against the provider's functions config and fills defaults in place
// (port 8080, the smallest size, the platform idle time, one instance at most and a concurrency of
// 50, a private exposure, the registry the credentials are for).
func (s *Spec) Validate(fc sandbox.FunctionsConfig) error {
	fc = fc.WithDefaults()
	switch {
	case given(s.GPU):
		return fmt.Errorf("gpu: GPU functions are %w", ErrNotOffered)
	case given(s.Source):
		return fmt.Errorf("source: functions built from a repository are %w: name an image", ErrNotOffered)
	case s.Exposure != "" && s.Exposure != ExposurePrivate:
		return fmt.Errorf("exposure: only %q is offered, a public endpoint is %w", ExposurePrivate, ErrNotOffered)
	}
	s.Exposure, s.GPU, s.Source = ExposurePrivate, nil, nil
	if !nameRe.MatchString(s.Name) {
		return fmt.Errorf("name must be 1-%d lowercase letters, digits or '-', starting with a letter and ending with a letter or digit", maxNameLen)
	}
	s.Image = strings.TrimSpace(s.Image)
	if !imageRe.MatchString(s.Image) {
		return fmt.Errorf("image must be a container image reference")
	}
	if err := checkRegistryHost(s.Image); err != nil {
		return err
	}
	if err := s.CheckRegistryAuth(); err != nil {
		return err
	}
	if s.Port == 0 {
		s.Port = 8080
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if s.CPUMillis == 0 {
		s.CPUMillis = cpuMillis[0]
	}
	if s.MemoryMi == 0 {
		s.MemoryMi = 256
	}
	if sizes := cpuSizes(fc.MaxCPUMillis); !slices.Contains(sizes, s.CPUMillis) {
		list := make([]string, len(sizes))
		for i, c := range sizes {
			list[i] = fmt.Sprint(c)
		}
		return fmt.Errorf("cpuMillis must be one of %s", strings.Join(list, ", "))
	}
	if s.MemoryMi < 256 || s.MemoryMi > fc.MaxMemoryMi {
		return fmt.Errorf("memoryMi must be between 256 and %d", fc.MaxMemoryMi)
	}
	if err := sandboxenv.Validate(s.Env); err != nil {
		return err
	}
	if _, set := s.Env["PORT"]; set {
		return fmt.Errorf("environment variable PORT is reserved (it is set from port)")
	}
	for _, list := range [][]string{s.Command, s.Args} {
		if len(list) > maxArgs {
			return fmt.Errorf("command and args take at most %d entries each", maxArgs)
		}
		for _, a := range list {
			if len(a) > maxArgLen || strings.IndexByte(a, 0) >= 0 {
				return fmt.Errorf("command and args entries must be at most %d bytes without NUL", maxArgLen)
			}
		}
	}
	if s.MinInstances < 0 || s.MinInstances > fc.MaxMinInstances {
		return fmt.Errorf("minInstances must be between 0 and %d", fc.MaxMinInstances)
	}
	if s.IdleSeconds == 0 {
		s.IdleSeconds = fc.IdleSeconds
	}
	if s.IdleSeconds < fc.MinIdleSeconds || s.IdleSeconds > fc.MaxIdleSeconds {
		return fmt.Errorf("idleSeconds must be between %d and %d", fc.MinIdleSeconds, fc.MaxIdleSeconds)
	}
	if s.MaxInstances == 0 {
		s.MaxInstances = 1
	}
	if s.MaxInstances < 1 || s.MaxInstances > fc.MaxInstances {
		return fmt.Errorf("maxInstances must be between 1 and %d", fc.MaxInstances)
	}
	if s.Concurrency == 0 {
		s.Concurrency = DefaultConcurrency
	}
	if s.Concurrency < 1 || s.Concurrency > MaxConcurrency {
		return fmt.Errorf("concurrency must be between 1 and %d", MaxConcurrency)
	}
	return nil
}

// ValidateSecrets checks a function's secrets (name -> value) next to its env: the rules and
// limits of env, PORT reserved, and no name in both. Errors never contain a value.
func ValidateSecrets(env, secrets map[string]string) error {
	if err := sandboxenv.Validate(secrets); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	for name := range secrets {
		if name == "PORT" {
			return fmt.Errorf("secret PORT is reserved (it is set from port)")
		}
		if _, both := env[name]; both {
			return fmt.Errorf("%s is both an environment variable and a secret", name)
		}
	}
	return nil
}

// EffectiveFloor is the number of instances a function is kept at when idle: the platform floor,
// or the function's own minInstances as far as the platform allows it. Computed from the current
// config on every wake/reap, so a config change needs no redeploy. It is never more than one
// instance: what runs above it is the load's.
func EffectiveFloor(fc sandbox.FunctionsConfig, minInstances int) int {
	return min(max(fc.MinInstances, min(minInstances, fc.MaxMinInstances)), 1)
}
