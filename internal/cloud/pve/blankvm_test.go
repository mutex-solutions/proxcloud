package pve

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// The real shapes, read on PVE 9.2 after POST /nodes/{n}/qemu with only vmid and pool.
const blankRowJSON = `{"id":"qemu/%d","type":"qemu","vmid":%d,"node":"pve1","name":"VM %d","status":"stopped","pool":"st-p1","maxcpu":1,"maxmem":536870912,"maxdisk":0}`

func blankConfigJSON(ctime int64) string {
	return fmt.Sprintf(`{"data":{"boot":" ","digest":"b4ca","meta":"creation-qemu=11.1.3,ctime=%d","smbios1":"uuid=529f","vmgenid":"3efe"}}`, ctime)
}

func TestBlankRowAndConfig(t *testing.T) {
	blank := ClusterResource{Type: "qemu", VMID: 7, Name: "VM 7", Status: "stopped"}
	if !blankRow(blank) {
		t.Fatal("the blank row must match")
	}
	for name, mutate := range map[string]func(r *ClusterResource){
		"named":        func(r *ClusterResource) { r.Name = "web" },
		"another's id": func(r *ClusterResource) { r.Name = "VM 8" },
		"tagged":       func(r *ClusterResource) { r.Tags = "proxcloud" },
		"has a disk":   func(r *ClusterResource) { r.MaxDisk = 1 },
		"running":      func(r *ClusterResource) { r.Status = "running" },
		"paused":       func(r *ClusterResource) { r.Status = "paused" },
		"template":     func(r *ClusterResource) { r.Template = 1 },
		"container":    func(r *ClusterResource) { r.Type = "lxc" },
	} {
		r := blank
		mutate(&r)
		if blankRow(r) {
			t.Errorf("row %s must not match", name)
		}
	}

	if !blankConfig(VMConfig{"boot": " ", "digest": "d", "meta": "ctime=1", "smbios1": "uuid=u", "vmgenid": "g"}) {
		t.Fatal("the blank config must match")
	}
	for _, key := range []string{"name", "tags", "scsi0", "unused0", "ide2", "net0", "hostpci0", "memory", "cores", "lock", "parent", "protection", "description", "agent"} {
		if blankConfig(VMConfig{"meta": "ctime=1", key: "x"}) {
			t.Errorf("a config with %s must not match", key)
		}
	}
	if blankConfig(VMConfig{"boot": "order=scsi0"}) {
		t.Error("a boot order must not match")
	}
}

func TestServerSyncSkipsBlankVMs(t *testing.T) {
	const blank, customer, unreadable = 201, 202, 203
	now := time.Now().Unix()
	_, c := fakePVE(t, fakeRoutes{
		"GET /api2/json/cluster/resources": say(fmt.Sprintf(`{"data":[`+blankRowJSON+`,`+blankRowJSON+`,`+blankRowJSON+`]}`,
			blank, blank, blank, customer, customer, customer, unreadable, unreadable, unreadable)),
		fmt.Sprintf("GET /api2/json/nodes/pve1/qemu/%d/config", blank): say(blankConfigJSON(now)),
		// The row reads blank, the config has a data disk off the boot order: a customer's, listed as ever.
		fmt.Sprintf("GET /api2/json/nodes/pve1/qemu/%d/config", customer):   say(`{"data":{"scsi1":"local-zfs:vm-202-disk-0,size=8G","meta":"ctime=1"}}`),
		fmt.Sprintf("GET /api2/json/nodes/pve1/qemu/%d/config", unreadable): refuse,
	})
	s := &Service{C: c, PoolPrefix: "st-"}

	for name, p := range map[string]*ServerSyncProvider{
		"full": s.ServerSyncProvider("r", "p1", nil),
	} {
		rs, err := p.List(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(rs) != 1 || rs[0].ExternalID != "202" {
			t.Fatalf("%s: listed %+v, want only the customer's 202", name, rs)
		}
		if _, kept := p.unread[unreadable]; !kept {
			t.Fatalf("%s: a config that could not be read is an unknown: its cached row stays", name)
		}
		if _, kept := p.unread[blank]; kept {
			t.Fatalf("%s: a blank VM's cached row must go", name)
		}
	}

	page, err := c.ClusterResources(context.Background(), "vm")
	if err != nil {
		t.Fatal(err)
	}
	p := s.SampledServerProvider("r", "p1", page, nil)
	rs, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].ExternalID != "202" {
		t.Fatalf("sampled: listed %+v, want only the customer's 202", rs)
	}
	if _, kept := p.unread[unreadable]; !kept {
		t.Fatal("sampled: a config that could not be read is an unknown")
	}
}

func TestRemoveBlankVMs(t *testing.T) {
	old := time.Now().Add(-2 * blankVMGrace).Unix()
	fresh := time.Now().Unix()
	deleteRoute := func(calls *int, status string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			*calls++
			fmt.Fprint(w, status)
		}
	}
	const upid = `{"data":"UPID:pve1:0:0:0:qmdestroy:201:u:"}`
	tests := []struct {
		name     string
		config   func(http.ResponseWriter, *http.Request)
		del      string
		lockBusy bool
		want     int
		wantDel  int
		wantErr  bool
	}{
		{name: "past the grace", config: say(blankConfigJSON(old)), del: upid, want: 1, wantDel: 1},
		{name: "inside the grace", config: say(blankConfigJSON(fresh))},
		{name: "configured", config: say(fmt.Sprintf(`{"data":{"meta":"ctime=%d","tags":"pc-holder;proxcloud","name":"pc-holder-pve1"}}`, old))},
		{name: "holds a disk", config: say(fmt.Sprintf(`{"data":{"meta":"ctime=%d","unused0":"local-zfs:vm-201-disk-0"}}`, old))},
		{name: "no creation time", config: say(`{"data":{"digest":"d"}}`)},
		{name: "config unreadable", config: refuse},
		{name: "lock busy", config: say(blankConfigJSON(old)), lockBusy: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dels := 0
			_, c := fakePVE(t, fakeRoutes{
				"GET /api2/json/cluster/resources":                                        say(fmt.Sprintf(`{"data":[`+blankRowJSON+`]}`, 201, 201, 201)),
				"GET /api2/json/nodes/pve1/qemu/201/config":                               tt.config,
				"DELETE /api2/json/nodes/pve1/qemu/201":                                   deleteRoute(&dels, tt.del),
				"GET /api2/json/nodes/pve1/tasks/UPID:pve1:0:0:0:qmdestroy:201:u:/status": say(`{"data":{"status":"stopped","exitstatus":"OK"}}`),
			})
			s := &Service{C: c, PoolPrefix: "st-"}
			if tt.lockBusy {
				s.HolderLock = func(context.Context, string, time.Duration, time.Duration) (func(), bool, error) {
					return nil, false, nil
				}
			}
			n, err := s.RemoveBlankVMs(context.Background(), "p1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err %v", err)
			}
			if n != tt.want || dels != tt.wantDel {
				t.Fatalf("removed %d, deletes %d; want %d, %d", n, dels, tt.want, tt.wantDel)
			}
		})
	}

	t.Run("another pool's blank VM is not this project's", func(t *testing.T) {
		dels := 0
		_, c := fakePVE(t, fakeRoutes{
			"GET /api2/json/cluster/resources":          say(fmt.Sprintf(`{"data":[`+blankRowJSON+`]}`, 201, 201, 201)),
			"GET /api2/json/nodes/pve1/qemu/201/config": say(blankConfigJSON(old)),
			"DELETE /api2/json/nodes/pve1/qemu/201":     deleteRoute(&dels, upid),
		})
		s := &Service{C: c, PoolPrefix: "st-"}
		if n, err := s.RemoveBlankVMs(context.Background(), "p2"); err != nil || n != 0 || dels != 0 {
			t.Fatalf("removed %d, deletes %d, err %v", n, dels, err)
		}
	})

	t.Run("a failed destroy is left for the next sync", func(t *testing.T) {
		_, c := fakePVE(t, fakeRoutes{
			"GET /api2/json/cluster/resources":          say(fmt.Sprintf(`{"data":[`+blankRowJSON+`]}`, 201, 201, 201)),
			"GET /api2/json/nodes/pve1/qemu/201/config": say(blankConfigJSON(old)),
			"DELETE /api2/json/nodes/pve1/qemu/201":     refuse,
		})
		s := &Service{C: c, PoolPrefix: "st-"}
		if n, err := s.RemoveBlankVMs(context.Background(), "p1"); err != nil || n != 0 {
			t.Fatalf("removed %d, err %v", n, err)
		}
	})
}

// A hung destroy must not stall the sequential sync.
func TestRemoveBlankVMsBoundsAHungDestroy(t *testing.T) {
	lostTaskWait = 50 * time.Millisecond
	t.Cleanup(func() { lostTaskWait = time.Minute })
	old := time.Now().Add(-2 * blankVMGrace).Unix()
	_, c := fakePVE(t, fakeRoutes{
		"GET /api2/json/cluster/resources":                                        say(fmt.Sprintf(`{"data":[`+blankRowJSON+`]}`, 201, 201, 201)),
		"GET /api2/json/nodes/pve1/qemu/201/config":                               say(blankConfigJSON(old)),
		"DELETE /api2/json/nodes/pve1/qemu/201":                                   say(`{"data":"UPID:pve1:0:0:0:qmdestroy:201:u:"}`),
		"GET /api2/json/nodes/pve1/tasks/UPID:pve1:0:0:0:qmdestroy:201:u:/status": say(`{"data":{"status":"running"}}`),
	})
	s := &Service{C: c, PoolPrefix: "st-"}
	start := time.Now()
	n, err := s.RemoveBlankVMs(context.Background(), "p1")
	if err != nil || n != 0 {
		t.Fatalf("removed %d, err %v", n, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("waited %s", d)
	}
}

// Only what is as blank as a create leaves it goes: a nameless VM holding a NIC or a description is someone's.
func TestDropLostCreateUsesTheBlankTest(t *testing.T) {
	lostTaskWait = 5 * time.Second
	t.Cleanup(func() { lostTaskWait = time.Minute })
	for name, c := range map[string]struct {
		cfg  string
		dels int
	}{
		"blank":          {blankConfigJSON(time.Now().Unix()), 1},
		"nameless + NIC": {`{"data":{"meta":"ctime=1","net0":"virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0"}}`, 0},
		"description":    {`{"data":{"meta":"ctime=1","description":"keep me"}}`, 0},
		"memory set":     {`{"data":{"meta":"ctime=1","memory":"2048"}}`, 0},
	} {
		t.Run(name, func(t *testing.T) {
			dels := 0
			_, pc := fakePVE(t, fakeRoutes{
				"GET /api2/json/cluster/resources":          say(fmt.Sprintf(`{"data":[`+blankRowJSON+`]}`, 201, 201, 201)),
				"GET /api2/json/nodes/pve1/qemu/201/config": say(c.cfg),
				"DELETE /api2/json/nodes/pve1/qemu/201": func(w http.ResponseWriter, _ *http.Request) {
					dels++
					fmt.Fprint(w, `{"data":"UPID:pve1:0:0:0:qmdestroy:201:u:"}`)
				},
				"GET /api2/json/nodes/pve1/tasks/UPID:pve1:0:0:0:qmdestroy:201:u:/status": say(`{"data":{"status":"stopped","exitstatus":"OK"}}`),
			})
			s := &Service{C: pc, PoolPrefix: "st-"}
			s.dropLostCreate(context.Background(), "p1", 201)
			if dels != c.dels {
				t.Fatalf("deletes %d, want %d", dels, c.dels)
			}
		})
	}
}
