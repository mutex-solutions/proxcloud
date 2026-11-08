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
		return s.C.CreateSDNZone(ctx, z.Key, "simple", map[string]string{
			"ipam": "pve", "dhcp": "dnsmasq",
		})
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

// CreateNetworkReq drives CreateNetwork. CIDR is required here (the platform
// auto-carves before calling).
type CreateNetworkReq struct {
	ProjectID string
	Name      string
	Zone      ZoneConfig
	CIDR      string
	Gateway   string // "" → first host of the CIDR
	DHCP      bool
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
	sub := SubnetOpts{CIDR: req.CIDR, Gateway: gw, SNAT: req.SNAT}
	if req.DHCP {
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
	want, owner, poolNet, complete, err := s.isolationWant(ctx, pool, blocked, sandbox, allow)
	if err != nil {
		return err
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
	// With a node down the list may miss a network only it knew, so DROPs in place stay until it is
	// back. The project's own ACCEPTs below do not depend on it.
	if !slices.EqualFunc(mine, want, sameLiveRule) && (complete || len(mine) == 0) {
		if err := s.rebuildIsolation(ctx, rules, want, owner, poolNet); err != nil {
			return err
		}
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

// isolationWant is this portal's provider-wide isolation rule set (isolationRules over the management networks plus
// blocked). complete is false when a node did not report its networks.
func (s *Service) isolationWant(ctx context.Context, pool string, blocked, sandbox, allow []string) (want []FirewallRule, owner string, poolNet *net.IPNet, complete bool, err error) {
	if _, poolNet, err = net.ParseCIDR(pool); err != nil {
		return nil, "", nil, false, fmt.Errorf("pve isolation: invalid cidrPool %q", pool)
	}
	mgmt, complete, err := s.C.ManagementCIDRs(ctx)
	if err != nil {
		return nil, "", nil, false, err
	}
	blocked = slices.Compact(slices.Sorted(slices.Values(append(mgmt, blocked...))))
	if sandbox, err = sandboxNets(poolNet, blocked, sandbox); err != nil {
		return nil, "", nil, false, err
	}
	owner = s.isolationOwner()
	return isolationRules(poolNet.String(), blocked, sandbox, owner, allow...), owner, poolNet, complete, nil
}

// CheckSandboxIsolation is the isolation release gate of a sandbox provider (docs/managed-sandbox.md §3): read-only,
// with the portal's token, it checks that the datacenter firewall is on and that this portal's isolation DROPs are in
// the cluster firewall, enabled, exactly as ReconcileIsolation writes them for these sandbox networks: the tenant pool
// may not reach them, and they reach neither the pool nor a blocked network.
func (s *Service) CheckSandboxIsolation(ctx context.Context, pool string, blocked, sandbox []string, allow ...string) error {
	if len(sandbox) == 0 {
		return errors.New("pve isolation: network.sandboxCidrs is empty, so no rule fences the sandbox network")
	}
	want, owner, _, complete, err := s.isolationWant(ctx, pool, blocked, sandbox, allow)
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
	if !slices.EqualFunc(mine, want, sameRule) {
		return fmt.Errorf("pve isolation: the %s rules in the cluster firewall are not the ones for sandbox networks %s (%d in place, %d wanted); the next sync rewrites them",
			owner, strings.Join(sandbox, ","), len(mine), len(want))
	}
	return nil
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

// VnetStaticPool returns the address pool of a vnet whose zone has no DHCP (PVE offers DHCP on
// simple zones only, so EVPN and VLAN zones): servers there get a fixed address over cloud-init,
// allocated by the platform. ok=false means the zone's own DHCP hands out addresses.
func (s *Service) VnetStaticPool(ctx context.Context, vnet string) (cidr, gateway, first, last string, ok bool, err error) {
	return s.VnetStaticPoolFor(ctx, vnet, "")
}

// VnetStaticPoolFor is VnetStaticPool on one subnet of the vnet: the one with this CIDR or holding
// this address ("" = the first).
func (s *Service) VnetStaticPoolFor(ctx context.Context, vnet, within string) (cidr, gateway, first, last string, ok bool, err error) {
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return "", "", "", "", false, err
	}
	zone := ""
	for _, v := range vnets {
		if v.Vnet == vnet {
			zone = v.Zone
		}
	}
	zones, err := s.C.ListSDNZones(ctx)
	if err != nil {
		return "", "", "", "", false, err
	}
	for _, z := range zones {
		if z.Zone == zone && z.DHCP != "" {
			return "", "", "", "", false, nil
		}
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
	first, last = dhcpRange(ipnet, sub.Gateway)
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
		return nil, err
	}
	now := nowUTC()
	var out []cloud.CloudResource
	for _, v := range vnets {
		pid, name, ok := aliasProject(v.Alias)
		if !ok || pid != p.projectID {
			continue
		}
		out = append(out, cloud.CloudResource{
			ExternalID: v.Vnet,
			ProjectID:  p.projectID,
			Region:     p.region,
			Type:       cloud.TypeNetwork,
			Data:       NetworkData(v.Vnet, name, v.Zone, p.subnetDetail(ctx, v.Vnet)),
			Info:       &cloud.Info{UpdatedAt: &now},
		})
	}
	return out, nil
}

// subnetDetail reads a vnet's subnets into the data.network sub-shape. The top-level
// cidr/gateway/snat describe the first subnet, subnetDetails all of them.
func (p *NetworkSyncProvider) subnetDetail(ctx context.Context, vnet string) map[string]any {
	det := map[string]any{}
	if subs, err := p.svc.C.ListSDNSubnets(ctx, vnet); err == nil {
		det = SubnetDetail(subs)
	}
	return det
}

// SubnetDetail renders a vnet's subnets for data.network.
func SubnetDetail(subs []SDNSubnet) map[string]any {
	det := map[string]any{}
	slices.SortFunc(subs, func(a, b SDNSubnet) int { return cmp.Compare(a.CIDR, b.CIDR) })
	cidrs := make([]any, 0, len(subs))
	rows := make([]any, 0, len(subs))
	for i, sub := range subs {
		cidrs = append(cidrs, sub.CIDR)
		rows = append(rows, map[string]any{"cidr": sub.CIDR, "gateway": sub.Gateway, "snat": sub.SNAT == 1})
		if i == 0 {
			det["cidr"], det["gateway"] = sub.CIDR, sub.Gateway
			det["snat"], det["dhcp"] = sub.SNAT == 1, len(sub.DHCPRange) > 0
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

// dhcpRange returns [.10 .. last host] of a v4 subnet, clear of the gateway.
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
