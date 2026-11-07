package kamaji

import "testing"

// Argo held the Application at Progressing for minutes after CAPI said Available, because its
// ClusterResourceSet waits out a retry backoff. Every customer-visible part done = READY; one
// node short, the config not synced, or no control plane yet stays PROGRESSING.
func TestClusterDataReadyPastArgoProgressing(t *testing.T) {
	app := func(sync string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"name": "pc-c-1"},
			"spec": map[string]any{"source": map[string]any{"helm": map[string]any{"valuesObject": map[string]any{
				"nodeGroups": []any{map[string]any{"name": "workers", "machineCount": float64(2)}},
			}}}},
			"status": map[string]any{"health": map[string]any{"status": "Progressing"}, "sync": map[string]any{"status": sync}},
		}
	}
	tcp := map[string]any{"status": map[string]any{
		"controlPlaneEndpoint": "10.0.0.5:6443",
		"kubernetesResources":  map[string]any{"version": map[string]any{"status": "Ready"}},
	}}
	md := func(ready float64) []map[string]any {
		return []map[string]any{{
			"metadata": map[string]any{"labels": map[string]any{labelNodeGroup: "workers"}},
			"status":   map[string]any{"replicas": float64(2), "readyReplicas": ready, "phase": "Running"},
		}}
	}
	for _, tc := range []struct {
		name  string
		sync  string
		tcp   map[string]any
		ready float64
		want  string
	}{
		{"all done", "Synced", tcp, 2, "READY"},
		{"one node short", "Synced", tcp, 1, "PROGRESSING"},
		{"not synced", "OutOfSync", tcp, 2, "PROGRESSING"},
		{"no control plane yet", "Synced", nil, 2, "PROGRESSING"},
	} {
		c := clusterData(app(tc.sync), tc.tcp, md(tc.ready))["cluster"].(map[string]any)
		if c["status"] != tc.want {
			t.Errorf("%s: status = %v, want %s", tc.name, c["status"], tc.want)
		}
	}
}
