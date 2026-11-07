package pve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
)

// The public entry of the sandbox cluster (docs/managed-sandbox.md §4): a platform HAProxy appliance, the load
// balancer appliance of P8-1 with one TCP listener, passes a port through to sandbox-proxy's MetalLB address on the
// sandbox vnet. TLS ends in sandbox-proxy, so the appliance never holds the certificate. The installer builds the VM
// (stage 56); the Proxmox provider's sync keeps its firewall and HAProxy config in line here.

// SandboxEntryTag marks the entry appliance. The sync touches a VM only when it carries it.
const SandboxEntryTag = "proxcloud-sandbox-entry"

// sandboxEntryFirewallOptions: anti-spoofing on, nothing comes in but the listener, no DHCP (static addresses).
var sandboxEntryFirewallOptions = map[string]string{
	"enable": "1", "ipfilter": "1", "macfilter": "1", "dhcp": "0", "ndp": "0", "radv": "0",
	"policy_in": "DROP", "policy_out": "ACCEPT",
}

// SandboxEntryConfig is the appliance's HAProxy config: port, passed through to proxy:port.
func SandboxEntryConfig(proxy string, port int) string {
	return RenderHAProxy(LBSpec{
		Listeners: []LBListener{{Port: port, Pool: "sandbox-proxy"}},
		Pools: []LBPool{{Name: "sandbox-proxy", Protocol: "tcp", Algorithm: "roundrobin",
			Members: []LBMember{{ServerID: "0", Port: port, Weight: 1, Address: proxy}}}},
	})
}

// SandboxEntry keeps the entry appliance vmid in line: its inbound firewall opens port alone, and HAProxy runs
// SandboxEntryConfig. The config is pushed only when the file on the appliance differs or HAProxy is not running. A
// VM that is not the appliance (no SandboxEntryTag, or in the sandbox pool: a recycled vmid) is never touched.
func (s *Service) SandboxEntry(ctx context.Context, vmid int, sandboxPool, proxy string, port int) error {
	addr, err := netip.ParseAddr(proxy)
	if err != nil || !addr.Is4() || port < 1 || port > 65535 {
		return fmt.Errorf("pve sandbox entry: invalid proxy address %q or port %d", proxy, port)
	}
	vms, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return err
	}
	var vm *ClusterResource
	for i := range vms {
		if vms[i].Type == "qemu" && vms[i].VMID == vmid {
			vm = &vms[i]
		}
	}
	switch {
	case vm == nil:
		return fmt.Errorf("pve sandbox entry: VM %d does not exist", vmid)
	case !HasTag(vm.Tags, SandboxEntryTag) || vm.Pool == sandboxPool || vm.Template == 1:
		return fmt.Errorf("pve sandbox entry: VM %d is not the entry appliance (tag %s)", vmid, SandboxEntryTag)
	case vm.Status != "running":
		return fmt.Errorf("pve sandbox entry: VM %d is %s", vmid, vm.Status)
	}
	// The installer pins each NIC's address (ipfilter-net0/1); the options keep that filter and the inbound DROP on.
	if err := s.C.SetVMFirewallOptions(ctx, vm.Node, vmid, sandboxEntryFirewallOptions); err != nil {
		return err
	}
	if err := s.SetVMInboundPorts(ctx, vm.Node, vmid, []int{port}); err != nil {
		return err
	}
	config := SandboxEntryConfig(addr.String(), port)
	sum := sha256.Sum256([]byte(config))
	exit, out, _, err := s.C.GuestRun(ctx, vm.Node, vmid, guestTimeout, "sh", "-c",
		"sha256sum "+haproxyConfig+" && systemctl is-active -q haproxy")
	if err == nil && exit == 0 && strings.HasPrefix(out, hex.EncodeToString(sum[:])) {
		return nil
	}
	return s.PushHAProxy(ctx, vm.Node, vmid, config)
}
