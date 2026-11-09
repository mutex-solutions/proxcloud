//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/registrypins"
)

type pinnedBuildCluster struct {
	*sbxCluster
	db      *pgdoc.DB
	mu      sync.Mutex
	pods    map[string]map[string]any
	applies int
	fail    bool
}

func (c *pinnedBuildCluster) ListPods(context.Context, string, string) ([]map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, p := range c.pods {
		out = append(out, p)
	}
	return out, nil
}
func (c *pinnedBuildCluster) ApplyCore(ctx context.Context, plural string, obj map[string]any) error {
	if plural != "pods" {
		return nil
	}
	if _, err := c.db.Tx(ctx); err != nil {
		return errors.New("existing project/start transaction absent")
	}
	var n int
	// The separate connection MUST see already committed pins before Apply.
	if err := c.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM registry_pin WHERE state='pending'`).Scan(&n); err != nil || n < 1 {
		return errors.New("pins were not committed before project TX")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applies++
	if c.fail {
		return errors.New("ambiguous apply")
	}
	md := obj["metadata"].(map[string]any)
	md["uid"] = "build-pod-uid"
	obj["status"] = map[string]any{"phase": "Running"}
	c.pods[md["name"].(string)] = obj
	return nil
}
func (c *pinnedBuildCluster) GetCore(context.Context, string, string, string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.pods {
		raw, _ := json.Marshal(p)
		var cp map[string]any
		_ = json.Unmarshal(raw, &cp)
		return cp, nil
	}
	return nil, nil
}
func (c *pinnedBuildCluster) PodLogs(context.Context, string, string, string, int) (string, error) {
	return "", nil
}
func (c *pinnedBuildCluster) DeleteCore(context.Context, string, string, string) error { return nil }
func newPinnedBuildFixture(t *testing.T) (*sbxFixture, *pinnedBuildCluster, string, *registrypins.Store) {
	t.Helper()
	f, _, key := newTemplateFixture(t)
	ctx := context.Background()
	bucket := httptest.NewServer(&s3Bucket{objects: map[string][]byte{}})
	t.Cleanup(bucket.Close)
	if _, err := f.db.C("externalService").SetByID(ctx, "sbx", pgdoc.M{"config.sandbox.buildStorage": pgdoc.M{"endpoint": bucket.URL, "bucket": "b", "region": "us-east-1"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := registrypins.EnsureSchema(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	f.h.SetSandboxBuildPins(f.db)
	c := &pinnedBuildCluster{sbxCluster: f.cluster, db: f.db, pods: map[string]map[string]any{}}
	f.h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
		return sandbox.NewWithAPI(c, es.SandboxConfig(), es.ID), nil
	})
	store, err := registrypins.New(f.db, "192.0.2.226")
	if err != nil {
		t.Fatal(err)
	}
	return f, c, key, store
}

type buildPinDoc struct {
	Tags   map[string]string
	Builds []struct {
		BuildID, Status, Image string
		Registry               struct {
			Operation                                       registrypins.Operation `json:"operation"`
			State, Base, Destination, PodUID, TerminalPhase string
			Pins                                            []registrypins.Pin
		} `json:"registryOperation"`
	}
}

func loadBuildPinDoc(t *testing.T, f *sbxFixture, tid string) buildPinDoc {
	t.Helper()
	var d buildPinDoc
	if _, err := f.db.C("sandbox_templates").Get(context.Background(), tid, &d); err != nil {
		t.Fatal(err)
	}
	return d
}
func TestRegistryBuildHandlerBusyRotationAndUnknownNoResend(t *testing.T) {
	for _, mode := range []string{"busy", "rotation", "apply_unknown"} {
		t.Run(mode, func(t *testing.T) {
			f, c, key, store := newPinnedBuildFixture(t)
			ctx := context.Background()
			tid, bid := requestBuild(t, f, key, "gated")
			if mode == "busy" {
				c.mu.Lock()
				for i := 0; i < 2; i++ {
					c.pods[fmt.Sprint(i)] = map[string]any{"metadata": map[string]any{"name": fmt.Sprint(i), "labels": map[string]any{sandbox.LabelProject: "pa"}}, "status": map[string]any{"phase": "Running"}}
				}
				c.mu.Unlock()
			}
			if mode == "rotation" {
				if err := f.db.C("sandbox_safety").Upsert(ctx, "rotation:sbx", pgdoc.M{"active": true}); err != nil {
					t.Fatal(err)
				}
				f.h.SetSandboxSafety(f.db.C("sandbox_safety"), nil)
			}
			if mode == "apply_unknown" {
				c.fail = true
			}
			path := "/v2/templates/" + tid + "/builds/" + bid
			code, _ := f.e2b(key, http.MethodPost, path, `{"fromTemplate":"base"}`)
			if code == http.StatusAccepted {
				t.Fatal("refused build accepted")
			}
			d := loadBuildPinDoc(t, f, tid)
			ps, err := store.PinsForOperation(ctx, d.Builds[0].Registry.Operation)
			if err != nil {
				t.Fatal(err)
			}
			want := "pre_dispatch_failed"
			state := registrypins.Released
			applies := 0
			if mode == "apply_unknown" {
				want = "unknown"
				state = registrypins.Unknown
				applies = 1
			}
			c.mu.Lock()
			got := c.applies
			c.mu.Unlock()
			if d.Builds[0].Registry.State != want || len(ps) != 1 || ps[0].State != state || got != applies {
				t.Fatalf("mode=%s doc=%+v pins=%+v applies=%d", mode, d, ps, got)
			}
			_, _ = f.e2b(key, http.MethodPost, path, `{"fromTemplate":"base"}`)
			f.h.SyncSandboxBuilds(ctx)
			c.mu.Lock()
			got = c.applies
			c.mu.Unlock()
			if got != applies {
				t.Fatal("retry/recovery redispatched")
			}
		})
	}
}

func TestRegistryBuildHandlerTerminalCrashStepRecoveryWithoutPod(t *testing.T) {
	for _, step := range []string{"before_handoff", "after_dest", "after_base"} {
		t.Run(step, func(t *testing.T) {
			f, c, key, store := newPinnedBuildFixture(t)
			ctx := context.Background()
			parentID, parentBuild := requestBuild(t, f, key, "parent")
			var parent map[string]any
			if _, err := f.db.C("sandbox_templates").Get(ctx, parentID, &parent); err != nil {
				t.Fatal(err)
			}
			pb := parent["builds"].([]any)[0].(map[string]any)
			pb["status"] = sandbox.BuildReady
			pb["image"] = "192.0.2.226/t/pa/" + parentID + "@sha256:" + strings.Repeat("b", 64)
			parent["tags"] = map[string]any{"default": parentBuild}
			if _, err := f.db.C("sandbox_templates").Replace(ctx, parentID, parent); err != nil {
				t.Fatal(err)
			}
			tid, bid := requestBuild(t, f, key, "recover")
			path := "/v2/templates/" + tid + "/builds/" + bid
			code, body := f.e2b(key, http.MethodPost, path, `{"fromTemplate":"parent"}`)
			if code != http.StatusAccepted {
				t.Fatalf("start %d %s", code, body)
			}
			d := loadBuildPinDoc(t, f, tid)
			op := d.Builds[0].Registry.Operation
			ev := registrypins.BuildPodEvidence{UID: "build-pod-uid", Operation: op.ID, Nonce: op.Generation, BaseRef: d.Builds[0].Registry.Base, DestinationRef: d.Builds[0].Registry.Destination, Phase: "Succeeded"}
			if err := store.RecordBuildPod(ctx, op, ev); err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if _, err := f.db.C("sandbox_templates").Get(ctx, tid, &raw); err != nil {
				t.Fatal(err)
			}
			bl := raw["builds"].([]any)[0].(map[string]any)
			reg := bl["registryOperation"].(map[string]any)
			image := "192.0.2.226/t/pa/" + tid + "@sha256:" + strings.Repeat("a", 64)
			bl["status"] = sandbox.BuildReady
			bl["image"] = image
			reg["state"] = "terminal_observed"
			reg["terminalPhase"] = "Succeeded"
			raw["tags"] = map[string]any{"default": bid}
			if _, err := f.db.C("sandbox_templates").Replace(ctx, tid, raw); err != nil {
				t.Fatal(err)
			}
			ps, err := store.PinsForOperation(ctx, op)
			if err != nil || len(ps) != 2 {
				t.Fatalf("expected owned BASE+DEST receipts: %+v %v", ps, err)
			}
			for _, p := range ps {
				if step == "after_dest" && p.RequestedRef == ev.DestinationRef {
					if _, err = store.ObserveBinding(ctx, p, registrypins.BindingEvidence{UID: "template:" + tid + ":" + bid, Generation: op.Generation, DigestRef: image}); err != nil {
						t.Fatal(err)
					}
				}
				if step == "after_base" && p.RequestedRef == ev.BaseRef {
					if _, err = store.ReleaseBuildTerminal(ctx, p, ev); err != nil {
						t.Fatal(err)
					}
				}
			}
			c.mu.Lock()
			c.pods = map[string]map[string]any{}
			c.mu.Unlock()
			f.h.SyncSandboxBuilds(ctx)
			d = loadBuildPinDoc(t, f, tid)
			ps, err = store.PinsForOperation(ctx, op)
			if err != nil || d.Builds[0].Registry.State != "finished" || len(ps) != 2 {
				t.Fatalf("recovery %+v %+v %v", d, ps, err)
			}
			for _, p := range ps {
				if p.RequestedRef == ev.BaseRef {
					if p.State != registrypins.Released || p.BindingUID != ev.UID || p.BindingGeneration != op.Generation {
						t.Fatalf("BASE recovery %+v", p)
					}
				} else if p.RequestedRef == ev.DestinationRef {
					if p.State != registrypins.Bound || p.Ref != image || p.BindingUID != "template:"+tid+":"+bid || p.BindingGeneration != op.Generation {
						t.Fatalf("DEST recovery %+v", p)
					}
				} else {
					t.Fatal("unexpected receipt", p)
				}
			}
			c.mu.Lock()
			n := c.applies
			c.mu.Unlock()
			if n != 1 {
				t.Fatal("recovery dispatched")
			}
		})
	}
}

func TestRegistryBuildHandlerRetainedFromHoldsParentBinding(t *testing.T) {
	f, _, key, store := newPinnedBuildFixture(t)
	ctx := context.Background()
	pid, pbid := requestBuild(t, f, key, "provenance-parent")
	var parent map[string]any
	if _, err := f.db.C("sandbox_templates").Get(ctx, pid, &parent); err != nil {
		t.Fatal(err)
	}
	op := pinOp()
	op.ProjectID = "pa"
	op.Owner = "build:" + pid + ":" + pbid
	dest := "192.0.2.226/t/pa/" + pid + ":" + pbid
	image := "192.0.2.226/t/pa/" + pid + "@sha256:" + strings.Repeat("a", 64)
	ps, err := store.BeginPins(ctx, op, []string{dest})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.ObserveBinding(ctx, ps[0], registrypins.BindingEvidence{UID: "template:" + pid + ":" + pbid, Generation: op.Generation, DigestRef: image})
	if err != nil {
		t.Fatal(err)
	}
	pb := parent["builds"].([]any)[0].(map[string]any)
	pb["status"] = sandbox.BuildReady
	pb["image"] = image
	pb["registryOperation"] = map[string]any{"operation": op, "state": "finished", "Destination": dest, "pins": []registrypins.Pin{bound}}
	parent["tags"] = map[string]any{"default": pbid}
	if _, err = f.db.C("sandbox_templates").Replace(ctx, pid, parent); err != nil {
		t.Fatal(err)
	}
	cid, _ := requestBuild(t, f, key, "provenance-child")
	var child map[string]any
	if _, err = f.db.C("sandbox_templates").Get(ctx, cid, &child); err != nil {
		t.Fatal(err)
	}
	cb := child["builds"].([]any)[0].(map[string]any)
	cb["status"] = sandbox.BuildReady
	cb["from"] = image
	if _, err = f.db.C("sandbox_templates").Replace(ctx, cid, child); err != nil {
		t.Fatal(err)
	}
	code, _ := f.e2b(key, http.MethodDelete, "/templates/"+pid, "")
	if code == http.StatusOK || code == http.StatusNoContent {
		t.Fatal("known child From did not hold parent")
	}
	ps, err = store.PinsForOperation(ctx, op)
	if err != nil || ps[0].State != registrypins.Bound {
		t.Fatalf("parent binding lost %+v %v", ps, err)
	}
	var held map[string]any
	found, err := f.db.C("sandbox_templates").Get(ctx, pid, &held)
	if err != nil || !found || len(held["retiredBuilds"].([]any)) != 1 {
		t.Fatal("held owner tombstone lost")
	}
	code, body := f.e2b(key, http.MethodDelete, "/templates/"+cid, "")
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("child removal %d %s", code, body)
	}
	code, body = f.e2b(key, http.MethodDelete, "/templates/"+pid, "")
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("parent later cleanup %d %s", code, body)
	}
	ps, _ = store.PinsForOperation(ctx, op)
	if ps[0].State != registrypins.Released {
		t.Fatal("parent binding not released after known child removal")
	}
}

func TestRegistryBuildHandlerRetiredChainMakesProgress(t *testing.T) {
	f, _, key, store := newPinnedBuildFixture(t)
	ctx := context.Background()
	tid, bid := requestBuild(t, f, key, "retired-chain")
	var raw map[string]any
	if _, err := f.db.C("sandbox_templates").Get(ctx, tid, &raw); err != nil {
		t.Fatal(err)
	}
	var builds []any
	var ops []registrypins.Operation
	previous := ""
	for i, id := range []string{bid, "second-build"} {
		op := pinOp()
		op.ProjectID = "pa"
		op.Owner = "build:" + tid + ":" + id
		dest := "192.0.2.226/t/pa/" + tid + ":" + id
		image := "192.0.2.226/t/pa/" + tid + "@sha256:" + strings.Repeat(fmt.Sprint(i+1), 64)
		ps, err := store.BeginPins(ctx, op, []string{dest})
		if err != nil {
			t.Fatal(err)
		}
		bound, err := store.ObserveBinding(ctx, ps[0], registrypins.BindingEvidence{UID: "template:" + tid + ":" + id, Generation: op.Generation, DigestRef: image})
		if err != nil {
			t.Fatal(err)
		}
		builds = append(builds, map[string]any{"buildID": id, "status": sandbox.BuildReady, "image": image, "from": previous, "registryOperation": map[string]any{"operation": op, "state": "finished", "Destination": dest, "pins": []registrypins.Pin{bound}}})
		ops = append(ops, op)
		previous = image
	}
	raw["builds"] = builds
	raw["tags"] = map[string]any{"default": "second-build"}
	if _, err := f.db.C("sandbox_templates").Replace(ctx, tid, raw); err != nil {
		t.Fatal(err)
	}
	_, _ = f.e2b(key, http.MethodDelete, "/templates/"+tid, "")
	f.h.SyncSandboxBuilds(ctx)
	f.h.SyncSandboxBuilds(ctx)
	for _, op := range ops {
		ps, err := store.PinsForOperation(ctx, op)
		if err != nil || len(ps) != 1 || ps[0].State != registrypins.Released {
			t.Fatalf("chain stalled %+v %v", ps, err)
		}
	}
	var cur map[string]any
	found, err := f.db.C("sandbox_templates").Get(ctx, tid, &cur)
	if err != nil || !found {
		t.Fatal("compact owner identity missing", err)
	}
	if retired, ok := cur["retiredBuilds"].([]any); ok && len(retired) != 0 {
		t.Fatal("eligible retired payloads retained")
	}
	ids, ok := cur["retiredBuildIDs"].(map[string]any)
	if !ok || len(ids) != 2 {
		t.Fatalf("compact retired identities lost %+v", cur["retiredBuildIDs"])
	}
}

func TestRegistryBuildHandlerConcurrentSingleApplyAndKnownReady(t *testing.T) {
	f, c, key, store := newPinnedBuildFixture(t)
	tid, bid := requestBuild(t, f, key, "pinned")
	path := "/v2/templates/" + tid + "/builds/" + bid
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := f.e2b(key, http.MethodPost, path, `{"fromTemplate":"base"}`)
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	accepted := 0
	for code := range codes {
		if code == http.StatusAccepted {
			accepted++
		}
	}
	c.mu.Lock()
	applies := c.applies
	c.mu.Unlock()
	if accepted != 1 || applies != 1 {
		t.Fatalf("accepted=%d applies=%d", accepted, applies)
	}
	var doc struct {
		Builds []struct {
			BuildID, Status, Image string
			Registry               struct {
				Operation registrypins.Operation `json:"operation"`
				State     string
				Pins      []registrypins.Pin
			} `json:"registryOperation"`
		}
		Tags map[string]string
	}
	if _, err := f.db.C("sandbox_templates").Get(context.Background(), tid, &doc); err != nil {
		t.Fatal(err)
	}
	op := doc.Builds[0].Registry.Operation
	ps, err := store.PinsForOperation(context.Background(), op)
	if err != nil || len(ps) != 1 || ps[0].State != registrypins.Pending || ps[0].BindingUID != "build-pod-uid" {
		t.Fatalf("running receipts=%+v %v", ps, err)
	}
	c.mu.Lock()
	for _, pod := range c.pods {
		pod["status"] = map[string]any{"phase": "Succeeded", "containerStatuses": []any{map[string]any{"name": "push", "state": map[string]any{"terminated": map[string]any{"exitCode": 0, "message": "sha256:" + strings.Repeat("a", 64)}}}}}
	}
	c.mu.Unlock()
	code, body := f.e2b(key, http.MethodGet, strings.TrimPrefix(path, "/v2")+"/status", "")
	if code != http.StatusOK {
		t.Fatalf("status %d %s", code, body)
	}
	if _, err = f.db.C("sandbox_templates").Get(context.Background(), tid, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Builds[0].Status != sandbox.BuildReady || doc.Builds[0].Registry.State != "finished" || doc.Tags["default"] != bid {
		t.Fatalf("not durably ready %+v", doc)
	}
	ps, err = store.PinsForOperation(context.Background(), op)
	if err != nil || ps[0].State != registrypins.Bound {
		t.Fatalf("DEST not bound %+v %v", ps, err)
	}
	// Explicit tag removal must persist but may not release the still-retained
	// ready build generation. Template removal then releases that exact binding.
	code, body = f.e2b(key, http.MethodDelete, "/templates/tags", `{"name":"pinned","tags":["default"]}`)
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("tag remove %d %s", code, body)
	}
	doc2 := loadBuildPinDoc(t, f, tid)
	if _, ok := doc2.Tags["default"]; ok {
		t.Fatal("removed tag restored")
	}
	ps, _ = store.PinsForOperation(context.Background(), op)
	if ps[0].State != registrypins.Bound {
		t.Fatal("tag removal released retained ready build")
	}
	code, body = f.e2b(key, http.MethodDelete, "/templates/"+tid, "")
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("template remove %d %s", code, body)
	}
	ps, _ = store.PinsForOperation(context.Background(), op)
	if ps[0].State != registrypins.Released {
		t.Fatal("deleted template binding not released")
	}
}
