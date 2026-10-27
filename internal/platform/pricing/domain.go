// Package pricing implements the rating core:
// the PricePlan/PricePlanRule/TaxRate domains + the rule-application engine that
// turns a resource's usage values into net amounts via a price plan's rules.
//
// This first slice is the PURE rating math, verified by golden unit tests (pricing
// is admin-configured, so there is no client-facing surface). Deferred to later
// slices: persistence/repo + config-as-code seed, the monthly rollup + ApplyMethod
// effect, bill assembly, tax + FX, credits, dunning/suspension, invoicing, and the
// admin CRUD endpoints. Money uses shopspring/decimal for exact add/sub/mul; the
// ONLY rounding inside the engine is two DECIMAL128 (34-significant-
// digit, HALF_EVEN) divisions — see mathctx.go.
package pricing

import (
	"time"

	"github.com/shopspring/decimal"
)

// AccessMode values.
const (
	AccessPublic = "PUBLIC"
	AccessScoped = "SCOPED"
)

// ApplyMethod values. Effect is applied in the
// monthly rollup, NOT inside the per-rule engine.
const (
	ApplyAddToTotal     = "ADD_TO_TOTAL"
	ApplyOverwriteTotal = "OVERWRITE_TOTAL"
)

// TaxRate audience levels (TaxRate.Level).
const (
	TaxAudienceBusinessOnly  = "BUSINESS_ONLY"
	TaxAudienceConsumersOnly = "CONSUMERS_ONLY"
	TaxAudienceAll           = "ALL"
)

// PricePlan is the pricePlan document.
type PricePlan struct {
	ID               string                     `json:"id,omitempty"`
	Name             string                     `json:"name,omitempty"`
	Enabled          bool                       `json:"enabled"`
	AccessMode       string                     `json:"accessMode,omitempty"`
	ServiceProviders []PricePlanServiceProvider `json:"serviceProviders,omitempty"`
	CreatedAt        *time.Time                 `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time                 `json:"updatedAt,omitempty"`
}

// IsServiceProviderScoped: no
// providers ⇒ applies to all; else match by serviceId (region is stored, unused).
func (p PricePlan) IsServiceProviderScoped(externalServiceID string) bool {
	if len(p.ServiceProviders) == 0 {
		return true
	}
	for _, sp := range p.ServiceProviders {
		if sp.ServiceID == externalServiceID {
			return true
		}
	}
	return false
}

// PricePlanServiceProvider (embedded).
type PricePlanServiceProvider struct {
	ServiceID string `json:"serviceId,omitempty"`
	Region    string `json:"region,omitempty"`
}

// PricePlanRule is the pricePlanRule document; FK pricePlanId
// to PricePlan (rules are not embedded).
type PricePlanRule struct {
	ID           string                  `json:"id,omitempty"`
	Name         string                  `json:"name,omitempty"`
	TimeUnit     string                  `json:"timeUnit,omitempty"`
	ResourceType string                  `json:"resourceType,omitempty"`
	PricePlanID  string                  `json:"pricePlanId,omitempty"`
	ApplyMethod  string                  `json:"applyMethod,omitempty"`
	Prices       []PricePlanRulePrice    `json:"prices,omitempty"`
	Filters      []PricePlanRuleFilter   `json:"filters,omitempty"`
	Modifiers    []PricePlanRuleModifier `json:"modifiers,omitempty"`
}

// PricePlanRulePrice (embedded): a priced attribute with graduated tiers.
type PricePlanRulePrice struct {
	AttributeName string      `json:"attributeName,omitempty"`
	Tiers         []PriceTier `json:"tiers,omitempty"`
}

// PriceTier (embedded). from/to/value are nullable — the nil-vs-set
// distinction is load-bearing in applyTier — so pointers here.
type PriceTier struct {
	From  *decimal.Decimal `json:"from,omitempty"`
	To    *decimal.Decimal `json:"to,omitempty"`
	Value *decimal.Decimal `json:"value,omitempty"`
}

// PricePlanRuleFilter (embedded): a match gate on a resource attribute.
type PricePlanRuleFilter struct {
	AttributeName string `json:"attributeName,omitempty"`
	Operator      string `json:"operator,omitempty"`
	Value         any    `json:"value,omitempty"`
	Values        []any  `json:"values,omitempty"`
}

// PricePlanRuleModifier (embedded): conditionally adjusts the rated amount.
type PricePlanRuleModifier struct {
	AttributeName    string          `json:"attributeName,omitempty"`
	AttributeValue   any             `json:"attributeValue,omitempty"`
	Operator         string          `json:"operator,omitempty"`
	ModifierOperator string          `json:"modifierOperator,omitempty"`
	AsPercentage     bool            `json:"asPercentage"`
	Value            decimal.Decimal `json:"value"`
}

// TaxRate / TaxLevel are the taxRate document. Authored for the deferred
// tax-calculation slice (config shape); the calc itself is not in this slice.
type TaxRate struct {
	ID               string     `json:"id,omitempty"`
	Name             string     `json:"name,omitempty"`
	State            string     `json:"state,omitempty"`
	Country          string     `json:"country,omitempty"`
	Level            string     `json:"level,omitempty"`
	AccessMode       string     `json:"accessMode,omitempty"`
	RateLevels       []TaxLevel `json:"rateLevels,omitempty"`
	StartDate        *time.Time `json:"startDate,omitempty"`
	EndDate          *time.Time `json:"endDate,omitempty"`
	StartDateEnabled bool       `json:"startDateEnabled"`
	EndDateEnabled   bool       `json:"endDateEnabled"`
}

// TaxLevel (embedded): a whole-percent level within a TaxRate.
type TaxLevel struct {
	Level      int `json:"level"`
	Percentage int `json:"percentage"`
}

// PricePlanRuleResult / PricePlanRuleResultAmount are transient compute outputs
// (not persisted, not seeded).
type PricePlanRuleResult struct {
	PricePlanRule PricePlanRule
	Amounts       []PricePlanRuleResultAmount
}

// PricePlanRuleResultAmount: one priced attribute's rated quantity + net amount.
type PricePlanRuleResultAmount struct {
	AttributeName  string
	AttributeValue decimal.Decimal
	NetAmount      decimal.Decimal
}
