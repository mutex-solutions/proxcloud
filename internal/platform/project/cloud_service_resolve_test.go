package project

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// resolveServiceID with an explicit service (#215). The handler has no externalservice.Service: an explicit choice is
// decided from the project's own bindings, before any provider lookup (a lookup would panic here). The default taken
// when no service is named needs the provider kinds and is covered by test/integration TestCloudServiceSelection.
func TestResolveServiceIDExplicit(t *testing.T) {
	p := &Project{Services: []any{
		map[string]any{"serviceId": "ceph", "provider": "ceph-s3"},
		map[string]any{"serviceId": "pve", "provider": "proxmox"},
	}}
	h := &Handler{}
	cases := []struct {
		name, header, query string
		want                string // "" = refused
		msg                 string
	}{
		{"attached header", "ceph", "", "ceph", ""},
		{"attached query", "", "pve", "pve", ""},
		{"header wins over query when both are attached", "ceph", "pve", "ceph", ""},
		{"unattached header", "s3-new", "", "", "Project is not attached to service s3-new"},
		{"unattached query", "", "s3-new", "", "Project is not attached to service s3-new"},
		{"unattached header with an attached query", "s3-new", "pve", "", "Project is not attached to service s3-new"},
		{"attached header with an unattached query", "pve", "s3-new", "", "Project is not attached to service s3-new"},
		{"an id not to echo back", "<script>", "", "", "Project is not attached to the requested service"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target := "/project/p1/cloud"
			if c.query != "" {
				target += "?serviceId=" + c.query
			}
			r := httptest.NewRequest(http.MethodPost, target, nil)
			if c.header != "" {
				r.Header.Set("x-service-id", c.header)
			}
			got, err := h.resolveServiceID(r, p)
			if c.want != "" {
				if err != nil || got != c.want {
					t.Fatalf("got %q, %v; want %q", got, err, c.want)
				}
				return
			}
			var he *httpx.HTTPError
			if !errors.As(err, &he) || he.Status != http.StatusBadRequest || he.Msg != c.msg || got != "" {
				t.Fatalf("got %q, %v; want 400 %q", got, err, c.msg)
			}
		})
	}
}

// A project on no provider has nothing to default to and nothing to look up: ("", nil), the callers' no-service guard.
// Naming a service then is still a refusal, not that guard.
func TestResolveServiceIDNoServices(t *testing.T) {
	h := &Handler{}
	p := &Project{Services: []any{}}
	if got, err := h.resolveServiceID(httptest.NewRequest(http.MethodGet, "/", nil), p); got != "" || err != nil {
		t.Fatalf("no services: %q, %v", got, err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("x-service-id", "pve")
	if _, err := h.resolveServiceID(r, p); err == nil || !strings.Contains(err.Error(), "not attached") {
		t.Fatalf("named service on a project with none: %v", err)
	}
}
