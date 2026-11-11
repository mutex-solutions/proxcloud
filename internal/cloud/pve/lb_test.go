package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestLBSpecValidate(t *testing.T) {
	good := func() LBSpec {
		return LBSpec{
			Listeners: []LBListener{{Port: 80, Pool: "web"}},
			Pools:     []LBPool{{Name: "web", Members: []LBMember{{ServerID: "101", Port: 8080}}}},
		}
	}
	s := good()
	if msg := s.Validate(); msg != "" {
		t.Fatal(msg)
	}
	if s.Pools[0].Protocol != "http" || s.Pools[0].Algorithm != "roundrobin" || s.Pools[0].Members[0].Weight != 1 {
		t.Fatalf("defaults not filled: %+v", s.Pools[0])
	}
	bad := map[string]func(*LBSpec){
		"newline in pool name": func(s *LBSpec) { s.Pools[0].Name = "web\n  bind :22"; s.Listeners[0].Pool = s.Pools[0].Name },
		"space in pool name":   func(s *LBSpec) { s.Pools[0].Name = "a b" },
		"protocol":             func(s *LBSpec) { s.Pools[0].Protocol = "http\nglobal" },
		"algorithm":            func(s *LBSpec) { s.Pools[0].Algorithm = "first" },
		"port":                 func(s *LBSpec) { s.Listeners[0].Port = 70000 },
		"dangling pool":        func(s *LBSpec) { s.Listeners[0].Pool = "other" },
		"member id":            func(s *LBSpec) { s.Pools[0].Members[0].ServerID = "101 check" },
		"member port":          func(s *LBSpec) { s.Pools[0].Members[0].Port = 0 },
		"weight":               func(s *LBSpec) { s.Pools[0].Members[0].Weight = 300 },
		"twice":                func(s *LBSpec) { s.Pools[0].Members = append(s.Pools[0].Members, s.Pools[0].Members[0]) },
	}
	for name, breakIt := range bad {
		s := good()
		breakIt(&s)
		if s.Validate() == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	// An address in a request is dropped: only the platform fills it in.
	s = good()
	s.Pools[0].Members[0].Address = "203.0.113.9"
	if msg := s.Validate(); msg != "" || s.Pools[0].Members[0].Address != "" {
		t.Fatalf("request address kept: %+v %q", s.Pools[0].Members[0], msg)
	}
}

// The renderer sees stored rows, so it must be safe on its own, even with a row validation never saw.
func TestRenderHAProxyNoInjection(t *testing.T) {
	evil := "x\n  bind :22\nglobal\n"
	cfg := RenderHAProxy(LBSpec{
		Listeners: []LBListener{{Port: 80, Pool: evil}},
		Pools: []LBPool{{
			Name: evil, Protocol: "tcp\n" + evil, Algorithm: "source\n" + evil,
			Members: []LBMember{
				{ServerID: "101", Port: 8080, Address: "10.0.0.5"},
				{ServerID: "102", Port: 8080, Address: "10.0.0.6\nserver x 1.1.1.1:1"},
				{ServerID: "103\n" + evil, Port: 8080, Address: "10.0.0.7"},
				{ServerID: "104", Port: 8080, Address: "10.0.0.8", Missing: true},
			},
		}},
	})
	for _, bad := range []string{":22", "1.1.1.1", "10.0.0.7", "10.0.0.8", evil} {
		if strings.Contains(cfg, bad) {
			t.Errorf("config holds %q:\n%s", bad, cfg)
		}
	}
	if !strings.Contains(cfg, "server m101p8080 10.0.0.5:8080 check weight 1") {
		t.Errorf("good member missing:\n%s", cfg)
	}
}

func TestParseHAProxyStat(t *testing.T) {
	spec := LBSpec{Pools: []LBPool{{Name: "web", Members: []LBMember{{ServerID: "101", Port: 80}, {ServerID: "102", Port: 80}, {ServerID: "103", Port: 80}}}}}
	out := "# pxname,svname,status,weight\nl80,FRONTEND,OPEN,\np0,m101p80,UP,1\np0,m102p80,DOWN,1\np0,BACKEND,UP,1\n"
	got, err := ParseHAProxyStat(out, spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"UP", "DOWN", ""}
	for i, h := range got {
		if h.Status != want[i] || h.Pool != "web" {
			t.Errorf("member %d: %+v", i, h)
		}
	}
}

// The config replaces Debian's, so it has to say itself that HAProxy does not stay root on the public NIC.
func TestRenderHAProxyDropsRoot(t *testing.T) {
	cfg := RenderHAProxy(LBSpec{})
	for _, want := range []string{"\n  user haproxy\n", "\n  group haproxy\n", "\n  chroot /var/lib/haproxy\n"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config lacks %q:\n%s", want, cfg)
		}
	}
}

// The largest spec Validate takes has to be caught by the caller's check: it renders past what PVE can write.
func TestLBMaxConfigIsReachable(t *testing.T) {
	var s LBSpec
	for i := 0; i < lbMaxPools; i++ {
		p := LBPool{Name: "p" + strconv.Itoa(i), Protocol: "http", Algorithm: "roundrobin"}
		for j := 0; j < lbMaxMembers; j++ {
			p.Members = append(p.Members, LBMember{ServerID: strconv.Itoa(100000 + j), Port: 10000 + j, Address: "10.100.200.250", Weight: 256})
		}
		s.Pools = append(s.Pools, p)
	}
	if msg := s.Validate(); msg != "" {
		t.Fatal(msg)
	}
	for i := range s.Pools { // Validate clears what the platform fills in
		for j := range s.Pools[i].Members {
			s.Pools[i].Members[j].Address = "10.100.200.250"
		}
	}
	if n := len(RenderHAProxy(s)); n <= LBMaxConfig {
		t.Fatalf("largest spec renders to %d, under the cap of %d", n, LBMaxConfig)
	}
}

const lbCreatePath = "/api2/json/nodes/pve1/qemu/500"

// lbCreateCluster fakes a create up to the read-back, which fails: the clone and every step before the boot work.
func lbCreateCluster(t *testing.T, del, resources func(http.ResponseWriter, *http.Request)) *Service {
	t.Helper()
	task := say(`{"data":"UPID:pve1:0:0:0:qm:500:u:"}`)
	_, c := fakePVE(t, fakeRoutes{
		"GET /api2/json/cluster/nextid":                                    say(`{"data":"500"}`),
		"POST /api2/json/nodes/pve1/qemu/9000/clone":                       task,
		"GET /api2/json/nodes/pve1/tasks/UPID:pve1:0:0:0:qm:500:u:/status": say(`{"data":{"status":"stopped","exitstatus":"OK"}}`),
		"GET " + lbCreatePath + "/config":                                  say(`{"data":{"scsi0":"local-zfs:vm-500-disk-0,size=10G"}}`),
		"POST " + lbCreatePath + "/config":                                 task,
		"PUT " + lbCreatePath + "/config":                                  task,
		"PUT " + lbCreatePath + "/resize":                                  task,
		"GET " + lbCreatePath + "/firewall/rules":                          say(`{"data":[]}`),
		"PUT " + lbCreatePath + "/firewall/options":                        say(`{"data":null}`),
		"GET /api2/json/cluster/sdn/vnets":                                 say(`{"data":[]}`),
		"GET /api2/json/cluster/sdn/zones":                                 say(`{"data":[]}`),
		"GET " + lbCreatePath + "/firewall/ipset":                          say(`{"data":[]}`),
		"POST " + lbCreatePath + "/status/start":                           task,
		"POST " + lbCreatePath + "/status/stop":                            task,
		"GET " + lbCreatePath + "/status/current":                          refuse,
		"DELETE " + lbCreatePath:                                           del,
		"GET /api2/json/cluster/resources":                                 resources,
	})
	return &Service{C: c, PoolPrefix: "tp-"}
}

// A read-back that fails must not leave a running VM behind a failed create: the caller frees its addresses when told
// the VM is gone, and only then.
func TestCreateServerFailedReadBack(t *testing.T) {
	task := say(`{"data":"UPID:pve1:0:0:0:qm:500:u:"}`)
	listed := say(`{"data":[{"id":"qemu/500","type":"qemu","vmid":500,"node":"pve1"}]}`)
	unlisted := say(`{"data":[]}`)
	req := CreateServerReq{ProjectID: "p1", Name: "a", TemplateVMID: 9000, TemplateNode: "pve1", Spec: ServerSpec{Slug: "s", VCPUs: 1, RAMMb: 512}}
	for _, c := range []struct {
		name           string
		del, resources func(http.ResponseWriter, *http.Request)
		left           bool
	}{
		{"destroy task ended well", task, listed, false},
		{"destroy answer lost, cluster no longer lists it", refuse, unlisted, false},
		{"destroy refused, still listed", refuse, listed, true},
		{"destroy refused, cluster unreadable", refuse, refuse, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := lbCreateCluster(t, c.del, c.resources).CreateServer(context.Background(), req)
			var left *VMLeftError
			if err == nil || !strings.Contains(err.Error(), "read back") || errors.As(err, &left) != c.left {
				t.Fatalf("err = %v, want a failed read-back with left = %v", err, c.left)
			}
		})
	}
}

// Every route a tenant can name a VM in refuses a balancer's, and none offers it.
func TestApplianceIsNoTarget(t *testing.T) {
	writes := map[string][]url.Values{}
	const tagged = `{"data":{"status":"running","tags":"proxcloud;pc-lb-abc"}}`
	s := volCluster(t, cfgLocalUnused, writes, fakeRoutes{
		"GET /api2/json/nodes/pve1/qemu/102/status/current": say(tagged),
	})
	ctx := context.Background()
	var ce *ConflictError
	if _, err := s.AddVolume(ctx, "p1", "102", 1, "local-zfs", 0); !errors.As(err, &ce) {
		t.Errorf("AddVolume: %v", err)
	}
	if _, err := s.CreateImage(ctx, "p1", "102", "img"); !errors.As(err, &ce) {
		t.Errorf("CreateImage: %v", err)
	}
	if err := s.AttachVolume(ctx, "p1", 101, "local-zfs:vm-101-disk-1", "102"); !errors.As(err, &ce) {
		t.Errorf("AttachVolume: %v", err)
	}
	if len(writes) != 0 {
		t.Errorf("a refusal wrote: %v", writes)
	}
}

func TestAttachTargetsHideAppliance(t *testing.T) {
	writes := map[string][]url.Values{}
	s := volCluster(t, cfgLocalUnused, writes, fakeRoutes{
		"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"id":"qemu/101","type":"qemu","vmid":101,"name":"a","node":"pve1","pool":"tp-p1"},
				{"id":"qemu/102","type":"qemu","vmid":102,"name":"lb","node":"pve1","pool":"tp-p1","tags":"proxcloud;pc-lb-abc"}]}`)
		},
	})
	vms, err := s.AttachTargets(context.Background(), "p1", 101, "local-zfs:vm-101-disk-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 || vms[0].VMID != 101 {
		t.Fatalf("targets = %+v", vms)
	}
}
