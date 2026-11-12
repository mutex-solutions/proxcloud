package templateupdates

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

// VMPackages are the published VM artifact packages: one per native OS plus the Kubernetes
// node-image line (docs/vm-artifact-publishing.md). The list is a source constant.
var VMPackages = []string{
	"proxcloud-vm-ubuntu-2404", "proxcloud-vm-ubuntu-2604", "proxcloud-vm-debian-12", "proxcloud-vm-debian-13",
	"proxcloud-vm-ubuntu-2604-k8s",
}

// candidateDepth is how many newest tags per line are content-checked before giving up.
const candidateDepth = 3

// Registry is what discovery needs from GitHub/GHCR (Client implements it; tests fake it).
type Registry interface {
	ListVersions(ctx context.Context, pkg string) ([]PackageVersion, error)
	GetManifest(ctx context.Context, pkg, digest string) (*Manifest, []byte, error)
	FetchBlob(ctx context.Context, pkg string, d Descriptor, limit int64) ([]byte, error)
}

// VerifiedVM is a VM artifact whose OCI manifest, manifest.json and SHA256SUMS all passed.
type VerifiedVM struct {
	Candidate
	Manifest    *VMManifest `json:"manifest"`
	Layers      *VMLayers   `json:"-"`
	DiskDigest  string      `json:"diskDigest"`
	DiskBytes   int64       `json:"diskBytes"`
	ManifestSHA string      `json:"manifestSha256"`
}

// VerifyVM resolves one candidate's content by digest and validates every contract layer. The
// disk layer digest must equal sha256:<manifest disk.sha256> and its size disk.bytes (ORAS stores
// files uncompressed), so a later blob download can be checked against both.
func VerifyVM(ctx context.Context, reg Registry, c Candidate) (*VerifiedVM, error) {
	m, _, err := reg.GetManifest(ctx, c.Package, c.Digest)
	if err != nil {
		return nil, err
	}
	layers, err := CheckVMArtifactManifest(m)
	if err != nil {
		return nil, err
	}
	raw, err := reg.FetchBlob(ctx, c.Package, layers.Manifest, maxSmallFileBytes)
	if err != nil {
		return nil, err
	}
	vm, err := ParseVMManifest(raw)
	if err != nil {
		return nil, err
	}
	if vm.ImageTag != c.Tag || vm.Package() != c.Package {
		return nil, errors.New("manifest.json does not belong to this tag/package")
	}
	sums, err := reg.FetchBlob(ctx, c.Package, layers.Sums, maxSmallFileBytes)
	if err != nil {
		return nil, err
	}
	if err := CheckSHA256SUMS(vm, raw, sums); err != nil {
		return nil, err
	}
	if layers.Disk.Digest != "sha256:"+vm.Disk.SHA256 || layers.Disk.Size != vm.Disk.Bytes {
		return nil, errors.New("disk layer does not match manifest.json disk sha256/bytes")
	}
	return &VerifiedVM{Candidate: c, Manifest: vm, Layers: layers, DiskDigest: layers.Disk.Digest, DiskBytes: layers.Disk.Size, ManifestSHA: strings.TrimPrefix(layers.Manifest.Digest, "sha256:")}, nil
}

// VMLine is the discovery row for one update line.
type VMLine struct {
	Line              string      `json:"line"`
	Package           string      `json:"package"`
	OS                string      `json:"os"`
	Variant           string      `json:"variant"`
	KubernetesVersion string      `json:"kubernetesVersion,omitempty"`
	Latest            *VerifiedVM `json:"latest,omitempty"`
	// Skipped lists newer tags that failed validation (tag + reason), newest first.
	Skipped []SkippedTag `json:"skipped,omitempty"`
	// Installed is what this site has for the line, per provider (filled by the caller).
	Installed       []InstalledVM `json:"installed"`
	UpdateAvailable bool          `json:"updateAvailable"`
}

// SkippedTag is a published tag not offered, with a short reason.
type SkippedTag struct {
	Tag    string `json:"tag"`
	Digest string `json:"digest"`
	Reason string `json:"reason"`
}

// InstalledVM is a template of the line found on a provider.
type InstalledVM struct {
	ServiceID    string `json:"serviceId"`
	ServiceName  string `json:"serviceName"`
	Node         string `json:"node"`
	VMID         int    `json:"vmid"`
	Version      string `json:"version"`                // pc-v- tag value (yyyymmddhhmm), or k8s build
	SourceDigest string `json:"sourceDigest,omitempty"` // pc-src- tag of a template this feature imported
	Published    bool   `json:"published"`
}

// DiscoverVM lists every VM package and returns one row per line with the newest valid artifact.
// A package that is missing (404) yields no rows; any other error aborts discovery (rate limits
// and auth errors must surface, not be hidden as "no update").
func DiscoverVM(ctx context.Context, reg Registry) ([]VMLine, error) {
	var out []VMLine
	for _, pkg := range VMPackages {
		versions, err := reg.ListVersions(ctx, pkg)
		var he *HTTPError
		if errors.As(err, &he) && he.Status == 404 {
			continue
		}
		if err != nil {
			return nil, err
		}
		for line, cands := range NewestPerLine(pkg, versions, candidateDepth) {
			row := VMLine{Line: line, Package: pkg, OS: cands[0].Info.OS, Variant: cands[0].Info.Variant, KubernetesVersion: cands[0].Info.KubernetesVersion, Installed: []InstalledVM{}}
			for _, c := range cands {
				v, err := VerifyVM(ctx, reg, c)
				var rl *RateLimitError
				if errors.As(err, &rl) {
					return nil, err
				}
				if err != nil {
					row.Skipped = append(row.Skipped, SkippedTag{Tag: c.Tag, Digest: c.Digest, Reason: shortReason(err)})
					continue
				}
				row.Latest = v
				break
			}
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out, nil
}

func shortReason(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// ── sandbox templates ─────────────────────────────────────────────────────────────────────────

// BuiltinSandboxNames are the built-in templates the sandbox-templates workflow publishes
// (deploy/templates/images.yaml sandbox-* entries). Discovery also covers any sandbox-<name> a
// provider's templates pin.
var BuiltinSandboxNames = []string{"amp", "base", "browser", "claude", "code-interpreter", "code-interpreter-gpu", "codex", "desktop", "gemini", "mcp-gateway", "node", "opencode"}

// SandboxTagRE is the sandbox-templates workflow tag: <YYYYMMDD>-<sha7>.
var SandboxTagRE = regexp.MustCompile(`^([0-9]{8})-([0-9a-f]{7})$`)

// SandboxPackage is the GHCR package of a built-in sandbox template image.
func SandboxPackage(name string) string { return "sandbox-" + name }

var sandboxNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// SandboxCandidate is the newest tag of a sandbox template package.
type SandboxCandidate struct {
	Name      string    `json:"name"`
	Package   string    `json:"package"`
	Tag       string    `json:"tag"`
	Digest    string    `json:"digest"`
	CreatedAt time.Time `json:"createdAt"`
	// Reference is the exact images.yaml pin value: ghcr.io/<owner>/<pkg>:<tag>@<digest>.
	Reference string `json:"reference"`
}

// NewestSandbox picks the newest <YYYYMMDD>-<sha7> tag by date, then created time.
func NewestSandbox(name string, versions []PackageVersion) []SandboxCandidate {
	var cs []SandboxCandidate
	for _, v := range versions {
		if !digestRE.MatchString(v.Name) {
			continue
		}
		for _, t := range v.Metadata.Container.Tags {
			if m := SandboxTagRE.FindStringSubmatch(t); m != nil {
				if _, err := time.Parse("20060102", m[1]); err != nil {
					continue
				}
				pkg := SandboxPackage(name)
				cs = append(cs, SandboxCandidate{Name: name, Package: pkg, Tag: t, Digest: v.Name, CreatedAt: v.CreatedAt,
					Reference: DefaultRegistry + "/" + Owner + "/" + pkg + ":" + t + "@" + v.Name})
			}
		}
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Tag[:8] != cs[j].Tag[:8] {
			return cs[i].Tag[:8] > cs[j].Tag[:8]
		}
		if !cs[i].CreatedAt.Equal(cs[j].CreatedAt) {
			return cs[i].CreatedAt.After(cs[j].CreatedAt)
		}
		return cs[i].Digest > cs[j].Digest
	})
	return cs
}

// CheckSandboxImageManifest accepts a runnable container image: an OCI/Docker image manifest
// with a container config, or an index listing a linux/amd64 image. VM artifacts (artifactType
// set / non-image config) are refused.
func CheckSandboxImageManifest(m *Manifest) error {
	if m == nil || m.SchemaVersion != 2 {
		return errors.New("not a schema 2 manifest")
	}
	if m.ArtifactType != "" {
		return errors.New("an OCI artifact is not a container image")
	}
	switch m.MediaType {
	case MediaOCIIndex, MediaDockerList:
		for _, d := range m.Manifests {
			if d.Platform != nil && d.Platform.OS == "linux" && d.Platform.Architecture == "amd64" && digestRE.MatchString(d.Digest) {
				return nil
			}
		}
		return errors.New("the index has no linux/amd64 image")
	case MediaOCIManifest, MediaDockerManifest, "":
		if m.Config == nil || (m.Config.MediaType != "application/vnd.oci.image.config.v1+json" && m.Config.MediaType != "application/vnd.docker.container.image.v1+json") {
			return errors.New("not a container image config")
		}
		if len(m.Layers) == 0 {
			return errors.New("image has no layers")
		}
		return nil
	}
	return errors.New("unsupported manifest media type")
}

// SandboxRow is one sandbox template's discovery row.
type SandboxRow struct {
	Name    string            `json:"name"`
	Latest  *SandboxCandidate `json:"latest,omitempty"`
	Skipped []SkippedTag      `json:"skipped,omitempty"`
	// Pinned is what the site's sandbox providers' templates use for this image (filled by caller).
	Pinned          []PinnedSandbox `json:"pinned"`
	UpdateAvailable bool            `json:"updateAvailable"`
}

// PinnedSandbox is a sandbox provider template whose image is this package (mirrored or not).
type PinnedSandbox struct {
	ServiceID   string `json:"serviceId"`
	ServiceName string `json:"serviceName"`
	Template    string `json:"template"`
	Image       string `json:"image"`
	Digest      string `json:"digest"`
}

// DiscoverSandbox lists sandbox-<name> for each name and returns the newest valid image.
func DiscoverSandbox(ctx context.Context, reg Registry, names []string) ([]SandboxRow, error) {
	var out []SandboxRow
	seen := map[string]bool{}
	for _, name := range names {
		if !sandboxNameRE.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		versions, err := reg.ListVersions(ctx, SandboxPackage(name))
		var he *HTTPError
		if errors.As(err, &he) && he.Status == 404 {
			out = append(out, SandboxRow{Name: name, Pinned: []PinnedSandbox{}})
			continue
		}
		if err != nil {
			return nil, err
		}
		row := SandboxRow{Name: name, Pinned: []PinnedSandbox{}}
		cands := NewestSandbox(name, versions)
		if len(cands) > candidateDepth {
			cands = cands[:candidateDepth]
		}
		for i := range cands {
			m, _, err := reg.GetManifest(ctx, cands[i].Package, cands[i].Digest)
			var rl *RateLimitError
			if errors.As(err, &rl) {
				return nil, err
			}
			if err == nil {
				err = CheckSandboxImageManifest(m)
			}
			if err != nil {
				row.Skipped = append(row.Skipped, SkippedTag{Tag: cands[i].Tag, Digest: cands[i].Digest, Reason: shortReason(err)})
				continue
			}
			row.Latest = &cands[i]
			break
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SandboxNameOfImage maps a template image ref to its built-in sandbox name: the repository's
// last path segment must be sandbox-<name> (ghcr.io/mutex-solutions/sandbox-base or the mirror
// <registry>/library/sandbox-base). Returns digest "" when the ref is not digest-pinned.
func SandboxNameOfImage(ref string) (name, digest string, ok bool) {
	repo, dig, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	last := repo[strings.LastIndex(repo, "/")+1:]
	name, ok = strings.CutPrefix(last, "sandbox-")
	if !ok || !sandboxNameRE.MatchString(name) {
		return "", "", false
	}
	if !digestRE.MatchString(dig) {
		dig = ""
	}
	return name, dig, true
}
