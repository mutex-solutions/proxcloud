package cloud

import (
	"strings"
	"testing"
)

// The cases are the three refusals of Kubernetes 1.36 for a legacy CIDR field (apimachinery validation/ip.go,
// parseCIDR with strictValidation) and its ratchet: a value the object already holds stays valid.
func TestStrictCIDRs(t *testing.T) {
	for _, ok := range []string{"10.0.0.0/8", "203.0.113.7/32", "0.0.0.0/0", "2001:db8::/32", "2001:DB8::/32"} {
		if err := StrictCIDRs([]string{ok}, nil); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for in, want := range map[string]string{
		"192.168.0.5/24":      "192.168.0.5/24 has host bits set: use 192.168.0.0/24",
		"2001:db8::1/64":      "use 2001:db8::/64",
		"010.0.0.0/8":         "is not a CIDR",
		"10.0.0.0/08":         "is not a CIDR",
		"10.0.0.1":            "is not a CIDR",
		"banana":              "is not a CIDR",
		"::ffff:10.0.0.0/104": "IPv4-mapped",
	} {
		if err := StrictCIDRs([]string{"10.0.0.0/8", in}, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", in, err, want)
		}
	}
	// What is stored stays, beside a new canonical entry; a new entry of the same kind does not.
	stored := []string{"192.168.0.5/24"}
	if err := StrictCIDRs([]string{"192.168.0.5/24", "10.0.0.0/8"}, stored); err != nil {
		t.Errorf("stored entry refused: %v", err)
	}
	if err := StrictCIDRs([]string{"192.168.0.5/24", "10.1.2.3/8"}, stored); err == nil {
		t.Error("a new entry with host bits passed beside a stored one")
	}
	if got := CanonicalCIDR("192.168.0.5/24") + " " + CanonicalCIDR("banana"); got != "192.168.0.0/24 banana" {
		t.Errorf("CanonicalCIDR: %s", got)
	}
}
