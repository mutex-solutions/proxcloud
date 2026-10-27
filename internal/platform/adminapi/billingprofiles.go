package adminapi

// billingprofiles.go serves /admin-api/v1/billing_profiles. activate/suspend/resume drive the
// ActivationService (KYC/promotional-credit/cloud suspend orchestration) — 501 when it is not
// wired, as with the /api/v1/admin billing-profile status transitions.

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
)

type apiBPMember struct {
	Sub string `json:"sub,omitempty"`
}

type apiBillingProfile struct {
	ID             string        `json:"id,omitempty"`
	OrganizationID string        `json:"organization_id,omitempty"`
	Status         string        `json:"status,omitempty"`
	Members        []apiBPMember `json:"members"`
	FirstName      string        `json:"first_name,omitempty"`
	LastName       string        `json:"last_name,omitempty"`
	Email          string        `json:"email,omitempty"`
	Company        bool          `json:"company"`
	CompanyName    string        `json:"company_name,omitempty"`
	TaxNumber      string        `json:"tax_number,omitempty"`
	Address        string        `json:"address,omitempty"`
	City           string        `json:"city,omitempty"`
	ZipCode        string        `json:"zip_code,omitempty"`
	Region         string        `json:"region,omitempty"`
	Country        string        `json:"country,omitempty"`
	Phone          string        `json:"phone,omitempty"`
	Currency       string        `json:"currency,omitempty"`
	CreatedAt      *time.Time    `json:"created_at,omitempty"`
	UpdatedAt      *time.Time    `json:"updated_at,omitempty"`
}

// bpRequest is the billing-profile request body (snake_case).
type bpRequest struct {
	OrganizationID string        `json:"organization_id"`
	Members        []apiBPMember `json:"members"`
	FirstName      string        `json:"first_name"`
	LastName       string        `json:"last_name"`
	Email          string        `json:"email"`
	Company        bool          `json:"company"`
	CompanyName    string        `json:"company_name"`
	TaxNumber      string        `json:"tax_number"`
	Address        string        `json:"address"`
	City           string        `json:"city"`
	ZipCode        string        `json:"zip_code"`
	Region         string        `json:"region"`
	Country        string        `json:"country"`
	Phone          string        `json:"phone"`
	Currency       string        `json:"currency"`
}

func mapBP(bp *billing.BillingProfile) apiBillingProfile {
	return apiBillingProfile{
		ID: bp.ID, OrganizationID: bp.OrganizationID, Status: bp.Status,
		Members:   []apiBPMember{{Sub: bp.Sub}}, // single member: the profile's sub
		FirstName: bp.FirstName, LastName: bp.LastName, Email: bp.Email,
		Company: bp.Company, CompanyName: bp.CompanyName, TaxNumber: bp.VatCode,
		Address: bp.Address, City: bp.City, ZipCode: bp.ZipCode,
		Region: bp.County, Country: bp.Country, Phone: bp.Phone, Currency: bp.Currency,
		CreatedAt: bp.CreatedAt, UpdatedAt: bp.UpdatedAt,
	}
}

func (h *Handler) routeBillingProfiles(r chi.Router) {
	r.Get("/billing_profiles", h.bpsList)
	r.Post("/billing_profiles", h.bpCreate)
	r.Get("/billing_profiles/{id}", h.bpGet)
	r.Put("/billing_profiles/{id}", h.bpUpdate)
	r.Post("/billing_profiles/{id}/activate", h.bpActivate)
	r.Post("/billing_profiles/{id}/suspend", h.bpSuspend)
	r.Post("/billing_profiles/{id}/resume", h.bpResume)
}

func (h *Handler) bpsList(w http.ResponseWriter, r *http.Request) {
	req, ok := listParams(w, r)
	if !ok {
		return
	}
	f := pgdoc.M{}
	if v := r.URL.Query().Get("organization_id"); v != "" {
		f["organizationId"] = v
	}
	if v := r.URL.Query().Get("email"); v != "" {
		f["email"] = v
	}
	if v := r.URL.Query().Get("member_sub"); v != "" {
		f["sub"] = v
	}
	bps, err := findPage[billing.BillingProfile](r.Context(), h.db.C("billingProfile"), f, req)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	page, next := pageOut(req, bps, func(b billing.BillingProfile) string { return b.ID })
	out := make([]apiBillingProfile, 0, len(page))
	for i := range page {
		out = append(out, mapBP(&page[i]))
	}
	writeList(w, out, next)
}

func (h *Handler) bpGet(w http.ResponseWriter, r *http.Request) {
	var bp billing.BillingProfile
	if found, err := h.db.C("billingProfile").Get(r.Context(), chi.URLParam(r, "id"), &bp); err != nil || !found {
		apiNotFound(w)
		return
	}
	writeEntity(w, mapBP(&bp))
}

// mapBPRequest maps the request body: exactly ONE member whose sub must resolve.
func (h *Handler) mapBPRequest(w http.ResponseWriter, r *http.Request, req *bpRequest) (*billing.BillingProfile, bool) {
	if len(req.Members) != 1 {
		badRequest(w, "Only 1 billing profile member is supported.")
		return nil, false
	}
	u, _ := h.users.FindBySub(r.Context(), req.Members[0].Sub)
	if u == nil {
		apiNotFoundMsg(w, "User not found")
		return nil, false
	}
	return &billing.BillingProfile{
		Sub: u.Sub, OrganizationID: req.OrganizationID,
		FirstName: req.FirstName, LastName: req.LastName, Email: req.Email,
		Company: req.Company, CompanyName: req.CompanyName, VatCode: req.TaxNumber,
		Address: req.Address, City: req.City, ZipCode: req.ZipCode,
		County: req.Region, Country: req.Country, Phone: req.Phone, Currency: req.Currency,
		Contacts: []billing.Contact{{Email: req.Email, Name: req.FirstName + " " + req.LastName}},
	}, true
}

func (h *Handler) bpCreate(w http.ResponseWriter, r *http.Request) {
	var req bpRequest
	if !decodeBody(w, r, &req) {
		return
	}
	// resolveOrCreateOrganization: an explicit organization_id must exist; otherwise a
	// "<first> <last>" organization is created for the profile.
	var o *org.Organization
	if req.OrganizationID != "" {
		var err error
		o, err = h.orgs.FindByID(r.Context(), req.OrganizationID)
		if err != nil || o == nil {
			apiNotFoundMsg(w, "Organization not found")
			return
		}
	} else {
		var err error
		o, err = h.orgs.Insert(r.Context(), &org.Organization{Name: req.FirstName + " " + req.LastName, CustomInfo: map[string]any{}})
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		h.logAdmin(r, audit.ActionCreate, "ORGANIZATION", o.ID, o.Name)
	}
	req.OrganizationID = o.ID
	bp, ok := h.mapBPRequest(w, r, &req)
	if !ok {
		return
	}
	now := nowUTC()
	bp.ID, bp.Status, bp.CreatedAt, bp.UpdatedAt = newID(), billing.StatusNew, &now, &now
	if _, err := h.db.C("billingProfile").InsertOne(r.Context(), bp); err != nil {
		badRequest(w, err.Error())
		return
	}
	o.BillingProfileID = bp.ID
	_ = h.orgs.Save(r.Context(), o)
	h.logAdmin(r, audit.ActionCreate, "BILLING_PROFILE", bp.ID, bp.Email)
	writeEntity(w, mapBP(bp))
}

func (h *Handler) bpUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var existing billing.BillingProfile
	if found, err := h.db.C("billingProfile").Get(r.Context(), id, &existing); err != nil || !found {
		apiNotFound(w)
		return
	}
	var req bpRequest
	if !decodeBody(w, r, &req) {
		return
	}
	bp, ok := h.mapBPRequest(w, r, &req)
	if !ok {
		return
	}
	// The mapped request REPLACES the profile, keeping id/status/createdAt.
	now := nowUTC()
	bp.ID, bp.Status, bp.CreatedAt, bp.UpdatedAt = existing.ID, existing.Status, existing.CreatedAt, &now
	if _, err := h.db.C("billingProfile").Replace(r.Context(), existing.ID, bp); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.logAdmin(r, audit.ActionUpdate, "BILLING_PROFILE", bp.ID, bp.Email)
	writeEntity(w, mapBP(bp))
}

// bpLoad resolves the profile for the activate/suspend/resume transitions (404 envelope).
func (h *Handler) bpLoad(w http.ResponseWriter, r *http.Request) (*billing.BillingProfile, bool) {
	var bp billing.BillingProfile
	if found, err := h.db.C("billingProfile").Get(r.Context(), chi.URLParam(r, "id"), &bp); err != nil || !found {
		apiNotFound(w)
		return nil, false
	}
	if h.activation == nil {
		seam(w, "billing-profile activation/suspension orchestration not implemented")
		return nil, false
	}
	return &bp, true
}

// bpActivate marks the activation constraint completed with source ADMIN_API (a non-NEW
// profile is a no-op — it has no effect for other statuses), then returns the profile.
func (h *Handler) bpActivate(w http.ResponseWriter, r *http.Request) {
	bp, ok := h.bpLoad(w, r)
	if !ok {
		return
	}
	if err := h.activation.Activate(r.Context(), bp, billing.SourceAdminAPI); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.logAdmin(r, "ACTIVATE", "BILLING_PROFILE", bp.ID, bp.Email)
	writeEntity(w, mapBP(bp))
}

// bpSuspend suspends the profile with source API.
func (h *Handler) bpSuspend(w http.ResponseWriter, r *http.Request) {
	bp, ok := h.bpLoad(w, r)
	if !ok {
		return
	}
	if err := h.activation.Suspend(r.Context(), bp, "API"); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.logAdmin(r, "SUSPEND", "BILLING_PROFILE", bp.ID, bp.Email)
	writeEntity(w, mapBP(bp))
}

// bpResume resumes the profile with source API.
func (h *Handler) bpResume(w http.ResponseWriter, r *http.Request) {
	bp, ok := h.bpLoad(w, r)
	if !ok {
		return
	}
	if err := h.activation.Unsuspend(r.Context(), bp, "API"); err != nil {
		badRequest(w, err.Error())
		return
	}
	h.logAdmin(r, "UNSUSPEND", "BILLING_PROFILE", bp.ID, bp.Email)
	writeEntity(w, mapBP(bp))
}
