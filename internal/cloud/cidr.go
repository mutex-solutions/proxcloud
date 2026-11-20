package cloud

import (
	"fmt"
	"net/netip"
	"slices"
)

// StrictCIDRs is the rule for a list of CIDRs that ends in a built-in Kubernetes field (a Service's
// loadBalancerSourceRanges). From Kubernetes 1.36 the API server refuses there a new or changed value with a leading
// zero, an IPv4-mapped IPv6 address or bits set beyond the prefix length, and keeps accepting a value the object
// already holds (StrictIPCIDRValidation, KEP-4858). So an entry that is in stored passes as it is and every other
// entry must be canonical. The error names the first entry that is not, with the form to write where there is one.
func StrictCIDRs(list, stored []string) error {
	for _, c := range list {
		if slices.Contains(stored, c) {
			continue
		}
		p, err := netip.ParsePrefix(c) // refuses a leading zero in an octet or in the prefix length, and a zone
		switch {
		case err != nil:
			return fmt.Errorf("%q is not a CIDR such as 203.0.113.0/24 (one address is 203.0.113.7/32)", c)
		case p.Addr().Is4In6():
			return fmt.Errorf("%q is an IPv4-mapped IPv6 CIDR: write the IPv4 network", c)
		case p != p.Masked():
			return fmt.Errorf("%s has host bits set: use %s", c, p.Masked())
		}
	}
	return nil
}

// CanonicalCIDR is c without its host bits (192.168.0.5/24 is 192.168.0.0/24, the network Kubernetes read it as before
// 1.36), and c itself when it is not a CIDR.
func CanonicalCIDR(c string) string {
	if p, err := netip.ParsePrefix(c); err == nil {
		return p.Masked().String()
	}
	return c
}
