package pve

import (
	"context"
	"net"
	"net/url"
	"slices"
	"strconv"
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
	var ifaces []struct {
		Iface    string `json:"iface"`
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
	var out []mgmtIface
	for _, i := range ifaces {
		if i.Gateway != "" || i.Gateway6 != "" || (ip != "" && (i.Address == ip || i.Address6 == ip)) {
			out = append(out, mgmtIface{i.Iface, node, i.CIDR})
		}
	}
	return out, nil
}
