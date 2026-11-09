//go:build integration

package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// Uses the existing real-handler/Postgres/S3 pause fixture, with host telemetry
// and a filesystem layer. Unlike the historical fake, capture refuses a CR that
// does not actually mount/boot the native helper; assembly/resume use that layer.
// Kubernetes and the image registry remain simulated, not a live-cluster test.
type nativeActionCluster struct {
	*pauseCluster
	telemetry           *abuseCluster
	files               map[string]string
	layer               []byte
	usable              bool
	stoppedBeforeUsable bool
	outage              atomic.Bool
	outageReads         atomic.Int32
	identityReads       atomic.Int32
}

func (c *nativeActionCluster) GetCR(ctx context.Context, g, v, p, ns, id string) (map[string]any, error) {
	if c.outage.Load() {
		c.outageReads.Add(1)
		return nil, fmt.Errorf("cluster read outage")
	}
	return c.sbxCluster.GetCR(ctx, g, v, p, ns, id)
}

func (c *nativeActionCluster) ListConfigMaps(ctx context.Context, ns, selector string) ([]map[string]any, error) {
	return c.telemetry.ListConfigMaps(ctx, ns, selector)
}
func (c *nativeActionCluster) ListCRsAllNamespaces(ctx context.Context, g, v, p, sel string) ([]map[string]any, error) {
	return c.telemetry.ListCRsAllNamespaces(ctx, g, v, p, sel)
}
func (c *nativeActionCluster) ListPodsAllNamespaces(ctx context.Context, sel string) ([]map[string]any, error) {
	return c.telemetry.ListPodsAllNamespaces(ctx, sel)
}
func (c *nativeActionCluster) GetCore(ctx context.Context, p, ns, id string) (map[string]any, error) {
	if ns == sandbox.BuildNamespace {
		return c.pauseCluster.GetCore(ctx, p, ns, id)
	}
	c.identityReads.Add(1)
	return c.telemetry.GetCore(ctx, p, ns, id)
}
func (c *nativeActionCluster) ExecStream(ctx context.Context, ns, id, container string, cmd []string, w io.Writer, limit int) (int, string, error) {
	cr, _ := c.GetCR(ctx, "", "", "", ns, id)
	if !sandbox.NativeActionTarget(cr) || len(cmd) != 2 || cmd[0] != sandbox.RuntimeMountPath+"/sandbox-init" || cmd[1] != "-snapshot" {
		return 2, "native helper/boot missing", nil
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, body := range c.files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o640, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	c.layer = b.Bytes()
	if c.cut {
		c.layer = c.layer[:len(c.layer)/2]
	}
	_, err := w.Write(c.layer)
	return 0, "", err
}
func (c *nativeActionCluster) DeleteCR(ctx context.Context, g, v, p, ns, id string) error {
	if !c.usable {
		c.stoppedBeforeUsable = true
	}
	c.files = nil
	return c.sbxCluster.DeleteCR(ctx, g, v, p, ns, id)
}
func (c *nativeActionCluster) ApplyCR(ctx context.Context, g, v, p string, obj map[string]any) error {
	if p == sandbox.CRPlural && c.layer != nil {
		if !c.usable {
			panic("resuming before image usable")
		}
		gz, err := gzip.NewReader(bytes.NewReader(c.layer))
		if err != nil {
			return err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		c.files = map[string]string{}
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			c.files[h.Name] = string(b)
		}
	}
	return c.sbxCluster.ApplyCR(ctx, g, v, p, obj)
}

func TestNativeAbusePauseRecovery(t *testing.T) {
	for _, scenario := range []string{"pause-resume", "policy-changed-during-assembly", "activity-during-assembly", "uid-during-assembly", "stale-during-assembly", "bad-capture", "delete-paused", "source-gone-during-assembly", "source-gone-with-outage", "cr-uid-during-assembly", "container-restart-during-assembly", "missing-container-identity"} {
		t.Run(scenario, func(t *testing.T) {
			f, pc, _, _ := newPauseFixture(t)
			ctx := context.Background()
			_, err := f.db.C("externalService").SetByID(ctx, "sbx", pgdoc.M{"config.sandbox.envd": false, "config.sandbox.abuse": pgdoc.M{"action": "pause", "windowSeconds": 60}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			code, body := f.do(sbxA, http.MethodPost, "/project/pa/cloud", `{"type":"SANDBOX","data":{"template":"base","timeoutSeconds":600}}`)
			var row struct {
				Data struct {
					ExternalID string `json:"externalId"`
				}
			}
			if code != 200 || json.Unmarshal([]byte(body), &row) != nil || row.Data.ExternalID == "" {
				t.Fatalf("create %d %s", code, body)
			}
			id := row.Data.ExternalID
			pc.ready("sb-pa", id, time.Now().UTC())
			pc.crs["sb-pa/"+id]["metadata"].(map[string]any)["uid"] = "cr"
			now := time.Now().UTC()
			cpu, rx, tx := uint64(60e9), uint64(10), uint64(10)
			a := &abuseCluster{sbxCluster: f.cluster, podUID: "pod", containerID: "containerd://original", sample: sandbox.Sample{Name: id, Namespace: "sb-pa", Node: "n", PodUID: "pod", At: now, CPUAt: now, NetworkAt: now, CPU: &cpu, RX: &rx, TX: &tx}}
			nc := &nativeActionCluster{pauseCluster: pc, telemetry: a, files: map[string]string{"home/user/notes.txt": "native files survive automatic pause"}}
			f.h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
				return sandbox.NewWithAPI(nc, es.SandboxConfig(), es.ID), nil
			})
			safety := f.db.C("sandbox_safety")
			if err := safety.Ensure(ctx); err != nil {
				t.Fatal(err)
			}
			if err := f.db.C("auditEvent").Ensure(ctx); err != nil {
				t.Fatal(err)
			}
			f.h.SetSandboxSafety(safety, nil)
			past := now.Add(-time.Minute)
			zero := uint64(0)
			prev := a.sample
			prev.At = past
			prev.CPUAt = past
			prev.NetworkAt = past
			prev.CPU = &zero
			prev.ProjectID = "pa"
			prev.SandboxID = id
			prev.CRUID = "cr"
			prev.ContainerID = "containerd://original"
			prev.Cores = 1
			obs, _ := sandbox.Observation{}.Next(prev, sandbox.AbusePolicy{Action: "pause", WindowSeconds: 60}, past)
			if err := safety.Upsert(ctx, "abuse:sbx:pa:"+id, obs); err != nil {
				t.Fatal(err)
			}
			pc.hold = true
			nc.cut = scenario == "bad-capture"
			if scenario == "missing-container-identity" {
				a.containerID = ""
			}
			f.h.RunSandboxAbuse(ctx)
			if scenario == "missing-container-identity" {
				f.pausedState(id, "gone")
				if nc.count() != 1 || nc.files["home/user/notes.txt"] == "" {
					t.Fatal("missing filesystem identity authorized pause")
				}
				return
			}
			if scenario == "bad-capture" {
				f.pausedState(id, "gone")
				if nc.files["home/user/notes.txt"] == "" || nc.count() != 1 {
					t.Fatal("failed capture lost source")
				}
				return
			}
			f.pausedState(id, "pausing")
			if nc.count() != 1 || len(nc.files) == 0 {
				t.Fatal("source stopped before image usable")
			}
			code, body = f.do(sbxA, http.MethodGet, "/project/pa/sandbox-paused", "")
			if code != 200 || !strings.Contains(body, id) || strings.Contains(body, "saved") || strings.Contains(body, "connectionHash") {
				t.Fatalf("paused list %d %s", code, body)
			}
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				path := "/project/pb/sandbox-paused/" + id
				if method == http.MethodPost {
					path += "/resume"
				}
				code, body = f.do(sbxB, method, path, `{}`)
				if code != 404 {
					t.Fatalf("foreign %s %d %s", method, code, body)
				}
			}
			identityReadsBefore := nc.identityReads.Load()
			switch scenario {
			case "policy-changed-during-assembly":
				_, err = f.db.C("externalService").SetByID(ctx, "sbx", pgdoc.M{"config.sandbox.abuse.disabled": true}, nil)
			case "activity-during-assembly":
				_, err = nc.MergePatchCR(ctx, "", "", "", "sb-pa", id, map[string]any{"metadata": map[string]any{"annotations": map[string]any{sandbox.AnnotationLastExec: time.Now().UTC().Format(time.RFC3339Nano)}}})
			case "uid-during-assembly":
				a.podUID = "reused"
				nc.files = map[string]string{} // replacement pod has none of the original's files
			case "cr-uid-during-assembly":
				pc.crs["sb-pa/"+id]["metadata"].(map[string]any)["uid"] = "replacement-cr"
				nc.files = map[string]string{}
			case "container-restart-during-assembly":
				a.containerID = "containerd://restarted"
				a.restartCount = 1
				nc.files = map[string]string{}
			case "stale-during-assembly":
				a.sample.CPUAt = now.Add(-5 * time.Minute)
			case "source-gone-during-assembly", "source-gone-with-outage":
				// The deadline/external deletion already removed the original;
				// its assembled image must never be discarded on failed reauth.
				err = nc.sbxCluster.DeleteCR(ctx, "", "", "", "sb-pa", id)
				nc.files = nil
			}
			if err != nil {
				t.Fatal(err)
			}
			nc.usable = true
			if scenario == "source-gone-with-outage" {
				nc.outage.Store(true)
			}
			pc.release()
			if scenario == "source-gone-with-outage" {
				// Wait for both final check and abandon guard to observe outage,
				// then the stopped driver leaves its copy and record recoverable.
				for deadline := time.Now().Add(10 * time.Second); nc.outageReads.Load() < 2 && time.Now().Before(deadline); {
					time.Sleep(10 * time.Millisecond)
				}
				if nc.outageReads.Load() < 2 {
					t.Fatal("finisher did not reach outage")
				}
				f.pausedState(id, "pausing")
				if len(f.snapshotRows("pa")) != 1 {
					t.Fatal("outage lost only snapshot")
				}
				nc.outage.Store(false)
				_, err = f.db.C("sandbox_paused").SetByID(ctx, id, pgdoc.M{"leaseUntil": time.Now().Add(-time.Minute)}, nil)
				if err != nil {
					t.Fatal(err)
				}
				f.h.AutoPauseSandboxes(ctx)
			}
			if scenario == "uid-during-assembly" || scenario == "cr-uid-during-assembly" || scenario == "container-restart-during-assembly" {
				// Wait for final authorization and abandonment to see replacement.
				// Unlike the old fake, the replacement does NOT have the notes file.
				wantReads := identityReadsBefore + 1
				if scenario == "cr-uid-during-assembly" {
					wantReads = identityReadsBefore
				} // CR mismatch short-circuits pod read
				for deadline := time.Now().Add(10 * time.Second); nc.identityReads.Load() < wantReads && time.Now().Before(deadline); {
					time.Sleep(10 * time.Millisecond)
				}
				// A ready storage row synchronizes image assembly even for a CR mismatch.
				for deadline := time.Now().Add(10 * time.Second); len(f.snapshotRows("pa")) != 1 && time.Now().Before(deadline); {
					time.Sleep(10 * time.Millisecond)
				}
				if len(f.snapshotRows("pa")) != 1 {
					t.Fatal("replacement lost original snapshot")
				}
				f.pausedState(id, "pausing")
				_, err = f.db.C("sandbox_paused").SetByID(ctx, id, pgdoc.M{"leaseUntil": time.Now().Add(-time.Minute)}, nil)
				if err != nil {
					t.Fatal(err)
				}
				path := "/project/pa/sandbox-paused/" + id + "/resume"
				code, body = f.do(sbxA, http.MethodPost, path, `{"timeoutSeconds":300}`)
				if code != 409 {
					t.Fatalf("replacement silently adopted: %d %s", code, body)
				}
				if len(f.snapshotRows("pa")) != 1 || nc.files["home/user/notes.txt"] != "" {
					t.Fatal("replacement discarded original files")
				}
				// Explicitly remove the replacement; recovery can now boot original image.
				if err = nc.sbxCluster.DeleteCR(ctx, "", "", "", "sb-pa", id); err != nil {
					t.Fatal(err)
				}
				code, body = f.do(sbxA, http.MethodPost, path, `{"timeoutSeconds":300}`)
				if code != 200 {
					t.Fatalf("original restore after replacement removal: %d %s", code, body)
				}
				f.pausedState(id, "gone")
				if nc.files["home/user/notes.txt"] != "native files survive automatic pause" {
					t.Fatal("original file not recovered")
				}
				return
			}
			if strings.Contains(scenario, "during-assembly") && scenario != "source-gone-during-assembly" {
				f.pausedState(id, "gone")
				if nc.count() != 1 || nc.files["home/user/notes.txt"] == "" {
					t.Fatal("late authorization failure deleted source")
				}
				return
			}
			f.pausedState(id, "")
			if nc.stoppedBeforeUsable || nc.count() != 0 || len(nc.files) != 0 {
				t.Fatal("pause did not stop source after usable image")
			}
			if len(f.snapshotRows("pa")) != 1 {
				t.Fatal("snapshot not billed")
			}
			if scenario == "delete-paused" {
				code, body = f.do(sbxA, http.MethodDelete, "/project/pa/sandbox-paused/"+id, "")
				if code != 204 {
					t.Fatalf("delete %d %s", code, body)
				}
				f.pausedState(id, "gone")
				return
			}
			// The native recovery path has the same profile/quota/release gates as E2B.
			_, err = f.db.C("billingProfile").SetByID(ctx, "bpa", pgdoc.M{"status": "SUSPENDED"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			path := "/project/pa/sandbox-paused/" + id + "/resume"
			code, body = f.do(sbxA, http.MethodPost, path, `{"timeoutSeconds":300}`)
			if code != 403 {
				t.Fatalf("suspended resume %d %s", code, body)
			}
			_, _ = f.db.C("billingProfile").SetByID(ctx, "bpa", pgdoc.M{"status": "ACTIVE"}, nil)
			code, body = f.do(sbxA, http.MethodPost, path, `{"timeoutSeconds":300}`)
			if code != 200 || !strings.Contains(body, `"resourceId"`) || !strings.Contains(body, id) {
				t.Fatalf("resume %d %s", code, body)
			}
			f.pausedState(id, "gone")
			if nc.count() != 1 || nc.files["home/user/notes.txt"] != "native files survive automatic pause" {
				t.Fatal("native resume lost files")
			}
			cr, _ := nc.GetCR(ctx, "", "", "", "sb-pa", id)
			if !sandbox.NativeActionTarget(cr) {
				t.Fatal("resume changed to SDK runtime")
			}
		})
	}
}
