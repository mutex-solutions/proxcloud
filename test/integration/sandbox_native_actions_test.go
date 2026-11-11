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

func TestNativeAbusePauseAdmissionPreservesSourceAndHistoricalCopy(t *testing.T) {
	for _, scenario := range []string{"pause-resume", "policy-changed-during-assembly", "activity-during-assembly", "uid-during-assembly", "stale-during-assembly", "bad-capture", "delete-paused", "source-gone-during-assembly", "source-gone-with-outage", "cr-uid-during-assembly", "container-restart-during-assembly", "missing-container-identity"} {
		t.Run(scenario, func(t *testing.T) {
			f, pc, _, _ := newPauseFixture(t)
			ctx := context.Background()
			if _, e := f.db.C("externalService").SetByID(ctx, "sbx", pgdoc.M{"config.sandbox.envd": false, "config.sandbox.abuse": pgdoc.M{"action": "pause", "windowSeconds": 60}}, nil); e != nil {
				t.Fatal(e)
			}
			code, body := f.do(sbxA, http.MethodPost, "/project/pa/cloud", `{"type":"SANDBOX","data":{"template":"base","timeoutSeconds":600}}`)
			var row struct {
				Data struct {
					ExternalID string `json:"externalId"`
				}
			}
			if code != 200 || json.Unmarshal([]byte(body), &row) != nil || row.Data.ExternalID == "" {
				t.Fatal(code, body)
			}
			id := row.Data.ExternalID
			pc.ready("sb-pa", id, time.Now().UTC())
			pc.crs["sb-pa/"+id]["metadata"].(map[string]any)["uid"] = "cr"
			now := time.Now().UTC()
			cpu, rx, tx := uint64(60e9), uint64(10), uint64(10)
			a := &abuseCluster{sbxCluster: f.cluster, podUID: "pod", containerID: "containerd://original", sample: sandbox.Sample{Name: id, Namespace: "sb-pa", Node: "n", PodUID: "pod", At: now, CPUAt: now, NetworkAt: now, CPU: &cpu, RX: &rx, TX: &tx}}
			nc := &nativeActionCluster{pauseCluster: pc, telemetry: a, files: map[string]string{"home/user/notes.txt": "native files retained by admission"}}
			f.h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
				return sandbox.NewWithAPI(nc, es.SandboxConfig(), es.ID), nil
			})
			safety := f.db.C("sandbox_safety")
			if e := safety.Ensure(ctx); e != nil {
				t.Fatal(e)
			}
			if e := f.db.C("auditEvent").Ensure(ctx); e != nil {
				t.Fatal(e)
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
			if e := safety.Upsert(ctx, "abuse:sbx:pa:"+id, obs); e != nil {
				t.Fatal(e)
			}
			if scenario == "missing-container-identity" {
				a.containerID = ""
			}
			nc.cut = scenario == "bad-capture"
			f.h.RunSandboxAbuse(ctx)
			if nc.count() != 1 || len(nc.layer) != 0 || nc.files["home/user/notes.txt"] == "" || nc.stoppedBeforeUsable {
				t.Fatal("abuse pause captured/stopped source")
			}
			// Detection/audit may persist under its existing contract, but mail is unwired.
			if scenario != "missing-container-identity" {
				n, e := f.db.C("auditEvent").Count(ctx, pgdoc.M{"action": "DETECTED"})
				if e != nil || n != 1 {
					t.Fatal("detection missing", n, e)
				}
				n, e = f.db.C("auditEvent").Count(ctx, pgdoc.M{"action": "ACTION_pause", "outcome": "FAILURE", "resourceMetadata.status": "refused-or-failed"})
				if e != nil || n != 1 {
					t.Fatal("refused pause action audit missing", n, e)
				}
			}
			seedHeldPause(t, f, pc, id, "pausing", now.Add(-time.Hour), false)
			switch scenario {
			case "policy-changed-during-assembly":
				_, _ = f.db.C("externalService").SetByID(ctx, "sbx", pgdoc.M{"config.sandbox.abuse.disabled": true}, nil)
			case "activity-during-assembly":
				_, _ = nc.MergePatchCR(ctx, "", "", "", "sb-pa", id, map[string]any{"metadata": map[string]any{"annotations": map[string]any{sandbox.AnnotationLastExec: now.Format(time.RFC3339Nano)}}})
			case "uid-during-assembly":
				a.podUID = "replacement-pod"
			case "cr-uid-during-assembly":
				pc.crs["sb-pa/"+id]["metadata"].(map[string]any)["uid"] = "replacement-cr"
			case "container-restart-during-assembly":
				a.containerID = "containerd://restart"
				a.restartCount = 1
			case "stale-during-assembly":
				a.sample.CPUAt = now.Add(-time.Hour)
			case "source-gone-during-assembly", "source-gone-with-outage":
				if e := pc.DeleteCR(ctx, "", "", "", "sb-pa", id); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "source-gone-with-outage" {
				nc.outage.Store(true)
			}
			before := heldStateDigest(t, f)
			code, body = f.do(sbxA, http.MethodGet, "/project/pa/sandbox-paused", "")
			if code != 200 || !strings.Contains(body, `"resumeAvailable":false`) || strings.Contains(body, "saved") || strings.Contains(body, "connectionHash") {
				t.Fatal("safe list", code, body)
			}
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				path := "/project/pb/sandbox-paused/" + id
				if method == http.MethodPost {
					path += "/resume"
				}
				if code, body = f.do(sbxB, method, path, `{}`); code != 404 {
					t.Fatal("foreign", code, body)
				}
			}
			code, body = f.do(sbxA, http.MethodPost, "/project/pa/sandbox-paused/"+id+"/resume", `{"timeoutSeconds":300}`)
			requireHeld(t, code, body)
			f.h.AutoPauseSandboxes(ctx)
			if heldStateDigest(t, f) != before || len(nc.layer) != 0 {
				t.Fatal("held recovery changed copy/state")
			}
			nc.outage.Store(false)
			if code, body = f.do(sbxA, http.MethodDelete, "/project/pa/sandbox-paused/"+id, ""); code != 204 {
				t.Fatal("explicit discard", code, body)
			}
			f.pausedState(id, "gone")
		})
	}
}
