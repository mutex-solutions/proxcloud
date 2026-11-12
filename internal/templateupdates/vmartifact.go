package templateupdates

// vmartifact.go — the Go side of the public VM artifact contract v1 (images/vm_artifact.py,
// docs/public-vm-artifacts.md). It must refuse exactly what the Python verifier refuses: an artifact
// is offered for update only after its OCI manifest shape AND its manifest.json/SHA256SUMS pass.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	VMArtifactType      = "application/vnd.proxcloud.vm-template.v1+json"
	MediaVMDisk         = "application/x-qemu-disk"
	MediaVMManifest     = "application/vnd.proxcloud.vm-template.manifest.v1+json"
	MediaSHA256SUMS     = "text/plain"
	MaxDiskBytes        = 8 << 30 // strictly below
	MinVirtualSizeBytes = 20 << 30
	MaxVirtualSizeBytes = 100 << 30
	maxSmallFileBytes   = 64 << 10
	annotationTitle     = "org.opencontainers.image.title"
)

// VMOSes are the OSes the contract accepts.
var VMOSes = []string{"ubuntu-2404", "ubuntu-2604", "debian-12", "debian-13"}

var (
	hex40RE       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hex64RE       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	upstreamSumRE = regexp.MustCompile(`^(?:[0-9a-f]{64}|[0-9a-f]{128})$`)
	buildRE       = regexp.MustCompile(`^[0-9]{14}$`)
	k8sVersionRE  = regexp.MustCompile(`^[1-9][0-9]{0,2}\.(?:0|[1-9][0-9]{0,2})\.(?:0|[1-9][0-9]{0,2})$`)
	identityRE    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	runRefRE      = regexp.MustCompile(`^https://github\.com/mutex-solutions/proxcloud/actions/runs/[1-9][0-9]{0,19}$`)
	ubuntuURLRE   = regexp.MustCompile(`^https://(?:cloud-images\.ubuntu\.com|releases\.ubuntu\.com)/[A-Za-z0-9._/-]+$`)
	debianURLRE   = regexp.MustCompile(`^https://cloud\.debian\.org/[A-Za-z0-9._/-]+$`)
	// TagRE is the published tag shape: <os>-os-<UTC>-<12hex> | <os>-k8s-<ver>-<UTC>-<12hex>.
	TagRE = regexp.MustCompile(`^(ubuntu-2404|ubuntu-2604|debian-12|debian-13)-(os|k8s-([1-9][0-9]{0,2}\.(?:0|[1-9][0-9]{0,2})\.(?:0|[1-9][0-9]{0,2})))-([0-9]{14})-([0-9a-f]{12})$`)
)

var vmChecks = []string{"cleaned_copy_only", "no_backing_file", "qemu_img_check", "offline_sanitization", "zero_free_space", "no_build_credentials_or_lab_network"}

// VMDisk is manifest.disk.
type VMDisk struct {
	Filename         string `json:"filename"`
	Format           string `json:"format"`
	SHA256           string `json:"sha256"`
	Bytes            int64  `json:"bytes"`
	VirtualSizeBytes int64  `json:"virtual_size_bytes"`
}

// VMManifest is the validated manifest.json (the receipt is kept opaque beyond validation).
type VMManifest struct {
	Kind              string `json:"kind"`
	OS                string `json:"os"`
	Arch              string `json:"arch"`
	Variant           string `json:"variant"`
	KubernetesVersion string `json:"kubernetes_version,omitempty"`
	SourceCommit      string `json:"source_commit"`
	UpstreamChecksum  string `json:"upstream_checksum"`
	BuildVersion      string `json:"build_version"`
	SchemaVersion     int    `json:"schema_version"`
	ImageTag          string `json:"image_tag"`
	Disk              VMDisk `json:"disk"`
}

// Package returns the GHCR package of the manifest (proxcloud-vm-<os>[-k8s]).
func (m *VMManifest) Package() string {
	if m.Variant == "kubernetes" {
		return "proxcloud-vm-" + m.OS + "-k8s"
	}
	return "proxcloud-vm-" + m.OS
}

// ImageTagFor derives the tag exactly as vm_artifact.image_tag does.
func ImageTagFor(osName, variant, k8sVersion, build, commit string) string {
	v := "os"
	if variant != "os" {
		v = "k8s-" + k8sVersion
	}
	if len(commit) < 12 {
		return ""
	}
	return fmt.Sprintf("%s-%s-%s-%s", osName, v, build, commit[:12])
}

// strictObject decodes one JSON object, refusing duplicate keys at any depth.
func strictObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeStrict(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("not an object")
	}
	return m, nil
}

func decodeStrict(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			m := map[string]any{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, errors.New("bad key")
				}
				if _, dup := m[k]; dup {
					return nil, errors.New("duplicate key")
				}
				v, err := decodeStrict(dec)
				if err != nil {
					return nil, err
				}
				m[k] = v
			}
			_, err := dec.Token()
			return m, err
		case '[':
			var out []any
			for dec.More() {
				v, err := decodeStrict(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			_, err := dec.Token()
			return out, err
		}
		return nil, errors.New("unexpected delimiter")
	default:
		return t, nil
	}
}

func keysExact(m map[string]any, required []string, optional ...string) bool {
	allowed := map[string]bool{}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			return false
		}
		allowed[k] = true
	}
	for _, k := range optional {
		allowed[k] = true
	}
	for k := range m {
		if !allowed[k] {
			return false
		}
	}
	return true
}

func strOf(v any) (string, bool) { s, ok := v.(string); return s, ok }

// intOf accepts only a JSON integer (Python's `type(x) is int`; a bool or float is refused).
func intOf(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok || strings.ContainsAny(n.String(), ".eE") {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

func match(re *regexp.Regexp, v any) bool {
	s, ok := strOf(v)
	return ok && re.MatchString(s)
}

// canonicalRequest reproduces vm_artifact.encode(q): json.dumps(sort_keys, indent=2,
// ensure_ascii) + "\n". The request values are restricted ASCII strings, so Go's
// MarshalIndent with sorted map keys produces the same bytes once HTML escaping is off.
func canonicalRequest(q map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(q); err != nil {
		return nil, err
	}
	for _, b := range buf.Bytes() {
		if b > 0x7e {
			return nil, errors.New("non-ascii request")
		}
	}
	return buf.Bytes(), nil
}

var errContract = errors.New("invalid artifact contract")

// ParseVMManifest validates manifest.json bytes exactly as vm_artifact.validate does.
func ParseVMManifest(raw []byte) (*VMManifest, error) {
	if len(raw) == 0 || len(raw) > maxSmallFileBytes {
		return nil, errContract
	}
	m, err := strictObject(raw)
	if err != nil {
		return nil, errContract
	}
	meta := []string{"kind", "os", "arch", "variant", "source_commit", "upstream_checksum", "build_version"}
	if !keysExact(m, append(append([]string{}, meta...), "schema_version", "image_tag", "disk", "clean_export_receipt"), "kubernetes_version", "provenance") {
		return nil, errContract
	}
	q := map[string]any{}
	for _, k := range append(meta, "kubernetes_version", "provenance") {
		if v, ok := m[k]; ok {
			q[k] = v
		}
	}
	if err := requestValid(q); err != nil {
		return nil, err
	}
	sv, ok := intOf(m["schema_version"])
	osName, _ := strOf(q["os"])
	variant, _ := strOf(q["variant"])
	k8s, _ := strOf(q["kubernetes_version"])
	build, _ := strOf(q["build_version"])
	commit, _ := strOf(q["source_commit"])
	tag, _ := strOf(m["image_tag"])
	if !ok || sv != 1 || tag != ImageTagFor(osName, variant, k8s, build, commit) {
		return nil, errContract
	}
	d, ok1 := m["disk"].(map[string]any)
	r, ok2 := m["clean_export_receipt"].(map[string]any)
	if !ok1 || !ok2 || !keysExact(d, []string{"filename", "format", "sha256", "bytes", "virtual_size_bytes"}) ||
		!keysExact(r, []string{"kind", "source_identity", "source_commit", "run_ref", "request_sha256", "disk_sha256", "disk_bytes", "virtual_size_bytes", "checks"}) {
		return nil, errContract
	}
	dBytes, okB := intOf(d["bytes"])
	dVirt, okV := intOf(d["virtual_size_bytes"])
	if d["filename"] != "disk.qcow2" || d["format"] != "qcow2" || !match(hex64RE, d["sha256"]) || !okB || dBytes <= 0 || dBytes >= MaxDiskBytes ||
		!okV || dVirt < MinVirtualSizeBytes || dVirt > MaxVirtualSizeBytes {
		return nil, errContract
	}
	canon, err := canonicalRequest(q)
	if err != nil {
		return nil, errContract
	}
	reqSum := sha256.Sum256(canon)
	rBytes, okRB := intOf(r["disk_bytes"])
	rVirt, okRV := intOf(r["virtual_size_bytes"])
	if r["kind"] != "CleanExportReceipt" || !match(identityRE, r["source_identity"]) || !match(runRefRE, r["run_ref"]) ||
		r["source_commit"] != q["source_commit"] || r["request_sha256"] != hex.EncodeToString(reqSum[:]) ||
		!okRB || !okRV || r["disk_sha256"] != d["sha256"] || rBytes != dBytes || rVirt != dVirt {
		return nil, errContract
	}
	checks, ok := r["checks"].(map[string]any)
	if !ok || !keysExact(checks, vmChecks) {
		return nil, errContract
	}
	for _, v := range checks {
		if b, ok := v.(bool); !ok || !b {
			return nil, errContract
		}
	}
	out := &VMManifest{Kind: "vm-template", OS: osName, Arch: "amd64", Variant: variant, KubernetesVersion: k8s, SourceCommit: commit,
		BuildVersion: build, SchemaVersion: 1, ImageTag: tag}
	out.UpstreamChecksum, _ = strOf(q["upstream_checksum"])
	out.Disk = VMDisk{Filename: "disk.qcow2", Format: "qcow2", Bytes: dBytes, VirtualSizeBytes: dVirt}
	out.Disk.SHA256, _ = strOf(d["sha256"])
	return out, nil
}

func requestValid(q map[string]any) error {
	if q["kind"] != "vm-template" || q["arch"] != "amd64" {
		return errContract
	}
	osName, _ := strOf(q["os"])
	okOS := false
	for _, o := range VMOSes {
		okOS = okOS || o == osName
	}
	variant, _ := strOf(q["variant"])
	if !okOS || (variant != "os" && variant != "kubernetes") || !match(hex40RE, q["source_commit"]) || !match(upstreamSumRE, q["upstream_checksum"]) || !match(buildRE, q["build_version"]) {
		return errContract
	}
	build, _ := strOf(q["build_version"])
	if _, err := time.Parse("20060102150405", build); err != nil {
		return errContract
	}
	if variant == "kubernetes" {
		if !strings.HasPrefix(osName, "ubuntu-") || !match(k8sVersionRE, q["kubernetes_version"]) {
			return errContract
		}
	} else if _, ok := q["kubernetes_version"]; ok {
		return errContract
	}
	if p, ok := q["provenance"]; ok {
		pm, ok := p.(map[string]any)
		if !ok || !keysExact(pm, []string{"upstream_url"}) {
			return errContract
		}
		u, _ := strOf(pm["upstream_url"])
		re := debianURLRE
		if strings.HasPrefix(osName, "ubuntu-") {
			re = ubuntuURLRE
		}
		if !re.MatchString(u) || strings.Contains(u, "..") || len(u) > 2048 {
			return errContract
		}
	}
	return nil
}

// CheckSHA256SUMS verifies the exact two-line SHA256SUMS bytes for a validated manifest.
func CheckSHA256SUMS(m *VMManifest, manifestRaw, sums []byte) error {
	ms := sha256.Sum256(manifestRaw)
	want := m.Disk.SHA256 + "  disk.qcow2\n" + hex.EncodeToString(ms[:]) + "  manifest.json\n"
	if string(sums) != want {
		return errors.New("SHA256SUMS does not match the manifest and disk hashes")
	}
	return nil
}

// VMLayers are the three named layers of a validated VM artifact OCI manifest.
type VMLayers struct{ Disk, Manifest, Sums Descriptor }

// CheckVMArtifactManifest validates the OCI manifest of a VM artifact: an image manifest (no
// index), artifact type application/vnd.proxcloud.vm-template.v1+json (ORAS image-spec v1.0 puts
// it in config.mediaType; v1.1 in artifactType), and exactly three layers with the fixed titles
// and media types.
func CheckVMArtifactManifest(m *Manifest) (*VMLayers, error) {
	if m == nil || m.SchemaVersion != 2 || (m.MediaType != "" && m.MediaType != MediaOCIManifest) || len(m.Manifests) > 0 {
		return nil, errors.New("not an OCI image manifest")
	}
	at := m.ArtifactType
	if at == "" && m.Config != nil {
		at = m.Config.MediaType
	}
	if at != VMArtifactType {
		return nil, fmt.Errorf("unexpected artifact type %q", at)
	}
	if len(m.Layers) != 3 {
		return nil, errors.New("a VM artifact has exactly three layers")
	}
	want := map[string]string{"disk.qcow2": MediaVMDisk, "manifest.json": MediaVMManifest, "SHA256SUMS": MediaSHA256SUMS}
	var out VMLayers
	seen := map[string]bool{}
	for _, l := range m.Layers {
		title := l.Annotations[annotationTitle]
		mt, ok := want[title]
		if !ok || seen[title] || l.MediaType != mt || !digestRE.MatchString(l.Digest) || l.Size <= 0 {
			return nil, fmt.Errorf("unexpected layer %q (%s)", title, l.MediaType)
		}
		seen[title] = true
		switch title {
		case "disk.qcow2":
			if l.Size >= MaxDiskBytes {
				return nil, errors.New("disk layer exceeds the contract bound")
			}
			out.Disk = l
		case "manifest.json":
			if l.Size > maxSmallFileBytes {
				return nil, errors.New("manifest.json layer too large")
			}
			out.Manifest = l
		case "SHA256SUMS":
			if l.Size > maxSmallFileBytes {
				return nil, errors.New("SHA256SUMS layer too large")
			}
			out.Sums = l
		}
	}
	return &out, nil
}

// TagInfo is a parsed published tag.
type TagInfo struct {
	OS, Variant, KubernetesVersion, BuildVersion, SourcePrefix string
}

// Line identifies an update line: os + variant (+ k8s version). Newer builds of the same line
// supersede each other; different lines never do.
func (t TagInfo) Line() string {
	if t.Variant == "kubernetes" {
		return t.OS + "/kubernetes/" + t.KubernetesVersion
	}
	return t.OS + "/os"
}

// ParseTag parses a published tag; ok=false for anything not matching the contract (e.g. a
// stray "latest", which is ignored).
func ParseTag(tag string) (TagInfo, bool) {
	m := TagRE.FindStringSubmatch(tag)
	if m == nil {
		return TagInfo{}, false
	}
	if _, err := time.Parse("20060102150405", m[4]); err != nil {
		return TagInfo{}, false
	}
	t := TagInfo{OS: m[1], Variant: "os", BuildVersion: m[4], SourcePrefix: m[5]}
	if m[2] != "os" {
		if !strings.HasPrefix(m[1], "ubuntu-") {
			return TagInfo{}, false
		}
		t.Variant, t.KubernetesVersion = "kubernetes", m[3]
	}
	return t, true
}

// Candidate is one package version whose tag matches the contract, before content validation.
type Candidate struct {
	Package   string    `json:"package"`
	Tag       string    `json:"tag"`
	Digest    string    `json:"digest"`
	CreatedAt time.Time `json:"createdAt"`
	Info      TagInfo   `json:"-"`
}

// NewestPerLine picks, per update line, the newest candidates ordered by build_version (from the
// tag, then package created time, then digest for a deterministic tie-break). It returns up to
// depth candidates per line, newest first, so a caller can fall back to the next one when the
// newest fails content validation. Versions whose tag does not belong to pkg's line are ignored
// (an os tag in a -k8s package, an OS mismatch).
func NewestPerLine(pkg string, versions []PackageVersion, depth int) map[string][]Candidate {
	byLine := map[string][]Candidate{}
	for _, v := range versions {
		if !digestRE.MatchString(v.Name) {
			continue
		}
		for _, tag := range v.Metadata.Container.Tags {
			info, ok := ParseTag(tag)
			if !ok {
				continue
			}
			wantPkg := "proxcloud-vm-" + info.OS
			if info.Variant == "kubernetes" {
				wantPkg += "-k8s"
			}
			if wantPkg != pkg {
				continue
			}
			byLine[info.Line()] = append(byLine[info.Line()], Candidate{Package: pkg, Tag: tag, Digest: v.Name, CreatedAt: v.CreatedAt, Info: info})
		}
	}
	for line, cs := range byLine {
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].Info.BuildVersion != cs[j].Info.BuildVersion {
				return cs[i].Info.BuildVersion > cs[j].Info.BuildVersion
			}
			if !cs[i].CreatedAt.Equal(cs[j].CreatedAt) {
				return cs[i].CreatedAt.After(cs[j].CreatedAt)
			}
			return cs[i].Digest > cs[j].Digest
		})
		if depth > 0 && len(cs) > depth {
			cs = cs[:depth]
		}
		byLine[line] = cs
	}
	return byLine
}
