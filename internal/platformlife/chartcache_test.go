package platformlife

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// The published charts these tests render (the cluster chart and cert-manager at the versions a BOM pins) come from
// the registry once per digest per machine, not once per call: a `go test ./...` pulled the cluster chart eleven times
// and cert-manager twice, in CI and on every workstation, and each pull is a download the package page counts.
//
// The cache is one directory of files named by the sha256 of their content: the OCI manifest a BOM pins by digest,
// and the chart archive that manifest names. Nothing in it is trusted: every use hashes the manifest against the
// BOM's digest and the archive against the manifest's layer digest, a file that is not what its name says is pulled
// again, and pulled content that is not what the BOM pins fails the test and is not kept.

// chartCacheEnv names the cache directory (CI restores and saves it with actions/cache); unset, the cache is
// proxcloud-test-charts under os.UserCacheDir().
const chartCacheEnv = "PROXCLOUD_TEST_CHART_CACHE"

// helmChartLayer is the media type of the chart archive in the OCI manifest `helm push` writes.
const helmChartLayer = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

// errChartUnreachable: the content is not in the cache (or the cached file is damaged), and its source did not
// answer. The one failure a workstation without a network may skip a test for.
var errChartUnreachable = errors.New("not in the chart cache, and its source could not be read")

// errNotInRegistry: the registry was reached and answered that it has no content of that digest. The pin names
// something that is not published, which fails the test everywhere: it is not errChartUnreachable.
var errNotInRegistry = errors.New("the registry has no content of that digest")

func chartCacheDir() string {
	if dir := os.Getenv(chartCacheEnv); dir != "" {
		return dir
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "proxcloud-test-charts")
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// cachedBlob is the file in dir whose content has the sha256 digest, and that content: the cached file when it still
// hashes to digest, else what fetch returns, which must hash to it. A new file is written under a temporary name and
// renamed, so test processes that run side by side see a whole file or none, and a damaged file is replaced in place.
func cachedBlob(dir, digest string, fetch func() ([]byte, error)) (string, []byte, error) {
	if !sha256Digest.MatchString(digest) { // it becomes a file name
		return "", nil, fmt.Errorf("%q is not a sha256 digest", digest)
	}
	sum := strings.TrimPrefix(digest, "sha256:")
	path := filepath.Join(dir, sum)
	if raw, err := os.ReadFile(path); err == nil && digestOf(raw) == digest {
		return path, raw, nil
	}
	raw, err := fetch()
	if errors.Is(err, errNotInRegistry) {
		return "", nil, fmt.Errorf("%s: %w", digest, err)
	}
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w: %v", digest, errChartUnreachable, err)
	}
	if got := digestOf(raw); got != digest {
		return "", nil, fmt.Errorf("the source answered %s for content pinned as %s", got, digest)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	tmp, err := os.CreateTemp(dir, sum+".*.tmp")
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(tmp.Name()) // nothing to remove once renamed
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return "", nil, err
	}
	if err := tmp.Close(); err != nil {
		return "", nil, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", nil, err
	}
	return path, raw, nil
}

// registryGet reads one manifest or blob of repo by digest, anonymously (the product's charts are public). It does
// not check what it reads: cachedBlob does, the same way for a pull and for a cached file.
func registryGet(repo name.Repository, kind, digest, accept string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rt, err := transport.NewWithContext(ctx, repo.Registry, authn.Anonymous, http.DefaultTransport, []string{repo.Scope(transport.PullScope)})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s://%s/v2/%s/%s/%s", repo.Scheme(), repo.RegistryStr(), repo.RepositoryStr(), kind, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("GET %s: %w", url, errNotInRegistry)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// ociChart is the archive of the chart c pins, as `helm template` reads it: the manifest of c.Digest, then the chart
// layer that manifest names, each from dir or, once, from the registry. pulled is told of every read of the registry.
func ociChart(dir string, c Chart, pulled func(format string, args ...any)) (string, error) {
	repo, err := name.NewRepository(c.Repo + "/" + c.Name)
	if err != nil {
		return "", err
	}
	_, raw, err := cachedBlob(dir, c.Digest, func() ([]byte, error) {
		pulled("chart cache: pulling the manifest of %s %s (%s) from %s", c.Name, c.Version, c.Digest, c.Repo)
		return registryGet(repo, "manifests", c.Digest, "application/vnd.oci.image.manifest.v1+json")
	})
	if err != nil {
		return "", err
	}
	var manifest struct {
		Layers []struct{ MediaType, Digest string }
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return "", fmt.Errorf("manifest %s of %s: %v", c.Digest, c.Name, err)
	}
	for _, l := range manifest.Layers {
		if l.MediaType != helmChartLayer {
			continue
		}
		path, _, err := cachedBlob(dir, l.Digest, func() ([]byte, error) {
			pulled("chart cache: pulling the archive of %s %s (%s) from %s", c.Name, c.Version, l.Digest, c.Repo)
			return registryGet(repo, "blobs", l.Digest, "")
		})
		return path, err
	}
	return "", fmt.Errorf("manifest %s of %s names no Helm chart archive", c.Digest, c.Name)
}

// chartOrSkip ends the test for a chart that could not be had, as the pulls before the cache did: a registry that
// cannot be reached fails in CI and skips elsewhere; anything else (content that is not the pinned one) always fails.
func chartOrSkip(t *testing.T, what, path string, err error) string {
	t.Helper()
	if errors.Is(err, errChartUnreachable) && os.Getenv("CI") == "" {
		t.Skipf("chart %s is not in the test chart cache and could not be pulled: %v", what, err)
	}
	if err != nil {
		t.Fatalf("chart %s: %v", what, err)
	}
	return path
}

// pinnedChart is the archive of a chart as a BOM pins it, by the BOM's digest.
func pinnedChart(t *testing.T, c Chart) string {
	t.Helper()
	path, err := ociChart(chartCacheDir(), c, t.Logf)
	return chartOrSkip(t, c.Name+" "+c.Version, path, err)
}

// fakeChartRegistry is a registry with one chart: a manifest that names one archive. It counts what it serves, and
// serves lie in place of the content of the digest named there.
type fakeChartRegistry struct {
	*httptest.Server
	chart     Chart
	archive   []byte
	manifests atomic.Int32
	blobs     atomic.Int32
	lieAbout  string
}

func newFakeChartRegistry(t *testing.T) *fakeChartRegistry {
	t.Helper()
	r := &fakeChartRegistry{archive: []byte("the chart archive")}
	manifest := []byte(`{"schemaVersion":2,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":"sha256:` + strings.Repeat("0", 64) + `"},` +
		`"layers":[{"mediaType":"application/vnd.cncf.helm.chart.provenance.v1.prov","digest":"sha256:` + strings.Repeat("1", 64) + `"},` +
		`{"mediaType":"` + helmChartLayer + `","digest":"` + digestOf(r.archive) + `"}]}`)
	content := map[string][]byte{"/v2/charts/demo/manifests/" + digestOf(manifest): manifest, "/v2/charts/demo/blobs/" + digestOf(r.archive): r.archive}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, ok := content[req.URL.Path]
		switch {
		case req.URL.Path == "/v2/":
			return
		case !ok:
			http.NotFound(w, req)
			return
		case strings.Contains(req.URL.Path, "/manifests/"):
			r.manifests.Add(1)
		default:
			r.blobs.Add(1)
		}
		if r.lieAbout != "" && strings.HasSuffix(req.URL.Path, r.lieAbout) {
			raw = []byte("something else")
		}
		_, _ = w.Write(raw)
	}))
	t.Cleanup(r.Close)
	r.chart = Chart{Repo: strings.TrimPrefix(r.URL, "http://") + "/charts", Name: "demo", Version: "1.2.3", Digest: digestOf(manifest)}
	return r
}

func (r *fakeChartRegistry) pulls() [2]int32 { return [2]int32{r.manifests.Load(), r.blobs.Load()} }

// A chart is read from the registry once per digest: every later use, in this process or another, is the cached
// file, with the registry gone as well. Without a cached copy an unreachable registry is errChartUnreachable, the
// failure a workstation skips the test for.
func TestChartIsPulledOncePerDigest(t *testing.T) {
	r, dir := newFakeChartRegistry(t), t.TempDir()
	var path string
	for range 3 {
		got, err := ociChart(dir, r.chart, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		if raw, err := os.ReadFile(got); err != nil || string(raw) != string(r.archive) {
			t.Fatalf("the archive is %q, %v", raw, err)
		}
		path = got
	}
	if got := r.pulls(); got != [2]int32{1, 1} {
		t.Fatalf("three uses made %d manifest and %d archive reads of the registry, want one of each", got[0], got[1])
	}
	r.Close()
	if got, err := ociChart(dir, r.chart, t.Logf); err != nil || got != path {
		t.Fatalf("with the registry gone the cached chart is %q, %v", got, err)
	}
	if _, err := ociChart(t.TempDir(), r.chart, t.Logf); !errors.Is(err, errChartUnreachable) {
		t.Fatalf("no cached copy and no registry: %v, want errChartUnreachable", err)
	}
}

// Every use hashes what it reads from the cache: a manifest or an archive that is no longer the content its name
// says is pulled again, and without a registry it is an error, never the damaged file.
func TestChartCacheIsVerifiedOnEveryUse(t *testing.T) {
	r, dir := newFakeChartRegistry(t), t.TempDir()
	path, err := ociChart(dir, r.chart, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, strings.TrimPrefix(r.chart.Digest, "sha256:"))
	for i, damaged := range []string{path, manifest} {
		if err := os.WriteFile(damaged, []byte("damaged"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ociChart(dir, r.chart, t.Logf)
		if err != nil || got != path {
			t.Fatalf("after damage to %s: %q, %v", filepath.Base(damaged), got, err)
		}
		if raw, _ := os.ReadFile(damaged); digestOf(raw) != "sha256:"+filepath.Base(damaged) {
			t.Fatalf("%s was not replaced by the pinned content", filepath.Base(damaged))
		}
		if got, want := r.pulls(), [][2]int32{{1, 2}, {2, 2}}[i]; got != want {
			t.Fatalf("after damage to %s the registry served %v, want %v: only the damaged file is pulled again", filepath.Base(damaged), got, want)
		}
	}
	r.Close()
	if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ociChart(dir, r.chart, t.Logf); !errors.Is(err, errChartUnreachable) {
		t.Fatalf("a damaged archive and no registry: %q, %v, want errChartUnreachable", got, err)
	}
}

// What the registry answers must be what the BOM pins: other content under the pinned digest fails (it is not the
// "could not be reached" a workstation skips for) and is not kept, for the manifest and for the archive it names.
func TestPulledChartMustBeThePinnedOne(t *testing.T) {
	for _, lie := range []string{"manifest", "archive"} {
		t.Run(lie, func(t *testing.T) {
			r, dir := newFakeChartRegistry(t), t.TempDir()
			r.lieAbout = map[string]string{"manifest": r.chart.Digest, "archive": digestOf(r.archive)}[lie]
			path, err := ociChart(dir, r.chart, t.Logf)
			if err == nil || errors.Is(err, errChartUnreachable) || !strings.Contains(err.Error(), "pinned as "+r.lieAbout) {
				t.Fatalf("a registry that answers other content for the %s: %q, %v", lie, path, err)
			}
			if _, err := os.Stat(filepath.Join(dir, strings.TrimPrefix(r.lieAbout, "sha256:"))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the content that is not the pinned one was kept: %v", err)
			}
		})
	}
	// A pin the registry has no content for is an answer, not a registry that could not be reached: a workstation
	// fails on it too (a BOM whose digest is wrong would else skip the release checks there).
	r, dir := newFakeChartRegistry(t), t.TempDir()
	unpublished := r.chart
	unpublished.Digest = digestOf([]byte("never published"))
	if path, err := ociChart(dir, unpublished, t.Logf); !errors.Is(err, errNotInRegistry) || errors.Is(err, errChartUnreachable) {
		t.Fatalf("a pin the registry has no content for: %q, %v, want errNotInRegistry", path, err)
	}
	// A digest names a file: one that is not a sha256 digest reads no registry and writes nothing.
	if _, _, err := cachedBlob(dir, "sha256:../../etc/passwd", func() ([]byte, error) { t.Error("fetched"); return nil, nil }); err == nil {
		t.Fatal("a digest that is not one was accepted")
	}
}

// Test processes that start with an empty cache at the same time each end with the whole file, and leave nothing
// but it behind.
func TestChartCacheUnderParallelWriters(t *testing.T) {
	dir, raw := t.TempDir(), []byte(strings.Repeat("chart ", 1<<16))
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			path, got, err := cachedBlob(dir, digestOf(raw), func() ([]byte, error) { return raw, nil })
			if err != nil || string(got) != string(raw) {
				t.Errorf("cachedBlob: %v", err)
				return
			}
			if onDisk, err := os.ReadFile(path); err != nil || digestOf(onDisk) != digestOf(raw) {
				t.Errorf("a reader saw a file that is not the whole content: %v", err)
			}
		})
	}
	wg.Wait()
	if files, err := os.ReadDir(dir); err != nil || len(files) != 1 {
		t.Fatalf("the cache holds %d files (%v), want the one blob and no temporary file", len(files), err)
	}
}

// The helpers the release checks take their charts from (TestUpgradeRollsNoMachine, TestChartInTreeRollsNoMachine,
// TestCertManagerMoveRenders) read the registry once for a chart, however often they are asked for it: they ran one
// `helm pull` per call.
func TestChartHelpersPullOnce(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	r := newFakeChartRegistry(t)
	t.Setenv(chartCacheEnv, t.TempDir())
	boms := []*BOM{{Components: []Component{{Name: "cert-manager", Chart: r.chart}}}}
	for range 3 {
		if a, b := clusterChart(t, helm, r.chart), certManagerChart(t, helm, boms, r.chart.Version); a != b || filepath.Dir(a) != chartCacheDir() {
			t.Fatalf("the cluster chart is %q and cert-manager %q, want the one file of the cache in %s", a, b, chartCacheDir())
		}
	}
	if got := r.pulls(); got != [2]int32{1, 1} {
		t.Fatalf("six uses made %d manifest and %d archive reads of the registry, want one of each", got[0], got[1])
	}
}

// A chart that could not be pulled skips the test on a workstation, as before the cache; CI fails instead, and so
// does everything else everywhere (TestPulledChartMustBeThePinnedOne shows which errors those are).
func TestUnreachableChartSkipsOutsideCI(t *testing.T) {
	t.Setenv("CI", "")
	skipped := false
	t.Run("skips", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		chartOrSkip(t, "demo 1.2.3", "", fmt.Errorf("%w: no route to host", errChartUnreachable))
	})
	if !skipped {
		t.Fatal("an unreachable registry did not skip the test outside CI")
	}
}
