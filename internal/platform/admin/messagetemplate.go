package admin

import (
	"encoding/json"
	"maps"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// messagetemplate.go serves the message-templates surface (/api/v1/admin/message-templates) —
// CRUD + the static placeholder map. Follows the custommenu.go reference:
// id-aware CRUD via the crud.go helpers, exact perms / error strings / response
// envelopes, `_id`→`id` shaping on the way out.
//
// Permissions:
//   - read endpoints   → ADMIN_MESSAGE_TEMPLATE_READ   ("admin:message_template:read")
//   - mutation endpts  → ADMIN_MESSAGE_TEMPLATE_MANAGE ("admin:message_template:manage")
//
// create/update/delete write audit events.
// Deferred this pass (// TODO(audit)); the state + response are faithful, which is what the admin
// UI exercises.
//
// NOTE: handler.go already registers the bare list `GET /message-templates` (h.listRaw — the
// admin-FE landing read). That list is a
// raw passthrough today; upgrading it to the single-DTO list is OUT OF SCOPE this pass
// (see 'deferred'). routeMessageTemplate registers only the not-yet-present routes:
// POST (create), GET/{id}, PUT/{id}, DELETE/{id}, GET /placeholders.

const messageTemplatePerm = "admin:message_template:read"
const messageTemplateManagePerm = "admin:message_template:manage"

const messageTemplateCollection = "messageTemplate"

// routeMessageTemplate registers the message-template admin mutation + by-id + placeholder routes.
// The bare list `GET /message-templates` is already registered in handler.go (h.listRaw).
func (h *Handler) routeMessageTemplate(r chi.Router) {
	r.Post("/message-templates", h.messageTemplateCreate)
	r.Get("/message-templates/placeholders", h.messageTemplatePlaceholders)
	r.Get("/message-templates/{id}", h.messageTemplateGet)
	r.Put("/message-templates/{id}", h.messageTemplateUpdate)
	r.Delete("/message-templates/{id}", h.messageTemplateDelete)
}

// messageTemplateReq holds the message-template request-body fields. Create persists the
// whole body (key/category/messageTitle/messageBody/disabled/systemTemplate/targets); update only
// overwrites messageTitle/messageBody/disabled. Optional blank
// strings are omitted so the JSON drops them (a null field is dropped, not "").
type messageTemplateReq struct {
	Key            string         `json:"key"`
	Category       string         `json:"category"`
	MessageTitle   string         `json:"messageTitle"`
	MessageBody    string         `json:"messageBody"`
	Disabled       bool           `json:"disabled"`
	SystemTemplate bool           `json:"systemTemplate"`
	Targets        map[string]any `json:"targets"`
}

// createDoc builds the stored JSON for the create handler: the full body. `disabled`/`systemTemplate` are
// primitives (always stored, default false). Optional strings/targets are omitted when blank
// (an absent field is dropped, not emitted as ""/null).
func (req messageTemplateReq) createDoc() pgdoc.M {
	d := pgdoc.M{"disabled": req.Disabled, "systemTemplate": req.SystemTemplate}
	if req.Key != "" {
		d["key"] = req.Key
	}
	if req.Category != "" {
		d["category"] = req.Category
	}
	if req.MessageTitle != "" {
		d["messageTitle"] = req.MessageTitle
	}
	if req.MessageBody != "" {
		d["messageBody"] = req.MessageBody
	}
	if req.Targets != nil {
		d["targets"] = req.Targets
	}
	return d
}

// messageTemplateCreate handles creation: reject with 400 when a template with the key already exists, otherwise persist and return the saved doc.
func (h *Handler) messageTemplateCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, messageTemplateManagePerm) {
		return
	}
	var req messageTemplateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	exists, err := h.repo.MessageTemplateExistsByKey(r.Context(), req.Key)
	if httpx.WriteError(w, err) {
		return
	}
	if exists {
		// 400 with the exact message (the trailing space is intentional).
		httpx.WriteError(w, httpx.BadRequest("Mail template with key already exists "))
		return
	}
	saved, err := h.repo.InsertDoc(r.Context(), messageTemplateCollection, req.createDoc())
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event for the template creation.
	httpx.OK(w, shapeDoc(saved))
}

// messageTemplateGet loads a template by id and returns it, or 404 when it doesn't exist.
func (h *Handler) messageTemplateGet(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, messageTemplatePerm) {
		return
	}
	doc, err := h.repo.FindDoc(r.Context(), messageTemplateCollection, chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		// 404 with the exact message (the trailing space is intentional).
		httpx.WriteError(w, httpx.NotFound("Mail template not found "))
		return
	}
	httpx.OK(w, shapeDoc(doc))
}

// messageTemplateUpdate loads a template by id (404 when absent), overwrites messageTitle/messageBody/
// disabled ONLY (key/category/systemTemplate/targets are left untouched), saves, and returns it.
func (h *Handler) messageTemplateUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, messageTemplateManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req messageTemplateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), messageTemplateCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Mail template not found "))
		return
	}
	before := maps.Clone(existing)
	// The update sets messageTitle/messageBody unconditionally (a null in the body nulls the field) and
	// disabled (primitive). Drop the old optional strings first so an omitted field becomes null
	// (dropped on the wire).
	delete(existing, "messageTitle")
	delete(existing, "messageBody")
	if req.MessageTitle != "" {
		existing["messageTitle"] = req.MessageTitle
	}
	if req.MessageBody != "" {
		existing["messageBody"] = req.MessageBody
	}
	existing["disabled"] = req.Disabled
	if err := h.repo.ReplaceDoc(r.Context(), messageTemplateCollection, id, existing); httpx.WriteError(w, err) {
		return
	}
	// UPDATE audit: record a field-level diff of the before/after snapshots.
	after, _ := h.repo.FindDoc(r.Context(), messageTemplateCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(existing))
}

// messageTemplateDelete loads a template by id (404 when absent), deletes it, and
// returns 202 Accepted (NO body).
func (h *Handler) messageTemplateDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, messageTemplateManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), messageTemplateCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Mail template not found "))
		return
	}
	if _, err := h.repo.DeleteDoc(r.Context(), messageTemplateCollection, id); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event for the template deletion.
	// 202 Accepted, no body.
	httpx.Accepted(w)
}

// messageTemplatePlaceholders returns the static per-category placeholder map
// (category → [placeholders]).
func (h *Handler) messageTemplatePlaceholders(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, messageTemplatePerm) {
		return
	}
	httpx.OK(w, messageTemplatePlaceholderMap())
}

// messageTemplatePlaceholder is a placeholder ({key, description}).
type messageTemplatePlaceholder struct {
	Key         string `json:"key"`
	Description string `json:"description"`
}

func ph(key, description string) messageTemplatePlaceholder {
	return messageTemplatePlaceholder{Key: key, Description: description}
}

// commonPlaceholders is the shared base list every category starts from
// (firstName/lastName/fullName/businessName).
func commonPlaceholders() []messageTemplatePlaceholder {
	return []messageTemplatePlaceholder{
		ph("{{firstName}}", "Customer first name"),
		ph("{{lastName}}", "Customer last name"),
		ph("{{fullName}}", "Customer full name"),
		ph("{{businessName}}", "Provider business name"),
	}
}

// withCommon returns the common base list plus the category-specific extras (preserving order:
// common first, then extras).
func withCommon(extras ...messageTemplatePlaceholder) []messageTemplatePlaceholder {
	out := commonPlaceholders()
	return append(out, extras...)
}

// messageTemplatePlaceholderMap is the static per-category placeholder map,
// keyed by the category name.
func messageTemplatePlaceholderMap() map[string][]messageTemplatePlaceholder {
	return map[string][]messageTemplatePlaceholder{
		"INVOICE": withCommon(
			ph("{{documentName}}", "Document name"),
			ph("{{grossAmount}}", "Gross amount of the invoice"),
			ph("{{invoiceNumber}}", "Invoice number"),
			ph("{{currency}}", "Currency"),
		),
		"BILL": withCommon(
			ph("{{documentName}}", "Document name"),
			ph("{{startDate}}", "The start of billing cycle"),
			ph("{{endDate}}", "The end of billing cycle"),
			ph("{{invoiceNumber}}", "Invoice number (stamped when the bill is issued)"),
			ph("{{issueDate}}", "Invoice issue date (YYYY-MM-DD)"),
			ph("{{dueDate}}", "Payment due date (YYYY-MM-DD; empty once the bill is paid)"),
			ph("{{amountDue}}", "Unpaid gross amount in the invoice currency (0.00 when paid)"),
			ph("{{currency}}", "Invoice currency"),
			ph("{{itemsCount}}", "Number of charged items on the bill"),
			ph("{{{itemsSummaryHtml}}}", "HTML table of the charges grouped by service (net amounts + total)"),
			ph("{{bankInstructions}}", "Bank transfer instructions of the BankTransfer payment gateway (plain text)"),
			ph("{{{bankInstructionsHtml}}}", "Bank transfer instructions as HTML (line breaks preserved)"),
			ph("{{billUrl}}", "Link to the invoice page in the customer console"),
		),
		"PAYMENT": withCommon(
			ph("{{grossAmount}}", "Gross amount"),
			ph("{{currency}}", "Currency"),
		),
		"SUSPENSION": withCommon(
			ph("{{balance}}", "Customer current balance"),
			ph("{{currency}}", "Currency"),
			ph("{{suspendAtBalance}}", "Suspend at balance amount - When customer balance is equal or less than this amount, customer will be suspended"),
			ph("{{suspendAtDueDays}}", "Suspend at due days - When customer has exceed due date for his bills, customer will be suspended"),
		),
		"BANK_TRANSFER": withCommon(
			ph("{{amount}}", "Amount"),
			ph("{{currency}}", "Currency"),
			ph("{{instructions}}", "Bank transfer instructions"),
			ph("{{referenceNumber}}", "Reference number"),
			ph("{{companyName}}", "Customer company name"),
			ph("{{bankReference}}", "Bank reference the customer quoted on submit"),
			ph("{{transferredAt}}", "Date the customer reports the transfer was made"),
			ph("{{invoiceNumber}}", "Invoice number of the bill being paid"),
			ph("{{billId}}", "Id of the bill being paid"),
			ph("{{adminUrl}}", "Admin console bank-transfers page"),
		),
		"BILLING": withCommon(
			ph("{{loginUrl}}", "Login URL for the customer"),
		),
		"PROJECT": withCommon(
			ph("{{projectName}}", "Name of the project"),
			ph("{{projectInviteUrl}}", "URL to accept project invite"),
			ph("{{email}}", "Email of the project invitee"),
			ph("{{expiryHours}}", "Hours until the invite expires"),
		),
	}
}
