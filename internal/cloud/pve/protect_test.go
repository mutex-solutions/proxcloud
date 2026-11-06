package pve

import "testing"

func TestTags(t *testing.T) {
	if got := addTag("a;b", "c"); got != "a;b;c" {
		t.Fatal(got)
	}
	if got := addTag("", "c"); got != "c" {
		t.Fatal(got)
	}
	if got := addTag("a;c", "c"); got != "a;c" {
		t.Fatal(got)
	}
	if got := removeTag("a;c;b", "c"); got != "a;b" {
		t.Fatal(got)
	}
	if HasTag("pc-protected-not", tagProtected) || !HasTag("x;pc-protected", tagProtected) {
		t.Fatal("HasTag matched wrong")
	}
}

func TestReserveMemLeavesRoomForFailover(t *testing.T) {
	nodes := []Node{{Node: "a", Status: "online", MaxCPU: 8, MaxMem: 8 << 30}, {Node: "b", Status: "online", MaxCPU: 8, MaxMem: 8 << 30}}
	// Without a reservation a 2 GiB VM fits on both; a takes it for having more free memory.
	req := SchedReq{VCPUs: 1, RAMMb: 2048}
	if got, err := PickNode(nodes, nil, req); err != nil || (got != "a" && got != "b") {
		t.Fatalf("got %s, %v", got, err)
	}
	// a must keep 7 GiB for a failover, so only b can take it.
	req.ReserveMem = map[string]int64{"a": 7 << 30}
	if got, err := PickNode(nodes, nil, req); err != nil || got != "b" {
		t.Fatalf("got %s, %v", got, err)
	}
	req.ReserveMem = map[string]int64{"a": 7 << 30, "b": 7 << 30}
	if _, err := PickNode(nodes, nil, req); err == nil {
		t.Fatal("placed a VM into memory reserved for failover")
	}
}
