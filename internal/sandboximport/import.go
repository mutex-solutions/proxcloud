// Package sandboximport implements the trusted, no-build registry digest importer.
// It never executes an image, expands a layer, or accepts an arbitrary registry endpoint.
package sandboximport

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"io"
)

const MaxImageBytes int64 = 30 << 30

var digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var repoRE = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
var idRE = regexp.MustCompile(`^[a-f0-9]{24}$`)
var userRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,31}$`)

// Result contains no secrets or image ENV values. ENV stays byte-for-byte in the image config.
type Result struct {
	Source            string `json:"source"`
	Image             string `json:"image"`
	SourceDigest      string `json:"sourceDigest"`
	DestinationDigest string `json:"destinationDigest"`
	Platform          string `json:"platform"`
	SizeBytes         int64  `json:"sizeBytes"`
	User              string `json:"user"`
	Workdir           string `json:"workdir"`
	ConfigDigest      string `json:"configDigest"`
}

func ValidateResult(r Result, source, destination string) error {
	if r.Source != source || r.Platform != "linux/amd64" || !digestRE.MatchString(r.SourceDigest) || !digestRE.MatchString(r.DestinationDigest) || !digestRE.MatchString(r.ConfigDigest) || r.SizeBytes < 0 || r.SizeBytes > MaxImageBytes {
		return errors.New("invalid import verification result")
	}
	if !strings.HasSuffix(source, "@"+r.SourceDigest) || r.Image != DestinationRepository(destination)+"@"+r.DestinationDigest {
		return errors.New("import result does not match the pinned source and destination")
	}
	return ValidateRuntime(r.User, r.Workdir)
}

func ValidateRuntime(user, workdir string) error {
	if user != "" && !userRE.MatchString(user) {
		return errors.New("import requires a named image USER (for example root or app); numeric UID and USER:GROUP are not supported by this sandbox runtime")
	}
	if len(workdir) > 4096 || strings.ContainsAny(workdir, "\x00\r\n") || (workdir != "" && !strings.HasPrefix(workdir, "/")) {
		return errors.New("workdir must be an absolute path of at most 4096 bytes without control characters")
	}
	return nil
}

// Credentials is intentionally repository-scoped, not host-scoped. Source and destination
// may be on the SAME registry host but must never accidentally use each other's identity.
type Credentials map[string]authn.AuthConfig

func (c Credentials) Resolve(r authn.Resource) (authn.Authenticator, error) {
	if a, ok := c[r.String()]; ok {
		return authn.FromConfig(a), nil
	}
	return authn.Anonymous, nil
}

// SourceCredential accepts only an explicit empty auths document as anonymous.
// Missing/malformed files and partially supplied credentials fail closed; callers
// must never retry source access with platform push/pull credentials.
func SourceCredential(data []byte, host string) (authn.AuthConfig, error) {
	var d struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if len(data) > 64<<10 || json.Unmarshal(data, &d) != nil || d.Auths == nil {
		return authn.AuthConfig{}, errors.New("invalid source registry credential configuration")
	}
	if len(d.Auths) == 0 {
		return authn.AuthConfig{}, nil
	}
	return DockerCredential(data, host)
}

func DockerCredential(data []byte, host string) (authn.AuthConfig, error) {
	var d struct {
		Auths map[string]authn.AuthConfig `json:"auths"`
	}
	if len(data) > 64<<10 || json.Unmarshal(data, &d) != nil {
		return authn.AuthConfig{}, errors.New("invalid registry credential configuration")
	}
	a, ok := d.Auths[host]
	if !ok {
		return a, errors.New("registry credentials do not contain the required registry host")
	}
	if a.Auth != "" && a.Username == "" {
		b, e := base64.StdEncoding.DecodeString(a.Auth)
		if e != nil {
			return a, errors.New("invalid registry auth")
		}
		a.Username, a.Password, _ = strings.Cut(string(b), ":")
	}
	if a.Username == "" || a.Password == "" || a.IdentityToken != "" || a.RegistryToken != "" {
		return a, errors.New("username/password registry credentials are required")
	}
	return a, nil
}

// GuardTransport fences token realms, redirects, foreign layer URLs and upload Locations.
// The sole permitted endpoints are the approved registry origin and exact source/dest repos.
// Private registry IPs are intentional; HTTPS verifies the operator-owned DNS names. Proxies
// are disabled so untrusted proxy environment cannot redirect the credential-bearing request.
type GuardTransport struct {
	Base            http.RoundTripper
	Repositories    map[string]bool
	MetadataDigests map[string]bool
}
type boundedBody struct {
	io.Reader
	io.Closer
}

// Hide MountableLayer: a platform push identity must never request source-repo
// pull scope even when both repositories share one registry host.
type streamingImage struct{ v1.Image }
type streamingLayer struct{ v1.Layer }

func (i streamingImage) Layers() ([]v1.Layer, error) {
	ls, e := i.Image.Layers()
	for j, l := range ls {
		ls[j] = streamingLayer{l}
	}
	return ls, e
}
func (i streamingImage) LayerByDigest(h v1.Hash) (v1.Layer, error) {
	l, e := i.Image.LayerByDigest(h)
	if e != nil {
		return nil, e
	}
	return streamingLayer{l}, nil
}
func (i streamingImage) LayerByDiffID(h v1.Hash) (v1.Layer, error) {
	l, e := i.Image.LayerByDiffID(h)
	if e != nil {
		return nil, e
	}
	return streamingLayer{l}, nil
}

func (t GuardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	host := r.URL.Host
	if r.URL.Scheme != "https" || r.URL.User != nil || !validHost(host) {
		return nil, errors.New("registry request left the approved HTTPS registry origins")
	}
	if r.URL.Opaque != "" || r.URL.Fragment != "" || strings.Contains(r.URL.EscapedPath(), "%") || strings.Contains(r.URL.Path, "..") || r.URL.Query().Get("mount") != "" || r.URL.Query().Get("from") != "" {
		return nil, errors.New("noncanonical registry path")
	}
	approvedOrigin := false
	for repo := range t.Repositories {
		h, _, _ := strings.Cut(repo, "/")
		if h == host {
			approvedOrigin = true
		}
	}
	if !approvedOrigin {
		return nil, errors.New("registry credential cannot cross registry origins")
	}
	allowed := r.URL.Path == "/v2/"
	for repo := range t.Repositories {
		h, p, _ := strings.Cut(repo, "/")
		if h == host && strings.HasPrefix(r.URL.Path, "/v2/"+p+"/") {
			allowed = true
		}
	}
	if r.URL.Path == "/service/token" {
		allowed = true
		for _, sc := range r.URL.Query()["scope"] {
			parts := strings.Split(sc, ":")
			if len(parts) != 3 || parts[0] != "repository" || !t.Repositories[host+"/"+parts[1]] {
				allowed = false
			}
		}
	}
	if !allowed {
		return nil, errors.New("registry request left the authorized repositories")
	}
	resp, err := t.Base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	// Fail closed on redirects rather than letting an authenticated round trip follow
	// a malicious token realm/blob/upload Location (even to a second approved alias).
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, errors.New("registry redirects are not supported")
	}
	last := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if strings.Contains(r.URL.Path, "/manifests/") || r.URL.Path == "/service/token" || t.MetadataDigests[last] {
		if resp.ContentLength > 2<<20 {
			resp.Body.Close()
			return nil, errors.New("registry metadata exceeds the import budget")
		}
		resp.Body = boundedBody{Reader: io.LimitReader(resp.Body, (2<<20)+1), Closer: resp.Body}
	}
	return resp, nil
}

// Run verifies config/manifest before a remote-to-remote crane streaming push. Canonical registration
// does not copy: it verifies that the runtime's pull robot can read the pinned manifest,
// config and every blob. Migration credentials are used ONLY for their exact source repo.
func Run(ctx context.Context, policy Policy, project, source, destination string, sourceAuth, pushAuth, pullAuth authn.AuthConfig) (*Result, error) {
	canonical, err := policy.Source(source, project)
	if err != nil {
		return nil, err
	}
	srcRepo, srcDigest, _ := strings.Cut(source, "@")
	dstRepo := DestinationRepository(destination)
	if err := policy.Destination(source, project, destination); err != nil {
		return nil, err
	}
	if !canonical {
		if sourceAuth == (authn.AuthConfig{}) && !policy.AllowAnonymous {
			return nil, errors.New("anonymous source access is not enabled")
		}
		if sourceAuth != (authn.AuthConfig{}) && (sourceAuth.Username == "" || sourceAuth.Password == "" || sourceAuth.IdentityToken != "" || sourceAuth.RegistryToken != "") {
			return nil, errors.New("source requires an explicit username/password pair")
		}
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.ResponseHeaderTimeout = 30 * time.Second
	return run(ctx, source, srcRepo, srcDigest, dstRepo, destination, canonical, sourceAuth, pushAuth, pullAuth, base)
}

func run(ctx context.Context, source, srcRepo, srcDigest, dstRepo, destination string, canonical bool, sourceAuth, pushAuth, pullAuth authn.AuthConfig, base http.RoundTripper) (*Result, error) {
	metadata := map[string]bool{}
	sourceTransport := GuardTransport{Base: base, Repositories: map[string]bool{srcRepo: true}, MetadataDigests: metadata}
	transport := GuardTransport{Base: base, Repositories: map[string]bool{dstRepo: true}, MetadataDigests: metadata}
	auth := Credentials{srcRepo: sourceAuth}
	if canonical {
		auth[srcRepo] = pullAuth
	}
	opts := []remote.Option{remote.WithContext(ctx), remote.WithTransport(sourceTransport), remote.WithAuthFromKeychain(auth), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"})}
	ref, err := name.NewDigest(source, name.StrictValidation)
	if err != nil {
		return nil, err
	}
	image, err := remote.Image(ref, opts...)
	if err != nil {
		return nil, errors.New("source manifest could not be read with the authorized registry identity")
	}
	raw, err := image.RawManifest()
	if err != nil || len(raw) > 2<<20 {
		return nil, errors.New("invalid or oversized source manifest")
	}
	sum := sha256.Sum256(raw)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	if actual != srcDigest {
		return nil, errors.New("source must pin the linux/amd64 image manifest digest, not a multi-platform index")
	}
	manifest, err := image.Manifest()
	if err != nil || manifest.Config.Size < 0 || manifest.Config.Size > 2<<20 || !digestRE.MatchString(manifest.Config.Digest.String()) || len(manifest.Config.URLs) > 0 {
		return nil, errors.New("invalid image config descriptor")
	}
	transport.MetadataDigests[manifest.Config.Digest.String()] = true
	cfgRaw, err := image.RawConfigFile()
	if err != nil || len(cfgRaw) > 2<<20 {
		return nil, errors.New("invalid or oversized image config")
	}
	cfg, err := image.ConfigFile()
	if err != nil || cfg.OS != "linux" || cfg.Architecture != "amd64" {
		return nil, errors.New("only linux/amd64 image config is supported")
	}
	if err = ValidateRuntime(cfg.Config.User, cfg.Config.WorkingDir); err != nil {
		return nil, err
	}
	// PROXCLOUD_* in an imported image is untrusted control-plane configuration, not ENV.
	for _, e := range cfg.Config.Env {
		if strings.HasPrefix(e, "PROXCLOUD_") || strings.HasPrefix(e, "KUBERNETES_") {
			return nil, errors.New("image ENV must not set reserved PROXCLOUD_ runtime controls")
		}
	}
	if int64(len(cfgRaw)) != manifest.Config.Size {
		return nil, errors.New("image config size mismatch")
	}
	configHash := sha256.Sum256(cfgRaw)
	if "sha256:"+hex.EncodeToString(configHash[:]) != manifest.Config.Digest.String() {
		return nil, errors.New("image config digest mismatch")
	}
	size := manifest.Config.Size
	for _, layer := range manifest.Layers {
		if len(layer.URLs) > 0 || layer.Size < 0 || !digestRE.MatchString(layer.Digest.String()) {
			return nil, errors.New("foreign layers and invalid descriptors are not supported")
		}
		if layer.Size > MaxImageBytes-size {
			return nil, errors.New("compressed image exceeds the 30 GiB import budget")
		}
		size += layer.Size
		if size > MaxImageBytes {
			return nil, errors.New("compressed image exceeds the 30 GiB import budget")
		}
	}
	if size < 0 || size > MaxImageBytes {
		return nil, errors.New("invalid image size")
	}
	destDigest := srcDigest
	if !canonical {
		co := []crane.Option{crane.WithContext(ctx), crane.WithTransport(transport), crane.WithAuthFromKeychain(Credentials{dstRepo: pushAuth}), crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: "amd64"})}
		if err = crane.Push(streamingImage{image}, destination, co...); err != nil {
			return nil, errors.New("registry-to-registry image copy failed")
		}
	}
	// Verify the durable destination with the runtime pull identity, not the push/source credential.
	pull := Credentials{dstRepo: pullAuth}
	dst, err := name.NewDigest(dstRepo+"@"+destDigest, name.StrictValidation)
	if err != nil {
		return nil, err
	}
	verified, err := remote.Image(dst, remote.WithContext(ctx), remote.WithTransport(transport), remote.WithAuthFromKeychain(pull))
	if err != nil {
		return nil, errors.New("runtime pull credential cannot read the imported image")
	}
	vr, err := verified.RawManifest()
	if err != nil {
		return nil, errors.New("destination manifest verification failed")
	}
	sh := sha256.Sum256(vr)
	if "sha256:"+hex.EncodeToString(sh[:]) != destDigest {
		return nil, errors.New("destination digest mismatch")
	}
	if _, err = verified.RawConfigFile(); err != nil {
		return nil, errors.New("runtime pull credential cannot read image config")
	}
	// HEAD every blob: no layer unpack/repack, and no fake readiness after a manifest-only push.
	for _, layer := range manifest.Layers {
		lr, _ := name.NewDigest(dstRepo+"@"+layer.Digest.String(), name.StrictValidation)
		l, err := remote.Layer(lr, remote.WithContext(ctx), remote.WithTransport(transport), remote.WithAuthFromKeychain(pull))
		if err != nil {
			return nil, errors.New("runtime cannot read an imported layer")
		}
		if n, e := l.Size(); e != nil || n != layer.Size {
			return nil, errors.New("imported layer size verification failed")
		}
	}
	return &Result{Source: source, Image: dst.String(), SourceDigest: srcDigest, DestinationDigest: destDigest, Platform: "linux/amd64", SizeBytes: size, User: cfg.Config.User, Workdir: cfg.Config.WorkingDir, ConfigDigest: manifest.Config.Digest.String()}, nil
}
