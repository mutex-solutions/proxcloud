//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/registrypins"
)

// Runtime-aware template fixtures use the REAL PRIMARY protocol, not a guard
// bypass or fake RuntimeProof. Negative readiness settings remain unchanged.
func wireRuntimeCreateFixture(t *testing.T, f *sbxFixture) {
	t.Helper()
	ctx := context.Background()
	if err := registrypins.EnsureSchema(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	if err := project.EnsureSandboxRuntimePins(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	f.h.SetSandboxRuntimePins(f.db)
}

// The fake controller's runtime Pod is a distinct object with its own UID,
// controller ownerReference, copied template annotations and actual main imageID.
// It is read independently, never manufactured from a pin/operation receipt.
func (c *sbxCluster) GetCore(_ context.Context, plural, ns, name string) (map[string]any, error) {
	if plural != "pods" {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cr := c.crs[ns+"/"+name]
	if cr == nil {
		return nil, nil
	}
	raw, _ := json.Marshal(cr["spec"].(map[string]any)["podTemplate"])
	var pod map[string]any
	_ = json.Unmarshal(raw, &pod)
	meta := pod["metadata"].(map[string]any)
	meta["name"], meta["namespace"], meta["uid"] = name, ns, "runtime-pod-"+name
	meta["ownerReferences"] = []any{map[string]any{"apiVersion": sandbox.CRGroup + "/" + sandbox.CRVersion, "kind": "Sandbox", "name": name, "uid": cr["metadata"].(map[string]any)["uid"], "controller": true}}
	image := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"].(string)
	pod["status"] = map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "main", "imageID": "docker-pullable://" + image}}}
	return pod, nil
}
