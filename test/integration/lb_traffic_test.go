//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// C2 (v0.11.0 plan): the traffic of a load balancer pair is the tap of each appliance's net1, which carries every VIP
// of the balancer. It is counted once per (appliance, balancer), never once per VIP: with two VIPs and one sample of
// 3 MiB in and 1 MiB out, the appliance has one counter of 3 and 1 MiB. On main the appliance had no counter at all
// (the VIP rows name the balancer, not the VM), and a per-VIP match would have billed the tap twice.
func TestPVETrafficOfALoadBalancerPairCountsOnce(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := cloud.NewRepo(db)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/cluster/resources":
			fmt.Fprint(w, `{"data":[{"id":"qemu/201","type":"qemu","vmid":201,"node":"n1","pool":"pc-p","status":"running"}]}`)
		case "/api2/json/nodes/n1/qemu/201/status/current":
			fmt.Fprintf(w, `{"data":{"status":"running","nics":{"tap201i0":{"netin":999999999,"netout":999999999},"tap201i1":{"netin":%d,"netout":%d}}}}`, 3<<20, 1<<20)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "portal@pve!t", Secret: "key"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	appliance, err := repo.Insert(ctx, &cloud.CloudResource{ProjectID: "p", ServiceID: "s", ExternalID: "201", Region: "r", Type: cloud.TypeServer, CreatedAt: &now,
		Data: map[string]any{"server": map[string]any{"status": "ACTIVE", "loadBalancerId": "abc"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"198.51.100.9", "198.51.100.10"} {
		if _, err := repo.Insert(ctx, &cloud.CloudResource{ProjectID: "p", ServiceID: "s", ExternalID: "pub/" + ip + "/1", Region: "r", Type: cloud.TypeFloatingIP, CreatedAt: &now,
			Data: map[string]any{"floatingIp": map[string]any{"floating_ip_address": ip, "server_id": "lb:abc", "nic": "net1", "platform_derived": true}}}); err != nil {
			t.Fatal(err)
		}
	}
	f := metrics.NewPVEFetcher(&pve.Service{C: c, ServiceID: "s", PoolPrefix: "pc-"}, repo, db, func() time.Time { return now })
	got, err := f.SearchInstanceInterfaces(ctx, "201")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != appliance.ID+"/lb:abc" {
		t.Fatalf("counters = %+v, want one for the balancer", got)
	}
	for metric, want := range map[string]int64{"network.incoming.bytes": 3, "network.outgoing.bytes": 1} {
		mb, err := f.MeasuresMBForCurrentMonth(ctx, got[0].Metrics[metric], now)
		if err != nil || mb.IntPart() != want {
			t.Errorf("%s = %s MB (%v), want %d", metric, mb, err, want)
		}
	}
}
