package admin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// priceadjustmentrule_repo.go holds the PriceAdjustmentRule-typed domain + DTO + store helpers the
// admin mutations need. The generic crud.go helpers operate on pgdoc.M; PriceAdjustmentRule carries
// decimal money (tier startAmount, modifier value) that must round-trip through the registered
// decimal codec, so these read/write the typed value. `_id` is a plain String id.
//
// The admin endpoints return the RAW document. Decimal money serializes as a JSON NUMBER; shopspring
// decimal would quote it, so the response goes through the DTO (json.Number money), mirroring how
// savingsplan.go reuses billing.SavingsPlanToDto.

// --- stored domain (money = decimal.Decimal, stored as a decimal string in jsonb) ---

// priceAdjustmentRule is the stored price-adjustment-rule domain.
type priceAdjustmentRule struct {
	ID          string                    `json:"id,omitempty"`
	Name        string                    `json:"name,omitempty"`
	Enabled     bool                      `json:"enabled"`
	Description string                    `json:"description,omitempty"`
	PricePlanID string                    `json:"pricePlanId,omitempty"`
	Targets     []priceAdjustmentTarget   `json:"targets,omitempty"`
	Tiers       []priceAdjustmentRuleTier `json:"tiers,omitempty"`
	CreatedAt   *time.Time                `json:"createdAt,omitempty"`
	UpdatedAt   *time.Time                `json:"updatedAt,omitempty"`
}

// priceAdjustmentTarget holds a target (resourceType + filters). Unlike
// SavingsPlanTarget, this target does NOT initialize filters to an empty list, so a
// null filters list is omitted — plain omitempty, no forced-empty MarshalJSON.
type priceAdjustmentTarget struct {
	ResourceType string                        `json:"resourceType,omitempty"`
	Filters      []pricing.PricePlanRuleFilter `json:"filters,omitempty"`
}

// priceAdjustmentRuleTier holds a tier (startAmount + modifier).
type priceAdjustmentRuleTier struct {
	StartAmount *decimal.Decimal             `json:"startAmount,omitempty"`
	Modifier    *priceAdjustmentRuleModifier `json:"modifier,omitempty"`
}

// priceAdjustmentRuleModifier holds a modifier (operator + asPercentage + value).
type priceAdjustmentRuleModifier struct {
	Operator     string           `json:"operator,omitempty"`
	AsPercentage bool             `json:"asPercentage"`
	Value        *decimal.Decimal `json:"value,omitempty"`
}

// --- response DTO (json; money = json.Number, so a plain JSON number not a quoted string) ---

type priceAdjustmentRuleDto struct {
	ID          string                       `json:"id,omitempty"`
	Name        string                       `json:"name,omitempty"`
	Enabled     bool                         `json:"enabled"`
	Description string                       `json:"description,omitempty"`
	PricePlanID string                       `json:"pricePlanId,omitempty"`
	Targets     []priceAdjustmentTarget      `json:"targets,omitempty"`
	Tiers       []priceAdjustmentRuleTierDto `json:"tiers,omitempty"`
	CreatedAt   *time.Time                   `json:"createdAt,omitempty"`
	UpdatedAt   *time.Time                   `json:"updatedAt,omitempty"`
}

type priceAdjustmentRuleTierDto struct {
	StartAmount json.Number                     `json:"startAmount,omitempty"`
	Modifier    *priceAdjustmentRuleModifierDto `json:"modifier,omitempty"`
}

type priceAdjustmentRuleModifierDto struct {
	Operator     string      `json:"operator,omitempty"`
	AsPercentage bool        `json:"asPercentage"`
	Value        json.Number `json:"value,omitempty"`
}

// priceAdjustmentRuleToDto maps the stored domain to the response DTO (decimal → json.Number).
func priceAdjustmentRuleToDto(rule *priceAdjustmentRule) priceAdjustmentRuleDto {
	return priceAdjustmentRuleDto{
		ID: rule.ID, Name: rule.Name, Enabled: rule.Enabled, Description: rule.Description,
		PricePlanID: rule.PricePlanID, Targets: rule.Targets,
		Tiers:     tiersToDtos(rule.Tiers),
		CreatedAt: rule.CreatedAt, UpdatedAt: rule.UpdatedAt,
	}
}

func tiersToDtos(tiers []priceAdjustmentRuleTier) []priceAdjustmentRuleTierDto {
	if tiers == nil {
		return nil
	}
	out := make([]priceAdjustmentRuleTierDto, 0, len(tiers))
	for i := range tiers {
		var mod *priceAdjustmentRuleModifierDto
		if tiers[i].Modifier != nil {
			mod = &priceAdjustmentRuleModifierDto{
				Operator:     tiers[i].Modifier.Operator,
				AsPercentage: tiers[i].Modifier.AsPercentage,
				Value:        parNum(tiers[i].Modifier.Value),
			}
		}
		out = append(out, priceAdjustmentRuleTierDto{StartAmount: parNum(tiers[i].StartAmount), Modifier: mod})
	}
	return out
}

// parNum renders a *decimal.Decimal as a json.Number (empty → omitted), a plain
// JSON number (a quoted decimal string would be wrong).
func parNum(d *decimal.Decimal) json.Number {
	if d == nil {
		return ""
	}
	return json.Number(d.String())
}

// --- datastore helpers ---

// InsertPriceAdjustmentRule saves a NEW rule. A String id field maps
// to the String `_id`; on a null id it generates a fresh hex String. The id is
// set on the returned value for the response.
func (r *Repo) InsertPriceAdjustmentRule(ctx context.Context, collection string, rule priceAdjustmentRule) (*priceAdjustmentRule, error) {
	rule.ID = pgdoc.NewID()
	if _, err := r.c(collection).InsertOne(ctx, rule); err != nil {
		return nil, err
	}
	return &rule, nil
}

// PriceAdjustmentRuleByID loads a rule by id: the typed rule, or (nil,nil) when absent.
func (r *Repo) PriceAdjustmentRuleByID(ctx context.Context, collection, id string) (*priceAdjustmentRule, error) {
	var rule priceAdjustmentRule
	found, err := r.c(collection).Get(ctx, id, &rule)
	if err != nil || !found {
		return nil, err
	}
	return &rule, nil
}

// ReplacePriceAdjustmentRule saves an EXISTING rule: id-preserving replace.
func (r *Repo) ReplacePriceAdjustmentRule(ctx context.Context, collection, id string, rule priceAdjustmentRule) error {
	rule.ID = id
	_, err := r.c(collection).Replace(ctx, id, rule)
	return err
}

// DeletePriceAdjustmentRule deletes by id → deleted count (0 → no-op; a missing id is not
// a 404).
func (r *Repo) DeletePriceAdjustmentRule(ctx context.Context, collection, id string) (int64, error) {
	ok, err := r.c(collection).DeleteByID(ctx, id)
	if err != nil {
		return 0, err
	}
	if ok {
		return 1, nil
	}
	return 0, nil
}

// priceAdjustmentRuleUsageDto is the usage wire shape {ruleId, ruleName, openBillsCount,
// totalAdjustmentsAmount} (money = json.Number so plain numbers survive).
type priceAdjustmentRuleUsageDto struct {
	RuleID                 string      `json:"ruleId,omitempty"`
	RuleName               string      `json:"ruleName,omitempty"`
	OpenBillsCount         int         `json:"openBillsCount"`
	TotalAdjustmentsAmount json.Number `json:"totalAdjustmentsAmount"`
}

// PriceAdjustmentRuleUsage aggregates rule usage: the OPEN
// bills carrying an adjustment for the rule, and Σ adjustments.amount for that rule across them.
func (r *Repo) PriceAdjustmentRuleUsage(ctx context.Context, ruleID string) (int, decimal.Decimal, error) {
	var bills []struct {
		Adjustments []struct {
			PriceAdjustmentRuleID string           `json:"priceAdjustmentRuleId"`
			Amount                *decimal.Decimal `json:"amount"`
		} `json:"adjustments"`
	}
	if err := r.c("bill").Find(ctx, pgdoc.M{
		"status":      "OPEN",
		"adjustments": pgdoc.M{"$elemMatch": pgdoc.M{"priceAdjustmentRuleId": ruleID}},
	}, &bills); err != nil {
		return 0, decimal.Zero, err
	}
	total := decimal.Zero
	for i := range bills {
		for _, a := range bills[i].Adjustments {
			if a.PriceAdjustmentRuleID == ruleID && a.Amount != nil {
				total = total.Add(*a.Amount)
			}
		}
	}
	return len(bills), total, nil
}

// PriceAdjustmentRulesByPricePlanID loads rules by price plan (ALL — not only enabled) → DTOs (never
// nil). Returns the raw documents; money → json.Number via the DTO.
func (r *Repo) PriceAdjustmentRulesByPricePlanID(ctx context.Context, collection, pricePlanID string) ([]priceAdjustmentRuleDto, error) {
	var rules []priceAdjustmentRule
	if err := r.c(collection).Find(ctx, pgdoc.M{"pricePlanId": pricePlanID}, &rules); err != nil {
		return nil, err
	}
	out := make([]priceAdjustmentRuleDto, 0, len(rules))
	for i := range rules {
		out = append(out, priceAdjustmentRuleToDto(&rules[i]))
	}
	return out, nil
}
