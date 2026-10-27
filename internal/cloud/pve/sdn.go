package pve

import (
	"context"
	"net/url"
	"strings"
)

// SDN objects are cluster-global and staged: zone/vnet/subnet CRUD writes a PENDING
// change; PUT /cluster/sdn commits + reloads the network on every node (one UPID).
// ⚠ Apply is global and NOT serialized by PVE — the platform serializes it with a
// pgdoc lock (key "sdn-apply"); see the network service in Phase 4.

// SDNZone is one row of GET /cluster/sdn/zones.
type SDNZone struct {
	Zone    string `json:"zone"`
	Type    string `json:"type"` // "simple" | "vlan" | "vxlan" | "evpn"
	Bridge  string `json:"bridge"`
	Peers   string `json:"peers"`
	MTU     int    `json:"mtu"`
	Nodes   string `json:"nodes"`
	IPAM    string `json:"ipam"`
	DHCP    string `json:"dhcp"`
	Pending any    `json:"pending"` // set when the zone has uncommitted changes
	State   string `json:"state"`   // "new" | "deleted" | "changed" when pending
}

func (c *Client) ListSDNZones(ctx context.Context) ([]SDNZone, error) {
	var out []SDNZone
	if err := c.get(ctx, "/cluster/sdn/zones", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateSDNZone creates a zone. params are the type-specific keys:
//   - simple: {dhcp: "dnsmasq"} (PVE ≥8.1 for DHCP/SNAT)
//   - vlan:   {bridge}
//   - evpn:   {controller, "vrf-vxlan": tag, exitnodes, mtu, ...}
func (c *Client) CreateSDNZone(ctx context.Context, zone, kind string, params map[string]string) error {
	form := formVals(params).values()
	form.Set("zone", zone)
	form.Set("type", kind)
	return c.post(ctx, "/cluster/sdn/zones", form, nil)
}

func (c *Client) DeleteSDNZone(ctx context.Context, zone string) error {
	err := c.delete(ctx, "/cluster/sdn/zones/"+url.PathEscape(zone), nil, nil)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// SDNController is one row of GET /cluster/sdn/controllers (EVPN needs one).
type SDNController struct {
	Controller string `json:"controller"`
	Type       string `json:"type"` // "evpn" | "bgp"
	ASN        int    `json:"asn"`
	Peers      string `json:"peers"`
}

func (c *Client) ListSDNControllers(ctx context.Context) ([]SDNController, error) {
	var out []SDNController
	if err := c.get(ctx, "/cluster/sdn/controllers", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateSDNController creates an EVPN/BGP controller.
func (c *Client) CreateSDNController(ctx context.Context, id, kind string, params map[string]string) error {
	form := formVals(params).values()
	form.Set("controller", id)
	form.Set("type", kind)
	return c.post(ctx, "/cluster/sdn/controllers", form, nil)
}

// SDNVnet is one row of GET /cluster/sdn/vnets.
type SDNVnet struct {
	Vnet      string `json:"vnet"` // ≤8 chars — the "hide L2" constraint: users only ever see the alias
	Zone      string `json:"zone"`
	Alias     string `json:"alias"`
	Tag       int    `json:"tag"` // VLAN id / VXLAN VNI
	VlanAware int    `json:"vlanaware"`
	State     string `json:"state"`
}

func (c *Client) ListSDNVnets(ctx context.Context) ([]SDNVnet, error) {
	var out []SDNVnet
	if err := c.get(ctx, "/cluster/sdn/vnets", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateSDNVnet stages a vnet. tag: VLAN id (vlan zone) / VNI (vxlan+evpn); 0 = auto/none.
func (c *Client) CreateSDNVnet(ctx context.Context, vnet, zone, alias string, tag int) error {
	form := formVals{"vnet": vnet, "zone": zone, "alias": alias, "tag": itoa(tag)}.values()
	return c.post(ctx, "/cluster/sdn/vnets", form, nil)
}

func (c *Client) DeleteSDNVnet(ctx context.Context, vnet string) error {
	err := c.delete(ctx, "/cluster/sdn/vnets/"+url.PathEscape(vnet), nil, nil)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// SDNSubnet is one row of GET /cluster/sdn/vnets/{vnet}/subnets.
type SDNSubnet struct {
	Subnet    string `json:"subnet"` // "<zone>-10.0.0.0-24" (PVE-generated id)
	CIDR      string `json:"cidr"`
	Gateway   string `json:"gateway"`
	SNAT      int    `json:"snat"`
	State     string `json:"state"`
	DHCPRange []struct {
		StartAddress string `json:"start-address"`
		EndAddress   string `json:"end-address"`
	} `json:"dhcp-range"`
}

func (c *Client) ListSDNSubnets(ctx context.Context, vnet string) ([]SDNSubnet, error) {
	var out []SDNSubnet
	if err := c.get(ctx, "/cluster/sdn/vnets/"+url.PathEscape(vnet)+"/subnets", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SubnetOpts drives CreateSDNSubnet.
type SubnetOpts struct {
	CIDR      string
	Gateway   string
	SNAT      bool
	DHCPStart string // both set → one dhcp-range; requires the zone to have dhcp=dnsmasq
	DHCPEnd   string
}

func (c *Client) CreateSDNSubnet(ctx context.Context, vnet string, o SubnetOpts) error {
	form := formVals{
		"subnet":  o.CIDR,
		"type":    "subnet",
		"gateway": o.Gateway,
		"snat":    pbool(o.SNAT),
	}.values()
	if o.DHCPStart != "" && o.DHCPEnd != "" {
		form.Set("dhcp-range", "start-address="+o.DHCPStart+",end-address="+o.DHCPEnd)
	}
	return c.post(ctx, "/cluster/sdn/vnets/"+url.PathEscape(vnet)+"/subnets", form, nil)
}

// DeleteSDNSubnet deletes by the PVE-generated subnet id (SDNSubnet.Subnet).
func (c *Client) DeleteSDNSubnet(ctx context.Context, vnet, subnetID string) error {
	err := c.delete(ctx, "/cluster/sdn/vnets/"+url.PathEscape(vnet)+"/subnets/"+url.PathEscape(subnetID), nil, nil)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// ApplySDN commits all pending SDN changes cluster-wide (PUT /cluster/sdn → UPID).
// The task fans out a reload to every node; WaitTask covers the whole rollout.
func (c *Client) ApplySDN(ctx context.Context) (UPID, error) {
	var upid UPID
	if err := c.put(ctx, "/cluster/sdn", nil, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// HasPendingSDN reports whether any zone/vnet still carries uncommitted state —
// the reconciler uses it to detect a crashed apply.
func (c *Client) HasPendingSDN(ctx context.Context) (bool, error) {
	zones, err := c.ListSDNZones(ctx)
	if err != nil {
		return false, err
	}
	for _, z := range zones {
		if z.State != "" || z.Pending != nil {
			return true, nil
		}
	}
	vnets, err := c.ListSDNVnets(ctx)
	if err != nil {
		return false, err
	}
	for _, v := range vnets {
		if v.State != "" {
			return true, nil
		}
	}
	return false, nil
}

// ParseNodeList splits PVE's comma/semicolon node-list syntax.
func ParseNodeList(s string) []string {
	if s == "" {
		return nil
	}
	f := func(r rune) bool { return r == ',' || r == ';' }
	return strings.FieldsFunc(s, f)
}
