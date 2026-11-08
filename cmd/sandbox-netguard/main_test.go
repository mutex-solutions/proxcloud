package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRateValidation(t *testing.T) {
	for _, s := range []string{"", "0M", "-1M", "1G", "100001M", "1M;reboot"} {
		if _, e := parseRate(s); e == nil {
			t.Fatal(s)
		}
	}
	if n, e := parseRate("100M"); e != nil || n != 100 {
		t.Fatal(n, e)
	}
}
func TestPrevResultRequiresCiliumHostVeth(t *testing.T) {
	if _, e := hostVeth(json.RawMessage(`{"interfaces":[{"name":"eth0","sandbox":"/run/netns/a"}]}`)); e == nil {
		t.Fatal("accepted no host veth")
	}
	n, e := hostVeth(json.RawMessage(`{"interfaces":[{"name":"lxc1234"},{"name":"eth0","sandbox":"/run/netns/a"}]}`))
	if e != nil || n != "lxc1234" {
		t.Fatal(n, e)
	}
}
func TestHostOwnedTBFNoCiliumFilterClobber(t *testing.T) {
	old := run
	defer func() { run = old }()
	var args string
	run = func(name string, a ...string) ([]byte, error) {
		args = name + " " + strings.Join(a, " ")
		return nil, nil
	}
	if e := police("/run/netns/cni-a", 1); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(args, "nsenter --net=/run/netns/cni-a -- tc qdisc replace dev eth0 root tbf rate 1mbit burst 65536") {
		t.Fatal(args)
	}
	if strings.Contains(args, "filter") {
		t.Fatal("touches Cilium filters")
	}
	run = func(string, ...string) ([]byte, error) { return nil, errors.New("tc missing") }
	if police("/run/netns/a", 1) == nil {
		t.Fatal("must fail closed")
	}
}
