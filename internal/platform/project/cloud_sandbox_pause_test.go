package project

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// autoPause and autoResume are refused where the location cannot honour them, on every create path: the console and
// MCP create_sandbox go through createSandboxResource like the E2B create.
func TestSandboxPauseRefusal(t *testing.T) {
	builds := sandbox.Config{TemplateRegistry: "reg/x", BuildStorage: sandbox.BuildStorage{Bucket: "b", Endpoint: "http://s3"}}
	wake := builds
	wake.ProxyWakeToken = "w"
	on := &Handler{sbTemplates: &pgdoc.Store{}, sbPaused: &pgdoc.Store{}}
	for _, c := range []struct {
		name    string
		h       *Handler
		cfg     sandbox.Config
		pause   bool
		resume  bool
		wantMsg string
	}{
		{"plain", &Handler{}, sandbox.Config{}, false, false, ""},
		{"pause, no stores", &Handler{}, builds, true, false, "autoPause is not available at this location"},
		{"pause, no build bucket", on, sandbox.Config{}, true, false, "autoPause is not available at this location"},
		{"pause", on, builds, true, false, sandbox.ErrManagedSavedStartHeld.Error()},
		{"resume without pause", on, wake, false, true, "autoResume needs autoPause"},
		{"resume, no wake token", on, builds, true, true, "autoResume is not available at this location"},
		{"pause and resume", on, wake, true, true, sandbox.ErrManagedSavedStartHeld.Error()},
	} {
		spec := &sandbox.Spec{ProjectID: "p", AutoPause: c.pause, AutoResume: c.resume}
		err := c.h.pauseRefusal(c.cfg, spec)
		if c.wantMsg == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", c.name, err)
			}
			continue
		}
		if c.wantMsg == sandbox.ErrManagedSavedStartHeld.Error() {
			if !errors.Is(err, sandbox.ErrManagedSavedStartHeld) {
				t.Errorf("%s: %v", c.name, err)
			}
			ss := sandbox.NewWithAPI(nil, c.cfg, "svc")
			if _, e := c.h.createSandboxResource(context.Background(), &Project{ID: "p"}, nil, ss, spec); !errors.Is(e, sandbox.ErrManagedSavedStartHeld) {
				t.Errorf("%s create: %v", c.name, e)
			}
			continue
		}
		var he *httpx.HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusBadRequest || he.Msg != c.wantMsg {
			t.Errorf("%s: got %v, want 400 %q", c.name, err, c.wantMsg)
		}
		// The shared create refuses before it touches the cluster (a nil API would panic).
		ss := sandbox.NewWithAPI(nil, c.cfg, "svc")
		if _, err := c.h.createSandboxResource(context.Background(), &Project{ID: "p"}, nil, ss, spec); !errors.As(err, &he) || he.Msg != c.wantMsg {
			t.Errorf("%s: createSandboxResource got %v, want %q", c.name, err, c.wantMsg)
		}
	}
}
