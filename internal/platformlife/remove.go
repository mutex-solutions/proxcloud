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
			return nil // an earlier Remove finished this part and then deleted the seed VM: nothing is left to delete
		}
		if !x.run.Confirmed {
			return permanent(CodeSeedLost, "The seed VM is gone, so Cluster API cannot delete the workload cluster. Start Remove with confirm \"remove %s\" "+
				"to delete the VMs of pool %s tagged pc-platform-%s and the %d recorded volumes.", x.in.Name, pve.SystemPool, x.in.ID, len(x.in.Status.Volumes))
		}
		if err := x.disableProviders(ctx); err != nil {
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
		if wl, err = x.tenantsGone(ctx, seed); err != nil {
			return err
		}
	}
	if err := x.disableProviders(ctx); err != nil {
		return err
	}
	// The workload answers: its volumes are read again, against the BOM that named its namespaces (the installed
	// one, else this release's). Otherwise the recorded ones stand.
	var bom *BOM
	if cur, err := Current(); err == nil {
		bom = cur.BOM
	}
	if ref := x.in.Status.BOM; ref != nil {
		if b, ok := embeddedBOM(ref.SHA256); ok {
			bom = b
		}
	}
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
	}
	machines, vmids, _, err := x.machines(ctx, seed)
	if err != nil {
		return err
	}
	if cl != nil || len(machines) > 0 {
		var left []string
		for _, m := range machines {
			left = append(left, fmt.Sprintf("%s (VM %d)", str(m, "metadata", "name"), vmids[str(m, "metadata", "name")]))
		}
		return waiting(CodeMachineDeleteStuck, "Cluster API is deleting %s; %d machines are left: %s", ClusterName, len(left), clip(strings.Join(left, ", ")))
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
// with the typed confirmation: the run cannot see whether tenants still run there.
func (x *acq) tenantsGone(ctx context.Context, seed *kamajik8s.Client) (*kamajik8s.Client, error) {
	wl, err := x.workload(ctx, seed)
	if err != nil || wl == nil {
		return nil, err
	}
	apps, err := wl.ListApplications(ctx, argoNamespace, tenantSelector)
	switch {
	case kamajik8s.NotFound(err): // no ArgoCD on the workload yet: no provider ever created anything
		return wl, nil
	case err != nil && x.run.Confirmed:
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
// and no other, with the CAPI token.
func (x *acq) deleteTagged(ctx context.Context) error {
	pool, err := x.capi.GetPool(ctx, pve.SystemPool)
	if err != nil {
		return classify(CodeMachineDeleteStuck, err)
	}
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
		if err := x.pveTask(ctx, CodeMachineDeleteStuck, func() (pve.UPID, error) { return x.capi.DeleteVM(ctx, m.Node, m.VMID) }); err != nil {
			return err
		}
	}
	return nil
}

// deleteVolumes deletes the recorded volumes with the CSI token, by their exact handles (region/node/storage/volume):
// names like vm-9999-pvc-* are shared with tenant volumes, so nothing is matched by pattern. Done when no store lists
// one of them any more.
func (x *acq) deleteVolumes(ctx context.Context) error {
	if len(x.in.Status.Volumes) == 0 {
		return nil
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
		if !slices.ContainsFunc(listed, func(c pve.StorageContent) bool { return c.VolID == storage+":"+vol }) {
			continue
		}
		if err := x.check(ctx); err != nil {
			return err
		}
		if err := csi.DeleteStorageVolume(ctx, node, storage, vol); err != nil {
			return classify(CodeVolumeDeleteFailed, err)
		}
		deleted++
	}
	if deleted > 0 {
		return waiting(CodeVolumeDeleteFailed, "%d volumes were deleted; the stores are read again.", deleted)
	}
	return nil
}
