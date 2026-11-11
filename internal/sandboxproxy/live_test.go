//go:build sandboxlive

// Live drill of the public entry (docs/managed-sandbox.md §4.6): two tenants' sandboxes, each serving its name on
// port 8000 and created with allowPublicTraffic false, held while the caller tries their hosts from outside:
//
//	SANDBOX_KUBECONFIG=~/sa.kubeconfig SANDBOX_DOMAIN=sbx.example.org SANDBOX_OUT=/tmp/sbx.json [SANDBOX_HOLD=20m] \
//	  go test -tags sandboxlive ./internal/sandboxproxy/ -run TestEntryLive -v
//
// SANDBOX_OUT gets {"a": {"host", "token"}, "b": {...}}; the sandboxes go when SANDBOX_OUT is removed or after the hold.
package sandboxproxy

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

func TestEntryLive(t *testing.T) {
	kc, err := os.ReadFile(os.Getenv("SANDBOX_KUBECONFIG"))
	if err != nil {
		t.Skipf("SANDBOX_KUBECONFIG: %v", err)
	}
	out, domain := os.Getenv("SANDBOX_OUT"), os.Getenv("SANDBOX_DOMAIN")
	hold, _ := time.ParseDuration(os.Getenv("SANDBOX_HOLD"))
	if hold <= 0 {
		hold = 20 * time.Minute
	}
	s, err := sandbox.New(sandbox.Config{Kubeconfig: string(kc), Domain: domain,
		Templates: map[string]sandbox.Template{"web": {Image: "python:3.12-slim", CPU: 1, MemoryGiB: 1,
			Command: []string{"sh", "-c", `mkdir -p /srv && echo "tenant $TENANT" > /srv/index.html && cd /srv && exec python3 -m http.server 8000`}}},
		Limits: sandbox.Limits{MaxConcurrent: 2}}, "svc-entry-live")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stamp := time.Now().UTC().Format("150405")
	no := false
	res := map[string]map[string]string{}
	for _, tenant := range []string{"a", "b"} {
		project := "sb4" + tenant + stamp
		t.Cleanup(func() { _ = s.DeleteProjectNamespace(context.Background(), project) })
		sp := sandbox.Spec{ProjectID: project, Template: "web", Name: "web-" + tenant, AllowPublicTraffic: &no,
			Env: map[string]string{"TENANT": tenant}, TimeoutSeconds: int(hold.Seconds()) + 300}
		if err := sp.Validate(s.Config()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateSandbox(ctx, &sp); err != nil {
			t.Fatalf("create %s: %v", tenant, err)
		}
		deadline := time.Now().Add(5 * time.Minute)
		for {
			d, err := s.GetSandbox(ctx, project, sp.ID)
			if err == nil && d["status"] == sandbox.StatusRunning {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s not RUNNING: %v %v", sp.ID, d, err)
			}
			time.Sleep(2 * time.Second)
		}
		a, err := s.AccessFor(ctx, project, sp.ID)
		if err != nil || a.TrafficAccessToken == "" {
			t.Fatalf("access %s: %v", sp.ID, err)
		}
		res[tenant] = map[string]string{"id": sp.ID, "host": "8000-" + sp.ID + "." + domain, "token": a.TrafficAccessToken, "project": project}
		t.Logf("tenant %s: sandbox %s in %s", tenant, sp.ID, project)
	}
	b, _ := json.Marshal(res)
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("holding for %s (remove %s to end)", hold, out)
	for end := time.Now().Add(hold); time.Now().Before(end); time.Sleep(5 * time.Second) {
		if _, err := os.Stat(out); err != nil {
			break
		}
	}
	for _, r := range res {
		if err := s.DeleteSandbox(ctx, r["project"], r["id"]); err != nil && !strings.Contains(err.Error(), "not found") {
			t.Errorf("delete %s: %v", r["id"], err)
		}
	}
}
