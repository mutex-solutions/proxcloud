package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type warmFake struct {
	*fakeAPI
	revision int
}

func (f *warmFake) CreateCore(ctx context.Context, p string, obj map[string]any) error {
	if f.core[p+"/"+digStr(obj, "metadata", "namespace")+"/"+digStr(obj, "metadata", "name")] != nil {
		return errors.New("conflict")
	}
	f.revision++
	obj["metadata"].(map[string]any)["resourceVersion"] = fmt.Sprint(f.revision)
	return f.ApplyCore(ctx, p, obj)
}
func (f *warmFake) GetCore(ctx context.Context, p, ns, name string) (map[string]any, error) {
	obj, err := f.fakeAPI.GetCore(ctx, p, ns, name)
	if obj == nil || err != nil {
		return obj, err
	}
	b, _ := json.Marshal(obj)
	var copy map[string]any
	_ = json.Unmarshal(b, &copy)
	return copy, nil
}
func (f *warmFake) ReplaceCore(ctx context.Context, p string, obj map[string]any) error {
	key := p + "/" + digStr(obj, "metadata", "namespace") + "/" + digStr(obj, "metadata", "name")
	if old := f.core[key]; old == nil || digStr(old, "metadata", "resourceVersion") != digStr(obj, "metadata", "resourceVersion") {
		return errors.New("resourceVersion conflict")
	}
	f.revision++
	obj["metadata"].(map[string]any)["resourceVersion"] = fmt.Sprint(f.revision)
	return f.ApplyCore(ctx, p, obj)
}
func warmNode(name string, image string) map[string]any {
	n := map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{LabelPrepullRuntime: "v1"}}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}
	if image != "" {
		n["status"].(map[string]any)["images"] = []any{map[string]any{"names": []any{image}}}
	}
	return n
}

const warmRegistry = "registry.example.test/t"
const warmProject = "0123456789abcdef01234567"

func warmTestConfig(f *warmFake) Config {
	cfg := testConfig()
	cfg.TemplateRegistry = warmRegistry
	f.secrets[NamespaceFor(warmProject)+"/"+TemplatePullSecret] = map[string][]byte{".dockerconfigjson": warmAuthDocument()}
	return cfg
}
func TestWarmAdmissionCoverageAndNewNodes(t *testing.T) {
	f := &warmFake{fakeAPI: newFake()}
	cfg := warmTestConfig(f)
	cfg.Warming = WarmConfig{Enabled: true, MaxImages: 2, MaxImageGiB: 15, MaxTotalGiB: 30, MaxParallel: 1, TTLSeconds: 60}
	s := NewWithAPI(f, cfg, "warm-test")
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	image := warmRegistry + "/0123456789abcdef01234567/0123456789abcdef0123@sha256:" + strings.Repeat("a", 64)
	tpl := Template{Image: image}
	f.nodes = []map[string]any{warmNode("n1", ""), warmNode("n2", image)}
	c, err := s.PrepareImage(context.Background(), warmProject, tpl, 12<<30)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pulled != 1 || c.Total != 2 || c.Ready {
		t.Fatal(c)
	}
	pods, _ := f.ListPods(context.Background(), PrepullNamespace, "")
	if len(pods) != 1 {
		t.Fatal("maxParallel", len(pods))
	}
	p := pods[0]
	ps := p["spec"].(map[string]any)
	cs := ps["containers"].([]any)[0].(map[string]any)
	if ps["runtimeClassName"] != "gvisor" || ps["automountServiceAccountToken"] != false || cs["command"].([]any)[1] != "-prepull" {
		t.Fatal(p)
	}
	if strings.Contains(fmt.Sprint(ps["volumes"]), "secret") || ps["hostPID"] != nil || ps["hostNetwork"] != nil {
		t.Fatal("pull worker leaked privilege", p)
	}
	f.nodes[0] = warmNode("n1", image)
	p["status"] = map[string]any{"phase": "Succeeded"}
	f.nodes = append(f.nodes, warmNode("n3", ""))
	if err = s.ReconcileWarming(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, err = s.ImageWarmCoverage(context.Background(), warmProject, tpl)
	if err != nil || c.Pulled != 2 || c.Total != 3 || c.Ready {
		t.Fatal(c, err)
	}
	refs, err := s.WarmRegistryRefs(context.Background())
	if err != nil || len(refs) != 1 || refs[0] != image {
		t.Fatal(refs, err)
	}
	now = now.Add(61 * time.Second)
	if err = s.ReconcileWarming(context.Background()); err != nil {
		t.Fatal(err)
	}
	refs, _ = s.WarmRegistryRefs(context.Background())
	pods, _ = f.ListPods(context.Background(), PrepullNamespace, "")
	if len(refs) != 0 || len(pods) != 0 {
		t.Fatal("expiry leaked intent", refs, pods)
	}
}
func TestWarmOwnershipDigestAndBudget(t *testing.T) {
	f := &warmFake{fakeAPI: newFake()}
	cfg := warmTestConfig(f)
	cfg.Warming = WarmConfig{Enabled: true, MaxImageGiB: 1, MaxTotalGiB: 1, MaxImages: 3}
	s := NewWithAPI(f, cfg, "warm-budget")
	for _, ref := range []string{warmRegistry + "/other/t@sha256:" + strings.Repeat("a", 64), "example.com/t@sha256:" + strings.Repeat("b", 64), warmRegistry + "/p1/t:mutable"} {
		if _, err := s.PrepareImage(context.Background(), warmProject, Template{Image: ref}, 0); err == nil {
			t.Fatal("accepted", ref)
		}
	}
	image := warmRegistry + "/0123456789abcdef01234567/0123456789abcdef0123@sha256:" + strings.Repeat("a", 64)
	if _, err := s.PrepareImage(context.Background(), warmProject, Template{Image: image}, 2<<30); err == nil {
		t.Fatal("exceeded disk admission")
	}
	for _, letter := range []string{"a", "b", "c"} {
		if _, err := s.PrepareImage(context.Background(), warmProject, Template{Image: warmRegistry + "/0123456789abcdef01234567/0123456789abcdef0123@sha256:" + strings.Repeat(letter, 64)}, 0); err != nil {
			t.Fatal(err)
		}
	}
	es, _ := s.warmEntries(context.Background())
	if len(es) != 1 {
		t.Fatal("disk/count cap", len(es))
	}
}
func TestWarmingRequiresRuntimeCapability(t *testing.T) {
	s := NewWithAPI(newFake(), testConfig(), "svc")
	n := warmNode("old", "")
	delete(n["metadata"].(map[string]any)["labels"].(map[string]any), LabelPrepullRuntime)
	if s.warmNode(n, warmEntry{}) {
		t.Fatal("old runtime must not start warming")
	}
}

func TestWarmingLeaseSerializesReplicas(t *testing.T) {
	f := &warmFake{fakeAPI: newFake()}
	cfg := warmTestConfig(f)
	cfg.Warming.Enabled = true
	a := NewWithAPI(f, cfg, "same-service")
	b := NewWithAPI(f, cfg, "same-service")
	release, err := a.claimWarmLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.claimWarmLease(context.Background()); err == nil {
		t.Fatal("lease admitted a second replica")
	}
	release()
	second, err := b.claimWarmLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second()
}

func TestWarmDisabledNeedsNoNewRBAC(t *testing.T) {
	s := newTestService(newFake())
	c, err := s.PrepareImage(context.Background(), warmProject, Template{Image: "whatever"}, 0)
	if err != nil || c.Enabled {
		t.Fatal(c, err)
	}
	if err = s.ReconcileWarming(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func warmAuthDocument() []byte {
	host, _, _ := strings.Cut(warmRegistry, "/")
	b, _ := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"username": "pull", "password": "only-this-project"}}})
	return b
}
