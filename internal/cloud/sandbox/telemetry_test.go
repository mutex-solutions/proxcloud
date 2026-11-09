package sandbox

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type sampleAPI struct {
	*fakeAPI
	cms, pods, all []map[string]any
}

func (a *sampleAPI) ListConfigMaps(context.Context, string, string) ([]map[string]any, error) {
	return a.cms, nil
}
func (a *sampleAPI) ListPodsAllNamespaces(context.Context, string) ([]map[string]any, error) {
	return a.pods, nil
}
func (a *sampleAPI) ListCRsAllNamespaces(context.Context, string, string, string, string) ([]map[string]any, error) {
	return a.all, nil
}
func TestTelemetryJoinsProviderProjectUIDAndNode(t *testing.T) {
	now := time.Now().UTC()
	id := "sbx-1234567890123456"
	cfg := testConfig()
	cr := BuildCR(cfg, "svc", Spec{ID: id, ProjectID: "p", Template: "python", CPU: 1, MemoryGiB: 1}, now)
	cr["metadata"].(map[string]any)["uid"] = "cr-uid"
	pod := map[string]any{"metadata": map[string]any{"name": id, "namespace": "sb-p", "uid": "pod-uid", "labels": map[string]any{LabelProject: "p", LabelService: "svc", LabelManagedBy: ManagedByValue, LabelSandboxID: id}}, "spec": map[string]any{"nodeName": "n"}, "status": map[string]any{"phase": "Running"}}
	x := sampleAt(now, 123)
	x.Namespace = "sb-p"
	x.Name = id
	x.Node = "n"
	x.PodUID = "pod-uid"
	b, _ := json.Marshal([]Sample{x})
	a := &sampleAPI{fakeAPI: &fakeAPI{}, all: []map[string]any{cr}, pods: []map[string]any{pod}, cms: []map[string]any{{"metadata": map[string]any{"name": "node-n"}, "data": map[string]any{"samples": string(b)}}}}
	s := NewWithAPI(a, cfg, "svc")
	xs, e := s.AbuseSamples(context.Background())
	if e != nil || len(xs) != 1 || xs[0].CPU == nil || xs[0].CRUID != "cr-uid" {
		t.Fatal(xs, e)
	}
	// A stale sample with another UID cannot authorize a new pod of the same name.
	pod["metadata"].(map[string]any)["uid"] = "recreated"
	xs, e = s.AbuseSamples(context.Background())
	if e != nil || len(xs) != 1 || xs[0].CPU != nil {
		t.Fatal(xs, e)
	}
	cr["metadata"].(map[string]any)["labels"].(map[string]any)[LabelService] = "foreign"
	xs, e = s.AbuseSamples(context.Background())
	if e != nil || len(xs) != 0 {
		t.Fatal(xs, e)
	}
}
