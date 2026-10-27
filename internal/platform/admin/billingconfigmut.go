package admin

import (
	"encoding/json"
	"maps"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// billingconfigmut.go serves the MUTATIONS of the billing-configuration surface
// (/api/v1/admin/billing/configuration) — create / update / delete / updateInvoiceGateway.
// The plain reads (GET list / GET {id} / GET current / GET countries) are already registered in
// handler.go and are out of scope here. Follows custommenu.go: id-aware CRUD via the crud.go
// helpers, exact perms / error strings / response envelopes, `_id`→`id` shaping on the way out.
//
// All mutations gate on ADMIN_BILLING_CONFIG_UPDATE (admin:billing_config:update); the reads use
// ADMIN_BILLING_CONFIG_READ. create/update/delete also write audit events — deferred this pass
// (// TODO(audit)); the state + response are faithful, which is what the admin UI exercises.

const billingConfigUpdatePerm = "admin:billing_config:update"

const billingConfigCollection = "billingConfiguration"

// Exact error messages (resolved translation values, incl. trailing spaces):
//
//	billingConfigurationNotFound → "Billing configuration not found " (badRequest → 400)
//	billingNotConfigured         → "Billing is not configured "       (badRequest → 400)
//	baseCurrencyRequired         → "Base Currency is required "        (required-field assert → 400)
//	countryNotValid              → "Country is not valid "             (validity assert → 400)
const (
	msgBillingConfigNotFound = "Billing configuration not found "
	msgBillingNotConfigured  = "Billing is not configured "
	msgBaseCurrencyRequired  = "Base Currency is required "
	msgCountryNotValid       = "Country is not valid "
)

// routeBillingConfigMut registers ONLY the billing-configuration admin mutation routes.
// (The reads at /billing/configuration[/{id}|/current|/countries] are already in handler.go.)
// The PUT /invoice-gateway static route precedes the PUT /{id} param route — chi matches the
// static segment first, so there is no conflict.
func (h *Handler) routeBillingConfigMut(r chi.Router) {
	r.Post("/billing/configuration", h.billingConfigCreate)
	r.Put("/billing/configuration/invoice-gateway", h.billingConfigUpdateInvoiceGateway)
	r.Put("/billing/configuration/{id}", h.billingConfigUpdate)
	r.Delete("/billing/configuration/{id}", h.billingConfigDelete)
}

// billingConfigReq is the mutable fields of the BillingConfiguration domain (the request body).
// Nested objects/config blocks (address, company, settings, provisioning/auto-activation/suspension/
// savings notification) are kept as raw pgdoc.M passthrough — deserialized + re-saved
// verbatim. promotionCodesEnabled is a nullable boolean (*bool: omitted when absent).
type billingConfigReq struct {
	Name                              string  `json:"name"`
	Address                           pgdoc.M `json:"address"`
	Company                           pgdoc.M `json:"company"`
	BaseCurrency                      string  `json:"baseCurrency"`
	MailGatewayID                     string  `json:"mailGatewayId"`
	InvoiceGatewayID                  string  `json:"invoiceGatewayId"`
	Settings                          pgdoc.M `json:"settings"`
	DefaultConfiguration              bool    `json:"defaultConfiguration"`
	PromotionCodesEnabled             *bool   `json:"promotionCodesEnabled"`
	ProvisioningSettings              pgdoc.M `json:"provisioningSettings"`
	AutoActivationFlow                pgdoc.M `json:"autoActivationFlow"`
	SuspensionConfiguration           pgdoc.M `json:"suspensionConfiguration"`
	SavingsContractNotificationConfig pgdoc.M `json:"savingsContractNotificationConfig"`
}

// doc builds the stored JSON for a BillingConfiguration. defaultConfiguration is a primitive bool
// (always stored). Optional strings are omitted when blank and nested objects are omitted when nil
// so the JSON of the saved doc drops them (a null field is dropped, not emitted).
func (req billingConfigReq) doc() pgdoc.M {
	d := pgdoc.M{"defaultConfiguration": req.DefaultConfiguration}
	if req.Name != "" {
		d["name"] = req.Name
	}
	if req.Address != nil {
		d["address"] = req.Address
	}
	if req.Company != nil {
		d["company"] = req.Company
	}
	if req.BaseCurrency != "" {
		d["baseCurrency"] = req.BaseCurrency
	}
	if req.MailGatewayID != "" {
		d["mailGatewayId"] = req.MailGatewayID
	}
	if req.InvoiceGatewayID != "" {
		d["invoiceGatewayId"] = req.InvoiceGatewayID
	}
	if req.Settings != nil {
		d["settings"] = req.Settings
	}
	if req.PromotionCodesEnabled != nil {
		d["promotionCodesEnabled"] = *req.PromotionCodesEnabled
	}
	if req.ProvisioningSettings != nil {
		d["provisioningSettings"] = req.ProvisioningSettings
	}
	if req.AutoActivationFlow != nil {
		d["autoActivationFlow"] = req.AutoActivationFlow
	}
	if req.SuspensionConfiguration != nil {
		d["suspensionConfiguration"] = req.SuspensionConfiguration
	}
	if req.SavingsContractNotificationConfig != nil {
		d["savingsContractNotificationConfig"] = req.SavingsContractNotificationConfig
	}
	return d
}

// mutableKeys are the 13 BillingConfiguration fields update() overwrites from the request body.
// On update we drop each from the existing doc first so an omitted field becomes null (matching
// setting it to the request's null), then re-apply whatever the request supplied.
var billingConfigMutableKeys = []string{
	"name", "address", "company", "baseCurrency", "mailGatewayId", "invoiceGatewayId",
	"settings", "defaultConfiguration", "promotionCodesEnabled", "provisioningSettings",
	"autoActivationFlow", "suspensionConfiguration", "savingsContractNotificationConfig",
}

// billingConfigCreate handles createConfiguration (POST): save the body → single(saved). No
// validation (create() saves directly). Gated ADMIN_BILLING_CONFIG_UPDATE.
func (h *Handler) billingConfigCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingConfigUpdatePerm) {
		return
	}
	var req billingConfigReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	saved, err := h.repo.InsertDoc(r.Context(), billingConfigCollection, req.doc())
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): auditAdmin(result, CREATE, PLATFORM)
	httpx.OK(w, shapeDoc(saved))
}

// billingConfigUpdate handles updateConfiguration (PUT /{id}): validate → getById-or-400 → overwrite
// the 13 mutable fields → save → single. Gated ADMIN_BILLING_CONFIG_UPDATE.
func (h *Handler) billingConfigUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingConfigUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req billingConfigReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	// configurationValidator: baseCurrency required; if address.country set, it must be a valid
	// country. Both are asserts → 400 (code 400). Runs BEFORE the
	// getById lookup.
	if err := h.validateBillingConfig(req); err != nil {
		httpx.WriteError(w, err)
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), billingConfigCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.BadRequest(msgBillingConfigNotFound))
		return
	}
	before := maps.Clone(existing)
	d := req.doc()
	for _, k := range billingConfigMutableKeys {
		delete(existing, k) // overwrite — drop the old value first so an omitted field becomes null
	}
	for k, v := range d {
		existing[k] = v
	}
	if err := h.repo.ReplaceDoc(r.Context(), billingConfigCollection, id, existing); httpx.WriteError(w, err) {
		return
	}
	// UPDATE audit: field-level diff (middleware computes diffSnapshots(before, after)).
	after, _ := h.repo.FindDoc(r.Context(), billingConfigCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(existing))
}

// billingConfigDelete handles deleteConfiguration (DELETE /{id}): getById-or-400 → delete →
// success("Successful operation"). Gated ADMIN_BILLING_CONFIG_UPDATE.
func (h *Handler) billingConfigDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingConfigUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), billingConfigCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.BadRequest(msgBillingConfigNotFound))
		return
	}
	if _, err := h.repo.DeleteDoc(r.Context(), billingConfigCollection, id); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): auditAdmin(existing, DELETE, PLATFORM)
	httpx.OK(w, "Successful operation")
}

// updateInvoiceGatewayReq is the updateInvoiceGateway request body (single field).
type updateInvoiceGatewayReq struct {
	InvoiceGatewayID string `json:"invoiceGatewayId"`
}

// billingConfigUpdateInvoiceGateway handles updateInvoiceGateway (PUT /invoice-gateway):
// getBillingConfiguration (the first config; 400 "Billing is not configured " when none) → set
// invoiceGatewayId → save → single. Gated ADMIN_BILLING_CONFIG_UPDATE.
func (h *Handler) billingConfigUpdateInvoiceGateway(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, billingConfigUpdatePerm) {
		return
	}
	var req updateInvoiceGatewayReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	// getBillingConfiguration = findAll().stream().findFirst() — the first config doc, NOT keyed by
	// defaultConfiguration. (getById/current differ; this one is the unconditional first.)
	existing, err := h.repo.FindOneBy(r.Context(), billingConfigCollection, pgdoc.M{})
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.BadRequest(msgBillingNotConfigured))
		return
	}
	id, ok := docID(existing)
	if !ok {
		httpx.WriteError(w, httpx.BadRequest(msgBillingNotConfigured))
		return
	}
	// setInvoiceGatewayId then save: a blank id sets it to null (omit from the doc → dropped).
	delete(existing, "invoiceGatewayId")
	if req.InvoiceGatewayID != "" {
		existing["invoiceGatewayId"] = req.InvoiceGatewayID
	}
	if err := h.repo.ReplaceDoc(r.Context(), billingConfigCollection, id, existing); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): CONFIGURE audit event
	httpx.OK(w, shapeDoc(existing))
}

// validateBillingConfig runs the configuration validator (the update path):
//   - baseCurrency must be present → 400 "Base Currency is required "
//   - if address.country is non-blank, it must be a valid country → 400 "Country is not valid "
//
// Both surface as HTTP 400 (code 400).
func (h *Handler) validateBillingConfig(req billingConfigReq) *httpx.HTTPError {
	if req.BaseCurrency == "" {
		return httpx.BadRequest(msgBaseCurrencyRequired)
	}
	if country := addressCountry(req.Address); country != "" {
		if !countryExists(country) {
			return httpx.BadRequest(msgCountryNotValid)
		}
	}
	return nil
}

// addressCountry reads a non-blank address.country from the raw address doc (case-insensitive
// match is done by countryExists). Returns "" when address or country is absent/blank.
func addressCountry(address pgdoc.M) string {
	if address == nil {
		return ""
	}
	c, _ := address["country"].(string)
	return c
}

// countryExists reports whether an alpha2 code is present in the country catalog
// (case-insensitive).
func countryExists(country string) bool {
	for _, c := range billing.Countries() {
		if equalFold(c.Alpha2, country) {
			return true
		}
	}
	return false
}

// docID extracts the string id from a raw doc's _id (a plain string; a legacy Hex() type is
// rendered as its hex), for ReplaceDoc.
func docID(doc pgdoc.M) (string, bool) {
	v, ok := doc["_id"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// equalFold is a tiny case-insensitive ASCII compare (avoids importing strings just for one call;
// country codes are ASCII alpha2).
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
