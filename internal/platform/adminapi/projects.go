package adminapi

// projects.go serves /admin-api/v1/projects. Provisioning (the per-provider project bootstrap)
// returns 501 when not wired; a create WITHOUT provision persists as DISABLED with no members.

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

type apiProjectMember struct {
	Sub string `json:"sub,omitempty"`
}

type apiProvisionedService struct {
	ServiceID string `json:"service_id,omitempty"`
	Provider  string `json:"provider,omitempty"` // PROXMOX | CEPH_S3 | KAMAJI | DBAAS
	Region    string `json:"region,omitempty"`
	Pool      string `json:"pool,omitempty"` // proxmox bindings only
}

type apiProject struct {
	ID                  string                  `json:"id,omitempty"`
	Name                string                  `json:"name,omitempty"`
	Status              string                  `json:"status,omitempty"`
	OrganizationID      string                  `json:"organization_id,omitempty"`
	BillingProfileID    string                  `json:"billing_profile_id,omitempty"`
	ProvisionedServices []apiProvisionedService `json:"provisioned_services"`
	Members             []apiProjectMember      `json:"members"`
}

func (h *Handler) routeProjects(r chi.Router) {
	r.Get("/projects", h.projectsList)
	r.Post("/projects", h.projectCreate)
	r.Get("/projects/{id}", h.projectGet)
	r.Post("/projects/{id}/provision", h.projectProvision)
}

func (h *Handler) mapProject(r *http.Request, p *project.Project) apiProject {
	members := make([]apiProjectMember, 0, len(p.Memberships))
	for _, m := range p.Memberships {
		members = append(members, apiProjectMember{Sub: m.Sub})
	}
	// billing_profile_id = the EFFECTIVE profile (the project's own, else the owning org's).
	bpID := p.BillingProfileID
	if bpID == "" {
		if o, err := h.orgs.FindByID(r.Context(), p.OrganizationID); err == nil && o != nil {
			bpID = o.BillingProfileID
		}
	}
	return apiProject{
		ID: p.ID, Name: p.Name, Status: p.Status, OrganizationID: p.OrganizationID,
		BillingProfileID: bpID, ProvisionedServices: provisionedServices(p), Members: members,
	}
}

// provisionedServices maps the project's service bindings to the wire shape.
func provisionedServices(p *project.Project) []apiProvisionedService {
	services := make([]apiProvisionedService, 0, len(p.Services))
	for _, sid := range p.ServiceIDs() {
		services = append(services, apiProvisionedService{
			ServiceID: sid, Provider: providerWire(p.ServiceProvider(sid)),
			Region: p.ServiceRegion(sid), Pool: p.ServicePool(sid),
		})
	}
	return services
}

func (h *Handler) projectsList(w http.ResponseWriter, r *http.Request) {
	req, ok := listParams(w, r)
	if !ok {
		return
	}
	f := pgdoc.M{}
	if v := r.URL.Query().Get("organization_id"); v != "" {
		f["organizationId"] = v
	}
	if v := r.URL.Query().Get("billing_profile_id"); v != "" {
		f["billingProfileId"] = v
	}
	if v := r.URL.Query().Get("member_sub"); v != "" {
		f["memberships"] = pgdoc.M{"$contains": pgdoc.M{"sub": v}}
	}
	if v := r.URL.Query().Get("status"); v != "" {
		f["status"] = v
	}
	projects, err := findPage[project.Project](r.Context(), h.db.C("project"), f, req)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	page, next := pageOut(req, projects, func(p project.Project) string { return p.ID })
	out := make([]apiProject, 0, len(page))
	for i := range page {
		out = append(out, h.mapProject(r, &page[i]))
	}
	writeList(w, out, next)
}

func (h *Handler) projectGet(w http.ResponseWriter, r *http.Request) {
	var p project.Project
	if found, err := h.db.C("project").Get(r.Context(), chi.URLParam(r, "id"), &p); err != nil || !found {
		apiNotFound(w)
		return
	}
	writeEntity(w, h.mapProject(r, &p))
}

func (h *Handler) projectCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name             string `json:"name"`
		OrganizationID   string `json:"organization_id"`
		BillingProfileID string `json:"billing_profile_id"`
		Provision        []any  `json:"provision"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Provision) > 0 && h.bootstrapProject == nil {
		seam(w, "project provisioning not implemented")
		return
	}
	p := project.Project{
		ID: newID(), Name: req.Name, OrganizationID: req.OrganizationID,
		Status: project.StatusDisabled, CustomInfo: map[string]any{},
		Memberships: []project.Membership{}, Services: []any{},
	}
	if req.BillingProfileID != "" {
		ok, err := h.db.C("billingProfile").Exists(r.Context(), pgdoc.M{"_id": req.BillingProfileID})
		if err != nil || !ok {
			apiNotFoundMsg(w, "Billing profile not found")
			return
		}
		p.BillingProfileID = req.BillingProfileID
	}
	now := nowUTC()
	p.CreatedAt = &now
	if _, err := h.db.C("project").InsertOne(r.Context(), &p); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.logAdmin(r, audit.ActionCreate, "PROJECT", p.ID, p.Name)
	// A create with a provision block provisions right after the save.
	if len(req.Provision) > 0 {
		if err := h.bootstrapProject(r.Context(), p.ID); err != nil {
			badRequest(w, err.Error()) // provisioning errors map to 400
			return
		}
		var fresh project.Project
		if found, err := h.db.C("project").Get(r.Context(), p.ID, &fresh); err == nil && found {
			p = fresh
		}
	}
	writeEntity(w, h.mapProject(r, &p))
}

// projectProvision looks up the project (404 if absent), then bootstraps it onto the platform
// providers + ENABLED (no per-spec provider selection; no per-user cloud grants).
func (h *Handler) projectProvision(w http.ResponseWriter, r *http.Request) {
	var p project.Project
	if found, err := h.db.C("project").Get(r.Context(), chi.URLParam(r, "id"), &p); err != nil || !found {
		apiNotFound(w)
		return
	}
	if h.bootstrapProject == nil {
		seam(w, "project provisioning not implemented")
		return
	}
	if err := h.bootstrapProject(r.Context(), p.ID); err != nil {
		badRequest(w, err.Error())
		return
	}
	if found, err := h.db.C("project").Get(r.Context(), p.ID, &p); err != nil || !found {
		apiNotFound(w)
		return
	}
	h.logAdmin(r, audit.ActionUpdate, "PROJECT", p.ID, p.Name)
	writeEntity(w, h.mapProject(r, &p))
}
