package pve

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strconv"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// ProveVMAbsent is a fresh, same-credential absence proof for one allocation's
// original cached VM. Reuse (even a foreign UUID with no NIC today) deliberately
// retains addresses: a pending external attachment can still consume them.
// Call only AFTER successful SERVER/IMAGE reconcile and before exact IP CAS.
func (s *Service) ProveVMAndAddressUnused(ctx context.Context, project string, old *cloud.CloudResource, address string) error {
	if old == nil || old.ProjectID != project || old.Type != cloud.TypeServer {
		return vmInventoryError(cloud.TypeServer, "", "IP has no original VM cache snapshot; manual reconciliation required", nil)
	}
	id, err := strconv.Atoi(old.ExternalID)
	if err != nil || id <= 0 {
		return vmInventoryError(cloud.TypeServer, old.ExternalID, "invalid allocation VM identity", err)
	}
	inv, err := s.readVMInventory(ctx, project, cloud.TypeServer, nil, false)
	if err != nil {
		return err
	}
	if inv.row(id) != nil {
		return vmInventoryError(cloud.TypeServer, old.ExternalID, "VMID still exists or was reused; retain address", nil)
	}
	if net.ParseIP(address) == nil {
		return vmInventoryError(cloud.TypeServer, old.ExternalID, "allocation address malformed", nil)
	}
	if err := inv.verify(ctx, cloud.TypeServer, []cloud.CloudResource{*old}, nil); err != nil {
		return err
	}
	before, err := s.C.completeVMVisibility(ctx)
	if err != nil {
		return err
	}
	configs := map[int]VMConfig{}
	for _, r := range inv.page {
		if r.Type != "qemu" || !inv.online[r.Node] {
			return vmInventoryError(cloud.TypeServer, old.ExternalID, "IP reference census has unknown guest type or offline node", nil)
		}
		if err := s.C.vmInventoryPermission(ctx, "/vms/"+strconv.Itoa(r.VMID), "VM.Audit", false); err != nil {
			return err
		}
		if s.SeedArtifacts != nil {
			pending, err := s.SeedArtifacts.ServerMutationUnknown(ctx, s.ServiceID, strconv.Itoa(r.VMID))
			if err != nil || pending {
				return vmInventoryError(cloud.TypeServer, old.ExternalID, "unknown seed attachment can use allocation", err)
			}
		}
		if s.HolderOperations != nil {
			prefix, ok := strings.CutPrefix(r.Pool, s.PoolPrefix)
			if !ok || prefix == "" {
				return vmInventoryError(cloud.TypeServer, old.ExternalID, "foreign holder operation scope is unknown", nil)
			}
			pending, err := s.HolderOperations.Pending(ctx, s.holderScope(prefix))
			if err != nil || pending != nil {
				return vmInventoryError(cloud.TypeServer, old.ExternalID, "holder mutation can use allocation", err)
			}
		}
		cfg, err := s.inventoryCurrentConfig(ctx, r.Node, r.VMID)
		if err != nil {
			return vmInventoryError(cloud.TypeServer, old.ExternalID, "global IP reference configuration unreadable", err)
		}
		for key, val := range cfg {
			if !numberedConfigKey(key, "ipconfig") {
				continue
			}
			for _, kv := range splitCommaKV(val.(string)) {
				if kv[0] != "ip" && kv[0] != "ip6" {
					continue
				}
				ip, _, err := net.ParseCIDR(kv[1])
				if err == nil && ip.Equal(net.ParseIP(address)) {
					return vmInventoryError(cloud.TypeServer, old.ExternalID, "allocation is referenced by a current VM or template", nil)
				}
			}
		}
		// Network cards without declarative ipconfig may use a copied static
		// guest address. A foreign/unknown NIC is not positive non-use evidence.
		if len(nicKeys(cfg)) > 0 {
			return vmInventoryError(cloud.TypeServer, old.ExternalID, "another guest NIC can retain a static address; manual release required", nil)
		}
		configs[r.VMID] = cfg
	}
	for _, r := range inv.page {
		cfg, err := s.inventoryCurrentConfig(ctx, r.Node, r.VMID)
		if err != nil || !reflect.DeepEqual(cfg, configs[r.VMID]) {
			return vmInventoryError(cloud.TypeServer, old.ExternalID, "IP reference configuration changed", err)
		}
	}
	if err := inv.completeNodes(ctx); err != nil {
		return err
	}
	page, err := s.C.inventoryVMPage(ctx, inv.pool.PoolID)
	if err != nil || vmPageManifest(page) != vmPageManifest(inv.page) {
		return errors.Join(err, errors.New("IP reference census changed"))
	}
	after, err := s.C.completeVMVisibility(ctx)
	if err != nil || after != before {
		return errors.Join(err, errors.New("IP reference authority changed"))
	}
	return nil
}
