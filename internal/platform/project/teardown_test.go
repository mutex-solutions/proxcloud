package project

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestDeletionOrder pins the dependency-safe teardown ordering: dependents delete before the things
// they reference, so a single ordered sweep rarely hits "resource still in use".
func TestDeletionOrder(t *testing.T) {
	lt := func(a, b string) {
		if deletionOrder(a) >= deletionOrder(b) {
			t.Errorf("%s (%d) should sort before %s (%d)", a, deletionOrder(a), b, deletionOrder(b))
		}
	}
	lt(cloud.TypeLoadBalancer, cloud.TypeServer) // composites before instances
	lt(cloud.TypeServer, cloud.TypeFloatingIP)   // instance releases its FIP
	lt(cloud.TypeFloatingIP, cloud.TypePort)
	lt(cloud.TypePort, cloud.TypeSubnet)
	lt(cloud.TypeSubnet, cloud.TypeNetwork)
	lt(cloud.TypeNetwork, cloud.TypeRouter)       // network before router (router removed last)
	lt(cloud.TypeVolume, cloud.TypeSubnet)        // volumes before network teardown
	lt(cloud.TypeServer, cloud.TypeSecurityGroup) // leaf types (SG/keypair/image) go last
	lt(cloud.TypeRouter, cloud.TypeSecurityGroup)
}

func TestSortCloudResourcesForDeletionPutsServerBeforeBootVolume(t *testing.T) {
	resources := []cloud.CloudResource{
		{ExternalID: "root-volume", Type: cloud.TypeVolume},
		{ExternalID: "server", Type: cloud.TypeServer},
	}
	SortCloudResourcesForDeletion(resources)
	if resources[0].Type != cloud.TypeServer || resources[1].Type != cloud.TypeVolume {
		t.Fatalf("deletion order = %s, %s; want SERVER before VOLUME", resources[0].Type, resources[1].Type)
	}
}

// A pass that needs a re-run must stay where the deletion job looks: DELETED is never looked at again.
func TestTeardownOutcome(t *testing.T) {
	now := time.Now().UTC()
	p := &Project{Status: StatusDeleteInProgress}
	teardownOutcome(p, false, now)
	if p.Status != StatusDeleted {
		t.Fatalf("a clean pass: %s", p.Status)
	}
	p = &Project{Status: StatusDeleteInProgress} // the admin's delete-now sets no schedule
	teardownOutcome(p, true, now)
	if p.Status != StatusDeleteInProgress || p.ScheduledForDeletionAt == nil || !p.ScheduledForDeletionAt.Before(now) {
		t.Fatalf("a retry: %+v", p)
	}
	past := now.Add(-time.Hour)
	p = &Project{Status: StatusDeleted, ScheduledForDeletionAt: &past}
	teardownOutcome(p, true, now)
	if p.Status != StatusDeleteInProgress || !p.ScheduledForDeletionAt.Equal(past) {
		t.Fatalf("a retry keeps its schedule: %+v", p)
	}
}

func TestProjectDeleting(t *testing.T) {
	for status, want := range map[string]bool{
		StatusEnabled: false, StatusDisabled: false, "": false,
		StatusScheduledForDeletion: true, StatusDeleteInProgress: true, StatusDeleted: true,
	} {
		if got := (&Project{Status: status}).Deleting(); got != want {
			t.Errorf("%q = %v", status, got)
		}
	}
}

// The raw client error carries the server's internal address; a tenant gets neither that nor the server's own text.
func TestZoneErrHidesTheBackend(t *testing.T) {
	raw := errors.New(`Get "https://dns.internal:8081/api/v1/servers/localhost/zones": dial tcp 10.0.0.5:8081: connect: connection refused`)
	var he *httpx.HTTPError
	if !errors.As(zoneErr(raw), &he) || he.Status != http.StatusBadGateway {
		t.Fatalf("%+v", he)
	}
	for _, leak := range []string{"dns.internal", "8081", "10.0.0.5", "localhost"} {
		if strings.Contains(he.Msg, leak) {
			t.Fatalf("%q leaks %q", he.Msg, leak)
		}
	}
}

func TestZoneRecordEvent(t *testing.T) {
	u, p := &user.User{Sub: "s"}, &Project{ID: "p", Name: "n"}
	cr := &cloud.CloudResource{ID: "c", Type: cloud.TypeDNSZone, ExternalID: "example.org."}
	ev := newZoneRecordEvent(u, p, "SET_RECORD", cr, map[string]any{"name": "mail", "type": "MX", "ttl": 60}, []string{"10 mx.evil.example."})
	rec, _ := ev.ResourceMetadata["record"].(map[string]any)
	if ev.Action != "CLOUD_RESOURCE_ACTION" || ev.ResourceMetadata["verb"] != "SET_RECORD" || rec["name"] != "mail" || rec["type"] != "MX" {
		t.Fatalf("%+v", ev)
	}
	if got, _ := rec["records"].([]string); len(got) != 1 || got[0] != "10 mx.evil.example." {
		t.Fatalf("the new values must be on the event: %+v", rec)
	}
	ev = newZoneRecordEvent(u, p, "DELETE_RECORD", cr, map[string]any{"name": "mail", "type": "MX"}, nil)
	if rec, _ := ev.ResourceMetadata["record"].(map[string]any); rec["name"] != "mail" || rec["records"] != nil {
		t.Fatalf("%+v", rec)
	}
}
