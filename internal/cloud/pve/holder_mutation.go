package pve

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// These adapters are the entire holder mutation surface. Non-holder bulk copies
// keep their existing WaitTask; the durable intent still fences future writers.
func (s *Service) holderConfig(ctx context.Context, project string, vm *ClusterResource, params map[string]string, del ...string) error {
	p, err := s.holderProofFor(ctx, project, "config", vm)
	if err != nil {
		return err
	}
	p.Params = maps.Clone(params)
	p.Delete = del
	return s.holderTask(ctx, p, s.HolderBudgets.defaults().Config, func(q context.Context) (UPID, error) { return s.C.SetVMConfig(q, vm.Node, vm.VMID, params, del...) })
}
func (s *Service) holderMove(ctx context.Context, project string, vm, target *ClusterResource, slot, free string) error {
	p, err := s.holderProofFor(ctx, project, "move", vm)
	if err != nil {
		return err
	}
	p.Slot, p.TargetSlot = slot, free
	p.Disk = diskVol(p.Source.Disks[slot])
	t, err := s.holderProofFor(ctx, project, "move", target)
	if err != nil {
		return err
	}
	p.Target = &t.Source
	return s.holderTask(ctx, p, s.HolderBudgets.defaults().Config, func(q context.Context) (UPID, error) {
		return s.C.MoveDisk(q, vm.Node, vm.VMID, MoveDiskOpts{Disk: slot, TargetVMID: target.VMID, TargetDisk: free})
	})
}
func (s *Service) holderStop(ctx context.Context, project string, vm *ClusterResource) error {
	p, err := s.holderProofFor(ctx, project, "stop", vm)
	if err != nil {
		return err
	}
	return s.holderTask(ctx, p, s.HolderBudgets.defaults().Config, func(q context.Context) (UPID, error) { return s.C.StopVM(q, vm.Node, vm.VMID) })
}
func (s *Service) holderDelete(ctx context.Context, project string, vm *ClusterResource) error {
	p, err := s.holderProofFor(ctx, project, "delete", vm)
	if err != nil {
		return err
	}
	return s.holderTask(ctx, p, s.HolderBudgets.defaults().Config, func(q context.Context) (UPID, error) { return s.C.DeleteVM(q, vm.Node, vm.VMID) })
}
func (s *Service) newFencedHolder(ctx context.Context, project, node, tags string) (*ClusterResource, error) {
	ctx, release, err := s.beginHolders(ctx, project)
	if err != nil {
		return nil, err
	}
	defer release()
	id, err := s.C.NextID(ctx)
	if err != nil {
		return nil, err
	}
	identity := "uuid=" + uuid.NewString()
	p := holderProof{Kind: "create", Pool: s.PoolFor(project), Source: holderVMIdentity{Node: node, VMID: id, UUID: identity}}
	// Name/tags still follow pool admission for narrow token rights. The UUID is
	// in the FIRST create request and cannot be inferred from a blank config.
	err = s.holderTask(ctx, p, s.HolderBudgets.defaults().Config, func(q context.Context) (UPID, error) {
		return s.C.CreateVM(q, node, map[string]string{"vmid": strconv.Itoa(id), "pool": p.Pool, "smbios1": identity})
	})
	if err != nil {
		return nil, err
	} // no destructive compensation, even if it looks empty
	vm := &ClusterResource{Type: "qemu", Node: node, VMID: id, Pool: p.Pool, Tags: tags, Name: "pc-holder-" + node}
	if err = s.holderConfig(ctx, project, vm, map[string]string{"name": vm.Name, "memory": "16", "cores": "1", "onboot": "0", "tags": tags}); err != nil {
		return nil, err
	}
	return vm, nil
}
func (s *Service) fencedCarry(ctx context.Context, project string, vm *ClusterResource, slot, node string) (*ClusterResource, string, error) {
	carrier, err := s.newFencedHolder(ctx, project, vm.Node, JoinTags(tagHolder, tagCarrier, TagManaged))
	if err != nil {
		return nil, "", err
	}
	const carried = "unused0"
	if err = s.holderMove(ctx, project, vm, carrier, slot, carried); err != nil {
		return nil, "", err
	}
	p, err := s.holderProofFor(ctx, project, "migrate", carrier)
	if err != nil {
		return nil, "", err
	}
	t := p.Source
	t.Node = node
	p.Target = &t
	err = s.holderTask(ctx, p, s.HolderBudgets.defaults().Migration, func(q context.Context) (UPID, error) { return s.C.MigrateVM(q, vm.Node, carrier.VMID, node, false) })
	if err != nil {
		return nil, "", err
	}
	moved := *carrier
	moved.Node = node
	return &moved, carried, nil
}

// deleteFencedServer is also used for project teardown. Holder deletion is only
// admitted here from teardown/empty-holder cleanup, never a tenant server action.
func (s *Service) deleteFencedServer(ctx context.Context, project string, vm *ClusterResource, allowHolder bool) error {
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	if isHolder(cfg.Str("tags")) && !allowHolder {
		return conflictf("A holder is not a server; delete its detached volumes instead")
	}
	if err = s.holderCheck(ctx); err != nil {
		return err
	}
	// Holders have no HA/replication; refusing unexpected protection avoids any
	// legacy protection compensation from running in their uncertain operation.
	if isHolder(cfg.Str("tags")) {
		if HasTag(cfg.Str("tags"), tagProtected) || cfg.Str("lock") != "" {
			return conflictf("Holder is protected or busy; administrator inspection required")
		}
	} else {
		if err = s.disableProtectionOn(ctx, vm); err != nil {
			return err
		}
		s.waitReplicaRemoved(ctx, vm.VMID)
	}
	st, err := s.C.GetVMStatus(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	if st.Status != "stopped" {
		if err = s.holderStop(ctx, project, vm); err != nil {
			return err
		}
	}
	return s.holderDelete(ctx, project, vm)
}
func (s *Service) holderProjectForPool(pool string) (string, error) {
	if !strings.HasPrefix(pool, s.PoolPrefix) || len(pool) == len(s.PoolPrefix) {
		return "", fmt.Errorf("pve: pool does not match this provider")
	}
	return strings.TrimPrefix(pool, s.PoolPrefix), nil
}
