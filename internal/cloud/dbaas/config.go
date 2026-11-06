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
	"regexp"
	"slices"
	"strings"
	"time"
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

// CustomTLSSecretName is the proxcloud-owned BYO-certificate secret for an opensearch database's
// customer domain (SET_CUSTOM_DOMAIN writes it; the chart mounts it on Dashboards when
// values.opensearch.customDomain is set).
func CustomTLSSecretName(dbID string) string { return dbID + "-custom-tls" }

// UserSecretName is the proxcloud-owned password Secret for a customer-created database user.
// Passwords NEVER travel through values (the Application CR is argocd-readable) — the chart
// only references this Secret by name from the engine's User/role CR.
func UserSecretName(dbID, username string) string { return dbID + "-u-" + K8sName(username) }

// K8sName maps a customer identifier to the shape a Kubernetes object name may take. identRE
// allows '_' (a legitimate SQL identifier character) but RFC1123 names do not — creating the
// user Secret or any derived CR for `keycloak_stag` was a 422 the customer saw as an internal
// error. '-' cannot appear in an ident, so the mapping is injective for a SINGLE ident. It is
// NOT injective for idents joined with '-' (`a_b`+`c` and `a`+`b_c` both map to `a-b-c`), so a
// name built from a PAIR must be collision-checked — ValidateAccess does, for the one such name
// (mariadb's per-grant CR). The chart applies the SAME mapping wherever it derives an object
// name from a customer ident (`replace "_" "-"`) — change both together.
func K8sName(ident string) string { return strings.ReplaceAll(ident, "_", "-") }

// identRE is the ONLY shape a customer-supplied database or user name may take. This is a
// security control, not cosmetics: mariadb-operator interpolates these strings straight into
// SQL (upstream issue #1722, closed as not planned), so proxcloud is the escaping layer. Lower
// case keeps postgres from folding identifiers and keeps the name usable as a Secret suffix.
var identRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)

// ValidIdent reports whether a customer-typed database/user name is safe to pass to an operator.
func ValidIdent(s string) bool { return identRE.MatchString(s) }

// reservedIdents are names a customer must not claim: the engine's own accounts and the
// proxcloud-provisioned app user. Taking one would either fail at the operator or, worse,
// silently retarget the credential the connection panel hands out.
var reservedIdents = map[string]bool{
	"postgres": true, "app": true, "root": true, "mysql": true, "mariadb": true,
	"admin": true, "kibanaserver": true, "template0": true, "template1": true,
	"streaming_replica": true, "cnpg_metrics_exporter": true,
	// valkey's built-in ACL account — the credential the connection panel hands out; a customer
	// redefining it would retarget that credential.
	"default": true,
}

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

	// LBAddressPool names the MetalLB address pool every tenant-facing LoadBalancer Service of a
	// database draws its address from (annotation metallb.universe.tf/address-pool): the
	// services subnet tenants reach over their normal egress. Empty = no annotation, MetalLB
	// picks from any auto-assign pool.
	LBAddressPool string
	// ZoneKey is the topology key the replicas of one database spread across as a hard
	// anti-affinity (chart scheduling.zoneKey). Empty = the chart default,
	// topology.kubernetes.io/zone; a site with a single node or zone overrides it.
	ZoneKey string
	// Backup is the operator-supplied S3 object store every database's backups land in (RustFS,
	// RGW, any S3 endpoint). Empty Bucket/Endpoint = the whole backup surface is off (no
	// capability, no UI, no CRs).
	Backup BackupConfig
	// StorageClasses is the storage-class allowlist offered to customers (empty = cluster
	// default only). Capacity-tier classes (CapacityClass) are never offered.
	StorageClasses []string
	Limits         Limits
	// Engines is the curated engine catalog (the ONLY engines/versions offered).
	Engines map[string]EngineOffer
	// NodeSelector/Tolerations place every pod a database runs — engine, proxy (haproxy for
	// mysql/mariadb), FerretDB frontend — on a dedicated database node pool. Both matter: a
	// selector alone still lets other workloads onto the pool, a toleration alone does not keep
	// database pods on it. Empty = schedule anywhere (the chart's default). Applied at create;
	// an existing database keeps the placement on its Application.
	NodeSelector map[string]string
	// Tolerations are opaque Kubernetes toleration objects, passed to the chart unchanged.
	Tolerations []map[string]any
}

// CapacityClass reports whether a storage class is the capacity tier (HDD-backed NFS), which is
// never offered to databases: a class named `capacity` or `capacity-*`.
func CapacityClass(name string) bool {
	return name == "capacity" || strings.HasPrefix(name, "capacity-")
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
	// ReadEndpoint adds a second, read-only endpoint. Only meaningful above one instance —
	// on a single node it would be the primary under another name, which is a lie the customer
	// would pay for. What it COSTS depends on the engine and that is not cosmetic:
	// postgresql's readers are different pods so it needs a second LoadBalancer, while mysql
	// and mariadb route reads through the same proxy on port 3307 and get it for free.
	ReadEndpoint bool
	// DashboardsEnabled deploys OpenSearch Dashboards (opensearch only) with its own
	// tenant-facing LB (<id>-dash-lb). SET_SSO also flips this on — SSO rides Dashboards.
	DashboardsEnabled bool
	// RestoreFrom bootstraps this database from ANOTHER database's backups instead of an empty
	// datadir. Restore is create-time by nature: the physical backups these engines take can
	// only be laid down on a fresh data directory, so recovery produces a NEW database and the
	// damaged one stays untouched until the customer is satisfied.
	RestoreFrom *RestoreSource
	// SSO is the Dashboards OIDC block, accepted at create so a customer does not have to
	// create the database and then immediately reconfigure it. Same shape SET_SSO patches
	// later (values.opensearch.sso); empty = off. Setting it implies DashboardsEnabled.
	SSO map[string]any
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
	if s.DashboardsEnabled && s.Engine != EngineOpenSearch {
		return fmt.Errorf("database: dashboards are an opensearch feature")
	}
	if s.ReadEndpoint {
		if !ReadEndpointEngines[s.Engine] {
			return fmt.Errorf("database: engine %s has no separate read endpoint", s.Engine)
		}
		if s.Replicas < 2 {
			return fmt.Errorf("database: a read-only endpoint needs more than one instance")
		}
	}
	if len(s.SSO) > 0 && s.Engine != EngineOpenSearch {
		return fmt.Errorf("database: sso is an opensearch feature")
	}
	if s.RestoreFrom != nil {
		// RESTORE, not BACKUP: valkey/opensearch/kafka have no object-store backup to restore from.
		if !Capabilities[s.Engine]["RESTORE"] {
			return fmt.Errorf("database: engine %s cannot be restored into a new database", s.Engine)
		}
		if !c.Backup.Enabled() {
			return fmt.Errorf("database: this location has no backup object store configured")
		}
		if err := s.RestoreFrom.Validate(); err != nil {
			return err
		}
		// mysql restores by naming a backup OBJECT; the other engines search the object store
		// for the right base backup themselves.
		if s.Engine == EngineMySQL && s.RestoreFrom.BackupName == "" {
			return fmt.Errorf("database: restoring %s needs a specific backup to restore from", s.Engine)
		}
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
	if s.StorageClass != "" && (CapacityClass(s.StorageClass) || !slices.Contains(c.StorageClasses, s.StorageClass)) {
		return fmt.Errorf("database: storage class %q is not offered by this provider", s.StorageClass)
	}
	for _, cidr := range s.AllowedCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("database: allowed CIDR %q: %w", cidr, err)
		}
	}
	return nil
}

// BackupConfig is the platform's backup object store, configured once per provider. Customers
// never see or choose it — they only turn backups on and pick a schedule. The credentials are
// NOT here: they live in the provider secret and are written to a Secret on the DB cluster,
// because every operator takes them as a SecretKeySelector and values must stay readable.
type BackupConfig struct {
	// Endpoint is the S3 endpoint. Stored WITH a scheme (https://s3.example.com, or a plain
	// http:// lab endpoint) — the
	// engines disagree about the shape, so normalising is the chart's job (mariadb wants
	// host:port with no scheme and derives TLS from a flag; CNPG and Percona want the URL).
	Endpoint  string
	Bucket    string
	Region    string
	Prefix    string
	AccessKey string
	SecretKey string
	// PathStyle forces path-style addressing. Default ON: RustFS and Ceph RGW are addressed
	// path-style, and a virtual-host request makes the gateway parse the bucket out of the
	// Host header — the trap that already broke object storage once.
	PathStyle bool
	// CACertSecret optionally names a Secret (key ca.crt) holding the endpoint's CA, for an
	// endpoint behind a private CA.
	CACertSecret string
}

// Enabled reports whether the provider can back anything up at all.
func (b BackupConfig) Enabled() bool { return b.Bucket != "" && b.Endpoint != "" }

// BackupSecretName is the proxcloud-owned Secret holding the object-store credentials for one
// database. Per database rather than per namespace so a delete cascade takes its credential
// with it, and so the key set can differ per engine without cross-talk.
func BackupSecretName(dbID string) string { return dbID + "-backup-s3" }

// BackupCredKeys are the key names written into that Secret. Every engine gets the aliases it
// expects from the SAME secret, so there is one object to write, rotate and reap:
//   - CNPG reads ACCESS_KEY_ID / ACCESS_SECRET_KEY (its own convention)
//   - Percona reads AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (a whole-secret reference)
//   - mariadb reads whichever keys its SecretKeyRefs name; the chart points it at the AWS pair
var BackupCredKeys = struct{ CNPGAccess, CNPGSecret, AWSAccess, AWSSecret string }{
	CNPGAccess: "ACCESS_KEY_ID",
	CNPGSecret: "ACCESS_SECRET_KEY",
	AWSAccess:  "AWS_ACCESS_KEY_ID",
	AWSSecret:  "AWS_SECRET_ACCESS_KEY",
}

// cronRE is a plain 5-field cron (minute hour dom month dow). ONE canonical format crosses the
// whole platform: CNPG's ScheduledBackup wants SIX fields (leading seconds) while Percona and
// mariadb want five, and generating the wrong arity is the single most common misconfiguration
// in this area — a 5-field string in a 6-field parser silently runs at the wrong time. ProxCloud
// stores five; the CNPG template prepends the seconds field.
var cronRE = regexp.MustCompile(`^[-0-9*/,]+ [-0-9*/,]+ [-0-9*/,?LW]+ [-0-9*/,]+ [-0-9*/,?L#]+$`)

// RestoreSource names what a new database recovers from.
type RestoreSource struct {
	// SourceID is the database id whose backups are read. Its object-store folder is derived
	// the same way the source's own backup path was, so nothing else needs recording.
	SourceID string
	// TargetTime (RFC3339) recovers to that instant instead of the latest available state.
	// Empty = replay everything, i.e. as close to the failure as the archive reaches.
	TargetTime string
	// PITR asks for true point-in-time recovery rather than "the nearest backup". Only
	// meaningful for mariadb, and only when the SOURCE ran replication — 26.6.0 archives
	// binlogs on no other topology. The handler decides it from the source's replica count;
	// customers never send it.
	PITR bool
	// BackupName pins WHICH backup to start from. Optional for CNPG and mariadb, which find
	// the right base backup themselves; REQUIRED for mysql, whose restore CR names a specific
	// PerconaServerMySQLBackup object rather than searching the object store.
	BackupName string
}

// Validate checks a restore request in isolation; the caller additionally proves the source
// exists, shares this engine and actually has backups.
func (r RestoreSource) Validate() error {
	if !strings.HasPrefix(r.SourceID, "std-") {
		return fmt.Errorf("restore: sourceDatabaseId is required")
	}
	if r.TargetTime != "" {
		if _, err := time.Parse(time.RFC3339, r.TargetTime); err != nil {
			return fmt.Errorf("restore: targetTime must be an RFC3339 timestamp")
		}
	}
	// The name reaches a CR reference, so keep it to the shape k8s names take.
	if r.BackupName != "" && !k8sNameRE.MatchString(r.BackupName) {
		return fmt.Errorf("restore: backupName %q is not a valid backup name", r.BackupName)
	}
	return nil
}

// k8sNameRE is the RFC1123 subdomain shape a Kubernetes object name takes.
var k8sNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// BackupSpec is a database's desired backup posture.
type BackupSpec struct {
	Enabled bool
	// Schedule is a 5-field cron, "" = on-demand only.
	Schedule string
	// RetentionDays bounds how long backups are kept. 0 = keep forever.
	RetentionDays int
}

// DefaultBackupSchedule and DefaultBackupRetentionDays are the posture stamped at CREATE, and
// they exist for two separate reasons.
//
// A schedule has to be stamped AT ALL because continuous archiving alone cannot restore
// anything. WAL and binlog shipping start with the database, but a log is a delta — without a
// base backup underneath it there is nothing to replay onto. A database created with no schedule
// would archive to the object store forever and still be unrecoverable, which is the worst shape
// this feature could take: it looks protected right up to the moment it is needed.
//
// The cadence is WEEKLY rather than daily because every operator here takes a FULL physical copy
// of the data directory on each run — CloudNativePG has no incremental base at all and mariadb
// does not expose one — so a 30-day retention at a daily cadence is THIRTY complete copies of
// the database sitting in the object store. Weekly is four. What makes that safe is the other
// half of the design: the log ships continuously either way, so recovery still reaches any
// second in the window. A base that is a few days old costs replay time, not data. Customers who
// want a shorter replay can move the schedule to daily; the cost is theirs to choose.
const (
	DefaultBackupSchedule      = "0 2 * * 0" // Sunday 02:00
	DefaultBackupRetentionDays = 30
)

// Validate rejects a backup posture the operators would refuse (or, worse, silently misread).
func (b BackupSpec) Validate() error {
	if b.Schedule != "" && !cronRE.MatchString(b.Schedule) {
		return fmt.Errorf("backup: schedule must be a 5-field cron expression, e.g. \"0 2 * * *\"")
	}
	if b.RetentionDays < 0 || b.RetentionDays > 3650 {
		return fmt.Errorf("backup: retention must be 0..3650 days")
	}
	return nil
}

// ReadEndpointEngines are the engines that expose a reader separate from the writer.
// valkey/opensearch/kafka have no such split, and ferretdb would need a second set of
// frontends rather than a second endpoint on the same ones.
var ReadEndpointEngines = map[string]bool{
	EnginePostgreSQL: true,
	EngineMySQL:      true,
	EngineMariaDB:    true,
}

// ReadEndpointCostsLB reports whether offering the read endpoint provisions a SECOND
// LoadBalancer Service. Only postgresql does: its readers are a different set of pods, so no port on
// the existing proxy reaches them. This is what billing keys on — charging mysql and mariadb
// for an endpoint that costs nothing would not be defensible.
func ReadEndpointCostsLB(engine string) bool { return engine == EnginePostgreSQL }

// DBDatabase is one logical database a customer asked for inside their instance.
type DBDatabase struct {
	Name string
	// Owner is the user that owns it; "" = the engine's built-in app user.
	Owner string
}

// DBUser is one customer-created login. Databases lists what it may reach (postgresql/mariadb);
// Roles lists the OpenSearch roles bound to it. A user carries NO password here — the password
// lives only in UserSecretName(dbID, name) on the DB cluster.
type DBUser struct {
	Name      string
	Databases []string
	Roles     []string
}

// openSearchRoles is the allowlist of built-in OpenSearch roles a customer may bind. Custom
// roles (OpensearchRole CRs with index patterns) are a separate, later surface — this list is
// what the security plugin ships and what cannot be typo'd into a privilege escalation.
var openSearchRoles = map[string]bool{
	"all_access": true, "readall": true, "readall_and_monitor": true,
	"kibana_user": true, "kibana_read_only": true, "manage_snapshots": true,
	"alerting_read_access": true, "alerting_full_access": true,
	"index_management_full_access": true, "logstash": true,
}

// OSRole is a customer-defined OpenSearch role: index permissions expressed as patterns plus
// action groups. Cluster permissions are deliberately NOT customer-settable — they are how a
// role reaches other tenants' concerns (and the operator's own admin surface).
type OSRole struct {
	Name string
	// IndexPatterns the role applies to (e.g. "logs-*").
	IndexPatterns []string
	// Actions are OpenSearch ACTION GROUP names, allowlisted (osActionGroups).
	Actions []string
}

// OSIndexPolicy is a retention/rollover policy, the useful subset of ISM. The full ISM state
// machine is an operator-grade tool; this is the shape customers actually ask for, and the
// chart expands it into the real two-state policy.
type OSIndexPolicy struct {
	Name          string
	IndexPatterns []string
	// DeleteAfterDays is the index age at which the policy deletes it. REQUIRED (>0) — a
	// policy that never deletes is a no-op that still costs a CR.
	DeleteAfterDays int
	// RolloverGiB / RolloverDays roll the write index over before it ages out. 0 = no rollover.
	RolloverGiB  int
	RolloverDays int
}

// osActionGroups is the allowlist of OpenSearch built-in action groups a customer role may
// grant. Free-form action strings are refused: "indices:admin/*" in the wrong hands is a
// cluster-management grant wearing an index-permission costume.
var osActionGroups = map[string]bool{
	"read": true, "search": true, "get": true, "write": true, "index": true,
	"crud": true, "delete": true, "create_index": true, "manage": true,
	"indices_all": true, "data_access": true,
}

// ValidateOSRoles checks customer-defined roles: safe names, at least one pattern, and only
// allowlisted action groups.
func ValidateOSRoles(roles []OSRole) error {
	seen := map[string]bool{}
	for _, r := range roles {
		switch {
		case !ValidIdent(r.Name):
			return fmt.Errorf("role %q: names must be lower-case, start with a letter and use only letters, digits and underscores (max 31)", r.Name)
		case seen[r.Name]:
			return fmt.Errorf("role %q is listed twice", r.Name)
		case openSearchRoles[r.Name]:
			return fmt.Errorf("role %q is a built-in role name", r.Name)
		case len(r.IndexPatterns) == 0:
			return fmt.Errorf("role %q: at least one index pattern is required", r.Name)
		case len(r.Actions) == 0:
			return fmt.Errorf("role %q: at least one permission is required", r.Name)
		}
		seen[r.Name] = true
		for _, p := range r.IndexPatterns {
			if p == "" || len(p) > 255 || strings.ContainsAny(p, " \t\n\"'\\") {
				return fmt.Errorf("role %q: index pattern %q is not valid", r.Name, p)
			}
		}
		for _, a := range r.Actions {
			if !osActionGroups[a] {
				return fmt.Errorf("role %q: permission %q is not offered", r.Name, a)
			}
		}
	}
	return nil
}

// ValidateOSIndexPolicies checks retention policies: safe names, real patterns, and a delete
// age that actually comes after any rollover age.
func ValidateOSIndexPolicies(policies []OSIndexPolicy) error {
	seen := map[string]bool{}
	for _, p := range policies {
		switch {
		case !ValidIdent(p.Name):
			return fmt.Errorf("policy %q: names must be lower-case, start with a letter and use only letters, digits and underscores (max 31)", p.Name)
		case seen[p.Name]:
			return fmt.Errorf("policy %q is listed twice", p.Name)
		case len(p.IndexPatterns) == 0:
			return fmt.Errorf("policy %q: at least one index pattern is required", p.Name)
		case p.DeleteAfterDays < 1 || p.DeleteAfterDays > 3650:
			return fmt.Errorf("policy %q: delete after must be 1..3650 days", p.Name)
		case p.RolloverGiB < 0 || p.RolloverGiB > 10000:
			return fmt.Errorf("policy %q: rollover size must be 0..10000 GiB", p.Name)
		case p.RolloverDays < 0 || p.RolloverDays >= p.DeleteAfterDays:
			return fmt.Errorf("policy %q: rollover age must be less than the delete age", p.Name)
		}
		seen[p.Name] = true
		for _, pat := range p.IndexPatterns {
			if pat == "" || len(pat) > 255 || strings.ContainsAny(pat, " \t\n\"'\\") {
				return fmt.Errorf("policy %q: index pattern %q is not valid", p.Name, pat)
			}
		}
	}
	return nil
}

// ValidateAccess checks a whole desired access list against the engine before anything is
// written. Fail here, not at the operator: half-applied access (a Secret with no CR, or a Grant
// naming a user that was rejected) is the state nobody can reason about.
func ValidateAccess(engine string, dbs []DBDatabase, users []DBUser, roles []OSRole) error {
	if engine == EngineOpenSearch {
		if err := ValidateOSRoles(roles); err != nil {
			return err
		}
	}
	// valkey is USERS ONLY: one keyspace, no logical databases to declare or grant. Each user is
	// an ACL entry on the ValkeyCluster CR, authenticated from its proxcloud-owned Secret.
	if engine == EngineValkey {
		if len(dbs) > 0 {
			return fmt.Errorf("valkey has no logical databases — manage users only")
		}
		for _, u := range users {
			if len(u.Databases) > 0 {
				return fmt.Errorf("user %q: valkey users take no database grants", u.Name)
			}
		}
	}
	custom := map[string]bool{}
	for _, r := range roles {
		custom[r.Name] = true
	}
	names := map[string]bool{}
	for _, u := range users {
		switch {
		case !ValidIdent(u.Name):
			return fmt.Errorf("user %q: names must be lower-case, start with a letter and use only letters, digits and underscores (max 31)", u.Name)
		case reservedIdents[u.Name]:
			return fmt.Errorf("user %q is reserved", u.Name)
		case names[u.Name]:
			return fmt.Errorf("user %q is listed twice", u.Name)
		}
		names[u.Name] = true
		if engine == EngineOpenSearch {
			for _, r := range u.Roles {
				// Built-in role, or one the caller declared in this very request. Anything else
				// would bind to whatever happens to exist in the cluster.
				if !openSearchRoles[r] && !custom[r] {
					return fmt.Errorf("user %q: role %q is not offered", u.Name, r)
				}
			}
		}
	}
	dbNames := map[string]bool{}
	for _, d := range dbs {
		switch {
		case !ValidIdent(d.Name):
			return fmt.Errorf("database %q: names must be lower-case, start with a letter and use only letters, digits and underscores (max 31)", d.Name)
		case reservedIdents[d.Name]:
			return fmt.Errorf("database %q is reserved", d.Name)
		case dbNames[d.Name]:
			return fmt.Errorf("database %q is listed twice", d.Name)
		case d.Owner != "" && !names[d.Owner]:
			return fmt.Errorf("database %q: owner %q is not one of the listed users", d.Name, d.Owner)
		}
		dbNames[d.Name] = true
	}
	// Derived-name collision guard. The chart renders one object per (user, database) grant named
	// <id>-g-<K8sName(user)>-<K8sName(db)> (mariadb access.yaml); K8sName is injective per ident
	// but the pair is JOINED with the same '-' the mapping produces, so `a_b`×`c` and `a`×`b_c`
	// derive one name — two CRs with the same identity wedge the ArgoCD sync. Refuse the list
	// instead, uniformly across engines: the rule is about the platform's name derivation, not one
	// operator. This also catches the same database listed twice on one user.
	grants := map[string][2]string{}
	for _, u := range users {
		for _, d := range u.Databases {
			if !dbNames[d] {
				return fmt.Errorf("user %q: database %q is not in the list", u.Name, d)
			}
			key := K8sName(u.Name) + "-" + K8sName(d)
			if prev, dup := grants[key]; dup {
				return fmt.Errorf("the grant of %q on %q collides with the grant of %q on %q — underscores and the name separator both become %q; rename one", u.Name, d, prev[0], prev[1], "-")
			}
			grants[key] = [2]string{u.Name, d}
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
