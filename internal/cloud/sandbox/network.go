package sandbox

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// Network is a sandbox's egress policy, E2B's SandboxNetworkConfig subset:
//
//	allowOut   CIDRs, bare IPs or domains ("example.com", "*.example.com") always reachable
//	denyOut    CIDRs or bare IPs blocked; "0.0.0.0/0" switches the sandbox to allow-list mode
//	dnsServers resolvers reachable in allow-list mode when allowOut has a domain (default 8.8.8.8)
//
// Private, CGNAT and link-local ranges stay unreachable whatever this says: the cluster-wide
// policy denies them with an egressDeny, which no per-sandbox allow can override.
type Network struct {
	AllowOut   []string  `json:"allowOut,omitempty"`
	DenyOut    []string  `json:"denyOut,omitempty"`
	DNSServers *[]string `json:"dnsServers,omitempty"`
}

const (
	maxNetworkRules  = 64
	defaultDNSServer = "8.8.8.8"
	allTraffic       = "0.0.0.0/0"
	CNPGroup         = "cilium.io"
	CNPVersion       = "v2"
	CNPPlural        = "ciliumnetworkpolicies"
	LabelSandboxID   = "proxcloud.io/sandbox-id"
)

var domainRe = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// parsedNetwork splits the rules into what the Cilium policy needs.
type parsedNetwork struct {
	allowCIDRs   []netip.Prefix
	allowDomains []string
	denyCIDRs    []netip.Prefix
	dnsServers   []netip.Addr
	allowList    bool // denyOut contains 0.0.0.0/0
}

func parsePrefix(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		if !p.Addr().Is4() {
			return netip.Prefix{}, fmt.Errorf("only IPv4 is supported: %q", s)
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return netip.Prefix{}, fmt.Errorf("not an IPv4 address or CIDR: %q", s)
	}
	return netip.PrefixFrom(a, 32), nil
}

// parse validates the rules. A nil Network is the default: open internet.
func (n *Network) parse() (parsedNetwork, error) {
	var p parsedNetwork
	if n == nil {
		return p, nil
	}
	if len(n.AllowOut)+len(n.DenyOut) > maxNetworkRules {
		return p, fmt.Errorf("network: at most %d allowOut + denyOut rules", maxNetworkRules)
	}
	for _, s := range n.AllowOut {
		s = strings.ToLower(strings.TrimSpace(s))
		if pr, err := parsePrefix(s); err == nil {
			p.allowCIDRs = append(p.allowCIDRs, pr)
			continue
		}
		if !domainRe.MatchString(s) {
			return p, fmt.Errorf("network.allowOut: %q is not a CIDR, IPv4 address or domain", s)
		}
		p.allowDomains = append(p.allowDomains, s)
	}
	for _, s := range n.DenyOut {
		pr, err := parsePrefix(strings.TrimSpace(s))
		if err != nil {
			return p, fmt.Errorf("network.denyOut: %w (domains are not supported for deny rules)", err)
		}
		if pr.Bits() == 0 {
			p.allowList = true
			continue
		}
		p.denyCIDRs = append(p.denyCIDRs, pr)
	}
	dns := []string{defaultDNSServer}
	if n.DNSServers != nil {
		dns = *n.DNSServers
	}
	if len(dns) > 10 {
		return p, fmt.Errorf("network.dnsServers: at most 10 entries")
	}
	for _, s := range dns {
		a, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil || !a.Is4() {
			return p, fmt.Errorf("network.dnsServers: %q is not an IPv4 address", s)
		}
		p.dnsServers = append(p.dnsServers, a)
	}
	return p, nil
}

// Validate reports a malformed policy (400).
func (n *Network) Validate() error {
	_, err := n.parse()
	return err
}

// AllowsInternet is false when the sandbox runs in allow-list mode with nothing allowed.
func (n *Network) AllowsInternet() bool {
	p, err := n.parse()
	return err != nil || !p.allowList || len(p.allowCIDRs)+len(p.allowDomains) > 0
}

// BuildEgressPolicy renders the per-sandbox CiliumNetworkPolicy. Every sandbox has one: the
// cluster-wide policy only grants DNS and denies private ranges, so a sandbox whose policy is
// missing has no internet at all (fail closed). ownerUID ties the policy to the Sandbox CR so
// Kubernetes garbage-collects it with the sandbox.
func BuildEgressPolicy(ns, id, ownerUID string, n *Network) (map[string]any, error) {
	p, err := n.parse()
	if err != nil {
		return nil, err
	}
	kubeDNS := map[string]any{
		"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{
			"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s:k8s-app": "kube-dns",
		}}},
		"toPorts": []any{map[string]any{
			"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}},
			// The DNS proxy is what lets toFQDNs learn which IPs a domain resolved to.
			"rules": map[string]any{"dns": []any{map[string]any{"matchPattern": "*"}}},
		}},
	}
	egress := []any{kubeDNS}
	var deny []any
	if p.allowList {
		if len(p.allowCIDRs) > 0 {
			egress = append(egress, map[string]any{"toCIDRSet": cidrSet(p.allowCIDRs, nil)})
		}
		if len(p.allowDomains) > 0 {
			var fqdns []any
			for _, d := range p.allowDomains {
				if strings.HasPrefix(d, "*.") {
					fqdns = append(fqdns, map[string]any{"matchPattern": d})
				} else {
					fqdns = append(fqdns, map[string]any{"matchName": d})
				}
			}
			egress = append(egress, map[string]any{"toFQDNs": fqdns})
			if len(p.dnsServers) > 0 {
				var dns []netip.Prefix
				for _, a := range p.dnsServers {
					dns = append(dns, netip.PrefixFrom(a, 32))
				}
				egress = append(egress, map[string]any{"toCIDRSet": cidrSet(dns, nil)})
			}
		}
	} else {
		egress = append(egress, map[string]any{"toCIDRSet": []any{map[string]any{"cidr": allTraffic}}})
		// "Allowed entries always take precedence over denied entries" (E2B): an allowed CIDR
		// inside a denied one becomes an `except` of that deny.
		for _, d := range p.denyCIDRs {
			var except []netip.Prefix
			for _, a := range p.allowCIDRs {
				if d.Bits() <= a.Bits() && d.Contains(a.Addr()) {
					except = append(except, a)
				}
			}
			deny = append(deny, cidrSet([]netip.Prefix{d}, except)...)
		}
	}
	spec := map[string]any{
		"endpointSelector": map[string]any{"matchLabels": map[string]any{LabelSandboxID: id}},
		"egress":           egress,
	}
	if len(deny) > 0 {
		spec["egressDeny"] = []any{map[string]any{"toCIDRSet": deny}}
	}
	meta := map[string]any{
		"name":      id + "-egress",
		"namespace": ns,
		"labels":    map[string]any{LabelManagedBy: ManagedByValue, LabelSandboxID: id},
	}
	if ownerUID != "" {
		meta["ownerReferences"] = []any{map[string]any{
			"apiVersion": CRGroup + "/" + CRVersion, "kind": "Sandbox", "name": id, "uid": ownerUID,
			"blockOwnerDeletion": false,
		}}
	}
	return map[string]any{
		"apiVersion": CNPGroup + "/" + CNPVersion,
		"kind":       "CiliumNetworkPolicy",
		"metadata":   meta,
		"spec":       spec,
	}, nil
}

func cidrSet(ps []netip.Prefix, except []netip.Prefix) []any {
	out := make([]any, 0, len(ps))
	for _, pr := range ps {
		m := map[string]any{"cidr": pr.String()}
		if len(except) > 0 {
			ex := make([]any, 0, len(except))
			for _, e := range except {
				ex = append(ex, e.String())
			}
			m["except"] = ex
		}
		out = append(out, m)
	}
	return out
}
