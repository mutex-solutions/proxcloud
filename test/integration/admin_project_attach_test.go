//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// TestAdminAttachServiceIsPostAndIdempotent (#214): the operator's attach is a POST through the real bootstrap; a GET is
// refused with 405 and leaves the project as it was; a second POST attaches nothing more, and a provider the project
// already carries is not provisioned again (here a Proxmox binding with no Proxmox client wired: any provider call would
// fail the request).
func TestAdminAttachServiceIsPostAndIdempotent(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	esSvc := externalservice.NewService(externalservice.NewRepo(db), enc)
	projects := project.NewService(project.NewRepo(db), nil, nil, nil, nil)
	projectH := project.NewHandler(projects, nil, nil, nil, nil, nil, cloud.NewRepo(db), esSvc, nil, nil, "r1")
	var bootstraps int
	const iss, cid = "test-iss", "test-cid"
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, esSvc, "r1", iss, cid)
	h.SetProjectCloudOps(&admin.ProjectCloudOps{Bootstrap: func(ctx context.Context, projectID, esID string) error {
		bootstraps++
		p, err := projects.GetProjectByID(ctx, projectID)
		if err != nil {
			return err
		}
		es, err := esSvc.Get(ctx, esID)
		if err != nil {
			return err
		}
		return projectH.BootstrapOnto(ctx, p, es)
	}})
	router := chi.NewRouter()
	router.MethodNotAllowed(httpx.MethodNotAllowedHandler())
	h.Routes(router)

	for _, es := range []pgdoc.M{
		{"_id": "pve", "name": "pve", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": pgdoc.M{"provider": "proxmox"}},
		{"_id": "dns", "name": "dns", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": pgdoc.M{"provider": "powerdns", "apiUrl": "http://127.0.0.1:1", "region": "r1"}},
	} {
		mustInsert(t, db, "externalService", es)
	}
	mustInsert(t, db, "project", pgdoc.M{"_id": "p1", "name": "p", "status": project.StatusEnabled, "organizationId": "o1",
		"services": []any{pgdoc.M{"serviceId": "pve", "provider": "proxmox", "region": "r1", "pool": "pc-p1"}}})

	do := func(method, esID string) (int, string) {
		req := httptest.NewRequest(method, fmt.Sprintf("/admin/project/p1/external-service/%s", esID), nil)
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	services := func() []string {
		p, err := projects.GetProjectByID(ctx, "p1")
		if err != nil {
			t.Fatal(err)
		}
		return p.ServiceIDs()
	}

	if code, body := do(http.MethodGet, "dns"); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d %s, want 405", code, body)
	}
	if got := services(); len(got) != 1 || bootstraps != 0 {
		t.Fatalf("a GET attached: services %v, bootstraps %d", got, bootstraps)
	}
	for i := 1; i <= 2; i++ {
		if code, body := do(http.MethodPost, "dns"); code != http.StatusOK {
			t.Fatalf("POST #%d: %d %s", i, code, body)
		}
		if got := services(); len(got) != 2 || got[1] != "dns" {
			t.Fatalf("after POST #%d: services %v, want [pve dns] once", i, got)
		}
	}
	// Re-attaching the Proxmox provider the project already carries: no PVE client is wired, so reaching the provider
	// would fail; the attach answers 200 with the bindings unchanged.
	if code, body := do(http.MethodPost, "pve"); code != http.StatusOK {
		t.Fatalf("re-attach: %d %s", code, body)
	}
	if got := services(); len(got) != 2 {
		t.Fatalf("re-attach changed the bindings: %v", got)
	}
}
