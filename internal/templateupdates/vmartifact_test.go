package templateupdates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// The golden files are produced by images/vm_artifact.py prepare (the reference implementation)
// with synthetic disk bytes; the Go validator must accept them unchanged.
func golden(t *testing.T, name string) (manifest, sums, disk []byte) {
	t.Helper()
	read := func(f string) []byte {
		b, err := os.ReadFile("testdata/" + name + "." + f)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return read("manifest.json"), read("SHA256SUMS"), read("disk.qcow2")
}

func TestParseVMManifestAcceptsReferenceOutput(t *testing.T) {
	for _, name := range []string{"os", "k8s"} {
		raw, sums, disk := golden(t, name)
		m, err := ParseVMManifest(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := CheckSHA256SUMS(m, raw, sums); err != nil {
			t.Fatalf("%s sums: %v", name, err)
		}
		d := sha256.Sum256(disk)
		if m.Disk.SHA256 != hex.EncodeToString(d[:]) || m.Disk.Bytes != int64(len(disk)) {
			t.Fatalf("%s disk facts: %+v", name, m.Disk)
		}
	}
	raw, _, _ := golden(t, "k8s")
	m, _ := ParseVMManifest(raw)
	if m.Package() != "proxcloud-vm-ubuntu-2604-k8s" || m.KubernetesVersion != "1.35.10" || m.ImageTag != "ubuntu-2604-k8s-1.35.10-20261112010203-cccccccccccc" {
		t.Fatalf("k8s manifest: %+v", m)
	}
}

func mutate(t *testing.T, raw []byte, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	b, _ := json.Marshal(m)
	return b
}

func TestParseVMManifestRefuses(t *testing.T) {
	raw, _, _ := golden(t, "os")
	cases := map[string]func(m map[string]any){
		"unknown key":         func(m map[string]any) { m["extra"] = 1 },
		"schema 2":            func(m map[string]any) { m["schema_version"] = 2 },
		"tag mismatch":        func(m map[string]any) { m["image_tag"] = "debian-13-os-20261111000000-bbbbbbbbbbbb" },
		"variant k8s on os":   func(m map[string]any) { m["kubernetes_version"] = "1.35.10" },
		"bad os":              func(m map[string]any) { m["os"] = "centos-9" },
		"disk too big":        func(m map[string]any) { m["disk"].(map[string]any)["bytes"] = MaxDiskBytes },
		"virtual too small":   func(m map[string]any) { m["disk"].(map[string]any)["virtual_size_bytes"] = 1 << 30 },
		"float bytes":         func(m map[string]any) { m["disk"].(map[string]any)["bytes"] = 1024.5 },
		"receipt check false": func(m map[string]any) { m["clean_export_receipt"].(map[string]any)["checks"].(map[string]any)["zero_free_space"] = false },
		"receipt hash":        func(m map[string]any) { m["clean_export_receipt"].(map[string]any)["request_sha256"] = strings.Repeat("0", 64) },
		"run ref":             func(m map[string]any) { m["clean_export_receipt"].(map[string]any)["run_ref"] = "https://github.com/evil/x/actions/runs/1" },
		"provenance host":     func(m map[string]any) { m["provenance"] = map[string]any{"upstream_url": "https://evil.example/x.qcow2"} },
		"disk filename":       func(m map[string]any) { m["disk"].(map[string]any)["filename"] = "../disk.qcow2" },
	}
	for name, fn := range cases {
		if _, err := ParseVMManifest(mutate(t, raw, fn)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dup := bytes.Replace(raw, []byte(`"arch": "amd64",`), []byte(`"arch": "amd64", "arch": "amd64",`), 1)
	if bytes.Equal(dup, raw) {
		t.Fatal("fixture shape changed")
	}
	if _, err := ParseVMManifest(dup); err == nil {
		t.Error("duplicate key accepted")
	}
}

func TestCheckSHA256SUMSRefusesTampering(t *testing.T) {
	raw, sums, _ := golden(t, "os")
	m, _ := ParseVMManifest(raw)
	if err := CheckSHA256SUMS(m, raw, append(sums, '\n')); err == nil {
		t.Fatal("extra line accepted")
	}
	if err := CheckSHA256SUMS(m, append(raw, ' '), sums); err == nil {
		t.Fatal("manifest bytes changed but sums accepted")
	}
}

func layer(title, media string, data []byte) Descriptor {
	s := sha256.Sum256(data)
	return Descriptor{MediaType: media, Digest: "sha256:" + hex.EncodeToString(s[:]), Size: int64(len(data)), Annotations: map[string]string{annotationTitle: title}}
}

func artifactManifest(raw, sums, disk []byte, v11 bool) *Manifest {
	m := &Manifest{SchemaVersion: 2, MediaType: MediaOCIManifest, Layers: []Descriptor{
		layer("disk.qcow2", MediaVMDisk, disk), layer("manifest.json", MediaVMManifest, raw), layer("SHA256SUMS", MediaSHA256SUMS, sums)}}
	if v11 {
		m.ArtifactType = VMArtifactType
		m.Config = &Descriptor{MediaType: "application/vnd.oci.empty.v1+json", Digest: "sha256:" + strings.Repeat("4", 64), Size: 2}
	} else {
		m.Config = &Descriptor{MediaType: VMArtifactType, Digest: "sha256:" + strings.Repeat("4", 64), Size: 2}
	}
	return m
}

func TestCheckVMArtifactManifest(t *testing.T) {
	raw, sums, disk := golden(t, "os")
	for _, v11 := range []bool{false, true} {
		if _, err := CheckVMArtifactManifest(artifactManifest(raw, sums, disk, v11)); err != nil {
			t.Fatalf("v1.1=%v: %v", v11, err)
		}
	}
	bad := map[string]func(m *Manifest){
		"wrong artifact type": func(m *Manifest) { m.Config.MediaType = "application/vnd.oci.image.config.v1+json" },
		"index":               func(m *Manifest) { m.MediaType = MediaOCIIndex },
		"extra layer":         func(m *Manifest) { m.Layers = append(m.Layers, layer("x", "text/plain", []byte("x"))) },
		"wrong media":         func(m *Manifest) { m.Layers[0].MediaType = "application/octet-stream" },
		"dup title":           func(m *Manifest) { m.Layers[2].Annotations[annotationTitle] = "manifest.json"; m.Layers[2].MediaType = MediaVMManifest },
		"bad digest":          func(m *Manifest) { m.Layers[1].Digest = "sha512:abc" },
		"huge disk":           func(m *Manifest) { m.Layers[0].Size = MaxDiskBytes },
	}
	for name, fn := range bad {
		m := artifactManifest(raw, sums, disk, false)
		fn(m)
		if _, err := CheckVMArtifactManifest(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func pv(digest string, created string, tags ...string) PackageVersion {
	v := PackageVersion{Name: digest}
	v.CreatedAt, _ = time.Parse(time.RFC3339, created)
	v.Metadata.Container.Tags = tags
	return v
}

func dg(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

func TestNewestPerLine(t *testing.T) {
	versions := []PackageVersion{
		pv(dg('1'), "2026-11-01T00:00:00Z", "ubuntu-2604-k8s-1.35.10-20261101000000-aaaaaaaaaaaa"),
		pv(dg('2'), "2026-11-05T00:00:00Z", "ubuntu-2604-k8s-1.35.10-20261105000000-bbbbbbbbbbbb"),
		// newer build but created earlier: build_version wins
		pv(dg('3'), "2026-10-01T00:00:00Z", "ubuntu-2604-k8s-1.35.10-20261110000000-cccccccccccc"),
		pv(dg('4'), "2026-11-11T00:00:00Z", "ubuntu-2604-k8s-1.36.1-20261111000000-dddddddddddd"),
		pv(dg('5'), "2026-11-12T00:00:00Z", "latest", "ubuntu-2604-os-20261112000000-eeeeeeeeeeee"), // wrong package line
		pv("sha512:zz", "2026-11-12T00:00:00Z", "ubuntu-2604-k8s-1.35.10-20261201000000-ffffffffffff"),   // bad digest
		pv(dg('6'), "2026-11-12T00:00:00Z", "ubuntu-2604-k8s-1.35.10-20261399000000-ffffffffffff"),     // impossible date
	}
	got := NewestPerLine("proxcloud-vm-ubuntu-2604-k8s", versions, 2)
	if len(got) != 2 {
		t.Fatalf("lines: %v", got)
	}
	l := got["ubuntu-2604/kubernetes/1.35.10"]
	if len(l) != 2 || l[0].Digest != dg('3') || l[1].Digest != dg('2') {
		t.Fatalf("1.35.10 order: %+v", l)
	}
	if got["ubuntu-2604/kubernetes/1.36.1"][0].Digest != dg('4') {
		t.Fatalf("1.36.1: %+v", got)
	}
	native := NewestPerLine("proxcloud-vm-ubuntu-2604", versions, 0)
	if len(native) != 1 || native["ubuntu-2604/os"][0].Digest != dg('5') {
		t.Fatalf("native: %+v", native)
	}
	// Same build version: created time then digest decide deterministically.
	tie := NewestPerLine("proxcloud-vm-debian-13", []PackageVersion{
		pv(dg('a'), "2026-11-01T00:00:00Z", "debian-13-os-20261101000000-aaaaaaaaaaaa"),
		pv(dg('b'), "2026-11-02T00:00:00Z", "debian-13-os-20261101000000-aaaaaaaaaaaa"),
	}, 0)
	if tie["debian-13/os"][0].Digest != dg('b') {
		t.Fatalf("tie-break: %+v", tie)
	}
}

func TestParseTagRefusesK8sOnDebian(t *testing.T) {
	if _, ok := ParseTag("debian-13-k8s-1.35.10-20261101000000-aaaaaaaaaaaa"); ok {
		t.Fatal("kubernetes on debian accepted")
	}
	if _, ok := ParseTag("ubuntu-2604-k8s-v1.35.10-20261101000000-aaaaaaaaaaaa"); ok {
		t.Fatal("v-prefixed version accepted")
	}
}

func TestNewestSandbox(t *testing.T) {
	vs := []PackageVersion{
		pv(dg('1'), "2026-11-10T00:00:00Z", "20261110-e93725b"),
		pv(dg('2'), "2026-11-11T00:00:00Z", "20261111-abcdef0", "latest"),
		pv(dg('3'), "2026-11-12T00:00:00Z", "buildcache"),
	}
	cs := NewestSandbox("base", vs)
	if len(cs) != 2 || cs[0].Digest != dg('2') || cs[0].Reference != "ghcr.io/mutex-solutions/sandbox-base:20261111-abcdef0@"+dg('2') {
		t.Fatalf("sandbox newest: %+v", cs)
	}
}

func TestSandboxNameOfImage(t *testing.T) {
	for ref, want := range map[string]string{
		"ghcr.io/mutex-solutions/sandbox-base:20261111-e93725b@" + dg('1'): "base",
		"198.51.100.226/library/sandbox-code-interpreter-gpu@" + dg('2'):  "code-interpreter-gpu",
		"10.0.0.1:5000/library/sandbox-node:x":                             "node",
	} {
		if n, _, ok := SandboxNameOfImage(ref); !ok || n != want {
			t.Errorf("%s → %q %v", ref, n, ok)
		}
	}
	if _, _, ok := SandboxNameOfImage("docker.io/library/python:3.12-slim@" + dg('1')); ok {
		t.Error("python matched")
	}
}

func TestCheckSandboxImageManifest(t *testing.T) {
	img := &Manifest{SchemaVersion: 2, MediaType: MediaOCIManifest, Config: &Descriptor{MediaType: "application/vnd.oci.image.config.v1+json"}, Layers: []Descriptor{{}}}
	if err := CheckSandboxImageManifest(img); err != nil {
		t.Fatal(err)
	}
	art := *img
	art.ArtifactType = VMArtifactType
	if CheckSandboxImageManifest(&art) == nil {
		t.Fatal("artifact accepted as an image")
	}
	idx := &Manifest{SchemaVersion: 2, MediaType: MediaOCIIndex, Manifests: []Descriptor{{Digest: dg('1'), Platform: &struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	}{"linux", "arm64"}}}}
	if CheckSandboxImageManifest(idx) == nil {
		t.Fatal("arm64-only index accepted")
	}
	idx.Manifests[0].Platform.Architecture = "amd64"
	if CheckSandboxImageManifest(idx) != nil {
		t.Fatal("amd64 index refused")
	}
}

func TestVerifyingReader(t *testing.T) {
	data := []byte("hello world")
	s := sha256.Sum256(data)
	d := "sha256:" + hex.EncodeToString(s[:])
	if b, err := io.ReadAll(NewVerifyingReader(bytes.NewReader(data), d, int64(len(data)))); err != nil || string(b) != "hello world" {
		t.Fatalf("good: %q %v", b, err)
	}
	if _, err := io.ReadAll(NewVerifyingReader(bytes.NewReader(data), d, 5)); err != ErrSizeMismatch {
		t.Fatalf("longer than bound: %v", err)
	}
	if _, err := io.ReadAll(NewVerifyingReader(bytes.NewReader(data), d, 20)); err != ErrSizeMismatch {
		t.Fatalf("shorter: %v", err)
	}
	if _, err := io.ReadAll(NewVerifyingReader(bytes.NewReader([]byte("hello WORLD")), d, int64(len(data)))); err != ErrDigestMismatch {
		t.Fatalf("tampered: %v", err)
	}
}

// fakeRegistry serves a set of packages from memory (discovery tests).
type fakeRegistry struct {
	versions  map[string][]PackageVersion
	manifests map[string]*Manifest // digest → manifest
	blobs     map[string][]byte    // digest → bytes
	listErr   error
}

func (f *fakeRegistry) ListVersions(_ context.Context, pkg string) ([]PackageVersion, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	v, ok := f.versions[pkg]
	if !ok {
		return nil, &HTTPError{Status: 404, Message: "Package not found."}
	}
	return v, nil
}

func (f *fakeRegistry) GetManifest(_ context.Context, _ string, digest string) (*Manifest, []byte, error) {
	m, ok := f.manifests[digest]
	if !ok {
		return nil, nil, &HTTPError{Status: 404, Message: "MANIFEST_UNKNOWN"}
	}
	return m, nil, nil
}

func (f *fakeRegistry) FetchBlob(_ context.Context, _ string, d Descriptor, limit int64) ([]byte, error) {
	b, ok := f.blobs[d.Digest]
	if !ok || int64(len(b)) > limit {
		return nil, fmt.Errorf("blob %s unavailable", d.Digest)
	}
	return io.ReadAll(NewVerifyingReader(bytes.NewReader(b), d.Digest, d.Size))
}

func (f *fakeRegistry) add(pkg, digest, created, tag string, m *Manifest, blobs ...[]byte) {
	if f.versions == nil {
		f.versions, f.manifests, f.blobs = map[string][]PackageVersion{}, map[string]*Manifest{}, map[string][]byte{}
	}
	f.versions[pkg] = append(f.versions[pkg], pv(digest, created, tag))
	if m != nil {
		f.manifests[digest] = m
	}
	for _, b := range blobs {
		s := sha256.Sum256(b)
		f.blobs["sha256:"+hex.EncodeToString(s[:])] = b
	}
}

func TestDiscoverVMSkipsInvalidNewest(t *testing.T) {
	raw, sums, disk := golden(t, "os")
	reg := &fakeRegistry{}
	reg.add("proxcloud-vm-debian-13", dg('1'), "2026-11-11T01:00:00Z", "debian-13-os-20261111000000-aaaaaaaaaaaa", artifactManifest(raw, sums, disk, false), raw, sums)
	// A newer tag whose OCI manifest is not a VM artifact: skipped with a reason, older offered.
	bad := artifactManifest(raw, sums, disk, false)
	bad.Config.MediaType = "application/vnd.oci.image.config.v1+json"
	reg.add("proxcloud-vm-debian-13", dg('2'), "2026-11-12T00:00:00Z", "debian-13-os-20261112000000-bbbbbbbbbbbb", bad)
	// A newer tag whose manifest.json belongs to another tag.
	reg.add("proxcloud-vm-debian-13", dg('3'), "2026-11-12T00:00:00Z", "debian-13-os-20261112100000-cccccccccccc", artifactManifest(raw, sums, disk, true))
	rows, err := DiscoverVM(context.Background(), reg)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %+v err %v", rows, err)
	}
	r := rows[0]
	if r.Latest == nil || r.Latest.Digest != dg('1') || len(r.Skipped) != 2 {
		t.Fatalf("row: latest=%+v skipped=%+v", r.Latest, r.Skipped)
	}
	if r.Latest.DiskBytes != int64(len(disk)) || !strings.HasPrefix(r.Latest.DiskDigest, "sha256:") {
		t.Fatalf("disk facts: %+v", r.Latest)
	}
}

func TestDiscoverVMSurfacesRateLimit(t *testing.T) {
	reg := &fakeRegistry{listErr: &RateLimitError{Remaining: 0}}
	if _, err := DiscoverVM(context.Background(), reg); err == nil {
		t.Fatal("rate limit hidden as an empty result")
	}
}
