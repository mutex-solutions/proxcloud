package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/registrypins"
)

func runtimeTestSpec() (Config, Spec) {
	cfg := testConfig().WithDefaults()
	cfg.TemplateRegistry = "registry.test/t"
	image := "registry.test/t/project1/0123456789abcdef0123@sha256:" + strings.Repeat("a", 64)
	cfg.Templates["managed"] = Template{Image: image, CPU: 1, MemoryGiB: 2, User: "user", Workdir: "/work", StartCmd: "serve", ReadyCmd: "ready"}
	q := DefaultProjectQuota
	return cfg, Spec{ID: "sbx-0123456789abcdef", ProjectID: "project1", Template: "managed", Quota: &q,
		Env: map[string]string{"KEY": "secret"}, Metadata: map[string]string{"key": "value"}, RuntimeSource: &RuntimeCreateSource{RequestedAlias: "alias:tag"}}
}

func TestRuntimeCreateCloneFreezesAllMutableFields(t *testing.T) {
	_, in := runtimeTestSpec()
	in.Custom = &Template{Image: "image", Command: []string{"sh", "-c"}, GPU: &GPURequest{Model: "gpu", Count: 1}}
	in.GPU = &GPURequest{Model: "gpu", Count: 1}
	dns := []string{"1.1.1.1"}
	in.Network = &Network{AllowOut: []string{"example.com"}, DNSServers: &dns}
	v := false
	in.AllowPublicTraffic = &v
	out := CloneCreateSpec(in)
	in.Env["KEY"], in.Metadata["key"] = "changed", "changed"
	in.Custom.Command[0], in.Custom.GPU.Model = "changed", "changed"
	in.GPU.Model, in.Network.AllowOut[0], (*in.Network.DNSServers)[0] = "changed", "changed", "changed"
	in.Quota.Sandboxes, in.RuntimeSource.RequestedAlias, *in.AllowPublicTraffic = 0, "changed", true
	if out.Env["KEY"] != "secret" || out.Metadata["key"] != "value" || out.Custom.Command[0] != "sh" || out.Custom.GPU.Model != "gpu" || out.GPU.Model != "gpu" || out.Network.AllowOut[0] != "example.com" || (*out.Network.DNSServers)[0] != "1.1.1.1" || out.Quota.Sandboxes == 0 || out.RuntimeSource.RequestedAlias != "alias:tag" || *out.AllowPublicTraffic {
		t.Fatal("clone aliases mutable caller state")
	}
	out.Env["NEW"] = "only clone"
	if _, ok := in.Env["NEW"]; ok {
		t.Fatal("clone mutated caller map")
	}
}

func TestRuntimeCreateFingerprintIncludesIgnoredAndEffectiveFields(t *testing.T) {
	cfg, in := runtimeTestSpec()
	base, err := RuntimeCreateSpec(cfg, "service", in)
	if err != nil {
		t.Fatal(err)
	}
	// Map order is canonical; private minted access tokens are intentionally absent.
	other := CloneCreateSpec(in)
	other.Env = map[string]string{"KEY": "secret"}
	other.envdToken, other.trafficToken = "new-envd", "new-traffic"
	same, err := RuntimeCreateSpec(cfg, "service", other)
	if err != nil || same != base {
		t.Fatalf("unstable hash: %v", err)
	}
	for name, change := range map[string]func(*Config, *Spec){
		"id": func(_ *Config, s *Spec) { s.ID = "sbx-aaaaaaaaaaaaaaaa" },
		"project": func(c *Config, s *Spec) {
			s.ProjectID = "project2"
			q := c.Templates["managed"]
			q.Image = strings.Replace(q.Image, "project1", "project2", 1)
			c.Templates = map[string]Template{"managed": q}
		},
		"quota":      func(_ *Config, s *Spec) { s.Quota.Sandboxes++ },
		"env":        func(_ *Config, s *Spec) { s.Env["KEY"] = "different" },
		"alias":      func(_ *Config, s *Spec) { s.RuntimeSource.RequestedAlias = "other:tag" },
		"custom":     func(c *Config, s *Spec) { q := c.Templates["managed"]; q.StartCmd = "other"; s.Custom = &q },
		"runtime":    func(c *Config, _ *Spec) { c.RuntimeClass = "different" },
		"envd":       func(c *Config, _ *Spec) { c.Envd = true },
		"connection": func(c *Config, _ *Spec) { c.Kubeconfig = "changed" },
		"egress":     func(c *Config, _ *Spec) { c.EgressMbps++ },
	} {
		t.Run(name, func(t *testing.T) {
			c := cfg
			s := CloneCreateSpec(in)
			change(&c, &s)
			got, err := RuntimeCreateSpec(c, "service", s)
			if err != nil {
				t.Fatal(err)
			}
			if got.Fingerprint == base.Fingerprint {
				t.Fatal("field omitted")
			}
		})
	}
}

func TestRuntimeCreateSourcePolicyAndGuardBeforeAnyK8s(t *testing.T) {
	cfg, in := runtimeTestSpec()
	for _, image := range []string{
		"registry.test/t/project1/0123456789abcdef0123:latest",
		"registry.test/t/foreign/0123456789abcdef0123@sha256:" + strings.Repeat("a", 64),
		"REGISTRY.TEST/t/project1/0123456789abcdef0123@sha256:" + strings.Repeat("a", 64),
		"https://registry.test/t/project1/0123456789abcdef0123@sha256:" + strings.Repeat("a", 64),
		"https://REGISTRY.TEST/t/project1/0123456789abcdef0123@sha256:" + strings.Repeat("a", 64),
		"https://external.test/base:latest",
		"external.test/../t/project1/0123456789abcdef0123:tag",
		"registry.test/t/project1/invalid@sha256:" + strings.Repeat("a", 64),
	} {
		c := cfg
		c.Templates = map[string]Template{"managed": {Image: image}}
		if _, _, err := RuntimeCreateImage(c, in); !errors.Is(err, registrypins.ErrScope) {
			t.Fatalf("accepted malformed/foreign %s: %v", image, err)
		}
	}
	for _, image := range []string{"registry.test/library/base:latest", "python:3.12", "external.test/team/base:latest"} {
		c := cfg
		c.Templates = map[string]Template{"managed": {Image: image}}
		if _, protected, err := RuntimeCreateImage(c, in); err != nil || protected {
			t.Fatalf("curated skip refused %s: %v", image, err)
		}
	}
	// A nil API panics on ANY K8s call: both required-proof and zero-proof-copy
	// failures must happen before even namespace/list reads.
	s := NewWithAPI(nil, cfg, "service")
	s.requireRuntimeCreatePins = true
	in.ID = ""
	if _, err := s.CreateSandbox(context.Background(), &in); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatalf("no proof: %v", err)
	}
	in.ID = "sbx-0123456789abcdef"
	in.RuntimePins = &registrypins.RuntimeProof{}
	if _, err := s.CreateSandbox(context.Background(), &in); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatalf("zero proof: %v", err)
	}
	s.requireRuntimeCreatePins = false
	if _, err := s.CreateSandbox(context.Background(), &in); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatalf("fake must consume supplied proof: %v", err)
	}
}

func TestRuntimeCreateMetadataCannotDecodeFromAPI(t *testing.T) {
	var s Spec
	if err := json.Unmarshal([]byte(`{"id":"sbx-01234567","RuntimePins":{},"RuntimeSource":{"RequestedAlias":"bypass"},"Quota":{"Sandboxes":99}}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.ID != "" || s.RuntimePins != nil || s.RuntimeSource != nil || s.Quota != nil {
		t.Fatal("server authority decoded from public request")
	}
}
