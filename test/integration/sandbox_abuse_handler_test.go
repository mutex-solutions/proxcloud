//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"testing"
	"time"
)

type abuseCluster struct {
	*sbxCluster
	sample sandbox.Sample
	podUID string
}

func (a *abuseCluster) ListConfigMaps(context.Context, string, string) ([]map[string]any, error) {
	b, _ := json.Marshal([]sandbox.Sample{a.sample})
	return []map[string]any{{"metadata": map[string]any{"name": "node-n"}, "data": map[string]any{"samples": string(b)}}}, nil
}
func (a *abuseCluster) ListCRsAllNamespaces(context.Context, string, string, string, string) ([]map[string]any, error) {
	return a.sbxCluster.ListCRs(context.Background(), "", "", "", "sb-pa", "")
}
func (a *abuseCluster) pod() map[string]any {
	cr, _ := a.sbxCluster.GetCR(context.Background(), "", "", "", "sb-pa", a.sample.Name)
	spec := map[string]any{"nodeName": "n"}
	if cr != nil {
		ps := cr["spec"].(map[string]any)["podTemplate"].(map[string]any)["spec"].(map[string]any)
		for k, v := range ps {
			spec[k] = v
		}
	}
	return map[string]any{"metadata": map[string]any{"name": a.sample.Name, "namespace": "sb-pa", "uid": a.podUID, "labels": map[string]any{sandbox.LabelProject: "pa", sandbox.LabelService: "sbx", sandbox.LabelManagedBy: sandbox.ManagedByValue, sandbox.LabelSandboxID: a.sample.Name}}, "spec": spec, "status": map[string]any{"phase": "Running"}}
}
func (a *abuseCluster) ListPodsAllNamespaces(context.Context, string) ([]map[string]any, error) {
	return []map[string]any{a.pod()}, nil
}
func (a *abuseCluster) GetCore(context.Context, string, string, string) (map[string]any, error) {
	return a.pod(), nil
}

type abuseNotify struct{ fn func() }

func (n abuseNotify) SendTemplate(context.Context, string, []string, map[string]any) error {
	if n.fn != nil {
		n.fn()
	}
	return nil
}
func TestSandboxAbuseHandlerSafeActions(t *testing.T) {
	for _, scenario := range []string{"kill", "alert", "envd-kill", "envd-pause", "fresh-disabled", "fresh-threshold", "fresh-connection", "capture-busy", "reused-pod", "fresh-exec", "fresh-traffic", "frozen-metrics", "pause-unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSbxFixture(t, gatesHold)
			row := f.create()
			ctx := context.Background()
			id := row.ExternalID
			f.cluster.crs["sb-pa/"+id]["metadata"].(map[string]any)["uid"] = "cr"
			now := time.Now().UTC()
			cpu, rx, tx := uint64(60e9), uint64(10), uint64(10)
			a := &abuseCluster{sbxCluster: f.cluster, podUID: "pod", sample: sandbox.Sample{Name: id, Namespace: "sb-pa", Node: "n", PodUID: "pod", At: now, CPUAt: now, NetworkAt: now, CPU: &cpu, RX: &rx, TX: &tx}}
			action := "kill"
			if scenario == "alert" {
				action = "alert"
			}
			if scenario == "envd-pause" || scenario == "pause-unavailable" {
				action = "pause"
			}
			policy := sandbox.AbusePolicy{Action: action, WindowSeconds: 60}
			envd := scenario == "envd-kill" || scenario == "envd-pause"
			setPolicy := func(disabled bool) {
				_, err := f.db.C("externalService").SetByID(ctx, "sbx", pgdoc.M{"config.sandbox.abuse": map[string]any{"action": action, "windowSeconds": 60, "disabled": disabled}, "config.sandbox.envd": envd}, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			setPolicy(false)
			f.h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
				return sandbox.NewWithAPI(a, es.SandboxConfig(), es.ID), nil
			})
			safety := f.db.C("sandbox_safety")
			if e := safety.Ensure(ctx); e != nil {
				t.Fatal(e)
			}
			if e := f.db.C("auditEvent").Ensure(ctx); e != nil {
				t.Fatal(e)
			}
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
			prev.Cores = 1
			obs, _ := sandbox.Observation{}.Next(prev, policy, past)
			if e := safety.Upsert(ctx, "abuse:sbx:pa:"+id, obs); e != nil {
				t.Fatal(e)
			}
			f.h.SetSandboxSafety(safety, abuseNotify{fn: func() {
				switch scenario {
				case "fresh-disabled":
					setPolicy(true)
				case "fresh-threshold":
					_, _ = f.db.C("externalService").SetByID(ctx, "sbx", pgdoc.M{"config.sandbox.abuse.cpuPercent": 99}, nil)
				case "fresh-connection":
					_, _ = f.db.C("externalService").SetByID(ctx, "sbx", pgdoc.M{"secret": map[string]any{"kubeconfig": "changed"}}, nil)
				case "reused-pod":
					a.podUID = "recreated"
				case "fresh-exec":
					_, _ = a.MergePatchCR(ctx, "", "", "", "sb-pa", id, map[string]any{"metadata": map[string]any{"annotations": map[string]any{sandbox.AnnotationLastExec: time.Now().UTC().Format(time.RFC3339Nano)}}})
				case "frozen-metrics":
					a.sample.CPUAt = time.Now().Add(-5 * time.Minute)
					a.sample.NetworkAt = a.sample.CPUAt
				case "fresh-traffic":
					v := uint64(1e6)
					a.sample.RX = &v
				}
			}})
			if scenario == "capture-busy" {
				_, err := f.db.WithAdvisoryLock(ctx, "sandbox-capture:sbx:pa:"+id, false, func() error { f.h.RunSandboxAbuse(ctx); return nil })
				if err != nil {
					t.Fatal(err)
				}
			} else {
				f.h.RunSandboxAbuse(ctx)
			}
			n, e := f.db.C("auditEvent").Count(ctx, pgdoc.M{"resourceType": "SANDBOX_ABUSE", "action": "DETECTED", "projectId": "pa"})
			if e != nil || n != 1 {
				t.Fatalf("detection %d %v", n, e)
			}
			killed := f.cluster.count() == 0
			if killed != (scenario == "kill") {
				t.Fatalf("%s killed=%v", scenario, killed)
			}
			// Tenant B's project/resources were never targeted or counted.
			n, e = f.db.C("auditEvent").Count(ctx, pgdoc.M{"resourceType": "SANDBOX_ABUSE", "projectId": "pb"})
			if e != nil || n != 0 {
				t.Fatal("foreign event", n, e)
			}
		})
	}
}
