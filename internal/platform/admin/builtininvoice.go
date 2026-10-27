package admin

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// builtininvoice.go serves the built-in-invoice surface (/api/v1/admin/built-in-invoice). Both
// endpoints are READS gated on ADMIN_BILL_READ:
//
//	GET /built-in-invoice/{id}          → list by gateway id (id == invoiceGatewayId)
//	GET /built-in-invoice/download/{id} → load by id or 404 → download the invoice PDF
//
// Neither is registered in handler.go (Grep'd), so both are added here. The integrator wires
// h.routeBuiltInInvoice(r) into Routes().
//
// The list read filters the `builtInInvoice` collection by invoiceGatewayId, newest first,
// wrapped in the list envelope.
// Empty under greenfield → {data:[],paging}; the raw BuiltInInvoice domain (carrying the nested
// BillingProfile + CreateInvoiceDetails) is passed through via shapeDoc (_id→id, drop _class). A
// populated doc's full raw shape (nested money / dates) is the same deferred concern as
// the other admin raw-domain lists — fails loud if populated, billing-list precedent.
//
// The download read loads by id (404 "Invoice %s not found" — interpolated, NO trailing space) then
// resolves the invoice integration provider and fetches the rendered PDF bytes from the external
// invoice vendor. That external fetch is an external integration point (no live vendor call this pass);
// the load-or-404 state check is faithful, then the endpoint returns 501.

const builtInInvoiceCollection = "builtInInvoice"

const builtInInvoiceReadPerm = "admin:bill:read"

// routeBuiltInInvoice registers the built-in-invoice admin reads. `download` is a static sibling of
// the {id} param at the same path position; chi resolves the static segment first, no conflict. Both
// param positions reuse the name {id} (an `id` path variable on both).
func (h *Handler) routeBuiltInInvoice(r chi.Router) {
	r.Get("/built-in-invoice/download/{id}", h.builtInInvoiceDownload)
	r.Get("/built-in-invoice/{id}", h.builtInInvoiceList)
}

// builtInInvoiceList lists the built-in invoices for a gateway id, newest first.
// The {id} path variable is the invoiceGatewayId.
func (h *Handler) builtInInvoiceList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, builtInInvoiceReadPerm) {
		return
	}
	items, err := h.repo.BuiltInInvoicesByGateway(r.Context(), chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	for i := range items {
		shapeDoc(items[i])
	}
	httpx.List(w, items)
}

// builtInInvoiceDownload loads the invoice by id or 404s, then performs the external invoice
// PDF download. The 404 is the faithful state check; the actual
// download from the invoice vendor is an external integration point → 501 after the existence check.
func (h *Handler) builtInInvoiceDownload(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, builtInInvoiceReadPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	doc, err := h.repo.FindDoc(r.Context(), builtInInvoiceCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("Invoice %s not found", id)))
		return
	}
	// External integration point (assessed dev232, stays deliberately): the download
	// renders the invoice PDF — via the stored HTML template or the
	// programmatic layout. Under greenfield NO builtInInvoice docs exist (the on-payment
	// invoice-generation leg itself is deferred), so this endpoint 404s before ever reaching the
	// render. Build the layout via fpdf when invoice generation lands.
	httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
		"downloadInvoice not implemented"))
}
