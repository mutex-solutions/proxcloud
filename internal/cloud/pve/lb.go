package pve

// lb.go — the HAProxy appliance of a load balancer. A tenant's input reaches the config only as
// numbers, parsed addresses and values from a fixed list: names stay in the row and the config uses
// generated ids, so nothing typed into a name can become a directive.

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const tagLBPref = "pc-lb-"

// LBTag marks a VM as the appliance of load balancer id.
func LBTag(id string) string { return tagLBPref + id }

// LBOfTags is the load balancer a VM is the appliance of ("" = none).
func LBOfTags(tags string) string { return TagValue(tags, tagLBPref) }

// refuseAppliance is the check on any VM a tenant's request names as a target: a disk moved onto a balancer's VM
// cannot be detached and goes with the balancer, and an image of it would carry its tag.
func refuseAppliance(vm *ClusterResource) error {
	if LBOfTags(vm.Tags) != "" {
		return conflictf("This server belongs to a load balancer. Change or delete the load balancer instead")
	}
	return nil
}

// CreatedStamp reads a VM's identity stamp, "" when it has none.
func CreatedStamp(tags string) string { return TagValue(tags, tagCreatedPref) }

// LBMember is one backend: a server of the project. Address and Stamp are the platform's, resolved
// from its own allocation table, and never come from a request.
type LBMember struct {
	ServerID string `json:"serverId"`
	Port     int    `json:"port"`
	Weight   int    `json:"weight,omitempty"`
	Address  string `json:"address,omitempty"`
	Stamp    string `json:"stamp,omitempty"`
	// Missing: the server left the project or the network, so the config leaves it out.
	Missing bool `json:"missing,omitempty"`
}

// LBPool is a set of members one or more listeners forward to.
type LBPool struct {
	Name      string     `json:"name"`
	Protocol  string     `json:"protocol"`  // http | tcp
	Algorithm string     `json:"algorithm"` // roundrobin | leastconn | source
	Members   []LBMember `json:"members"`
	// HealthCheck nil is today's check: a TCP connection to the member's port.
	HealthCheck *LBHealthCheck `json:"healthCheck,omitempty"`
	// ProxyProtocol sends the client's address to the members in a PROXY v2 header.
	ProxyProtocol bool `json:"proxyProtocol,omitempty"`
}

// LBHealthCheck is how a pool's members are checked.
type LBHealthCheck struct {
	Type string `json:"type"`           // tcp | http
	Port int    `json:"port,omitempty"` // 0 = the member's own port
	Path string `json:"path,omitempty"` // http: the path of the GET, "/" when empty
}

// LBListener is a port on the load balancer's addresses.
type LBListener struct {
	Port int    `json:"port"`
	Pool string `json:"pool"`
	// SourceRanges are the IPv4 networks the port admits (empty = any). They become the source of the listener's
	// firewall rule, so a refused client never reaches HAProxy.
	SourceRanges []string `json:"sourceRanges,omitempty"`
}

// LBSpec is what a tenant configures.
type LBSpec struct {
	Listeners []LBListener `json:"listeners"`
	Pools     []LBPool     `json:"pools"`
}

const (
	lbMaxListeners = 20
	lbMaxPools     = 20
	lbMaxMembers   = 50
	// LBMaxConfig is what a config may render to. PVE's file-write takes 60 KiB of content in a POST of at most
	// 64 KiB, and form encoding grows this text by about a tenth.
	LBMaxConfig = 45 << 10
)

var (
	lbNameRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	lbServerIDRe = regexp.MustCompile(`^[0-9]{1,9}$`)
	lbProtocols  = []string{"http", "tcp"}
	lbAlgorithms = []string{"roundrobin", "leastconn", "source"}
	// lbCheckPathRe: what a health check's path may hold, so it can only be a path in the config.
	lbCheckPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/%=&?:@+-]{0,199}$`)
)

// LBNameOK is the rule of a load balancer's and a pool's name.
func LBNameOK(name string) bool { return lbNameRe.MatchString(name) }

// LBNameRule words LBNameOK for a customer.
const LBNameRule = "use lowercase letters, digits and dashes, at most 32 characters"

// Validate checks everything a request can get wrong on its own, filling in the defaults. The returned
// text is shown to the customer. Whether a member's server is the project's is the caller's check.
func (s *LBSpec) Validate() string {
	if len(s.Listeners) > lbMaxListeners || len(s.Pools) > lbMaxPools {
		return fmt.Sprintf("A load balancer holds at most %d listeners and %d pools", lbMaxListeners, lbMaxPools)
	}
	pools := map[string]bool{}
	for i := range s.Pools {
		p := &s.Pools[i]
		if !LBNameOK(p.Name) {
			return "Pool names " + LBNameRule
		}
		if pools[p.Name] {
			return "Pool " + p.Name + " is defined twice"
		}
		pools[p.Name] = true
		if p.Protocol == "" {
			p.Protocol = "http"
		}
		if !slices.Contains(lbProtocols, p.Protocol) {
			return "Pool " + p.Name + ": protocol must be http or tcp"
		}
		if p.Algorithm == "" {
			p.Algorithm = "roundrobin"
		}
		if !slices.Contains(lbAlgorithms, p.Algorithm) {
			return "Pool " + p.Name + ": algorithm must be roundrobin, leastconn or source"
		}
		if len(p.Members) > lbMaxMembers {
			return fmt.Sprintf("Pool %s holds more than %d members", p.Name, lbMaxMembers)
		}
		if hc := p.HealthCheck; hc != nil {
			if hc.Type == "" {
				hc.Type = "tcp"
			}
			switch {
			case !slices.Contains(lbProtocols, hc.Type):
				return "Pool " + p.Name + ": health check type must be tcp or http"
			case hc.Port < 0 || hc.Port > 65535:
				return "Pool " + p.Name + ": health check port must be 1 to 65535, or empty for the member's port"
			case hc.Type == "tcp" && hc.Path != "":
				return "Pool " + p.Name + ": a TCP health check has no path"
			case hc.Type == "http" && hc.Path != "" && !lbCheckPathRe.MatchString(hc.Path):
				return "Pool " + p.Name + ": health check path must start with / and hold no spaces"
			}
		}
		seen := map[string]bool{}
		for j := range p.Members {
			m := &p.Members[j]
			if !lbServerIDRe.MatchString(m.ServerID) {
				return "Pool " + p.Name + ": a member needs a server"
			}
			if m.Port < 1 || m.Port > 65535 {
				return "Pool " + p.Name + ": member port must be 1 to 65535"
			}
			if m.Weight == 0 {
				m.Weight = 1
			}
			if m.Weight < 1 || m.Weight > 256 {
				return "Pool " + p.Name + ": member weight must be 1 to 256"
			}
			key := m.ServerID + ":" + strconv.Itoa(m.Port)
			if seen[key] {
				return fmt.Sprintf("Pool %s lists server %s port %d twice", p.Name, m.ServerID, m.Port)
			}
			seen[key] = true
			m.Address, m.Stamp, m.Missing = "", "", false // the platform's to fill in
		}
	}
	ports := map[int]bool{}
	for _, l := range s.Listeners {
		if l.Port < 1 || l.Port > 65535 {
			return "Listener port must be 1 to 65535"
		}
		if ports[l.Port] {
			return fmt.Sprintf("Port %d has two listeners", l.Port)
		}
		ports[l.Port] = true
		if !pools[l.Pool] {
			return fmt.Sprintf("Listener on port %d names no pool of this load balancer", l.Port)
		}
	}
	for i := range s.Listeners {
		ranges, msg := lbSourceRanges(s.Listeners[i].SourceRanges)
		if msg != "" {
			return fmt.Sprintf("Listener on port %d: %s", s.Listeners[i].Port, msg)
		}
		s.Listeners[i].SourceRanges = ranges
	}
	return ""
}

// lbSourceRanges checks a listener's source list and returns it in the form the firewall rule gets: IPv4 networks,
// host bits cleared, sorted, no repeats. Proxmox takes at most fwMaxAddrs characters in a rule's source.
func lbSourceRanges(in []string) ([]string, string) {
	out := make([]string, 0, len(in))
	for _, r := range in {
		r = strings.TrimSpace(r)
		if !strings.Contains(r, "/") {
			r += "/32"
		}
		p, err := netip.ParsePrefix(r)
		if err != nil || !p.Addr().Is4() || p.Bits() == 0 {
			return nil, "source ranges must be IPv4 networks such as 203.0.113.0/24 (not 0.0.0.0/0: leave the list empty for any)"
		}
		out = append(out, p.Masked().String())
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(strings.Join(out, ",")) > fwMaxAddrs {
		return nil, fmt.Sprintf("the source ranges take more than %d characters, the most a firewall rule holds: use fewer, wider networks", fwMaxAddrs)
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, ""
}

const haproxyGlobal = `global
  log /dev/log local0
  chroot /var/lib/haproxy
  user haproxy
  group haproxy
  maxconn 20000
  stats socket /run/haproxy/admin.sock mode 660 level admin

defaults
  log global
  timeout connect 5s
  timeout client 60s
  timeout server 60s
  default-server inter 3s fall 2 rise 2
`

func lbServerName(m LBMember) string { return "m" + m.ServerID + "p" + strconv.Itoa(m.Port) }

// RenderHAProxy renders the config of a single appliance: every listener binds its port on any address.
func RenderHAProxy(s LBSpec) string { return RenderHAProxyOn(s, nil) }

// RenderHAProxyOn renders the config. With vips, each listener binds its port on each of them, as an appliance of a
// pair does: the standby binds addresses it does not hold (ip_nonlocal_bind), so a switch needs no reload. It works
// from a stored row, so it trusts nothing a validation would have caught: a value off the lists or an unparsable
// address is left out.
func RenderHAProxyOn(s LBSpec, vips []string) string {
	var binds []string
	for _, v := range vips {
		if a, err := netip.ParseAddr(v); err == nil && a.Is4() {
			binds = append(binds, a.String())
		}
	}
	if len(binds) == 0 {
		binds = []string{""}
	}
	var b strings.Builder
	b.WriteString(haproxyGlobal)
	mode := func(p LBPool) string {
		if slices.Contains(lbProtocols, p.Protocol) {
			return p.Protocol
		}
		return "tcp"
	}
	idx := map[string]int{}
	for i, p := range s.Pools {
		idx[p.Name] = i
	}
	listeners := 0
	for _, l := range s.Listeners {
		i, ok := idx[l.Pool]
		if !ok || l.Port < 1 || l.Port > 65535 {
			continue
		}
		listeners++
		m := mode(s.Pools[i])
		fmt.Fprintf(&b, "\nfrontend l%d\n", l.Port)
		for _, a := range binds {
			fmt.Fprintf(&b, "  bind %s:%d\n", a, l.Port)
		}
		fmt.Fprintf(&b, "  mode %s\n  default_backend p%d\n", m, i)
		if m == "http" {
			b.WriteString("  option forwardfor\n")
		}
	}
	if listeners == 0 { // HAProxy refuses to start with no proxy
		b.WriteString("\nlisten idle\n  mode tcp\n  bind unix@/run/haproxy/idle.sock\n")
	}
	for i, p := range s.Pools {
		algo := "roundrobin"
		if slices.Contains(lbAlgorithms, p.Algorithm) {
			algo = p.Algorithm
		}
		fmt.Fprintf(&b, "\nbackend p%d\n  mode %s\n  balance %s\n", i, mode(p), algo)
		check, extra := "check", ""
		if hc := p.HealthCheck; hc != nil {
			if hc.Type == "http" {
				path := "/"
				if lbCheckPathRe.MatchString(hc.Path) {
					path = hc.Path
				}
				fmt.Fprintf(&b, "  option httpchk GET %s\n", path)
			}
			if hc.Port > 0 && hc.Port <= 65535 {
				check += " port " + strconv.Itoa(hc.Port)
			}
		}
		if p.ProxyProtocol { // never check-send-proxy: a check on its own port reaches a plain health endpoint
			extra = " send-proxy-v2"
		}
		for _, m := range p.Members {
			addr, err := netip.ParseAddr(m.Address)
			if m.Missing || err != nil || !addr.Is4() || m.Port < 1 || m.Port > 65535 || !lbServerIDRe.MatchString(m.ServerID) {
				continue
			}
			w := m.Weight
			if w < 1 || w > 256 {
				w = 1
			}
			fmt.Fprintf(&b, "  server %s %s:%d %s weight %d%s\n", lbServerName(m), addr, m.Port, check, w, extra)
		}
	}
	return b.String()
}

// ErrLBNotReady: the appliance is still booting or installing HAProxy. It clears by itself.
var ErrLBNotReady = errors.New("the appliance is not ready yet")

const (
	haproxyConfig = "/etc/haproxy/haproxy.cfg"
	// haproxyReload checks the new file before it replaces the one in use, and puts the old one back if the
	// reload still fails, as for a port the check cannot see is taken, so the next boot does not read a config
	// that never ran. systemctl cannot tell: it reports a reload HAProxy gave up on as done, so this asks the
	// master process, whose reload answers Success=0 or 1 (HAProxy 2.7 or newer). Nothing a tenant wrote is in
	// this command. sshd goes first: nobody logs in to an appliance, and a listener may want its port.
	haproxyReload = `systemctl disable --now ssh ssh.socket 2>/dev/null
set -e
F=` + haproxyConfig + `
haproxy -c -q -f $F.new
cp $F $F.old
mv $F.new $F
swap() {
  systemctl is-active -q haproxy || { systemctl start haproxy; return; }
  out=$(printf 'reload\n' | socat -t30 - /run/haproxy-master.sock)
  case "$out" in Success=1*) ;; *) echo "$out" | grep ALERT >&2; return 1 ;; esac
}
swap || { mv $F.old $F; swap; exit 1; }`
	// haproxyInstall prepares a stock image from its repositories, over the public NIC.
	// ponytail: a site with no egress bakes haproxy and socat into the image config.lbImage names.
	haproxyInstall = "pgrep -x apt-get >/dev/null || { apt-get update -qq; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq haproxy socat; }"
	haproxyReady   = "command -v haproxy && command -v socat"
	guestTimeout   = 30 * time.Second
)

// PushHAProxy installs config on a running appliance, and HAProxy first if the image lacks it. A guest
// without its agent or without HAProxy yet answers ErrLBNotReady.
func (s *Service) PushHAProxy(ctx context.Context, node string, vmid int, config string) error {
	if err := s.lbReady(ctx, node, vmid, haproxyReady, haproxyInstall); err != nil {
		return err
	}
	return s.swapHAProxy(ctx, node, vmid, config)
}

// lbReady installs what an appliance runs when ready says it lacks, and answers ErrLBNotReady meanwhile.
func (s *Service) lbReady(ctx context.Context, node string, vmid int, ready, install string) error {
	exit, _, _, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", ready)
	if err != nil {
		if IsForbidden(err) {
			return conflictf("The provider's API token cannot run commands in the appliance (it needs VM.GuestAgent.Unrestricted)")
		}
		return fmt.Errorf("%w: %v", ErrLBNotReady, err)
	}
	if exit != 0 {
		if _, err := s.C.GuestStart(ctx, node, vmid, "sh", "-c", install); err != nil {
			return fmt.Errorf("%w: %v", ErrLBNotReady, err)
		}
		return ErrLBNotReady
	}
	return nil
}

func (s *Service) swapHAProxy(ctx context.Context, node string, vmid int, config string) error {
	if err := s.C.GuestWriteFile(ctx, node, vmid, haproxyConfig+".new", config); err != nil {
		return err
	}
	exit, _, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", haproxyReload)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("HAProxy refused the change, the last config stays in use: %s", strings.TrimSpace(stderr))
	}
	return nil
}

// SetVMInboundPorts makes the VM's direct firewall rules open exactly these TCP ports. New rules go
// in before old ones leave, so a port that stays never closes. Every other rule is deleted: the sandbox entry
// appliance owns its VM's rules whole. A load balancer uses SetLBRules.
func (s *Service) SetVMInboundPorts(ctx context.Context, node string, vmid int, ports []int) error {
	base := vmFirewallPath(node, vmid) + "/rules"
	var rules []FirewallRule
	if err := s.C.get(ctx, base, nil, &rules); err != nil {
		return err
	}
	want := map[string]bool{}
	for _, p := range ports {
		want[strconv.Itoa(p)] = true
	}
	have := map[string]bool{}
	for _, r := range rules {
		have[r.DPort] = true
	}
	for port := range want {
		if !have[port] {
			form := ruleForm(FirewallRule{Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: port, Comment: lbListenerComment})
			if err := s.C.post(ctx, base, form, nil); err != nil {
				return err
			}
		}
	}
	// A new rule lands on top and moves the others down, so read the positions again.
	rules = nil
	if err := s.C.get(ctx, base, nil, &rules); err != nil {
		return err
	}
	for i := len(rules) - 1; i >= 0; i-- {
		if !want[rules[i].DPort] {
			if err := s.C.delete(ctx, base+"/"+strconv.Itoa(rules[i].Pos), nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

const (
	// lbListenerComment marks a listener rule. The rules of a single appliance carry it since the first release, so
	// SetLBRules takes them over as they are.
	lbListenerComment = "load balancer listener"
	// lbVRRPComment marks the rule that lets the peer appliance's VRRP adverts in.
	lbVRRPComment = "load balancer vrrp"
	// fwMaxPorts: Proxmox takes at most 15 numbers in a rule's dport list, a range counting 2 (pve-firewall
	// src/PVE/Firewall.pm:1241-1245 at 8381d96).
	fwMaxPorts = 15
	// fwMaxAddrs: Proxmox takes at most 512 characters in a rule's source and dest (Firewall.pm:1716-1729).
	fwMaxAddrs = 512
)

// LBListenerRules is the firewall a balancer's listeners need: one rule per (address, source list), its ports in
// lists of at most fwMaxPorts. Without vips (a single appliance) the rules name no destination and hold one port
// each, as they always have.
func LBListenerRules(listeners []LBListener, vips []string) []FirewallRule {
	bySource := map[string][]int{}
	for _, l := range listeners {
		if l.Port < 1 || l.Port > 65535 {
			continue
		}
		ranges, msg := lbSourceRanges(l.SourceRanges)
		if msg != "" {
			continue // a stored row a validation never saw: nothing is opened for it
		}
		src := strings.Join(ranges, ",")
		bySource[src] = append(bySource[src], l.Port)
	}
	// A single appliance keeps one rule per port, the rules it has had since the first release, so an existing
	// balancer's firewall writes stay what they were (C13); it has at most lbMaxListeners of them.
	dests, size := []string{""}, 1
	if len(vips) > 0 {
		dests, size = nil, fwMaxPorts
		for _, v := range vips {
			if a, err := netip.ParseAddr(v); err == nil && a.Is4() {
				dests = append(dests, a.String())
			}
		}
	}
	var out []FirewallRule
	for _, dest := range dests {
		for _, src := range slices.Sorted(maps.Keys(bySource)) {
			ports := slices.Clone(bySource[src])
			slices.Sort(ports)
			for chunk := range slices.Chunk(ports, size) {
				nums := make([]string, len(chunk))
				for i, p := range chunk {
					nums[i] = strconv.Itoa(p)
				}
				out = append(out, FirewallRule{Type: "in", Action: "ACCEPT", Proto: "tcp", Source: src, Dest: dest,
					DPort: strings.Join(nums, ","), Comment: lbListenerComment})
			}
		}
	}
	return out
}

// LBVRRPRule lets the peer appliance's VRRP adverts in, from its tenant address. The product's security groups
// refuse protocol vrrp (112), so this rule is the load balancer's own; Proxmox takes "vrrp" by name (spike,
// 2026-11-21).
func LBVRRPRule(peer string) FirewallRule {
	return FirewallRule{Type: "in", Action: "ACCEPT", Proto: "vrrp", Source: peer, Comment: lbVRRPComment}
}

func lbRuleKey(r FirewallRule) string {
	return strings.Join([]string{r.Type, r.Action, r.Proto, r.Source, r.Dest, r.DPort, r.Comment}, "|")
}

// SetLBRules makes the VM's load balancer rules (comment lbListenerComment or lbVRRPComment) exactly want. Missing
// rules go in first and extra ones leave after, so a port that stays never closes; a rule with any other comment is
// never touched.
func (s *Service) SetLBRules(ctx context.Context, node string, vmid int, want []FirewallRule) error {
	rules, err := s.C.ListVMFirewallRules(ctx, node, vmid)
	if err != nil {
		return err
	}
	ours := func(r FirewallRule) bool { return r.Comment == lbListenerComment || r.Comment == lbVRRPComment }
	have := map[string]bool{}
	for _, r := range rules {
		if ours(r) {
			have[lbRuleKey(r)] = true
		}
	}
	base := vmFirewallPath(node, vmid) + "/rules"
	for _, r := range want {
		if !have[lbRuleKey(r)] {
			if err := s.C.post(ctx, base, ruleForm(r), nil); err != nil {
				return err
			}
			have[lbRuleKey(r)] = true
		}
	}
	// A new rule lands on top and moves the others down, so read the positions again.
	if rules, err = s.C.ListVMFirewallRules(ctx, node, vmid); err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, r := range want {
		keep[lbRuleKey(r)] = true
	}
	for i := len(rules) - 1; i >= 0; i-- {
		r := rules[i]
		if !ours(r) {
			continue
		}
		if k := lbRuleKey(r); keep[k] {
			delete(keep, k) // one copy stays, a duplicate goes
			continue
		}
		if err := s.C.delete(ctx, base+"/"+strconv.Itoa(r.Pos), nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// LBMemberHealth is one member as HAProxy sees it.
type LBMemberHealth struct {
	Pool     string `json:"pool"`
	ServerID string `json:"serverId"`
	Port     int    `json:"port"`
	Status   string `json:"status"` // UP, DOWN, ... as HAProxy says; "" = not in the running config
}

// LBHealth reads the running HAProxy's view of every member.
func (s *Service) LBHealth(ctx context.Context, node string, vmid int, spec LBSpec) ([]LBMemberHealth, error) {
	exit, out, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", `echo "show stat" | socat - /run/haproxy/admin.sock`)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, fmt.Errorf("haproxy stats: %s", strings.TrimSpace(stderr))
	}
	return ParseHAProxyStat(out, spec)
}

// ParseHAProxyStat maps `show stat` CSV back to the spec's members.
func ParseHAProxyStat(out string, spec LBSpec) ([]LBMemberHealth, error) {
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(strings.TrimSpace(out), "# ")))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("haproxy stats: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("haproxy stats: empty")
	}
	col := map[string]int{}
	for i, name := range rows[0] {
		col[name] = i
	}
	px, sv, st := col["pxname"], col["svname"], col["status"]
	status := map[string]string{}
	for _, row := range rows[1:] {
		if len(row) > max(px, sv, st) {
			status[row[px]+"/"+row[sv]] = row[st]
		}
	}
	health := []LBMemberHealth{}
	for i, p := range spec.Pools {
		for _, m := range p.Members {
			health = append(health, LBMemberHealth{
				Pool: p.Name, ServerID: m.ServerID, Port: m.Port,
				Status: status["p"+strconv.Itoa(i)+"/"+lbServerName(m)],
			})
		}
	}
	return health, nil
}
