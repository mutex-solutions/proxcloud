// Command e2edrv drives the Proxmox VPC + VM lifecycle against a REAL cluster
// through the pve.Service the platform uses. Production-safe: every resource is
// prefixed "e2e", nothing pre-existing is touched, and teardown runs on every
// exit path (defer + panic recovery). Read-only until it creates its own pool.
//
//	PVE_URL=… PVE_TOKENID=… PVE_SECRET=… PVE_FP=… go run ./cmd/e2edrv
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

func env(k string) string { return os.Getenv(k) }

func step(n int, msg string)      { fmt.Printf("\n[%d] %s\n", n, msg) }
func ok(msg string, a ...any)     { fmt.Printf("    ✓ "+msg+"\n", a...) }
func fatal(err error, ctx string) { panic(fmt.Errorf("%s: %w", ctx, err)) }

func main() {
	cfg := pve.Config{
		APIURL:      env("PVE_URL"),
		TokenID:     env("PVE_TOKENID"),
		Secret:      env("PVE_SECRET"),
		Fingerprint: env("PVE_FP"),
	}
	svc, err := pve.NewService(cfg, "svc-e2e", "pc-")
	if err != nil {
		fatal(err, "new service")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// Unique, obviously-scoped identifiers. base36 of the minute keeps it short + unique enough.
	pid := "e2e" + fmt.Sprintf("%x", time.Now().Unix()%0xfffff)
	zone := "z" + pid[:6] // simple SDN zone (≤8 chars), fully isolated + torn down
	pool := svc.PoolFor(pid)
	fmt.Printf("== proxcloud E2E ==  project=%s pool=%s zone=%s\n", pid, pool, zone)

	// Teardown is registered up front so a failure mid-way still cleans up.
	var vnetID, sgID, vmExtID string
	defer func() {
		fmt.Println("\n== TEARDOWN ==")
		if r := recover(); r != nil {
			fmt.Printf("!! run failed: %v\n", r)
		}
		tctx, tcancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer tcancel()
		if sgID != "" {
			if err := svc.DeleteSecurityGroup(tctx, pid, sgID); err != nil {
				fmt.Printf("   SG %s delete: %v\n", sgID, err)
			} else {
				fmt.Printf("   SG %s deleted\n", sgID)
			}
		}
		// TeardownProjectPool stops+destroys every VM in the pool, then the pool.
		if err := svc.TeardownProjectPool(tctx, pool); err != nil {
			fmt.Printf("   pool teardown: %v\n", err)
		} else {
			fmt.Printf("   pool %s + its VMs destroyed\n", pool)
		}
		if vnetID != "" {
			if err := svc.DeleteNetwork(tctx, pid, vnetID); err != nil {
				fmt.Printf("   vnet %s delete: %v\n", vnetID, err)
			}
			if err := svc.ApplyAndWait(tctx); err != nil {
				fmt.Printf("   sdn apply (vnet delete): %v\n", err)
			} else {
				fmt.Printf("   vnet %s deleted + applied\n", vnetID)
			}
		}
		if err := svc.C.DeleteSDNZone(tctx, zone); err != nil {
			fmt.Printf("   zone %s delete: %v\n", zone, err)
		} else {
			if err := svc.ApplyAndWait(tctx); err != nil {
				fmt.Printf("   sdn apply (zone delete): %v\n", err)
			} else {
				fmt.Printf("   zone %s deleted + applied\n", zone)
			}
		}
		// Orphan check
		if p, _ := svc.C.GetPool(tctx, pool); p != nil {
			fmt.Printf("   !! ORPHAN pool still present: %s\n", pool)
		} else {
			fmt.Println("   orphan check: pool gone ✓")
		}
		vnets, _ := svc.C.ListSDNVnets(tctx)
		leaked := false
		for _, v := range vnets {
			if v.Vnet == vnetID {
				leaked = true
			}
		}
		if leaked {
			fmt.Printf("   !! ORPHAN vnet still present: %s\n", vnetID)
		} else {
			fmt.Println("   orphan check: vnet gone ✓")
		}
		fmt.Println("== DONE ==")
	}()

	// --- 1. pool bootstrap ---
	step(1, "EnsureProjectPool (project provisioning)")
	if _, err := svc.EnsureProjectPool(ctx, pid); err != nil {
		fatal(err, "ensure pool")
	}
	ok("pool %s created", pool)

	// --- 2. VPC: SDN zone + vnet + subnet + apply ---
	step(2, "VPC: create isolated network (zone + vnet + subnet, DHCP+SNAT)")
	if err := svc.EnsureZone(ctx, pve.ZoneConfig{Key: zone, Kind: "simple"}); err != nil {
		fatal(err, "ensure zone")
	}
	ok("zone %s (simple) staged", zone)
	netRes, err := svc.CreateNetwork(ctx, pve.CreateNetworkReq{
		ProjectID: pid, Name: "e2e-net",
		Zone: pve.ZoneConfig{Key: zone, Kind: "simple"},
		CIDR: "10.222.44.0/24", DHCP: true, SNAT: true,
	})
	if err != nil {
		fatal(err, "create network")
	}
	vnetID = netRes.Vnet
	ok("vnet %s staged (cidr %s gw %s)", vnetID, netRes.CIDR, netRes.Gateway)
	if err := svc.ApplyAndWait(ctx); err != nil {
		fatal(err, "sdn apply")
	}
	ok("SDN applied cluster-wide (network live)")
	// verify vnet + subnet present
	vnets, _ := svc.C.ListSDNVnets(ctx)
	found := false
	for _, v := range vnets {
		if v.Vnet == vnetID {
			found = true
		}
	}
	if !found {
		fatal(fmt.Errorf("vnet not listed after apply"), "verify vnet")
	}
	subs, _ := svc.C.ListSDNSubnets(ctx, vnetID)
	ok("verified: vnet present, %d subnet(s)", len(subs))

	// --- 3. VM create (clone from template) ---
	step(3, "VM: clone from template 9999, attach to e2e vnet, boot")
	spec := pve.ServerSpec{Slug: "e2e-small", VCPUs: 1, RAMMb: 1024, DiskGb: 4}
	cr, err := svc.CreateServer(ctx, pve.CreateServerReq{
		ProjectID: pid, Region: "pve", Name: "e2e-vm1", Spec: spec,
		TemplateVMID: 9999, TemplateNode: "pve", ImageSlug: "ubuntu-2404",
		TargetNode: "pve", Storage: "nvme", Bridge: vnetID, Password: "E2eTest!Pass9",
	})
	if err != nil {
		fatal(err, "create server")
	}
	vmExtID = cr.ExternalID
	srv := cr.Data["server"].(map[string]any)
	ok("VM %s created: name=%v status=%v node=%v", vmExtID, srv["name"], srv["status"], srv["host"])
	fl := srv["flavor"].(map[string]any)
	ok("flavor stamped: vcpus=%v ram=%v disk=%v (billing contract)", fl["vcpus"], fl["ram"], fl["disk"])

	// --- 4. power ops ---
	step(4, "Power lifecycle: STOP → START → reboot")
	if err := svc.PowerAction(ctx, pid, vmExtID, "STOP"); err != nil {
		fatal(err, "stop")
	}
	st, _ := reResolveStatus(ctx, svc, pid, vmExtID)
	ok("stopped: %s", st)
	if err := svc.PowerAction(ctx, pid, vmExtID, "START"); err != nil {
		fatal(err, "start")
	}
	st, _ = reResolveStatus(ctx, svc, pid, vmExtID)
	ok("started: %s", st)

	// --- 5. resize ---
	step(5, "Resize: e2e-small → e2e-large (2c/2g/6g)")
	if err := svc.ResizeServer(ctx, pid, vmExtID, pve.ServerSpec{Slug: "e2e-large", VCPUs: 2, RAMMb: 2048, DiskGb: 6}, 10); err != nil {
		fatal(err, "resize")
	}
	vm, _ := svc.ResolveVM(ctx, pid, vmExtID)
	cfg2, _ := svc.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	ok("resized: cores=%d memory=%s", cfg2.Int("cores"), cfg2.Str("memory"))

	// --- 6. volume add ---
	step(6, "Volume: add a 3 GiB data disk")
	slot, err := svc.AddVolume(ctx, pid, vmExtID, 3, "nvme", 10)
	if err != nil {
		fatal(err, "add volume")
	}
	ok("data disk added at slot %s", slot)

	// --- 7. security group ---
	step(7, "Security group: create + rule (allow tcp/22) + attach to VM")
	sgID, err = svc.CreateSecurityGroup(ctx, pid, "e2e-sg")
	if err != nil {
		fatal(err, "create sg")
	}
	ok("SG %s created", sgID)
	if err := svc.SGAddRule(ctx, pid, sgID, pve.FirewallRule{Type: "in", Action: "ACCEPT", Proto: "tcp", DPort: "22", Comment: "e2e ssh"}); err != nil {
		fatal(err, "sg rule")
	}
	ok("rule allow tcp/22 added")
	if err := svc.AttachSecurityGroup(ctx, pid, vmExtID, sgID); err != nil {
		fatal(err, "attach sg")
	}
	ok("SG attached to VM + firewall enabled on NICs")

	// --- 8. console ---
	step(8, "Console: mint a VNC websocket ticket (proxy target)")
	backend, _, ticket, err := svc.Console(ctx, pid, vmExtID, false)
	if err != nil {
		fatal(err, "console")
	}
	ok("console backend: %s (ticket len %d)", backend, len(ticket))

	fmt.Println("\n== ALL FEATURES PASSED ==  (teardown next)")
}

func reResolveStatus(ctx context.Context, svc *pve.Service, pid, ext string) (string, error) {
	vm, err := svc.ResolveVM(ctx, pid, ext)
	if err != nil || vm == nil {
		return "?", err
	}
	s, err := svc.C.GetVMStatus(ctx, vm.Node, vm.VMID)
	if err != nil {
		return "?", err
	}
	return s.Status, nil
}
