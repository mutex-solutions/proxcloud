package dbaas

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// engines.go — the ONE place per-engine knowledge lives: catalog offers, action capabilities,
// connection-secret conventions, ports and URI shapes. The chart's engine templates are the
// other half of each contract; TestConnectionSecretContract pins the tuples so drift between
// the two is a test failure, not a silent broken GET_CONNECTION_INFO.

// Engine names — the values.engine discriminator the chart switches templates on.
const (
	EnginePostgreSQL = "postgresql" // CloudNativePG
	EngineMySQL      = "mysql"      // Percona Server for MySQL operator (group replication)
	EngineMariaDB    = "mariadb"    // mariadb-operator (+ chart-owned HAProxy)
	EngineValkey     = "valkey"     // valkey-operator — pre-GA, beta-gated
	EngineFerretDB   = "ferretdb"   // FerretDB v2 over a CNPG backend (Mongo-compatible)
	EngineOpenSearch = "opensearch" // opensearch-k8s-operator (+ optional Dashboards, OIDC SSO)
	EngineKafka      = "kafka"      // Strimzi (KRaft; external loadbalancer listener, SCRAM)
)

// EngineOffer is one engine's catalog entry on a provider (config.database.engines.<name>).
type EngineOffer struct {
	Versions []string
	Default  string
	Replicas []int // allowed instance counts; empty = {1}
	Beta     bool  // pre-GA operator: create requires an explicit beta acknowledgment
}

// ReplicaChoices is the allowed instance-count set ({1} when unconfigured).
func (o EngineOffer) ReplicaChoices() []int {
	if len(o.Replicas) == 0 {
		return []int{1}
	}
	return o.Replicas
}

// Capabilities gates day-2 actions per engine. An action lands here only after its mechanism is
// verified against the live operator (RESET_PASSWORD semantics differ per operator; RESTART maps
// to different knobs). GET_CONNECTION_INFO and SET_ALLOWED_CIDRS are engine-agnostic (secret +
// Service reads / LB annotation) and not gated.
// ponytail: SWITCHOVER/PROMOTE deliberately absent — CNPG promotion is a CR-subresource
// operation, not values-shaped; add a pinned CNPG path + status patch when a customer asks.
// RESTART is OFF for mysql and valkey because no chart template consumes restartedAt for them
// (their template headers say exactly that) — returning "RESTARTING" for a no-op would be a
// lie; enable only together with a live-verified chart mechanism.
// UPGRADE mutates values.engineVersion (upward only, catalog-gated); the chart resolves the
// new image and the operator performs its own rolling upgrade (CNPG rolling minor / declarative
// in-place major, Percona smart update, mariadb rolling, ferretdb moves BOTH its images off the
// version via the matched pair in the chart). Off for valkey (operator unpinned).
// SET_SSO (opensearch only) writes the values.opensearch.sso block — Dashboards OIDC against a
// PUBLIC IdP client (no client secret; values must never carry secrets). RESIZE_STORAGE stays
// off for opensearch until the operator's diskSize-change semantics are live-verified; RESTART
// is off for opensearch/kafka (no values-shaped restart knob — Strimzi rolls via a per-podset
// annotation, not the CR).
// BACKUP (postgresql/mariadb/mysql/ferretdb) is object-store backup with a schedule and a
// retention window, plus on-demand runs. Gated on the PROVIDER having an object store as well:
// without one the capability is reported off, because a backup toggle that writes nowhere is
// worse than no toggle. valkey/opensearch/kafka are out by nature — caches and streams are not
// what customers ask to restore, and neither operator has an object-store backup CR worth the
// surface.
//
// SET_PARAMETERS is the runtime-configuration surface (parameters.go). ferretdb is the one
// engine with a backing store but NO tunables: its CNPG cluster carries a fixed DocumentDB
// parameter block that customer settings must never be able to disturb.
//
// RESTORE always produces a NEW database, but the mechanism differs. postgresql/ferretdb/mariadb
// BOOTSTRAP from an object-store folder. mysql cannot bootstrap: its PerconaServerMySQLRestore
// targets a RUNNING cluster — so the chart brings the new cluster up empty and then applies a
// Restore against it (sync-wave 1), which the operator carries out. Same outcome for the
// customer, two phases underneath, and it needs a specific backup NAME rather than a search.
//
// MANAGE_ACCESS (postgresql/mariadb/opensearch/valkey) is customer-managed logical databases,
// users and grants, declared in values and rendered as the operators' own CRs (CNPG Database +
// managed.roles; mariadb Database/User/Grant; OpensearchUser + OpensearchUserRoleBinding) or,
// for valkey, extra ACL entries on the ValkeyCluster's own spec.users list — valkey has no
// logical databases, so its surface is USERS ONLY (ValidateAccess enforces that).
// mysql is deliberately OUT: the Percona ps-operator has no user/database CRDs, so there is
// nothing to reconcile against and a SQL-over-exec implementation is not a values-shaped
// mechanism. ferretdb/kafka are out by nature — ferretdb authenticates through its CNPG
// backend's app role, and kafka already ships its KafkaUser.
//
// SET_AUTOSCALE (all engines) opts into the surcharge-priced vertical autoscale tick
// (autoscale.go): CPU/RAM bounds work wherever RESIZE does; the disk leg additionally requires
// RESIZE_STORAGE, so it self-disables for valkey/opensearch.
var Capabilities = map[string]map[string]bool{
	EnginePostgreSQL: {"SET_PARAMETERS": true, "BACKUP": true, "RESTORE": true, "SET_READ_ENDPOINT": true, "RESIZE": true, "RESIZE_STORAGE": true, "SCALE_REPLICAS": true, "RESTART": true, "RESET_PASSWORD": true, "UPGRADE": true, "SET_AUTOSCALE": true, "MANAGE_ACCESS": true},
	EngineMySQL:      {"SET_PARAMETERS": true, "BACKUP": true, "RESTORE": true, "SET_READ_ENDPOINT": true, "RESIZE": true, "RESIZE_STORAGE": true, "SCALE_REPLICAS": true, "RESTART": false, "RESET_PASSWORD": true, "UPGRADE": true, "SET_AUTOSCALE": true},
	EngineMariaDB:    {"SET_PARAMETERS": true, "BACKUP": true, "RESTORE": true, "SET_READ_ENDPOINT": true, "RESIZE": true, "RESIZE_STORAGE": true, "SCALE_REPLICAS": true, "RESTART": true, "RESET_PASSWORD": true, "UPGRADE": true, "SET_AUTOSCALE": true, "MANAGE_ACCESS": true},
	EngineValkey:     {"SET_PARAMETERS": true, "RESIZE": true, "RESIZE_STORAGE": false, "SCALE_REPLICAS": true, "RESTART": false, "RESET_PASSWORD": false, "UPGRADE": false, "SET_AUTOSCALE": true, "MANAGE_ACCESS": true},
	EngineFerretDB:   {"BACKUP": true, "RESTORE": true, "RESIZE": true, "RESIZE_STORAGE": true, "SCALE_REPLICAS": true, "RESTART": true, "RESET_PASSWORD": true, "UPGRADE": true, "SET_AUTOSCALE": true},
	// SET_INDEX_POLICIES was gated by the handler but declared by NO engine, so the action 400'd
	// before its dispatch could run and opensearch retention policies were dead end-to-end (the
	// console offered the dialog, the server refused every submit).
	EngineOpenSearch: {"SET_PARAMETERS": true, "RESIZE": true, "RESIZE_STORAGE": false, "SCALE_REPLICAS": true, "RESTART": false, "RESET_PASSWORD": false, "UPGRADE": true, "SET_SSO": true, "SET_CUSTOM_DOMAIN": true, "SET_AUTOSCALE": true, "MANAGE_ACCESS": true, "SET_INDEX_POLICIES": true},
	EngineKafka:      {"SET_PARAMETERS": true, "RESIZE": true, "RESIZE_STORAGE": true, "SCALE_REPLICAS": true, "RESTART": false, "RESET_PASSWORD": true, "UPGRADE": true, "SET_AUTOSCALE": true},
}

// LogContainerFor is the container whose stdout carries the engine's own log. Every pinned
// engine logs to stdout — none of them needs a sidecar — but they disagree about the container
// name, and reading the wrong one returns an operator's chatter instead of the database's.
func LogContainerFor(engine string) string {
	switch engine {
	case EnginePostgreSQL, EngineFerretDB:
		// CNPG's instance manager runs postgres in `postgres` and logs JSON. For ferretdb this
		// is the BACKEND's log; the stateless frontend has little to say.
		return "postgres"
	case EngineMySQL:
		return "mysql"
	case EngineMariaDB:
		return "mariadb"
	case EngineValkey:
		// "server", NOT "valkey": valkey-operator names the main container `server`
		// (valkeynode_resources.go on the pinned v0.4.0). Asking for `valkey` made every
		// per-pod log read fail on a healthy database.
		return "server"
	case EngineOpenSearch:
		return "opensearch"
	case EngineKafka:
		return "kafka"
	}
	return ""
}

// PodSelectorFor is the label selector matching EVERY pod of one database — the ownership
// boundary for pod-level reads (Logs). Derivation twin of the podSelector switch in the chart's
// templates/networkpolicy.yaml — change both together. app.kubernetes.io/instance is the
// convention most operators follow (CNPG via inheritedMetadata, Percona, mariadb, the chart's
// own Deployments), but three stamp their own cluster label instead:
//   - valkey:     valkey.io/cluster — the operator stamps app.kubernetes.io/instance PER NODE
//     as `<cluster>-<shard>-<node>`, so selecting on the bare id matched no pod and the Logs
//     read failed on a healthy database (drill-verified 2026-08-06)
//   - opensearch: opensearch.org/opensearch-cluster (labels moved with the api group on 3.0.2)
//   - kafka:      strimzi.io/cluster
func PodSelectorFor(engine, dbID string) string {
	switch engine {
	case EngineValkey:
		return "valkey.io/cluster=" + dbID
	case EngineOpenSearch:
		return "opensearch.org/opensearch-cluster=" + dbID
	case EngineKafka:
		return "strimzi.io/cluster=" + dbID
	}
	return "app.kubernetes.io/instance=" + dbID
}

// PodNamePrefixFor is the name prefix every pod of one database carries — Logs' second
// ownership check next to PodSelectorFor. Every operator here names workloads off the CR name
// (= the resource id) EXCEPT valkey-operator, which prepends its own "valkey-"
// (resourcePrefix in its utils.go, pinned v0.4.0): pods are valkey-<id>-<shard>-<node>-…, so
// requiring the bare id matched no pod and Logs failed on a healthy valkey.
func PodNamePrefixFor(engine, dbID string) string {
	if engine == EngineValkey {
		return "valkey-" + dbID
	}
	return dbID
}

// BackupCRFor maps an engine to the custom resource its operator produces per backup run
// (plural, group, version) — what ListBackups enumerates. Empty plural = the engine has no
// object-store backup CR and BACKUP is not offered for it.
func BackupCRFor(engine string) (plural, group, version string) {
	switch engine {
	case EnginePostgreSQL, EngineFerretDB:
		return "backups", "postgresql.cnpg.io", "v1"
	case EngineMySQL:
		return "perconaservermysqlbackups", "ps.percona.com", "v1"
	case EngineMariaDB:
		// PhysicalBackup is the one that can seed a PITR; the logical Backup CR is only used
		// for per-database dumps, which this platform does not offer.
		return "physicalbackups", "k8s.mariadb.com", "v1alpha1"
	}
	return "", "", ""
}

// ValidateUpgradePath enforces the version-change policy: the target must be OFFERED for the
// engine and strictly NEWER than the current version (dotted-numeric compare — "9.6" < "10");
// downgrades are refused outright (operators do not support them; a PG datadir is not
// backward-compatible). Same-major vs cross-major is left to the operator: CNPG handles both
// (rolling minor, declarative offline in-place major), mysql/mariadb catalogs are curated to
// sane steps.
func ValidateUpgradePath(current, target string) error {
	if current == target {
		return fmt.Errorf("already on version %s", current)
	}
	cur, err := parseVersion(current)
	if err != nil {
		return fmt.Errorf("current version %q: %w", current, err)
	}
	tgt, err := parseVersion(target)
	if err != nil {
		return fmt.Errorf("target version %q: %w", target, err)
	}
	for i := 0; i < len(cur) || i < len(tgt); i++ {
		c, t := 0, 0
		if i < len(cur) {
			c = cur[i]
		}
		if i < len(tgt) {
			t = tgt[i]
		}
		if t > c {
			return nil
		}
		if t < c {
			return fmt.Errorf("cannot downgrade from %s to %s", current, target)
		}
	}
	return fmt.Errorf("already on version %s", current)
}

// parseVersion splits a dotted numeric version ("11.4" → [11 4]).
func parseVersion(v string) ([]int, error) {
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("not a dotted numeric version")
		}
		out = append(out, n)
	}
	return out, nil
}

// ConnectionSecret returns the (secretName, userKey, passKey, dbKey) tuple GET_CONNECTION_INFO
// reads for an engine. Empty userKey/dbKey = the value is fixed, not in the secret — see
// DefaultUser/DefaultDB. Conventions (the chart's engine templates are the other half):
//   - postgresql: CNPG-generated SUPERUSER secret `<id>-superuser` (the real `postgres` role,
//     kept in sync by the operator because the chart sets enableSuperuserAccess). The customer
//     owns the database, so they get the account that can install extensions and create roles;
//     `<id>-app` stays as the fallback for databases created before that switch
//   - ferretdb:   the CNPG BACKEND is named `<id>-pg`, so its app secret is `<id>-pg-app`;
//     FerretDB reuses the backend's app credential over the Mongo wire protocol
//   - mysql:      the Percona operator auto-generates `<id>-secrets` (chart pins
//     spec.secretsName); the exposed account is root under key "root" — PS has no
//     declarative app users, and inventing one imperatively would fight the operator
//   - mariadb:    PROXCLOUD-provisioned `<id>-auth` (key "password"), wired via the CR's
//     passwordSecretKeyRef for the declarative app user. ProxCloud owns it — a
//     chart-generated password would re-roll on every ArgoCD render (selfHeal)
//   - valkey:     PROXCLOUD-provisioned `<id>-auth` (key "password"; AUTH only — no user/db)
//   - opensearch: operator-generated `<id>-admin-password` (keys username/password; the
//     operator mints it when no custom securityconfig is supplied — TODO-verify(drill)
//     the exact name on operator 3.x)
//   - kafka:      PROXCLOUD-provisioned `<id>-auth` (key "password"), wired via the
//     KafkaUser's password.valueFrom (BYO password — the SCRAM credential follows it);
//     the SASL username is the KafkaUser name `<id>-app`
func ConnectionSecret(engine, dbID string) (name, userKey, passKey, dbKey string) {
	switch engine {
	case EnginePostgreSQL:
		// dbKey stays empty on purpose: the superuser Secret's own `dbname` is not the database
		// the customer wants to land in (CNPG fills it for its own use), so DefaultDB decides.
		return dbID + "-superuser", "username", "password", ""
	case EngineFerretDB:
		return dbID + "-pg-app", "username", "password", "dbname"
	case EngineMySQL:
		return dbID + "-secrets", "", "root", ""
	case EngineMariaDB:
		return AuthSecretName(dbID), "", "password", ""
	case EngineValkey:
		return AuthSecretName(dbID), "", "password", ""
	case EngineOpenSearch:
		return dbID + "-admin-password", "username", "password", ""
	case EngineKafka:
		return AuthSecretName(dbID), "", "password", ""
	}
	return "", "", "", ""
}

// AuthSecretName is the proxcloud-provisioned app-credential secret for engines whose operator
// does not mint one itself (mariadb, valkey). Applied at create, patched by RESET_PASSWORD;
// deliberately OUTSIDE the Application so an ArgoCD sync never re-rolls the password.
func AuthSecretName(dbID string) string { return dbID + "-auth" }

// NeedsAuthSecret reports whether CreateDatabase must provision the engine's auth secret.
func NeedsAuthSecret(engine string) bool {
	return engine == EngineMariaDB || engine == EngineValkey || engine == EngineKafka
}

// DefaultUser is the exposed account name when ConnectionSecret carries no userKey. Kafka's is
// derived from the database id (the KafkaUser name IS the SASL username); valkey's is the
// `default` ACL user the chart wires to the <id>-auth secret — shown so the connection panel
// names the account, and reserved so MANAGE_ACCESS cannot redefine it.
func DefaultUser(engine, dbID string) string {
	switch engine {
	case EngineMySQL:
		return "root"
	case EngineMariaDB:
		return "app"
	case EngineKafka:
		return dbID + "-app"
	case EngineValkey:
		return "default"
	}
	return ""
}

// DefaultDB is the exposed database name when ConnectionSecret carries no dbKey. postgresql and
// mariadb both ship an `app` database created at bootstrap — the superuser/root account can
// reach every database, but a connection string still has to name one, and `app` is the one that
// exists and is the customer's. mysql root has no default schema; valkey has none by design.
func DefaultDB(engine string) string {
	if engine == EngineMariaDB || engine == EnginePostgreSQL {
		return "app"
	}
	return ""
}

// PriorConnectionSecret is the tuple used before postgresql moved to the superuser account —
// the scoped `app` role CNPG creates unconditionally. A database provisioned with
// superuserAccess off has no `<id>-superuser` Secret at all, so the connection panel and
// RESET_PASSWORD fall back to this rather than reporting "credentials not ready" on a database
// that is serving traffic. Empty name = no fallback for that engine.
func PriorConnectionSecret(engine, dbID string) (name, userKey, passKey, dbKey string) {
	if engine == EnginePostgreSQL {
		return dbID + "-app", "username", "password", "dbname"
	}
	return "", "", "", ""
}

// Port is the engine's client port — the LB Service port the chart renders (for kafka, the
// external listener port on the Strimzi-created bootstrap LB).
func Port(engine string) int {
	switch engine {
	case EnginePostgreSQL:
		return 5432
	case EngineMySQL, EngineMariaDB:
		return 3306
	case EngineValkey:
		return 6379
	case EngineFerretDB:
		return 27017
	case EngineOpenSearch:
		return 9200
	case EngineKafka:
		return 9094
	}
	return 0
}

// LBServiceNameFor is the tenant-facing LoadBalancer Service name per engine. Every chart-owned
// LB is `<id>-lb`; kafka is the exception — Strimzi creates the external listener's bootstrap
// Service itself as `<cluster>-kafka-external-bootstrap` (listener name "external";
// TODO-verify(drill) on Strimzi 1.1.0 — the supported contract is Kafka
// status.listeners[].bootstrapServers, adopt it if the name ever drifts).
func LBServiceNameFor(engine, dbID string) string {
	if engine == EngineKafka {
		return dbID + "-kafka-external-bootstrap"
	}
	return LBServiceName(dbID)
}

// URI assembles the engine's connection URI. Credentials are URL-escaped (generated passwords
// may carry reserved characters).
func URI(engine, user, pass, host string, port int, db string) string {
	u := url.UserPassword(user, pass).String()
	switch engine {
	case EnginePostgreSQL:
		return fmt.Sprintf("postgresql://%s@%s:%d/%s", u, host, port, db)
	case EngineMySQL, EngineMariaDB:
		return fmt.Sprintf("mysql://%s@%s:%d/%s", u, host, port, db)
	case EngineValkey:
		// The username matters now that customer ACL users exist: `redis://default:…` and a
		// customer user's URI differ only here.
		return fmt.Sprintf("redis://%s@%s:%d/0", u, host, port)
	case EngineFerretDB:
		return fmt.Sprintf("mongodb://%s@%s:%d/%s", u, host, port, db)
	case EngineOpenSearch:
		return fmt.Sprintf("https://%s@%s:%d", u, host, port)
	case EngineKafka:
		// Informational — Kafka clients take bootstrap host:port + SASL/SCRAM-SHA-512 creds.
		return fmt.Sprintf("kafka://%s@%s:%d", u, host, port)
	}
	return ""
}
