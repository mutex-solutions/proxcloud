package pricing

import (
	"fmt"
	"time"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// BillingResource is the rated unit — only the fields the rating engine reads are modeled.
type BillingResource struct {
	ResourceID            string
	ProjectID             string
	ResourceType          string
	CreatedAt             *time.Time
	DeletedAt             *time.Time
	DisplayPrice          bool
	Values                map[string]any
	NotEligibleForBilling bool
	// SkipInterval, with NotEligibleForBilling, advances the watermark so the interval is never
	// back-billed (the cloud state behind it could not be observed).
	SkipInterval        bool
	BillingResourceType *BillingResourceType
	// ChargeFrom, VariableRate and EarlierInterval describe a resource charged by its running
	// intervals (billingapi.BillingResource has the contract). With none of them set the engine
	// charges exactly as it did before they existed.
	//
	// ChargeFrom is the start of the running interval: a minute or hour charge counts from the later
	// of the watermark and ChargeFrom cut to the unit (getTimeUnitsDiff).
	ChargeFrom *time.Time
	// VariableRate lifts the money cap on minute and hour charges (updateBillItems); never on month.
	VariableRate bool
	// EarlierInterval is a closed interval [ChargeFrom, DeletedAt] that ended before the one fed
	// after it: rated on minute charges only (SaveChargingToBill).
	EarlierInterval bool
}

// BillingResourceType declares the attributes (name→type) of a resource type.
type BillingResourceType struct {
	ResourceType string
	Attributes   []ResourceAttribute
}

// AttributeTypeByName
// returns the declared attribute's type, or a 404 error if the attribute is not
// declared. A missing attribute returns not-found, aborting the whole rating call
// — so the engine bubbles this error rather than treating a missing attribute as a
// non-match. Match is case-sensitive.
func (t *BillingResourceType) AttributeTypeByName(name string) (string, error) {
	for i := range t.Attributes {
		if t.Attributes[i].Name == name {
			return t.Attributes[i].Type, nil
		}
	}
	return "", httpx.NotFound(fmt.Sprintf("Could not find attribute with name: %s", name))
}

// ResourceAttributeByName returns the named attribute, or a 404 error (same as AttributeTypeByName).
func (t *BillingResourceType) ResourceAttributeByName(name string) (*ResourceAttribute, error) {
	for i := range t.Attributes {
		if t.Attributes[i].Name == name {
			return &t.Attributes[i], nil
		}
	}
	return nil, httpx.NotFound(fmt.Sprintf("Could not find attribute with name: %s", name))
}

// ResourceAttribute: IsUsage is nullable — the
// nil-vs-false distinction matters in skipCurrentPrice.
type ResourceAttribute struct {
	Type    string
	Name    string
	IsUsage *bool
}
