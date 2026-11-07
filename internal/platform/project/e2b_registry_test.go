package project

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

const (
	tProj = "6a4d47094f34c7e7d800e7b0"
	tTpl  = "0123456789abcdef0123"
	tHost = "192.0.2.10"
)

var tDigest = "sha256:" + strings.Repeat("ab", 32)

func TestRegistryKeys(t *testing.T) {
	repo := "t/" + tProj + "/" + tTpl
	full := tHost + "/" + repo
	cases := map[string][]string{
		full + "@" + tDigest:                          {repo + "@" + tDigest},
		full + ":build-1":                             {repo + ":build-1"},
		full + ":build-1@" + tDigest:                  {repo + ":build-1", repo + "@" + tDigest},
		"python:3.12-slim":                            nil,
		tHost + "/library/sandbox-base@" + tDigest:    nil, // the built-ins are never pruned
		"198.51.100.1/t/" + tProj + "/" + tTpl + ":x": nil, // another registry
		full + "@sha256:short":                        nil,
	}
	for ref, want := range cases {
		got := registryKeys(tHost, ref)
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Errorf("registryKeys(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestPruneCandidates(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	repo := "t/" + tProj + "/" + tTpl
	old := now.Add(-48 * time.Hour)
	d2 := "sha256:" + strings.Repeat("cd", 32)
	d3 := "sha256:" + strings.Repeat("ef", 32)
	arts := []registryArtifact{
		{Repo: repo, Digest: tDigest, Created: old, Tags: []string{"b1"}},                        // live by digest
		{Repo: repo, Digest: d2, Created: old, Tags: []string{"b2"}},                             // live by tag (build in progress)
		{Repo: repo, Digest: d3, Created: old, Tags: []string{"b3"}},                             // unreferenced: delete
		{Repo: repo, Digest: "sha256:" + strings.Repeat("11", 32), Created: now.Add(-time.Hour)}, // too new
		{Repo: repo, Digest: "sha256:" + strings.Repeat("22", 32)},                               // age unknown
		{Repo: "t/other/repo", Digest: d3, Created: old},                                         // not a template repo
		{Repo: "library/sandbox-base", Digest: d3, Created: old},                                 // a built-in
		{Repo: repo, Digest: "sha256:bad", Created: old},                                         // bad digest
	}
	live := map[string]bool{repo + "@" + tDigest: true, repo + ":b2": true}
	got := pruneCandidates(arts, live, now, RegistryPruneGrace)
	if len(got) != 1 || got[0].Digest != d3 {
		t.Fatalf("candidates = %+v", got)
	}
}

// fakeOCI is a registry speaking the OCI distribution API the way Zot does for the gc account.
func fakeOCI(t *testing.T, deleted *[]string) *httptest.Server {
	repo := "t/" + tProj + "/" + tTpl
	cfgDigest := "sha256:" + strings.Repeat("99", 32)
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "gc" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch p := r.URL.Path; {
		case r.Method == http.MethodGet && p == "/v2/_catalog":
			_ = json.NewEncoder(w).Encode(map[string]any{"repositories": []string{"library/sandbox-base", repo}})
		case r.Method == http.MethodGet && p == "/v2/"+repo+"/tags/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": []string{"b1", "b1-again"}})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/v2/"+repo+"/manifests/"):
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.manifest.v1+json") {
				t.Errorf("manifest GET without the OCI accept header: %q", r.Header.Get("Accept"))
			}
			w.Header().Set("Docker-Content-Digest", tDigest)
			_ = json.NewEncoder(w).Encode(map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "config": map[string]any{"digest": cfgDigest}})
		case r.Method == http.MethodGet && p == "/v2/"+repo+"/blobs/"+cfgDigest:
			_, _ = w.Write([]byte(`{"created":"2026-11-01T00:00:00Z","architecture":"amd64"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/v2/"+repo+"/manifests/"):
			*deleted = append(*deleted, p[strings.LastIndex(p, "/")+1:])
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
}

func TestOCIRegistryClient(t *testing.T) {
	var deleted []string
	srv := fakeOCI(t, &deleted)
	defer srv.Close()
	reg := &ociRegistry{base: srv.URL, cred: sandbox.RegistryCredential{User: "gc", Password: "pw"}, http: srv.Client()}
	arts, err := reg.artifacts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// library/ is skipped; both tags on one digest are one image; created comes from the image config.
	if len(arts) != 1 || arts[0].Repo != "t/"+tProj+"/"+tTpl || arts[0].Digest != tDigest || len(arts[0].Tags) != 2 ||
		!arts[0].Created.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("artifacts = %+v", arts)
	}
	if err := reg.delete(context.Background(), arts[0]); err != nil || len(deleted) != 1 || deleted[0] != tDigest {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	bad := &ociRegistry{base: srv.URL, cred: sandbox.RegistryCredential{User: "gc", Password: "wrong"}, http: srv.Client()}
	if _, err := bad.artifacts(context.Background()); err == nil {
		t.Fatal("a refused listing must be an error (nothing may be deleted on a partial view)")
	}
}

func TestPruneTarget(t *testing.T) {
	gc := sandbox.RegistryCredential{User: "gc", Password: "pw"}
	host, _, mode, cred, err := pruneTarget([]sandbox.Config{
		{TemplateRegistry: tHost + "/t", RegistryPrune: "on", RegistryGC: gc},
		{TemplateRegistry: tHost + "/t/", RegistryPrune: "dry-run"},
		{}, // no registry: not a user of it
	})
	if err != nil || host != tHost || mode != "dry-run" || cred != gc {
		t.Fatalf("target %q %q %v %v", host, mode, cred, err)
	}
	if _, _, _, _, err := pruneTarget([]sandbox.Config{{TemplateRegistry: tHost + "/t"}, {TemplateRegistry: "198.51.100.1/t"}}); err == nil {
		t.Fatal("two registries: each would see the other's images as unreferenced")
	}
}

// fakeS3 records what the context store sends: the payload hash header of every request, and the operations.
type fakeS3 struct {
	mu      sync.Mutex
	ops     []string
	hashes  []string
	objects map[string][]byte
	parts   map[string][]byte
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	q := r.URL.Query()
	f.hashes = append(f.hashes, r.Header.Get("X-Amz-Content-Sha256"))
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.ops = append(f.ops, "create")
		_, _ = w.Write([]byte(`<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>u1</UploadId></InitiateMultipartUploadResult>`))
	case r.Method == http.MethodPut && q.Has("partNumber"):
		f.ops = append(f.ops, "part")
		f.parts[q.Get("partNumber")] = body
		w.Header().Set("ETag", `"e`+q.Get("partNumber")+`"`)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		f.ops = append(f.ops, "complete")
		var all []byte
		for i := 1; i <= len(f.parts); i++ {
			all = append(all, f.parts[string(rune('0'+i))]...)
		}
		f.objects[r.URL.Path] = all
		_, _ = w.Write([]byte(`<CompleteMultipartUploadResult><ETag>"x-2"</ETag></CompleteMultipartUploadResult>`))
	case r.Method == http.MethodPut:
		f.ops = append(f.ops, "put")
		f.objects[r.URL.Path] = body
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestBuildStorePutSingleAndMultipart(t *testing.T) {
	f := &fakeS3{objects: map[string][]byte{}, parts: map[string][]byte{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	bs, err := buildStorage(sandbox.Config{BuildStorage: sandbox.BuildStorage{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"}})
	if err != nil {
		t.Fatal(err)
	}
	small := []byte("a small context")
	if err := bs.put(context.Background(), "p/t/h1.tar", bytes.NewReader(small)); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, contextPartSize+1234)
	_, _ = rand.Read(big)
	// A stream that is not seekable, as an HTTP request body is.
	if err := bs.put(context.Background(), "p/t/h2.tar", io.MultiReader(bytes.NewReader(big))); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.ops, ","); got != "put,create,part,part,complete" {
		t.Fatalf("operations %s", got)
	}
	if !bytes.Equal(f.objects["/b/p/t/h1.tar"], small) || !bytes.Equal(f.objects["/b/p/t/h2.tar"], big) {
		t.Fatal("the stored objects differ from what was uploaded")
	}
	for i, h := range f.hashes {
		if f.ops[i] != "create" && f.ops[i] != "complete" && h != "UNSIGNED-PAYLOAD" {
			t.Errorf("%s sent payload hash %q, want UNSIGNED-PAYLOAD", f.ops[i], h)
		}
	}
	// Presigned GETs name the endpoint the nodes reach.
	bs, _ = buildStorage(sandbox.Config{BuildStorage: sandbox.BuildStorage{Endpoint: srv.URL, PodEndpoint: "http://198.51.100.7:9000", Bucket: "b", AccessKey: "a", SecretKey: "s"}})
	u, err := bs.fetchURL(context.Background(), "p/t/h1.tar")
	if err != nil || !strings.HasPrefix(u, "http://198.51.100.7:9000/b/p/t/h1.tar?") || !strings.Contains(u, "X-Amz-Signature=") {
		t.Fatalf("presigned %s %v", u, err)
	}
}

func TestLimitedReaderRefusesLongBodies(t *testing.T) {
	lr := &limitedReader{r: strings.NewReader("12345"), n: 4}
	if _, err := io.ReadAll(lr); err != errContextTooLarge {
		t.Fatalf("over the limit: %v", err)
	}
	lr = &limitedReader{r: strings.NewReader("1234"), n: 4}
	if b, err := io.ReadAll(lr); err != nil || string(b) != "1234" {
		t.Fatalf("at the limit: %q %v", b, err)
	}
}

func TestConsoleView(t *testing.T) {
	now := time.Now().UTC()
	tpl := &sandboxTemplateDoc{ID: tTpl, Name: "app", CPU: 2, MemoryGiB: 2, Tags: map[string]string{"default": "b2", "v1": "b1"},
		Builds: []templateBuild{{BuildID: "b1", Status: sandbox.BuildReady, CreatedAt: &now}, {BuildID: "b2", Status: sandbox.BuildError, Reason: "a build step failed (exit 1)", CreatedAt: &now}}}
	v := consoleView(tpl)
	if v.Status != sandbox.BuildError || !v.Ready || len(v.Builds) != 2 || v.Builds[0].BuildID != "b2" || v.Builds[1].Tags[0] != "v1" {
		t.Fatalf("view %+v", v)
	}
}
