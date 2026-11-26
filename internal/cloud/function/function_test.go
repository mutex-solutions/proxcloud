package function

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// The names every other part of functions builds against: the proxy selects on them, the chart's role and policies
// name them, the records carry them. A change here is a change of that contract, not a rename.
func TestNamesAreTheContract(t *testing.T) {
	for _, c := range [][2]string{
		{LabelID, "proxcloud.io/function-id"},
		{LabelHost, "proxcloud.io/function-host"},
		{LabelTenant, "proxcloud.io/function-tenant"},
		{AnnotationPort, "proxcloud.io/function-port"},
		{AnnotationSuspended, "proxcloud.io/function-suspended"},
		{AnnotationSecrets, "proxcloud.io/function-secrets-version"},
		{AnnotationRegistryAuth, "proxcloud.io/function-registry-auth-version"},
		{QuotaName, "proxcloud-function"},
		{sandbox.FunctionRole, "proxcloud-function-namespace"},
		{sandbox.LabelFunctionID, LabelID},
		{NamespaceFor("p1"), "fn-p1"},
		{PullSecretName("fn-0123456789abcdef"), "fn-0123456789abcdef-registry"},
		// What a function shares with a sandbox, and what it must never be taken for.
		{sandbox.LabelTenant, "proxcloud.io/tenant"},
		{sandbox.SharedAccount, "proxcloud-sandbox"}, // the RoleBinding's name in each fn-* namespace
		{sandbox.SharedRole, "proxcloud-sandbox-namespace"},
		{sandbox.NamespaceFor("p1"), "sb-p1"},
		{sandbox.LabelSandboxID, "proxcloud.io/sandbox-id"},
	} {
		if c[0] != c[1] {
			t.Errorf("name %q, want %q", c[0], c[1])
		}
	}
}

func TestIDAndHost(t *testing.T) {
	id := NewID()
	if !ValidID(id) || id == NewID() {
		t.Fatalf("id %q", id)
	}
	for _, bad := range []string{"", "fn-123", "sbx-0123456789abcdef", "fn-0123456789abcdeg", "fn-0123456789abcdef/scale"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
	host := NewHost("api", "sbx.example.com")
	label := HostLabel(host)
	if !strings.HasPrefix(label, "api-") || len(label) != len("api-")+6 || host != label+".sbx.example.com" || !ValidHostLabel(label) {
		t.Fatalf("host %q label %q", host, label)
	}
	// The longest allowed name still fits one DNS label once the suffix is on, and is still a function's.
	if l := HostLabel(NewHost(strings.Repeat("a", maxNameLen), "sbx.example.com")); len(l) != 47 || !ValidHostLabel(l) {
		t.Fatalf("label %q, length %d", l, len(l))
	}
	// A sandbox's host label starts with its port, a function's with a letter: one proxy on one domain tells them
	// apart by that, and the names it serves beside them (the domain's own labels, localhost) are no function's either.
	for _, other := range []string{"8080-sbx-0123456789abcdef", "49983-sbx-01234567", "sandbox", "localhost", "api", "api-a1b2c", "api-a1b2c3d", "api-A1B2C3", "-api-a1b2c3", "api--a1b2c3x", "1api-a1b2c3", strings.Repeat("a", maxNameLen+1) + "-a1b2c3", "api-a1b2c3,x", "api-a1b2c3=x"} {
		if ValidHostLabel(other) {
			t.Errorf("ValidHostLabel(%q) = true", other)
		}
	}
}

func TestSpecValidate(t *testing.T) {
	fc := sandbox.FunctionsConfig{Enabled: true}
	s := Spec{ProjectID: "p1", Name: "api", Snapshot: Snapshot{Image: " ghcr.io/acme/api@sha256:abc "}}
	if err := s.Validate(fc); err != nil {
		t.Fatal(err)
	}
	// A function that names no maxInstances runs one at most: whoever can reach its address keeps it awake, and
	// the owner pays for what runs.
	if s.Image != "ghcr.io/acme/api@sha256:abc" || s.Port != 8080 || s.CPUMillis != 250 || s.MemoryMi != 256 || s.IdleSeconds != 300 || s.MinInstances != 0 ||
		s.MaxInstances != 1 || s.Concurrency != 50 || s.Exposure != ExposurePrivate {
		t.Fatalf("defaults: %+v", s)
	}
	valid := func() Spec {
		return Spec{ProjectID: "p1", Name: "api", Snapshot: Snapshot{Image: "nginx:1.27"}}
	}
	ok := valid()
	ok.Name, ok.CPUMillis, ok.MemoryMi, ok.IdleSeconds, ok.MaxInstances, ok.Concurrency = strings.Repeat("a", maxNameLen), 2000, 4096, 60, 5, 1000
	if err := ok.Validate(fc); err != nil {
		t.Fatalf("at the limits: %v", err)
	}
	for _, c := range []struct {
		want string
		mut  func(*Spec)
	}{
		{"name must", func(s *Spec) { s.Name = "Api" }},
		{"name must", func(s *Spec) { s.Name = "-api" }},
		{"name must", func(s *Spec) { s.Name = "api-" }},
		{"name must", func(s *Spec) { s.Name = "" }},
		{"name must", func(s *Spec) { s.Name = "8080-api" }}, // a sandbox's host label starts with a digit: a function's never does
		{"name must", func(s *Spec) { s.Name = strings.Repeat("a", maxNameLen+1) }},
		{"image must", func(s *Spec) { s.Image = "nginx; rm -rf /" }},
		{"image must", func(s *Spec) { s.Image = "" }},
		{"port must", func(s *Spec) { s.Port = 70000 }},
		{"cpuMillis must", func(s *Spec) { s.CPUMillis = 300 }},
		{"cpuMillis must be one of 250, 500, 1000, 2000", func(s *Spec) { s.CPUMillis = 4000 }},
		{"memoryMi must", func(s *Spec) { s.MemoryMi = 128 }},
		{"memoryMi must", func(s *Spec) { s.MemoryMi = 8192 }},
		{"PORT is reserved", func(s *Spec) { s.Env = map[string]string{"PORT": "1"} }},
		{"is reserved", func(s *Spec) { s.Env = map[string]string{"PROXCLOUD_X": "1"} }},
		{"invalid environment", func(s *Spec) { s.Env = map[string]string{"a-b": "1"} }},
		{"command and args", func(s *Spec) { s.Args = make([]string, maxArgs+1) }},
		{"entries must", func(s *Spec) { s.Command = []string{"a\x00b"} }},
		{"minInstances must", func(s *Spec) { s.MinInstances = 1 }}, // maxMinInstances defaults to 0
		{"idleSeconds must", func(s *Spec) { s.IdleSeconds = 30 }},
		{"idleSeconds must be", func(s *Spec) { s.IdleSeconds = 7200 }},
		{"maxInstances must be between 1 and 5", func(s *Spec) { s.MaxInstances = 6 }}, // functions.maxInstances defaults to 5
		{"maxInstances must be between 1 and 5", func(s *Spec) { s.MaxInstances = -1 }},
		{"concurrency must be between 1 and 1000", func(s *Spec) { s.Concurrency = 1001 }},
		{"concurrency must be between 1 and 1000", func(s *Spec) { s.Concurrency = -5 }},
	} {
		s := valid()
		c.mut(&s)
		if err := s.Validate(fc); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v", c.want, err)
		}
	}

	// The provider's limits decide what is on offer.
	small := sandbox.FunctionsConfig{MaxCPUMillis: 500, MaxMinInstances: 1}
	s = valid()
	s.CPUMillis = 1000
	if err := s.Validate(small); err == nil || !strings.Contains(err.Error(), "one of 250, 500") {
		t.Errorf("cpu above the provider maximum: %v", err)
	}
	s = valid()
	s.MinInstances = 1
	if err := s.Validate(small); err != nil {
		t.Errorf("minInstances 1 with maxMinInstances 1: %v", err)
	}
	large := sandbox.FunctionsConfig{MaxCPUMillis: 4000}
	s = valid()
	s.CPUMillis = 3000
	if err := s.Validate(large); err != nil {
		t.Errorf("whole vCPUs above two under a provider maximum of four: %v", err)
	}
}

// A GPU, a build from a repository and a public endpoint are not offered. A request that names one is refused
// with ErrNotOffered as it arrives over the wire: decoded into the same Spec, never dropped as an unknown field and
// answered with a plain function from an image.
func TestSpecRefusesWhatIsNotOffered(t *testing.T) {
	fc := sandbox.FunctionsConfig{Enabled: true}
	for body, want := range map[string]string{
		`{"name":"render","image":"nginx:1","gpu":{"model":"nvidia-3090"}}`:                                      "gpu",
		`{"name":"render","image":"nginx:1","gpu":{}}`:                                                           "gpu",
		`{"name":"api","source":{"type":"github","installationId":7,"repo":"acme/api","branch":"main"}}`:         "source",
		`{"name":"api","image":"nginx:1","source":{"repo":"acme/api"}}`:                                          "source",
		`{"name":"api","image":"nginx:1","exposure":"public"}`:                                                   "exposure",
		`{"name":"api","image":"nginx:1","exposure":"internal"}`:                                                 "exposure",
		`{"name":"api","image":"nginx:1","exposure":"public","gpu":{"model":"nvidia-3090"},"source":{"x":true}}`: "gpu",
	} {
		var s Spec
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if err := s.Validate(fc); !errors.Is(err, ErrNotOffered) || !strings.HasPrefix(err.Error(), want+":") {
			t.Errorf("%s: got %v, want a refusal of %s", body, err, want)
		}
	}
	// Named as nothing, or as what a function is anyway, they change nothing; and what is stored afterwards names
	// neither.
	for _, body := range []string{
		`{"name":"api","image":"nginx:1"}`,
		`{"name":"api","image":"nginx:1","gpu":null,"source":null,"exposure":""}`,
		`{"name":"api","image":"nginx:1","exposure":"private"}`,
	} {
		var s Spec
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if err := s.Validate(fc); err != nil || s.Exposure != ExposurePrivate {
			t.Errorf("%s: %v, exposure %q", body, err, s.Exposure)
		}
		if out, _ := json.Marshal(s); strings.Contains(string(out), "gpu") || strings.Contains(string(out), "source") {
			t.Errorf("%s is stored as %s", body, out)
		}
	}
}

// The image is pulled by containerd on a sandbox machine, outside every network policy of the pod, from the host
// the customer names. That host is a DNS name: not an address in any spelling, not localhost, not a name the
// machine resolves by itself.
func TestImageRegistryMustBeADNSName(t *testing.T) {
	fc := sandbox.FunctionsConfig{Enabled: true}
	for _, image := range []string{
		"10.0.0.1/acme/api:1",
		"10.0.0.1:5000/acme/api:1",
		"127.0.0.1/api",
		"127.0.0.1:5000/api@sha256:abc",
		"169.254.169.254/latest:meta-data",
		"0x7f.0.0.1/api:1",                 // an address as inet_aton reads it
		"0x7f.0.0.0x1/api:1",               // the same, the last part in hex
		"2130706433:5000/api:1",            // 127.0.0.1 as one number
		"127.1/api:1",                      // 127.0.0.1, short
		"fe80::1/api:1",                    // IPv6, unbracketed
		"localhost/api:1",                  // the machine itself
		"localhost:5000/api:1",             //
		"LOCALHOST:5000/api:1",             //
		"registry:5000/api:1",              // one label: the machine's hosts file or search domain decides
		"registry.example.com.:5000/api:1", // an empty label
		"registry..example.com/api:1",
		"-registry.example.com/api:1",
		"registry.example.com:/api:1",
		"registry.example.com:99999x/api:1",
		"registry.example.123/api:1", // a numeric last label is an address's
	} {
		s := Spec{ProjectID: "p1", Name: "api", Snapshot: Snapshot{Image: image}}
		err := s.Validate(fc)
		if err == nil || !strings.Contains(err.Error(), "image must") {
			t.Errorf("%s: got %v, want a refusal", image, err)
		}
	}
	for _, image := range []string{
		"nginx:1.27", "acme/api@sha256:abc", "docker.io/acme/api:1", "ghcr.io/acme/api:1", "GHCR.io/acme/api:1",
		"registry.example.com:5000/team/api:1", "registry-1.docker.io/acme/api:1", "r.example.xn--p1ai/api:1", "1.example.com/api:1",
		// Two labels ending in a letter is a DNS name by this rule, whatever it resolves to: the machine's resolver
		// answers it, and nothing is claimed here of the address a name leads to.
		"kubernetes.default:443/api:1",
	} {
		s := Spec{ProjectID: "p1", Name: "api", Snapshot: Snapshot{Image: image}}
		if err := s.Validate(fc); err != nil {
			t.Errorf("%s: %v", image, err)
		}
	}
	// Credentials do not change the rule: they are for the image's registry, and that one is refused.
	s := Spec{ProjectID: "p1", Name: "api", Snapshot: Snapshot{Image: "10.0.0.1:5000/api:1", RegistryAuth: &RegistryAuth{Username: "bot"}}}
	if err := s.Validate(fc); err == nil || !strings.Contains(err.Error(), "DNS name") {
		t.Errorf("an address with credentials: %v", err)
	}
}

func TestEffectiveFloor(t *testing.T) {
	for _, c := range []struct{ platform, maxMin, fn, want int }{
		{0, 0, 0, 0},
		{0, 0, 1, 0}, // the customer's 1 is no longer allowed: clamped
		{0, 1, 1, 1},
		{0, 1, 0, 0},
		{1, 0, 0, 1}, // the platform floor applies to every function
		{1, 1, 1, 1},
	} {
		got := EffectiveFloor(sandbox.FunctionsConfig{MinInstances: c.platform, MaxMinInstances: c.maxMin}, c.fn)
		if got != c.want {
			t.Errorf("floor(platform %d, maxMin %d, fn %d) = %d, want %d", c.platform, c.maxMin, c.fn, got, c.want)
		}
	}
}

// Secrets follow the env rules, may not share a name with env, and no refusal repeats a value.
func TestValidateSecrets(t *testing.T) {
	env := map[string]string{"MODE": "prod"}
	if err := ValidateSecrets(env, map[string]string{"TOKEN": "hunter2", "EMPTY": ""}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSecrets(env, nil); err != nil {
		t.Fatal(err)
	}
	many := map[string]string{}
	for i := range 257 {
		many[fmt.Sprintf("S%d", i)] = "hunter2"
	}
	for want, secrets := range map[string]map[string]string{
		"PORT is reserved":   {"PORT": "hunter2"},
		"is reserved":        {"PROXCLOUD_X": "hunter2"},
		"invalid":            {"a-b": "hunter2"},
		"contains NUL":       {"TOKEN": "hunter2\x00"},
		"longer than":        {"TOKEN": strings.Repeat("hunter2", 5000)},
		"both an environmen": {"MODE": "hunter2"},
		"at most 256":        many,
	} {
		if err := ValidateSecrets(env, secrets); err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%q: got %v", want, err)
		}
	}
}

// Registry credentials are for the registry the image is pulled from: the server defaults to it
// (Docker Hub under the name the kubelet looks it up by), and credentials for another registry
// are refused instead of being sent along to fail the pull. Hosts are compared as the kubelet
// compares them: as written, and registry-1.docker.io is not Docker Hub to it.
func TestRegistryAuthValidate(t *testing.T) {
	fc := sandbox.FunctionsConfig{Enabled: true}
	for image, want := range map[string]string{
		"nginx:1.27":                      "docker.io",
		"acme/api@sha256:abc":             "docker.io",
		"docker.io/acme/api:1":            "docker.io",
		"index.docker.io/acme/api:1":      "docker.io",
		"ghcr.io/acme/api:1":              "ghcr.io",
		"GHCR.io/acme/api:1":              "GHCR.io",
		"registry-1.docker.io/acme/api:1": "registry-1.docker.io",
		"registry.example.com:5000/api:1": "registry.example.com:5000",
		"localhost/api:1":                 "localhost",
	} {
		if got := RegistryHost(image); got != want {
			t.Errorf("RegistryHost(%q) = %q, want %q", image, got, want)
		}
	}
	with := func(image, server string) Spec {
		return Spec{ProjectID: "p1", Name: "api", Snapshot: Snapshot{Image: image, RegistryAuth: &RegistryAuth{Server: server, Username: "bot"}}}
	}
	for _, c := range []struct{ image, server, want string }{
		{"acme/api:1", "", DockerHubServer},
		{"docker.io/acme/api:1", "", DockerHubServer},
		{"acme/api:1", "docker.io", "docker.io"},
		{"acme/api:1", "index.docker.io", "index.docker.io"},
		{"ghcr.io/acme/api:1", "", "ghcr.io"},
		{"GHCR.io/acme/api:1", "", "GHCR.io"},
		{"registry-1.docker.io/acme/api:1", "", "registry-1.docker.io"},
		{"ghcr.io/acme/api:1", "https://ghcr.io", "https://ghcr.io"},
		{"ghcr.io/acme/api:1", "ghcr.io/acme", "ghcr.io/acme"},
		{"registry.example.com:5000/api:1", "", "registry.example.com:5000"},
	} {
		s := with(c.image, c.server)
		if err := s.Validate(fc); err != nil || s.RegistryAuth.Server != c.want {
			t.Errorf("%s with server %q: server %q (err %v), want %q", c.image, c.server, s.RegistryAuth.Server, err, c.want)
		}
	}
	for _, c := range []struct {
		want string
		mut  func(*Spec)
	}{
		{"for ghcr.io but the image is pulled from docker.io", func(s *Spec) { s.Image = "acme/api:1" }},
		{"for quay.io but the image is pulled from ghcr.io", func(s *Spec) { s.RegistryAuth.Server = "https://quay.io/v2/" }},
		{"or null to remove", func(s *Spec) { s.Image = "registry.example.com:5000/api:1" }},
		{"for GHCR.IO but the image is pulled from ghcr.io", func(s *Spec) { s.RegistryAuth.Server = "GHCR.IO" }},
		{"for ghcr.io but the image is pulled from GHCR.io", func(s *Spec) { s.Image = "GHCR.io/acme/api:1" }},
		{"for registry-1.docker.io but the image is pulled from docker.io", func(s *Spec) {
			s.Image, s.RegistryAuth.Server = "acme/api:1", "registry-1.docker.io"
		}},
		{"for docker.io but the image is pulled from registry-1.docker.io", func(s *Spec) {
			s.Image, s.RegistryAuth.Server = "registry-1.docker.io/acme/api:1", DockerHubServer
		}},
		{"registryAuth.server must", func(s *Spec) { s.RegistryAuth.Server = "ghcr.io; rm -rf /" }},
		{"registryAuth.server must", func(s *Spec) { s.RegistryAuth.Server = "*.ghcr.io" }},
		{"registryAuth.username is required", func(s *Spec) { s.RegistryAuth.Username = "" }},
		{"registryAuth.username is required", func(s *Spec) { s.RegistryAuth.Username = "bot\n" }},
	} {
		s := with("ghcr.io/acme/api:1", "ghcr.io")
		c.mut(&s)
		if err := s.Validate(fc); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v", c.want, err)
		}
	}
}
