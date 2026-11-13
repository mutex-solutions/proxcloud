package sandboximport

// mirror.go — the trusted worker's copy mode for the BUILT-IN sandbox template images
// (docs/template-updates.md §4). It copies exactly one published image
//
//	ghcr.io/mutex-solutions/sandbox-<name>@sha256:<digest>
//
// to the sandbox registry as <registry>/library/sandbox-<name>:<YYYYMMDD>-<sha7>, byte for byte (the same
// manifest bytes, so the same digest), and proves the destination resolves to that digest. Inputs are fixed argv
// (mode, source, destination, policy); the source credential is the GitHub integration's PAT in a pod-owned pull
// Secret, the destination credential the narrow Zot "mirror" account (read/create on library/sandbox-* only).
// Nothing is executed or unpacked; no other registry, repository, redirect or token realm is reachable.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

const (
	// MirrorMode is the worker's first argument in copy mode (a project id is 24 hex, so it cannot collide).
	MirrorMode = "mirror"
	// MirrorSourceHost / MirrorSourceOwner are fixed: only the ProxCloud organization's packages are mirrored.
	MirrorSourceHost  = "ghcr.io"
	MirrorSourceOwner = "mutex-solutions"
	// MirrorBlobHost is where GHCR redirects blob downloads (short-lived signed URLs). Reached without credentials.
	MirrorBlobHost = "pkg-containers.githubusercontent.com"
)

var (
	mirrorNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// MirrorTagRE is the sandbox-templates workflow tag: <YYYYMMDD>-<sha7>.
	MirrorTagRE = regexp.MustCompile(`^[0-9]{8}-[0-9a-f]{7}$`)
)

// MirrorPolicy is the operator-derived, non-secret worker argument of copy mode.
type MirrorPolicy struct {
	// Registry is the sandbox registry host[:port] as the nodes pull from it.
	Registry string `json:"registry"`
}

func (p MirrorPolicy) Validate() error {
	if !validHost(p.Registry) || strings.Contains(p.Registry, "/") || p.Registry == MirrorSourceHost {
		return errors.New("mirror requires the sandbox registry host")
	}
	return nil
}

func (p MirrorPolicy) JSON() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(p)
	return string(b), err
}

// ParseMirrorPolicy strictly decodes the policy argument.
func ParseMirrorPolicy(raw string) (MirrorPolicy, error) {
	var p MirrorPolicy
	if len(raw) > 1024 {
		return p, errors.New("mirror policy exceeds its budget")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, errors.New("invalid mirror policy")
	}
	if _, err := dec.Token(); err != io.EOF {
		return p, errors.New("invalid trailing mirror policy")
	}
	return p, p.Validate()
}

// MirrorSourceRepository / MirrorDestinationRepository are the only repositories a mirror of name may touch.
func MirrorSourceRepository(name string) string {
	return MirrorSourceHost + "/" + MirrorSourceOwner + "/sandbox-" + name
}
func (p MirrorPolicy) MirrorDestinationRepository(name string) string {
	return p.Registry + "/library/sandbox-" + name
}

// Check validates a source/destination pair: source = ghcr.io/mutex-solutions/sandbox-<name>@sha256:<64hex>,
// destination = <registry>/library/sandbox-<name>:<YYYYMMDD>-<sha7> (the same name). It returns the name and the
// pinned digest.
func (p MirrorPolicy) Check(source, destination string) (name, digest string, err error) {
	if err = p.Validate(); err != nil {
		return "", "", err
	}
	repo, digest, ok := strings.Cut(source, "@")
	if !ok || !digestRE.MatchString(digest) {
		return "", "", errors.New("mirror source must be an immutable repository@sha256:<64 lowercase hex> reference")
	}
	name, ok = strings.CutPrefix(repo, MirrorSourceHost+"/"+MirrorSourceOwner+"/sandbox-")
	if !ok || !mirrorNameRE.MatchString(name) {
		return "", "", errors.New("mirror source must be ghcr.io/mutex-solutions/sandbox-<name>")
	}
	dstRepo, tag, ok := strings.Cut(destination, ":")
	if i := strings.LastIndex(destination, ":"); i > strings.LastIndex(destination, "/") {
		dstRepo, tag, ok = destination[:i], destination[i+1:], true
	} else {
		ok = false
	}
	if !ok || dstRepo != p.MirrorDestinationRepository(name) || !MirrorTagRE.MatchString(tag) {
		return "", "", errors.New("mirror destination must be <registry>/library/sandbox-<same name>:<YYYYMMDD>-<sha7>")
	}
	return name, digest, nil
}

// MirrorResult is the worker's single stdout line (no secret): it carries what the portal re-verifies.
type MirrorResult struct {
	Source            string `json:"source"`
	Destination       string `json:"destination"`
	SourceDigest      string `json:"sourceDigest"`
	DestinationDigest string `json:"destinationDigest"`
	Platform          string `json:"platform"`
	SizeBytes         int64  `json:"sizeBytes"`
	ConfigDigest      string `json:"configDigest"`
}

// ValidateMirrorResult accepts only a result for exactly this pair whose destination digest equals the source's.
func ValidateMirrorResult(r MirrorResult, source, destination string) error {
	if r.Source != source || r.Destination != destination || r.Platform != "linux/amd64" ||
		!digestRE.MatchString(r.SourceDigest) || !digestRE.MatchString(r.ConfigDigest) || r.SizeBytes < 0 || r.SizeBytes > MaxImageBytes {
		return errors.New("invalid mirror result")
	}
	if !strings.HasSuffix(source, "@"+r.SourceDigest) || r.DestinationDigest != r.SourceDigest {
		return errors.New("mirror result does not preserve the pinned digest")
	}
	return nil
}

// mirrorSourceGuard fences the credential-bearing source transport to GHCR: the token endpoint (pull scope of the
// one source repository), /v2/, the repository's manifests and blobs by digest, and GHCR's blob redirect to its
// storage host — reached without the Authorization header (net/http drops it on a cross-host redirect; this strips
// it again). Anything else, including another redirect, fails before the network.
type mirrorSourceGuard struct {
	Base http.RoundTripper
	Repo string // mutex-solutions/sandbox-<name>
}

func (g mirrorSourceGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.User != nil || r.URL.Opaque != "" || r.URL.Fragment != "" ||
		strings.Contains(r.URL.EscapedPath(), "%") || strings.Contains(r.URL.Path, "..") {
		return nil, errors.New("mirror source request left the approved HTTPS origin")
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return nil, errors.New("mirror source is read-only")
	}
	switch r.URL.Host {
	case MirrorBlobHost:
		// Only GHCR's signed blob URL (reached through its redirect), never with a credential.
		if r.Method != http.MethodGet {
			return nil, errors.New("mirror blob storage is read-only")
		}
		r = r.Clone(r.Context())
		r.Header.Del("Authorization")
		return g.check(g.Base.RoundTrip(r))
	case MirrorSourceHost:
	default:
		return nil, errors.New("mirror source request left the approved origins")
	}
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/token":
		for k, vs := range q {
			switch k {
			case "scope":
				for _, sc := range vs {
					if sc != "repository:"+g.Repo+":pull" {
						return nil, errors.New("mirror token scope outside the source repository")
					}
				}
			case "service":
				if len(vs) != 1 || vs[0] != MirrorSourceHost {
					return nil, errors.New("unexpected mirror token service")
				}
			default:
				return nil, errors.New("unexpected mirror token parameter")
			}
		}
		return g.check(g.Base.RoundTrip(r))
	case r.URL.Path == "/v2/" && len(q) == 0:
		return g.check(g.Base.RoundTrip(r))
	}
	suffix, ok := strings.CutPrefix(r.URL.Path, "/v2/"+g.Repo+"/")
	if !ok || len(q) != 0 {
		return nil, errors.New("mirror source request outside the source repository")
	}
	if ref, ok := strings.CutPrefix(suffix, "manifests/"); ok && digestRE.MatchString(ref) {
		return g.check(g.Base.RoundTrip(r))
	}
	if d, ok := strings.CutPrefix(suffix, "blobs/"); ok && digestRE.MatchString(d) {
		resp, err := g.Base.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc, lerr := url.Parse(resp.Header.Get("Location"))
			if lerr != nil || loc.Scheme != "https" || loc.Host != MirrorBlobHost || loc.User != nil {
				resp.Body.Close()
				return nil, errors.New("mirror blob redirect outside the registry storage host")
			}
			return resp, nil
		}
		return resp, nil
	}
	return nil, errors.New("mirror source request outside the manifest/blob grammar")
}

func (g mirrorSourceGuard) check(resp *http.Response, err error) (*http.Response, error) {
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, errors.New("registry redirects are not supported here")
	}
	return resp, nil
}

// MirrorSourceCredential reads the source credential of copy mode: a dockerconfigjson for ghcr.io holding ONLY a
// short-lived registry bearer token ("registrytoken": pull scope of the one source repository, minted by the portal
// from the GitHub integration's PAT). A username/password, identity token or any other host is refused: the PAT itself
// never reaches the worker.
func MirrorSourceCredential(data []byte) (authn.AuthConfig, error) {
	var d struct {
		Auths map[string]authn.AuthConfig `json:"auths"`
	}
	if len(data) > 64<<10 || json.Unmarshal(data, &d) != nil || len(d.Auths) != 1 {
		return authn.AuthConfig{}, errors.New("invalid mirror source credential configuration")
	}
	a, ok := d.Auths[MirrorSourceHost]
	if !ok || a.RegistryToken == "" || len(a.RegistryToken) > 8192 || strings.ContainsAny(a.RegistryToken, " \t\r\n") ||
		a.Username != "" || a.Password != "" || a.Auth != "" || a.IdentityToken != "" {
		return authn.AuthConfig{}, errors.New("the mirror source credential must be a ghcr.io registry token only")
	}
	return authn.AuthConfig{RegistryToken: a.RegistryToken}, nil
}

// RunMirror copies source to destination with the given credentials (source: a short-lived GHCR registry token;
// push: the Zot mirror account) and verifies the destination resolves to the pinned digest.
func RunMirror(ctx context.Context, policy MirrorPolicy, source, destination string, sourceAuth, pushAuth authn.AuthConfig) (*MirrorResult, error) {
	if sourceAuth.RegistryToken == "" || sourceAuth.Username != "" || sourceAuth.Password != "" || sourceAuth.IdentityToken != "" ||
		pushAuth.Username == "" || pushAuth.Password == "" || pushAuth.IdentityToken != "" || pushAuth.RegistryToken != "" {
		return nil, errors.New("mirror requires a source registry token and destination username/password credentials")
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.ResponseHeaderTimeout = 30 * time.Second
	return runMirror(ctx, policy, source, destination, sourceAuth, pushAuth, base)
}

func runMirror(ctx context.Context, policy MirrorPolicy, source, destination string, sourceAuth, pushAuth authn.AuthConfig, base http.RoundTripper) (*MirrorResult, error) {
	nm, digest, err := policy.Check(source, destination)
	if err != nil {
		return nil, err
	}
	srcRepo := MirrorSourceRepository(nm)
	dstRepo := policy.MirrorDestinationRepository(nm)
	srcT := mirrorSourceGuard{Base: base, Repo: MirrorSourceOwner + "/sandbox-" + nm}
	metadata := map[string]bool{}
	dstT := GuardTransport{Base: base, Repositories: map[string]bool{dstRepo: true}, MetadataDigests: metadata}
	ref, err := name.NewDigest(source, name.StrictValidation)
	if err != nil {
		return nil, err
	}
	image, err := remote.Image(ref, remote.WithContext(ctx), remote.WithTransport(srcT),
		remote.WithAuthFromKeychain(Credentials{srcRepo: sourceAuth}), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		return nil, errors.New("source manifest could not be read with the GHCR registry token (expired or not pull-scoped?)")
	}
	raw, err := image.RawManifest()
	if err != nil || len(raw) > 2<<20 {
		return nil, errors.New("invalid or oversized source manifest")
	}
	sum := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return nil, errors.New("source must pin the linux/amd64 image manifest digest, not a multi-platform index")
	}
	manifest, err := image.Manifest()
	if err != nil || manifest.Config.Size < 0 || manifest.Config.Size > 2<<20 || !digestRE.MatchString(manifest.Config.Digest.String()) || len(manifest.Config.URLs) > 0 {
		return nil, errors.New("invalid image config descriptor")
	}
	metadata[manifest.Config.Digest.String()] = true
	cfgRaw, err := image.RawConfigFile()
	if err != nil || int64(len(cfgRaw)) != manifest.Config.Size {
		return nil, errors.New("invalid image config")
	}
	ch := sha256.Sum256(cfgRaw)
	if "sha256:"+hex.EncodeToString(ch[:]) != manifest.Config.Digest.String() {
		return nil, errors.New("image config digest mismatch")
	}
	cfg, err := image.ConfigFile()
	if err != nil || cfg.OS != "linux" || cfg.Architecture != "amd64" {
		return nil, errors.New("only linux/amd64 image config is supported")
	}
	size := manifest.Config.Size
	for _, l := range manifest.Layers {
		if len(l.URLs) > 0 || l.Size < 0 || !digestRE.MatchString(l.Digest.String()) || l.Size > MaxImageBytes-size {
			return nil, errors.New("foreign layers, invalid descriptors or an image over the 30 GiB budget are not mirrored")
		}
		size += l.Size
	}
	// remote.Write pushes the source's own manifest bytes: the destination digest is the source digest. The mirror
	// account may create a tag but never move one, so an existing tag is checked first: the same digest is done
	// already (a re-run after a lost acquisition), another digest is a refusal.
	tag, err := name.NewTag(destination, name.StrictValidation)
	if err != nil {
		return nil, err
	}
	dstAuth := remote.WithAuthFromKeychain(Credentials{dstRepo: pushAuth})
	if have, herr := remote.Head(tag, remote.WithContext(ctx), remote.WithTransport(dstT), dstAuth); herr == nil {
		if have.Digest.String() != digest {
			return nil, errors.New("the destination tag already exists with another digest; tags are never moved")
		}
	} else if !isNotFound(herr) {
		return nil, errors.New("the destination tag could not be checked with the mirror account")
	} else if err = remote.Write(tag, streamingImage{image}, remote.WithContext(ctx), remote.WithTransport(dstT), dstAuth); err != nil {
		return nil, errors.New("copy into the sandbox registry failed (the mirror account may only create library/sandbox-* tags)")
	}
	// Verify: the tag now resolves to the pinned digest and every blob is readable (mirror account, read).
	desc, err := remote.Get(tag, remote.WithContext(ctx), remote.WithTransport(dstT), dstAuth)
	if err != nil {
		return nil, errors.New("destination manifest could not be read back")
	}
	dh := sha256.Sum256(desc.Manifest)
	if desc.Digest.String() != digest || "sha256:"+hex.EncodeToString(dh[:]) != digest {
		return nil, errors.New("destination digest mismatch")
	}
	for _, l := range manifest.Layers {
		lr, _ := name.NewDigest(dstRepo+"@"+l.Digest.String(), name.StrictValidation)
		layer, err := remote.Layer(lr, remote.WithContext(ctx), remote.WithTransport(dstT), dstAuth)
		if err != nil {
			return nil, errors.New("a mirrored layer is not readable")
		}
		if n, e := layer.Size(); e != nil || n != l.Size {
			return nil, errors.New("mirrored layer size verification failed")
		}
	}
	return &MirrorResult{Source: source, Destination: destination, SourceDigest: digest, DestinationDigest: desc.Digest.String(),
		Platform: "linux/amd64", SizeBytes: size, ConfigDigest: manifest.Config.Digest.String()}, nil
}

func isNotFound(err error) bool {
	var te *transport.Error
	return errors.As(err, &te) && te.StatusCode == http.StatusNotFound
}
