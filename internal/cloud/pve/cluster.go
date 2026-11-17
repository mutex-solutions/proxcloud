package pve

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// ClusterResource is one row of GET /cluster/resources — the sync workhorse: a
// single call returns every VM, storage, node and pool in the cluster.
type ClusterResource struct {
	ID       string  `json:"id"`   // "qemu/101", "storage/pve1/local-zfs", "node/pve1", "/pool/tp-abc"
	Type     string  `json:"type"` // "qemu" | "lxc" | "storage" | "node" | "sdn" | "pool"
	Node     string  `json:"node"`
	VMID     int     `json:"vmid"`
	Name     string  `json:"name"`
	Status   string  `json:"status"` // qemu: "running"|"stopped"; node: "online"; storage: "available"
	Pool     string  `json:"pool"`
	Template int     `json:"template"` // 1 when the qemu row is a template
	Tags     string  `json:"tags"`     // semicolon-separated
	Lock     string  `json:"lock"`
	MaxCPU   float64 `json:"maxcpu"`
	MaxMem   int64   `json:"maxmem"`
	MaxDisk  int64   `json:"maxdisk"`
	CPU      float64 `json:"cpu"`
	Mem      int64   `json:"mem"`
	Disk     int64   `json:"disk"`
	Uptime   int64   `json:"uptime"`
	Storage  string  `json:"storage"` // storage rows: the storage id
	Shared   int     `json:"shared"`  // storage rows
	Content  string  `json:"content"` // storage rows: comma-separated content types

	// NodeDown: a qemu row whose node is not online (see markNodeDown). The VM exists, nothing else in the row counts.
	NodeDown bool `json:"-"`
	// StatusErr: ResolveVM could not read the node, so Status is the /cluster/resources one, which reads running for a paused VM.
	StatusErr error `json:"-"`
}

// ClusterResources lists cluster-wide resources. kind filters server-side:
// "vm" | "storage" | "node" | "sdn" (empty = all).
func (c *Client) ClusterResources(ctx context.Context, kind string) ([]ClusterResource, error) {
	var form url.Values
	if kind != "" {
		form = formVals{"type": kind}.values()
	}
	var out []ClusterResource
	if err := c.get(ctx, "/cluster/resources", form, &out); err != nil {
		return nil, err
	}
	if kind == "vm" {
		c.markNodeDown(ctx, out)
	}
	return out, nil
}

// markNodeDown makes VM rows honest about a dead node. Lab, PVE 9.2: for about five minutes after a node dies its VMs
// still read as last seen, "running" included; after that as status "unknown" with no name, template flag or sizes. So
// a row of such a node says the VM exists and nothing more. Best effort: without the node list a row just stays as PVE
// gave it for one more read.
func (c *Client) markNodeDown(ctx context.Context, rows []ClusterResource) {
	var nodes []ClusterResource
	if err := c.get(ctx, "/cluster/resources", formVals{"type": "node"}.values(), &nodes); err != nil {
		return
	}
	down := map[string]bool{}
	for _, n := range nodes {
		if n.Type == "node" && n.Status != "online" {
			down[n.Node] = true
		}
	}
	for i := range rows {
		if rows[i].Type == "qemu" && down[rows[i].Node] {
			rows[i].NodeDown, rows[i].Status = true, "unknown"
		}
	}
}

// Node is one row of GET /nodes.
type Node struct {
	Node    string  `json:"node"`
	Status  string  `json:"status"` // "online" | "offline" | "unknown"
	MaxCPU  int     `json:"maxcpu"`
	MaxMem  int64   `json:"maxmem"`
	CPU     float64 `json:"cpu"`
	Mem     int64   `json:"mem"`
	Uptime  int64   `json:"uptime"`
	Level   string  `json:"level"`
	SSLFing string  `json:"ssl_fingerprint"`
}

// Nodes lists cluster nodes.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var out []Node
	if err := c.get(ctx, "/nodes", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// NextID reserves nothing — it just returns the lowest free VMID. Racy by nature;
// creation retries on "already exists" with a fresh id.
func (c *Client) NextID(ctx context.Context) (int, error) {
	// PVE returns the id as a JSON string.
	var s string
	if err := c.get(ctx, "/cluster/nextid", nil, &s); err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

// VMIDFree reports whether no guest in the cluster holds vmid. /cluster/nextid?vmid= reads the cluster's
// whole guest list, so unlike /cluster/resources it also sees guests this token has no VM.Audit on; any
// user may call it. PVE answers 400 "VM <id> already exists" for a held id and echoes a free id back.
// Any other refusal, and any 2xx that does not carry that id (null, an empty body, another number), is an
// error: callers delete on "free", so only the documented answer may count as one.
func (c *Client) VMIDFree(ctx context.Context, vmid int) (bool, error) {
	var raw json.RawMessage
	err := c.get(ctx, "/cluster/nextid", formVals{"vmid": strconv.Itoa(vmid)}.values(), &raw)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusBadRequest && strings.Contains(ae.Errors["vmid"], "already exists") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// PVE sends the id as a JSON string; a number is accepted too.
	if got := strings.Trim(string(raw), `"`); got != strconv.Itoa(vmid) {
		return false, fmt.Errorf("/cluster/nextid?vmid=%d answered %q, not the id", vmid, raw)
	}
	return true, nil
}

// CSIControllerVMID is proxmox-csi's default features.controllerVmID, which neither the platform cluster's plugin nor
// a tenant cluster's changes. The plugin names every volume it creates vm-<id>-pvc-<uuid>, and to Proxmox a disk named
// vm-<id>-… is VM <id>'s, whether a VM's config names it or not.
const CSIControllerVMID = 9999

// CSIVMIDCheck is the csi-vmid row of the connection test and of the managed-services preflight: whether a guest of
// the cluster holds CSIControllerVMID. The row is ok when the id is known to be free and a warning otherwise; it never
// fails, since the platform works either way and the danger is in what an operator may do to that guest later.
func (c *Client) CSIVMIDCheck(ctx context.Context) (msg string, free, known bool) {
	id := strconv.Itoa(CSIControllerVMID)
	free, err := c.VMIDFree(ctx, CSIControllerVMID)
	switch {
	case err != nil:
		return "Could not read whether VM id " + id + " is in use: " + err.Error() + ". proxmox-csi names its volumes vm-" + id +
			"-pvc-<uuid>; if a guest has that id, never destroy it with \"Destroy unreferenced disks owned by guest\" (docs/managed-services.md).", false, false
	case !free:
		return "VM id " + id + " is in use on this cluster. proxmox-csi names every volume it creates vm-" + id + "-pvc-<uuid>: the platform's own " +
			"(Kamaji etcd, the registry, managed databases) and the volumes of tenant Kubernetes clusters. To Proxmox these are disks of VM " + id +
			". Never destroy VM " + id + " with \"Destroy unreferenced disks owned by guest\" ticked (qm destroy " + id +
			" --destroy-unreferenced-disks 1): that removes these volumes with it. Keep the VM as it is, and read docs/managed-services.md before you remove or move it.", false, true
	}
	return "No guest has VM id " + id + ", the id proxmox-csi names its volumes after.", true, true
}

// ManagementBridges maps each bridge that carries a node's own IP — the node
// address from /cluster/status, or the default gateway — to that node. A public
// network on such a bridge would put tenant VMs on the hypervisor's L2. nodes
// narrows the check (none = every node in the cluster).
func (c *Client) ManagementBridges(ctx context.Context, nodes ...string) (map[string]string, error) {
	mgmt, err := c.managementIfaces(ctx, nodes...)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, m := range mgmt {
		out[m.iface] = m.node
	}
	return out, nil
}

// ManagementCIDRs lists the IPv4 networks of the bridges ManagementBridges finds on every node:
// what a tenant must not reach through an exit node, storage on that network included. A node that
// cannot answer is left out and complete comes back false: a dead node must not stop the other
// nodes' tenants from changing their networks.
func (c *Client) ManagementCIDRs(ctx context.Context) (cidrs []string, complete bool, err error) {
	nodeIP, err := c.clusterNodeIPs(ctx)
	if err != nil {
		return nil, false, err
	}
	complete = true
	for node, ip := range nodeIP {
		mgmt, err := c.nodeManagementIfaces(ctx, node, ip)
		if err != nil {
			complete = false
			continue
		}
		for _, m := range mgmt {
			if _, n, err := net.ParseCIDR(m.cidr); err == nil && n.IP.To4() != nil && !slices.Contains(cidrs, n.String()) {
				cidrs = append(cidrs, n.String())
			}
		}
	}
	return cidrs, complete, nil
}

// NodeAddresses lists every IPv4 address the nodes hold, as /32: the /cluster/status address and the address of every
// interface in /nodes/{node}/network (uplinks, bonds and VLANs too, not only bridges with a gateway). What a node
// cannot answer for is left out and complete comes back false, as in ManagementCIDRs.
func (c *Client) NodeAddresses(ctx context.Context) (addrs []string, complete bool, err error) {
	nodeIP, err := c.clusterNodeIPs(ctx)
	if err != nil {
		return nil, false, err
	}
	complete = true
	add := func(a string) {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil && !slices.Contains(addrs, ip.String()+"/32") {
			addrs = append(addrs, ip.String()+"/32")
		}
	}
	for node, ip := range nodeIP {
		add(ip)
		var ifaces []struct {
			Address string `json:"address"`
		}
		if err := c.get(ctx, "/nodes/"+url.PathEscape(node)+"/network", nil, &ifaces); err != nil {
			complete = false
			continue
		}
		for _, i := range ifaces {
			add(i.Address)
		}
	}
	slices.Sort(addrs)
	return addrs, complete, nil
}

type mgmtIface struct{ iface, node, cidr string }

func (c *Client) managementIfaces(ctx context.Context, nodes ...string) ([]mgmtIface, error) {
	nodeIP, err := c.clusterNodeIPs(ctx)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		for n := range nodeIP {
			nodes = append(nodes, n)
		}
	}
	var out []mgmtIface
	for _, node := range nodes {
		mgmt, err := c.nodeManagementIfaces(ctx, node, nodeIP[node])
		if err != nil {
			return nil, err
		}
		out = append(out, mgmt...)
	}
	return out, nil
}

func (c *Client) clusterNodeIPs(ctx context.Context) (map[string]string, error) {
	var status []struct {
		Type string `json:"type"`
		Name string `json:"name"`
		IP   string `json:"ip"`
	}
	if err := c.get(ctx, "/cluster/status", nil, &status); err != nil {
		return nil, err
	}
	nodeIP := map[string]string{}
	for _, s := range status {
		if s.Type == "node" {
			nodeIP[s.Name] = s.IP
		}
	}
	return nodeIP, nil
}

func (c *Client) nodeManagementIfaces(ctx context.Context, node, ip string) ([]mgmtIface, error) {
	bridges, err := c.nodeBridges(ctx, node, ip)
	if err != nil {
		return nil, err
	}
	var out []mgmtIface
	for _, b := range bridges {
		if b.Management {
			out = append(out, mgmtIface{b.Iface, node, b.CIDR})
		}
	}
	return out, nil
}

// NodeBridge is one bridge of a node. Management: it carries the node's own address or a default gateway, so a
// public network must not use it (ManagementBridges).
type NodeBridge struct {
	Node       string `json:"node"`
	Iface      string `json:"iface"`
	Type       string `json:"type"` // "bridge" | "OVSBridge" | "vnet"
	CIDR       string `json:"cidr,omitempty"`
	Management bool   `json:"management"`
}

// NodeBridges lists the bridges of every node, by node and name. A node that cannot answer is left out.
func (c *Client) NodeBridges(ctx context.Context) ([]NodeBridge, error) {
	nodeIP, err := c.clusterNodeIPs(ctx)
	if err != nil {
		return nil, err
	}
	out := []NodeBridge{}
	for node, ip := range nodeIP {
		bridges, err := c.nodeBridges(ctx, node, ip)
		if err != nil {
			continue
		}
		out = append(out, bridges...)
	}
	slices.SortFunc(out, func(a, b NodeBridge) int {
		return cmp.Or(strings.Compare(a.Node, b.Node), strings.Compare(a.Iface, b.Iface))
	})
	return out, nil
}

func (c *Client) nodeBridges(ctx context.Context, node, ip string) ([]NodeBridge, error) {
	var ifaces []struct {
		Iface    string `json:"iface"`
		Type     string `json:"type"`
		Address  string `json:"address"`
		Address6 string `json:"address6"`
		Gateway  string `json:"gateway"`
		Gateway6 string `json:"gateway6"`
		CIDR     string `json:"cidr"`
	}
	form := formVals{"type": "any_bridge"}.values()
	if err := c.get(ctx, "/nodes/"+url.PathEscape(node)+"/network", form, &ifaces); err != nil {
		return nil, err
	}
	var out []NodeBridge
	for _, i := range ifaces {
		mgmt := i.Gateway != "" || i.Gateway6 != "" || (ip != "" && (i.Address == ip || i.Address6 == ip))
		out = append(out, NodeBridge{Node: node, Iface: i.Iface, Type: i.Type, CIDR: i.CIDR, Management: mgmt})
	}
	return out, nil
}

// NodesWithoutPackage lists the online nodes where a Debian package is not installed, from the package list
// `pveversion -v` prints (GET /nodes/{node}/apt/versions). The installer's preflight reads frr-pythontools the same
// way with its install credential; reading it with PVEAuditor on /nodes is not lab-verified.
func (c *Client) NodesWithoutPackage(ctx context.Context, pkg string) ([]string, error) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		var pkgs []struct {
			Package      string `json:"Package"`
			CurrentState string `json:"CurrentState"`
		}
		if err := c.get(ctx, "/nodes/"+url.PathEscape(n.Node)+"/apt/versions", nil, &pkgs); err != nil {
			return nil, err
		}
		installed := false
		for _, p := range pkgs {
			installed = installed || (p.Package == pkg && p.CurrentState == "Installed")
		}
		if !installed {
			out = append(out, n.Node)
		}
	}
	slices.Sort(out)
	return out, nil
}
