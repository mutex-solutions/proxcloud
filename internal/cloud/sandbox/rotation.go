package sandbox

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

const WorkerRoleLabel = "node-restriction.kubernetes.io/proxcloud-sandbox-worker"

// RotationPolicy names an exact management namespace/cluster and allowlisted MDs.
// Default off. Wait-only: the controller NEVER evicts, deletes or pauses a pod.
type RotationPolicy struct {
	Enabled            bool     `json:"enabled"`
	IntervalHours      int      `json:"intervalHours"`
	Namespace          string   `json:"namespace"`
	Cluster            string   `json:"cluster"`
	MachineDeployments []string `json:"machineDeployments"`
	Kubeconfig         string   `json:"-"`
}

func (p RotationPolicy) Validate() error {
	if !p.Enabled {
		return nil
	}
	if p.IntervalHours < 24 || p.IntervalHours > 8760 || !validName(p.Namespace) || !validName(p.Cluster) || len(p.MachineDeployments) == 0 {
		return fmt.Errorf("sandbox.rotation: exact namespace, cluster, deployments and intervalHours 24-8760 required")
	}
	for _, md := range p.MachineDeployments {
		if !validName(md) {
			return fmt.Errorf("sandbox.rotation: invalid deployment name")
		}
	}
	return nil
}

type RotationPlan struct {
	Ready       bool     `json:"ready"`
	Reasons     []string `json:"reasons"`
	Nodes       []string `json:"nodes"`
	Deployments []string `json:"deployments"`
}

// RotationInspect is read-only, including when rotation is disabled. It proves
// positive ownership from CAPI Machines AND NodeRestriction-protected Node labels.
func (s *Service) RotationInspect(ctx context.Context) (RotationPlan, error) {
	return s.rotationInspect(ctx, false)
}
func (s *Service) RotationRecheck(ctx context.Context) (RotationPlan, error) {
	return s.rotationInspect(ctx, true)
}
func (s *Service) rotationInspect(ctx context.Context, allowCordoned bool) (RotationPlan, error) {
	p := s.cfg.Rotation
	r := RotationPlan{Reasons: []string{}, Nodes: []string{}, Deployments: []string{}}
	if err := p.Validate(); err != nil {
		return r, err
	}
	if p.Namespace == "" || p.Cluster == "" || len(p.MachineDeployments) == 0 {
		r.Reasons = append(r.Reasons, "rotation scope is not configured")
		return r, nil
	}
	if p.Kubeconfig == "" {
		r.Reasons = append(r.Reasons, "separate management rotation credential is not configured")
		return r, nil
	}
	mgmt, err := kamajik8s.New(p.Kubeconfig)
	if err != nil {
		return r, fmt.Errorf("rotation management client invalid")
	}
	api, ok := s.api.(interface {
		ListNodes(context.Context) ([]map[string]any, error)
	})
	if !ok {
		return r, fmt.Errorf("rotation node reader unavailable")
	}
	nodes, err := api.ListNodes(ctx)
	if err != nil {
		return r, err
	}
	byNode := map[string]map[string]any{}
	for _, n := range nodes {
		byNode[digStr(n, "metadata", "name")] = n
	}
	for _, name := range p.MachineDeployments {
		md, err := mgmt.GetCR(ctx, "cluster.x-k8s.io", "v1beta2", "machinedeployments", p.Namespace, name)
		if err != nil {
			return r, err
		}
		if md == nil || digStr(md, "spec", "clusterName") != p.Cluster || digStr(md, "metadata", "labels", "cluster.x-k8s.io/cluster-name") != p.Cluster || digStr(md, "spec", "template", "metadata", "labels", WorkerRoleLabel) != "true" || digStr(md, "spec", "template", "metadata", "labels", "node-restriction.kubernetes.io/proxcloud-system") == "true" {
			r.Reasons = append(r.Reasons, "deployment "+name+" lacks the protected sandbox-worker role or cluster ownership")
			continue
		}
		r.Deployments = append(r.Deployments, name)
		machines, err := mgmt.ListCRs(ctx, "cluster.x-k8s.io", "v1beta2", "machines", p.Namespace, "cluster.x-k8s.io/deployment-name="+name+",cluster.x-k8s.io/cluster-name="+p.Cluster)
		if err != nil {
			return r, err
		}
		if len(machines) == 0 {
			r.Reasons = append(r.Reasons, "deployment "+name+" has no Machines")
		}
		for _, m := range machines {
			node := digStr(m, "status", "nodeRef", "name")
			n := byNode[node]
			if node == "" || n == nil || digStr(m, "status", "nodeRef", "uid") == "" || digStr(m, "status", "nodeRef", "uid") != digStr(n, "metadata", "uid") || digStr(m, "spec", "clusterName") != p.Cluster || digStr(m, "metadata", "labels", WorkerRoleLabel) != "true" || digStr(n, "metadata", "labels", WorkerRoleLabel) != "true" || digStr(n, "metadata", "labels", "node-restriction.kubernetes.io/proxcloud-system") == "true" {
				r.Reasons = append(r.Reasons, "Machine of "+name+" has no protected worker Node")
				continue
			}
			if !allowCordoned && dig(n, "spec", "unschedulable") == true {
				r.Reasons = append(r.Reasons, "worker is already cordoned; operator must resolve it before rotation")
			}
			if dig(n, "metadata", "deletionTimestamp") != nil || dig(m, "metadata", "deletionTimestamp") != nil {
				r.Reasons = append(r.Reasons, "replacement already in progress")
			}
			if !slices.Contains(r.Nodes, node) {
				r.Nodes = append(r.Nodes, node)
			}
		}
	}
	pods, err := s.api.ListPodsAllNamespaces(ctx, "")
	if err != nil {
		return r, err
	}
	for _, pod := range pods {
		if digStr(pod, "spec", "nodeName") == "" && digStr(pod, "metadata", "namespace") == BuildNamespace && digStr(pod, "status", "phase") != "Succeeded" && digStr(pod, "status", "phase") != "Failed" {
			r.Reasons = append(r.Reasons, "pending build/snapshot pod must finish before rotation")
		}
		if !slices.Contains(r.Nodes, digStr(pod, "spec", "nodeName")) {
			continue
		}
		phase := digStr(pod, "status", "phase")
		if phase == "Succeeded" || phase == "Failed" {
			continue
		}
		owners, _ := dig(pod, "metadata", "ownerReferences").([]any)
		daemon := false
		for _, o := range owners {
			m, _ := o.(map[string]any)
			if str(m["kind"]) == "DaemonSet" {
				daemon = true
			}
		}
		if !daemon {
			r.Reasons = append(r.Reasons, "worker holds a non-DaemonSet pod (includes sandboxes, builds and snapshot assembly)")
		}
	}
	// Pending CRs could schedule as soon as a replacement joins. Block the whole
	// provider, not just Running pods, so none can be killed by an old-gen deletion.
	crs, err := s.api.ListCRsAllNamespaces(ctx, CRGroup, CRVersion, CRPlural, "")
	if err != nil {
		return r, err
	}
	if len(crs) > 0 {
		r.Reasons = append(r.Reasons, "sandbox cluster still has Sandbox CRs; wait for them to finish")
	}
	if !p.Enabled {
		r.Reasons = append(r.Reasons, "rotation is disabled (default)")
	}
	slices.Sort(r.Nodes)
	slices.Sort(r.Deployments)
	r.Ready = len(r.Reasons) == 0 && len(r.Nodes) > 0 && len(r.Deployments) == len(p.MachineDeployments)
	return r, nil
}
func (s *Service) CordonRotationNodes(ctx context.Context, names []string, value bool) error {
	api, ok := s.api.(interface {
		SetNodeUnschedulable(context.Context, string, bool) error
	})
	if !ok {
		return fmt.Errorf("rotation cordon client unavailable")
	}
	var changed []string
	for _, name := range names {
		if err := api.SetNodeUnschedulable(ctx, name, value); err != nil {
			if value {
				for _, done := range changed {
					_ = api.SetNodeUnschedulable(context.WithoutCancel(ctx), done, false)
				}
			}
			return err
		}
		changed = append(changed, name)
	}
	return nil
}

// StartRotation uses only beta2 rollout.after, not delete/eviction/drain/force flags.
func (s *Service) StartRotation(ctx context.Context, at time.Time) error {
	p := s.cfg.Rotation
	mgmt, err := kamajik8s.New(p.Kubeconfig)
	if err != nil {
		return err
	}
	for _, name := range p.MachineDeployments {
		md, e := mgmt.GetCR(ctx, "cluster.x-k8s.io", "v1beta2", "machinedeployments", p.Namespace, name)
		if e != nil {
			return e
		}
		if md == nil || digStr(md, "spec", "clusterName") != p.Cluster || digStr(md, "spec", "template", "metadata", "labels", WorkerRoleLabel) != "true" || digStr(md, "spec", "template", "metadata", "labels", "node-restriction.kubernetes.io/proxcloud-system") == "true" {
			return fmt.Errorf("rotation deployment identity changed")
		}
		if _, err := mgmt.MergePatchCR(ctx, "cluster.x-k8s.io", "v1beta2", "machinedeployments", p.Namespace, name, map[string]any{"spec": map[string]any{"rollout": map[string]any{"after": at.UTC().Format(time.RFC3339)}}}); err != nil {
			return fmt.Errorf("rotation patch failed: %w", err)
		}
	}
	return nil
}
func (s *Service) RotationConverged(ctx context.Context, old []string) (bool, error) {
	api, ok := s.api.(interface {
		ListNodes(context.Context) ([]map[string]any, error)
	})
	if !ok {
		return false, fmt.Errorf("node reader unavailable")
	}
	nodes, err := api.ListNodes(ctx)
	if err != nil {
		return false, err
	}
	for _, n := range nodes {
		if slices.Contains(old, digStr(n, "metadata", "name")) {
			return false, nil
		}
	}
	p := s.cfg.Rotation
	mgmt, err := kamajik8s.New(p.Kubeconfig)
	if err != nil {
		return false, err
	}
	for _, name := range p.MachineDeployments {
		md, err := mgmt.GetCR(ctx, "cluster.x-k8s.io", "v1beta2", "machinedeployments", p.Namespace, name)
		if err != nil {
			return false, err
		}
		if md == nil {
			return false, nil
		}
		want := intOf(dig(md, "spec", "replicas"))
		if want < 1 || intOf(dig(md, "metadata", "generation")) < 1 || intOf(dig(md, "status", "observedGeneration")) != intOf(dig(md, "metadata", "generation")) {
			return false, nil
		}
		for _, key := range []string{"replicas", "readyReplicas", "availableReplicas", "upToDateReplicas"} {
			if intOf(dig(md, "status", key)) != want {
				return false, nil
			}
		}
		machines, err := mgmt.ListCRs(ctx, "cluster.x-k8s.io", "v1beta2", "machines", p.Namespace, "cluster.x-k8s.io/deployment-name="+name+",cluster.x-k8s.io/cluster-name="+p.Cluster)
		if err != nil {
			return false, err
		}
		if len(machines) != want {
			return false, nil
		}
		for _, m := range machines {
			found := false
			for _, n := range nodes {
				if digStr(n, "metadata", "name") != digStr(m, "status", "nodeRef", "name") {
					continue
				}
				if digStr(n, "metadata", "labels", WorkerRoleLabel) != "true" || dig(n, "spec", "unschedulable") == true || dig(n, "metadata", "deletionTimestamp") != nil || !nodeReady(n) {
					return false, nil
				}
				taints, _ := dig(n, "spec", "taints").([]any)
				for _, v := range taints {
					tm, _ := v.(map[string]any)
					if str(tm["key"]) == "proxcloud.io/unsecured" {
						return false, nil
					}
				}
				found = true
			}
			if !found {
				return false, nil
			}
		}
	}
	if err := s.CheckNetworkGuard(ctx); err != nil {
		return false, err
	}
	return true, nil
}
func nodeReady(n map[string]any) bool {
	cs, _ := dig(n, "status", "conditions").([]any)
	for _, v := range cs {
		c, _ := v.(map[string]any)
		if c["type"] == "Ready" && c["status"] == "True" {
			return true
		}
	}
	return false
}
