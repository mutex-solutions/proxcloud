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
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
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
			"dhcp":       "dnsmasq",
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
	// The vnet's isolation rules go with it (not SDN-staged — removed at once).
	if err := s.removeVnetIsolation(ctx, vnetID); err != nil {
		return err
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

// TeardownProjectNetworks stages removal of every vnet the project owns (its
// isolation rules go at once) and reports whether anything was staged. Project
// teardown: the pool's VMs are already gone. The caller holds the sdn-apply lock
// and applies.
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
	return staged, nil
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
//   - "shared" (default): every tenant vnet lives in the one default zone; each
//     vnet carries forward rules that ACCEPT the project's own subnets and DROP
//     anything else inside the cidrPool, so tenants cannot reach each other.
//     Non-overlapping, in-pool CIDRs are enforced by the platform at create.
//   - "evpn-per-project": each project gets its own EVPN zone (= its own VRF),
//     id ZoneIDFor(projectID). The same vnet rules still apply — exit nodes leak
//     every VRF into the default table, so the VRF alone is not a wall.

const (
	IsolationShared = "shared"
	IsolationEVPN   = "evpn-per-project"
)

// isolationComment marks the platform-managed vnet rules (operator rules are
// left alone).
const isolationComment = "proxcloud:isolation"

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

// isolationRules is the managed rule set for one vnet, top to bottom: allow the
// project's own subnets among themselves, then drop anything else headed into the
// pool. Rules are FORWARD, so they see guest<->host traffic in both directions:
// our VM -> another tenant has dest in the pool, and another tenant -> our VM
// has dest = our subnet, also in the pool — both dropped, on both vnets, so a
// stale allow-list on one side still cannot open a path. There is deliberately
// no "DROP source=pool": every tenant source is in the pool, so it would also
// drop the VM's own egress to the internet (SNAT); replies pass as established.
// ponytail: a vnet outside the pool (made before the pool was enforced) can
// still be reached FROM a tenant (not into one); reconcile/migrate those vnets.
// TODO(verify-lab): on the nftables firewall, (a) routed traffic between two
// vnets of the shared zone is dropped by these rules, (b) a VM still reaches the
// internet through SNAT, and (c) rules can be written on a vnet that is staged
// but not yet applied (create reconciles before the apply).
func isolationRules(projectCIDRs []string, pool string) []FirewallRule {
	var out []FirewallRule
	if len(projectCIDRs) > 0 { // an empty source/dest would mean "any"
		own := strings.Join(projectCIDRs, ",")
		out = append(out, FirewallRule{Type: "forward", Action: "ACCEPT", Source: own, Dest: own, Comment: isolationComment})
	}
	return append(out, FirewallRule{Type: "forward", Action: "DROP", Dest: pool, Comment: isolationComment})
}

// ReconcileIsolation brings every vnet of the project to isolationRules(project
// subnets, pool) and switches its firewall on. Idempotent; call after a vnet of
// the project is created or deleted (the allow-list spans all its subnets).
func (s *Service) ReconcileIsolation(ctx context.Context, projectID, pool string) error {
	_, poolNet, err := net.ParseCIDR(pool)
	if err != nil {
		return fmt.Errorf("pve isolation: invalid cidrPool %q", pool)
	}
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return err
	}
	var own, cidrs []string
	for _, v := range vnets {
		if pid, _, ok := aliasProject(v.Alias); !ok || pid != projectID || v.State == "deleted" {
			continue
		}
		own = append(own, v.Vnet)
		subs, err := s.C.ListSDNSubnets(ctx, v.Vnet)
		if err != nil {
			return err
		}
		for _, sub := range subs {
			cidrs = append(cidrs, sub.CIDR)
		}
	}
	want := isolationRules(cidrs, poolNet.String())
	for _, vnet := range own {
		if err := s.ensureVnetRules(ctx, vnet, want); err != nil {
			return fmt.Errorf("pve isolation %s: %w", vnet, err)
		}
	}
	return nil
}

// ensureVnetRules rewrites the managed rules only when they differ from want.
func (s *Service) ensureVnetRules(ctx context.Context, vnet string, want []FirewallRule) error {
	managed, err := s.managedVnetRules(ctx, vnet)
	if err != nil {
		return err
	}
	same := len(managed) == len(want)
	for i := 0; same && i < len(want); i++ {
		m, w := managed[i], want[i]
		same = m.Type == w.Type && m.Action == w.Action && m.Source == w.Source && m.Dest == w.Dest
	}
	if !same {
		if err := s.deleteVnetRules(ctx, vnet, managed); err != nil {
			return err
		}
		// PVE prepends each new rule, so add bottom-up to land want[0] on top.
		// Should it append instead, the DROPs end up first: fail closed, not open.
		for i := len(want) - 1; i >= 0; i-- {
			if err := s.C.AddVnetFirewallRule(ctx, vnet, want[i]); err != nil {
				return err
			}
		}
	}
	return s.C.EnableVnetFirewall(ctx, vnet)
}

func (s *Service) managedVnetRules(ctx context.Context, vnet string) ([]FirewallRule, error) {
	rules, err := s.C.ListVnetFirewallRules(ctx, vnet)
	if err != nil {
		return nil, err
	}
	var out []FirewallRule
	for _, r := range rules {
		if r.Comment == isolationComment {
			out = append(out, r)
		}
	}
	return out, nil
}

// deleteVnetRules removes rules bottom-up so the remaining positions stay valid
// (rules arrive in ascending pos order).
func (s *Service) deleteVnetRules(ctx context.Context, vnet string, rules []FirewallRule) error {
	for i := len(rules) - 1; i >= 0; i-- {
		if err := s.C.DeleteVnetFirewallRule(ctx, vnet, rules[i].Pos); err != nil {
			return err
		}
	}
	return nil
}

// removeVnetIsolation drops the vnet's managed rules (idempotent on a vnet with
// no firewall config).
func (s *Service) removeVnetIsolation(ctx context.Context, vnet string) error {
	managed, err := s.managedVnetRules(ctx, vnet)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	return s.deleteVnetRules(ctx, vnet, managed)
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

// VnetInUse reports whether any pool VM NIC sits on the vnet (delete guard).
func (s *Service) VnetInUse(ctx context.Context, projectID, vnetID string) (bool, error) {
	vms, _, err := s.poolVMs(ctx, projectID)
	if err != nil {
		return false, err
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

// subnetDetail reads a vnet's subnets into the data.network sub-shape.
func (p *NetworkSyncProvider) subnetDetail(ctx context.Context, vnet string) map[string]any {
	det := map[string]any{}
	if subs, err := p.svc.C.ListSDNSubnets(ctx, vnet); err == nil {
		cidrs := make([]any, 0, len(subs))
		for _, sub := range subs {
			cidrs = append(cidrs, sub.CIDR)
			det["cidr"] = sub.CIDR
			det["gateway"] = sub.Gateway
			det["snat"] = sub.SNAT == 1
			det["dhcp"] = len(sub.DHCPRange) > 0
		}
		det["subnets"] = cidrs
	}
	return det
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

// dhcpRange returns [.10 .. last-1] of a v4 subnet, clear of the gateway.
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
