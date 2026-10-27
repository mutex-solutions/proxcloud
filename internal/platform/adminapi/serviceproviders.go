package adminapi

// serviceproviders.go serves /admin-api/v1/service_providers. Read-only; the DTO exposes ONLY
// identity_url + the customer domain id — never the externalService secret.

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

type apiOpenstackConfig struct {
	IdentityURL string `json:"identity_url,omitempty"`
	DomainID    string `json:"domain_id,omitempty"`
}

type apiCloudConfig struct {
	Provider  string             `json:"provider,omitempty"`
	Openstack apiOpenstackConfig `json:"openstack"`
}

type apiServiceProviderConfig struct {
	Cloud apiCloudConfig `json:"cloud"`
}

type apiServiceProvider struct {
	ID            string                   `json:"id,omitempty"`
	Name          string                   `json:"name,omitempty"`
	Type          string                   `json:"type,omitempty"`
	Configuration apiServiceProviderConfig `json:"configuration"`
}

func mapServiceProvider(es *externalservice.ExternalService) apiServiceProvider {
	typ := ""
	if es.Type == externalservice.TypeCloud {
		typ = "CLOUD" // non-CLOUD types map to null
	}
	domainID := ""
	if cust, ok := es.Config["customer"].(map[string]any); ok {
		domainID, _ = cust["domainId"].(string)
	}
	return apiServiceProvider{
		ID: es.ID, Name: es.Name, Type: typ,
		Configuration: apiServiceProviderConfig{Cloud: apiCloudConfig{
			Provider:  "OPENSTACK",
			Openstack: apiOpenstackConfig{IdentityURL: identityURLRaw(es), DomainID: domainID},
		}},
	}
}

// identityURLRaw returns config.identityUrl VERBATIM (this DTO does not normalize to /v3).
func identityURLRaw(es *externalservice.ExternalService) string {
	s, _ := es.Config["identityUrl"].(string)
	return s
}

func (h *Handler) routeServiceProviders(r chi.Router) {
	r.Get("/service_providers", h.serviceProvidersList)
	r.Get("/service_providers/{id}", h.serviceProviderGet)
}

func (h *Handler) serviceProvidersList(w http.ResponseWriter, r *http.Request) {
	req, ok := listParams(w, r)
	if !ok {
		return
	}
	all, err := h.es.List(r.Context())
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	// The externalService collection is tiny — apply the keyset cursor in memory.
	items := make([]apiServiceProvider, 0, len(all))
	started := req.Marker == ""
	for i := range all {
		if !started {
			if all[i].ID >= req.Marker {
				started = true
			} else {
				continue
			}
		}
		items = append(items, mapServiceProvider(&all[i]))
		if len(items) == req.Limit+1 {
			break
		}
	}
	page, next := pageOut(req, items, func(sp apiServiceProvider) string { return sp.ID })
	writeList(w, page, next)
}

func (h *Handler) serviceProviderGet(w http.ResponseWriter, r *http.Request) {
	es, err := h.es.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil || es == nil {
		apiNotFoundMsg(w, "Service provider not found")
		return
	}
	writeEntity(w, mapServiceProvider(es))
}
