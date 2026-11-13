package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
)

const sandboxCleanupKubeconfig = `apiVersion: v1
kind: Config
current-context: c
clusters:
- name: k
  cluster: {server: "https://192.0.2.10:6443"}
contexts:
- name: c
  context: {cluster: k, user: u}
users:
- name: u
  user: {token: t}
`

// N4: the sandbox mirror runner's cleanup executor needs the provider's cluster only. A provider that lost its mirror
// account (no secret.registryMirror), moved its template registry and has no valid catalog still gets one, although
// Build refuses the first two as permanent; only a provider that no longer exists is permanent, a store error or an
// unusable kubeconfig is retried by the dead-job pass.
func TestSandboxCleanupBuilderNeedsOnlyTheCluster(t *testing.T) {
	p := templateupdates.SandboxPayload{Version: 1, Action: templateupdates.SandboxMirror, ServiceID: "svc-sbx", Name: "base",
		Registry: "192.0.2.226", Tag: "20261112-9d00563", Digest: "sha256:" + strings.Repeat("2", 64)}
	provider := func(kind string, secret map[string]any) *externalservice.ExternalService {
		return &externalservice.ExternalService{ID: "svc-sbx", Type: externalservice.TypeCloud, Secret: secret,
			Config: map[string]any{"provider": kind, "sandbox": map[string]any{"templateRegistry": "198.51.100.7/t"}}}
	}
	build := func(es *externalservice.ExternalService, err error) (*templateupdates.SandboxExecutor, error) {
		return sandboxCleanupBuilder(func(_ context.Context, id string) (*externalservice.ExternalService, error) {
			if id != p.ServiceID {
				t.Errorf("looked up %q", id)
			}
			return es, err
		}, templateupdates.PGSandbox{})(context.Background(), p)
	}
	x, err := build(provider("sandbox", map[string]any{"kubeconfig": sandboxCleanupKubeconfig}), nil)
	if err != nil || x == nil || x.Cluster == nil || x.Receipts == nil || x.Pins == nil || x.Registry != nil || x.SourceToken != nil {
		t.Fatalf("cleanup executor %+v %v", x, err)
	}
	type lookup struct {
		es  *externalservice.ExternalService
		err error
	}
	for name, c := range map[string]lookup{
		"deleted":           {nil, fmt.Errorf("%w: svc-sbx", externalservice.ErrNotFound)},
		"gone":              {nil, nil},
		"no longer sandbox": {provider("proxmox", map[string]any{"kubeconfig": sandboxCleanupKubeconfig}), nil},
	} {
		var perm *templateupdates.Permanent
		if _, err := build(c.es, c.err); !errors.As(err, &perm) || perm.Code != "provider_missing" {
			t.Errorf("%s: want permanent provider_missing, got %v", name, err)
		}
	}
	for name, c := range map[string]lookup{
		"store down":    {nil, errors.New("connection refused")},
		"no kubeconfig": {provider("sandbox", map[string]any{}), nil},
	} {
		var perm *templateupdates.Permanent
		if _, err := build(c.es, c.err); err == nil || errors.As(err, &perm) {
			t.Errorf("%s: want a retryable error, got %v", name, err)
		}
	}
}
