//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// TestVolumeFipLbBillingResources verifies the VOLUME/FLOATING_IP bridge: seeded
// cloud resources (the shape the sync providers persist) map to the priced BillingResources the
// rating loop consumes.
func TestVolumeFipLbBillingResources(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := cloud.NewRepo(db)
	const projID, svcID = "proj1", "svc1"

	seed := func(typ string, data pgdoc.M) {
		_, err := db.C("cloudResource").InsertOne(ctx, pgdoc.M{
			"projectId": projID, "serviceId": svcID,
			"type": typ, "externalId": typ + "-ext", "data": data,
		})
		if err != nil {
			t.Fatalf("seed %s: %v", typ, err)
		}
	}
	seed(cloud.TypeVolume, pgdoc.M{"volume": pgdoc.M{"size": 50, "volume_type": "ssd", "status": "available", "bootable": "false"}})
	seed(cloud.TypeFloatingIP, pgdoc.M{"floatingIp": pgdoc.M{"status": "ACTIVE", "floating_network_id": "pubnet"}})

	registry := map[string]billingresource.Provider{
		cloud.TypeVolume:     billingresource.NewVolumeProvider(),
		cloud.TypeFloatingIP: billingresource.NewFloatingIPProvider(),
	}
	brs, err := billingresource.GetBillingResources(ctx, repo, registry, projID, svcID, billingapi.BillingContext{})
	if err != nil {
		t.Fatalf("GetBillingResources: %v", err)
	}
	got := map[string]map[string]any{}
	for _, br := range brs {
		got[br.ResourceType] = br.Values
	}
	if len(got) != 2 {
		t.Fatalf("resourceTypes = %v, want volume/floating_ip", keysOf(got))
	}
	if v := got["volume"]; v == nil || v["size"] != int64(50) && v["size"] != int32(50) && v["size"] != 50 && v["size"] != float64(50) {
		t.Fatalf("volume BR values: %+v", v)
	}
	if got["volume"]["display_name"] == nil || got["floating_ip"]["display_name"] != "floating-ip-FLOATING_IP-ext" {
		t.Fatalf("fip display_name: %+v", got["floating_ip"])
	}
}

func keysOf(m map[string]map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
