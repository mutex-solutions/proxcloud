package admin

import (
	"net/http"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// AdminInsights is the dashboard-charts payload. Empty buckets are valid —
// the admin FE renders empty charts rather than erroring.
type AdminInsights struct {
	CurrentMonthCosts    float64             `json:"currentMonthCosts"`
	CurrentMonthPayments float64             `json:"currentMonthPayments"`
	Bills                []MRRDetails        `json:"bills"`
	Payments             []MRRDetails        `json:"payments"`
	NewUsers             []DailyCountDetails `json:"newUsers"`
	NewBillingProfiles   []DailyCountDetails `json:"newBillingProfiles"`
}

// AdminStatsResponse is the admin-stats response payload.
type AdminStatsResponse struct {
	Users                   int64         `json:"users"`
	Projects                int64         `json:"projects"`
	CloudResources          int64         `json:"cloudResources"`
	Transactions            int64         `json:"transactions"`
	CloudProviderConfigured bool          `json:"cloudProviderConfigured"`
	BillingConfigured       bool          `json:"billingConfigured"`
	BrandingConfigured      bool          `json:"brandingConfigured"`
	MailGatewayConfigured   bool          `json:"mailGatewayConfigured"`
	PricePlanConfigured     bool          `json:"pricePlanConfigured"`
	Insights                AdminInsights `json:"insights"`
}

// adminStats returns live counts + configured flags + the insight
// buckets (MRR bills/payments + new users/billing-profiles, computed by buildInsights). All real.
func (h *Handler) adminStats(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:stats:read") {
		return
	}
	ctx := r.Context()
	count := func(c string) int64 { n, _ := h.repo.CountDocs(ctx, c); return n }
	// mailGatewayConfigured is true when some Mail-category catalog entry (SMTP)
	// has an installed thirdPartyIntegration doc.
	mailConfigured := false
	if installed, err := h.repo.InstalledThirdParties(ctx); err == nil {
		for _, e := range ThirdPartyCatalog {
			for _, c := range e.Categories {
				if c == "Mail" && installed[e.Name] {
					mailConfigured = true
				}
			}
		}
	}
	httpx.OK(w, AdminStatsResponse{
		Users:          count("users"),
		Projects:       count("project"),
		CloudResources: count("cloudResource"),
		// Total transactions = account-credit transactions + collect transactions.
		Transactions:            count("accountCreditTransaction") + count("collectTransaction"),
		CloudProviderConfigured: count("externalService") > 0,
		BillingConfigured:       count("billingConfiguration") > 0,
		BrandingConfigured:      count("platformConfiguration") > 0,
		MailGatewayConfigured:   mailConfigured,
		PricePlanConfigured:     count("pricePlan") > 0,
		Insights:                h.buildInsights(ctx),
	})
}
