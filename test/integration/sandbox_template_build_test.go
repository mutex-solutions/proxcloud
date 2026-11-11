//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// buildCluster adds the build pods of sbx-build to the in-memory cluster.
type buildCluster struct {
	*sbxCluster
	pmu  sync.Mutex
	pods map[string]map[string]any
}

func (c *buildCluster) ListPods(_ context.Context, ns, _ string) ([]map[string]any, error) {
	if ns != sandbox.BuildNamespace {
		return nil, nil
	}
	time.Sleep(150 * time.Millisecond) // a real list is a round trip to the API server
	c.pmu.Lock()
	defer c.pmu.Unlock()
	var out []map[string]any
	for _, p := range c.pods {
		out = append(out, p)
	}
	return out, nil
}
func (c *buildCluster) ApplyCore(_ context.Context, plural string, obj map[string]any) error {
	if plural == "pods" {
		c.pmu.Lock()
		defer c.pmu.Unlock()
		c.pods[obj["metadata"].(map[string]any)["name"].(string)] = obj
	}
	return nil
}
func (c *buildCluster) GetCore(ctx context.Context, plural, ns, name string) (map[string]any, error) {
	if plural == "pods" && strings.HasPrefix(ns, "sb-") {
		return c.sbxCluster.GetCore(ctx, plural, ns, name)
	}
	c.pmu.Lock()
	defer c.pmu.Unlock()
	if plural == "pods" {
		return c.pods[name], nil
	}
	return nil, nil
}

func newTemplateFixture(t *testing.T) (*sbxFixture, *buildCluster, string) {
	f, keyA, _ := newE2BFixture(t, gatesHold)
	bc := &buildCluster{sbxCluster: f.cluster, pods: map[string]map[string]any{}}
	f.h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
		return sandbox.NewWithAPI(bc, es.SandboxConfig(), es.ID), nil
	})
	if _, err := f.db.C("externalService").SetByID(context.Background(), "sbx", pgdoc.M{
		"config": pgdoc.M{"provider": "sandbox", "region": "az1", "services": pgdoc.M{"sandbox": pgdoc.M{"az1": true}},
			"sandbox": pgdoc.M{"envd": true, "domain": "sbx.example.org", "templateRegistry": "192.0.2.226/t",
				"buildStorage": pgdoc.M{"endpoint": "http://192.0.2.10:9000", "bucket": "b", "region": "us-east-1"},
				"templates":    pgdoc.M{"base": pgdoc.M{"image": "base:1", "cpu": 1, "memoryGiB": 1}}}},
		"secret": pgdoc.M{"buildStorageAccessKey": "a", "buildStorageSecretKey": "s"}}, nil); err != nil {
		t.Fatal(err)
	}
	f.h.SetSandboxTemplates(f.db.C("sandbox_templates"))
	wireRuntimeCreateFixture(t, f)
	return f, bc, keyA
}

func requestBuild(t *testing.T, f *sbxFixture, key, name string) (tid, bid string) {
	t.Helper()
	code, body := f.e2b(key, http.MethodPost, "/v3/templates", `{"name":"`+name+`"}`)
	var out struct{ TemplateID, BuildID string }
	if code != http.StatusAccepted || json.Unmarshal([]byte(body), &out) != nil {
		t.Fatalf("v3/templates: %d %s", code, body)
	}
	return out.TemplateID, out.BuildID
}

// A template's ready, tagged build must survive later builds: ten more build requests (failed CI retries, or requests
// never started) must not trim away the build "default" points at. Otherwise the template stops resolving and the
// prune's live set loses its image.
func TestTemplateTaggedBuildSurvivesTrim(t *testing.T) {
	f, _, key := newTemplateFixture(t)
	tid, bid := requestBuild(t, f, key, "app")
	ctx := context.Background()
	var doc map[string]any
	if _, err := f.db.C("sandbox_templates").Get(ctx, tid, &doc); err != nil {
		t.Fatal(err)
	}
	b := doc["builds"].([]any)[0].(map[string]any)
	b["status"], b["image"] = sandbox.BuildReady, "192.0.2.226/t/pa/"+tid+"@sha256:"+strings.Repeat("a", 64)
	b["finishedAt"] = time.Now().UTC()
	doc["tags"] = map[string]any{"default": bid}
	if _, err := f.db.C("sandbox_templates").Replace(ctx, tid, doc); err != nil {
		t.Fatal(err)
	}
	if code, body := f.e2b(key, http.MethodPost, "/sandboxes", `{"templateID":"app","timeout":60}`); code != http.StatusCreated {
		t.Fatalf("before: create from app: %d %s", code, body)
	}
	for i := 0; i < 10; i++ {
		requestBuild(t, f, key, "app")
	}
	if code, body := f.e2b(key, http.MethodPost, "/sandboxes", `{"templateID":"app","timeout":60}`); code != http.StatusCreated {
		t.Fatalf("after 10 more build requests the tagged ready build is gone: %d %s", code, body)
	}
}

// maxConcurrentBuilds (2) must hold under concurrent starts, not only sequential ones: the count and the start are not
// serialised.
func TestTemplateConcurrentBuildLimit(t *testing.T) {
	f, bc, key := newTemplateFixture(t)
	type tb struct{ tid, bid string }
	var bs []tb
	for i := 0; i < 6; i++ {
		tid, bid := requestBuild(t, f, key, "app"+string(rune('a'+i)))
		bs = append(bs, tb{tid, bid})
	}
	var wg sync.WaitGroup
	for _, b := range bs {
		wg.Add(1)
		go func(b tb) {
			defer wg.Done()
			f.e2b(key, http.MethodPost, "/v2/templates/"+b.tid+"/builds/"+b.bid, `{"fromImage":"docker.io/library/alpine:3.20","steps":[]}`)
		}(b)
	}
	wg.Wait()
	bc.pmu.Lock()
	n := len(bc.pods)
	bc.pmu.Unlock()
	if n > 2 {
		t.Fatalf("%d build pods started concurrently for one project, limit is 2", n)
	}
}
