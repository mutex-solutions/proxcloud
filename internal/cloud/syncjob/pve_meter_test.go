package syncjob

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

func TestIsVMTask(t *testing.T) {
	cases := []struct {
		typ, id string
		want    bool
	}{
		{"qmstart", "105", true},
		{"qmdestroy", "105", true},
		{"resize", "105", true},
		{"hastart", "105", true},
		{"vncproxy", "105", false},
		{"qmtermproxy", "105", false},
		{"imgdel", "999999@local-zfs", false},
		{"srvreload", "networking", false},
		{"aptupdate", "", false},
	}
	for _, c := range cases {
		if got := isVMTask(&pve.NodeTask{Type: c.typ, ID: c.id}); got != c.want {
			t.Errorf("%s/%s = %v", c.typ, c.id, got)
		}
	}
}

func TestNextTaskCursor(t *testing.T) {
	done := func(start int64) pve.NodeTask {
		return pve.NodeTask{Type: "qmstart", ID: "1", StartTime: start, EndTime: start + 1}
	}
	if got := nextTaskCursor(nil, 50); got != 50 {
		t.Errorf("no tasks: %d", got)
	}
	if got := nextTaskCursor([]pve.NodeTask{done(90), done(70)}, 50); got != 90 {
		t.Errorf("newest start: %d", got)
	}
	running := pve.NodeTask{Type: "qmclone", ID: "9", StartTime: 60}
	if got := nextTaskCursor([]pve.NodeTask{done(90), running}, 50); got != 60 {
		t.Errorf("held back to the running task: %d", got)
	}
	shell := pve.NodeTask{Type: "vncshell", ID: "", StartTime: 60}
	if got := nextTaskCursor([]pve.NodeTask{done(90), shell}, 50); got != 90 {
		t.Errorf("a long-lived non-VM task must not hold the cursor: %d", got)
	}
}

// The allocation time is part of the row's identity: the same address handed out again is a new
// row and bill item, and the row bills from the allocation, not the first sample after it.
func TestFloatingIPProvider(t *testing.T) {
	first := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	alloc := func(at time.Time) project.PublicIP {
		return project.PublicIP{NetworkID: "vmbr1", IP: "203.0.113.7", ServerID: "105", NIC: "net1", CreatedAt: &at}
	}
	list := func(allocs ...project.PublicIP) *floatingIPProvider {
		return &floatingIPProvider{region: "r", projectID: "p", serviceID: "s", list: func(context.Context, string, string) ([]project.PublicIP, error) {
			return allocs, nil
		}}
	}
	vnet := project.PublicIP{NetworkID: "v1", IP: "10.0.0.5", ServerID: "105", NIC: "net0", CreatedAt: &first}
	a, err := list(alloc(first), vnet).List(context.Background())
	if err != nil || len(a) != 1 {
		t.Fatalf("%v %+v", err, a)
	}
	b, _ := list(alloc(second)).List(context.Background())
	if a[0].ExternalID == b[0].ExternalID {
		t.Fatal("a re-allocated address must be a new row")
	}
	if !a[0].CreatedAt.Equal(first) {
		t.Fatalf("createdAt = %v", a[0].CreatedAt)
	}
	fip := a[0].Data["floatingIp"].(map[string]any)
	if fip["server_id"] != "105" || fip["nic"] != "net1" || fip["floating_ip_address"] != "203.0.113.7" || fip["platform_derived"] != true {
		t.Fatalf("data = %+v", fip)
	}
	if rows, err := (&floatingIPProvider{}).List(context.Background()); err != nil || rows != nil {
		t.Fatal("no lister means no rows")
	}
}

func TestOrphanIPs(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	alloc := func(server, nic string, age time.Duration) project.PublicIP {
		return project.PublicIP{ServerID: server, NIC: nic, CreatedAt: at(-age)}
	}
	allocs := []project.PublicIP{
		alloc("100", "net1", time.Hour),   // VM listed, row older: healthy
		alloc("101", "net1", time.Hour),   // VM gone: orphan
		alloc("101", "net0", time.Hour),   // same VM's vnet address: orphan too
		alloc("102", "net1", time.Minute), // not listed yet, inside the grace: keep
		alloc("103", "net1", time.Hour),   // listed, but the row is younger: the vmid was reused
		alloc("104", "net0", time.Minute), // allocated before its VM's row: never a reuse
		// A load balancer's public NIC, plugged during the build: same. Lab: this released a live address.
		{ServerID: "105", NIC: "net1", BeforeRow: true, CreatedAt: at(-time.Minute)},
	}
	listed := map[string]bool{"100": true, "103": true, "104": true, "105": true}
	rows := map[string]*cloud.CloudResource{
		"100": {CreatedAt: at(-2 * time.Hour)},
		"103": {CreatedAt: at(-10 * time.Minute)},
		"104": {CreatedAt: at(-time.Second)},
		"105": {CreatedAt: at(-time.Second)},
	}
	var got []string
	for _, o := range orphanIPs(allocs, listed, rows, now) {
		got = append(got, o.ServerID+"/"+o.NIC)
	}
	if strings.Join(got, " ") != "101/net1 101/net0 103/net1" {
		t.Fatalf("got %v", got)
	}
}
