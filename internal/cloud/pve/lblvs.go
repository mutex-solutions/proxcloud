package pve

// lblvs.go — the UDP listeners of an appliance pair. HAProxy balances no generic UDP, so they are served by the
// kernel's IPVS in NAT mode, filled by a second keepalived that runs only its health checks ("--check") in a unit of
// its own, proxcloud-lvs.service. The VRRP keepalived of lbpair.go and its config are not touched: a member that
// comes or goes reloads the LVS process alone (spike of v0.11.1: 20 such reloads, no VRRP transition).
//
// The packet path. A client's datagram reaches the VIP on the public NIC and is local there; IPVS picks a member and
// sends it out of the tenant NIC, where an nftables rule masquerades it: the member sees the appliance's own tenant
// address, so it answers the appliance on its own subnet and nothing has to change on a member (its default route is
// the tenant gateway, not the appliance). Connection tracking undoes the masquerade, the packet is forwarded, IPVS
// puts the VIP back as its source, and with vs.snat_reroute the route is looked up again with that source, so the
// pair's rule "from <vip> table 100" sends it out of the public NIC. Both vs.conntrack=1 and vs.snat_reroute=1 are
// needed (spike: without the first no answer, without the second the answer left by the tenant NIC). The upstream
// balancer the method is taken from is named in the C4 change log (2026-11-22, UDP listeners).
//
// Forwarding makes a two-NIC appliance a router between the public and the tenant network, so the same nftables
// table holds a forward chain with policy drop that lets only the answers of flows IPVS opened through, and TCP is
// never tracked: HAProxy's connections would fill the connection table (spike: 2046 entries per 1000 requests
// against 46). The table is loaded before forwarding is switched on, by the unit, at every start; ip_forward sits
// in no sysctl.d file, where the boot would set it long before the table exists, and it goes off when the unit
// stops or fails to start.
//
// Nothing here reaches an appliance of a pair without a UDP listener: RenderLVS answers "" for it, and the caller
// then writes no file, installs no package and asks the guest nothing.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

const (
	lvsConfig  = "/etc/keepalived/lvs.conf"
	lvsDir     = "/etc/proxcloud-lb"
	lvsNft     = lvsDir + "/lvs.nft"
	lvsSysctl  = lvsDir + "/lvs.sysctl"
	lvsNet     = "/usr/local/sbin/proxcloud-lvs-net"
	lvsUnit    = "/etc/systemd/system/proxcloud-lvs.service"
	lvsService = "proxcloud-lvs"
	// lvsTable and its chains: plain names of the product's own. nft reads "fwd" and other short words as keywords
	// (spike, nft 1.1.6: "unexpected fwd, expecting string").
	lvsTable   = "ip proxcloud-lb"
	lvsForward = "lb-forward"
	// lvsConntrackMax is the least the connection table holds once UDP flows are tracked (the upstream reference
	// sizes it too). A larger value of the image's stays.
	lvsConntrackMax = 262144

	// What a UDP pool's health check is when the pool names no figure, as measured in the spike: a check every 5 s,
	// a member out after a failed check and two more tries 2 s apart.
	lvsInterval, lvsTimeout, lvsReplyTimeout, lvsRetry, lvsRetryDelay = 5, 5, 2, 2, 2
)

// lvsServer is one virtual server of the LVS config: a UDP listener on one address, and the members it balances.
type lvsServer struct {
	vip     netip.Addr
	port    int
	pool    int
	limit   int
	members []LBMember
}

// key is the server as /proc/net/ip_vs prints it.
func lvsKey(a netip.Addr, port int) string {
	b := a.As4()
	return fmt.Sprintf("%02X%02X%02X%02X:%04X", b[0], b[1], b[2], b[3], port)
}

// lvsServers lists what the LVS config serves, for the renderer and for the health read alike: every UDP listener
// with a UDP pool that has a member, on its own address (a Service's) or on each of the balancer's. It works from a
// stored row, so anything a validation would have caught is left out. A listener without a member gets no virtual
// server: keepalived takes none without a real server, and the appliance then answers "port unreachable" itself.
func lvsServers(s LBSpec, vips []string) []lvsServer {
	var binds []netip.Addr
	for _, v := range vips {
		if a, err := netip.ParseAddr(v); err == nil && a.Is4() {
			binds = append(binds, a)
		}
	}
	idx := map[string]int{}
	for i, p := range s.Pools {
		idx[p.Name] = i
	}
	var out []lvsServer
	seen := map[string]bool{}
	for _, l := range s.Listeners {
		i, ok := idx[l.Pool]
		if !ok || l.Protocol != "udp" || s.Pools[i].Protocol != "udp" || l.Port < 1 || l.Port > 65535 {
			continue
		}
		var members []LBMember
		for _, m := range s.Pools[i].Members {
			if _, ok := lbMemberAddr(m); ok {
				members = append(members, m)
			}
		}
		if len(members) == 0 {
			continue
		}
		on := binds
		if l.VIP != "" {
			a, err := netip.ParseAddr(l.VIP)
			if err != nil || !a.Is4() || !slices.Contains(binds, a) {
				continue
			}
			on = []netip.Addr{a}
		}
		limit := 0
		if l.ConnectionLimit >= 1 && l.ConnectionLimit <= lbMaxConnections {
			limit = l.ConnectionLimit
		}
		for _, a := range on {
			if k := lvsKey(a, l.Port); !seen[k] {
				seen[k] = true
				out = append(out, lvsServer{vip: a, port: l.Port, pool: i, limit: limit, members: members})
			}
		}
	}
	return out
}

// RenderLVS renders the config of the LVS keepalived for a pair: one virtual_server per UDP listener and address, in
// NAT mode. "" when nothing of the spec is served over UDP, and then no appliance gets anything of this file's.
// Written for keepalived 2.3.x and checked with `keepalived -t` on Debian 13 and Ubuntu 26.04
// (test/integration/lb_keepalived_test.go). No text of a tenant's reaches the file but a health check's path and
// payload, each held to its pattern here again.
//
// "alpha" on every server with a check: a member is in the table only after its first good check, so a member added
// while its port is closed takes no traffic (spike K4: 0 of 277 datagrams lost with it, 78 of 286 without). Its
// price is a table without members for one check round after a start of the process, which happens at boot and at
// the first UDP listener; a reload keeps the members that are up.
//
// "warmup 0" beside it: keepalived starts a checker's first run after a random wait of up to delay_loop, which is
// there to spread the checks of many virtual servers and which a new member waited out before it could enter (drill
// R2a-1 of v0.11.1-rc.2; both images: 3.9 to 6.0 s with the default, 1.0 s with 0, of which 1 s is the UDP check
// waiting for an ICMP error that does not come). The first check still decides: a member whose port is closed does
// not enter. A reload that only adds the line keeps every member in the table.
func RenderLVS(s LBSpec, vips []string) string {
	servers := lvsServers(s, vips)
	if len(servers) == 0 {
		return ""
	}
	in := func(v, lo, hi, or int) int {
		if v < lo || v > hi {
			return or
		}
		return v
	}
	var b strings.Builder
	// "instance" gives this keepalived its own pid files and log name beside the VRRP one's.
	b.WriteString("instance lvs\nglobal_defs {\n  router_id proxcloud-lvs\n  lvs_flush_on_stop\n}\n")
	for _, v := range servers {
		p := s.Pools[v.pool]
		hc := LBHealthCheck{Type: "udp-connect"}
		if p.HealthCheck != nil {
			hc = *p.HealthCheck
		}
		if !slices.Contains(lbUDPChecks, hc.Type) {
			hc.Type = "udp-connect"
		}
		sched := map[string]string{"leastconn": "lc", "source": "sh"}[p.Algorithm]
		if sched == "" {
			sched = "wrr"
		}
		interval := in(hc.Interval, 1, 300, lvsInterval)
		fmt.Fprintf(&b, "\nvirtual_server %s %d {\n  protocol UDP\n  lvs_method NAT\n  lvs_sched %s\n", v.vip, v.port, sched)
		if hc.Type != "none" {
			b.WriteString("  alpha\n  warmup 0\n")
		}
		// keepalived counts the tries after a first failed check: "fall 3" is retry 2. It takes no "retry 0" (both
		// images' keepalived -t: "number '0' outside range"), so a member is out after two failed checks at least.
		fmt.Fprintf(&b, "  delay_loop %d\n  retry %d\n  delay_before_retry %d\n", interval, in(hc.Fall, 2, 10, lvsRetry+1)-1, min(lvsRetryDelay, interval))
		if sp := p.SessionPersistence; sp != nil && sp.Type == "source_ip" {
			fmt.Fprintf(&b, "  persistence_timeout %d\n", in(sp.Timeout, 1, 86400, lbPersistenceTimeout))
		}
		timeout := lvsTimeout
		if hc.RequireReply {
			timeout = lvsReplyTimeout
		}
		timeout = in(hc.Timeout, 1, 300, timeout)
		var check strings.Builder
		line := func(format string, a ...any) { fmt.Fprintf(&check, "      "+format+"\n", a...) }
		port := func() {
			if hc.Port > 0 && hc.Port <= 65535 {
				line("connect_port %d", hc.Port)
			}
		}
		switch hc.Type {
		case "udp-connect":
			check.WriteString("    UDP_CHECK {\n")
			line("connect_timeout %d", timeout)
			port()
			if lbPayloadRe.MatchString(hc.Payload) {
				line("payload %s", strings.ToLower(hc.Payload))
			}
			if hc.RequireReply {
				line("require_reply")
			}
			check.WriteString("    }\n")
			if !hc.RequireReply {
				// Without a required reply the UDP check fails only on an ICMP error, and a dead host sends none
				// until its neighbour entry has failed: 39 s in the spike. The ping takes it out in 10 to 12 s.
				check.WriteString("    PING_CHECK {\n      connect_timeout 2\n      retry 2\n      delay_before_retry 1\n    }\n")
			}
		case "tcp":
			check.WriteString("    TCP_CHECK {\n")
			line("connect_timeout %d", timeout)
			port()
			check.WriteString("    }\n")
		case "http":
			path := "/"
			if lbCheckPathRe.MatchString(hc.Path) {
				path = hc.Path
			}
			check.WriteString("    HTTP_GET {\n      url {\n")
			line("  path %s", path)
			line("  status_code 200")
			check.WriteString("      }\n")
			line("connect_timeout %d", timeout)
			port()
			check.WriteString("    }\n")
		}
		for _, m := range v.members {
			addr, _ := lbMemberAddr(m)
			fmt.Fprintf(&b, "  real_server %s %d {\n    weight %d\n", addr, m.Port, in(m.Weight, 1, 256, 1))
			if v.limit > 0 { // IPVS counts per member: the listener's limit holds for each of them
				fmt.Fprintf(&b, "    uthreshold %d\n", v.limit)
			}
			b.WriteString(check.String() + "  }\n")
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// lvsFiles are what the LVS keepalived needs besides its config, in the order their digest is taken. tenantIface is
// the guest's name of net0, read from the guest (eth0 on Ubuntu 26.04, not "net0").
func lvsFiles(tenantIface string) [][2]string {
	return [][2]string{
		// One statement a line: nft refuses a one-line chain without a semicolon before its brace (spike). The first
		// two lines make the load replace the table in one transaction.
		{lvsNft, "table " + lvsTable + "\ndelete table " + lvsTable + "\ntable " + lvsTable + ` {
  chain lb-prerouting {
    type filter hook prerouting priority -300;
    meta l4proto tcp notrack
  }
  chain lb-output {
    type filter hook output priority -300;
    meta l4proto tcp notrack
  }
  chain lb-masquerade {
    type nat hook postrouting priority 100;
    oifname "` + tenantIface + `" meta l4proto udp counter masquerade
  }
  chain ` + lvsForward + ` {
    type filter hook forward priority 0; policy drop;
    ct state established,related counter accept
    counter comment "dropped"
  }
}
`},
		// The vs.* keys exist only once ip_vs is loaded, so this is no sysctl.d file. No ICMP redirect to a member
		// whose answer leaves by another NIC than it came in on.
		{lvsSysctl, "net.ipv4.vs.conntrack = 1\nnet.ipv4.vs.snat_reroute = 1\nnet.ipv4.vs.expire_nodest_conn = 1\nnet.ipv4.vs.expire_quiescent_template = 1\n" +
			"net.ipv4.conf.all.send_redirects = 0\nnet.ipv4.conf." + tenantIface + ".send_redirects = 0\n"},
		// up: the modules, the sysctls, the table, and forwarding last and only when the forward chain is loaded.
		// down: forwarding off first. A failed "up" ends in "down": never forwarding without the chain.
		{lvsNet, `#!/bin/sh
down() {
  sysctl -q -w net.ipv4.ip_forward=0
  nft delete table ` + lvsTable + ` 2>/dev/null
  return 0
}
case "$1" in
up)
  modprobe -a ip_vs ip_vs_wrr ip_vs_lc ip_vs_sh 2>/dev/null
  if sysctl -q -p ` + lvsSysctl + ` && nft -f ` + lvsNft + ` && nft list chain ` + lvsTable + ` ` + lvsForward + ` | grep -q 'policy drop'; then
    m=/proc/sys/net/netfilter/nf_conntrack_max
    [ "$(cat $m 2>/dev/null || echo 0)" -ge ` + strconv.Itoa(lvsConntrackMax) + ` ] || { echo ` + strconv.Itoa(lvsConntrackMax) + ` > $m; } 2>/dev/null || true
    sysctl -q -w net.ipv4.ip_forward=1 && exit 0
  fi
  echo "the kernel's IPVS or the nftables table could not be set up" >&2
  down
  exit 1 ;;
down) down ;;
*) exit 2 ;;
esac
`},
		{lvsUnit, `[Unit]
Description=ProxCloud load balancer: UDP listeners (LVS and their health checks)
After=network-online.target
Wants=network-online.target
ConditionFileNotEmpty=` + lvsConfig + `

[Service]
Type=simple
ExecStartPre=` + lvsNet + ` up
ExecStart=/usr/sbin/keepalived --check --dont-fork -f ` + lvsConfig + ` -p /run/proxcloud-lvs.pid -c /run/proxcloud-lvs-checkers.pid
ExecReload=` + lvsNet + ` up
ExecReload=/bin/kill -HUP $MAINPID
ExecStopPost=` + lvsNet + ` down
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
`},
	}
}

// LVSHash is what an appliance's LVS files hash to when it runs config: the config and every support file, as the
// guest sums them (lvsSumFn). The caller records it, and compares it with what the appliance holds at every pass:
// a file changed by hand, or by a release, is pushed again.
func LVSHash(config, tenantIface string) string {
	h := sha256.New()
	h.Write([]byte(config))
	for _, f := range lvsFiles(tenantIface) {
		h.Write([]byte(f[1]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

const (
	// lvsSumFn defines lvssum(): the digest LVSHash gives for the files on the appliance, "-" without a config.
	lvsSumFn = `lvssum() { [ -f ` + lvsConfig + ` ] && cat ` + lvsConfig + ` ` + lvsNft + ` ` + lvsSysctl + ` ` + lvsNet + ` ` + lvsUnit + ` 2>/dev/null | sha256sum | cut -c1-64 || echo -; }
`
	// lvsDriftFn defines lvsdrift(): what the kernel's IPVS table holds differently from the config on disk, of
	// everything the config can change on a virtual or a real server that stays: a virtual server's scheduler and
	// persistence timeout, a real server's weight and connection limit (its upper threshold). It prints nothing
	// when there is no difference, and otherwise one line each, the arguments of the ipvsadm call that puts it
	// right. Only what both hold is compared: a member that is out of the table is keepalived's own matter (its
	// check), and a virtual server the table lacks is lvsserving()'s.
	//
	// keepalived 2.3.x rewrites a real server at a reload only when its weight changed, so a connection limit
	// added to, raised on or removed from a listener stayed in the file and never reached the kernel (drill R3-F2
	// of v0.11.1-rc.2: a removed limit of 5 kept refusing new flows). A scheduler and a persistence timeout it
	// does rewrite; they are compared all the same, so a table changed by hand reads as one.
	//
	// ipvsadm comes with keepalived on both images (a recommended package). Where it is absent nothing is compared
	// and nothing set, as before this function existed. A subshell: nothing it sets reaches its caller.
	lvsDriftFn = `lvsdrift() (
  command -v ipvsadm >/dev/null 2>&1 || exit 0
  { cat ` + lvsConfig + `; echo ==; ipvsadm -Ln; echo ==; ipvsadm -Ln --thresholds; } 2>/dev/null | awk '
    $1=="==" { n++; v=""; next }
    n==0 {
      if ($1=="virtual_server") { v=$2":"$3; r="" }
      else if ($1=="lvs_sched") S[v]=$2
      else if ($1=="persistence_timeout") P[v]=$2
      else if ($1=="real_server") { r=v" "$2":"$3; W[r]=1; U[r]=0 }
      else if ($1=="weight" && r!="") W[r]=$2
      else if ($1=="uthreshold" && r!="") U[r]=$2
      next
    }
    $1=="UDP" {
      v=$2
      if (n==1 && (v in S)) {
        p=0
        for (i=4; i<NF; i++) if ($i=="persistent") p=$(i+1)
        if ($3!=S[v] || p!=P[v]+0) print "-E -u", v, "-s", S[v] (P[v]+0>0 ? " -p " P[v] : "")
      }
      next
    }
    $1!="->" { v=""; next }
    v!="" && ((v" "$2) in W) {
      r=v" "$2
      if (n==1) w[r]=$4
      else if ((r in w) && (w[r]!=W[r] || $3!=U[r])) print "-e -u", v, "-r", $2, "-m -w", W[r], "-x", U[r], "-y 0"
    }'
)
`
	// lvsServingFn defines lvsserving(): the unit runs, IPVS tracks and reroutes, the forward chain is loaded with
	// forwarding on, the IPVS table holds every virtual server of the config on disk, and what it holds of them is
	// what the config says (lvsdrift); it prints what is missing otherwise. The table is keepalived's own word that
	// it runs the file: /proc/net/ip_vs prints a server as "UDP  CB00710A:1388 wrr". A subshell, as serving() is:
	// nothing it sets reaches its caller.
	lvsServingFn = lvsDriftFn + `lvsserving() (
  systemctl is-active -q ` + lvsService + ` || { echo "the UDP service is not running"; return 1; }
  [ "$(cat /proc/sys/net/ipv4/vs/conntrack 2>/dev/null)" = 1 ] && [ "$(cat /proc/sys/net/ipv4/vs/snat_reroute 2>/dev/null)" = 1 ] || { echo "IPVS does not track and reroute its connections"; return 1; }
  nft list chain ` + lvsTable + ` ` + lvsForward + ` 2>/dev/null | grep -q 'policy drop' || { echo "the forwarding filter is not loaded"; return 1; }
  [ "$(cat /proc/sys/net/ipv4/ip_forward)" = 1 ] || { echo "forwarding is off"; return 1; }
  have=$(awk '$1=="UDP"{print $2}' /proc/net/ip_vs 2>/dev/null)
  for v in $(awk '$1=="virtual_server"{split($2,a,"."); printf "%02X%02X%02X%02X:%04X/%s:%s\n",a[1],a[2],a[3],a[4],$3,$2,$3}' ` + lvsConfig + `); do
    echo "$have" | grep -qxF -e "${v%%/*}" || { echo "the UDP listener ${v#*/} is not in the IPVS table"; return 1; }
  done
  [ -z "$(lvsdrift)" ] || { echo "the IPVS table does not hold a connection limit, a weight, an algorithm or a persistence as the UDP configuration has it"; return 1; }
)
`
	// lvsSetFn defines lvsset(): it puts right what lvsdrift() names, one ipvsadm edit each. An edit changes the
	// figures of a server in place: its connections, and every other server's, stay (the real-guest test holds a
	// flow across each). Never an error: what an edit did not put right, lvsserving() says.
	lvsSetFn = `lvsset() { lvsdrift | ( set -f; while read -r a; do ipvsadm $a; done ) >/dev/null 2>&1 || true; }
`
	// lvsHave opens a push: it makes the directories, takes away what an interrupted push left beside a file's place,
	// and prints what each file of the UDP service on the appliance hashes to, one a line, lvsFiles' and then the config
	// ("-" for one that is not there). PushLVS then writes the files that differ and no other: a member that comes
	// or goes changes lvs.conf alone, and the four support files change with a release only (drill R2a-1 of
	// v0.11.1-rc.2: every push wrote all five, seven guest calls where three do).
	lvsHave = `mkdir -p ` + lvsDir + ` /etc/keepalived /usr/local/sbin
for f in ` + lvsNft + ` ` + lvsSysctl + ` ` + lvsNet + ` ` + lvsUnit + ` ` + lvsConfig + `; do rm -f $f.new; [ -f $f ] && sha256sum < $f | cut -c1-64 || echo -; done`
	// lvsApply runs after the files that differ are written, each beside its place as <path>.new; a file without one
	// stays as it is. keepalived checks a new config before it replaces the one in use; a running unit reloads (HUP:
	// the table keeps the members that are up), a stopped one starts, and a unit file a release changed restarts it.
	// keepalived does not write a changed connection limit to a real server that stays, so lvsset() does, after the
	// reload and before each look. Then lvsserving() must hold: only on the answer 0 is the hash recorded. If it does
	// not, a config this push replaced goes back and is loaded again; a first config that was not served goes and
	// the unit stops, which switches forwarding off; a push that brought no config changes nothing more. Every file
	// is moved in here by a rename with its owner and mode set, as keepalived's own are (lbKeepalivedApply): the unit
	// reads the table's file and runs the script at every start, also one systemd makes by itself, and never meets
	// one that is half written. A file that stays gets its owner and mode set too: the hash lvsHave compares says
	// nothing of them, and a script that lost its mode would keep the unit from starting at every push.
	lvsApply = lbNoSSH + lbPkgGateKeepalived + `set -e
F=` + lvsConfig + `
U=` + lvsUnit + `
new() { [ -f "$1.new" ]; }
own() { [ ! -f "$2" ] || { chown root:root "$2"; chmod $1 "$2"; }; }
for f in ` + lvsNft + ` ` + lvsSysctl + ` $F $U; do own 0644 $f.new; own 0644 $f; done
own 0755 ` + lvsNet + `.new
own 0755 ` + lvsNet + `
! new $F || keepalived -t -f $F.new
for f in ` + lvsNft + ` ` + lvsSysctl + ` ` + lvsNet + `; do ! new $f || mv -f $f.new $f; done
how=reload
if new $U; then if cmp -s $U.new $U; then rm -f $U.new; else mv $U.new $U; systemctl daemon-reload; how=restart; fi; fi
before=kept
if new $F; then
  if [ -f $F ]; then cp $F $F.old; before=old; else rm -f $F.old; before=none; fi
  mv $F.new $F
fi
systemctl enable -q ` + lvsService + `
` + lvsServingFn + lvsSetFn + `swap() {
  if systemctl is-active -q ` + lvsService + `; then systemctl $how ` + lvsService + ` || return 1; else systemctl reset-failed ` + lvsService + ` 2>/dev/null || true; systemctl start ` + lvsService + ` || { echo "the UDP service did not start" >&2; return 1; }; fi
  n=0
  until lvsset; why=$(lvsserving); do
    n=$((n+1))
    [ $n -lt 20 ] || { echo "$why" >&2; return 1; }
    sleep 0.5
  done
}
swap && exit 0
case $before in
old) mv $F.old $F; swap ;;
none) systemctl disable -q --now ` + lvsService + `; rm -f $F ;;
esac
exit 1`
	// lvsRemove takes everything of the UDP listeners off an appliance whose balancer has none left, forwarding
	// first, and proves it: no forwarding, no table, no UDP server in the IPVS table (lvs_flush_on_stop).
	lvsRemove = `systemctl disable -q --now ` + lvsService + ` 2>/dev/null
sysctl -q -w net.ipv4.ip_forward=0
nft delete table ` + lvsTable + ` 2>/dev/null
rm -f ` + lvsConfig + ` ` + lvsConfig + `.old ` + lvsConfig + `.new ` + lvsNft + ` ` + lvsSysctl + ` ` + lvsNet + ` ` + lvsUnit + ` ` + lvsUnit + `.new ` + lvsNft + `.new ` + lvsSysctl + `.new ` + lvsNet + `.new
rmdir ` + lvsDir + ` 2>/dev/null
systemctl daemon-reload
[ "$(cat /proc/sys/net/ipv4/ip_forward)" = 0 ] || { echo "forwarding is still on" >&2; exit 1; }
! nft list table ` + lvsTable + ` >/dev/null 2>&1 || { echo "the forwarding filter is still loaded" >&2; exit 1; }
! grep -q '^UDP' /proc/net/ip_vs 2>/dev/null || { echo "the IPVS table still holds a UDP listener" >&2; exit 1; }`
	// lvsInstall is the install line of a pair with a UDP listener: nftables, which Ubuntu 26.04's image has and
	// Debian 13's may lack. No other appliance ever runs it (C13: a release that changes a package list makes no
	// existing appliance run apt). It names nftables alone and upgrades nothing: the appliance serves when it runs
	// (the pair's own packages are in, PushLVS comes after the pair's pushes), and apt-get install upgrades every
	// installed package it is given, which would restart HAProxy and keepalived on both appliances of a balancer in
	// use, in the same pass.
	lvsInstall = lbNoSSH + "pgrep -x apt-get >/dev/null || { apt-get update -qq; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-upgrade nftables; }"
)

var lvsReady = lbReadyScript("haproxy socat keepalived nftables", "haproxy.service keepalived.service")

// PushLVS brings the LVS config and its support files on an appliance of a pair in line and (re)loads the unit,
// installing nftables first if the image lacks it. Only a file that is not on the appliance as it should be is
// written. nil means the IPVS table was seen to hold every virtual server of config as config has it, with the
// forwarding filter loaded: any other answer means not applied, and the caller records nothing. A config keepalived
// -t refuses leaves the running one in place.
func (s *Service) PushLVS(ctx context.Context, node string, vmid int, config, tenantIface string) error {
	if !lbIfaceRe.MatchString(tenantIface) {
		return fmt.Errorf("lvs: bad interface %q", tenantIface)
	}
	if err := s.lbReady(ctx, node, vmid, lvsReady, lvsInstall); err != nil {
		return err
	}
	files := append(lvsFiles(tenantIface), [2]string{lvsConfig, config}) // the order lvsHave answers in
	exit, out, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", lvsHave)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("lvs: %s", strings.TrimSpace(stderr))
	}
	// Each beside its place: lvsApply moves them in. An answer that is not one hash a file writes them all.
	have := strings.Fields(out)
	for i, f := range files {
		sum := sha256.Sum256([]byte(f[1]))
		if len(have) == len(files) && have[i] == hex.EncodeToString(sum[:]) {
			continue
		}
		if err := s.C.GuestWriteFile(ctx, node, vmid, f[0]+".new", f[1]); err != nil {
			return err
		}
	}
	exit, _, stderr, err = s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", lvsApply)
	if err != nil {
		return err
	}
	if exit == lbBusyExit {
		return ErrLBPackageBusy
	}
	if exit != 0 {
		return fmt.Errorf("the appliance did not take the UDP listeners, the last ones stay in use: %s", strings.TrimSpace(stderr))
	}
	return nil
}

// RemoveLVS takes the UDP service off an appliance: the unit, forwarding, the nftables table and the files. nil
// means the appliance was seen without them.
func (s *Service) RemoveLVS(ctx context.Context, node string, vmid int) error {
	exit, _, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", lvsRemove)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("the appliance did not give up its UDP listeners: %s", strings.TrimSpace(stderr))
	}
	return nil
}

// LVSHealth reads the IPVS table of an appliance and answers the spec's UDP members as it holds them, and the UDP
// listeners it does not hold (each as "port/udp" or "address:port/udp"). An appliance without IPVS holds none.
func (s *Service) LVSHealth(ctx context.Context, node string, vmid int, spec LBSpec, vips []string) ([]LBMemberHealth, []string, error) {
	exit, out, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", "cat /proc/net/ip_vs 2>/dev/null || true")
	if err != nil {
		return nil, nil, err
	}
	if exit != 0 {
		return nil, nil, fmt.Errorf("ipvs table: %s", strings.TrimSpace(stderr))
	}
	health, notLoaded := ParseLVSTable(out, spec, vips)
	return health, notLoaded, nil
}

// ParseLVSTable maps /proc/net/ip_vs back to the spec's UDP pools and listeners. keepalived takes a member that
// fails its check out of the table ("alpha" keeps a new one out until its first good check), so a member is UP when
// every virtual server of its pool holds it, DOWN when one does not, and LBNotLoaded when the table holds none of
// its pool's servers. A member the config leaves out (its server left, or its pool has no listener) keeps "".
func ParseLVSTable(out string, spec LBSpec, vips []string) ([]LBMemberHealth, []string) {
	table := map[string]map[string]bool{} // server -> members
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		switch f := strings.Fields(line); {
		case len(f) >= 2 && (f[0] == "TCP" || f[0] == "UDP" || f[0] == "SCTP" || f[0] == "FWM"):
			cur = ""
			if f[0] == "UDP" {
				cur = f[1]
				table[cur] = map[string]bool{}
			}
		case len(f) >= 2 && f[0] == "->" && cur != "":
			table[cur][f[1]] = true
		}
	}
	servers := lvsServers(spec, vips)
	var health []LBMemberHealth
	for i, p := range spec.Pools {
		if p.Protocol != "udp" {
			continue
		}
		for _, m := range p.Members {
			h := LBMemberHealth{Pool: p.Name, ServerID: m.ServerID, Port: m.Port}
			if m.Node {
				h.Address = m.Address
			}
			if addr, ok := lbMemberAddr(m); ok {
				loaded, in := 0, 0
				for _, v := range servers {
					if members, ok := table[lvsKey(v.vip, v.port)]; ok && v.pool == i {
						loaded++
						if members[lvsKey(addr, m.Port)] {
							in++
						}
					}
				}
				switch {
				case !slices.ContainsFunc(servers, func(v lvsServer) bool { return v.pool == i }):
				case loaded == 0:
					h.Status = LBNotLoaded
				case in == loaded:
					h.Status = "UP"
				default:
					h.Status = "DOWN"
				}
			}
			health = append(health, h)
		}
	}
	var notLoaded []string
	multi := len(vips) > 1
	for _, v := range servers {
		if _, ok := table[lvsKey(v.vip, v.port)]; !ok {
			name := strconv.Itoa(v.port) + "/udp"
			if multi {
				name = v.vip.String() + ":" + name
			}
			notLoaded = append(notLoaded, name)
		}
	}
	return health, notLoaded
}
