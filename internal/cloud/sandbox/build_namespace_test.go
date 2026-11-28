package sandbox

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

const readRegistry = "registry.example.org/sandbox-templates"

// sharedRead is a managed platform's cluster as the portal's identity meets it (sharedRBAC): project p1 attached and
// bound with one sandbox, no build namespace, and no right in a namespace the portal did not bind.
func sharedRead(t *testing.T, registry string) (*Service, *sharedRBAC) {
	t.Helper()
	ctx := context.Background()
	api := newSharedAPI()
	cfg := testConfig()
	cfg.Shared, cfg.TemplateRegistry = true, registry
	s := NewWithAPI(api, cfg, "svc")
	if err := s.EnsureProjectNamespace(ctx, "p1", nil); err != nil {
		t.Fatal(err)
	}
	cr := BuildCR(cfg, "svc", Spec{ID: "sbx-0000000000000010", ProjectID: "p1", Template: "python", CPU: 1, MemoryGiB: 1}, time.Now().Add(time.Hour))
	if err := api.ApplyCR(ctx, CRGroup, CRVersion, CRPlural, cr); err != nil {
		t.Fatal(err)
	}
	api.refused = nil
	return s, api
}

// buildNS404 answers a list in the build namespace as an API server that says the namespace is not there.
type buildNS404 struct{ *sharedRBAC }

func (a buildNS404) ListPods(ctx context.Context, ns, sel string) ([]map[string]any, error) {
	if ns == BuildNamespace {
		return nil, &kamajik8s.APIError{Status: 404, Message: `namespaces "sbx-build" not found`}
	}
	return a.sharedRBAC.ListPods(ctx, ns, sel)
}

func forbidden(err error) bool {
	var ae *kamajik8s.APIError
	return errors.As(err, &ae) && ae.Status == 403
}

// The sandbox read of a project on a cluster without a build namespace (v0.12.0-rc.2 on a managed platform: the list
// of builds answered 403 and failed the read of every project, so no ended sandbox was archived).
func TestSandboxReadWithoutABuildNamespace(t *testing.T) {
	ctx := context.Background()
	askedBuilds := []string{"list pods in " + BuildNamespace}

	t.Run("no registry: the build namespace is not asked", func(t *testing.T) {
		s, api := sharedRead(t, "")
		got, err := s.SyncProvider("az1", "p1").List(ctx)
		if err != nil || len(got) != 1 || len(api.refused) != 0 {
			t.Fatalf("read: %d rows, err %v, refused %v", len(got), err, api.refused)
		}
	})

	t.Run("registry, namespace not there, forbidden: no builds", func(t *testing.T) {
		s, api := sharedRead(t, readRegistry)
		got, err := s.SyncProvider("az1", "p1").List(ctx)
		if err != nil || len(got) != 1 || !slices.Equal(api.refused, askedBuilds) {
			t.Fatalf("read: %d rows, err %v, refused %v", len(got), err, api.refused)
		}
	})

	t.Run("registry, not found: no builds, whatever a read of the namespace says", func(t *testing.T) {
		s, api := sharedRead(t, readRegistry)
		api.namespaces[BuildNamespace] = map[string]string{}
		got, err := NewWithAPI(buildNS404{api}, s.cfg, "svc").SyncProvider("az1", "p1").List(ctx)
		if err != nil || len(got) != 1 {
			t.Fatalf("read: %d rows, err %v", len(got), err)
		}
	})

	t.Run("registry, the namespace is there and the identity is refused: an error", func(t *testing.T) {
		s, api := sharedRead(t, readRegistry)
		api.namespaces[BuildNamespace] = map[string]string{}
		if got, err := s.SyncProvider("az1", "p1").List(ctx); !forbidden(err) {
			t.Fatalf("a broken list of builds read as %d rows, err %v: running builds would end", len(got), err)
		}
	})

	t.Run("another namespace forbidden: an error", func(t *testing.T) {
		for _, registry := range []string{"", readRegistry} {
			s, api := sharedRead(t, registry)
			api.namespaces["sb-p2"] = map[string]string{LabelTenant: "true", LabelService: "svc"} // attached, never bound
			if got, err := s.SyncProvider("az1", "p2").List(ctx); !forbidden(err) {
				t.Fatalf("registry %q: a refused list of sandboxes read as %d rows, err %v: every sandbox of the project would end", registry, len(got), err)
			}
		}
	})

	// The other lists of the build namespace follow the same rule for a namespace that is not there, and none of them
	// reads a refusal in an existing namespace as "nothing". Only the list of builds rests on the registry: the mirror
	// cleanup and the registry prune ask a provider that lost its registry too.
	t.Run("the other lists of the build namespace", func(t *testing.T) {
		lists := func(s *Service) map[string]error {
			_, builds := s.ListBuilds(ctx, "p1")
			_, workers := s.ActiveBuildWorkers(ctx, "p1")
			_, refs := s.RegistryRefsInUse(ctx)
			_, mirror := s.ListMirrorPods(ctx, "0123456789abcdef")
			_, mirrors := s.ListAllMirrorPods(ctx)
			return map[string]error{"ListBuilds": builds, "ActiveBuildWorkers": workers, "RegistryRefsInUse": refs, "ListMirrorPods": mirror, "ListAllMirrorPods": mirrors}
		}
		s, api := sharedRead(t, readRegistry)
		for name, err := range lists(s) {
			if err != nil {
				t.Errorf("%s with no build namespace: %v", name, err)
			}
		}
		api.namespaces[BuildNamespace] = map[string]string{}
		for name, err := range lists(s) {
			if !forbidden(err) {
				t.Errorf("%s refused in an existing build namespace: %v", name, err)
			}
		}
		s, api = sharedRead(t, "")
		api.namespaces[BuildNamespace] = map[string]string{}
		for name, err := range lists(s) {
			if (name == "ListBuilds") != (err == nil) {
				t.Errorf("%s without a registry, refused in an existing build namespace: %v", name, err)
			}
		}
	})
}
