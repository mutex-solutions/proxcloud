// Command e2egpu drives ONE GPU VM end-to-end on a real cluster: inventory →
// scheduler placement → hostpci reservation → boot → verify the card is attached
// → teardown. Production-safe: takes exactly one FREE mapping, never touches a
// card another VM holds, and destroys everything it made on every exit path.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

func main() {
	svc, err := pve.NewService(pve.Config{
		APIURL: os.Getenv("PVE_URL"), TokenID: os.Getenv("PVE_TOKENID"),
		Secret: os.Getenv("PVE_SECRET"), Fingerprint: os.Getenv("PVE_FP"),
	}, "svc-e2e-gpu", "pc-")
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	pid := "gpu" + fmt.Sprintf("%x", time.Now().Unix()%0xfffff)
	pool := svc.PoolFor(pid)
	alias := "nvidia-p40"
	mappings := []string{"gpu1", "gpu2", "gpu3", "gpu4"}
	fmt.Printf("== GPU E2E ==  project=%s pool=%s alias=%s\n", pid, pool, alias)

	defer func() {
		fmt.Println("\n== TEARDOWN ==")
		if r := recover(); r != nil {
			fmt.Printf("!! failed: %v\n", r)
		}
		tctx, tc := context.WithTimeout(context.Background(), 8*time.Minute)
		defer tc()
		if err := svc.TeardownProjectPool(tctx, pool); err != nil {
			fmt.Printf("   pool teardown: %v\n", err)
		} else {
			fmt.Printf("   pool %s + VMs destroyed\n", pool)
		}
		if p, _ := svc.C.GetPool(tctx, pool); p != nil {
			fmt.Println("   !! ORPHAN pool")
		} else {
			fmt.Println("   orphan check: pool gone ✓")
		}
		// The cards must be free again.
		if inv, err := svc.GPUInventoryFor(tctx, alias, mappings); err == nil {
			fmt.Printf("   GPU inventory after teardown: %d/%d free\n", len(inv.Free), len(inv.All))
		}
		fmt.Println("== DONE ==")
	}()

	fmt.Println("\n[1] GPU inventory (mappings → free cards)")
	inv, err := svc.GPUInventoryFor(ctx, alias, mappings)
	if err != nil {
		panic(err)
	}
	fmt.Printf("    total mapped: %d | free: %d\n", len(inv.All), len(inv.Free))
	for _, s := range inv.All {
		state := "IN USE"
		for _, f := range inv.Free {
			if f.MappingID == s.MappingID {
				state = "free"
			}
		}
		fmt.Printf("      %-6s node=%s  %s\n", s.MappingID, s.Node, state)
	}
	if len(inv.Free) == 0 {
		panic("no free GPU — refusing to touch a card another VM holds")
	}

	fmt.Println("\n[2] Scheduler: pick a node with a free card")
	nodes, err := svc.C.Nodes(ctx)
	if err != nil {
		panic(err)
	}
	vms, err := svc.C.ClusterResources(ctx, "vm")
	if err != nil {
		panic(err)
	}
	target, err := pve.PickNode(nodes, vms, pve.SchedReq{
		VCPUs: 2, RAMMb: 2048, OvercommitCPU: 4,
		GPUs: 1, GPUFree: inv.FreeCountByNode(),
	})
	if err != nil {
		panic(err)
	}
	picked, err := inv.PickGPUSlots(target, 1)
	if err != nil {
		panic(err)
	}
	fmt.Printf("    node=%s reserved mapping=%v\n", target, picked)

	fmt.Println("\n[3] Create the GPU VM")
	if _, err := svc.EnsureProjectPool(ctx, pid); err != nil {
		panic(err)
	}
	cr, err := svc.CreateServer(ctx, pve.CreateServerReq{
		ProjectID: pid, Region: "pve", Name: "e2e-gpu-vm",
		Spec: pve.ServerSpec{Slug: "gpu-1x-p40", VCPUs: 2, RAMMb: 2048, DiskGb: 4,
			StorageProfile: "std", GPUModel: alias, GPUCount: 1},
		TemplateVMID: 9999, TemplateNode: "pve", ImageSlug: "ubuntu-2404",
		TargetNode: target, Storage: "nvme", GPUMappingIDs: picked,
	})
	if err != nil {
		panic(err)
	}
	ext := cr.ExternalID
	fmt.Printf("    VM %s created (%v)\n", ext, cr.Data["server"].(map[string]any)["status"])

	fmt.Println("\n[4] Verify the passthrough landed on the VM")
	vm, err := svc.ResolveVM(ctx, pid, ext)
	if err != nil || vm == nil {
		panic(fmt.Errorf("resolve vm: %v", err))
	}
	cfg, err := svc.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		panic(err)
	}
	hostpci := cfg.Str("hostpci0")
	fmt.Printf("    hostpci0 = %q\n", hostpci)
	fmt.Printf("    machine  = %q (passthrough needs q35)\n", cfg.Str("machine"))
	fmt.Printf("    tags     = %q\n", cfg.Str("tags"))
	if !strings.Contains(hostpci, "mapping="+picked[0]) {
		panic(fmt.Errorf("hostpci0 does not carry the reserved mapping"))
	}
	if cfg.Str("machine") != "q35" {
		panic(fmt.Errorf("machine is not q35"))
	}

	fmt.Println("\n[5] Billing/quota contract: tag → extra_specs")
	specs := pve.GPUSpecsFromTags(cfg.Str("tags"))
	fmt.Printf("    extra_specs = %v\n", specs)
	if specs["pci_passthrough:alias"] != alias+":1" {
		panic(fmt.Errorf("extra_specs wrong: %v", specs))
	}

	fmt.Println("\n[6] Inventory now reports the card as taken")
	inv2, err := svc.GPUInventoryFor(ctx, alias, mappings)
	if err != nil {
		panic(err)
	}
	fmt.Printf("    free: %d (was %d)\n", len(inv2.Free), len(inv.Free))
	if len(inv2.Free) != len(inv.Free)-1 {
		panic(fmt.Errorf("free count did not drop by exactly one"))
	}
	for _, f := range inv2.Free {
		if f.MappingID == picked[0] {
			panic(fmt.Errorf("the reserved mapping is still advertised as free"))
		}
	}

	fmt.Println("\n[7] VM boots with the card attached (status check)")
	st, err := svc.C.GetVMStatus(ctx, vm.Node, vm.VMID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("    status=%s qmp=%s\n", st.Status, st.QMPStatus)
	if st.Status != "running" {
		panic(fmt.Errorf("VM is not running with the GPU attached (status %s)", st.Status))
	}

	fmt.Println("\n== GPU E2E PASSED ==")
}
