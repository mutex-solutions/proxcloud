// Package billingapi is the wire contract between the two halves of the charge flow:
// the RESOLUTION half (proxcloud: read the cloud-resource cache and turn it into billable units)
// and the RATING half (the billing service: price those units onto a bill).
//
// It is deliberately a LEAF package — standard library only, no proxcloud imports — so that both
// sides can depend on it once billing is extracted into its own module. Nothing here may import
// internal/cloud or internal/platform/pricing; the mapping to the rating engine's own types lives
// on the rating side (see pricing.FromAPIResources).
//
// # Numeric fidelity
//
// BillingResource.Values ends up persisted verbatim as BillItem.Metadata on the bill, and its
// numbers are also what the rating engine prices. Decoding numbers into float64 (encoding/json's
// default for `any`) would silently round integers above 2^53 and reformat decimals, so every
// decode path in this package uses json.Decoder.UseNumber(): numbers arrive as json.Number, which
// preserves the literal exactly and re-encodes byte-identically. The rating engine's toDecimal
// already accepts json.Number, so rating is unaffected.
//
// Always decode with UnmarshalResources / NewDecoder rather than plain json.Unmarshal.
package billingapi

import (
	"bytes"
	"encoding/json"
	"io"
	"time"
)

// BillingResource is one billable unit as it crosses the wire — the serializable projection of the
// rating engine's resource. Field-for-field identical to pricing.BillingResource so the mapping
// stays mechanical.
type BillingResource struct {
	ResourceID            string               `json:"resourceId"`
	ProjectID             string               `json:"projectId"`
	ResourceType          string               `json:"resourceType"`
	CreatedAt             *time.Time           `json:"createdAt,omitempty"`
	DeletedAt             *time.Time           `json:"deletedAt,omitempty"`
	DisplayPrice          bool                 `json:"displayPrice"`
	Values                map[string]any       `json:"values,omitempty"`
	NotEligibleForBilling bool                 `json:"notEligibleForBilling"`
	SkipInterval          bool                 `json:"skipInterval,omitempty"`
	BillingResourceType   *BillingResourceType `json:"billingResourceType,omitempty"`
}

// BillingResourceType declares the attributes (name→type) of a resource type. The rating engine
// resolves every priced/filtered attribute against this schema, so an attribute that is absent here
// aborts the charge rather than rating as zero.
type BillingResourceType struct {
	ResourceType string              `json:"resourceType"`
	Attributes   []ResourceAttribute `json:"attributes"`
}

// ResourceAttribute is one declared attribute. IsUsage is a pointer because the nil-vs-false
// distinction is meaningful to the engine (skipCurrentPrice).
type ResourceAttribute struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	IsUsage *bool  `json:"isUsage,omitempty"`
}

// BillingContext carries the per-time-unit charge limits (from
// billingConfiguration.settings.timeUnitLimits); nil/absent → the rating side's defaults.
type BillingContext struct {
	TimeUnitLimits map[string]int `json:"timeUnitLimits,omitempty"`
	// CycleStart is the billing cycle the resolution is for, which is the month before the clock's on
	// the pass closing a month. Zero means the clock's month. Local to the resolution side.
	CycleStart time.Time `json:"-"`
}

// NewDecoder returns a JSON decoder configured for this contract (UseNumber). Prefer it over
// json.NewDecoder for anything carrying a BillingResource.
func NewDecoder(r io.Reader) *json.Decoder {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	return dec
}

// UnmarshalResources decodes a JSON array of billing resources, preserving numeric literals
// exactly (see the package doc).
func UnmarshalResources(data []byte) ([]*BillingResource, error) {
	var out []*BillingResource
	if err := NewDecoder(bytes.NewReader(data)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// UnmarshalResourceTypes decodes a JSON array of resource-type schemas (the catalog).
func UnmarshalResourceTypes(data []byte) ([]*BillingResourceType, error) {
	var out []*BillingResourceType
	if err := NewDecoder(bytes.NewReader(data)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
