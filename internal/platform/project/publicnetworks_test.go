package project

import "testing"

func TestPublicNetworkAllowed(t *testing.T) {
	// nil allow-list = all allowed
	if !publicNetworkAllowed(&Project{}, "vmbr0") {
		t.Fatal("nil list must allow")
	}
	// empty list = none
	if publicNetworkAllowed(&Project{PublicNetworkIds: []string{}}, "vmbr0") {
		t.Fatal("empty list must deny")
	}
	p := &Project{PublicNetworkIds: []string{"vmbr0"}}
	if !publicNetworkAllowed(p, "vmbr0") || publicNetworkAllowed(p, "vmbr1") {
		t.Fatal("allow-list filter broken")
	}
}
