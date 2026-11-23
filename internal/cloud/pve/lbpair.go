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
	Self, Peer  string // the two appliances' tenant addresses; no Peer: the appliance is alone (lbNoPeer)
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
	// lbNoPeer is the unicast peer of an appliance that has none: the first of a pair until the second is built,
	// or the one a replace left. It hears no advert, so it becomes master and holds the addresses, and its own
	// adverts reach no keepalived: they are addressed to a loopback address, which no host takes from a NIC and no
	// router forwards. What is on the tenant network meanwhile follows the appliance's routes. With a default
	// route through the tenant NIC, which an appliance has, one advert a second goes to the tenant gateway's
	// Ethernet address alone, and the gateway drops it; no other host of the network is sent it. With no such
	// route the kernel asks for 127.0.0.1 by ARP once a second and nothing leaves but that request, which no host
	// answers. Leaving unicast_peer out is not the way: keepalived 2.3 then
	// falls back to multicast (and -t refuses the file), and multicast adverts meet every VRRP instance of the
	// network with the same router id, a tenant's own among them (lbRouterID is one of 255). When the peer comes
	// the config names it and keepalived reloads, which keeps its state and the addresses
	// (test/integration/lb_lone_test.go).
	lbNoPeer = "127.0.0.1"
)

var lbIfaceRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// RenderKeepalived renders keepalived.conf for one appliance. Every value is checked here, since it comes from a
// stored row and from the guest: an address must parse, an interface name must be a plain name.
func RenderKeepalived(k LBKeepalived) (string, error) {
	if k.Peer == "" {
		k.Peer = lbNoPeer
	}
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
		lbSysctlFile: "net.ipv4.ip_nonlocal_bind = 1\nnet.ipv4.conf.all.rp_filter = 2\nnet.ipv4.conf.default.rp_filter = 2\n",
		// No cloud-init config names the public NIC, so nothing else brings its link up.
		lbDropIn: "[Service]\nExecStartPre=/usr/sbin/ip link set dev " + publicIface + " up\n",
		// The track script: HAProxy's master answers and has a worker, and the first address the config binds is
		// listened on. A stopped HAProxy, or one that runs without the balancer's listeners (another config than
		// the file: drill N3-3, the address stayed on it for 25 s), puts the appliance in FAULT, and the peer takes
		// over. One bind and no process names keep it cheap: keepalived runs it every second. A config with no
		// listener binds nothing to look for, and one an apply put in place in the last ten seconds is still being
		// loaded (haproxyReload sets its time): the listeners are not asked for meanwhile.
		lbCheckScript: "#!/bin/sh\n" + lbWorkerFn + "worker || exit 1\n" +
			"[ $(( $(date +%s) - $(stat -c %Y " + haproxyConfig + " 2>/dev/null || echo 0) )) -ge 10 ] || exit 0\n" +
			"b=$(awk '$1==\"bind\" && $2 !~ /^unix@/{print $2; exit}' " + haproxyConfig + " 2>/dev/null)\n" +
			"[ -n \"$b\" ] || exit 0\ncase \"$b\" in :*) b=\"0.0.0.0$b\" ;; esac\n" +
			"ss -Hltn | awk '{print $4}' | grep -qxF -e \"$b\"\n",
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

// lbWorkerFn defines worker(): HAProxy's master answers and has a worker.
const lbWorkerFn = "worker() { out=$(echo 'show proc' | socat -t1 - UNIX-CONNECT:/run/haproxy-master.sock 2>/dev/null) && echo \"$out\" | grep -qw worker; }\n"

const (
	// lbPairInstall is the pair's own install line; a single appliance keeps haproxyInstall (C13).
	lbPairInstall = "pgrep -x apt-get >/dev/null || { apt-get update -qq; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq haproxy socat keepalived iproute2; }"
	lbSysctlFile  = "/etc/sysctl.d/90-proxcloud-lb.conf"
	lbDropIn      = "/etc/systemd/system/keepalived.service.d/proxcloud.conf"
	// lbKeepalivedApply runs after the files are written: keepalived checks the new config before it is moved in, and
	// a running keepalived reloads, which keeps its state; a stopped one is started. The agent creates a file with
	// mode 0666, so every file gets its mode here, before anything reads it. It answers 0 only when keepalived
	// runs after the change: only then is the config's hash recorded.
	lbKeepalivedApply = `set -e
F=/etc/keepalived/keepalived.conf
chown root:root ` + lbCheckScript + ` ` + lbNotifyScript + ` ` + lbSysctlFile + ` ` + lbDropIn + ` $F.new
chmod 0755 ` + lbCheckScript + ` ` + lbNotifyScript + `
chmod 0644 ` + lbSysctlFile + ` ` + lbDropIn + ` $F.new
sysctl -q -p ` + lbSysctlFile + `
systemctl daemon-reload
keepalived -t -f $F.new
mv $F.new $F
systemctl enable -q keepalived
if systemctl is-active -q keepalived; then systemctl reload keepalived; else systemctl start keepalived; fi
n=0
until systemctl is-active -q keepalived; do
  n=$((n+1))
  [ $n -lt 10 ] || { echo "keepalived does not run after the change" >&2; exit 1; }
  sleep 0.5
done
[ "$(stat -c %a:%U:%G $F)" = 644:root:root ]`
)

// lbPairReady: the pair's packages, as lbReadyScript has it.
var lbPairReady = lbReadyScript("haproxy socat keepalived", "haproxy.service keepalived.service")

// PushPairHAProxy installs config on an appliance of a pair, and the pair's packages first if the image lacks them.
// As PushHAProxy, nil means the running HAProxy was seen to serve config.
func (s *Service) PushPairHAProxy(ctx context.Context, node string, vmid int, config string, certs ...LBCertFile) error {
	if err := s.lbReady(ctx, node, vmid, lbPairReady, lbPairInstall); err != nil {
		return err
	}
	return s.swapHAProxy(ctx, node, vmid, config, certs)
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
		return fmt.Errorf("keepalived did not take the change: %s", strings.TrimSpace(stderr))
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
	HAProxy  bool   // HAProxy's master has a worker
	HoldsVIP bool   // the public NIC carries a VIP
	Serving  bool   // HAProxy listens on every bind of its config (lbServingFn)
	// What the appliance's files hash to, "" when the guest did not say (sha256, "-" for a missing file): the
	// HAProxy config, the VRRP keepalived's config, and the LVS files as LVSHash sums them. A pass compares them
	// with what it recorded: a hash in a row says the appliance runs that file only while the file is still there
	// (spike of v0.11.1, SP-2: a config edited by hand stayed, since only "listeners are served" was asked).
	HAProxySum, KeepalivedSum, LVSSum string
	LVS                               bool // the UDP service runs its config (lvsServingFn)
}

// ReadVRRPState asks an appliance for its VRRP state, whether HAProxy runs, whether it holds the first VIP, whether
// HAProxy serves the config on disk, what its config files hash to, and whether its UDP service serves its config.
func (s *Service) ReadVRRPState(ctx context.Context, node string, vmid int, vip string) (VRRPState, error) {
	holds := "v=0"
	if a, err := netip.ParseAddr(vip); err == nil && a.Is4() {
		holds = "ip -o -4 addr show | grep -qF ' " + a.String() + "/' && v=1 || v=0"
	}
	script := lbServingFn + lbWorkerFn + lvsServingFn + lvsSumFn + "sum() { [ -f $1 ] && sha256sum < $1 | cut -c1-64 || echo -; }\n" +
		"s=$(cat " + lbStateFile + " 2>/dev/null); systemctl is-active -q keepalived || s=STOPPED; " +
		// serving and lvsserving run in a subshell: their loops use variables of their own, and lvsserving's is v,
		// which is the answer to "holds the address" here (with a UDP listener the holder read as holding nothing).
		"worker && h=1 || h=0; " + holds + "; (serving) >/dev/null 2>&1 && c=1 || c=0; (lvsserving) >/dev/null 2>&1 && l=1 || l=0; " +
		"echo \"${s:-UNKNOWN} $h $v $c $(sum " + haproxyConfig + ") $(sum /etc/keepalived/keepalived.conf) $(lvssum) $l\""
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
	if len(f) == 4 || len(f) == 8 {
		switch f[0] {
		case "MASTER", "BACKUP", "FAULT", "STOP", "STOPPED":
			st.State = f[0]
		}
		st.HAProxy, st.HoldsVIP, st.Serving = f[1] == "1", f[2] == "1", f[3] == "1"
	}
	if len(f) == 8 {
		st.HAProxySum, st.KeepalivedSum, st.LVSSum, st.LVS = f[4], f[5], f[6], f[7] == "1"
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
