package billing

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// Savings domains + their client DTOs. The savings-contracts
// and savings-plans list endpoints return the raw documents; money is a JSON number
// (big-decimal), so the DTOs use json.Number (decimal.Decimal would quote). Money in the tree is
// shallow: SavingsContract.{discountRate,monthlyCommittedAmount}; SavingsPlanSchedule.maxAmount;
// SavingsPlanTier.{startAmount,discount}. Targets carry no money (resourceType + filters).

// SavingsContract.Status values.
const (
	SavingsStatusActive    = "ACTIVE"
	SavingsStatusExpired   = "EXPIRED"
	SavingsStatusCancelled = "CANCELLED"
	// SavingsStatusPendingPayment is an upfront contract nobody has paid for: it discounts nothing
	// until a deposit bound to it settles.
	SavingsStatusPendingPayment = "PENDING_PAYMENT"
)

// --- decode structs (money = decimal.Decimal) ---

// UpfrontUnpaid reports an upfront contract with no record of its money: no deposit paid it and no
// operator answers for it. It must not discount, whatever its status says.
func (c *SavingsContract) UpfrontUnpaid() bool {
	return c.PaidUpfront && c.PaymentTransactionID == "" && c.PaymentRecordedBy == ""
}

type SavingsContract struct {
	ID                     string              `json:"id,omitempty"`
	BillingProfileID       string              `json:"billingProfileId,omitempty"`
	Status                 string              `json:"status,omitempty"`
	SavingsPlanID          string              `json:"savingsPlanId,omitempty"`
	SavingsPlanName        string              `json:"savingsPlanName,omitempty"`
	DurationMonths         int                 `json:"durationMonths"`
	StartDate              *time.Time          `json:"startDate,omitempty"`
	EndDate                *time.Time          `json:"endDate,omitempty"`
	DiscountRate           *decimal.Decimal    `json:"discountRate,omitempty"`
	Targets                []SavingsPlanTarget `json:"targets,omitempty"`
	MonthlyCommittedAmount *decimal.Decimal    `json:"monthlyCommittedAmount,omitempty"`
	PaidUpfront            bool                `json:"paidUpfront"`
	// UpfrontAmount is what an upfront contract costs: the term's commitment less its discount.
	UpfrontAmount *decimal.Decimal `json:"upfrontAmount,omitempty"`
	// PaymentTransactionID is the deposit that paid it, which makes a retried settle a no-op.
	PaymentTransactionID string `json:"paymentTransactionId,omitempty"`
	// PaymentRecordedBy is the operator answering for money taken outside the portal, PaymentReference
	// their note of it (a bank reference, a receipt number).
	PaymentRecordedBy string     `json:"paymentRecordedBy,omitempty"`
	PaymentReference  string     `json:"paymentReference,omitempty"`
	OrderID           string     `json:"orderId,omitempty"`
	CreatedAt         *time.Time `json:"createdAt,omitempty"`
	UpdatedAt         *time.Time `json:"updatedAt,omitempty"`
}

type SavingsPlan struct {
	ID              string                      `json:"id,omitempty"`
	Name            string                      `json:"name,omitempty"`
	Available       bool                        `json:"available"`
	Description     string                      `json:"description,omitempty"`
	Targets         []SavingsPlanTarget         `json:"targets,omitempty"`
	SavingSchedule  []SavingsPlanSchedule       `json:"savingSchedule,omitempty"`
	AccessMode      string                      `json:"accessMode,omitempty"`
	BillingProfiles []SavingsPlanBillingProfile `json:"billingProfiles,omitempty"`
	CreatedAt       *time.Time                  `json:"createdAt,omitempty"`
	UpdatedAt       *time.Time                  `json:"updatedAt,omitempty"`
}

type SavingsPlanTarget struct {
	ResourceType string                        `json:"resourceType,omitempty"`
	Filters      []pricing.PricePlanRuleFilter `json:"filters,omitempty"`
}

// MarshalJSON emits `filters` as a non-null array even when empty (SavingsPlanTarget
// initializes filters to an empty list → `filters:[]` is kept, not omitted).
func (t SavingsPlanTarget) MarshalJSON() ([]byte, error) {
	filters := t.Filters
	if filters == nil {
		filters = []pricing.PricePlanRuleFilter{}
	}
	return json.Marshal(struct {
		ResourceType string                        `json:"resourceType,omitempty"`
		Filters      []pricing.PricePlanRuleFilter `json:"filters"`
	}{ResourceType: t.ResourceType, Filters: filters})
}

type SavingsPlanSchedule struct {
	DurationMonths int               `json:"durationMonths"`
	MaxAmount      *decimal.Decimal  `json:"maxAmount,omitempty"`
	NoUpfrontTiers []SavingsPlanTier `json:"noUpfrontTiers,omitempty"`
	UpfrontTiers   []SavingsPlanTier `json:"upfrontTiers,omitempty"`
}

type SavingsPlanTier struct {
	StartAmount *decimal.Decimal `json:"startAmount,omitempty"`
	Discount    *decimal.Decimal `json:"discount,omitempty"`
}

type SavingsPlanBillingProfile struct {
	BillingProfileID string `json:"billingProfileId,omitempty"`
}

// --- client DTOs (json; money = json.Number) ---

type SavingsContractDto struct {
	ID                     string              `json:"id,omitempty"`
	BillingProfileID       string              `json:"billingProfileId,omitempty"`
	Status                 string              `json:"status,omitempty"`
	SavingsPlanID          string              `json:"savingsPlanId,omitempty"`
	SavingsPlanName        string              `json:"savingsPlanName,omitempty"`
	DurationMonths         int                 `json:"durationMonths"`
	StartDate              *time.Time          `json:"startDate,omitempty"`
	EndDate                *time.Time          `json:"endDate,omitempty"`
	DiscountRate           json.Number         `json:"discountRate,omitempty"`
	Targets                []SavingsPlanTarget `json:"targets,omitempty"`
	MonthlyCommittedAmount json.Number         `json:"monthlyCommittedAmount,omitempty"`
	PaidUpfront            bool                `json:"paidUpfront"`
	UpfrontAmount          json.Number         `json:"upfrontAmount,omitempty"`
	OrderID                string              `json:"orderId,omitempty"`
	CreatedAt              *time.Time          `json:"createdAt,omitempty"`
	UpdatedAt              *time.Time          `json:"updatedAt,omitempty"`
}

type SavingsPlanScheduleDto struct {
	DurationMonths int                  `json:"durationMonths"`
	MaxAmount      json.Number          `json:"maxAmount,omitempty"`
	NoUpfrontTiers []SavingsPlanTierDto `json:"noUpfrontTiers,omitempty"`
	UpfrontTiers   []SavingsPlanTierDto `json:"upfrontTiers,omitempty"`
}

type SavingsPlanTierDto struct {
	StartAmount json.Number `json:"startAmount,omitempty"`
	Discount    json.Number `json:"discount,omitempty"`
}

type SavingsPlanDto struct {
	ID              string                      `json:"id,omitempty"`
	Name            string                      `json:"name,omitempty"`
	Available       bool                        `json:"available"`
	Description     string                      `json:"description,omitempty"`
	Targets         []SavingsPlanTarget         `json:"targets,omitempty"`
	SavingSchedule  []SavingsPlanScheduleDto    `json:"savingSchedule,omitempty"`
	AccessMode      string                      `json:"accessMode,omitempty"`
	BillingProfiles []SavingsPlanBillingProfile `json:"billingProfiles,omitempty"`
	CreatedAt       *time.Time                  `json:"createdAt,omitempty"`
	UpdatedAt       *time.Time                  `json:"updatedAt,omitempty"`
}

// --- mappers ---

func SavingsContractToDto(c *SavingsContract) SavingsContractDto {
	return SavingsContractDto{
		ID: c.ID, BillingProfileID: c.BillingProfileID, Status: c.Status,
		SavingsPlanID: c.SavingsPlanID, SavingsPlanName: c.SavingsPlanName,
		DurationMonths: c.DurationMonths, StartDate: c.StartDate, EndDate: c.EndDate,
		DiscountRate: numPtr(c.DiscountRate), Targets: c.Targets,
		MonthlyCommittedAmount: numPtr(c.MonthlyCommittedAmount),
		PaidUpfront:            c.PaidUpfront, UpfrontAmount: numPtr(c.UpfrontAmount), OrderID: c.OrderID,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

func SavingsPlanToDto(p *SavingsPlan) SavingsPlanDto {
	schedules := make([]SavingsPlanScheduleDto, 0, len(p.SavingSchedule))
	for i := range p.SavingSchedule {
		s := &p.SavingSchedule[i]
		schedules = append(schedules, SavingsPlanScheduleDto{
			DurationMonths: s.DurationMonths, MaxAmount: numPtr(s.MaxAmount),
			NoUpfrontTiers: tiersToDtos(s.NoUpfrontTiers), UpfrontTiers: tiersToDtos(s.UpfrontTiers),
		})
	}
	return SavingsPlanDto{
		ID: p.ID, Name: p.Name, Available: p.Available, Description: p.Description,
		Targets: p.Targets, SavingSchedule: schedules, AccessMode: p.AccessMode,
		BillingProfiles: p.BillingProfiles, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func tiersToDtos(tiers []SavingsPlanTier) []SavingsPlanTierDto {
	if tiers == nil {
		return nil
	}
	out := make([]SavingsPlanTierDto, 0, len(tiers))
	for i := range tiers {
		out = append(out, SavingsPlanTierDto{StartAmount: numPtr(tiers[i].StartAmount), Discount: numPtr(tiers[i].Discount)})
	}
	return out
}
