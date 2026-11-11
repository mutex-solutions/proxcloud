package project

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

func TestRegistryHost(t *testing.T) {
	for ref, want := range map[string]string{
		"python:3.12": "", "library/python": "", "acme/app:1": "",
		"ghcr.io/acme/app:1": "ghcr.io", "localhost/x": "localhost", "reg.example.com:5000/a/b@sha256:1": "reg.example.com:5000",
		"192.0.2.10/t/p/t@sha256:1": "192.0.2.10",
	} {
		if got := registryHost(ref); got != want {
			t.Errorf("registryHost(%q) = %q, want %q", ref, got, want)
		}
	}
}

func authOf(t *testing.T, cfg []byte, key string) string {
	t.Helper()
	var c struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	a, ok := c.Auths[key]
	if !ok {
		t.Fatalf("no auth for %q in %s", key, cfg)
	}
	raw, _ := base64.StdEncoding.DecodeString(a.Auth)
	return string(raw)
}

var testSandboxCfg = sandbox.Config{TemplateRegistry: "192.0.2.10/t"}

func TestRegistryDockerConfig(t *testing.T) {
	cfg, err := registryDockerConfig(testSandboxCfg, "ghcr.io/acme/app:1", &fromRegistry{Type: "registry", Username: "u", Password: "p:w"})
	if err != nil || authOf(t, cfg, "ghcr.io") != "u:p:w" {
		t.Fatalf("registry: %s %v", cfg, err)
	}
	cfg, err = registryDockerConfig(testSandboxCfg, "europe-docker.pkg.dev/p/r/app", &fromRegistry{Type: "gcp", ServiceAccountJSON: `{"type":"service_account"}`})
	if err != nil || authOf(t, cfg, "europe-docker.pkg.dev") != `_json_key:{"type":"service_account"}` {
		t.Fatalf("gcp: %s %v", cfg, err)
	}
	// ECR through type registry: what `aws ecr get-login-password` prints is the password of user AWS.
	ecr := "123456789012.dkr.ecr.eu-west-1.amazonaws.com"
	cfg, err = registryDockerConfig(testSandboxCfg, ecr+"/app:1", &fromRegistry{Type: "registry", Username: "AWS", Password: "eyJ0b2tlbiI6"})
	if err != nil || authOf(t, cfg, ecr) != "AWS:eyJ0b2tlbiI6" {
		t.Fatalf("ecr as registry: %s %v", cfg, err)
	}
	for _, b := range []struct {
		image string
		r     fromRegistry
	}{
		{"ghcr.io/a", fromRegistry{Type: "registry", Username: "u"}},
		{"ghcr.io/a", fromRegistry{Type: "gcp", ServiceAccountJSON: "not json"}},
		{ecr + "/a", fromRegistry{Type: "aws"}}, // explicit AWS fields are required
		{"ghcr.io/a", fromRegistry{Type: "azure"}},
		{"192.0.2.10/t/p/t@sha256:1", fromRegistry{Type: "registry", Username: "u", Password: "p"}}, // our own registry
	} {
		if _, err := registryDockerConfig(testSandboxCfg, b.image, &b.r); !errors.Is(err, sandbox.ErrBadBuild) {
			t.Errorf("%s %+v: want a 400, got %v", b.image, b.r, err)
		}
	}
	if _, err := registryDockerConfig(testSandboxCfg, ecr+"/a", &fromRegistry{Type: "aws"}); !strings.Contains(err.Error(), "awsAccessKeyId") {
		t.Errorf("the AWS validation must require explicit credentials: %v", err)
	}
}

func TestInheritFromParent(t *testing.T) {
	parent := &templateBuild{User: "user", Workdir: "/home/user/app", StartCmd: "serve", ReadyCmd: "probe", StartAsRoot: true}
	bl := &templateBuild{}
	inheritFromParent(bl, parent, "", "")
	if bl.User != "user" || bl.Workdir != "/home/user/app" || bl.StartCmd != "serve" || bl.ReadyCmd != "probe" || !bl.StartAsRoot {
		t.Errorf("child without overrides = %+v", bl)
	}
	bl = &templateBuild{User: "root", StartCmd: "mine"}
	inheritFromParent(bl, parent, "mine", "")
	if bl.User != "root" || bl.Workdir != "/home/user/app" || bl.StartCmd != "mine" || bl.ReadyCmd != "" || bl.StartAsRoot {
		t.Errorf("setting a start command replaces the parent's pair: %+v", bl)
	}
	bl = &templateBuild{}
	inheritFromParent(bl, nil, "", "")
	if bl.User != "" || bl.Workdir != "" || bl.StartCmd != "" || bl.ReadyCmd != "" || bl.StartAsRoot {
		t.Errorf("no parent: %+v", bl)
	}
}

func TestDockerHubKeyAndRepoCase(t *testing.T) {
	for _, img := range []string{"acme/app:1", "docker.io/acme/app:1", "index.docker.io/acme/app"} {
		cfg, err := registryDockerConfig(testSandboxCfg, img, &fromRegistry{Type: "registry", Username: "u", Password: "p"})
		if err != nil || authOf(t, cfg, dockerHubAuthKey) != "u:p" {
			t.Errorf("%s: %s %v", img, cfg, err)
		}
	}
	for ref, want := range map[string]string{"ghcr.io/Acme/App:V1": "Acme/App", "Reg.Example.com:5000/a/b": "a/b", "python:3.12": "python"} {
		if got := imageRepoName(ref); got != want {
			t.Errorf("imageRepoName(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestTrimBuildsKeepsTaggedAndBuilding(t *testing.T) {
	id := func(i int) string { return string(rune('A' + i)) }
	var tpl sandboxTemplateDoc
	for i := 0; i < keptBuilds+5; i++ {
		tpl.Builds = append(tpl.Builds, templateBuild{BuildID: id(i), Status: sandbox.BuildWaiting})
	}
	tpl.Builds[0].Status = sandbox.BuildReady
	tpl.Builds[1].Status = sandbox.BuildBuilding
	tpl.Tags = map[string]string{"default": id(0)}
	trimBuilds(&tpl)
	if len(tpl.Builds) != keptBuilds+5 || tpl.build(id(0)) == nil || tpl.build(id(1)) == nil || tpl.build(id(2)) == nil || tpl.build(id(keptBuilds+4)) == nil {
		t.Fatalf("kept %+v", tpl.Builds)
	}
}
