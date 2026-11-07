//go:build sandboxlive

// Live drill against a real sandbox cluster (the SB0 spike, or deploy/sandbox-cluster from
// SB2a), run with the proxcloud service account's kubeconfig:
//
//	SANDBOX_KUBECONFIG=~/sbx.kubeconfig [SANDBOX_GPU_MODEL=nvidia-3090] \
//	  go test -tags sandboxlive ./internal/cloud/sandbox/ -run TestLive -v
//
// Creates a throwaway project namespace, a CPU sandbox (and a GPU one when SANDBOX_GPU_MODEL is
// set), waits for Ready, runs commands, reads its log, extends the timeout, deletes everything
// and waits for the CR to be gone.
package sandbox

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

func TestLive(t *testing.T) {
	kc, err := os.ReadFile(os.Getenv("SANDBOX_KUBECONFIG"))
	if err != nil {
		t.Skipf("SANDBOX_KUBECONFIG: %v", err)
	}
	gpu := os.Getenv("SANDBOX_GPU_MODEL")
	cfg := Config{
		Kubeconfig: string(kc),
		Templates: map[string]Template{
			"python": {Image: "python:3.12-slim", CPU: 1, MemoryGiB: 1,
				Command: []string{"sh", "-c", "echo sandbox-live-started; exec sleep infinity"}},
			"pytorch": {Image: "pytorch/pytorch:2.9.1-cuda12.8-cudnn9-runtime", CPU: 2, MemoryGiB: 8},
		},
		Limits: Limits{MaxConcurrent: 3, MaxConcurrentGPUs: 1},
	}
	if gpu != "" {
		cfg.GPUModels = []string{gpu}
	}
	s, err := New(cfg, "svc-live")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	project := "live" + time.Now().UTC().Format("150405")
	t.Cleanup(func() {
		if err := s.DeleteProjectNamespace(context.Background(), project); err != nil {
			t.Errorf("cleanup namespace: %v", err)
		}
	})

	waitRunning := func(id string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			d, err := s.GetSandbox(ctx, project, id)
			if err != nil {
				t.Fatalf("get %s: %v", id, err)
			}
			if d["status"] == StatusRunning {
				if d["runningSince"] == nil {
					t.Fatalf("%s RUNNING without runningSince (billing start): %v", id, d)
				}
				return
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("%s not RUNNING within 5m", id)
	}

	specs := []Spec{{ProjectID: project, Template: "python", Name: "cpu"}}
	if gpu != "" {
		specs = append(specs, Spec{ProjectID: project, Template: "pytorch", Name: "gpu", GPU: &GPURequest{Model: gpu}})
	}
	for i := range specs {
		start := time.Now()
		if _, err := s.CreateSandbox(ctx, &specs[i]); err != nil {
			t.Fatalf("create %s: %v", specs[i].Name, err)
		}
		waitRunning(specs[i].ID)
		t.Logf("%s sandbox %s RUNNING after %s", specs[i].Name, specs[i].ID, time.Since(start).Round(100*time.Millisecond))
	}
	if gpu != "" {
		extra := Spec{ProjectID: project, Template: "pytorch", GPU: &GPURequest{Model: gpu}}
		if _, err := s.CreateSandbox(ctx, &extra); err == nil {
			t.Error("second GPU sandbox: want quota error")
		}
	}
	// Exec through the API server's WebSocket (konnectivity on a Kamaji control plane).
	start := time.Now()
	res, err := s.Exec(ctx, project, specs[0].ID, []string{"python", "-c", "import sys,platform; print(platform.release()); sys.stderr.write(sys.stdin.read()); sys.exit(7)"}, []byte("from-stdin"), 0)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "gvisor") || res.Stderr != "from-stdin" || res.ExitCode != 7 {
		t.Fatalf("exec result: %+v", res)
	}
	t.Logf("exec round trip %s: %+v", time.Since(start).Round(time.Millisecond), res)
	// A binary that is not in the image: must come back as a Kubernetes 400 (the API maps it to 400).
	_, err = s.Exec(ctx, project, specs[0].ID, []string{"no-such-binary-xyz"}, nil, 0)
	var kerr *kamajik8s.APIError
	if !errors.As(err, &kerr) || kerr.Status != 400 {
		t.Fatalf("missing executable: err = %v, want a kamajik8s.APIError with status 400", err)
	}
	t.Logf("missing executable: %v", err)
	// Egress, when the operator names targets: SANDBOX_PROBE_BLOCKED (a private host:port the
	// guardrail must drop) and SANDBOX_PROBE_OPEN (an internet host:port it must let through).
	// This is what proves the namespace label the service writes is the one the policy selects.
	probe := func(target string) int {
		t.Helper()
		host, port, _ := strings.Cut(target, ":")
		res, err := s.Exec(ctx, project, specs[0].ID, []string{"python", "-c",
			"import socket,sys; s=socket.socket(); s.settimeout(3)\ntry: s.connect((sys.argv[1], int(sys.argv[2])))\nexcept Exception as e: print(e); sys.exit(1)",
			host, port}, nil, 30*time.Second)
		if err != nil {
			t.Fatalf("probe %s: %v", target, err)
		}
		return res.ExitCode
	}
	if b := os.Getenv("SANDBOX_PROBE_BLOCKED"); b != "" {
		if probe(b) == 0 {
			t.Fatalf("sandbox reached %s: the guardrail does not select the namespace", b)
		}
		t.Logf("%s blocked", b)
	}
	if o := os.Getenv("SANDBOX_PROBE_OPEN"); o != "" {
		if probe(o) != 0 {
			t.Fatalf("sandbox could not reach %s", o)
		}
		t.Logf("%s open", o)
	}
	if gpu != "" {
		res, err := s.Exec(ctx, project, specs[1].ID, []string{"python", "-c", "import torch; print(torch.cuda.get_device_name(0))"}, nil, 2*time.Minute)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("gpu exec: %+v %v", res, err)
		}
		t.Logf("gpu exec: %q", strings.TrimSpace(res.Stdout))
	}
	d, err := s.SetTimeout(ctx, project, specs[0].ID, 7200)
	if err != nil {
		t.Fatalf("SetTimeout: %v", err)
	}
	t.Logf("shutdownTime now %v", d["shutdownTime"])
	// pods/log through the same tunnel: the template command's first line.
	logs, err := s.Logs(ctx, project, specs[0].ID, 50)
	if err != nil || !strings.Contains(logs, "sandbox-live-started") {
		t.Fatalf("logs: %q, err %v", logs, err)
	}
	t.Logf("logs: %q", strings.TrimSpace(logs))
	rows, err := s.SyncProvider("az1", project).List(ctx)
	if err != nil || len(rows) != len(specs) {
		t.Fatalf("sync list: %d rows, err %v", len(rows), err)
	}
	for _, sp := range specs {
		if err := s.DeleteSandbox(ctx, project, sp.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	// Gone means gone from the cluster, not only "delete answered".
	deadline := time.Now().Add(2 * time.Minute)
	for _, sp := range specs {
		for {
			if _, err := s.GetSandbox(ctx, project, sp.ID); errors.Is(err, ErrNotFound) {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("%s still there 2m after delete (err %v)", sp.ID, err)
			}
			time.Sleep(time.Second)
		}
	}
	if _, err := s.Logs(ctx, project, specs[0].ID, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("logs after delete: err %v, want ErrNotFound", err)
	}
}
