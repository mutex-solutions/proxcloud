package project

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

func TestCallerBaseSameHostUsesOnlyExplicitCredentials(t *testing.T) {
	cfg := sandbox.Config{TemplateRegistry: "registry.example.org/t"}
	for _, image := range []string{"registry.example.org/customer/app:1", "registry.example.org/t-extra/app:1"} {
		dc, err := registryDockerConfigContext(context.Background(), cfg, image, &fromRegistry{Type: "registry", Username: "caller", Password: "private"})
		if err != nil || authOf(t, dc, "registry.example.org") != "caller:private" {
			t.Fatalf("caller same-host repository rejected: %v", err)
		}
		c := &e2bCaller{proj: &Project{ID: "p"}, ss: sandbox.NewWithAPI(nil, cfg, "svc")}
		base, err := (&Handler{}).resolveBase(context.Background(), c, image, "", nil)
		if err != nil || base.Auth != sandbox.BaseAuthNone || len(base.DockerConfig) != 0 {
			t.Fatal("caller source inherited platform registry credential", err)
		}
	}
	for _, image := range []string{"registry.example.org/t/foreign/0123456789abcdef0123:1", "alias.example.org:5443/t/foreign/0123456789abcdef0123:1", "registry.example.org./t/project/0123456789abcdef0123:1"} {
		if _, err := parseCallerImage(image); !errors.Is(err, sandbox.ErrBadBuild) {
			t.Fatal("managed namespace escaped through origin alias", image)
		}
	}
}

func TestMalformedCallerBaseNeverEchoesCredentialURL(t *testing.T) {
	for _, image := range []string{"https://Alice:SuperSECRET@registry.example.org/customer/app:tag", "Alice:SuperSECRET@registry.example.org/customer/app:tag", "https://registry.example.org/customer/app:tag"} {
		c := &e2bCaller{proj: &Project{ID: "p"}, ss: sandbox.NewWithAPI(nil, sandbox.Config{}, "svc")}
		_, err := (&Handler{}).resolveBase(context.Background(), c, image, "", json.RawMessage(`{"type":"registry","username":"caller","password":"CallerSECRET"}`))
		if err == nil || strings.Contains(err.Error(), "SuperSECRET") || strings.Contains(err.Error(), "CallerSECRET") || strings.Contains(err.Error(), "Alice") {
			t.Fatalf("credential-bearing ref echoed: %v", err)
		}
	}
}
