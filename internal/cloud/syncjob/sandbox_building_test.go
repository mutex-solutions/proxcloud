package syncjob

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// A timed sync that falls between a platform run's stage cluster (config.sandbox written) and its stage register (the
// kubeconfig written) meets a sandbox pool without a kubeconfig. While that run is not finished it is an expected
// state and one INFO line; with no run in flight, on an installer's provider, and when the run cannot be read, it is
// the ERROR it was (lab, v0.12.0-rc.2: one ERROR per timed sync during every build).
func TestSandboxPassWithoutKubeconfigWhileARunBuilds(t *testing.T) {
	ctx := context.Background()
	provider := func(owner string) *externalservice.ExternalService {
		sb := map[string]any{"pool": "proxcloud-sbx"}
		if owner != "" {
			sb["platformInstallationId"] = owner
		}
		return &externalservice.ExternalService{ID: "svc-pve", Config: map[string]any{"provider": "proxmox", "sandbox": sb}}
	}
	for _, c := range []struct {
		name      string
		es        *externalservice.ExternalService
		runActive func(context.Context, string) (bool, error)
		want      string
	}{
		{"a run of the owner is building", provider("pi-1"), func(_ context.Context, id string) (bool, error) { return id == "pi-1", nil }, "INFO"},
		{"no run in flight", provider("pi-1"), func(context.Context, string) (bool, error) { return false, nil }, "ERROR"},
		{"the run cannot be read", provider("pi-1"), func(context.Context, string) (bool, error) { return true, errors.New("db down") }, "ERROR"},
		{"the installer's provider", provider(""), func(context.Context, string) (bool, error) { return true, nil }, "ERROR"},
		{"no platform wired", provider("pi-1"), nil, "ERROR"},
	} {
		var out bytes.Buffer
		j := &Job{log: slog.New(slog.NewTextHandler(&out, nil)), runActive: c.runActive}
		// The pass itself: a pool and no kubeconfig stop it before any Proxmox call.
		err := j.reconcileSandboxWorkers(ctx, nil, c.es, true)
		if !errors.Is(err, errSandboxKubeconfigMissing) {
			t.Fatalf("%s: the pass answered %v", c.name, err)
		}
		j.logSandboxWorkers(ctx, c.es, err)
		if got := out.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "level="+c.want) || !strings.Contains(got, "svc-pve") {
			t.Errorf("%s: want one %s line, got %q", c.name, c.want, got)
		}
	}
	// Any other failure of the pass stays an error while a run builds.
	var out bytes.Buffer
	j := &Job{log: slog.New(slog.NewTextHandler(&out, nil)), runActive: func(context.Context, string) (bool, error) { return true, nil }}
	j.logSandboxWorkers(ctx, provider("pi-1"), errors.New("sandbox: kubeconfig: bad"))
	if got := out.String(); !strings.Contains(got, "level=ERROR") {
		t.Errorf("another error while a run builds: %q", got)
	}
}
