package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

func main() {
	svc, err := pve.NewService(pve.Config{
		APIURL: os.Getenv("PVE_URL"), TokenID: os.Getenv("PVE_TOKENID"),
		Secret: os.Getenv("PVE_SECRET"), Fingerprint: os.Getenv("PVE_FP"),
	}, "svc-pve-e2e", "pc-")
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	pid := "prj-e2e-01"
	if os.Args[1] == "up" {
		if err := svc.EnsureZone(ctx, pve.ZoneConfig{Key: "e2ez", Kind: "simple"}); err != nil {
			panic(err)
		}
		n, err := svc.CreateNetwork(ctx, pve.CreateNetworkReq{
			ProjectID: pid, Name: "platform-net", Zone: pve.ZoneConfig{Key: "e2ez", Kind: "simple"},
			CIDR: "10.223.7.0/24", DHCP: true, SNAT: true,
		})
		if err != nil {
			panic(err)
		}
		if err := svc.ApplyAndWait(ctx); err != nil {
			panic(err)
		}
		fmt.Println("vnet", n.Vnet)
		cr, err := svc.CreateServer(ctx, pve.CreateServerReq{
			ProjectID: pid, Region: "pve", Name: "platform-vm",
			Spec:         pve.ServerSpec{Slug: "std-1", VCPUs: 1, RAMMb: 1024, DiskGb: 4, StorageProfile: "std"},
			TemplateVMID: 9999, TemplateNode: "pve", ImageSlug: "ubuntu-2404",
			TargetNode: "pve", Storage: "nvme", Bridge: n.Vnet,
		})
		if err != nil {
			panic(err)
		}
		fmt.Println("vm", cr.ExternalID)
		return
	}
	// down
	if err := svc.TeardownProjectPool(ctx, svc.PoolFor(pid)); err != nil {
		fmt.Println("pool teardown:", err)
	}
	vnets, _ := svc.C.ListSDNVnets(ctx)
	for _, v := range vnets {
		if pidv, _, ok := pve.SplitOwnershipAlias(v.Alias); ok && pidv == pid {
			_ = svc.DeleteNetwork(ctx, pid, v.Vnet)
		}
	}
	_ = svc.C.DeleteSDNZone(ctx, "e2ez")
	_ = svc.ApplyAndWait(ctx)
	fmt.Println("torn down")
}
