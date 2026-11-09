package pve

import (
	"regexp"
	"strings"
)

// vmNameRe is PVE's dns-name format for a VM's name (pve_verify_dns_name in PVE::JSONSchema, read on 9.2).
var vmNameRe = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)*[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?$`)

// ValidVMName reports whether PVE takes name for a VM. PVE sets no length, but the name becomes the guest's hostname, so
// a DNS name's limits apply as well.
func ValidVMName(name string) bool {
	if len(name) > 253 || !vmNameRe.MatchString(name) {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) > 63 {
			return false
		}
	}
	return true
}
