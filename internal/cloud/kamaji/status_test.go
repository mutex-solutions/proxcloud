package kamaji

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Argo holds the Application at Progressing for minutes after CAPI says Available, because the
// ClusterResourceSet (MetalLB address pool) waits out a retry backoff. The sync reads READY only
// while that CRS is the one part not done, and only within resourceSetGrace of the create.
func TestClusterReadyPastResourceSet(t *testing.T) {
	cond := func(typ, status string) map[string]any {
		return map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": typ, "status": status}}}}
	}
	named := func(name string, obj map[string]any) map[string]any {
		obj["metadata"] = map[string]any{"name": name, "generation": float64(1)}
		return obj
	}
	type world struct {
		age     time.Duration
		sync    string
		ready   float64 // ready workers of 2
		hcp     string  // Ready condition of the MetalLB HelmChartProxy
		crs     string  // ResourcesApplied condition of the CRS
		readErr error
	}
	base := world{age: 10 * time.Minute, sync: "Synced", ready: 2, hcp: "True", crs: "False"}
	for _, tc := range []struct {
		name string
		edit func(*world)
		want string
	}{
		{"only the CRS left, inside the grace", func(*world) {}, "READY"},
		{"CRS still not applied past the grace", func(w *world) { w.age = 2 * time.Hour }, "PROGRESSING"},
		{"a chart not Ready", func(w *world) { w.hcp = "False" }, "PROGRESSING"},
		{"CRS applied, something else progressing", func(w *world) { w.crs = "True" }, "PROGRESSING"},
		{"one node short", func(w *world) { w.ready = 1 }, "PROGRESSING"},
		{"not synced", func(w *world) { w.sync = "OutOfSync" }, "PROGRESSING"},
		{"add-on read fails", func(w *world) { w.readErr = errors.New("boom") }, "PROGRESSING"},
	} {
		w := base
		tc.edit(&w)
		api := newFakeAPI()
		svc := NewWithAPI(api, testCfg(), "svc-1")
		const id = "pc-c-1"
		api.apps["argocd/"+id] = map[string]any{
			"metadata": map[string]any{"name": id, "creationTimestamp": time.Now().Add(-w.age).UTC().Format(time.RFC3339),
				"labels": map[string]any{LabelProject: "p1", LabelService: "svc-1", LabelManagedBy: ManagedByValue}},
			"spec": map[string]any{"source": map[string]any{"helm": map[string]any{"valuesObject": map[string]any{
				"nodeGroups": []any{map[string]any{"name": "workers", "machineCount": float64(2)}},
			}}}},
			"status": map[string]any{
				"health": map[string]any{"status": "Progressing"},
				"sync":   map[string]any{"status": w.sync},
				// The live shape on Argo 3.6: names and kinds, no health.
				"resources": []any{
					map[string]any{"kind": "Secret", "name": id + "-metallb-config"},
					map[string]any{"group": "addons.cluster.x-k8s.io", "kind": "ClusterResourceSet", "name": id + "-tenant-config"},
					map[string]any{"group": "addons.cluster.x-k8s.io", "kind": "HelmChartProxy", "name": id + "-metallb"},
				},
			},
		}
		api.tcps["pc-p1/"+ControlPlaneName(id)] = map[string]any{"status": map[string]any{
			"controlPlaneEndpoint": "10.0.0.5:6443",
			"kubernetesResources":  map[string]any{"version": map[string]any{"status": "Ready"}},
		}}
		api.mds = []map[string]any{{
			"metadata": map[string]any{"labels": map[string]any{labelNodeGroup: "workers"}},
			"status":   map[string]any{"replicas": float64(2), "readyReplicas": w.ready, "phase": "Running"},
		}}
		api.crs = map[string][]map[string]any{
			"helmchartproxies":    {named(id+"-metallb", cond("Ready", w.hcp))},
			"clusterresourcesets": {named(id+"-tenant-config", cond("ResourcesApplied", w.crs))},
		}
		api.crsErr = w.readErr
		rows, err := svc.SyncProvider("az1", "p1").List(context.Background())
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: List = %d rows, %v", tc.name, len(rows), err)
		}
		if got := rows[0].Data["cluster"].(map[string]any)["status"]; got != tc.want {
			t.Errorf("%s: status = %v, want %s", tc.name, got, tc.want)
		}
	}
}
