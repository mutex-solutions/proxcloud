// Package dbaas drives Managed Database clusters on a dedicated, ops-built DB Kubernetes
// cluster: one ArgoCD Application per database (chart `database-cluster` from our OCI registry,
// pinned targetRevision + full generated values), applied through the DB cluster's own API —
// the kamaji delivery pattern on a different cluster. ArgoCD renders the engine operator CR
// (CNPG / Percona PS / MariaDB / Valkey / FerretDB-on-CNPG), the tenant-facing LoadBalancer
// Service and a NetworkPolicy; proxcloud reads status back off the Application and the LB
// Service. Unlike kamaji, the compute/storage under a database is OPS-owned — the only
// tenant-facing artifact is the LB ingress IP, restricted by loadBalancerSourceRanges.
package dbaas

import (
	"fmt"
	"net"
	"slices"
)

// Namespace / naming derivations — the ONE place these are derived (kamaji precedent).
// Customer-typed names are display-only; every k8s-side identifier is the generated database id
// (`std-<8 hex>`), so duplicate display names, unicode and RFC1123 limits are non-issues.

// NamespaceFor is the DB-cluster namespace holding a project's databases. Same derivation as
// kamaji's (different physical cluster, so no collision).
func NamespaceFor(projectID string) string { return "pc-" + projectID }

// LBServiceName is the chart-rendered tenant-facing LoadBalancer Service for a database — the
// chart contract twin of values.proxcloud.resourceId (templates/service-lb.yaml). The sync reads
// the LB ingress IP off its status; getting this wrong is invisible until the first endpoint read,
// so it lives here next to the other name derivations.
func LBServiceName(dbID string) string { return dbID + "-lb" }

// MarkerSecretName is the per-database marker secret (DB-cluster side only): the durable record
// that a database exists, which outlives the Application. FinalizeOrphans keys on it to reap the
// auth secret and GC the namespace once the delete cascade is gone (the kamaji cloud-credentials
// precedent).
func MarkerSecretName(dbID string) string { return dbID + markerSuffix }

// markerSuffix lets FinalizeOrphans map a secret name back to its database id.
const markerSuffix = "-marker"

// Application labels/annotations stamped by proxcloud and read back by the sync. Same ownership
// discipline as kamaji: the sync lists ONLY objects carrying the managed-by marker, and
// delete/patch REFUSE any object without it.
const (
	LabelProject          = "proxcloud.io/project"
	LabelService          = "proxcloud.io/service"
	LabelManagedBy        = "app.kubernetes.io/managed-by"
	ManagedByValue        = "proxcloud"
	AnnotationDisplayName = "proxcloud.io/display-name"
)

// managedBy reports whether obj carries the proxcloud ownership marker.
func managedBy(obj map[string]any) bool {
	labels, _ := dig(obj, "metadata", "labels").(map[string]any)
	v, _ := labels[LabelManagedBy].(string)
	return v == ManagedByValue
}

// Config is the provider-level connection + chart contract, assembled from the dbaas
// externalService document by externalservice.DbaasConfig.
type Config struct {
	Kubeconfig    string // DB-cluster kubeconfig (provider secret)
	Region        string // the proxcloud region stamped on cached resources
	ArgoNamespace string // namespace holding Application CRs (default "argocd")
	ArgoProject   string // AppProject constraining sources/destinations (default "proxcloud-dbaas")
	ChartRepo     string // OCI helm repo, e.g. "ghcr.io/mutex-solutions/charts" (no oci:// prefix)
	ChartName     string // default "database-cluster"
	ChartVersion  string // pinned default for NEW databases; existing ones keep their own pin

	// StorageClasses is the storage-class allowlist offered to customers (empty = cluster
	// default only).
	StorageClasses []string
	Limits         Limits
	// Engines is the curated engine catalog (the ONLY engines/versions offered).
	Engines map[string]EngineOffer
}

// Limits bound a single database's per-instance size (fail at create, not at the operator).
type Limits struct {
	MaxCPU        int
	MaxMemoryGiB  int
	MaxStorageGiB int
}

// Validate rejects a config that cannot possibly provision (fail at create, not mid-flight).
func (c Config) Validate() error {
	switch {
	case c.Kubeconfig == "":
		return fmt.Errorf("dbaas provider: secret.kubeconfig is required")
	case c.ChartRepo == "":
		return fmt.Errorf("dbaas provider: config.argocd.chartRepo is required")
	case c.ChartVersion == "":
		return fmt.Errorf("dbaas provider: config.argocd.chartVersion is required (never latest)")
	}
	return nil
}

// DatabaseSpec is the desired state of ONE customer database — the values-builder input. The
// full generated values live on the Application (spec.source.helm.valuesObject); this struct is
// what the client API accepts and what actions mutate.
type DatabaseSpec struct {
	ID          string // generated std-<8hex>; Application/release/CR name
	DisplayName string // customer-typed, display-only
	ProjectID   string
	Engine      string // key of Config.Engines
	Version     string // must be offered for the engine
	Replicas    int    // engine-semantic instance count (CNPG instances / GR size / …)
	CPU         int    // cores per instance (requests == limits — Guaranteed QoS)
	MemoryGiB   int
	StorageGiB  int
	// StorageClass picks off Config.StorageClasses; "" = the DB cluster's default class.
	StorageClass string
	// AllowedCIDRs restricts LB ingress (loadBalancerSourceRanges). Empty = open.
	AllowedCIDRs []string
	// BetaAck acknowledges a beta engine (valkey): required true when the engine's offer is
	// flagged Beta, so nobody lands on a pre-GA operator by accident.
	BetaAck bool
}

// Validate rejects an unbuildable spec against the provider catalog/limits.
func (s DatabaseSpec) Validate(c Config) error {
	switch {
	case s.ID == "":
		return fmt.Errorf("database: id is required")
	case s.ProjectID == "":
		return fmt.Errorf("database: projectId is required")
	case s.Engine == "":
		return fmt.Errorf("database: engine is required")
	}
	offer, ok := c.Engines[s.Engine]
	if !ok {
		return fmt.Errorf("database: engine %q is not offered by this provider", s.Engine)
	}
	if offer.Beta && !s.BetaAck {
		return fmt.Errorf("database: engine %q is in beta — pass beta:true to acknowledge", s.Engine)
	}
	if s.Version == "" || !slices.Contains(offer.Versions, s.Version) {
		return fmt.Errorf("database: version %q is not offered for engine %s", s.Version, s.Engine)
	}
	if replicas := offer.ReplicaChoices(); !slices.Contains(replicas, s.Replicas) {
		return fmt.Errorf("database: engine %s offers %v replicas, not %d", s.Engine, replicas, s.Replicas)
	}
	if s.CPU < 1 || (c.Limits.MaxCPU > 0 && s.CPU > c.Limits.MaxCPU) {
		return fmt.Errorf("database: cpu must be 1..%d", max(c.Limits.MaxCPU, 1))
	}
	if s.MemoryGiB < 1 || (c.Limits.MaxMemoryGiB > 0 && s.MemoryGiB > c.Limits.MaxMemoryGiB) {
		return fmt.Errorf("database: memoryGiB must be 1..%d", max(c.Limits.MaxMemoryGiB, 1))
	}
	if s.StorageGiB < 1 || (c.Limits.MaxStorageGiB > 0 && s.StorageGiB > c.Limits.MaxStorageGiB) {
		return fmt.Errorf("database: storageGiB must be 1..%d", max(c.Limits.MaxStorageGiB, 1))
	}
	if s.StorageClass != "" && !slices.Contains(c.StorageClasses, s.StorageClass) {
		return fmt.Errorf("database: storage class %q is not offered by this provider", s.StorageClass)
	}
	for _, cidr := range s.AllowedCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("database: allowed CIDR %q: %w", cidr, err)
		}
	}
	return nil
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
