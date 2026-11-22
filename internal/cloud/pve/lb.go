package pve

// lb.go — the HAProxy appliance of a load balancer. A tenant's input reaches the config only as
// numbers, parsed addresses and values from a fixed list: names stay in the row and the config uses
// generated ids, so nothing typed into a name can become a directive.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
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
	// Node: a worker of a Kubernetes cluster, named by its Address alone (no server). Only the platform sets it, from
	// the cluster's own worker addresses; Validate clears it on a tenant's spec.
	Node bool `json:"node,omitempty"`
}

// LBPool is a set of members one or more listeners forward to.
type LBPool struct {
	Name string `json:"name"`
	// Protocol is what the members are spoken to in: http | tcp | udp. What clients are spoken to in is the
	// listener's (LBListener.Protocol, lbPoolProtocolOf).
	Protocol  string     `json:"protocol"`
	Algorithm string     `json:"algorithm"` // roundrobin | leastconn | source
	Members   []LBMember `json:"members"`
	// HealthCheck nil is today's check: a TCP connection to the member's port.
	HealthCheck *LBHealthCheck `json:"healthCheck,omitempty"`
	// ProxyProtocol sends the client's address to the members in a PROXY header, of ProxyVersion 1 or 2 (0 = 2).
	ProxyProtocol bool `json:"proxyProtocol,omitempty"`
	ProxyVersion  int  `json:"proxyVersion,omitempty"`
	// SessionPersistence nil: every connection is balanced on its own.
	SessionPersistence *LBPersistence `json:"sessionPersistence,omitempty"`
	// TimeoutMemberConnect and TimeoutMemberData, in milliseconds (0 = the balancer's own): how long a member may
	// take to accept a connection, and how long a connection to it may stay idle. They sit on the pool and not on the
	// listener: one HAProxy backend has one pair of them.
	TimeoutMemberConnect int `json:"timeoutMemberConnect,omitempty"`
	TimeoutMemberData    int `json:"timeoutMemberData,omitempty"`
}

// LBPersistence keeps a client on the member it first reached. It is an object with a type, so that a cookie is a
// new type later and not a new field.
type LBPersistence struct {
	Type    string `json:"type"`              // source_ip
	Timeout int    `json:"timeout,omitempty"` // seconds without a connection after which a client is balanced afresh; 0 = lbPersistenceTimeout
}

// LBHealthCheck is how a pool's members are checked.
type LBHealthCheck struct {
	Type string `json:"type"`           // tcp | http | https | tls-hello | udp-connect (a UDP pool's own) | none
	Port int    `json:"port,omitempty"` // 0 = the member's own port
	Path string `json:"path,omitempty"` // http, https: the path of the GET, "/" when empty
	// Interval and Timeout in seconds, Rise and Fall in checks; 0 = the balancer's own (every 3 s, the answer within
	// the interval, 2 good checks up, 2 bad ones down; a UDP pool's are in lblvs.go).
	Interval int `json:"interval,omitempty"`
	Timeout  int `json:"timeout,omitempty"`
	Rise     int `json:"rise,omitempty"`
	Fall     int `json:"fall,omitempty"`
	// Payload and RequireReply belong to udp-connect. Payload is the datagram the check sends, as hexadecimal digits
	// (empty: random bytes). RequireReply makes a member up only when it answers the datagram: without it a member
	// is down only when an ICMP error comes back, and one whose port is closed is not seen under traffic (lblvs.go).
	Payload      string `json:"payload,omitempty"`
	RequireReply bool   `json:"requireReply,omitempty"`
}

// LBListener is a port on the load balancer's addresses.
type LBListener struct {
	Port int `json:"port"`
	// Protocol is what clients are spoken to in: tcp | udp | tls | http | https (lbPoolProtocolOf). Empty in a row
	// from before v0.11.1, where the protocol sat on the pool alone: such a listener reads its pool's, and Validate
	// writes it out at the next change.
	Protocol string `json:"protocol,omitempty"`
	Pool     string `json:"pool"`
	// Certificates are the certificates a tls or https listener presents, by id: the one whose names hold the name a
	// client asks for (SNI), and the first one when none does or the client asks for no name.
	Certificates []string `json:"certificates,omitempty"`
	// CertFiles are the files of Certificates on an appliance, in the same order (lbtls.go). Only the platform sets
	// them, from the project's certificates; Validate clears them on a tenant's spec.
	CertFiles []string `json:"certFiles,omitempty"`
	// SourceRanges are the IPv4 networks the port admits (empty = any). They become the source of the listener's
	// firewall rule, so a refused client never reaches HAProxy.
	SourceRanges []string `json:"sourceRanges,omitempty"`
	// VIP is the one address the listener binds, a Kubernetes Service's own (empty: every address of the balancer).
	// Only the platform sets it; Validate clears it on a tenant's spec.
	VIP string `json:"vip,omitempty"`
	// ConnectionLimit is the most connections the listener holds open at a time (0 = no limit of its own).
	ConnectionLimit int `json:"connectionLimit,omitempty"`
	// TimeoutClientData, in milliseconds (0 = the balancer's own): how long a client's connection may stay idle.
	TimeoutClientData int `json:"timeoutClientData,omitempty"`
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
	lbAlgorithms = []string{"roundrobin", "leastconn", "source"}
	lbChecks     = []string{"tcp", "http", "https", "tls-hello", "none"}
	// lbUDPChecks are the checks of a UDP pool: keepalived runs them, not HAProxy (lblvs.go).
	lbUDPChecks = []string{"udp-connect", "tcp", "http", "none"}
	// lbPayloadRe: a check's datagram, 1 to 128 bytes as hexadecimal digits.
	lbPayloadRe = regexp.MustCompile(`^([0-9a-fA-F]{2}){1,128}$`)
	// lbCheckPathRe: what a health check's path may hold, so it can only be a path in the config.
	lbCheckPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/%=&?:@+-]{0,199}$`)

	// lbPoolProtocolOf is the validation matrix: the protocol a listener's pool must speak, for every listener
	// protocol of the model, one pool protocol per listener protocol: tls and https end TLS on the balancer and speak
	// plain TCP and HTTP to the members. A pool therefore serves listeners of one family. (Where the table comes
	// from: docs/architecture/c4/CHANGELOG.md, 2026-11-22.)
	lbPoolProtocolOf = map[string]string{"tcp": "tcp", "tls": "tcp", "http": "http", "https": "http", "udp": "udp"}
)

const (
	// lbMaxConnections is the appliance's whole budget of connections, HAProxy's global maxconn. A listener's own
	// limit "should not exceed the global maxconn" (HAProxy 3.0 configuration.txt, maxconn of a frontend).
	lbMaxConnections = 20000
	// lbPersistenceTimeout is how long a client stays on its member without a new connection when the pool names no
	// time: Kubernetes' own default for sessionAffinity ClientIP, three hours.
	lbPersistenceTimeout = 10800
	// What a timeout may be, in milliseconds: one second to a day.
	lbMinTimeout, lbMaxTimeout = 1000, 24 * 3600 * 1000
)

// lbFamily is the transport a listener protocol runs on, "tcp" or "udp". One port holds one listener of each: the
// key of a listener is (family, port), in the validation and in the firewall rule.
func lbFamily(protocol string) string {
	if protocol == "udp" {
		return "udp"
	}
	return "tcp"
}

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
	// A pool that names no protocol speaks what its first listener needs, and HTTP when none says: a request from
	// before v0.11.1 names the protocol on the pool alone, one after it may name it on the listener alone.
	asked := map[string]string{}
	for _, l := range s.Listeners {
		if p, ok := lbPoolProtocolOf[l.Protocol]; ok && asked[l.Pool] == "" {
			asked[l.Pool] = p
		}
	}
	pools := map[string]*LBPool{}
	for i := range s.Pools {
		p := &s.Pools[i]
		if !LBNameOK(p.Name) {
			return "Pool names " + LBNameRule
		}
		if pools[p.Name] != nil {
			return "Pool " + p.Name + " is defined twice"
		}
		pools[p.Name] = p
		if p.Protocol == "" {
			p.Protocol = cmp.Or(asked[p.Name], "http")
		}
		if !slices.Contains([]string{"http", "tcp", "udp"}, p.Protocol) {
			return "Pool " + p.Name + ": protocol must be http, tcp or udp"
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
		udp := p.Protocol == "udp"
		if hc := p.HealthCheck; hc != nil {
			if hc.Type == "" {
				hc.Type = "tcp"
				if udp {
					hc.Type = "udp-connect"
				}
			}
			hasPath := hc.Type == "http" || hc.Type == "https"
			switch {
			case udp && !slices.Contains(lbUDPChecks, hc.Type):
				return "Pool " + p.Name + ": the health check of a UDP pool must be udp-connect, tcp, http or none"
			case !udp && !slices.Contains(lbChecks, hc.Type):
				return "Pool " + p.Name + ": health check type must be tcp, http, https, tls-hello or none"
			case hc.Type != "udp-connect" && (hc.Payload != "" || hc.RequireReply):
				return "Pool " + p.Name + ": only a udp-connect health check takes a payload and a required reply"
			case hc.Payload != "" && !lbPayloadRe.MatchString(hc.Payload):
				return "Pool " + p.Name + ": health check payload must be 1 to 128 bytes written as hexadecimal digits, such as 70696e67"
			case udp && hc.Rise != 0:
				return "Pool " + p.Name + ": a UDP pool's member is up again at its first good check, so its health check takes no rise"
			case udp && hc.Fall == 1:
				// keepalived tries a failed check again at least once ("retry" is 1 or more, 2.3.3 and 2.3.4).
				return "Pool " + p.Name + ": a UDP pool's member is down after 2 to 10 failed checks, so its health check's fall cannot be 1"
			case hc.Type == "none" && *hc != (LBHealthCheck{Type: "none"}):
				return "Pool " + p.Name + ": a pool without a health check takes no port, path or figures of one"
			case hc.Port < 0 || hc.Port > 65535:
				return "Pool " + p.Name + ": health check port must be 1 to 65535, or empty for the member's port"
			case !hasPath && hc.Path != "":
				return "Pool " + p.Name + ": only an HTTP or HTTPS health check has a path"
			case hasPath && hc.Path != "" && !lbCheckPathRe.MatchString(hc.Path):
				return "Pool " + p.Name + ": health check path must start with / and hold no spaces"
			case hc.Interval < 0 || hc.Interval > 300 || hc.Timeout < 0 || hc.Timeout > 300:
				return "Pool " + p.Name + ": health check interval and timeout must be 1 to 300 seconds, or empty"
			case hc.Rise < 0 || hc.Rise > 10 || hc.Fall < 0 || hc.Fall > 10:
				return "Pool " + p.Name + ": health check rise and fall must be 1 to 10 checks, or empty"
			}
		}
		switch {
		case udp && p.ProxyProtocol:
			return "Pool " + p.Name + ": UDP has no PROXY protocol; the members see the load balancer's address, not the client's"
		case udp && (p.TimeoutMemberConnect != 0 || p.TimeoutMemberData != 0):
			return "Pool " + p.Name + ": a UDP pool takes no member timeouts"
		case p.ProxyVersion < 0 || p.ProxyVersion > 2:
			return "Pool " + p.Name + ": PROXY protocol version must be 1 or 2"
		case p.ProxyVersion != 0 && !p.ProxyProtocol:
			return "Pool " + p.Name + ": a PROXY protocol version needs proxyProtocol on"
		case !lbTimeoutOK(p.TimeoutMemberConnect) || !lbTimeoutOK(p.TimeoutMemberData):
			return "Pool " + p.Name + ": member timeouts must be 1000 to 86400000 milliseconds (a second to a day), or empty"
		}
		if sp := p.SessionPersistence; sp != nil {
			switch {
			case sp.Type != "source_ip":
				return "Pool " + p.Name + ": session persistence type must be source_ip"
			case sp.Timeout < 0 || sp.Timeout > 86400:
				return "Pool " + p.Name + ": session persistence timeout must be 1 to 86400 seconds, or empty"
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
			m.Address, m.Stamp, m.Missing, m.Node = "", "", false, false // the platform's to fill in
		}
	}
	ports := map[string]bool{}
	for i := range s.Listeners {
		l := &s.Listeners[i]
		if l.Port < 1 || l.Port > 65535 {
			return "Listener port must be 1 to 65535"
		}
		on := fmt.Sprintf("Listener on port %d", l.Port)
		pool := pools[l.Pool]
		if pool == nil {
			return on + " names no pool of this load balancer"
		}
		if l.Protocol == "" {
			l.Protocol = pool.Protocol
		}
		needs, known := lbPoolProtocolOf[l.Protocol]
		if !known {
			return on + ": protocol must be tcp, udp, tls, http or https"
		}
		if needs != pool.Protocol {
			return fmt.Sprintf("%s: %s %s listener needs %s %s pool, and pool %s is %s", on, lbArticle(l.Protocol), l.Protocol, lbArticle(needs), needs, pool.Name, pool.Protocol)
		}
		key := lbFamily(l.Protocol) + "/" + strconv.Itoa(l.Port)
		if ports[key] {
			return fmt.Sprintf("Port %d has two listeners", l.Port)
		}
		ports[key] = true
		switch {
		case len(l.Certificates) > 0 && !lbTerminatesTLS(l.Protocol):
			return on + ": only a tls or https listener takes certificates"
		case lbTerminatesTLS(l.Protocol) && len(l.Certificates) == 0:
			return fmt.Sprintf("%s: %s %s listener needs a certificate", on, lbArticle(l.Protocol), l.Protocol)
		case len(l.Certificates) > lbMaxListenerCerts:
			return fmt.Sprintf("%s: a listener takes at most %d certificates", on, lbMaxListenerCerts)
		case slices.ContainsFunc(l.Certificates, func(id string) bool { return !lbCertIDRe.MatchString(id) }):
			return on + ": a certificate is named by its id"
		case len(slices.Compact(slices.Sorted(slices.Values(l.Certificates)))) != len(l.Certificates):
			return on + " names a certificate twice"
		case l.ConnectionLimit < 0 || l.ConnectionLimit > lbMaxConnections:
			return fmt.Sprintf("%s: connection limit must be 1 to %d, or empty for none (%d is what the load balancer holds in all)", on, lbMaxConnections, lbMaxConnections)
		case l.Protocol == "udp" && l.TimeoutClientData != 0:
			return on + ": a UDP listener takes no client timeout"
		case !lbTimeoutOK(l.TimeoutClientData):
			return on + ": client timeout must be 1000 to 86400000 milliseconds (a second to a day), or empty"
		}
		l.VIP, l.CertFiles = "", nil
		ranges, msg := lbSourceRanges(l.SourceRanges)
		if msg != "" {
			return on + ": " + msg
		}
		l.SourceRanges = ranges
	}
	return ""
}

// HasTLS: the spec holds a listener that ends TLS, which needs its certificates on the appliance.
func (s *LBSpec) HasTLS() bool {
	return slices.ContainsFunc(s.Listeners, func(l LBListener) bool { return lbTerminatesTLS(l.Protocol) })
}

// HasUDP: the spec holds a UDP listener or pool, which only an appliance pair serves.
func (s *LBSpec) HasUDP() bool {
	return slices.ContainsFunc(s.Listeners, func(l LBListener) bool { return l.Protocol == "udp" }) ||
		slices.ContainsFunc(s.Pools, func(p LBPool) bool { return p.Protocol == "udp" })
}

func lbTimeoutOK(ms int) bool { return ms == 0 || (ms >= lbMinTimeout && ms <= lbMaxTimeout) }

func lbArticle(protocol string) string {
	if protocol == "http" || protocol == "https" {
		return "an"
	}
	return "a"
}

// LBSourceRanges is lbSourceRanges for a caller outside the package: the gateway checks a Service's list with it.
func LBSourceRanges(in []string) ([]string, string) { return lbSourceRanges(in) }

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

// haproxyGlobal opens every config. Three things in it keep a request from being lost when a member goes away or
// HAProxy is reloaded (drill N3-8 of v0.11.0: 2.2 to 10.3 s of lost requests at a reload or a node change):
//   - server-state-file with load-server-state-from-file: a reload starts every member UP unless it is told what the
//     member was; the apply saves "show servers state" there just before it reloads (haproxyReload), and HAProxy
//     finds a member again by its backend's and its own name (src/server_state.c, "be_name srv_name"). A member new
//     to the config still starts UP: "init-state down" needs HAProxy 3.1, and Debian 13 ships 3.0.
//   - option redispatch 1 with retries: a connection a member does not take is tried on another member at once, so
//     a member that just went away costs a request the connect timeout, not its answer. Only the connect is tried
//     again, never a request that was sent.
//   - a connect timeout of 2 s (it was 5): members are on the balancer's own network, and the timeout is what a
//     client waits before the next member is tried.
const haproxyGlobal = `global
  log /dev/log local0
  chroot /var/lib/haproxy
  user haproxy
  group haproxy
  maxconn 20000
  stats socket /run/haproxy/admin.sock mode 660 level admin
  server-state-file ` + haproxyState + `

defaults
  log global
  timeout connect 2s
  retries 3
  option redispatch 1
  load-server-state-from-file global
`

// haproxyTimeouts follows haproxyGlobal: how long a connection may stay idle (C5). A tenant's own balancer keeps
// 60 s; a Kubernetes cluster's gets an hour, as the MetalLB address it replaces has no idle timeout at all, and a
// database, websocket or gRPC connection would otherwise be cut after a minute.
const haproxyTimeouts = `  timeout client %[1]s
  timeout server %[1]s
  default-server inter 3s fall 2 rise 2
`

// lbBackends names each pool's backend. A reload carries a member's state over by the names of its backend and of
// the member, so a backend's name must not move when another pool comes or goes (it was the pool's place, p0, p1,
// ...: a port added to one Service of a cluster renamed the backends of every Service after it). It is "p-" and the
// pool's name, which HAProxy's log then shows next to a member that went down. Still nothing typed into a name can
// become a directive: only a plain name (LBNameOK: lowercase letters, digits, dashes) is used as it is; of any other
// (a stored row no validation saw) a digest is, and a name a pool before it has keeps the pool's place.
func lbBackends(pools []LBPool) []string {
	out := make([]string, len(pools))
	seen := map[string]bool{}
	for i, p := range pools {
		switch {
		case seen[p.Name]:
			out[i] = "p" + strconv.Itoa(i)
		case LBNameOK(p.Name):
			out[i] = "p-" + p.Name
		default:
			sum := sha256.Sum256([]byte(p.Name))
			out[i] = "p" + hex.EncodeToString(sum[:6])
		}
		seen[p.Name] = true
	}
	return out
}

// lbServerName is a member's name in the config: its server and port, or a worker's address and port.
func lbServerName(m LBMember) string {
	if m.Node {
		return "n" + m.Address + "p" + strconv.Itoa(m.Port)
	}
	return "m" + m.ServerID + "p" + strconv.Itoa(m.Port)
}

// RenderHAProxy renders the config of a single appliance: every listener binds its port on any address.
func RenderHAProxy(s LBSpec) string { return RenderHAProxyOn(s, nil) }

// RenderHAProxyOn renders the config. With vips, each listener binds its port on each of them, as an appliance of a
// pair does: the standby binds addresses it does not hold (ip_nonlocal_bind), so a switch needs no reload. It works
// from a stored row, so it trusts nothing a validation would have caught: a value off the lists or an unparsable
// address is left out.
func RenderHAProxyOn(s LBSpec, vips []string) string { return renderHAProxy(s, vips, "60s") }

// RenderClusterHAProxy renders the config of a Kubernetes cluster's balancer: each listener binds its Service's own
// address, and connections may stay idle for an hour.
func RenderClusterHAProxy(s LBSpec, vips []string) string { return renderHAProxy(s, vips, "1h") }

// lbBackendMode is the HAProxy mode of a pool's backend. A protocol off the list (a stored row no validation saw)
// is plain TCP.
func lbBackendMode(p LBPool) string {
	if p.Protocol == "http" {
		return "http"
	}
	return "tcp"
}

// lbFrontendMode is the HAProxy mode of a listener's frontend. ok false: HAProxy has no frontend for it, so the
// config, the health read and nothing else may count on one. That is a protocol HAProxy does not serve here (udp),
// and a listener whose pool speaks another protocol than it needs (a stored row no validation saw). A tls listener
// is a TCP frontend and an https listener an HTTP one, both with TLS on the bind line (lbBindTLS). A listener
// without a protocol is one from before v0.11.1 and reads its pool's.
func lbFrontendMode(l LBListener, p LBPool) (mode string, ok bool) {
	backend := lbBackendMode(p)
	switch l.Protocol {
	case "":
		return backend, true
	case "tcp", "http":
		return l.Protocol, l.Protocol == backend
	case "tls", "https":
		return lbPoolProtocolOf[l.Protocol], lbPoolProtocolOf[l.Protocol] == backend
	}
	return "", false
}

// renderHAProxy writes one frontend per listener, named by its port and never by its pool, that ends in its
// default_backend: what chooses another pool later (a rule of a layer 7 listener) goes above that line and moves
// nothing. A field at its default writes no line, so a config from before the field keeps its bytes.
func renderHAProxy(s LBSpec, vips []string, idle string) string {
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
	global := haproxyGlobal
	if slices.ContainsFunc(s.Listeners, func(l LBListener) bool { return lbTerminatesTLS(l.Protocol) && lbCertFiles(l) != nil }) {
		global = strings.Replace(global, "\n\ndefaults\n", "\n"+haproxyTLS+"\ndefaults\n", 1)
	}
	b.WriteString(global)
	fmt.Fprintf(&b, haproxyTimeouts, idle)
	idx := map[string]int{}
	for i, p := range s.Pools {
		idx[p.Name] = i
	}
	backends := lbBackends(s.Pools)
	listeners := 0
	for _, l := range s.Listeners {
		i, ok := idx[l.Pool]
		if !ok || l.Port < 1 || l.Port > 65535 {
			continue
		}
		m, ok := lbFrontendMode(l, s.Pools[i])
		if !ok {
			continue
		}
		name, vip, ok := lbFrontend(l)
		if !ok {
			continue
		}
		// A listener that ends TLS and has no certificate file (its certificate left the project, or a stored row
		// no validation saw) gets no frontend: HAProxy takes no "ssl" bind without one, and the health read then
		// lists the listener as not loaded.
		tls := ""
		if lbTerminatesTLS(l.Protocol) {
			if lbCertFiles(l) == nil {
				continue
			}
			tls = lbBindTLS(l)
		}
		on := binds
		if vip != "" { // one Service's address: two Services may share a port
			on = []string{vip}
		}
		listeners++
		fmt.Fprintf(&b, "\nfrontend %s\n", name)
		for _, a := range on {
			fmt.Fprintf(&b, "  bind %s:%d%s\n", a, l.Port, tls)
		}
		fmt.Fprintf(&b, "  mode %s\n  default_backend %s\n", m, backends[i])
		if m == "http" {
			b.WriteString("  option forwardfor\n")
		}
		if l.Protocol == "https" { // whatever the client sent in the header: the member reads what the balancer saw
			b.WriteString("  http-request set-header X-Forwarded-Proto https\n")
		}
		if l.ConnectionLimit >= 1 && l.ConnectionLimit <= lbMaxConnections {
			fmt.Fprintf(&b, "  maxconn %d\n", l.ConnectionLimit)
		}
		if t := l.TimeoutClientData; t != 0 && lbTimeoutOK(t) {
			fmt.Fprintf(&b, "  timeout client %dms\n", t)
		}
	}
	if listeners == 0 { // HAProxy refuses to start with no proxy
		b.WriteString("\nlisten idle\n  mode tcp\n  bind unix@/run/haproxy/idle.sock\n")
	}
	for i, p := range s.Pools {
		if p.Protocol == "udp" { // keepalived's, not HAProxy's (RenderLVS)
			continue
		}
		algo := "roundrobin"
		if slices.Contains(lbAlgorithms, p.Algorithm) {
			algo = p.Algorithm
		}
		fmt.Fprintf(&b, "\nbackend %s\n  mode %s\n  balance %s\n", backends[i], lbBackendMode(p), algo)
		check, extra := " check", ""
		if hc := p.HealthCheck; hc != nil {
			switch hc.Type {
			case "http", "https":
				path := "/"
				if lbCheckPathRe.MatchString(hc.Path) {
					path = hc.Path
				}
				fmt.Fprintf(&b, "  option httpchk GET %s\n", path)
			case "tls-hello":
				b.WriteString("  option ssl-hello-chk\n")
			}
			if hc.Port > 0 && hc.Port <= 65535 {
				check += " port " + strconv.Itoa(hc.Port)
			}
			if hc.Type == "https" { // the check alone speaks TLS; nothing verifies a member's certificate
				check += " check-ssl verify none"
			}
			in := func(v, lo, hi, or int) int {
				if v < lo || v > hi {
					return or
				}
				return v
			}
			if t := in(hc.Timeout, 1, 300, 0); t != 0 {
				fmt.Fprintf(&b, "  timeout check %ds\n", t)
			}
			// The three figures go together, each at the balancer's own when the pool names none, so the line means
			// the same whatever a backend takes over from the defaults.
			if inter, fall, rise := in(hc.Interval, 1, 300, 0), in(hc.Fall, 1, 10, 0), in(hc.Rise, 1, 10, 0); inter+fall+rise != 0 {
				fmt.Fprintf(&b, "  default-server inter %ds fall %d rise %d\n", cmp.Or(inter, 3), cmp.Or(fall, 2), cmp.Or(rise, 2))
			}
			if hc.Type == "none" {
				// Nothing brings a member without a check back up, so what the last process knew of it is not read:
				// a member that was down when its check was taken away would stay down for good, and at every reload
				// after it (the state is saved again as it stands). HAProxy 3.0.11 and 3.2.25 both do that.
				check = ""
				b.WriteString("  load-server-state-from-file none\n")
			}
		}
		if t := p.TimeoutMemberConnect; t != 0 && lbTimeoutOK(t) {
			fmt.Fprintf(&b, "  timeout connect %dms\n", t)
		}
		if t := p.TimeoutMemberData; t != 0 && lbTimeoutOK(t) {
			fmt.Fprintf(&b, "  timeout server %dms\n", t)
		}
		// The table is this HAProxy's own: it starts empty after a reload and on the other appliance of a pair.
		// ponytail: a "peers" section carries it over both; add it when a tenant needs a client kept across them.
		if sp := p.SessionPersistence; sp != nil && sp.Type == "source_ip" {
			expire := sp.Timeout
			if expire < 1 || expire > 86400 {
				expire = lbPersistenceTimeout
			}
			fmt.Fprintf(&b, "  stick-table type ip size 100k expire %ds\n  stick on src\n", expire)
		}
		if p.ProxyProtocol { // never check-send-proxy: a check on its own port reaches a plain health endpoint
			extra = " send-proxy-v2"
			if p.ProxyVersion == 1 {
				extra = " send-proxy"
			}
		}
		for _, m := range p.Members {
			addr, ok := lbMemberAddr(m)
			if !ok {
				continue
			}
			w := m.Weight
			if w < 1 || w > 256 {
				w = 1
			}
			fmt.Fprintf(&b, "  server %s %s:%d%s weight %d%s\n", lbServerName(m), addr, m.Port, check, w, extra)
		}
	}
	return b.String()
}

// lbFrontend is a listener's frontend in the config: its name, and the one address it binds when it has its own
// (a Service's). ok false: the listener is not rendered.
func lbFrontend(l LBListener) (name, vip string, ok bool) {
	if l.VIP == "" {
		return fmt.Sprintf("l%d", l.Port), "", true
	}
	a, err := netip.ParseAddr(l.VIP)
	if err != nil || !a.Is4() {
		return "", "", false
	}
	return fmt.Sprintf("l%s_%d", a, l.Port), a.String(), true
}

// lbMemberAddr is a member's address in the config. ok false: the member is not rendered (its server left, or the
// row holds a value no validation saw).
func lbMemberAddr(m LBMember) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(m.Address)
	if m.Missing || err != nil || !addr.Is4() || m.Port < 1 || m.Port > 65535 || (!m.Node && !lbServerIDRe.MatchString(m.ServerID)) {
		return netip.Addr{}, false
	}
	return addr, true
}

// ErrLBNotReady: the appliance is still booting or installing HAProxy. It clears by itself.
var ErrLBNotReady = errors.New("the appliance is not ready yet")

const (
	haproxyConfig = "/etc/haproxy/haproxy.cfg"
	// haproxyState is where an apply saves what every member is (UP, DOWN) for the reload that follows. It stays on
	// disk: a boot reads the state of the last apply, and a member that was DOWN then needs its two good checks.
	haproxyState = "/var/lib/haproxy/server-state"
	// lbServingFn defines serving(): HAProxy is active and listens on every TCP bind line of the config on disk; it
	// prints what is missing otherwise. A master with a worker is not enough: the package's own first start runs
	// the package's config, with no listener of the product's (drill N2-2: a pair refused connections for 458 s
	// and read "up"). ss prints a bind without an address as 0.0.0.0. The unix socket of an idle config is no
	// listener of a tenant's and is left out.
	lbServingFn = `serving() {
  systemctl is-active -q haproxy || { echo "HAProxy is not running"; return 1; }
  l=$(ss -Hltnp | awk '/"haproxy"/{print $4}')
  for b in $(awk '$1=="bind" && $2 !~ /^unix@/{print $2}' ` + haproxyConfig + `); do
    case "$b" in :*) b="0.0.0.0$b" ;; esac
    echo "$l" | grep -qxF -e "$b" || { echo "HAProxy does not listen on $b"; return 1; }
  done
}
`
	// haproxyReload checks the new file before it replaces the one in use, makes HAProxy run it, and proves that
	// it does before it answers 0: only then is the config's hash recorded. HAProxy is started when it does not
	// run, and then reloaded all the same: a start that was already under way (the package's own, at install) has
	// read the file that was there before, and "systemctl start" only joins it. The reload goes through the master
	// process, whose answer is Success=0 or 1 (HAProxy 2.7 or newer); systemctl cannot tell, it reports a reload
	// HAProxy gave up on as done. The master's socket refuses connections for a moment after a start (HAProxy 3.0
	// of Debian 13, seen in test/integration/lb_appliance_test.go), so the connection is tried again for a few
	// seconds. Then serving() must hold. If any of it fails the old file goes back and is
	// loaded again, so the next boot does not read a config that never ran, and the answer is 1: not applied.
	// The agent creates a file with mode 0666, so the mode is set here. Nothing a tenant wrote is in this
	// command. sshd goes first: nobody logs in to an appliance, and a listener may want its port.
	//
	// Before the check, the members' state is saved for the reload (haproxyGlobal). A HAProxy that does not answer
	// has no state to keep: the file goes, and HAProxy starts every member UP, as it does without one (a missing
	// file is a notice to it, src/server_state.c). The save can never fail the apply. The file's time is set when
	// it goes in: a pair's track script does not ask for its listeners while HAProxy loads it (lbpair.go).
	haproxyReload = `systemctl disable --now ssh ssh.socket 2>/dev/null
set -e
F=` + haproxyConfig + `
S=` + haproxyState + `
if echo "show servers state" | socat -t5 - /run/haproxy/admin.sock > $S.new 2>/dev/null && [ "$(head -n1 $S.new)" = 1 ]; then mv $S.new $S; else rm -f $S $S.new; fi
chown root:root $F.new
chmod 0644 $F.new
haproxy -c -q -f $F.new
cp $F $F.old
mv $F.new $F
` + lbServingFn + `swap() {
  touch $F
  systemctl is-active -q haproxy || systemctl start haproxy || return 1
  n=0
  until out=$(printf 'reload\n' | socat -t30 - /run/haproxy-master.sock 2>/dev/null); do
    n=$((n+1))
    [ $n -lt 20 ] || { echo "HAProxy's master socket does not answer" >&2; return 1; }
    sleep 0.3
  done
  case "$out" in Success=1*) ;; *) echo "$out" | grep ALERT >&2; return 1 ;; esac
  n=0
  until why=$(serving); do
    n=$((n+1))
    [ $n -lt 10 ] || { echo "$why" >&2; return 1; }
    sleep 0.5
  done
}
swap || { mv $F.old $F; swap; exit 1; }
` + lbCertPrune + `chmod 0644 $F $F.old
[ "$(stat -c %a:%U:%G $F)" = 644:root:root ]`
	// haproxyInstall prepares a stock image from its repositories, over the public NIC.
	// ponytail: a site with no egress bakes haproxy and socat into the image config.lbImage names.
	haproxyInstall = "pgrep -x apt-get >/dev/null || { apt-get update -qq; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq haproxy socat iproute2; }"
	guestTimeout   = 30 * time.Second
	// lbBusyExit is a ready check's answer while the guest's package manager runs: wait, and start no install.
	lbBusyExit = 3
)

// lbReadyScript answers 0 when an appliance can take a config: dpkg has every package installed and configured,
// no apt or dpkg runs, and the units exist. That the binaries exist is not enough: they do from the moment apt
// unpacks them, while dpkg still configures the package and starts its service (drill N2-2).
func lbReadyScript(packages, units string) string {
	return "pgrep -x 'apt-get|apt|dpkg' >/dev/null && exit " + strconv.Itoa(lbBusyExit) + "\n" +
		"for p in " + packages + "; do [ \"$(dpkg-query -W -f='${Status}' $p 2>/dev/null)\" = 'install ok installed' ] || exit 1; done\n" +
		"command -v ss >/dev/null && systemctl cat " + units + " >/dev/null 2>&1"
}

var haproxyReady = lbReadyScript("haproxy socat", "haproxy.service")

// PushHAProxy installs config on a running appliance, and HAProxy first if the image lacks it. certs are the
// certificate files config names (lbtls.go), written before it. A guest
// without its agent or without HAProxy yet answers ErrLBNotReady. It answers nil only when the running HAProxy
// was seen to serve config: any other answer means not applied, and the caller records nothing.
func (s *Service) PushHAProxy(ctx context.Context, node string, vmid int, config string, certs ...LBCertFile) error {
	if err := s.lbReady(ctx, node, vmid, haproxyReady, haproxyInstall); err != nil {
		return err
	}
	return s.swapHAProxy(ctx, node, vmid, config, certs)
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
	switch exit {
	case 0:
		return nil
	case lbBusyExit:
		return fmt.Errorf("%w: its package manager is running", ErrLBNotReady)
	}
	if _, err := s.C.GuestStart(ctx, node, vmid, "sh", "-c", install); err != nil {
		return fmt.Errorf("%w: %v", ErrLBNotReady, err)
	}
	return ErrLBNotReady
}

// swapHAProxy writes the certificate files config names (certs), then config, and makes HAProxy run it. The files
// go first: the check of the new config reads them.
func (s *Service) swapHAProxy(ctx context.Context, node string, vmid int, config string, certs []LBCertFile) error {
	if err := s.pushCertificates(ctx, node, vmid, certs); err != nil {
		return err
	}
	if err := s.C.GuestWriteFile(ctx, node, vmid, haproxyConfig+".new", config); err != nil {
		return err
	}
	exit, _, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", haproxyReload)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("HAProxy did not take the change, the last config stays in use: %s", strings.TrimSpace(stderr))
	}
	return nil
}

// HAProxyServing asks a single appliance whether HAProxy runs and listens on every bind of its config, and what is
// missing when it does not. With hash, the sha256 of the config that was applied, the file on the appliance must be
// that one too: listeners alone say nothing of a file edited by hand (spike of v0.11.1, SP-2).
func (s *Service) HAProxyServing(ctx context.Context, node string, vmid int, hash string) (serving bool, why string, err error) {
	exit, out, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", lbServingFn+"serving && echo ok $(sha256sum < "+haproxyConfig+" | cut -c1-64)")
	if err != nil {
		return false, "", err
	}
	out = strings.TrimSpace(out)
	sum, ok := strings.CutPrefix(out, "ok")
	switch sum = strings.TrimSpace(sum); {
	case exit == 0 && ok && sum != "" && hash != "" && sum != hash:
		return false, "HAProxy's configuration file is not the one that was applied", nil
	case exit == 0 && ok:
		return true, "", nil
	case out == "":
		return false, "", fmt.Errorf("haproxy state: exit %d %s", exit, strings.TrimSpace(stderr))
	}
	return false, out, nil
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
// each, as they always have. A listener with its own VIP opens its port on that address only, so a Service's source
// list never opens another Service's address.
func LBListenerRules(listeners []LBListener, vips []string) []FirewallRule {
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
	for _, proto := range []string{"tcp", "udp"} { // a listener's key is (family, port): TCP 53 and UDP 53 are two rules
		bySource := map[string]map[string][]int{} // dest -> source list -> ports
		for _, l := range listeners {
			if l.Port < 1 || l.Port > 65535 || lbFamily(l.Protocol) != proto {
				continue
			}
			ranges, msg := lbSourceRanges(l.SourceRanges)
			if msg != "" {
				continue // a stored row a validation never saw: nothing is opened for it
			}
			on := dests
			if l.VIP != "" {
				a, err := netip.ParseAddr(l.VIP)
				if err != nil || !a.Is4() || !slices.Contains(dests, a.String()) {
					continue
				}
				on = []string{a.String()}
			}
			src := strings.Join(ranges, ",")
			for _, dest := range on {
				if bySource[dest] == nil {
					bySource[dest] = map[string][]int{}
				}
				bySource[dest][src] = append(bySource[dest][src], l.Port)
			}
		}
		for _, dest := range dests {
			for _, src := range slices.Sorted(maps.Keys(bySource[dest])) {
				ports := slices.Clone(bySource[dest][src])
				slices.Sort(ports)
				for chunk := range slices.Chunk(ports, size) {
					nums := make([]string, len(chunk))
					for i, p := range chunk {
						nums[i] = strconv.Itoa(p)
					}
					out = append(out, FirewallRule{Type: "in", Action: "ACCEPT", Proto: proto, Source: src, Dest: dest,
						DPort: strings.Join(nums, ","), Comment: lbListenerComment})
				}
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

// LBNotLoaded is a member's status when the stored config holds it and the running HAProxy does not: the appliance
// does not run the stored config. A member the config leaves out (its server left) keeps "".
const LBNotLoaded = "NOT LOADED"

// LBMemberHealth is one member as HAProxy sees it.
type LBMemberHealth struct {
	Pool     string `json:"pool"`
	ServerID string `json:"serverId"`
	Address  string `json:"address,omitempty"` // a cluster's worker
	Port     int    `json:"port"`
	Status   string `json:"status"` // UP, DOWN, ... as HAProxy says; LBNotLoaded; "" = the config leaves it out
}

// ErrLBNoHAProxy: the appliance answered and its HAProxy did not (it is stopped). The next pass starts it.
var ErrLBNoHAProxy = errors.New("HAProxy does not answer on the appliance")

// LBHealth reads the running HAProxy's view of every member, and the listeners of spec it does not run (each as
// "port" or "address:port"): none when it runs the stored config. ErrLBNoHAProxy when the guest ran the command and
// HAProxy's socket did not answer.
func (s *Service) LBHealth(ctx context.Context, node string, vmid int, spec LBSpec) ([]LBMemberHealth, []string, error) {
	exit, out, stderr, err := s.C.GuestRun(ctx, node, vmid, guestTimeout, "sh", "-c", `echo "show stat" | socat - /run/haproxy/admin.sock`)
	if err != nil {
		return nil, nil, err
	}
	if exit != 0 {
		return nil, nil, fmt.Errorf("%w: %s", ErrLBNoHAProxy, strings.TrimSpace(stderr))
	}
	return ParseHAProxyStat(out, spec)
}

// ParseHAProxyStat maps `show stat` CSV back to the spec's members and listeners.
func ParseHAProxyStat(out string, spec LBSpec) ([]LBMemberHealth, []string, error) {
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(strings.TrimSpace(out), "# ")))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("haproxy stats: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil, fmt.Errorf("haproxy stats: empty")
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
	pools := map[string]LBPool{}
	backends := lbBackends(spec.Pools)
	for i, p := range spec.Pools {
		pools[p.Name] = p
		if p.Protocol == "udp" { // HAProxy holds no member of it: ParseLVSTable reads them
			continue
		}
		for _, m := range p.Members {
			addr := ""
			if m.Node {
				addr = m.Address
			}
			state, running := status[backends[i]+"/"+lbServerName(m)]
			if _, rendered := lbMemberAddr(m); rendered && !running {
				state = LBNotLoaded
			}
			health = append(health, LBMemberHealth{Pool: p.Name, ServerID: m.ServerID, Address: addr, Port: m.Port, Status: state})
		}
	}
	var notLoaded []string
	for _, l := range spec.Listeners {
		name, vip, ok := lbFrontend(l)
		pool, has := pools[l.Pool]
		if _, served := lbFrontendMode(l, pool); !ok || !has || !served || l.Port < 1 || l.Port > 65535 {
			continue
		}
		if _, running := status[name+"/FRONTEND"]; !running {
			notLoaded = append(notLoaded, strings.TrimPrefix(vip+":"+strconv.Itoa(l.Port), ":"))
		}
	}
	return health, notLoaded, nil
}
