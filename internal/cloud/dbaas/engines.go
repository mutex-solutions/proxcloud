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
// in-place major, Percona smart update, mariadb rolling). Off for valkey (operator unpinned)
// and ferretdb (values.ferretdb.postgresImage is a pinned matched-pair with the frontend —
// bumping only engineVersion would split the pair; enable once the chart maps BOTH images off
// the version).
// SET_SSO (opensearch only) writes the values.opensearch.sso block — Dashboards OIDC against a
// PUBLIC IdP client (no client secret; values must never carry secrets). RESIZE_STORAGE stays
// off for opensearch until the operator's diskSize-change semantics are live-verified; RESTART
// is off for opensearch/kafka (no values-shaped restart knob — Strimzi rolls via a per-podset
// annotation, not the CR).
// SET_AUTOSCALE (all engines) opts into the surcharge-priced vertical autoscale tick
// (autoscale.go): CPU/RAM bounds work wherever RESIZE does; the disk leg additionally requires
// RESIZE_STORAGE, so it self-disables for valkey/opensearch.
var Capabilities = map[string]map[string]bool{
	EnginePostgreSQL: {"RESIZE": true, "RESIZE_STORAGE": true, "SCALE_REPLICAS": true, "RESTART": true, "RESET_PASSWORD": true, "UPGRADE": true, "SET_AUTOSCALE": true},
	EngineMySQL:      {"RESIZE": true, "RESIZE_STORAGE": true, "SCALE_REPLICAS": true, "RESTART": false, "RESET_PASSWORD": true, "UPGRADE": true, "SET_AUTOSCALE": true},
	EngineMariaDB:    {"RESIZE": true, "RESIZE_STORAGE": true, "SCALE_REPLICAS": true, "RESTART": true, "RESET_PASSWORD": true, "UPGRADE": true, "SET_AUTOSCALE": true},
	EngineValkey:     {"RESIZE": true, "RESIZE_STORAGE": false, "SCALE_REPLICAS": true, "RESTART": false, "RESET_PASSWORD": false, "UPGRADE": false, "SET_AUTOSCALE": true},
	EngineFerretDB:   {"RESIZE": true, "RESIZE_STORAGE": true, "SCALE_REPLICAS": true, "RESTART": true, "RESET_PASSWORD": true, "UPGRADE": false, "SET_AUTOSCALE": true},
	EngineOpenSearch: {"RESIZE": true, "RESIZE_STORAGE": false, "SCALE_REPLICAS": true, "RESTART": false, "RESET_PASSWORD": false, "UPGRADE": true, "SET_SSO": true, "SET_AUTOSCALE": true},
	EngineKafka:      {"RESIZE": true, "RESIZE_STORAGE": true, "SCALE_REPLICAS": true, "RESTART": false, "RESET_PASSWORD": true, "UPGRADE": true, "SET_AUTOSCALE": true},
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
//   - postgresql: CNPG-generated app-user secret `<id>-app` (superuser stays internal)
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
		return dbID + "-app", "username", "password", "dbname"
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
// derived from the database id (the KafkaUser name IS the SASL username).
func DefaultUser(engine, dbID string) string {
	switch engine {
	case EngineMySQL:
		return "root"
	case EngineMariaDB:
		return "app"
	case EngineKafka:
		return dbID + "-app"
	}
	return ""
}

// DefaultDB is the exposed database name when ConnectionSecret carries no dbKey. The mariadb
// chart template creates database "app" for the declarative app user; mysql root has no
// default schema; valkey has none by design.
func DefaultDB(engine string) string {
	if engine == EngineMariaDB {
		return "app"
	}
	return ""
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
		return fmt.Sprintf("redis://:%s@%s:%d/0", url.QueryEscape(pass), host, port)
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
