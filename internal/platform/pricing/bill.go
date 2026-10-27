package pricing

import (
	"time"

	"github.com/shopspring/decimal"
)

// Bill / BillItem assembly domains. This
// slice models the CORE build path; money is kept as shopspring/decimal for exact
// math (the running BillItem net is accumulated at scale 16). The wire/JSON form is
// a JSON number at fixed scale — `decimal.StringFixed(16)` on the build path,
// `StringFixed(2)` after ScaleUpItems — which lands with the persistence slice
// (there is no repo/serialization here; golden tests assert decimal values). Bill's
// peripheral collections (adjustments, applied credits, collected amounts) are
// deferred with the credits/dunning slices.

// BillStatus is the bill status.
type BillStatus string

const (
	BillStatusOpen BillStatus = "OPEN"
	BillStatusSent BillStatus = "SENT"
	BillStatusPaid BillStatus = "PAID"
)

// TimeUnit values (the BillingCycle time-unit values).
const (
	TimeUnitMinute = "minute"
	TimeUnitHour   = "hour"
	TimeUnitMonth  = "month"
)

// Bill is the "bill" collection aggregate (core fields only).
type Bill struct {
	ID               string            `json:"id,omitempty"`
	Status           BillStatus        `json:"status,omitempty"`
	Items            []BillItem        `json:"items"`
	InvoiceCurrency  string            `json:"invoiceCurrency,omitempty"`
	BillingProfileID string            `json:"billingProfileId,omitempty"`
	BillingCycle     *BillBillingCycle `json:"billingCycle,omitempty"`
	// Settlement collections (credits/dunning slices): credits + collected payments
	// applied against the bill net, plus adjustments.
	Adjustments               []BillAdjustment           `json:"adjustments,omitempty"`
	AppliedAccountCredits     []AppliedAccountCredit     `json:"appliedAccountCredits,omitempty"`
	AppliedPromotionalCredits []AppliedPromotionalCredit `json:"appliedPromotionalCredits,omitempty"`
	CollectedAmounts          []AppliedCollectedCredit   `json:"collectedAmounts,omitempty"`
	DueAt                     *time.Time                 `json:"dueAt,omitempty"`
	SentAt                    *time.Time                 `json:"sentAt,omitempty"`
	LockedAt                  *time.Time                 `json:"lockedAt,omitempty"`
	CreatedAt                 *time.Time                 `json:"createdAt,omitempty"`
	UpdatedAt                 *time.Time                 `json:"updatedAt,omitempty"`
}

// BillItem is an embedded per-resource line. NetAmount carries the running total at
// scale 16 (re-derived from AppliedPricePlanRules each charge, then cap-clamped).
type BillItem struct {
	Name                  string                         `json:"name,omitempty"`
	ResourceID            string                         `json:"resourceId,omitempty"`
	Currency              string                         `json:"currency,omitempty"`
	ProjectID             string                         `json:"projectId,omitempty"`
	CreatedAt             *time.Time                     `json:"createdAt,omitempty"`
	UpdatedAt             *time.Time                     `json:"updatedAt,omitempty"`
	ResourceType          string                         `json:"resourceType,omitempty"`
	NetAmount             decimal.Decimal                `json:"netAmount"`
	TimeUnits             *BillItemTimeUnits             `json:"timeUnits,omitempty"`
	Metadata              map[string]any                 `json:"metadata,omitempty"`
	AppliedPricePlanRules []BillItemAppliedPricePlanRule `json:"appliedPricePlanRules"`
}

// BillItemAppliedPricePlanRule groups the per-attribute applied amounts contributed
// by one price-plan rule.
type BillItemAppliedPricePlanRule struct {
	PricePlanRuleID string                               `json:"pricePlanRuleId,omitempty"`
	AppliedAmounts  []BillItemAppliedPricePlanRuleAmount `json:"appliedAmounts"`
}

// BillItemAppliedPricePlanRuleAmount is the per-attribute running net (scale 16).
type BillItemAppliedPricePlanRuleAmount struct {
	AttributeName      string          `json:"attributeName,omitempty"`
	LastAttributeValue any             `json:"lastAttributeValue,omitempty"`
	NetAmount          decimal.Decimal `json:"netAmount"`
}

// BillItemTimeUnits tracks per-cadence charge counters + watermarks. Pointers: a nil
// month watermark means "no MONTH charge yet" (the new-item case); nil minute/hour
// watermark forces a diff of 1 on the next charge.
type BillItemTimeUnits struct {
	Minute             *int64     `json:"minute,omitempty"`
	MinuteLastRateTime *time.Time `json:"minuteLastRateTime,omitempty"`
	Hour               *int64     `json:"hour,omitempty"`
	HourLastRateTime   *time.Time `json:"hourLastRateTime,omitempty"`
	Month              *int64     `json:"month,omitempty"`
	MonthLastRateTime  *time.Time `json:"monthLastRateTime,omitempty"`
}

// BillBillingCycle is the bill's [startDate, endDate) window.
type BillBillingCycle struct {
	StartDate *time.Time `json:"startDate,omitempty"`
	EndDate   *time.Time `json:"endDate,omitempty"`
}

// RatingContext is the cadence + the charge instant.
type RatingContext struct {
	TimeUnit       string
	CycleTimestamp time.Time
}

// BillingContext carries the per-time-unit charge limits (from
// billingConfiguration.settings.timeUnitLimits); nil/absent → the default map.
type BillingContext struct {
	TimeUnitLimits map[string]int
}

// defaultTimeUnitLimits is the default per-time-unit limits (30-day month).
func defaultTimeUnitLimits() map[string]int {
	return map[string]int{TimeUnitMinute: 43200, TimeUnitHour: 720, TimeUnitMonth: 1}
}

// timeUnitLimit returns the configured limit for the
// time unit, else the default.
func (bc BillingContext) timeUnitLimit(timeUnit string) int {
	if bc.TimeUnitLimits != nil {
		if v, ok := bc.TimeUnitLimits[timeUnit]; ok {
			return v
		}
	}
	return defaultTimeUnitLimits()[timeUnit]
}

// CalculateTotalAmount computes the bill total (never persisted; not
// serialized) — the sum of item net amounts in the bill's product currency.
func (b *Bill) CalculateTotalAmount() decimal.Decimal {
	total := decimal.Zero
	for i := range b.Items {
		total = total.Add(b.Items[i].NetAmount)
	}
	return total
}
