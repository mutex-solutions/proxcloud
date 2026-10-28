package billingresource

import (
	"context"
	"fmt"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// volumeNotEligibleStatuses: a volume in one of these
// statuses is excluded from billing (error/deleting/creating/error_deleting).
var volumeNotEligibleStatuses = map[string]bool{"error": true, "deleting": true, "creating": true, "error_deleting": true}

// VolumeProvider maps a VOLUME → a "volume"
// BillingResource (size/type/status attributes), flagged notEligibleForBilling for bad statuses.
type VolumeProvider struct{}

func NewVolumeProvider() *VolumeProvider { return &VolumeProvider{} }

func (p *VolumeProvider) Type() string { return cloud.TypeVolume }

func (p *VolumeProvider) GetBillingInformation(_ context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	vol, ok := mapAt(cr.Data, "volume")
	if !ok {
		return []*billingapi.BillingResource{}, nil
	}
	values := map[string]any{
		"size":              vol["size"],
		"bootable":          vol["bootable"],
		"type":              vol["volume_type"],
		"status":            vol["status"],
		"availability_zone": vol["availability_zone"],
	}
	dn := str(vol["name"])
	if dn == "" {
		dn = fmt.Sprintf("volume-%vgb-%v", vol["size"], vol["volume_type"])
	}
	values["display_name"] = dn
	return []*billingapi.BillingResource{{
		ResourceID:            cr.ID,
		ProjectID:             cr.ProjectID,
		ResourceType:          "volume",
		Values:                values,
		BillingResourceType:   volumeType(),
		NotEligibleForBilling: volumeNotEligibleStatuses[strings.ToLower(str(vol["status"]))],
	}}, nil
}

func volumeType() *billingapi.BillingResourceType {
	num := func(n string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: n, Type: "number"}
	}
	s := func(n string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: n, Type: "string"}
	}
	return &billingapi.BillingResourceType{ResourceType: "volume", Attributes: []billingapi.ResourceAttribute{
		num("size"), {Name: "bootable", Type: "boolean"}, s("type"), s("status"),
		s("availability_zone"), s("display_name"),
	}}
}
