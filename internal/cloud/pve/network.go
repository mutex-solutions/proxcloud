package pve

// network.go — tenant L3 self-service over PVE SDN. The user only ever sees
// {name, CIDR, DHCP, SNAT}; the vnet id (≤8 chars, "v"+random base36) and the
// zone kind are platform implementation detail — that id ceiling is precisely the
// "hide L2" mechanism: users see the alias, never the wire.
//
// Ownership: vnets have NO pool scoping in Proxmox. The vnet alias carries the
// marker "st:<projectID>:<name>" — the sync lists only matching vnets, and every
// mutation re-verifies the alias against the caller's project (the dbaas
// ownership-label discipline).
//
// ⚠ SDN apply (PUT /cluster/sdn) is cluster-global and NOT serialized by PVE —
// the PLATFORM serializes it (pgdoc lock "sdn-apply"); everything here only
// stages + applies, it never locks.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// ZoneConfig is one admin-configured zone (config.network.zones.<key>).
type ZoneConfig struct {
	Key        string // the PVE zone id (≤8 chars, validated at config save)
	Kind       string // "evpn" (the "overlay" default) | "vlan" | "simple"
	Bridge     string // vlan: the underlying bridge
	VlanMin    int    // vlan: tag range
	VlanMax    int
	Controller string   // evpn: controller id (defaults to Key)
	ASN        int      // evpn
	Peers      []string // evpn
	ExitNodes  []string // evpn: gateway/SNAT nodes
	VRFVxlan   int      // evpn: vrf VNI
	MTU        int      // evpn: usually 1450
	// NoDHCP (simple; config.network.zones.<key>.dhcp: false): EnsureZone makes the zone without Proxmox's DHCP server,
	// so its networks get platform-assigned addresses and can hold tenant clusters and load balancers. A zone that
	// exists is never changed.
	NoDHCP bool
}

// Type is the PVE zone type the zone becomes: an empty kind and "overlay" are EVPN (EnsureZone).
func (z ZoneConfig) Type() string {
	if z.Kind == "" || z.Kind == "overlay" {
		return "evpn"
	}
	return z.Kind
}

// OwnershipAlias is the vnet ownership marker — the ONE derivation.
func OwnershipAlias(projectID, name string) string { return "st:" + projectID + ":" + name }

// aliasProject splits an ownership alias; ok=false for foreign/unmanaged vnets.
func aliasProject(alias string) (projectID, name string, ok bool) {
	if !strings.HasPrefix(alias, "st:") {
		return "", "", false
	}
	rest := alias[3:]
	i := strings.IndexByte(rest, ':')
	if i <= 0 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// EnsureZone stages the zone (and its EVPN controller) when missing. Idempotent;
// the caller applies.
func (s *Service) EnsureZone(ctx context.Context, z ZoneConfig) error {
	zones, err := s.C.ListSDNZones(ctx)
	if err != nil {
		return err
	}
	for _, existing := range zones {
		if existing.Zone == z.Key {
			return nil
		}
	}
	switch z.Kind {
	case "simple":
		params := map[string]string{"ipam": "pve"}
		if !z.NoDHCP {
			params["dhcp"] = "dnsmasq"
		}
		return s.C.CreateSDNZone(ctx, z.Key, "simple", params)
	case "vlan":
		if z.Bridge == "" {
			return fmt.Errorf("pve zone %s: vlan zone needs a bridge", z.Key)
		}
		return s.C.CreateSDNZone(ctx, z.Key, "vlan", map[string]string{"bridge": z.Bridge})
	case "evpn", "overlay", "":
		ctrl := z.Controller
		if ctrl == "" {
			ctrl = z.Key
		}
		ctrls, err := s.C.ListSDNControllers(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, c := range ctrls {
			if c.Controller == ctrl {
				found = true
				break
			}
		}
		if !found {
			asn := z.ASN
			if asn == 0 {
				asn = 65000
			}
			if err := s.C.CreateSDNController(ctx, ctrl, "evpn", map[string]string{
				"asn":   strconv.Itoa(asn),
				"peers": strings.Join(z.Peers, ","),
			}); err != nil {
				return err
			}
		}
		vrf := z.VRFVxlan
		if vrf == 0 {
			vrf = 10000
		}
		mtu := z.MTU
		if mtu == 0 {
			mtu = 1450
		}
		params := map[string]string{
			"controller": ctrl,
			"vrf-vxlan":  strconv.Itoa(vrf),
			"mtu":        strconv.Itoa(mtu),
			"ipam":       "pve",
		}
		if len(z.ExitNodes) > 0 {
			params["exitnodes"] = strings.Join(z.ExitNodes, ",")
		}
		return s.C.CreateSDNZone(ctx, z.Key, "evpn", params)
	default:
		return fmt.Errorf("pve zone %s: unknown kind %q", z.Key, z.Kind)
	}
}

// ZoneDHCP reports whether Proxmox hands out addresses in the networks of z. The live zone decides, as it does for a
// server's address (VnetStaticPoolFor): a subnet's dhcp-range does nothing in a zone without a DHCP server. A zone
// EnsureZone has yet to make gets one when it is simple and not NoDHCP.
func (s *Service) ZoneDHCP(ctx context.Context, z ZoneConfig) (bool, error) {
	zones, err := s.C.ListSDNZones(ctx)
	if err != nil {
		return false, err
	}
	for _, existing := range zones {
		if existing.Zone == z.Key {
			return existing.DHCP != "", nil
		}
	}
	return z.Kind == "simple" && !z.NoDHCP, nil
}

// CreateNetworkReq drives CreateNetwork. CIDR is required here (the platform
// auto-carves before calling).
type CreateNetworkReq struct {
	ProjectID string
	Name      string
	Zone      ZoneConfig
	CIDR      string
	Gateway   string // "" → first host of the CIDR
	DHCP      bool   // write a dhcp-range: only for a zone with a DHCP server (ZoneDHCP)
	SNAT      bool
}

// NetworkResult is the staged network's identity.
type NetworkResult struct {
	Vnet    string
	CIDR    string
	Gateway string
	Tag     int
}

// CreateNetwork stages vnet + subnet. The caller ensures the zone first, holds
// the sdn-apply lock, and applies after.
func (s *Service) CreateNetwork(ctx context.Context, req CreateNetworkReq) (*NetworkResult, error) {
	_, ipnet, err := net.ParseCIDR(req.CIDR)
	if err != nil {
		return nil, fmt.Errorf("pve network: bad cidr %q", req.CIDR)
	}
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	maxTag := 0
	for _, v := range vnets {
		taken[v.Vnet] = true
		// VXLAN VNIs are cluster-global (per-project EVPN zones share one VNI
		// space), so the max spans every zone; vlan tags never reach the floor.
		if v.Tag > maxTag {
			maxTag = v.Tag
		}
	}
	vnetID := ""
	for range 8 {
		id := "v" + randBase36(5)
		if !taken[id] {
			vnetID = id
			break
		}
	}
	if vnetID == "" {
		return nil, fmt.Errorf("pve network: could not allocate a vnet id")
	}
	// Tag: vlan zones pick the next free tag in range; evpn/vxlan VNIs count up
	// from a high floor; simple zones carry none.
	tag := 0
	switch req.Zone.Kind {
	case "vlan":
		used := map[int]bool{}
		for _, v := range vnets {
			if v.Zone == req.Zone.Key {
				used[v.Tag] = true
			}
		}
		for t := req.Zone.VlanMin; t <= req.Zone.VlanMax; t++ {
			if !used[t] {
				tag = t
				break
			}
		}
		if tag == 0 {
			return nil, fmt.Errorf("pve network: vlan range exhausted in zone %s", req.Zone.Key)
		}
	case "simple":
		// no tag
	default: // evpn/overlay
		tag = maxTag + 1
		if tag < 100000 {
			tag = 100000
		}
	}
	alias := OwnershipAlias(req.ProjectID, req.Name)
	if err := s.C.CreateSDNVnet(ctx, vnetID, req.Zone.Key, alias, tag); err != nil {
		return nil, err
	}
	gw := req.Gateway
	if gw == "" {
		gw = firstHost(ipnet)
	}
	// A VLAN zone is a bare layer 2 segment: Proxmox holds no gateway address there, runs no DHCP and does no SNAT, so
	// neither is written. The gateway stays on the subnet as the address the site's router must hold: servers and
	// cluster nodes get it as their default route.
	routed := req.Zone.Kind != "vlan"
	sub := SubnetOpts{CIDR: req.CIDR, Gateway: gw, SNAT: req.SNAT && routed}
	if req.DHCP && routed {
		start, end := dhcpRange(ipnet, gw)
		sub.DHCPStart, sub.DHCPEnd = start, end
	}
	if err := s.C.CreateSDNSubnet(ctx, vnetID, sub); err != nil {
		// Roll the staged vnet back so a failed create leaves nothing pending.
		_ = s.C.DeleteSDNVnet(ctx, vnetID)
		return nil, err
	}
	return &NetworkResult{Vnet: vnetID, CIDR: req.CIDR, Gateway: gw, Tag: tag}, nil
}

// DeleteNetwork stages the removal of a vnet + its subnets, verifying the
// ownership alias first. The caller holds the lock and applies.
func (s *Service) DeleteNetwork(ctx context.Context, projectID, vnetID string) error {
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, v := range vnets {
		if v.Vnet == vnetID {
			if pid, _, ok := aliasProject(v.Alias); !ok || pid != projectID {
				return fmt.Errorf("pve network: vnet %s is not owned by project %s", vnetID, projectID)
			}
			found = true
			break
		}
	}
	if !found {
		return nil // already gone
	}
	subs, err := s.C.ListSDNSubnets(ctx, vnetID)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if err := s.C.DeleteSDNSubnet(ctx, vnetID, sub.Subnet); err != nil {
			return err
		}
	}
	return s.C.DeleteSDNVnet(ctx, vnetID)
}

// TeardownProjectNetworks stages removal of every vnet the project owns, removes its
// isolation ACCEPT at once, and reports whether anything was staged. Project teardown:
// the pool's VMs are already gone. The caller holds the sdn-apply lock and applies.
func (s *Service) TeardownProjectNetworks(ctx context.Context, projectID string) (bool, error) {
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return false, err
	}
	staged := false
	for _, v := range vnets {
		if pid, _, ok := aliasProject(v.Alias); !ok || pid != projectID {
			continue
		}
		if err := s.DeleteNetwork(ctx, projectID, v.Vnet); err != nil {
			return staged, err
		}
		staged = true
	}
	return staged, s.setProjectIsolation(ctx, projectID, nil)
}

// SubnetCIDRs lists every subnet CIDR on the cluster — all zones, all projects,
// foreign vnets too: the overlap check's ground truth. Read under the sdn-apply
// lock it also covers staged-but-unapplied networks (no create race).
// ponytail: one GET per vnet; fine at lab scale, cache when vnet counts grow.
func (s *Service) SubnetCIDRs(ctx context.Context) ([]string, error) {
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range vnets {
		if v.State == "deleted" {
			continue
		}
		subs, err := s.C.ListSDNSubnets(ctx, v.Vnet)
		if err != nil {
			return nil, err
		}
		for _, sub := range subs {
			out = append(out, sub.CIDR)
		}
	}
	return out, nil
}

// --- tenant isolation (decision 4) ---
//
// Two models, both keyed off config.network.isolation:
//   - "shared" (default): every tenant vnet lives in the one default zone, one
//     routing domain. Non-overlapping, in-pool CIDRs are enforced by the platform at
//     create, so the cluster firewall rules below can tell tenants apart by address.
//   - "evpn-per-project": each project gets its own EVPN zone (= its own VRF),
//     id ZoneIDFor(projectID). The same cluster rules still apply: exit nodes leak
//     every VRF into the default table, so the VRF alone is not a wall.

const (
	IsolationShared = "shared"
	IsolationEVPN   = "evpn-per-project"
)

// isolationComment marks the platform-managed cluster firewall rules; a project's own rules
// add ":<projectID>" (operator rules are left alone).
const isolationComment = "proxcloud:isolation"

// isolationOwner is the comment of this portal's provider-wide rules, the only ones it rewrites.
// A cluster normally has one portal, which then owns its rules outright: a changed pool or
// blocked list replaces them. A portal with another pool prefix keeps its own set, so nobody
// deletes the DROPs protecting someone else's tenants. The default prefix keeps the bare comment:
// clusters already running need no migration.
func (s *Service) isolationOwner() string {
	if s.PoolPrefix == "" || s.PoolPrefix == "pc-" {
		return isolationComment
	}
	return isolationComment + "/" + s.PoolPrefix
}

// ZoneIDFor is the project's EVPN zone id: "p" + 7 base36 chars of
// sha256(projectID) — 8 chars, PVE's zone-id ceiling. Deterministic, so no DB row.
func ZoneIDFor(projectID string) string {
	sum := sha256.Sum256([]byte(projectID))
	n := binary.BigEndian.Uint64(sum[:8]) % 78364164096 // 36^7
	id := strconv.FormatUint(n, 36)
	return "p" + strings.Repeat("0", 7-len(id)) + id
}

// EnsureProjectZone stages the project's EVPN zone (idempotent). tmpl is the
// admin's default EVPN zone (asn/peers/exitnodes/mtu); controller overrides its
// controller so every project zone shares one. The VRF VNI is the next free one.
// Collision check: an existing zone with our id must be evpn and hold only this
// project's vnets. The caller holds the sdn-apply lock and applies.
func (s *Service) EnsureProjectZone(ctx context.Context, projectID string, tmpl ZoneConfig, controller string) (ZoneConfig, error) {
	switch tmpl.Kind {
	case "evpn", "overlay", "":
	default:
		return ZoneConfig{}, fmt.Errorf("pve: per-project isolation needs an evpn default zone, got %q", tmpl.Kind)
	}
	id := ZoneIDFor(projectID)
	z := tmpl
	z.Key, z.Kind = id, "evpn"
	z.Controller = controller
	if z.Controller == "" {
		z.Controller = tmpl.Controller
	}
	if z.Controller == "" {
		z.Controller = tmpl.Key // EnsureZone's own default — never the per-project id
	}
	zones, err := s.C.ListSDNZones(ctx)
	if err != nil {
		return ZoneConfig{}, err
	}
	maxVRF := 0
	for _, existing := range zones {
		if existing.VRFVxlan > maxVRF {
			maxVRF = existing.VRFVxlan
		}
		if existing.Zone != id {
			continue
		}
		if existing.Type != "evpn" {
			return ZoneConfig{}, fmt.Errorf("pve zone %s: id collides with a %s zone", id, existing.Type)
		}
		vnets, err := s.C.ListSDNVnets(ctx)
		if err != nil {
			return ZoneConfig{}, err
		}
		for _, v := range vnets {
			if v.Zone != id {
				continue
			}
			if pid, _, ok := aliasProject(v.Alias); !ok || pid != projectID {
				return ZoneConfig{}, fmt.Errorf("pve zone %s: id collides with another owner's zone", id)
			}
		}
		z.VRFVxlan = existing.VRFVxlan
		return z, nil
	}
	// VRF VNIs stay below the vnet VNI floor (100000).
	z.VRFVxlan = max(maxVRF+1, 10000)
	return z, s.EnsureZone(ctx, z)
}

// DeleteProjectZoneIfEmpty stages removal of the project's EVPN zone once its
// last vnet is gone. Returns whether a delete was staged (the caller applies).
func (s *Service) DeleteProjectZoneIfEmpty(ctx context.Context, projectID string) (bool, error) {
	id := ZoneIDFor(projectID)
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return false, err
	}
	for _, v := range vnets {
		if v.Zone == id && v.State != "deleted" {
			return false, nil
		}
	}
	zones, err := s.C.ListSDNZones(ctx)
	if err != nil {
		return false, err
	}
	for _, z := range zones {
		if z.Zone == id && z.Type == "evpn" {
			return true, s.C.DeleteSDNZone(ctx, id)
		}
	}
	return false, nil
}

// isolationRules is the provider-wide part of the isolation rule set, in the order wanted in
// the cluster firewall (first match wins): nothing from the pool reaches another address of the
// pool or a blocked network. Each project's own ACCEPTs sit above these (projectIsolationRules).
// They are cluster FORWARD rules because tenant traffic between subnets is routed by the node:
// lab-verified on PVE 9.2 and proxmox-firewall, vnet FORWARD rules only see frames switched
// inside one vnet bridge, and a routed packet never hits them. The IN rules cover the node's own
// addresses, which forward never sees: another tenant's gateway, and whatever a leaked VRF route
// reaches on an exit node.
// The sandbox networks (the platform's sandbox cluster workers, docs/managed-sandbox.md) are fenced
// both ways: the pool may not reach them, and they may reach neither the pool nor a blocked
// network. Their traffic to the management networks leaves through an exit node that SNATs it to a
// node address, so it has to drop here, before the rewrite. The one exception is allow: the "address:port"
// endpoints the workers' own VM firewall opens (registry, build bucket, portal wake), which may well sit on a
// management network (lab: the build bucket on 10.88.0.41:9000), accepted over TCP above the sandbox DROPs.
// Without it a build pod's fetch timed out on every connect while the VM firewall said yes.
// There is deliberately no blanket DROP source=pool: it would also drop internet egress (SNAT).
func isolationRules(pool string, blocked, sandbox []string, owner string, allow ...string) []FirewallRule {
	out := []FirewallRule{
		{Type: "forward", Action: "DROP", Source: pool, Dest: pool, Enable: 1, Comment: owner},
		{Type: "in", Action: "DROP", Source: pool, Dest: pool, Enable: 1, Comment: owner},
	}
	drop := func(src string, dst []string) {
		dest := strings.Join(dst, ",")
		out = append(out,
			FirewallRule{Type: "forward", Action: "DROP", Source: src, Dest: dest, Enable: 1, Comment: owner},
			FirewallRule{Type: "in", Action: "DROP", Source: src, Dest: dest, Enable: 1, Comment: owner},
		)
	}
	if dst := append(slices.Clone(blocked), sandbox...); len(dst) > 0 {
		drop(pool, dst)
	}
	if len(sandbox) > 0 {
		src := strings.Join(sandbox, ",")
		for _, ep := range sandboxEndpoints(allow) {
			for _, typ := range []string{"forward", "in"} {
				out = append(out, FirewallRule{Type: typ, Action: "ACCEPT", Proto: "tcp", Source: src, Dest: ep.Addr().String(),
					DPort: strconv.Itoa(int(ep.Port())), Enable: 1, Comment: owner})
			}
		}
		drop(src, append([]string{pool}, blocked...))
	}
	return out
}

// sandboxEndpoints parses "address:port" entries, sorted and without duplicates; an entry that is not an IPv4 address
// with a port is skipped (the VM firewall refuses it too, and the fence gate reports it), so a bad endpoint never
// stops the isolation of tenant networks.
func sandboxEndpoints(allow []string) []netip.AddrPort {
	var out []netip.AddrPort
	for _, a := range allow {
		ep, err := netip.ParseAddrPort(strings.TrimSpace(a))
		if err == nil && ep.Addr().Is4() && ep.Port() != 0 {
			out = append(out, ep)
		}
	}
	slices.SortFunc(out, func(a, b netip.AddrPort) int { return a.Compare(b) })
	return slices.Compact(out)
}

// managementEndpoints drops the sandbox endpoints inside the tenant pool or an operator-blocked range (tenant): only a
// management-side endpoint may be opened to the sandbox networks, never a way into a tenant's network.
func managementEndpoints(allow, tenant []string) []string {
	var nets []netip.Prefix
	for _, c := range tenant {
		if p, err := netip.ParsePrefix(strings.TrimSpace(c)); err == nil {
			nets = append(nets, p.Masked())
		}
	}
	var out []string
	for _, a := range allow {
		ep, err := netip.ParseAddrPort(strings.TrimSpace(a))
		if err == nil && slices.ContainsFunc(nets, func(p netip.Prefix) bool { return p.Contains(ep.Addr()) }) {
			slog.Warn("pve isolation: a sandbox endpoint in a tenant range is not opened", "endpoint", a)
			continue
		}
		out = append(out, a)
	}
	return out
}

// projectIsolationRules let a project's subnets reach each other and their own gateways.
func projectIsolationRules(projectID string, cidrs []string) []FirewallRule {
	if len(cidrs) == 0 {
		return nil // an empty source would mean "any"
	}
	own := strings.Join(cidrs, ",")
	c := isolationComment + ":" + projectID
	return []FirewallRule{
		{Type: "forward", Action: "ACCEPT", Source: own, Dest: own, Enable: 1, Comment: c},
		{Type: "in", Action: "ACCEPT", Source: own, Dest: own, Enable: 1, Comment: c},
	}
}

func sameRule(a, b FirewallRule) bool {
	return a.Type == b.Type && a.Action == b.Action && a.Proto == b.Proto && a.Source == b.Source && a.Dest == b.Dest &&
		a.DPort == b.DPort && a.Comment == b.Comment
}

// sameLiveRule is sameRule plus the enable flag: a rule disabled by hand reads as missing, so the reconcile puts it back.
func sameLiveRule(a, b FirewallRule) bool { return sameRule(a, b) && a.Enable == b.Enable }

// ReconcileIsolation brings the cluster firewall to the isolation rule set: this portal's
// provider-wide DROPs (blocked = the management networks plus the operator's extra ones; sandbox =
// the sandbox cluster's networks, fenced in both directions) and this project's ACCEPTs over its
// subnets; allow are the sandbox endpoints (isolationRules). Idempotent; call after a vnet of the project is created or
// deleted, and from the sync job, which repairs drift and rolls a changed rule set out to projects
// made before it. PVE adds a rule on top, and every step fails closed: DROPs go in before anything
// is removed, and an ACCEPT is only ever replaced by adding it after the old one is gone.
func (s *Service) ReconcileIsolation(ctx context.Context, projectID, pool string, blocked, sandbox []string, allow ...string) error {
	return s.reconcileIsolation(ctx, projectID, pool, blocked, sandbox, nil, allow...)
}

// ReconcileIsolationInventory is the sync path: use the verified cached network
// payload, not a second permission-filtered SDN list that could erase ACCEPTs.
func (s *Service) ReconcileIsolationInventory(ctx context.Context, projectID, pool string, blocked, sandbox, cidrs []string, allow ...string) error {
	return s.reconcileIsolation(ctx, projectID, pool, blocked, sandbox, &cidrs, allow...)
}

func (s *Service) reconcileIsolation(ctx context.Context, projectID, pool string, blocked, sandbox []string, verifiedCIDRs *[]string, allow ...string) error {
	if err := s.reconcileDrops(ctx, false, pool, blocked, sandbox, allow); err != nil {
		return err
	}

	if verifiedCIDRs != nil {
		cidrs := slices.Clone(*verifiedCIDRs)
		slices.Sort(cidrs)
		return s.setProjectIsolation(ctx, projectID, cidrs)
	}
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return err
	}
	var cidrs []string
	for _, v := range vnets {
		if pid, _, ok := aliasProject(v.Alias); !ok || pid != projectID {
			continue
		}
		subs, err := s.C.ListSDNSubnets(ctx, v.Vnet)
		if err != nil {
			return err
		}
		for _, sub := range subs {
			cidrs = append(cidrs, sub.CIDR)
		}
	}
	slices.Sort(cidrs)
	return s.setProjectIsolation(ctx, projectID, cidrs)
}

// ErrIsolationRefused marks the cases in which ReconcileProviderIsolation writes nothing on purpose and the next cycle
// will not either: a config or a cluster state someone has to change, not a failed call.
var ErrIsolationRefused = errors.New("pve isolation")

func refused(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrIsolationRefused, fmt.Sprintf(format, a...))
}

// ReconcileProviderIsolation writes this portal's provider-wide DROPs and touches no project's ACCEPTs: the sync calls
// it once per provider that carries a managed-services installation (and for no other provider), so the rules exist
// before the first tenant network and the managed-services preflight can be made green on a provider with no project
// (on a cluster another portal shares, ErrIsolationPending never applies).
// It has no tenant waiting and none of its own to protect, so it refuses (ErrIsolationRefused) wherever a write could
// do harm: on a node read that is not complete, for a cidrPool that overlaps a management network ("DROP pool to pool"
// would only cut the nodes off each other), and when the rules in place are for another pool (otherPool), drop
// something its own set does not (uncovered) or accept other sandbox endpoints than its own (otherAccepts): a portal
// or provider that shares the rule owner may depend on them. It refuses as well while a project's rules for the pool
// are in place and the set is not its own (projectMaintained): that project's sync writes the set. It therefore only
// ever adds DROPs, to a set no project's sync maintains; a changed cidrPool, a narrowed list or changed sandbox
// endpoints are rolled out by the per-project path, or by hand.
func (s *Service) ReconcileProviderIsolation(ctx context.Context, pool string, blocked, sandbox []string, allow ...string) error {
	return s.reconcileDrops(ctx, true, pool, blocked, sandbox, allow)
}

// poolOverManagement is the error for a cidrPool that overlaps a management network of the nodes, which
// ReconcileProviderIsolation refuses to write rules for.
func poolOverManagement(pool *net.IPNet, mgmt []string) error {
	for _, m := range mgmt {
		if _, n, err := net.ParseCIDR(m); err == nil && (pool.Contains(n.IP) || n.Contains(pool.IP)) {
			return refused("cidrPool %s overlaps the management network %s: the provider sync writes no rule for it, choose another cidrPool", pool, n)
		}
	}
	return nil
}

// byHand is how rules no provider sync replaces are replaced on a provider with no project.
const byHand = "delete the %[1]s rules under Datacenter > Firewall and run a provider sync"

// otherPool is the error for owner rules in place that fence another pool than pool, nil otherwise: every rule set
// has "DROP pool to pool" (isolationRules), which names the pool it was written for.
func otherPool(mine []FirewallRule, owner string, pool *net.IPNet) error {
	for _, r := range mine {
		if r.Action == "DROP" && r.Source == r.Dest && r.Source != pool.String() {
			return refused("the %[1]s rules in the cluster firewall are for another pool (%[2]s), not cidrPool %[3]s: another portal or provider on this cluster uses the same pool prefix (give this provider its own poolPrefix), or this provider's cidrPool changed (a project's sync or a network create rewrites them; with no project, "+byHand+"); a provider sync with no project leaves them in place",
				owner, r.Source, pool)
		}
	}
	return nil
}

// uncovered is the error for an owner DROP in place that want would not keep: some source and destination it drops
// (each a network of its comma lists) lies in no DROP of want of the same type. Equal or wider networks cover.
func uncovered(mine, want []FirewallRule, owner string) error {
	covers := func(list, entry string) bool {
		e, eerr := netip.ParsePrefix(entry)
		for _, c := range strings.Split(list, ",") {
			if p, err := netip.ParsePrefix(c); c == entry || err == nil && eerr == nil && p.Bits() <= e.Bits() && p.Contains(e.Addr()) {
				return true
			}
		}
		return false
	}
	for _, r := range mine {
		if r.Action != "DROP" {
			continue
		}
		for _, src := range strings.Split(r.Source, ",") {
			for _, dst := range strings.Split(r.Dest, ",") {
				if !slices.ContainsFunc(want, func(w FirewallRule) bool {
					return w.Action == "DROP" && w.Type == r.Type && covers(w.Source, src) && covers(w.Dest, dst)
				}) {
					return refused("the %[1]s rules in the cluster firewall drop %[2]s to %[3]s, which this provider's cidrPool, blockedCidrs and sandboxCidrs do not: another portal or provider on this cluster uses the same pool prefix (give this provider its own poolPrefix), or this provider's list was narrowed (a project's sync or a network create rewrites them; with no project, "+byHand+"); a provider sync with no project removes no DROP in place",
						owner, src, dst)
				}
			}
		}
	}
	return nil
}

// otherAccepts is the error for owner ACCEPTs in place (the sandbox endpoints, isolationRules) that are not the ones of
// want. They sit above the sandbox DROPs, so replacing them would close an endpoint a portal or provider that shares
// the owner opened, or open one to its sandbox network that its own set drops.
func otherAccepts(mine, want []FirewallRule, owner string) error {
	if len(mine) == 0 {
		return nil
	}
	lacks := func(in []FirewallRule, r FirewallRule) bool {
		return r.Action == "ACCEPT" && !slices.ContainsFunc(in, func(x FirewallRule) bool { return sameRule(x, r) })
	}
	say := func(r FirewallRule, what string) error {
		return refused("the %[1]s rules in the cluster firewall %[2]s (%[3]s to %[4]s:%[5]s): another portal or provider on this cluster uses the same pool prefix (give this provider its own poolPrefix), or this provider's sandbox endpoints changed (a project's sync or a network create rewrites them; with no project, "+byHand+"); a provider sync with no project replaces no ACCEPT in place",
			owner, what, r.Source, r.Dest, r.DPort)
	}
	for _, r := range mine {
		if lacks(want, r) {
			return say(r, "have an ACCEPT for a sandbox endpoint that is not one of this provider's")
		}
	}
	for _, r := range want {
		if lacks(mine, r) {
			return say(r, "have no ACCEPT for this provider's sandbox endpoint")
		}
	}
	return nil
}

// projectMaintained is the error for owner rules in place that are not want while a project's ACCEPTs for this pool are
// in the firewall: that project's sync writes the provider-wide set on every cycle, whatever is in place. If it is a
// project of a portal or provider that shares the owner, a write here would be undone by it and done again, for ever,
// each time with the project's ACCEPTs deleted and added again; if it is this provider's, its sync has the write.
// ponytail: an ACCEPT does not say whose project it is. A sharer whose projects have no network yet is not seen (the
// two then rewrite in turn until its first network), and a project of another prefix on the same cidrPool is taken for
// a sharer; recording the set the provider-level call last wrote would tell them apart.
func projectMaintained(rules, mine, want []FirewallRule, owner string, pool *net.IPNet) error {
	if len(mine) == 0 || slices.EqualFunc(mine, want, sameLiveRule) ||
		!slices.ContainsFunc(rules, func(r FirewallRule) bool { return isProjectIsolationRule(r) && sourceInPool(r.Source, pool) }) {
		return nil
	}
	return refused("the %[1]s rules in the cluster firewall are not this provider's set (%[2]d in place, %[3]d wanted), and a project's rules for cidrPool %[4]s are in place, whose sync maintains them: a provider sync with no project replaces no such set. If the project is this provider's, its sync or a network create writes the set (run a provider sync). If not, another portal or provider on this cluster uses the same pool prefix (give this provider its own poolPrefix), or the same cidrPool under another prefix (then "+byHand+")",
		owner, len(mine), len(want), pool)
}

// providerRefusal is why the provider-level call leaves the owner rules in place (mine) as they are, nil when it may
// bring them to want.
func providerRefusal(rules, mine, want []FirewallRule, owner string, pool *net.IPNet) error {
	return cmp.Or(otherPool(mine, owner, pool), uncovered(mine, want, owner), otherAccepts(mine, want, owner),
		projectMaintained(rules, mine, want, owner, pool))
}

// reconcileDrops brings this portal's provider-wide rules (isolationWant) into the cluster firewall. providerLevel
// (the call with no project) refuses instead wherever ReconcileProviderIsolation says so.
func (s *Service) reconcileDrops(ctx context.Context, providerLevel bool, pool string, blocked, sandbox, allow []string) error {
	want, owner, poolNet, mgmt, complete, err := s.isolationWant(ctx, pool, blocked, sandbox, allow)
	if err != nil {
		return err
	}
	if providerLevel {
		// A node that did not answer may hold the management network the pool overlaps, and want lacks its DROP.
		if !complete {
			return refused("a PVE node did not report its networks; the provider sync writes no rule until it does")
		}
		if err := poolOverManagement(poolNet, mgmt); err != nil {
			return err
		}
	}
	rules, err := s.C.ListClusterFirewallRules(ctx)
	if err != nil {
		return err
	}
	var mine []FirewallRule
	for _, r := range rules {
		if r.Comment == owner {
			mine = append(mine, r)
		}
	}
	if providerLevel {
		if err := providerRefusal(rules, mine, want, owner, poolNet); err != nil {
			return err
		}
	}
	// With a node down the list may miss a network only it knew, so DROPs in place stay until it is
	// back. A project's own ACCEPTs do not depend on it. A tenant's first network is not held up by a dead node.
	if !slices.EqualFunc(mine, want, sameLiveRule) && (complete || len(mine) == 0) {
		return s.rebuildIsolation(ctx, rules, want, owner, poolNet)
	}
	return nil
}

// isolationWant is this portal's provider-wide isolation rule set (isolationRules over the management networks plus
// blocked). complete is false when a node did not report its networks.
func (s *Service) isolationWant(ctx context.Context, pool string, blocked, sandbox, allow []string) (want []FirewallRule, owner string, poolNet *net.IPNet, mgmt []string, complete bool, err error) {
	if _, poolNet, err = net.ParseCIDR(pool); err != nil {
		return nil, "", nil, nil, false, fmt.Errorf("pve isolation: invalid cidrPool %q", pool)
	}
	if mgmt, complete, err = s.C.ManagementCIDRs(ctx); err != nil {
		return nil, "", nil, nil, false, err
	}
	allow = managementEndpoints(allow, append([]string{poolNet.String()}, blocked...))
	blocked = slices.Compact(slices.Sorted(slices.Values(append(mgmt, blocked...))))
	if sandbox, err = sandboxNets(poolNet, blocked, sandbox); err != nil {
		return nil, "", nil, nil, false, err
	}
	owner = s.isolationOwner()
	return isolationRules(poolNet.String(), blocked, sandbox, owner, allow...), owner, poolNet, mgmt, complete, nil
}

// CheckSandboxIsolation is the isolation release gate of a sandbox provider (docs/managed-sandbox.md §3): read-only,
// with the portal's token, it checks that the datacenter firewall is on and that this portal's isolation DROPs are in
// the cluster firewall, enabled, exactly as ReconcileIsolation writes them for these sandbox networks: the tenant pool
// may not reach them, and they reach neither the pool nor a blocked network.
func (s *Service) CheckSandboxIsolation(ctx context.Context, pool string, blocked, sandbox []string, allow ...string) error {
	if len(sandbox) == 0 {
		return errors.New("pve isolation: network.sandboxCidrs is empty, so no rule fences the sandbox network")
	}
	return s.checkIsolation(ctx, false, pool, blocked, sandbox, allow...)
}

// ErrIsolationPending: none of this portal's isolation rules is in the cluster firewall, and no tenant network
// exists on the cluster yet. Only the managed-services preflight gets it, so the provider has an installation: the next
// provider sync writes them (ReconcileProviderIsolation), the first tenant network does too before it goes live, and until then the sandbox gate (CheckSandboxIsolation) refuses every sandbox, so no
// tenant can reach anything.
var ErrIsolationPending = errors.New("pve isolation: no rule is written yet; a provider sync or the first tenant network writes them")

// CheckIsolation is CheckSandboxIsolation without its sandbox requirement (the managed-services preflight): read-only,
// it checks that the datacenter firewall is on and that this portal's isolation DROPs are in the cluster firewall,
// enabled, exactly as ReconcileIsolation writes them. With no rule of this portal and no tenant network on the whole
// cluster it answers ErrIsolationPending, with or without sandbox networks. A vnet alias does not say which portal
// made it, so another portal's tenant network ends the pending case too: the check then fails until a provider sync
// has written this portal's rules, which is the strict side of a question the cluster cannot answer.
func (s *Service) CheckIsolation(ctx context.Context, pool string, blocked, sandbox []string, allow ...string) error {
	return s.checkIsolation(ctx, true, pool, blocked, sandbox, allow...)
}

func (s *Service) checkIsolation(ctx context.Context, pendingOK bool, pool string, blocked, sandbox []string, allow ...string) error {
	want, owner, poolNet, mgmt, complete, err := s.isolationWant(ctx, pool, blocked, sandbox, allow)
	if err != nil {
		return err
	}
	if !complete {
		return errors.New("pve isolation: a PVE node did not report its networks, so the rule set cannot be checked")
	}
	on, err := s.C.DatacenterFirewallEnabled(ctx)
	if err != nil {
		return err
	}
	if !on {
		return errors.New("pve isolation: the datacenter firewall is off, so no isolation rule is enforced")
	}
	rules, err := s.C.ListClusterFirewallRules(ctx)
	if err != nil {
		return err
	}
	var mine []FirewallRule
	for _, r := range rules {
		if r.Comment == owner {
			if r.Enable != 1 {
				return fmt.Errorf("pve isolation: rule %d (%s) is disabled", r.Pos, owner)
			}
			mine = append(mine, r)
		}
	}
	if slices.EqualFunc(mine, want, sameRule) {
		return nil
	}
	// The provider sync refuses in these cases (ReconcileProviderIsolation), so neither "pending" nor "a provider
	// sync writes them" would be true.
	if err := cmp.Or(poolOverManagement(poolNet, mgmt), providerRefusal(rules, mine, want, owner, poolNet)); err != nil {
		return err
	}
	if len(mine) == 0 && pendingOK {
		vnets, err := s.C.ListSDNVnets(ctx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(vnets, func(v SDNVnet) bool { _, _, ok := aliasProject(v.Alias); return ok }) {
			return ErrIsolationPending
		}
	}
	what := "for sandbox networks " + strings.Join(sandbox, ",")
	if len(sandbox) == 0 {
		what = "for these blocked networks"
	}
	return fmt.Errorf("pve isolation: the %s rules in the cluster firewall are not the ones %s (%d in place, %d wanted); a provider sync writes them for a provider that has a project or a managed-services installation",
		owner, what, len(mine), len(want))
}

// sandboxNets normalises the sandbox networks. One that overlaps the tenant pool is refused: a
// tenant subnet inside it would be cut off from its own project, and the pool's DROPs would no
// longer say which side a packet came from. One that overlaps a blocked network (the management
// networks among them) is refused too: its DROPs would cut that network off the nodes' own
// addresses (PVE API, ssh, corosync).
func sandboxNets(pool *net.IPNet, blocked, cidrs []string) ([]string, error) {
	overlaps := func(a, b *net.IPNet) bool { return a.Contains(b.IP) || b.Contains(a.IP) }
	out := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(strings.TrimSpace(c))
		if err != nil || n.IP.To4() == nil {
			return nil, fmt.Errorf("pve isolation: invalid sandboxCidrs entry %q (IPv4 CIDR)", c)
		}
		if overlaps(pool, n) {
			return nil, fmt.Errorf("pve isolation: sandboxCidrs entry %s overlaps cidrPool %s", n, pool)
		}
		for _, b := range blocked {
			if _, bn, err := net.ParseCIDR(b); err == nil && overlaps(bn, n) {
				return nil, fmt.Errorf("pve isolation: sandboxCidrs entry %s overlaps blocked network %s", n, b)
			}
		}
		out = append(out, n.String())
	}
	return slices.Compact(slices.Sorted(slices.Values(out))), nil
}

// rebuildIsolation replaces this portal's DROPs when they are missing or the networks changed.
// ACCEPTs inside the pool must stay above them, so they are re-added on top afterwards. Rules of
// another owner and ACCEPTs outside the pool are not touched: the new DROPs cannot shadow them.
func (s *Service) rebuildIsolation(ctx context.Context, rules, want []FirewallRule, owner string, pool *net.IPNet) error {
	for i := len(want) - 1; i >= 0; i-- {
		if err := s.C.AddClusterFirewallRule(ctx, want[i]); err != nil {
			return err
		}
	}
	var accepts []FirewallRule
	for i := len(rules) - 1; i >= 0; i-- {
		r := rules[i]
		isAccept := isProjectIsolationRule(r) && sourceInPool(r.Source, pool)
		if r.Comment != owner && !isAccept {
			continue
		}
		if isAccept {
			accepts = append(accepts, r)
		}
		if err := s.deleteClusterRule(ctx, r); err != nil {
			return err
		}
	}
	for _, r := range accepts {
		if err := s.C.AddClusterFirewallRule(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// deleteClusterRule removes the lowest rule equal to r. PVE addresses rules by position only, and
// another portal or an operator can edit the list at any moment, so the position is looked up
// fresh and sent with the list's digest: PVE refuses the delete if anything changed in between.
// The lowest match matters while a rebuild has the new copy of a rule on top. The enable flag is part of the
// match: deleting a rule disabled by hand must not take its enabled twin instead.
func (s *Service) deleteClusterRule(ctx context.Context, r FirewallRule) error {
	var err error
	for range 5 {
		rules, lerr := s.C.ListClusterFirewallRules(ctx)
		if lerr != nil {
			return lerr
		}
		i := len(rules) - 1
		for i >= 0 && !sameLiveRule(rules[i], r) {
			i--
		}
		if i < 0 {
			return nil
		}
		if err = s.C.DeleteClusterFirewallRule(ctx, rules[i].Pos, rules[i].Digest); err == nil {
			return nil
		}
	}
	return err
}

func isProjectIsolationRule(r FirewallRule) bool {
	return strings.HasPrefix(r.Comment, isolationComment+":")
}

// sourceInPool reports whether a rule's source list starts inside the pool. A project's subnets
// all come from one pool, so the first entry decides.
func sourceInPool(source string, pool *net.IPNet) bool {
	first, _, _ := strings.Cut(source, ",")
	ip, _, err := net.ParseCIDR(first)
	return err == nil && pool.Contains(ip)
}

// setProjectIsolation makes the project's ACCEPT rules match cidrs (none = no rules).
func (s *Service) setProjectIsolation(ctx context.Context, projectID string, cidrs []string) error {
	rules, err := s.C.ListClusterFirewallRules(ctx)
	if err != nil {
		return err
	}
	missing := projectIsolationRules(projectID, cidrs)
	comment := isolationComment + ":" + projectID
	for _, r := range rules {
		if r.Comment != comment {
			continue
		}
		if i := slices.IndexFunc(missing, func(w FirewallRule) bool { return sameLiveRule(w, r) }); i >= 0 {
			missing = slices.Delete(missing, i, i+1)
			continue
		}
		if err := s.deleteClusterRule(ctx, r); err != nil {
			return err
		}
	}
	for _, r := range missing {
		if err := s.C.AddClusterFirewallRule(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// ApplyAndWait commits pending SDN changes and waits the rollout. Caller holds
// the platform sdn-apply lock.
func (s *Service) ApplyAndWait(ctx context.Context) error {
	upid, err := s.C.ApplySDN(ctx)
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, upid)
}

// VnetStaticPool returns the address pool of a vnet whose zone has no DHCP (EVPN and VLAN zones, and a
// simple zone made without it): servers there get a fixed address over cloud-init,
// allocated by the platform. ok=false means the zone's own DHCP hands out addresses.
func (s *Service) VnetStaticPool(ctx context.Context, vnet string) (cidr, gateway, first, last string, ok bool, err error) {
	return s.VnetStaticPoolFor(ctx, vnet, "")
}

// vnetZone is the live zone of a vnet (the zero zone when Proxmox lists neither).
func (s *Service) vnetZone(ctx context.Context, vnet string) (SDNZone, error) {
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return SDNZone{}, err
	}
	zones, err := s.C.ListSDNZones(ctx)
	if err != nil {
		return SDNZone{}, err
	}
	for _, v := range vnets {
		if v.Vnet != vnet {
			continue
		}
		for _, z := range zones {
			if z.Zone == v.Zone {
				return z, nil
			}
		}
	}
	return SDNZone{}, nil
}

// VnetStaticPoolFor is VnetStaticPool on one subnet of the vnet: the one with this CIDR or holding
// this address ("" = the first).
func (s *Service) VnetStaticPoolFor(ctx context.Context, vnet, within string) (cidr, gateway, first, last string, ok bool, err error) {
	zone, err := s.vnetZone(ctx, vnet)
	if err != nil {
		return "", "", "", "", false, err
	}
	if zone.DHCP != "" {
		return "", "", "", "", false, nil
	}
	subs, err := s.C.ListSDNSubnets(ctx, vnet)
	if err != nil {
		return "", "", "", "", false, err
	}
	if len(subs) == 0 {
		return "", "", "", "", false, fmt.Errorf("pve: vnet %s has no subnet", vnet)
	}
	sub := subs[0]
	if within != "" {
		found := false
		for _, c := range subs {
			if SubnetHolds(c.CIDR, within) {
				sub, found = c, true
				break
			}
		}
		if !found {
			return "", "", "", "", false, conflictf("Network %s has no subnet %s", vnet, within)
		}
	}
	_, ipnet, err := net.ParseCIDR(sub.CIDR)
	if err != nil {
		return "", "", "", "", false, fmt.Errorf("pve: vnet %s subnet %q: %w", vnet, sub.CIDR, err)
	}
	first, last = staticHostRange(ipnet, sub.Gateway)
	if first == "" {
		return "", "", "", "", false, fmt.Errorf("pve: vnet %s subnet %q has no usable static IPv4 host range", vnet, sub.CIDR)
	}
	return sub.CIDR, sub.Gateway, first, last, true, nil
}

// VnetInUse reports whether any pool VM NIC sits on the vnet (delete guard).
func (s *Service) VnetInUse(ctx context.Context, projectID, vnetID string) (bool, error) {
	vms, _, unread, err := s.poolVMs(ctx, projectID)
	if err != nil {
		return false, err
	}
	if len(unread) > 0 {
		return false, errUnreadVMs()
	}
	for _, vm := range vms {
		cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
		if err != nil {
			return false, err
		}
		for key := range cfg {
			if strings.HasPrefix(key, "net") && strings.Contains(cfg.Str(key), "bridge="+vnetID) {
				return true, nil
			}
		}
	}
	return false, nil
}

// --- NETWORK sync provider ---

type NetworkSyncProvider struct {
	svc       *Service
	region    string
	projectID string
}

func (s *Service) NetworkSyncProvider(region, projectID string) *NetworkSyncProvider {
	return &NetworkSyncProvider{svc: s, region: region, projectID: projectID}
}

func (p *NetworkSyncProvider) Type() string          { return cloud.TypeNetwork }
func (p *NetworkSyncProvider) ProjectID() string     { return p.projectID }
func (p *NetworkSyncProvider) CompareKeys() []string { return []string{} }

// List returns the project's vnets (ownership-alias filtered) with their subnet
// detail, shaped for the FE networks pages.
func (p *NetworkSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	vnets, err := p.svc.C.ListSDNVnets(ctx)
	if err != nil {
		return nil, networkInventoryError("", "cannot list vnets", err)
	}
	now := nowUTC()
	var out []cloud.CloudResource
	var zoneOf map[string]SDNZone // read once, at the project's first vnet
	for _, v := range vnets {
		pid, name, ok := aliasProject(v.Alias)
		if !ok || pid != p.projectID {
			continue
		}
		if zoneOf == nil {
			zones, err := p.svc.C.ListSDNZones(ctx)
			if err != nil {
				return nil, networkInventoryError("", "cannot list zones", err)
			}
			zoneOf = map[string]SDNZone{}
			for _, z := range zones {
				zoneOf[z.Zone] = z
			}
		}
		subs, err := p.svc.C.ListSDNSubnets(ctx, v.Vnet)
		if err != nil {
			return nil, networkInventoryError(v.Vnet, "cannot read subnets", err)
		}
		detail := SubnetDetail(subs, zoneOf[v.Zone])
		out = append(out, cloud.CloudResource{
			ExternalID: v.Vnet,
			ProjectID:  p.projectID,
			Region:     p.region,
			Type:       cloud.TypeNetwork,
			Data:       NetworkData(v.Vnet, name, v.Zone, detail),
			Info:       &cloud.Info{UpdatedAt: &now},
		})
	}
	return out, nil
}

// NetworkDetail reads a vnet's subnets and its zone into the data.network sub-shape (SubnetDetail).
func (s *Service) NetworkDetail(ctx context.Context, vnet string) (map[string]any, error) {
	zone, err := s.vnetZone(ctx, vnet)
	if err != nil {
		return nil, err
	}
	subs, err := s.C.ListSDNSubnets(ctx, vnet)
	if err != nil {
		return nil, err
	}
	return SubnetDetail(subs, zone), nil
}

// SubnetDetail renders a vnet's subnets for data.network: the top-level cidr/gateway/snat/dhcp describe the first
// subnet, subnetDetails all of them. zone is the vnet's live zone, and it decides with the subnet. In a VLAN zone
// Proxmox does no SNAT, and in a zone without a DHCP server it hands out no address, whatever the subnet says (one
// written before CreateNetwork left the dhcp-range out there still carries it): the answer is false.
func SubnetDetail(subs []SDNSubnet, zone SDNZone) map[string]any {
	vlan := zone.Type == "vlan"
	det := map[string]any{}
	slices.SortFunc(subs, func(a, b SDNSubnet) int { return cmp.Compare(a.CIDR, b.CIDR) })
	cidrs := make([]any, 0, len(subs))
	rows := make([]any, 0, len(subs))
	for i, sub := range subs {
		cidrs = append(cidrs, sub.CIDR)
		snat := !vlan && sub.SNAT == 1
		rows = append(rows, map[string]any{"cidr": sub.CIDR, "gateway": sub.Gateway, "snat": snat})
		if i == 0 {
			det["cidr"], det["gateway"] = sub.CIDR, sub.Gateway
			det["snat"], det["dhcp"] = snat, zone.DHCP != "" && len(sub.DHCPRange) > 0
		}
	}
	det["subnets"], det["subnetDetails"] = cidrs, rows
	return det
}

// AddSubnet stages one more subnet on an owned vnet. On a vnet with DHCP it gets a range too, or
// its addresses would never be handed out. The caller holds the lock, reconciles isolation, applies.
func (s *Service) AddSubnet(ctx context.Context, projectID, vnet, cidr string, snat bool) (*SDNSubnet, error) {
	if err := s.ownedVnet(ctx, projectID, vnet); err != nil {
		return nil, err
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("pve network: bad cidr %q", cidr)
	}
	gw := firstHost(ipnet)
	o := SubnetOpts{CIDR: cidr, Gateway: gw, SNAT: snat}
	_, _, _, _, static, err := s.VnetStaticPool(ctx, vnet)
	if err != nil {
		return nil, err
	}
	if !static {
		o.DHCPStart, o.DHCPEnd = dhcpRange(ipnet, gw)
	}
	if err := s.C.CreateSDNSubnet(ctx, vnet, o); err != nil {
		return nil, err
	}
	return &SDNSubnet{CIDR: cidr, Gateway: gw}, nil
}

// RemoveSubnet stages the removal of a subnet of an owned vnet; a vnet keeps at least one.
func (s *Service) RemoveSubnet(ctx context.Context, projectID, vnet, cidr string) error {
	sub, count, err := s.ownedSubnet(ctx, projectID, vnet, cidr)
	if err != nil {
		return err
	}
	if count < 2 {
		return conflictf("A network keeps at least one subnet. Delete the network instead")
	}
	return s.C.DeleteSDNSubnet(ctx, vnet, sub.Subnet)
}

// SetSubnetSNAT stages the outbound-internet switch of a subnet.
func (s *Service) SetSubnetSNAT(ctx context.Context, projectID, vnet, cidr string, snat bool) error {
	sub, _, err := s.ownedSubnet(ctx, projectID, vnet, cidr)
	if err != nil {
		return err
	}
	return s.C.UpdateSDNSubnetSNAT(ctx, vnet, sub.Subnet, snat)
}

func (s *Service) ownedVnet(ctx context.Context, projectID, vnet string) error {
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return err
	}
	for _, v := range vnets {
		if v.Vnet == vnet {
			if pid, _, ok := aliasProject(v.Alias); ok && pid == projectID {
				return nil
			}
			break
		}
	}
	return fmt.Errorf("pve network: vnet %s is not owned by project %s", vnet, projectID)
}

func (s *Service) ownedSubnet(ctx context.Context, projectID, vnet, cidr string) (*SDNSubnet, int, error) {
	if err := s.ownedVnet(ctx, projectID, vnet); err != nil {
		return nil, 0, err
	}
	subs, err := s.C.ListSDNSubnets(ctx, vnet)
	if err != nil {
		return nil, 0, err
	}
	for i := range subs {
		if subs[i].CIDR == cidr {
			return &subs[i], len(subs), nil
		}
	}
	return nil, 0, conflictf("Network %s has no subnet %s", vnet, cidr)
}

// SubnetHolds reports whether within (a CIDR or an address) is this subnet or inside it.
func SubnetHolds(cidr, within string) bool {
	if cidr == within {
		return true
	}
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(within)
	return ip != nil && n.Contains(ip)
}

// NetworkData builds the canonical data.network payload (create + sync share it).
func NetworkData(vnet, name, zone string, detail map[string]any) map[string]any {
	netMap := map[string]any{
		"id": vnet, "vnet": vnet, "name": name, "zone": zone,
		"status": "ACTIVE", "subnets": []any{},
	}
	for k, v := range detail {
		netMap[k] = v
	}
	return map[string]any{"networkName": name, "network": netMap}
}

// --- helpers ---

// firstHost is the .1 of a CIDR (the SDN gateway convention).
func firstHost(ipnet *net.IPNet) string {
	ip := ipnet.IP.To4()
	if ip == nil {
		return ""
	}
	out := make(net.IP, 4)
	copy(out, ip)
	out[3]++
	return out.String()
}

// staticHostRange is the platform-assigned IPv4 span, not a PVE DHCP range.
// Keep the first ten addresses reserved on /28 and larger subnets; smaller
// subnets use all hosts. Trim a gateway at either end; pickPublicIP skips one
// inside the span, along with addresses already allocated there.
func staticHostRange(ipnet *net.IPNet, gateway string) (string, string) {
	if ipnet == nil {
		return "", ""
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 || ones > 30 {
		return "", "" // IPv4 network + broadcast must leave usable hosts
	}
	ip := ipnet.IP.Mask(ipnet.Mask).To4()
	if ip == nil {
		return "", ""
	}
	u32 := func(ip net.IP) uint32 {
		return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	}
	base := u32(ip)
	first := base + 1
	if ones <= 28 {
		first = base + 10
	}
	last := (base | (^uint32(0) >> ones)) - 1
	if gw := net.ParseIP(gateway).To4(); gw != nil {
		n := u32(gw)
		if n == first {
			first++
		}
		if n == last {
			last--
		}
	}
	if first > last {
		return "", ""
	}
	addr := func(n uint32) string {
		return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)).String()
	}
	return addr(first), addr(last)
}

// dhcpRange keeps PVE's DHCP sizing policy: network+10 through the last host,
// only on /28 or larger IPv4 subnets. Static allocation has no such minimum.
func dhcpRange(ipnet *net.IPNet, gateway string) (string, string) {
	ip := ipnet.IP.To4()
	if ip == nil {
		return "", ""
	}
	ones, bits := ipnet.Mask.Size()
	size := 1 << (bits - ones)
	if size < 16 {
		return "", "" // too small for a sane pool
	}
	start := make(net.IP, 4)
	copy(start, ip)
	start[3] += 10
	last := make(net.IP, 4)
	copy(last, ip)
	n := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	n += uint32(size) - 2 // last host
	last[0], last[1], last[2], last[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	return start.String(), last.String()
}

const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"

func randBase36(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = base36[rand.Intn(len(base36))]
	}
	return string(b)
}

// SplitOwnershipAlias exposes the ownership-alias parse for tooling (drivers,
// operator scripts). ok=false for foreign/unmanaged vnets.
func SplitOwnershipAlias(alias string) (projectID, name string, ok bool) {
	return aliasProject(alias)
}
