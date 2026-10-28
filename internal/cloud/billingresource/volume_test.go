package billingresource

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

func TestVolumeProfileAttributes(t *testing.T) {
	tests := []struct {
		name               string
		vol                map[string]any
		wantProfile, class any
	}{
		{"proxmox capacity disk", map[string]any{"size": 500, "volume_type": "capacity", "profile": "capacity", "storage_class": "capacity", "status": "in-use"}, "capacity", "capacity"},
		{"no profile (other backend)", map[string]any{"size": 10, "volume_type": "ssd1", "status": "in-use"}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := &cloud.CloudResource{ID: "v1", ProjectID: "p1", Data: map[string]any{"volume": tt.vol}}
			brs, err := NewVolumeProvider().GetBillingInformation(context.Background(), billingapi.BillingContext{}, cr)
			if err != nil || len(brs) != 1 {
				t.Fatalf("brs=%v err=%v", brs, err)
			}
			br := brs[0]
			if br.Values["profile"] != tt.wantProfile || br.Values["storage_class"] != tt.class {
				t.Fatalf("values = %v", br.Values)
			}
			// Price-rule filters need the attributes declared (an undeclared one aborts the charge).
			declared := map[string]string{}
			for _, a := range br.BillingResourceType.Attributes {
				declared[a.Name] = a.Type
			}
			if declared["profile"] != "string" || declared["storage_class"] != "string" {
				t.Fatalf("attributes = %v", declared)
			}
		})
	}
}
