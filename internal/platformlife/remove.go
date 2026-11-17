package platformlife

// remove.go — the cluster part of a remove (design §4.6, steps 1 to 5), before the seed part in stages.go. Every step
// observes again, so a remove resumes after a crash. With the seed VM gone, Cluster API cannot delete anything: the
// typed confirmation then deletes the VMs of proxcloud-system tagged for this installation, and the recorded volumes.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// tenantSelector matches the Applications the Kubernetes and database providers create on the workload's ArgoCD.
const tenantSelector = "app.kubernetes.io/managed-by=proxcloud"

// note adds items to the current stage, each replacing the one of its id: a Remove lists what it deleted, what it
// left and what it could not check, so the finished run says what happened on Proxmox.
func (x *acq) note(ctx context.Context, items ...StageItem) error {
	cur := slices.Clone(x.run.Stages[x.si].Items)
	for _, it := range items {
		if i := slices.IndexFunc(cur, func(c StageItem) bool { return c.ID == it.ID }); i >= 0 {
			cur[i] = it
		} else {
			cur = append(cur, it)
		}
	}
	if slices.Equal(cur, x.run.Stages[x.si].Items) {
		return nil
	}
	return x.saveStage(ctx, func(s *Stage) { s.Items = cur })
}

func (x *acq) removeCluster(ctx context.Context) error {
	st := x.in.Status.Seed
	if x.sec.SeedKubeconfig == nil && len(x.in.Status.Clusters) == 0 && len(x.in.Status.Volumes) == 0 {
		return errSkipped // no seed ever became ready, so nothing was built with it
	}
	if err := x.needCAPI(); err != nil {
		return err
	}
	gone := st == nil
	if !gone {
		vm, err := x.observe(ctx, st.VMID, st.Generation)
		if err != nil {
			return err
		}
		gone = vm.absent || vm.foreign
	}
	if gone {
		if len(x.in.Status.Clusters) == 0 && len(x.in.Status.Volumes) == 0 {
			// An earlier Remove finished this part and then deleted the seed VM, or the apply never reached stage 7
			// (which records the cluster) and the seed VM was deleted by hand: nothing is left to delete.
			return x.note(ctx, StageItem{ID: "cluster", State: "gone", Message: "No platform cluster and no volume is recorded and the seed VM is gone: " +
				"nothing to delete here (an earlier Remove deleted them, or none was built)."})
		}
		if !x.run.Confirmed {
			return permanent(CodeSeedLost, "The seed VM is gone, so Cluster API cannot delete the workload cluster. Start Remove with confirm \"remove %s\" "+
				"to delete the VMs of pool %s tagged pc-platform-%s and the %d recorded volumes.", x.in.Name, pve.SystemPool, x.in.ID, len(x.in.Status.Volumes))
		}
		if err := x.disableProviders(ctx); err != nil {
			return err
		}
		if err := x.note(ctx, StageItem{ID: "tenants", State: "not checked", Message: "The seed VM is gone, so nothing could say whether tenant clusters or databases still ran on the platform."}); err != nil {
			return err
		}
		if err := x.deleteTagged(ctx); err != nil {
			return err
		}
		return x.clusterRemoved(ctx)
	}
	seed, err := x.seed()
	if err != nil {
		return err
	}
	cl, err := seed.GetCR(ctx, capiGroup, capiVersion, "clusters", ClusterName, ClusterName)
	if err != nil && !kamajik8s.NotFound(err) { // 404: Cluster API was never installed
		return classify(CodeSeedAPIUnreachable, err)
	}
	app, err := seed.GetApplication(ctx, argoNamespace, ClusterName)
	if err != nil && !kamajik8s.NotFound(err) {
		return classify(CodeSeedAPIUnreachable, err)
	}
	live := cl != nil && str(cl, "metadata", "deletionTimestamp") == ""
	var wl *kamajik8s.Client
	if live {
		// No tenant can be on a cluster whose control plane never came up: its silence is not retried and needs no
		// confirmation. The recorded providers do not say so: stage 10 records them only after both are registered,
		// and the first row is public from the moment it exists.
		empty := dig(cl, "status", "initialization", "controlPlaneInitialized") != true && condition(cl, "ControlPlaneInitialized") != "True"
		if wl, err = x.tenantsGone(ctx, seed, empty); err != nil {
			return err
		}
		tenants := StageItem{ID: "tenants", State: "none", Message: "The platform cluster lists no tenant cluster or database."}
		switch {
		case wl != nil:
		case empty:
			tenants.State, tenants.Message = "not checked", "The cluster's control plane never came up, so no tenant cluster or database could run on it."
		default:
			tenants.State, tenants.Message = "not checked", "The cluster did not answer whether tenant clusters or databases still ran on it; the run went on without the check."
		}
		items := []StageItem{tenants}
		if wl == nil && len(x.in.Status.Volumes) == 0 && !empty {
			// Passed on the confirmation alone: the volumes could not be read again, and none was recorded.
			items = append(items, StageItem{ID: "volumes", State: "not checked", Message: "The cluster did not answer and no volume of the platform was recorded, so none " +
				"is deleted. Look on the boot store for vm-*-pvc-* disks of the platform's own components (etcd of Kamaji) and delete them by hand."})
		}
		if err := x.note(ctx, items...); err != nil {
			return err
		}
	}
	if err := x.disableProviders(ctx); err != nil {
		return err
	}
	// The workload answers: its volumes are read again, against the BOM that named its namespaces (the installed
	// one, else this release's). Otherwise the recorded ones stand.
	bom, _ := bomOf(x.in)
	if wl != nil && bom != nil {
		if vols, err := platformVolumes(ctx, bom, wl); err == nil && !slices.Equal(vols, x.in.Status.Volumes) {
			if err := x.record(ctx, func(_ *Run, in *Installation, _ *Secrets) { in.Status.Volumes = vols }); err != nil {
				return err
			}
		}
	}
	if app != nil || live {
		if err := x.check(ctx); err != nil {
			return err
		}
		// The Application first and without a cascade (it has no finalizer), so ArgoCD cannot build the cluster again.
		if err := seed.DeleteApplication(ctx, argoNamespace, ClusterName); err != nil {
			return classify(CodeSeedAPIUnreachable, err)
		}
		if live {
			if err := seed.DeleteCR(ctx, capiGroup, capiVersion, "clusters", ClusterName, ClusterName); err != nil {
				return classify(CodeSeedAPIUnreachable, err)
			}
		}
		if app != nil && cl == nil {
			// A sync of the Application that was running may have created the Cluster after the read above: the next
			// look reads it again, now that ArgoCD can build nothing more.
			return waiting(CodeMachineDeleteStuck, "The Application %s was deleted; the seed is read again for a cluster it may still have made.", ClusterName)
		}
	}
	machines, vmids, _, err := x.machines(ctx, seed)
	if err != nil {
		return err
	}
	if cl != nil || len(machines) > 0 {
		var left []string
		var items []StageItem
		for _, m := range machines {
			name := str(m, "metadata", "name")
			vm := vmText(vmids[name])
			// CAPMOX deletes the ProxmoxMachine before Cluster API lets go of the Machine: a VMID seen at an earlier look stays.
			if i := slices.IndexFunc(x.run.Stages[x.si].Items, func(c StageItem) bool { return c.ID == name && c.State == "deleting" }); vmids[name] == 0 && i >= 0 {
				vm = x.run.Stages[x.si].Items[i].Message
			}
			left = append(left, fmt.Sprintf("%s (%s)", name, vm))
			items = append(items, StageItem{ID: name, State: "deleting", Message: vm})
		}
		if err := x.note(ctx, items...); err != nil {
			return err
		}
		return waiting(CodeMachineDeleteStuck, "Cluster API is deleting %s; %d machines are left: %s", ClusterName, len(left), clip(strings.Join(left, ", ")))
	}
	// The machines this run saw are gone now: Cluster API deleted each with its VM, disk and bootstrap ISO.
	done := []StageItem{{ID: "cluster", State: "gone", Message: "The seed lists no cluster " + ClusterName + " and no machine of it."}}
	for _, it := range x.run.Stages[x.si].Items {
		if it.State == "deleting" {
			it.State = "deleted"
			done = append(done, it)
		}
	}
	if err := x.note(ctx, done...); err != nil {
		return err
	}
	return x.clusterRemoved(ctx)
}

// clusterRemoved deletes the recorded volumes and then takes the cluster and the volumes out of the status, so a
// Remove started again after the seed VM is deleted has nothing to ask of Cluster API.
func (x *acq) clusterRemoved(ctx context.Context) error {
	if err := x.deleteVolumes(ctx); err != nil {
		return err
	}
	return x.record(ctx, func(_ *Run, in *Installation, _ *Secrets) { in.Status.Clusters, in.Status.Volumes = nil, nil })
}

// tenantsGone refuses the remove while tenant Applications exist on the workload, and answers the workload's client
// (nil when the cluster has no API to ask). A workload that should answer and does not is retried, and passed only
// with the typed confirmation: the run cannot see whether tenants still run there. empty = it never could hold one.
func (x *acq) tenantsGone(ctx context.Context, seed *kamajik8s.Client, empty bool) (*kamajik8s.Client, error) {
	wl, err := x.workload(ctx, seed)
	if err != nil || wl == nil {
		return nil, err
	}
	apps, err := wl.ListApplications(ctx, argoNamespace, tenantSelector)
	switch {
	case kamajik8s.NotFound(err): // no ArgoCD on the workload yet: no provider ever created anything
		return wl, nil
	case err != nil && (x.run.Confirmed || empty):
		return nil, nil
	case err != nil:
		return nil, &stageErr{kindRetry, CodeWorkloadUnreachable, "The workload cluster does not answer whether tenant clusters or databases still run on it. " +
			"If it is beyond repair, start Remove with confirm \"remove " + x.in.Name + "\". " + clip(err.Error())}
	case len(apps) > 0:
		var names []string
		for _, a := range apps[:min(len(apps), 5)] {
			names = append(names, str(a, "metadata", "name"))
		}
		return nil, permanent(CodeTenantsExist, "%d tenant clusters or databases still run on the platform (%s): delete them first.", len(apps), strings.Join(names, ", "))
	}
	return wl, nil
}

func (x *acq) disableProviders(ctx context.Context) error {
	if err := x.check(ctx); err != nil {
		return err
	}
	return x.r.Disable(ctx, x.in.ID)
}

// deleteTagged is the seed-lost mode: it deletes the VMs of pool proxcloud-system that carry this installation's tag,
// and no other, with the CAPI token, each after its bootstrap ISO (user-data-<vmid>.iso, which only CAPMOX's own
// delete removes for a machine that never joined). CAPMOX tags a VM after its clone, so a machine that was still being
// cloned when the seed was lost has no tag: it is not deleted, and the stage's items name it.
func (x *acq) deleteTagged(ctx context.Context) error {
	pool, err := x.capi.GetPool(ctx, pve.SystemPool)
	if err != nil {
		return classify(CodeMachineDeleteStuck, err)
	}
	var items []StageItem
	for i := 0; pool != nil && i < len(pool.Members); i++ {
		m := pool.Members[i]
		if m.Type != "qemu" {
			continue
		}
		cfg, err := x.capi.GetVMConfig(ctx, m.Node, m.VMID)
		if err != nil {
			return classify(CodeMachineDeleteStuck, err)
		}
		if !slices.Contains(vmTags(cfg), "pc-platform-"+x.in.ID) {
			if strings.HasPrefix(cfg.Str("name"), ClusterName+"-") && !slices.ContainsFunc(vmTags(cfg), func(t string) bool { return strings.HasPrefix(t, "pc-platform-") }) {
				items = append(items, StageItem{ID: fmt.Sprintf("vm-%d", m.VMID), State: "left", Message: fmt.Sprintf("VM %d (%s) on %s is in pool %s and carries no "+
					"installation's tag: not deleted. If it is a machine of this platform that was still being cloned, delete it in Proxmox.", m.VMID, cfg.Str("name"), m.Node, pve.SystemPool)})
			}
			continue
		}
		st, err := x.capi.GetVMStatus(ctx, m.Node, m.VMID)
		if err != nil {
			return classify(CodeMachineDeleteStuck, err)
		}
		if st.Status == "running" {
			if err := x.pveTask(ctx, CodeMachineDeleteStuck, func() (pve.UPID, error) { return x.capi.StopVM(ctx, m.Node, m.VMID) }); err != nil {
				return err
			}
		}
		// The ISO before the VM: once the VM is gone nothing says whose ISO a user-data-<vmid>.iso is.
		iso, rel := x.in.Spec.Storage.ISO, fmt.Sprintf("iso/user-data-%d.iso", m.VMID)
		listed, err := x.capi.StorageContents(ctx, m.Node, iso, "iso", 0)
		if err != nil {
			return classify(CodeMachineDeleteStuck, err)
		}
		if slices.ContainsFunc(listed, func(c pve.StorageContent) bool { return c.VolID == iso+":"+rel }) {
			if err := x.check(ctx); err != nil {
				return err
			}
			if err := x.capi.DeleteStorageVolume(ctx, m.Node, iso, rel); err != nil {
				return classify(CodeMachineDeleteStuck, err)
			}
		}
		if err := x.pveTask(ctx, CodeMachineDeleteStuck, func() (pve.UPID, error) { return x.capi.DeleteVM(ctx, m.Node, m.VMID) }); err != nil {
			return err
		}
		if err := x.note(ctx, StageItem{ID: fmt.Sprintf("vm-%d", m.VMID), State: "deleted", Message: fmt.Sprintf("VM %d (%s) on %s, after its bootstrap ISO.", m.VMID, cfg.Str("name"), m.Node)}); err != nil {
			return err
		}
	}
	return x.note(ctx, items...)
}

// deleteVolumes deletes the recorded volumes with the CSI token, by their exact handles (region/node/storage/volume):
// names like vm-9999-pvc-* are shared with tenant volumes, so nothing is matched by pattern. Done when no store lists
// one of them any more.
func (x *acq) deleteVolumes(ctx context.Context) error {
	// noted: the stage has an item of that id; with "", any item about volumes.
	noted := func(id string) bool {
		return slices.ContainsFunc(x.run.Stages[x.si].Items, func(it StageItem) bool { return it.ID == id || (id == "" && strings.HasPrefix(it.ID, "volume")) })
	}
	if len(x.in.Status.Volumes) == 0 {
		if noted("") {
			return nil
		}
		return x.note(ctx, StageItem{ID: "volumes", State: "none recorded", Message: "No volume of the platform's own components was recorded (stage 9 records them), so none was deleted."})
	}
	csi, err := TokenClient(x.es, x.sec.CSI)
	if err != nil || csi == nil {
		return permanent(CodeCredentialsMissing, "The token %s is not stored or not usable, so the platform's %d volumes cannot be deleted: paste it, then Remove again.",
			pve.CSITokenID, len(x.in.Status.Volumes))
	}
	deleted := 0
	for _, v := range x.in.Status.Volumes {
		p := strings.SplitN(v.Handle, "/", 4)
		if len(p) != 4 {
			return permanent(CodeVolumeDeleteFailed, "The recorded volume handle %q is not region/node/storage/volume.", v.Handle)
		}
		node, storage, vol := p[1], p[2], p[3]
		listed, err := csi.StorageContents(ctx, node, storage, "images", 0)
		if err != nil {
			return classify(CodeVolumeDeleteFailed, err)
		}
		item := StageItem{ID: "volume-" + vol, State: "not found", Message: fmt.Sprintf("%s (%s, claim %s): the store no longer lists it.", v.Handle, v.Component, v.PVC)}
		if !slices.ContainsFunc(listed, func(c pve.StorageContent) bool { return c.VolID == storage+":"+vol }) {
			if !noted(item.ID) {
				if err := x.note(ctx, item); err != nil {
					return err
				}
			}
			continue
		}
		if err := x.check(ctx); err != nil {
			return err
		}
		if err := csi.DeleteStorageVolume(ctx, node, storage, vol); err != nil {
			return classify(CodeVolumeDeleteFailed, err)
		}
		item.State, item.Message = "deleted", fmt.Sprintf("%s (%s, claim %s)", v.Handle, v.Component, v.PVC)
		if err := x.note(ctx, item); err != nil {
			return err
		}
		deleted++
	}
	if deleted > 0 {
		return waiting(CodeVolumeDeleteFailed, "%d volumes were deleted; the stores are read again.", deleted)
	}
	return nil
}
