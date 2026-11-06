package kamaji

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

// K8sAPI is the management-cluster surface Service consumes — implemented by *kamajik8s.Client,
// fake-able in tests.
type K8sAPI interface {
	EnsureNamespace(ctx context.Context, name string, labels map[string]string) error
	GetNamespace(ctx context.Context, name string) (map[string]any, error)
	DeleteNamespace(ctx context.Context, name string) error
	ApplySecret(ctx context.Context, ns, name string, stringData map[string]string, labels, annotations map[string]string) error
	ApplyResourceSetSecret(ctx context.Context, ns, name string, stringData, labels map[string]string) error
	GetSecretData(ctx context.Context, ns, name string) (map[string][]byte, error)
	ListSecrets(ctx context.Context, ns, labelSelector string) ([]map[string]any, error)
	ListSecretsAllNamespaces(ctx context.Context, labelSelector string) ([]map[string]any, error)
	AnnotateSecret(ctx context.Context, ns, name string, annotations map[string]string) error
	DeleteSecret(ctx context.Context, ns, name string) error
	ApplyApplication(ctx context.Context, app map[string]any) error
	GetApplication(ctx context.Context, ns, name string) (map[string]any, error)
	ListApplications(ctx context.Context, ns, labelSelector string) ([]map[string]any, error)
	DeleteApplication(ctx context.Context, ns, name string) error
	GetTenantControlPlane(ctx context.Context, ns, name string) (map[string]any, error)
	ListTenantControlPlanes(ctx context.Context, ns, labelSelector string) ([]map[string]any, error)
	ListMachineDeployments(ctx context.Context, ns, labelSelector string) ([]map[string]any, error)
}

// defaultFinalizeGrace: a cloud credentials secret younger than this is never treated as an orphan.
// This closes the create-race: CreateCluster applies the secret BEFORE the Application, so a
// concurrent finalize pass could otherwise see a fresh secret with no Application — the exact
// signature of a finished delete cascade — and destroy a cluster mid-create.
const defaultFinalizeGrace = 30 * time.Minute

// Service drives one kamaji provider (one management cluster). Built per external service via
// New (live) or NewWithAPI (tests).
type Service struct {
	api           K8sAPI
	cfg           Config
	serviceID     string
	finalizeGrace time.Duration
	onClusterGone func(ctx context.Context, computeServiceID, projectID, clusterID string) error
}

// OnClusterGone registers what must outlive the delete cascade and run after it: revoking the
// cluster's PVE tokens and freeing its addresses. CAPMOX deletes the workers with that very token,
// so the delete path cannot do this itself. An error keeps the secret, so the next sweep retries.
func (s *Service) OnClusterGone(f func(ctx context.Context, computeServiceID, projectID, clusterID string) error) {
	s.onClusterGone = f
}

// ClusterSecrets is what a cluster runs under, written next to its Application and never into it.
type ClusterSecrets struct {
	// Cloud is the CAPMOX credential: url, token, secret.
	Cloud map[string]string
	// CSIManifest is the tenant-side proxmox-csi config, delivered by a ClusterResourceSet.
	CSIManifest string
	// ComputeServiceID names the proxmox provider hosting the workers; the reaper needs it.
	ComputeServiceID string
}

// annotationComputeService records ClusterSecrets.ComputeServiceID on the cloud secret.
const annotationComputeService = "proxcloud.io/compute-service"

// New builds a live Service from the provider config.
func New(cfg Config, serviceID string) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	kc, err := kamajik8s.New(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kamaji: management kubeconfig: %w", err)
	}
	return &Service{api: kc, cfg: cfg, serviceID: serviceID, finalizeGrace: defaultFinalizeGrace}, nil
}

// NewWithAPI builds a Service over a fake API (tests).
func NewWithAPI(api K8sAPI, cfg Config, serviceID string) *Service {
	return &Service{api: api, cfg: cfg, serviceID: serviceID, finalizeGrace: defaultFinalizeGrace}
}

// Config exposes the provider config (read-only use: version catalog, chart pin).
func (s *Service) Config() Config { return s.cfg }

// EnsureProjectNamespace creates/labels the project's namespace on the management cluster —
// the kamaji bootstrap leg (BootstrapKamajiOnto).
func (s *Service) EnsureProjectNamespace(ctx context.Context, projectID string) error {
	return s.api.EnsureNamespace(ctx, NamespaceFor(projectID), map[string]string{
		LabelProject:   projectID,
		LabelService:   s.serviceID,
		LabelManagedBy: ManagedByValue,
	})
}

// CreateCluster provisions one cluster: cloud credentials secret (mgmt-side only) + Application
// CR. Returns the initial cached data payload (status PENDING until the first sync reads real
// health).
func (s *Service) CreateCluster(ctx context.Context, spec ClusterSpec, sec ClusterSecrets) (map[string]any, error) {
	if err := spec.Validate(s.cfg.Defaults); err != nil {
		return nil, err
	}
	ns := NamespaceFor(spec.ProjectID)
	if err := s.EnsureProjectNamespace(ctx, spec.ProjectID); err != nil {
		return nil, fmt.Errorf("kamaji: ensure namespace: %w", err)
	}
	labels := map[string]string{LabelProject: spec.ProjectID, LabelService: s.serviceID, LabelManagedBy: ManagedByValue}
	if err := s.api.ApplySecret(ctx, ns, CloudSecretName(spec.ID), sec.Cloud, labels,
		map[string]string{annotationComputeService: sec.ComputeServiceID}); err != nil {
		return nil, fmt.Errorf("kamaji: apply cloud credentials secret: %w", err)
	}
	if err := s.api.ApplyResourceSetSecret(ctx, ns, CSISecretName(spec.ID),
		map[string]string{"csi.yaml": sec.CSIManifest}, labels); err != nil {
		return nil, fmt.Errorf("kamaji: apply csi secret: %w", err)
	}
	values := BuildValues(s.cfg, spec)
	app := BuildApplication(s.cfg, spec, s.serviceID, s.cfg.ChartVersion, values)
	if err := s.api.ApplyApplication(ctx, app); err != nil {
		return nil, fmt.Errorf("kamaji: apply application: %w", err)
	}
	return clusterData(app, nil, nil), nil
}

// DeleteCluster removes the cluster: Application delete only (the resources-finalizer cascades
// the rendered chart). The cloud credentials secret deliberately STAYS — the infrastructure
// provider authenticates with it to delete the worker VMs during the cascade, so removing it here
// would strand those cloud resources with stuck finalizers. FinalizeOrphans (sync-driven) reaps
// the secret and GCs the namespace once the cascade has finished.
// Idempotent (absent objects are success). Ownership-guarded: an Application without the
// managed-by marker is NOT ours (a pre-proxcloud cluster or foreign app that happens to share the
// name) — refuse rather than cascade-delete it.
func (s *Service) DeleteCluster(ctx context.Context, projectID, clusterID string) error {
	app, err := s.api.GetApplication(ctx, s.cfg.ArgoNamespace, clusterID)
	if err != nil {
		return err
	}
	if app != nil {
		if !managedBy(app) {
			return fmt.Errorf("kamaji: cluster %s is not managed by proxcloud — refusing to delete", clusterID)
		}
		if err := s.api.DeleteApplication(ctx, s.cfg.ArgoNamespace, clusterID); err != nil {
			return fmt.Errorf("kamaji: delete application: %w", err)
		}
	}
	return nil
}

// FinalizeOrphans completes asynchronous cluster deletions for ONE project (teardown + tests);
// the periodic reaper is FinalizeAllOrphans. Returns how many secrets still await the cascade.
func (s *Service) FinalizeOrphans(ctx context.Context, projectID string) (int, error) {
	ns := NamespaceFor(projectID)
	sel := LabelProject + "=" + projectID + "," + LabelManagedBy + "=" + ManagedByValue
	secrets, err := s.api.ListSecrets(ctx, ns, sel)
	if err != nil {
		return 0, err
	}
	return s.finalizeNamespace(ctx, ns, projectID, secrets)
}

// FinalizeAllOrphans is the service-level orphan sweep, run once per sync cycle: it scans EVERY
// managed cloud credentials secret of this provider across all namespaces — so leftovers of
// projects whose proxcloud doc is already gone (scheduled deletion, teardown) are still reaped —
// deletes finished-cascade secrets and GCs emptied project namespaces. Returns the number of
// secrets still awaiting their cascade.
func (s *Service) FinalizeAllOrphans(ctx context.Context) (int, error) {
	sel := LabelService + "=" + s.serviceID + "," + LabelManagedBy + "=" + ManagedByValue
	secrets, err := s.api.ListSecretsAllNamespaces(ctx, sel)
	if err != nil {
		return 0, err
	}
	byNS := map[string][]map[string]any{}
	for _, sec := range secrets {
		if ns := digStr(sec, "metadata", "namespace"); ns != "" {
			byNS[ns] = append(byNS[ns], sec)
		}
	}
	pending := 0
	var errs []error
	for ns, secs := range byNS {
		projectID := digStr(secs[0], "metadata", "labels", LabelProject)
		p, err := s.finalizeNamespace(ctx, ns, projectID, secs)
		pending += p
		if err != nil {
			errs = append(errs, err)
		}
	}
	return pending, errors.Join(errs...)
}

// finalizeNamespace reaps finished-cascade cluster secrets in one namespace. A secret is an
// orphan only when ALL of: it is older than the finalize grace window (create-race guard — the
// secret is applied before the Application), a point-in-time GetApplication finds no
// Application (never a stale list), and the TCP + MachineDeployments are gone (the cascade
// authenticates with this secret; reaping early would strand cloud resources). The namespace is
// GC'd only on a pass that actually reaped something and verified nothing lives there anymore —
// an idle project's bootstrap namespace is never touched.
func (s *Service) finalizeNamespace(ctx context.Context, ns, projectID string, secrets []map[string]any) (int, error) {
	var errs []error
	pending, reaped := 0, 0
	for _, sec := range secrets {
		name := digStr(sec, "metadata", "name")
		cid, isCloud := strings.CutSuffix(name, cloudSecretSuffix)
		if !isCloud {
			continue
		}
		if created, err := time.Parse(time.RFC3339, digStr(sec, "metadata", "creationTimestamp")); err == nil {
			if time.Since(created) < s.finalizeGrace {
				// Possibly mid-create — the Application may not be applied yet. Counts as
				// pending: teardown reads pending as "the cascade may still need the tenant".
				pending++
				continue
			}
		}
		app, err := s.api.GetApplication(ctx, s.cfg.ArgoNamespace, cid)
		if err != nil {
			return pending, err
		}
		if app != nil {
			if dig(app, "metadata", "deletionTimestamp") != nil {
				pending++ // delete cascade in flight
			}
			continue // cluster alive (or deleting) — its secret is in use
		}
		tcp, err := s.findTCP(ctx, ns, cid)
		if err != nil {
			return pending, err
		}
		mds, err := s.api.ListMachineDeployments(ctx, ns, "cluster.x-k8s.io/cluster-name="+cid)
		if err != nil {
			return pending, err
		}
		if tcp != nil || len(mds) > 0 {
			pending++
			continue
		}
		if s.onClusterGone != nil {
			if err := s.onClusterGone(ctx, digStr(sec, "metadata", "annotations", annotationComputeService), projectID, cid); err != nil {
				errs = append(errs, fmt.Errorf("cluster %s: release infrastructure: %w", cid, err))
				pending++
				continue
			}
		}
		if err := s.api.DeleteSecret(ctx, ns, CSISecretName(cid)); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s: delete csi secret: %w", cid, err))
			pending++
			continue
		}
		if err := s.api.DeleteSecret(ctx, ns, name); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s: delete cloud secret: %w", cid, err))
			pending++
			continue
		}
		reaped++
	}
	if reaped > 0 && pending == 0 {
		// Fresh look (never the caller's snapshot): any Application for this project means the
		// namespace is still in use.
		apps, err := s.api.ListApplications(ctx, s.cfg.ArgoNamespace,
			LabelProject+"="+projectID+","+LabelManagedBy+"="+ManagedByValue)
		if err != nil {
			errs = append(errs, err)
		} else if len(apps) == 0 {
			if err := s.gcNamespace(ctx, ns); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return pending, errors.Join(errs...)
}

// gcNamespace deletes the project namespace once it demonstrably holds no cluster remnants —
// and only if proxcloud created it (ownership label on the namespace).
func (s *Service) gcNamespace(ctx context.Context, ns string) error {
	nsObj, err := s.api.GetNamespace(ctx, ns)
	if err != nil || nsObj == nil {
		return err
	}
	if !managedBy(nsObj) {
		return nil
	}
	tcps, err := s.api.ListTenantControlPlanes(ctx, ns, "")
	if err != nil {
		return err
	}
	mds, err := s.api.ListMachineDeployments(ctx, ns, "")
	if err != nil {
		return err
	}
	if len(tcps) > 0 || len(mds) > 0 {
		return nil
	}
	return s.api.DeleteNamespace(ctx, ns)
}

// AdminKubeconfig fetches the cluster's admin kubeconfig from the Kamaji-generated secret —
// read-on-demand, streamed to the caller, NEVER stored in proxcloud (plan D5).
func (s *Service) AdminKubeconfig(ctx context.Context, projectID, clusterID string) ([]byte, error) {
	ns := NamespaceFor(projectID)
	tcp, err := s.findTCP(ctx, ns, clusterID)
	if err != nil {
		return nil, err
	}
	if tcp == nil {
		return nil, fmt.Errorf("kamaji: cluster %s: control plane not found (still provisioning?)", clusterID)
	}
	data, err := s.api.GetSecretData(ctx, ns, adminKubeconfigSecret(tcp, clusterID))
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("kamaji: cluster %s: admin kubeconfig not ready", clusterID)
	}
	// The secret holds the kubeconfig under "admin.conf" (kamaji convention); fall back to the
	// single value if the key differs across kamaji versions.
	fqdn := s.cfg.Defaults.ClusterFQDN(clusterID)
	if b, ok := data["admin.conf"]; ok {
		return publicKubeconfig(b, fqdn), nil
	}
	for _, b := range data {
		return publicKubeconfig(b, fqdn), nil
	}
	return nil, fmt.Errorf("kamaji: cluster %s: admin kubeconfig secret is empty", clusterID)
}

// publicKubeconfig rewrites every cluster server in the DOWNLOADED kubeconfig onto the
// cluster's public FQDN (the external-dns name of the API-server LoadBalancer). The Kamaji
// secret's own kubeconfig points at the LB address on the management network — nodes and
// in-cluster components keep using that; the customer operates from outside, over the DNS name
// the apiserver cert already carries as a SAN (BuildValues certSANs). The port is preserved.
// No DNS zone configured, or anything unparseable → the original bytes, unchanged.
func publicKubeconfig(kc []byte, fqdn string) []byte {
	if fqdn == "" {
		return kc
	}
	var doc map[string]any
	if err := yaml.Unmarshal(kc, &doc); err != nil {
		return kc
	}
	clusters, _ := doc["clusters"].([]any)
	changed := false
	for _, raw := range clusters {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		cl, ok := entry["cluster"].(map[string]any)
		if !ok {
			continue
		}
		server, _ := cl["server"].(string)
		u, err := url.Parse(server)
		if err != nil || u.Host == "" {
			continue
		}
		port := u.Port()
		if port == "" {
			port = "6443"
		}
		u.Host = net.JoinHostPort(fqdn, port)
		cl["server"] = u.String()
		changed = true
	}
	if !changed {
		return kc
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return kc
	}
	return out
}

// RotateKubeconfig triggers a Kamaji certificate rotation for the cluster's admin kubeconfig:
// annotating the secret with `certs.kamaji.clastix.io/rotate` makes the CertificateLifecycle
// controller regenerate it and roll the control plane
// (https://kamaji.clastix.io/guides/certs-lifecycle/). Every kubeconfig already handed out stops
// working — the caller is expected to have confirmed that with the user.
func (s *Service) RotateKubeconfig(ctx context.Context, projectID, clusterID string) error {
	ns := NamespaceFor(projectID)
	tcp, err := s.findTCP(ctx, ns, clusterID)
	if err != nil {
		return err
	}
	if tcp == nil {
		return fmt.Errorf("kamaji: cluster %s: control plane not found (still provisioning?)", clusterID)
	}
	return s.api.AnnotateSecret(ctx, ns, adminKubeconfigSecret(tcp, clusterID),
		map[string]string{"certs.kamaji.clastix.io/rotate": ""})
}

// adminKubeconfigSecret is the Kamaji admin-kubeconfig secret for a control plane: `<tcp>-admin-
// kubeconfig` (https://kamaji.clastix.io/concepts/tenant-control-plane/). Prefers the TCP's own
// name so a hand-migrated cluster still resolves; falls back to the chart's derivation.
func adminKubeconfigSecret(tcp map[string]any, clusterID string) string {
	if name, _ := dig(tcp, "metadata", "name").(string); name != "" {
		return name + "-admin-kubeconfig"
	}
	return AdminKubeconfigSecretName(clusterID)
}

// PatchClusterApp mutates the cluster's Application (read → guard ownership → mutate → full
// re-apply). The one reconcile path every post-create change goes through (plan §9).
func (s *Service) PatchClusterApp(ctx context.Context, clusterID string, mutate func(app map[string]any) error) error {
	app, err := s.api.GetApplication(ctx, s.cfg.ArgoNamespace, clusterID)
	if err != nil {
		return err
	}
	if app == nil {
		return fmt.Errorf("kamaji: cluster %s not found", clusterID)
	}
	if !managedBy(app) {
		return fmt.Errorf("kamaji: cluster %s is not managed by proxcloud — refusing to modify", clusterID)
	}
	if err := mutate(app); err != nil {
		return err
	}
	// Re-apply the WHOLE spec plus the metadata proxcloud stamped at create — NOT a partial patch.
	// The Application was created via server-side apply by this same field manager, so a partial
	// apply is an ownership retraction: every owned field absent from the patch is REMOVED, and
	// the api server then rejects the result with 422 "spec.destination/spec.project: Required
	// value" (or silently drops the resources-finalizer and the display-name annotation). The
	// mutated values are already inside app's spec (mutate edited them in place).
	meta := map[string]any{
		"name":      clusterID,
		"namespace": s.cfg.ArgoNamespace,
	}
	for _, k := range []string{"labels", "annotations", "finalizers"} {
		if v := dig(app, "metadata", k); v != nil {
			meta[k] = v
		}
	}
	patch := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   meta,
		"spec":       app["spec"],
	}
	return s.api.ApplyApplication(ctx, patch)
}

// PatchClusterValues mutates the Application's helm values in place (UPGRADE, SET_NODE_GROUPS…):
// same chart pin, same full re-apply.
func (s *Service) PatchClusterValues(ctx context.Context, clusterID string, mutate func(values map[string]any) error) error {
	return s.PatchClusterApp(ctx, clusterID, func(app map[string]any) error {
		values, _ := dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
		if values == nil {
			return fmt.Errorf("kamaji: cluster %s: application carries no values", clusterID)
		}
		return mutate(values)
	})
}

// SetChartVersion re-pins the cluster's Application onto a chart version (the "platform
// update"). ArgoCD re-renders the SAME desired state with the new chart — which may roll the
// CCM or, when the machine-template output changes between chart versions, the nodes
// (surge-first per the chart's rollout strategy).
func (s *Service) SetChartVersion(ctx context.Context, clusterID, version string) error {
	if version == "" {
		return fmt.Errorf("kamaji: chart version is required")
	}
	return s.PatchClusterApp(ctx, clusterID, func(app map[string]any) error {
		src, _ := dig(app, "spec", "source").(map[string]any)
		if src == nil {
			return fmt.Errorf("kamaji: cluster %s: application carries no source", clusterID)
		}
		src["targetRevision"] = version
		return nil
	})
}

// ClusterPin is one managed cluster's chart pin — the admin bump surface's row.
type ClusterPin struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ProjectID    string `json:"projectId"`
	ChartVersion string `json:"chartVersion"`
}

// ListClusterPins lists every proxcloud-managed cluster on this provider with its chart pin
// (ownership-labelled Applications only — pre-proxcloud clusters stay invisible).
func (s *Service) ListClusterPins(ctx context.Context) ([]ClusterPin, error) {
	apps, err := s.api.ListApplications(ctx, s.cfg.ArgoNamespace, LabelManagedBy+"="+ManagedByValue)
	if err != nil {
		return nil, err
	}
	out := make([]ClusterPin, 0, len(apps))
	for _, app := range apps {
		if dig(app, "metadata", "deletionTimestamp") != nil {
			continue
		}
		id := digStr(app, "metadata", "name")
		if id == "" {
			continue
		}
		out = append(out, ClusterPin{
			ID:           id,
			Name:         digStr(app, "metadata", "annotations", AnnotationDisplayName),
			ProjectID:    digStr(app, "metadata", "labels", LabelProject),
			ChartVersion: digStr(app, "spec", "source", "targetRevision"),
		})
	}
	return out, nil
}

// findTCP resolves the cluster's TenantControlPlane. The chart names the KamajiControlPlane
// `<clusterID>-kamaji-cp` and the Kamaji CAPI provider gives the TenantControlPlane the same name,
// so that is the direct hit; the fallback is the label the provider stamps on it,
// `cluster.x-k8s.io/cluster-name` (both confirmed against a live management cluster). Nil when the
// control plane has not been provisioned yet.
func (s *Service) findTCP(ctx context.Context, ns, clusterID string) (map[string]any, error) {
	tcp, err := s.api.GetTenantControlPlane(ctx, ns, ControlPlaneName(clusterID))
	if err != nil || tcp != nil {
		return tcp, err
	}
	list, err := s.api.ListTenantControlPlanes(ctx, ns, "cluster.x-k8s.io/cluster-name="+clusterID)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return list[0], nil
}

// dig walks nested map[string]any keys; nil when any hop is absent.
func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func digStr(m map[string]any, keys ...string) string {
	s, _ := dig(m, keys...).(string)
	return s
}
