package platformlife

import (
	"crypto/ed25519"
	"crypto/rand"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func sshKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " ops@lab"
}

// validInstallation is the lab profile on three nodes: seed .10, VIP .20, nodes .30-.49, MetalLB .200-.220.
func validInstallation(t *testing.T) *Installation {
	t.Helper()
	m := func(node string) Machine { return Machine{Node: node, Cores: 2, MemoryMiB: 4096, DiskGiB: 30} }
	g := func(name, node string) NodeGroup {
		return NodeGroup{Name: name, Nodes: []string{node}, Count: 1, Cores: 2, MemoryMiB: 4096, DiskGiB: 30}
	}
	in := &Installation{Name: "lab", ServiceID: "svc-pve", Spec: Spec{
		PVEAPIURL: "https://10.200.200.2:8006",
		Storage:   Storage{Boot: "local-lvm", ISO: "local", Import: "nas-templates"},
		Network:   Network{Bridge: "vmbr1", Prefix: "10.200.200.0/24", Gateway: "10.200.200.1", DNS: []string{"10.200.200.1"}},
		Seed: SeedSpec{Node: "pve1", Address: "10.200.200.10", Cores: 2, MemoryMiB: 4096, DiskGiB: 30,
			SSHKeys: []string{sshKey(t)}, APISources: []string{"192.168.33.0/24"}},
		Clusters: []ClusterSpec{{Endpoint: "10.200.200.20", NodeRange: "10.200.200.30-10.200.200.49", MetalLBRange: "10.200.200.200-10.200.200.220",
			ControlPlane: m("pve1"), NodeGroups: []NodeGroup{g("system", "pve1"), g("kamaji", "pve2"), g("dbaas", "pve3")}}},
	}}
	in.Normalize()
	return in
}

func TestNormalizeDefaults(t *testing.T) {
	in := validInstallation(t)
	c := in.Spec.Clusters[0]
	if in.ProvisionedBy != ByPortal || in.Layout != LayoutShared || in.Spec.Seed.Mode != SeedVM || in.Spec.Seed.Pool != "proxcloud-seed" ||
		in.Spec.NodeImage.Mode != ImageBOM || c.Name != DefaultClusterName || !slices.Equal(c.Roles, []string{RoleKamaji, RoleDBaaS}) {
		t.Fatalf("defaults: %+v", in)
	}
}

// The validation table: the lab profile and two other valid shapes pass; each mutation fails naming its field.
func TestValidateTable(t *testing.T) {
	if err := validInstallation(t).Validate(); err != nil {
		t.Fatalf("lab profile: %v", err)
	}
	for name, mut := range map[string]func(*Installation){
		"node templates": func(in *Installation) {
			in.Spec.NodeImage = NodeImage{Mode: ImageTemplates, Templates: map[string]int{"pve1": 9001, "pve2": 9002, "pve3": 9003}}
		},
	} {
		in := validInstallation(t)
		mut(in)
		if err := in.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, tc := range []struct {
		name, want string
		mut        func(*Installation)
	}{
		{"name", "name", func(in *Installation) { in.Name = "Lab Site" }},
		{"no provider", "serviceId", func(in *Installation) { in.ServiceID = "" }},
		{"provisioned by", "provisionedBy", func(in *Installation) { in.ProvisionedBy = "ansible" }},
		// Nothing writes or runs an installer installation or an existing seed yet: refused here, not at stage 3.
		{"provisioned by the installer", "provisionedBy installer comes in a later release", func(in *Installation) { in.ProvisionedBy = ByInstaller }},
		{"existing seed", "spec.seed.mode existing comes in a later release", func(in *Installation) {
			in.Spec.Seed.Mode, in.Spec.Seed.Pool = SeedExisting, "proxcloud-system"
		}},
		{"import store is the boot store", "is the boot store", func(in *Installation) { in.Spec.Storage.Import = in.Spec.Storage.Boot }},
		{"separate layout", "later release", func(in *Installation) { in.Layout = LayoutSeparate }},
		{"plain http api", "pveApiUrl", func(in *Installation) { in.Spec.PVEAPIURL = "http://10.200.200.2:8006" }},
		{"api with credentials", "pveApiUrl", func(in *Installation) { in.Spec.PVEAPIURL = "https://root:pw@10.200.200.2:8006" }},
		{"no boot store", "storage.boot", func(in *Installation) { in.Spec.Storage.Boot = "" }},
		{"no import store for the images", "storage.import", func(in *Installation) { in.Spec.Storage.Import = "" }},
		{"store with a slash", "storage.iso", func(in *Installation) { in.Spec.Storage.ISO = "local/../x" }},
		{"bridge", "bridge", func(in *Installation) { in.Spec.Network.Bridge = "vmbr1;reboot" }},
		{"prefix not a network", "prefix", func(in *Installation) { in.Spec.Network.Prefix = "10.200.200.5/24" }},
		{"prefix too small", "prefix", func(in *Installation) { in.Spec.Network.Prefix = "10.200.200.0/30" }},
		{"gateway outside", "gateway", func(in *Installation) { in.Spec.Network.Gateway = "10.9.9.1" }},
		{"seed on the broadcast address", "seed.address", func(in *Installation) { in.Spec.Seed.Address = "10.200.200.255" }},
		{"no dns", "dns", func(in *Installation) { in.Spec.Network.DNS = nil }},
		{"dns name", "dns", func(in *Installation) { in.Spec.Network.DNS = []string{"dns.example"} }},
		{"mtu", "mtu", func(in *Installation) { in.Spec.Network.MTU = 600 }},
		{"seed in the system pool", "seed.pool", func(in *Installation) { in.Spec.Seed.Pool = "proxcloud-system" }},
		{"seed mode", "seed.mode", func(in *Installation) { in.Spec.Seed.Mode = "lxc" }},
		{"seed node", "seed.node", func(in *Installation) { in.Spec.Seed.Node = "pve 1" }},
		{"seed too small", "spec.seed: cores", func(in *Installation) { in.Spec.Seed.Cores = 1 }},
		{"seed disk too small", "spec.seed: cores", func(in *Installation) { in.Spec.Seed.DiskGiB = 6 }},
		{"not an ssh key", "sshKeys[0]", func(in *Installation) { in.Spec.Seed.SSHKeys = []string{"hunter2"} }},
		{"two keys in one entry", "one line", func(in *Installation) {
			in.Spec.Seed.SSHKeys = []string{in.Spec.Seed.SSHKeys[0] + "\n" + in.Spec.Seed.SSHKeys[0]}
		}},
		{"no api source", "apiSources", func(in *Installation) { in.Spec.Seed.APISources = nil }},
		{"api open to all", "narrower than /0", func(in *Installation) { in.Spec.Seed.APISources = []string{"0.0.0.0/0"} }},
		{"two clusters", "exactly one cluster", func(in *Installation) {
			in.Spec.Clusters = append(in.Spec.Clusters, in.Spec.Clusters[0])
		}},
		{"one role only", "roles", func(in *Installation) { in.Spec.Clusters[0].Roles = []string{RoleKamaji} }},
		{"vip on the seed", "same address", func(in *Installation) { in.Spec.Clusters[0].Endpoint = "10.200.200.10" }},
		{"vip in the node range", "inside", func(in *Installation) { in.Spec.Clusters[0].Endpoint = "10.200.200.31" }},
		{"gateway in the metallb range", "inside", func(in *Installation) { in.Spec.Network.Gateway = "10.200.200.201" }},
		{"ranges overlap", "overlap", func(in *Installation) { in.Spec.Clusters[0].MetalLBRange = "10.200.200.45-10.200.200.60" }},
		{"node range reversed", "first-last", func(in *Installation) { in.Spec.Clusters[0].NodeRange = "10.200.200.49-10.200.200.30" }},
		{"node range outside", "first-last", func(in *Installation) { in.Spec.Clusters[0].NodeRange = "10.200.201.30-10.200.201.49" }},
		{"node range too small", "nodeRange holds", func(in *Installation) { in.Spec.Clusters[0].NodeRange = "10.200.200.30-10.200.200.36" }},
		{"a group missing", "exactly the groups", func(in *Installation) { in.Spec.Clusters[0].NodeGroups = in.Spec.Clusters[0].NodeGroups[:2] }},
		{"a sandbox group", "exactly the groups", func(in *Installation) { in.Spec.Clusters[0].NodeGroups[2].Name = "sandbox" }},
		{"an empty machine deployment", "count", func(in *Installation) {
			in.Spec.Clusters[0].NodeGroups[0].Nodes = []string{"pve1", "pve2"}
		}},
		{"a node twice", "listed twice", func(in *Installation) {
			in.Spec.Clusters[0].NodeGroups[0].Nodes, in.Spec.Clusters[0].NodeGroups[0].Count = []string{"pve1", "pve1"}, 2
		}},
		{"group too small", "nodeGroups[1]: cores", func(in *Installation) { in.Spec.Clusters[0].NodeGroups[1].MemoryMiB = 1024 }},
		{"control plane node", "controlPlane.node", func(in *Installation) { in.Spec.Clusters[0].ControlPlane.Node = "" }},
		{"templates with the bom image", "go with mode templates", func(in *Installation) {
			in.Spec.NodeImage.Templates = map[string]int{"pve1": 9001}
		}},
		{"a node without a template", "no template VMID for node pve2", func(in *Installation) {
			in.Spec.NodeImage = NodeImage{Mode: ImageTemplates, Templates: map[string]int{"pve1": 9001, "pve3": 9003}}
		}},
		{"image mode", "nodeImage.mode", func(in *Installation) { in.Spec.NodeImage.Mode = "latest" }},
	} {
		in := validInstallation(t)
		tc.mut(in)
		err := in.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error naming %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestSiteProblems(t *testing.T) {
	site := Site{CIDRPool: "10.100.0.0/16", Sandbox: []string{"10.110.0.0/24"}, Public: []string{"203.0.113.0/24"}, Blocked: []string{"10.200.200.0/25"}}
	if p := validInstallation(t).SiteProblems(site); len(p) != 0 {
		t.Fatalf("a clean site: %v", p)
	}
	for _, tc := range []struct {
		name, want string
		mut        func(*Installation, *Site)
	}{
		{"platform network in the tenant pool", "overlaps cidrPool", func(in *Installation, s *Site) { s.CIDRPool = "10.200.0.0/16" }},
		{"platform network on a public network", "overlaps public networks", func(in *Installation, s *Site) { s.Public = []string{"10.200.200.128/25"} }},
		{"platform network in the sandbox", "overlaps sandboxCidrs", func(in *Installation, s *Site) { s.Sandbox = []string{"10.200.200.0/24"} }},
		{"api source is a tenant network", "tenants must not reach the seed", func(in *Installation, s *Site) {
			in.Spec.Seed.APISources = []string{"10.100.4.0/24"}
		}},
		{"metallb range blocked", "MetalLB range", func(in *Installation, s *Site) { s.Blocked = []string{"10.200.200.0/24"} }},
		{"metallb range on a management network", "MetalLB range", func(in *Installation, s *Site) { s.Mgmt = []string{"10.200.200.192/26"} }},
	} {
		in, s := validInstallation(t), site
		tc.mut(in, &s)
		if p := strings.Join(in.SiteProblems(s), "\n"); !strings.Contains(p, tc.want) {
			t.Errorf("%s: want %q, got %q", tc.name, tc.want, p)
		}
	}
}

// Unfenced names exactly what layer 0 does not cover yet, as the CIDRs to add to blockedCidrs.
func TestUnfenced(t *testing.T) {
	in := validInstallation(t)
	for _, tc := range []struct {
		fenced []string
		want   []string
	}{
		{nil, []string{"10.200.200.10/32", "10.200.200.20/32", "10.200.200.30/31", "10.200.200.32/28", "10.200.200.48/31"}},
		{[]string{"10.200.200.0/24"}, nil},
		{[]string{"10.200.200.0/27"}, []string{"10.200.200.32/28", "10.200.200.48/31"}},                     // seed, VIP and .30-.31 covered
		{[]string{"10.200.200.0/28", "10.200.200.16/28"}, []string{"10.200.200.32/28", "10.200.200.48/31"}}, // two adjacent networks
		{[]string{"10.200.200.40/29"}, []string{"10.200.200.10/32", "10.200.200.20/32", "10.200.200.30/31", "10.200.200.32/29", "10.200.200.48/31"}},
	} {
		if got := in.Unfenced(tc.fenced); !slices.Equal(got, tc.want) {
			t.Errorf("fenced %v: got %v, want %v", tc.fenced, got, tc.want)
		}
	}
}

// Fixed fields: the provider always; the path, the layout, the network, the seed's mode, pool and place and the
// cluster's identity once a run succeeded. Sizes and counts stay changeable.
func TestFixedChanges(t *testing.T) {
	old := validInstallation(t)
	next := func(mut func(*Installation)) *Installation {
		n := validInstallation(t)
		n.Spec.Seed.SSHKeys = old.Spec.Seed.SSHKeys
		mut(n)
		return n
	}
	if f := FixedChanges(old, next(func(n *Installation) { n.Spec.Network.Gateway = "10.200.200.254"; n.Name = "lab2" })); len(f) != 0 {
		t.Fatalf("before the first run, network and name change: %v", f)
	}
	if f := FixedChanges(old, next(func(n *Installation) { n.ServiceID = "svc-other"; n.Layout = LayoutSeparate })); !slices.Equal(f, []string{"serviceId"}) {
		t.Fatalf("always fixed: %v", f)
	}
	// A draft made with the wrong path and seed mode can be corrected: existing in proxcloud-system to a portal seed,
	// whose pool Normalize fills.
	draft := next(func(n *Installation) {
		n.ProvisionedBy, n.Spec.Seed.Mode, n.Spec.Seed.Pool, n.Spec.NodeImage.Mode = ByInstaller, SeedExisting, "proxcloud-system", ImageTemplates
		n.Spec.NodeImage.Templates = map[string]int{"pve1": 9000, "pve2": 9000, "pve3": 9000}
	})
	fixed := next(func(n *Installation) { n.Spec.Seed.Pool = "" })
	fixed.Normalize()
	if f := FixedChanges(draft, fixed); len(f) != 0 || fixed.Validate() != nil {
		t.Fatalf("a draft's path and seed mode: %v %v", f, fixed.Validate())
	}
	old.Status.BOM = &BOMRef{Release: "0.7.0-dev", SHA256: "x"}
	for _, tc := range []struct {
		want []string
		mut  func(*Installation)
	}{
		{nil, func(n *Installation) {
			n.Spec.Seed.Cores, n.Spec.Clusters[0].NodeGroups[1].Count, n.Spec.Clusters[0].MetalLBRange = 4, 2, "10.200.200.200-10.200.200.230"
			n.Spec.Clusters[0].NodeGroups[1].Nodes = []string{"pve2", "pve3"}
		}},
		{[]string{"provisionedBy", "layout"}, func(n *Installation) { n.ProvisionedBy, n.Layout = ByInstaller, LayoutSeparate }},
		{[]string{"name"}, func(n *Installation) { n.Name = "lab2" }},
		{[]string{"spec.seed.mode", "spec.seed.pool"}, func(n *Installation) { n.Spec.Seed.Mode, n.Spec.Seed.Pool = SeedExisting, "proxcloud-system" }},
		{[]string{"spec.network"}, func(n *Installation) { n.Spec.Network.DNS = []string{"1.1.1.1"} }},
		{[]string{"spec.seed.node", "spec.seed.address"}, func(n *Installation) { n.Spec.Seed.Node, n.Spec.Seed.Address = "pve2", "10.200.200.11" }},
		{[]string{"spec.clusters[0].name", "spec.clusters[0].endpoint"}, func(n *Installation) {
			n.Spec.Clusters[0].Name, n.Spec.Clusters[0].Endpoint = "pc-other", "10.200.200.21"
		}},
		{[]string{"spec.clusters[0].nodeGroups"}, func(n *Installation) { n.Spec.Clusters[0].NodeGroups[2].Name = "sandbox" }},
		{[]string{"spec.clusters"}, func(n *Installation) { n.Spec.Clusters = nil }},
	} {
		if f := FixedChanges(old, next(tc.mut)); !slices.Equal(f, tc.want) {
			t.Errorf("after the first run: got %v, want %v", f, tc.want)
		}
	}
}
