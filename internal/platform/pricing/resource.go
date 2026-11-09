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
