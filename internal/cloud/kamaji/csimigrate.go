package kamaji

// csimigrate.go is the management side of moving a cluster made with the tenant-side proxmox-csi controller (and its
// csi@pve!<id> token in the tenant) onto the CSI gateway: what its Application says about block volumes, its admin
// kubeconfig as Kamaji wrote it, and the switch itself. The project package drives it (MigrateClusterCSI).

import (
	"context"
	"fmt"
)

// ClusterCSI is what a cluster's Application says about its block volumes.
type ClusterCSI struct {
	ProjectID    string
	Region       string
	Pool         string
	ChartVersion string
	Classes      []StorageClass
	// Gateway: the controller already runs on the services cluster against the gateway.
	Gateway bool
}

// ClusterCSI reads the cluster's Application. A cluster this provider does not manage is an error.
func (s *Service) ClusterCSI(ctx context.Context, clusterID string) (*ClusterCSI, error) {
	app, err := s.api.GetApplication(ctx, s.cfg.ArgoNamespace, clusterID)
	if err != nil {
		return nil, err
	}
	if app == nil || !managedBy(app) || digStr(app, "metadata", "labels", LabelService) != s.serviceID {
		return nil, fmt.Errorf("kamaji: cluster %s not found", clusterID)
	}
	// A cluster being deleted: its release may have run already, so a credential or disk rows made now would outlive it.
	if dig(app, "metadata", "deletionTimestamp") != nil {
		return nil, fmt.Errorf("kamaji: cluster %s is being deleted", clusterID)
	}
	values, _ := dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
	out := &ClusterCSI{
		ProjectID:    digStr(app, "metadata", "labels", LabelProject),
		Region:       digStr(values, "proxmox", "region"),
		Pool:         digStr(values, "proxmox", "pool"),
		ChartVersion: digStr(app, "spec", "source", "targetRevision"),
		Gateway:      digStr(values, "proxmox", "csi", "gateway", "secretName") != "",
	}
	classes, _ := dig(values, "proxmox", "csi", "storageClasses").([]any)
	for _, c := range classes {
		m, _ := c.(map[string]any)
		if name, st := digStr(m, "name"), digStr(m, "storage"); name != "" && st != "" {
			out.Classes = append(out.Classes, StorageClass{Name: name, Storage: st})
		}
	}
	if out.ProjectID == "" {
		return nil, fmt.Errorf("kamaji: cluster %s: application has no project label", clusterID)
	}
	return out, nil
}

// TenantKubeconfig is the cluster's Kamaji admin kubeconfig as Kamaji wrote it: the control plane's LoadBalancer
// address, not the public name AdminKubeconfig gives customers. The portal reaches it over the services subnet.
func (s *Service) TenantKubeconfig(ctx context.Context, projectID, clusterID string) ([]byte, error) {
	ns := NamespaceFor(projectID)
	tcp, err := s.findTCP(ctx, ns, clusterID)
	if err != nil {
		return nil, err
	}
	if tcp == nil {
		return nil, fmt.Errorf("kamaji: cluster %s: control plane not found", clusterID)
	}
	data, err := s.api.GetSecretData(ctx, ns, adminKubeconfigSecret(tcp, clusterID))
	if err != nil {
		return nil, err
	}
	if b := data["admin.conf"]; len(b) > 0 {
		return b, nil
	}
	return nil, fmt.Errorf("kamaji: cluster %s: admin kubeconfig not ready", clusterID)
}

// ServicesPersistentVolumes lists the management (services) cluster's own PersistentVolumes: its proxmox-csi disks
// (Kamaji etcd, registry, DBaaS) share names and stores with the tenants'.
func (s *Service) ServicesPersistentVolumes(ctx context.Context) ([]map[string]any, error) {
	return s.api.ListPersistentVolumes(ctx)
}

// SwitchCSIGateway moves the cluster's proxmox-csi controller onto the services cluster in one Application apply: the
// controller's Secret (config with the cluster's gateway credential, and the CA) goes in first, the resource set loses
// the tenant-side config (resourceSet, the csi-proxmox namespace alone), then the chart is pinned to chartVersion with
// the gateway values. ArgoCD renders the services-side controller and CAAPH brings the tenant's to 0 replicas; both
// elect a leader on the same leases in the tenant's csi-proxmox, so they do not run side by side. The tenant's copy of
// the old config Secret stays: the resource set does not delete what it no longer carries.
func (s *Service) SwitchCSIGateway(ctx context.Context, projectID, clusterID, chartVersion string, controllerSecret map[string]string, resourceSet string) error {
	if !ChartHasGatewayCSI(chartVersion) {
		return fmt.Errorf("kamaji: chart %s has no services-side csi controller, %s or later is needed", chartVersion, GatewayChartVersion)
	}
	ns := NamespaceFor(projectID)
	labels := map[string]string{LabelProject: projectID, LabelService: s.serviceID, LabelManagedBy: ManagedByValue}
	if err := s.api.ApplySecret(ctx, ns, CSIGatewaySecretName(clusterID), controllerSecret, labels, nil); err != nil {
		return fmt.Errorf("kamaji: apply csi gateway secret: %w", err)
	}
	if err := s.api.ApplyResourceSetSecret(ctx, ns, CSISecretName(clusterID), map[string]string{"csi.yaml": resourceSet}, labels); err != nil {
		return fmt.Errorf("kamaji: apply csi secret: %w", err)
	}
	return s.PatchClusterApp(ctx, clusterID, func(app map[string]any) error {
		src, _ := dig(app, "spec", "source").(map[string]any)
		csi, _ := dig(app, "spec", "source", "helm", "valuesObject", "proxmox", "csi").(map[string]any)
		if src == nil || csi == nil {
			return fmt.Errorf("kamaji: cluster %s: application carries no csi values", clusterID)
		}
		src["targetRevision"] = chartVersion
		csi["gateway"] = map[string]any{
			"secretName":           CSIGatewaySecretName(clusterID),
			"kubeconfigSecretName": AdminKubeconfigSecretName(clusterID),
		}
		return nil
	})
}
