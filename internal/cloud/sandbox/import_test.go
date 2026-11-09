package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

const importTemplateID = "0123456789abcdef0123"

var importDigest = "sha256:" + strings.Repeat("a", 64)

func importFixture() (Config, *fakeAPI, ImportRequest) {
	f := newFake()
	cfg := testConfig()
	cfg.TemplateRegistry = warmRegistry
	cfg.Import.SourceRepositories = map[string][]string{"https://source.example.test": {"migration/app"}}
	f.secrets[NamespaceFor(warmProject)+"/"+TemplatePullSecret] = map[string][]byte{".dockerconfigjson": warmAuthDocument()}
	f.secrets[PrepullNamespace+"/sandbox-import-worker"] = map[string][]byte{"image": []byte("registry.example.test/library/proxcloud-sandbox-import@" + importDigest)}
	return cfg, f, ImportRequest{ProjectID: warmProject, TemplateID: importTemplateID, BuildID: "b1", Source: warmRegistry + "/" + warmProject + "/" + importTemplateID + "@" + importDigest, Destination: warmRegistry + "/" + warmProject + "/" + importTemplateID}
}

func TestImportWorkerRequiresOperatorDigestAndHost(t *testing.T) {
	cfg, f, _ := importFixture()
	s := NewWithAPI(f, cfg, "svc")
	for _, image := range []string{"", "registry.example.test/library/proxcloud-sandbox-import:latest", "evil.test/library/proxcloud-sandbox-import@" + importDigest, "registry.example.test.evil/library/proxcloud-sandbox-import@" + importDigest, "registry.example.test/library/stratos-sandbox-import@" + importDigest, "registry.example.test/x/../library/proxcloud-sandbox-import@" + importDigest} {
		f.secrets[PrepullNamespace+"/sandbox-import-worker"] = map[string][]byte{"image": []byte(image)}
		if _, err := s.ImportWorker(context.Background()); err == nil {
			t.Errorf("accepted worker %q", image)
		}
	}
}

func TestImportDispatchScopeAndCredentialSeparation(t *testing.T) {
	cfg, f, req := importFixture()
	s := NewWithAPI(f, cfg, "svc")
	if err := s.StartImport(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	pod := f.core["pods/"+BuildNamespace+"/build-b1"]
	cs := dig(pod, "spec", "containers").([]any)[0].(map[string]any)
	if cs["command"].([]any)[0] != "/sandbox-import" || len(cs["args"].([]any)) != 4 {
		t.Fatal(pod)
	}
	if strings.Contains(string(mustJSON(pod)), "only-this-project") {
		t.Fatal("credentials escaped Secret")
	}
	if strings.Contains(string(mustJSON(pod)), "push-secret") || strings.Contains(string(mustJSON(pod)), "source-secret") {
		t.Fatal("managed verification received push or source credentials")
	}
	if _, err := s.ImportPodState(context.Background(), "other", req.BuildID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	pod["status"] = map[string]any{"phase": "Unknown"}
	if err := s.DeleteImport(context.Background(), req.BuildID); err == nil {
		t.Fatal("unknown worker deleted")
	}
	if rows, _ := s.ListBuilds(context.Background(), warmProject); len(rows) != 0 {
		t.Fatal("import billed as build", rows)
	}
	if d, _ := s.BuildData(context.Background(), req.BuildID); d != nil {
		t.Fatal("import billed", d)
	}

	req.BuildID = "b2"
	req.Source = "source.example.test/migration/app@" + importDigest
	req.Destination += " :bad"
	if err := s.StartImport(context.Background(), req); err == nil {
		t.Fatal("malformed destination accepted")
	}
	req.Destination = warmRegistry + "/" + warmProject + "/" + importTemplateID + ":b2"
	if err := s.StartImport(context.Background(), req); err == nil {
		t.Fatal("omitted source auth admitted")
	}
	req.DockerConfig = []byte(`{"auths":{"source.example.test":{"username":"owner","password":"caller-only"}}}`)
	if err := s.StartImport(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	pod = f.core["pods/"+BuildNamespace+"/build-b2"]
	secret := f.core["secrets/"+BuildNamespace+"/"+baseSecretName(req.BuildID)]
	if digStr(secret, "metadata", "ownerReferences") != "" {
	} // owner references are validated below
	refs := dig(secret, "metadata", "ownerReferences").([]any)
	if refs[0].(map[string]any)["uid"] != digStr(pod, "metadata", "uid") {
		t.Fatal("source credential not pod-owned")
	}
	if strings.Contains(string(mustJSON(pod)), "caller-only") {
		t.Fatal("source secret in Pod")
	}
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

type importUncertainAPI struct{ *fakeAPI }

func (f *importUncertainAPI) ApplyCore(ctx context.Context, p string, obj map[string]any) error {
	_ = f.fakeAPI.ApplyCore(ctx, p, obj)
	if p == "pods" {
		return context.DeadlineExceeded
	}
	return nil
}
func TestImportUncertainApplyNeverDeletesWorker(t *testing.T) {
	cfg, base, req := importFixture()
	f := &importUncertainAPI{base}
	s := NewWithAPI(f, cfg, "svc")
	if err := s.StartImport(context.Background(), req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if base.core["pods/"+BuildNamespace+"/build-b1"] == nil {
		t.Fatal("ambiguous worker deleted")
	}
	s.requireBuildPins = true
	req.BuildID = "b3"
	if err := s.StartImport(context.Background(), req); err == nil {
		t.Fatal("production import accepted missing proof")
	}
	if base.core["pods/"+BuildNamespace+"/build-b3"] != nil {
		t.Fatal("unproven import dispatched")
	}
}

type importOutputAPI struct {
	*fakeAPI
	text string
}

func (f *importOutputAPI) PodLogs(context.Context, string, string, string, int) (string, error) {
	return f.text, nil
}
func TestImportVerifiedResultRequiresFrozenRefs(t *testing.T) {
	cfg, base, req := importFixture()
	f := &importOutputAPI{fakeAPI: base}
	s := NewWithAPI(f, cfg, "svc")
	if err := s.StartImport(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	base.core["pods/"+BuildNamespace+"/build-b1"]["status"] = map[string]any{"phase": "Succeeded"}
	result := sandboximport.Result{Source: req.Source, Image: req.Source, SourceDigest: importDigest, DestinationDigest: importDigest, Platform: "linux/amd64", ConfigDigest: importDigest, User: "app", Workdir: "/srv"}
	f.text = "2026-11-09T00:00:00Z " + string(mustJSON(result))
	st, r, _, err := s.ImportState(context.Background(), warmProject, req.BuildID)
	if err != nil || st != BuildReady || r == nil {
		t.Fatal(st, r, err)
	}
	result.Image = "evil.test/t/app@" + importDigest
	f.text = string(mustJSON(result))
	st, r, _, err = s.ImportState(context.Background(), warmProject, req.BuildID)
	if err != nil || st != BuildError || r != nil {
		t.Fatal("forged verification accepted", st, r, err)
	}
}
