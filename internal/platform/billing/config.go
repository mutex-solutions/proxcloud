package billing

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// PublicBillingConfiguration is the
// public view of BillingConfiguration. promotionCodesEnabled is a primitive bool
// (always serialized); baseCurrency is omitted when blank.
type PublicBillingConfiguration struct {
	BaseCurrency          string `json:"baseCurrency,omitempty"`
	PromotionCodesEnabled bool   `json:"promotionCodesEnabled"`
}

// ConfigHandler serves the client billing-configuration read.
// Open-source build: billing is always available, so the
// only gate is whether a billingConfiguration document exists.
type ConfigHandler struct {
	repo *Repo
}

func NewConfigHandler(repo *Repo) *ConfigHandler {
	return &ConfigHandler{repo: repo}
}

func (h *ConfigHandler) Routes(r chi.Router) {
	r.Get("/billing-configuration/default", h.getDefault)
}

// getDefault returns PublicBillingConfiguration.from(getBillingConfiguration()):
// 400 when no config is seeded (BILLING_NOT_CONFIGURED). The
// promotionCodesEnabled mapping is !FALSE.equals(promo): null/true → true, false → false.
func (h *ConfigHandler) getDefault(w http.ResponseWriter, r *http.Request) {
	baseCurrency, promo, found, err := h.repo.Configuration(r.Context())
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, 500, "internal.error")
		return
	}
	if !found {
		httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "Billing is not configured")
		return
	}
	httpx.OK(w, PublicBillingConfiguration{
		BaseCurrency:          baseCurrency,
		PromotionCodesEnabled: !(promo != nil && !*promo),
	})
}
