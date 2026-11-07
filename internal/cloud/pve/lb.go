package pve

// lb.go — the HAProxy appliance of a load balancer. A tenant's input reaches the config only as
// numbers, parsed addresses and values from a fixed list: names stay in the row and the config uses
// generated ids, so nothing typed into a name can become a directive.

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
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
}

// LBListener is a port on the load balancer's addresses.
type LBListener struct {
	Port int    `json:"port"`
	Pool string `json:"pool"`
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
)

var (
	lbNameRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	lbServerIDRe = regexp.MustCompile(`^[0-9]{1,9}$`)
	lbProtocols  = []string{"http", "tcp"}
	lbAlgorithms = []string{"roundrobin", "leastconn", "source"}
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
	return ""
}

const haproxyGlobal = `global
  log /dev/log local0
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

// RenderHAProxy renders the config. It works from a stored row, so it trusts nothing a validation
// would have caught: a value off the lists or an unparsable address is left out.
func RenderHAProxy(s LBSpec) string {
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
		fmt.Fprintf(&b, "\nfrontend l%d\n  bind :%d\n  mode %s\n  default_backend p%d\n", l.Port, l.Port, m, i)
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
		for _, m := range p.Members {
			addr, err := netip.ParseAddr(m.Address)
			if m.Missing || err != nil || !addr.Is4() || m.Port < 1 || m.Port > 65535 || !lbServerIDRe.MatchString(m.ServerID) {
				continue
			}
			w := m.Weight
			if w < 1 || w > 256 {
				w = 1
			}
			fmt.Fprintf(&b, "  server %s %s:%d check weight %d\n", lbServerName(m), addr, m.Port, w)
		}
	}
	return b.String()
}

// ErrLBNotReady: the appliance is still booting or installing HAProxy. It clears by itself.
var ErrLBNotReady = errors.New("the appliance is not ready yet")

const (
	haproxyConfig = "/etc/haproxy/haproxy.cfg"
	// haproxyReload checks the new file before it replaces the one in use, so a config HAProxy rejects
	// leaves the balancer running on the last good one. Nothing a tenant wrote is in this command.
	// sshd goes first: nobody logs in to an appliance, and a listener may want its port.
	haproxyReload = "systemctl disable --now ssh ssh.socket 2>/dev/null; set -e; haproxy -c -q -f " + haproxyConfig + ".new; mv " + haproxyConfig + ".new " + haproxyConfig + "; systemctl reload-or-restart haproxy"
	// haproxyInstall prepares a stock image from its repositories, over the public NIC.
	// ponytail: a site with no egress bakes haproxy and socat into the image config.lbImage names.
	haproxyInstall = "pgrep -x apt-get >/dev/null || { apt-get update -qq; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq haproxy socat; }"
	guestTimeout   = 30 * time.Second
)

// PushHAProxy installs config on a running appliance, and HAProxy first if the image lacks it. A guest
// without its agent or without HAProxy yet answers ErrLBNotReady.
func (s *Service) PushHAProxy(ctx context.Context, node string, vmid int, config string) error {
	exit, _, _, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", "command -v haproxy && command -v socat")
	if err != nil {
		if IsForbidden(err) {
			return conflictf("The provider's API token cannot run commands in the appliance (it needs VM.GuestAgent.Unrestricted)")
		}
		return fmt.Errorf("%w: %v", ErrLBNotReady, err)
	}
	if exit != 0 {
		if _, err := s.C.GuestStart(ctx, node, vmid, "sh", "-c", haproxyInstall); err != nil {
			return fmt.Errorf("%w: %v", ErrLBNotReady, err)
		}
		return ErrLBNotReady
	}
	if err := s.C.GuestWriteFile(ctx, node, vmid, haproxyConfig+".new", config); err != nil {
		return err
	}
	exit, _, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", haproxyReload)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("haproxy refused the config: %s", strings.TrimSpace(stderr))
	}
	return nil
}

// SetVMInboundPorts makes the VM's direct firewall rules open exactly these TCP ports. New rules go
// in before old ones leave, so a port that stays never closes.
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
			form := ruleForm(FirewallRule{Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: port, Comment: "load balancer listener"})
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
