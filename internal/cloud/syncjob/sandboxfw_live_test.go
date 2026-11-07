//go:build sandboxlive

// Live drill of the sandbox layer-1 pass against a real PVE cluster and sandbox cluster, with the portal's own PVE
// token (so a missing grant shows) and the proxcloud-firewall kubeconfig:
//
//	SANDBOX_PVE_URL=https://pve.example:8006 SANDBOX_PVE_TOKEN='proxcloud@pve!platform' SANDBOX_PVE_SECRET=... \
//	SANDBOX_FW_KUBECONFIG=~/fw.kubeconfig SANDBOX_PROVIDER=provider.json [SANDBOX_PASSES=30 SANDBOX_EVERY=20s] \
//	  go test -tags sandboxlive ./internal/cloud/syncjob/ -run TestSandboxFenceLive -v
//
// provider.json is the provider's config ({"network": {...}, "sandbox": {...}}). One pass by default; SANDBOX_PASSES
// repeats it, the way the sync job does, which is how a new worker is shown joining through its firewall.
package syncjob

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

func TestSandboxFenceLive(t *testing.T) {
	kc, err := os.ReadFile(os.Getenv("SANDBOX_FW_KUBECONFIG"))
	if err != nil {
		t.Skipf("SANDBOX_FW_KUBECONFIG: %v", err)
	}
	raw, err := os.ReadFile(os.Getenv("SANDBOX_PROVIDER"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["provider"] = "proxmox"
	es := &externalservice.ExternalService{ID: "live", Config: cfg, Secret: map[string]any{"sandboxKubeconfig": string(kc)}}
	ps, err := pve.NewService(pve.Config{
		APIURL: os.Getenv("SANDBOX_PVE_URL"), TokenID: os.Getenv("SANDBOX_PVE_TOKEN"), Secret: os.Getenv("SANDBOX_PVE_SECRET"),
		InsecureTLS: true,
	}, "live", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	j := &Job{log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})), now: time.Now}
	passes, _ := strconv.Atoi(os.Getenv("SANDBOX_PASSES"))
	every, _ := time.ParseDuration(os.Getenv("SANDBOX_EVERY"))
	for i := range max(passes, 1) {
		if i > 0 {
			time.Sleep(every)
		}
		start := time.Now()
		if err := j.reconcileSandboxWorkers(context.Background(), ps, es); err != nil {
			t.Errorf("pass %d: %v", i, err)
			continue
		}
		t.Logf("pass %d done in %s", i, time.Since(start).Round(time.Millisecond))
	}
}
