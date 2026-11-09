package admin

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// thirdpartyintegration.go implements the MUTATIONS (+ the external-integration reads) of
// the integrations surface (/api/v1/admin/integrations). The plain GET reads
// (list / by-id / by-type / by-category / stats) are ALREADY registered in handler.go and are
// out of scope here (their faithful-DTO upgrade is a separate pass — noted in 'deferred').
//
// This file adds, following the gold-standard custommenu.go (id-aware CRUD via crud.go,
// exact perms / error strings / response envelopes, `_id`→`id` shaping on the way out):
//
//	POST   /integrations            create   (ADMIN_INTEGRATION_MANAGE)
//	PUT    /integrations/{id}        update   (ADMIN_INTEGRATION_MANAGE)
//	DELETE /integrations/{id}        delete   (ADMIN_INTEGRATION_MANAGE)
//	POST   /integrations/healthcheck/{id}  healthCheck-by-id  (ADMIN_INTEGRATION_READ) — external integration
//	POST   /integrations/healthcheck       healthCheck-by-body(ADMIN_INTEGRATION_READ) — external integration
//
// create/update/delete should also write admin audit events —
// deferred this pass (// TODO(audit)); the state + response are faithful.

const (
	integrationManagePerm = "admin:integration:manage"
	integrationReadPerm   = "admin:integration:read"
	integrationCollection = "thirdPartyIntegration"
)

// routeThirdPartyIntegration registers ONLY the new ThirdPartyIntegration admin routes (the plain
// GET reads at /integrations, /integrations/{id}, /integrations/type|category|stats are already in
// handler.go and intentionally NOT re-registered here).
func (h *Handler) routeThirdPartyIntegration(r chi.Router) {
	r.Post("/integrations", h.integrationCreate)
	// Static siblings of the {id} routes — chi dispatches the literal segments before the param,
	// so these do not conflict with PUT/DELETE /integrations/{id} (different HTTP methods anyway).
	r.Post("/integrations/healthcheck", h.integrationHealthCheckBody)
	r.Post("/integrations/healthcheck/{id}", h.integrationHealthCheckByID)
	r.Put("/integrations/{id}", h.integrationUpdate)
	r.Delete("/integrations/{id}", h.integrationDelete)
}

// thirdPartyIntegrationReq holds the ThirdPartyIntegration's mutable request-body fields.
// config/secret are free-form; metadata is a free-form map.
type thirdPartyIntegrationReq struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	ThirdParty  string         `json:"thirdParty"`
	Config      any            `json:"config"`
	Secret      any            `json:"secret"`
	Metadata    map[string]any `json:"metadata"`
}

// integrationCreate creates an integration:
//   - name defaults to thirdParty when blank; description defaults to "<thirdParty> Integration".
//   - the secret is meant to be encrypted at rest — see the SECRET
//     note below; config/metadata pass through.
//   - store → reload (decrypting the secret) → map to the wire DTO, which ALWAYS nulls the secret.
//
// Response: the saved integration via httpx.OK(w, dto). Gated ADMIN_INTEGRATION_MANAGE.
func (h *Handler) integrationCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, integrationManagePerm) {
		return
	}
	var req thirdPartyIntegrationReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	name := req.Name
	if name == "" {
		name = req.ThirdParty
	}
	description := req.Description
	if description == "" {
		description = fmt.Sprintf("%s Integration", req.ThirdParty)
	}
	doc := integrationDoc(name, description, req.ThirdParty, req.Config, req.Secret, req.Metadata)
	// Encrypt the secret's textual leaves before it reaches the datastore (symmetric with the
	// consumers' decrypt: mail SMTPFromStore + billing GetGateway). Mirrors externalServiceCreate.
	if sec, ok := doc["secret"]; ok && h.esSvc != nil {
		doc["secret"] = h.esSvc.EncryptSecret(sec)
	}
	saved, err := h.repo.InsertDoc(r.Context(), integrationCollection, doc)
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when an integration is created.
	httpx.OK(w, integrationDto(saved))
}

// integrationUpdate updates an integration: load-or-404, overwrite
// name/description/thirdParty/config/metadata, conditionally re-encrypt the secret
// (isNeededToUpdateSecret: a non-null, non-empty secret whose every field is non-null → replace;
// otherwise keep the existing encrypted secret), store, reload → DTO (secret nulled). Note the
// update does NOT default name/description like create — it stores them verbatim (so a blank name
// is persisted blank). Gated ADMIN_INTEGRATION_MANAGE.
func (h *Handler) integrationUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, integrationManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req thirdPartyIntegrationReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), integrationCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("ThirdParty Integration with id %s not found", id)))
		return
	}
	before := maps.Clone(existing)
	// Overwrite the mutable fields (drop first so an omitted optional becomes null, nulls omitted).
	for _, k := range []string{"name", "description", "thirdParty", "config", "metadata"} {
		delete(existing, k)
	}
	for k, v := range integrationFields(req.Name, req.Description, req.ThirdParty, req.Config, req.Metadata) {
		existing[k] = v
	}
	// Secret: replace only when the request carries a non-empty secret all of whose fields are
	// non-null (isNeededToUpdateSecret); otherwise the existing secret is retained.
	if isNeededToUpdateSecret(req.Secret) {
		// Encrypt at rest (symmetric with the mail/billing consumers' decrypt). Mirrors create.
		if h.esSvc != nil {
			existing["secret"] = h.esSvc.EncryptSecret(req.Secret)
		} else {
			existing["secret"] = req.Secret
		}
	}
	if err := h.repo.ReplaceDoc(r.Context(), integrationCollection, id, existing); httpx.WriteError(w, err) {
		return
	}
	// UPDATE audit: field-level diff (middleware; the `secret` is skipped by the
	// sensitive-key filter so credentials never reach the audit log).
	after, _ := h.repo.FindDoc(r.Context(), integrationCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, integrationDto(existing))
}

// integrationDelete deletes an integration:
// load-or-404, then a deletion-eligibility gate (nothing blocks a greenfield integration), then
// delete by id → "Successful operation". Gated ADMIN_INTEGRATION_MANAGE.
//
// The eligibility check would block an integration still referenced by other resources (e.g. a
// gateway still used by a bill) — there is no such registry here, so deletion always proceeds
// (greenfield). When it DOES block, the delete returns 400 "ThirdParty Integration cannot be deleted because it is used
// by other resources".
func (h *Handler) integrationDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, integrationManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), integrationCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("ThirdParty Integration with id %s not found", id)))
		return
	}
	// TODO: deletion-eligibility gate → 400 "ThirdParty Integration cannot be deleted because
	// it is used by other resources" when the integration is still referenced (no such registry here).
	if _, err := h.repo.DeleteDoc(r.Context(), integrationCollection, id); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when an integration is deleted.
	httpx.OK(w, "Successful operation")
}

// integrationHealthCheckByID runs the health check for a stored integration by id: the check is a
// live vendor call — not wired (no vendor registry /
// no live calls here). Gated ADMIN_INTEGRATION_READ.
func (h *Handler) integrationHealthCheckByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, integrationReadPerm) {
		return
	}
	// The integration is first loaded (404 if absent) before the health check; the live
	// health check itself is the external part → not wired.
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), integrationCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("ThirdParty Integration with id %s not found", id)))
		return
	}
	// Card gateways: an authenticated no-op call with the stored (decrypted) credentials.
	if h.billing != nil {
		gw, err := h.billing.GetGateway(r.Context(), id)
		if httpx.WriteError(w, err) {
			return
		}
		if g := payment.NewGateway(gw); g != nil {
			if err := g.Ping(r.Context()); err != nil {
				httpx.WriteError(w, httpx.BadRequest("Health check failed: "+err.Error()))
				return
			}
			httpx.OK(w, "Successful operation")
			return
		}
	}
	httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented, "integration health check not implemented"))
}

// integrationHealthCheckBody runs the health check over an ad-hoc (unsaved) integration body:
// same live vendor call as the by-id variant. Gated ADMIN_INTEGRATION_READ.
func (h *Handler) integrationHealthCheckBody(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, integrationReadPerm) {
		return
	}
	var req thirdPartyIntegrationReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented, "integration health check not implemented"))
}

// integrationFields builds the non-secret mutable fields of a ThirdPartyIntegration doc. Optional
// blank strings are omitted (so an omitted field is dropped, not stored as "" — nulls omitted);
// config/metadata are stored only when non-nil. (name/description are passed already-defaulted by
// create; update passes them verbatim.)
func integrationFields(name, description, thirdParty string, config any, metadata map[string]any) pgdoc.M {
	d := pgdoc.M{}
	if name != "" {
		d["name"] = name
	}
	if description != "" {
		d["description"] = description
	}
	if thirdParty != "" {
		d["thirdParty"] = thirdParty
	}
	if config != nil {
		d["config"] = config
	}
	if metadata != nil {
		d["metadata"] = metadata
	}
	return d
}

// integrationDoc builds the full stored doc for a create, including the RAW secret. The caller
// (integrationCreate) encrypts the secret at rest via h.esSvc.EncryptSecret before persisting — this
// builder stays pure so the unit test can assert the plain shape. The response DTO always nulls the
// secret regardless. config/secret/metadata are stored only when non-nil.
func integrationDoc(name, description, thirdParty string, config, secret any, metadata map[string]any) pgdoc.M {
	d := integrationFields(name, description, thirdParty, config, metadata)
	if secret != nil {
		d["secret"] = secret // encrypted at rest by integrationCreate (esSvc.EncryptSecret).
	}
	return d
}

// integrationDto maps a stored doc to the wire shape: `_id`→`id`, drop
// `_class`, and ALWAYS null the secret. Under
// null-omitting serialization a null secret is omitted entirely (it does not appear as `"secret":null`).
func integrationDto(doc pgdoc.M) pgdoc.M {
	shapeDoc(doc)         // _id→id, drop _class
	delete(doc, "secret") // the DTO always nulls the secret → omitted
	return doc
}

// isNeededToUpdateSecret reports whether the incoming secret should replace the stored one: a secret is
// replaced only when it is non-null, non-empty, and every field value is non-null. A null secret,
// an empty object, or one with any null field → keep the existing secret.
func isNeededToUpdateSecret(secret any) bool {
	if secret == nil {
		return false
	}
	m, ok := secret.(map[string]any)
	if !ok {
		// A non-object, non-null secret (treated as an empty object → false).
		return false
	}
	if len(m) == 0 {
		return false
	}
	for _, v := range m {
		if v == nil {
			return false
		}
	}
	return true
}
