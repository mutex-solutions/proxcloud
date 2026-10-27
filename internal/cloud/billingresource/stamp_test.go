package billingresource

import (
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// attrTypeByName is the local equivalent of the rating engine's AttributeTypeByName lookup. The
// wire contract (billingapi) is pure data by design — the resolution side must not depend on the
// rating package — so the test inspects the declared attributes directly.
func attrTypeByName(t *billingapi.BillingResourceType, name string) string {
	if t == nil {
		return ""
	}
	for i := range t.Attributes {
		if t.Attributes[i].Name == name {
			return t.Attributes[i].Type
		}
	}
	return ""
}

func TestStampResourceValues(t *testing.T) {
	info := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	doc := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)

	// Info.CreatedAt preferred over doc createdAt; region/service_id stamped + attrs declared.
	withType := &billingapi.BillingResource{BillingResourceType: &billingapi.BillingResourceType{ResourceType: "instance"}}
	noType := &billingapi.BillingResource{}
	cr := &cloud.CloudResource{Region: "RegionOne", ServiceID: "svc-x", CreatedAt: &doc, Info: &cloud.Info{CreatedAt: &info}}

	stampResourceValues([]*billingapi.BillingResource{withType, noType}, cr)

	if withType.Values["region"] != "RegionOne" || withType.Values["service_id"] != "svc-x" {
		t.Errorf("region/service_id not stamped: %#v", withType.Values)
	}
	if withType.CreatedAt == nil || !withType.CreatedAt.Equal(info) {
		t.Errorf("CreatedAt = %v, want Info.CreatedAt %v", withType.CreatedAt, info)
	}
	if got := attrTypeByName(withType.BillingResourceType, "region"); got != "string" {
		t.Errorf("region attr not declared as string, got %q", got)
	}
	if got := attrTypeByName(withType.BillingResourceType, "service_id"); got != "string" {
		t.Errorf("service_id attr not declared as string, got %q", got)
	}
	// nil-type resource: createdAt still stamped, region/service_id left unset (no type to declare on).
	if noType.CreatedAt == nil || noType.Values["region"] != nil {
		t.Errorf("nil-type should stamp createdAt but not region: %#v", noType.Values)
	}

	// Idempotent: re-stamping does not duplicate the attrs.
	stampResourceValues([]*billingapi.BillingResource{withType}, cr)
	n := 0
	for _, a := range withType.BillingResourceType.Attributes {
		if a.Name == "region" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("region attr duplicated: count=%d", n)
	}

	// Fallback to doc createdAt when Info is absent.
	noInfo := &billingapi.BillingResource{BillingResourceType: &billingapi.BillingResourceType{}}
	stampResourceValues([]*billingapi.BillingResource{noInfo}, &cloud.CloudResource{CreatedAt: &doc})
	if noInfo.CreatedAt == nil || !noInfo.CreatedAt.Equal(doc) {
		t.Errorf("fallback CreatedAt = %v, want doc %v", noInfo.CreatedAt, doc)
	}
}
