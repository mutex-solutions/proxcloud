package pve

import (
	"slices"
	"testing"
)

// A NIC with a VLAN tag is checked on /sdn/zones/localnetwork/<bridge>/<tag>, and the NIC of a routed platform
// network on /sdn/zones/<zone>/<vnet>: the managed-services script reaches both with one row, the CAPI role on
// /sdn/zones with propagate, for the user and for the token (a token holds the intersection of the two).
func TestManagedACLsReachTheNICPaths(t *testing.T) {
	acls := ManagedACLs(DefaultPlatformTokenID, "local-lvm", "local", "")
	i := slices.IndexFunc(acls, func(a ManagedACL) bool { return a.Path == "/sdn/zones" && a.Role == roleCAPI })
	if i < 0 || !acls[i].Propagate || acls[i].User != capiUser || acls[i].Token != CAPITokenID {
		t.Fatalf("no propagated %s row on /sdn/zones for %s and its token: %+v", roleCAPI, capiUser, acls)
	}
	if !slices.Contains(ManagedRoles[0].Privs, "SDN.Use") || ManagedRoles[0].Name != roleCAPI {
		t.Fatalf("%s lacks SDN.Use: %+v", roleCAPI, ManagedRoles[0])
	}
}
