package pve

// lbpair.go — a load balancer on two appliances. keepalived runs VRRPv3 between them, unicast over the tenant NIC
// (net0), both in state BACKUP with equal priority and nopreempt: the one that holds the public addresses keeps them
// until it fails. Those addresses (VIPs) live on the public NIC (net1), which has no address of its own and no
// cloud-init config. Both appliances run the same HAProxy config, bound to the VIPs. Written for keepalived 2.3.x
// (Debian 13 ships 2.3.3, Ubuntu 26.04 2.3.4) and checked with `keepalived -t` on both
// (test/integration/lb_keepalived_test.go).

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// LBKeepalived is one appliance's side of the pair.
type LBKeepalived struct {
	RouterID    int    // virtual_router_id, 1 to 255
	Self, Peer  string // the two appliances' tenant addresses
	TenantIface string // net0 in the guest
	PublicIface string // net1 in the guest
	VIPs        []string
	Prefix      int    // the public network's prefix length
	Gateway     string // the public network's gateway: replies from a VIP leave through it
}

const (
	lbCheckScript  = "/usr/local/sbin/proxcloud-lb-check"
	lbNotifyScript = "/usr/local/sbin/proxcloud-lb-notify"
	lbStateFile    = "/run/proxcloud-lb.state"
	// lbVIPTable is the routing table of traffic from a VIP: its default route is the public gateway, while the
	// main table keeps the tenant gateway for the appliance's own traffic (apt at first boot).
	lbVIPTable = 100
	// lbPlaceholderVIP stands in when a pair serves no address: keepalived refuses an instance without one in
	// virtual_ipaddress (2.3.3 and 2.3.4: "No VIP specified", spike S1). On loopback it answers nobody.
	lbPlaceholderVIP = "192.0.2.1/32 dev lo"
)

var lbIfaceRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// RenderKeepalived renders keepalived.conf for one appliance. Every value is checked here, since it comes from a
// stored row and from the guest: an address must parse, an interface name must be a plain name.
func RenderKeepalived(k LBKeepalived) (string, error) {
	self, err1 := netip.ParseAddr(k.Self)
	peer, err2 := netip.ParseAddr(k.Peer)
	switch {
	case err1 != nil || err2 != nil || !self.Is4() || !peer.Is4() || self == peer:
		return "", fmt.Errorf("keepalived: bad tenant addresses %q and %q", k.Self, k.Peer)
	case !lbIfaceRe.MatchString(k.TenantIface) || !lbIfaceRe.MatchString(k.PublicIface) || k.TenantIface == k.PublicIface:
		return "", fmt.Errorf("keepalived: bad interfaces %q and %q", k.TenantIface, k.PublicIface)
	case k.RouterID < 1 || k.RouterID > 255:
		return "", fmt.Errorf("keepalived: virtual router id %d out of range", k.RouterID)
	}
	var vips []netip.Addr
	for _, v := range k.VIPs {
		a, err := netip.ParseAddr(v)
		if err != nil || !a.Is4() {
			return "", fmt.Errorf("keepalived: bad address %q", v)
		}
		vips = append(vips, a)
	}
	var gw netip.Addr
	if len(vips) > 0 {
		var err error
		if gw, err = netip.ParseAddr(k.Gateway); err != nil || !gw.Is4() || k.Prefix < 1 || k.Prefix > 32 {
			return "", fmt.Errorf("keepalived: bad public gateway %q or prefix %d", k.Gateway, k.Prefix)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, `global_defs {
  router_id proxcloud-lb
  vrrp_version 3
  enable_script_security
  script_user root
}

vrrp_script proxcloud_haproxy {
  script "%s"
  interval 1
  fall 2
  rise 2
}

vrrp_instance lb {
  state BACKUP
  interface %s
  virtual_router_id %d
  priority 100
  advert_int 1
  nopreempt
  unicast_src_ip %s
  unicast_peer {
    %s
  }
  check_unicast_src
  garp_master_refresh 5
  track_interface {
    %s
  }
  track_script {
    proxcloud_haproxy
  }
`, lbCheckScript, k.TenantIface, k.RouterID, self, peer, k.PublicIface)
	// One address in virtual_ipaddress, the rest excluded from the advert: a VRRPv3 advert carries at most a
	// few dozen addresses, and keepalived needs at least one there (S1).
	b.WriteString("  virtual_ipaddress {\n")
	if len(vips) == 0 {
		b.WriteString("    " + lbPlaceholderVIP + "\n")
	} else {
		fmt.Fprintf(&b, "    %s/%d dev %s\n", vips[0], k.Prefix, k.PublicIface)
	}
	b.WriteString("  }\n")
	if len(vips) > 1 {
		b.WriteString("  virtual_ipaddress_excluded {\n")
		for _, v := range vips[1:] {
			fmt.Fprintf(&b, "    %s/%d dev %s\n", v, k.Prefix, k.PublicIface)
		}
		b.WriteString("  }\n")
	}
	if len(vips) > 0 {
		// Replies from a VIP leave through the public NIC; keepalived needs the preference stated (S1).
		b.WriteString("  virtual_rules {\n")
		for _, v := range vips {
			fmt.Fprintf(&b, "    from %s/32 table %d preference %d\n", v, lbVIPTable, lbVIPTable)
		}
		b.WriteString("  }\n  virtual_routes {\n")
		fmt.Fprintf(&b, "    %s dev %s scope link table %d\n", netip.PrefixFrom(vips[0], k.Prefix).Masked(), k.PublicIface, lbVIPTable)
		fmt.Fprintf(&b, "    default via %s dev %s table %d\n", gw, k.PublicIface, lbVIPTable)
		b.WriteString("  }\n")
	}
	fmt.Fprintf(&b, "  notify %s\n}\n", lbNotifyScript)
	return b.String(), nil
}

// lbPairFiles are what keepalived needs besides its config, written with every config change.
func lbPairFiles(publicIface string) map[string]string {
	return map[string]string{
		// The standby's HAProxy binds VIPs it does not hold; rp_filter loose, as a reply's route and its
		// request's interface may differ.
		"/etc/sysctl.d/90-proxcloud-lb.conf": "net.ipv4.ip_nonlocal_bind = 1\nnet.ipv4.conf.all.rp_filter = 2\nnet.ipv4.conf.default.rp_filter = 2\n",
		// No cloud-init config names the public NIC, so nothing else brings its link up.
		"/etc/systemd/system/keepalived.service.d/proxcloud.conf": "[Service]\nExecStartPre=/usr/sbin/ip link set dev " + publicIface + " up\n",
		// The track script: HAProxy's master answers and has a worker. A stopped HAProxy puts the appliance in
		// FAULT, and the peer takes over.
		lbCheckScript: "#!/bin/sh\nout=$(echo 'show proc' | socat -t1 - UNIX-CONNECT:/run/haproxy-master.sock 2>/dev/null) || exit 1\necho \"$out\" | grep -qw worker\n",
		// keepalived calls it with TYPE NAME STATE PRIORITY; the portal reads the state back.
		lbNotifyScript: "#!/bin/sh\necho \"$3\" > " + lbStateFile + "\n",
	}
}

// LBPairFilesDigest is what the support files add to the hash of a keepalived config: a release that changes one
// pushes it again.
func LBPairFilesDigest(publicIface string) string {
	files := lbPairFiles(publicIface)
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(files)) {
		b.WriteString(k + "\n" + files[k])
	}
	return b.String()
}

const (
	// lbPairInstall is the pair's own install line; a single appliance keeps haproxyInstall (C13).
	lbPairInstall = "pgrep -x apt-get >/dev/null || { apt-get update -qq; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq haproxy socat keepalived; }"
	lbPairReady   = "command -v haproxy && command -v socat && command -v keepalived"
	// lbKeepalivedApply runs after the files are written: keepalived checks the new config before it is moved in, and
	// a running keepalived reloads, which keeps its state.
	lbKeepalivedApply = `set -e
chown root:root ` + lbCheckScript + ` ` + lbNotifyScript + `
chmod 0755 ` + lbCheckScript + ` ` + lbNotifyScript + `
sysctl -q -p /etc/sysctl.d/90-proxcloud-lb.conf
systemctl daemon-reload
F=/etc/keepalived/keepalived.conf
keepalived -t -f $F.new
mv $F.new $F
systemctl enable -q keepalived
if systemctl is-active -q keepalived; then systemctl reload keepalived; else systemctl start keepalived; fi`
)

// PushPairHAProxy installs config on an appliance of a pair, and the pair's packages first if the image lacks them.
func (s *Service) PushPairHAProxy(ctx context.Context, node string, vmid int, config string) error {
	if err := s.lbReady(ctx, node, vmid, lbPairReady, lbPairInstall); err != nil {
		return err
	}
	return s.swapHAProxy(ctx, node, vmid, config)
}

// PushKeepalived writes keepalived's config and support files on an appliance of a pair and (re)loads it. A config
// keepalived -t refuses leaves the running one in place.
func (s *Service) PushKeepalived(ctx context.Context, node string, vmid int, config, publicIface string) error {
	if !lbIfaceRe.MatchString(publicIface) {
		return fmt.Errorf("keepalived: bad interface %q", publicIface)
	}
	if err := s.lbReady(ctx, node, vmid, lbPairReady, lbPairInstall); err != nil {
		return err
	}
	exit, _, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "mkdir", "-p", "/etc/systemd/system/keepalived.service.d", "/etc/keepalived")
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("keepalived: %s", strings.TrimSpace(stderr))
	}
	files := lbPairFiles(publicIface)
	files["/etc/keepalived/keepalived.conf.new"] = config
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if err := s.C.GuestWriteFile(ctx, node, vmid, path, files[path]); err != nil {
			return err
		}
	}
	exit, _, stderr, err = s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", lbKeepalivedApply)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("keepalived refused the change, the last config stays in use: %s", strings.TrimSpace(stderr))
	}
	return nil
}

// StopKeepalived makes an appliance give up the VIPs at once: keepalived sends a priority 0 advert on its way out,
// and the peer takes over after the skew time, about 0.6 s (the planned switch of a replace).
func (s *Service) StopKeepalived(ctx context.Context, node string, vmid int) error {
	exit, _, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "systemctl", "stop", "keepalived")
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("stop keepalived: %s", strings.TrimSpace(stderr))
	}
	return nil
}

// VRRPState is one appliance as the guest says it is.
type VRRPState struct {
	State    string // MASTER, BACKUP, FAULT, STOPPED (keepalived not running), UNKNOWN
	HAProxy  bool   // the track script passes
	HoldsVIP bool   // the public NIC carries a VIP
}

// ReadVRRPState asks an appliance for its VRRP state, whether HAProxy runs, and whether it holds the first VIP.
func (s *Service) ReadVRRPState(ctx context.Context, node string, vmid int, vip string) (VRRPState, error) {
	holds := "v=0"
	if a, err := netip.ParseAddr(vip); err == nil && a.Is4() {
		holds = "ip -o -4 addr show | grep -qF ' " + a.String() + "/' && v=1 || v=0"
	}
	script := "s=$(cat " + lbStateFile + " 2>/dev/null); systemctl is-active -q keepalived || s=STOPPED; " +
		lbCheckScript + " && h=1 || h=0; " + holds + "; echo \"${s:-UNKNOWN} $h $v\""
	exit, out, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", script)
	if err != nil {
		return VRRPState{}, err
	}
	if exit != 0 {
		return VRRPState{}, fmt.Errorf("vrrp state: %s", strings.TrimSpace(stderr))
	}
	return ParseVRRPState(out), nil
}

// ParseVRRPState reads the line ReadVRRPState's script prints.
func ParseVRRPState(out string) VRRPState {
	f := strings.Fields(out)
	st := VRRPState{State: "UNKNOWN"}
	if len(f) == 3 {
		switch f[0] {
		case "MASTER", "BACKUP", "FAULT", "STOP", "STOPPED":
			st.State = f[0]
		}
		st.HAProxy, st.HoldsVIP = f[1] == "1", f[2] == "1"
	}
	if st.State == "STOP" {
		st.State = "STOPPED"
	}
	return st
}

// LBIfaces names the guest's interfaces of net0 and net1, matched by MAC through the guest agent.
func (s *Service) LBIfaces(ctx context.Context, node string, vmid int) (tenant, public string, err error) {
	cfg, err := s.C.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return "", "", err
	}
	ifs, err := s.C.AgentNetworkInterfaces(ctx, node, vmid)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrLBNotReady, err)
	}
	byMAC := map[string]string{}
	for _, i := range ifs {
		byMAC[strings.ToLower(i.HardwareMAC)] = i.Name
	}
	tenant, public = byMAC[strings.ToLower(nicMAC(cfg.Str("net0")))], byMAC[strings.ToLower(nicMAC(cfg.Str("net1")))]
	if !lbIfaceRe.MatchString(tenant) || !lbIfaceRe.MatchString(public) {
		return "", "", fmt.Errorf("%w: the guest does not show both NICs yet", ErrLBNotReady)
	}
	return tenant, public, nil
}

// SDNZoneOf says whether bridge is an SDN vnet, and the type of its zone ("simple", "vlan", "evpn", ...).
func (s *Service) SDNZoneOf(ctx context.Context, bridge string) (zoneType string, vnet bool, err error) {
	vnets, err := s.C.ListSDNVnets(ctx)
	if err != nil {
		return "", false, err
	}
	i := slices.IndexFunc(vnets, func(v SDNVnet) bool { return v.Vnet == bridge })
	if i < 0 {
		return "", false, nil
	}
	zones, err := s.C.ListSDNZones(ctx)
	if err != nil {
		return "", true, err
	}
	for _, z := range zones {
		if z.Zone == vnets[i].Zone {
			return z.Type, true, nil
		}
	}
	return "", true, nil
}
