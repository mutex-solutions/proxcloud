package dbaas

import (
	"fmt"
	"strings"
)

// values.go — the SINGLE source of truth for the `database-cluster` chart values contract
// (deploy/charts/database-cluster). Every key BuildValues emits is a key the chart reads;
// TestBuildValues pins all of them per engine, so a rename on either side is a test failure,
// not a silently ignored value (the kamaji values.go lesson). No credentials in values, EVER —
// the Application CR is readable by anyone with argocd read; passwords ride in Secrets only.

// BuildValues renders the full chart values for one database. The generated values live inline
// on the Application (spec.source.helm.valuesObject) — the DB cluster is the state store,
// proxcloud stays stateless about desired spec.
func BuildValues(cfg Config, spec DatabaseSpec) map[string]any {
	storage := map[string]any{"sizeGi": spec.StorageGiB}
	if spec.StorageClass != "" {
		storage["storageClassName"] = spec.StorageClass
	}
	cidrs := make([]any, 0, len(spec.AllowedCIDRs))
	for _, c := range spec.AllowedCIDRs {
		cidrs = append(cidrs, c)
	}
	// The LB address comes from MetalLB in a services subnet tenants reach over their normal
	// egress — no tenant network is involved. Isolation is loadBalancerSourceRanges.
	network := map[string]any{"allowedCidrs": cidrs}
	if cfg.LBAddressPool != "" {
		network["addressPool"] = cfg.LBAddressPool
	}
	if spec.ReadEndpoint {
		network["readEndpoint"] = true
	}
	values := map[string]any{
		"engine":        spec.Engine,
		"engineVersion": spec.Version,
		"instances":     spec.Replicas,
		"resources":     map[string]any{"cpu": spec.CPU, "memoryGi": spec.MemoryGiB},
		"storage":       storage,
		"network":       network,
		"proxcloud": map[string]any{
			"projectId":   spec.ProjectID,
			"resourceId":  spec.ID,
			"displayName": spec.DisplayName,
		},
	}
	// Pod placement (chart `scheduling`): the zone spread key plus the optional dedicated node
	// pool. Omitted when the provider configures none, so the chart defaults stand
	// (zoneKey topology.kubernetes.io/zone, schedule anywhere).
	if placement := placementValues(cfg.ZoneKey, cfg.NodeSelector, cfg.Tolerations); placement != nil {
		values["scheduling"] = placement
	}
	// Emitted for opensearch only, and only the keys that are on: the chart defaults everything
	// off, and SET_SSO/SET_CUSTOM_DOMAIN patch this same block later — unconditional keys here
	// would fight those patches.
	if spec.Engine == EngineOpenSearch {
		osBlock := map[string]any{}
		// SSO rides Dashboards (the chart reads the sso block only under dashboards.enabled),
		// so asking for SSO at create implies them — same rule SET_SSO applies later.
		if spec.DashboardsEnabled || len(spec.SSO) > 0 {
			osBlock["dashboards"] = map[string]any{"enabled": true}
		}
		if len(spec.SSO) > 0 {
			sso := map[string]any{"enabled": true}
			for k, v := range spec.SSO {
				sso[k] = v
			}
			osBlock["sso"] = sso
		}
		if len(osBlock) > 0 {
			values["opensearch"] = osBlock
		}
	}
	if spec.RestoreFrom != nil {
		values["restore"] = restoreValues(cfg, spec.ID, *spec.RestoreFrom)
	}
	// Backups are wired at CREATE whenever the location has an object store, the way every
	// managed-database service does it. This is not a preference — it is what stops "turn on
	// backups" from being a destructive act later: on CloudNativePG the barman plugin runs as a
	// SIDECAR, so wiring it injects a container into every instance pod and the operator must
	// roll the whole cluster. Doing that at create costs nothing; doing it to a live database
	// restarts it. Continuous archiving from day one is also what makes recovery to a point in
	// time possible at all.
	//
	// What the customer toggles later is the SCHEDULE, not this.
	if cfg.Backup.Enabled() && Capabilities[spec.Engine]["BACKUP"] {
		values["backup"] = backupValues(cfg, spec.Engine, spec.ID, BackupSpec{
			Enabled:       true,
			Schedule:      DefaultBackupSchedule,
			RetentionDays: DefaultBackupRetentionDays,
		})
	}
	return values
}

// restoreValues renders the recovery source. The new database reads the SOURCE's folder in the
// same bucket, so only the source id travels — the path is derived identically on both sides.
// Credentials come from the new database's own backup Secret, which the create path writes
// before the Application exists precisely so the bootstrap can read them.
func restoreValues(cfg Config, dbID string, src RestoreSource) map[string]any {
	out := map[string]any{
		"sourceId":    src.SourceID,
		"credsSecret": BackupSecretName(dbID),
		"s3": map[string]any{
			"endpoint":  cfg.Backup.Endpoint,
			"bucket":    cfg.Backup.Bucket,
			"prefix":    cfg.Backup.Prefix,
			"pathStyle": cfg.Backup.PathStyle,
		},
	}
	if cfg.Backup.Region != "" {
		out["s3"].(map[string]any)["region"] = cfg.Backup.Region
	}
	if cfg.Backup.CACertSecret != "" {
		out["s3"].(map[string]any)["caCertSecret"] = cfg.Backup.CACertSecret
	}
	if src.TargetTime != "" {
		out["targetTime"] = src.TargetTime
	}
	if src.BackupName != "" {
		out["backupName"] = src.BackupName
	}
	if src.PITR {
		out["pitr"] = true
	}
	return out
}

// backupValues renders the chart's backup block. The object store comes from the PROVIDER, the
// posture from the customer — they never pick a bucket. Credentials are absent by construction:
// only the NAME of the Secret proxcloud wrote travels, because every operator takes them as a
// SecretKeySelector and the Application is argocd-readable.
//
// The schedule stays the ONE canonical 5-field cron here. CNPG's ScheduledBackup wants six
// fields (leading seconds) and the chart prepends it; keeping both formats in values would
// eventually put a 5-field string in a 6-field parser, which runs at the wrong hour silently.
func backupValues(cfg Config, engine, dbID string, spec BackupSpec) map[string]any {
	out := map[string]any{"enabled": spec.Enabled}
	if !spec.Enabled {
		return out
	}
	if spec.Schedule != "" {
		out["schedule"] = spec.Schedule
		if incr := IncrementalSchedule(engine, spec.Schedule); incr != "" {
			out["incrementalSchedule"] = incr
		}
	}
	if spec.RetentionDays > 0 {
		out["retentionDays"] = spec.RetentionDays
		out["keepBackups"] = keepBackups(spec.Schedule, spec.RetentionDays)
	}
	s3 := map[string]any{
		"endpoint":    cfg.Backup.Endpoint,
		"bucket":      cfg.Backup.Bucket,
		"pathStyle":   cfg.Backup.PathStyle,
		"credsSecret": BackupSecretName(dbID),
	}
	for k, v := range map[string]string{
		"region":       cfg.Backup.Region,
		"prefix":       cfg.Backup.Prefix,
		"caCertSecret": cfg.Backup.CACertSecret,
	} {
		if v != "" {
			s3[k] = v
		}
	}
	out["s3"] = s3
	return out
}

// IncrementalSchedule is the cron for mysql's INCREMENTAL runs: the days of the week the base
// backup does not run. "" means no incremental pass, which is the right answer for every other
// engine and for a base that already runs daily or oftener.
//
// Percona is the only operator here that can take an incremental base backup — CloudNativePG has
// no such concept and mariadb-operator does not expose one — and mysql is the engine that most
// needs it, because restoring from a week-old base means replaying a week of binlogs. A weekly
// full plus daily increments keeps both the object store and the restore window small.
//
// Retention is safe by construction rather than by care: Percona applies retention to the CHAIN,
// so pruning a base takes its increments with it and can never orphan one. Its own docs are
// blunt that the reverse is unsupported ("specifying the retention policy for increments is not
// supported"), which is why the chart puts `keep` on the full entry only.
func IncrementalSchedule(engine, base string) string {
	if engine != EngineMySQL {
		return ""
	}
	f := strings.Fields(base)
	// Only a base pinned to ONE weekday leaves whole days free. Anything else either has no gap
	// (daily, hourly) or no clean complement (day-of-month, a list of weekdays), and an
	// increment firing in the same minute as its base would chain onto the PREVIOUS week's full.
	if len(f) != 5 || f[2] != "*" || f[3] != "*" || len(f[4]) != 1 || f[4][0] < '0' || f[4][0] > '6' {
		return ""
	}
	days := make([]string, 0, 6)
	for c := byte('0'); c <= '6'; c++ {
		if c != f[4][0] {
			days = append(days, string(c))
		}
	}
	return fmt.Sprintf("%s %s * * %s", f[0], f[1], strings.Join(days, ","))
}

// keepBackups converts a retention in DAYS into the number of BACKUPS to keep, which is what
// Percona's `keep` field counts — the only engine whose retention is expressed that way.
//
// Only the weekly shape is converted, and deliberately so. Keeping too many backups costs money;
// keeping too few loses data. So anything this cannot read confidently falls through to the days
// value, which over-keeps for a sub-daily schedule rather than pruning a backup someone still
// needs. Without the conversion a 30-day retention on the weekly default would keep thirty full
// copies — seven months of them.
func keepBackups(schedule string, retentionDays int) int {
	f := strings.Fields(schedule)
	if len(f) == 5 && f[1] != "*" && f[4] != "*" {
		if n := retentionDays / 7; n > 0 {
			return n
		}
		return 1
	}
	return retentionDays
}

// databasesValues / usersValues render the customer-managed access lists into the values
// contract the chart's per-engine templates read (postgresql/mariadb databases + roles,
// opensearch users + role bindings). NEVER a password — only the NAME of the Secret proxcloud
// wrote, which the engine CR references.
func databasesValues(dbs []DBDatabase, users []DBUser) []any {
	owners := effectiveOwners(dbs, users)
	out := make([]any, 0, len(dbs))
	for _, d := range dbs {
		entry := map[string]any{"name": d.Name}
		if o := owners[d.Name]; o != "" {
			entry["owner"] = o
		}
		out = append(out, entry)
	}
	return out
}

// effectiveOwners fills in the owner of every database that did not name one: the FIRST user
// that asked for it. Load-bearing for postgres, where "user may use database" is expressed as
// membership of the database's owner role — leaving an owner blank would hand the database to
// the engine's own app role and silently grant the customer's users nothing at all. A database
// nobody listed keeps a blank owner and falls back to the app user in the chart.
func effectiveOwners(dbs []DBDatabase, users []DBUser) map[string]string {
	out := make(map[string]string, len(dbs))
	for _, d := range dbs {
		out[d.Name] = d.Owner
	}
	for _, u := range users {
		for _, name := range u.Databases {
			if cur, known := out[name]; known && cur == "" {
				out[name] = u.Name
			}
		}
	}
	return out
}

// osRolesValues renders customer-defined roles. The chart turns each into an OpensearchRole CR
// whose NAME is the role name OpenSearch sees (prefixed, like users — one namespace holds every
// database in the project), so `role` echoes the effective name back for display.
func osRolesValues(roles []OSRole) []any {
	out := make([]any, 0, len(roles))
	for _, r := range roles {
		patterns := make([]any, 0, len(r.IndexPatterns))
		for _, p := range r.IndexPatterns {
			patterns = append(patterns, p)
		}
		actions := make([]any, 0, len(r.Actions))
		for _, a := range r.Actions {
			actions = append(actions, a)
		}
		out = append(out, map[string]any{
			"name":          r.Name,
			"indexPatterns": patterns,
			"actions":       actions,
		})
	}
	return out
}

// osIndexPoliciesValues renders retention policies. Only the customer-facing knobs travel; the
// chart expands them into the real ISM state machine.
func osIndexPoliciesValues(policies []OSIndexPolicy) []any {
	out := make([]any, 0, len(policies))
	for _, p := range policies {
		patterns := make([]any, 0, len(p.IndexPatterns))
		for _, pat := range p.IndexPatterns {
			patterns = append(patterns, pat)
		}
		entry := map[string]any{
			"name":            p.Name,
			"indexPatterns":   patterns,
			"deleteAfterDays": p.DeleteAfterDays,
		}
		if p.RolloverGiB > 0 {
			entry["rolloverGiB"] = p.RolloverGiB
		}
		if p.RolloverDays > 0 {
			entry["rolloverDays"] = p.RolloverDays
		}
		out = append(out, entry)
	}
	return out
}

func usersValues(dbs []DBDatabase, users []DBUser) []any {
	// A postgres role reaches a database by being a member of that database's owner role, so
	// each user's grants become inRoles on the owners of the databases it listed.
	owner := effectiveOwners(dbs, users)
	out := make([]any, 0, len(users))
	for _, u := range users {
		entry := map[string]any{"name": u.Name}
		if len(u.Databases) > 0 {
			names := make([]any, 0, len(u.Databases))
			owners := make([]any, 0, len(u.Databases))
			seen := map[string]bool{}
			for _, d := range u.Databases {
				names = append(names, d)
				if o := owner[d]; o != "" && o != u.Name && !seen[o] {
					seen[o] = true
					owners = append(owners, o)
				}
			}
			entry["databases"] = names
			if len(owners) > 0 {
				entry["inRoles"] = owners
			}
		}
		if len(u.Roles) > 0 {
			roles := make([]any, 0, len(u.Roles))
			for _, r := range u.Roles {
				roles = append(roles, r)
			}
			entry["roles"] = roles
		}
		out = append(out, entry)
	}
	return out
}

// placementValues renders the chart's `scheduling` block (zoneKey + nodeSelector + tolerations)
// from the provider config. nil when none is configured, so the key stays off the values and
// the chart's defaults stand (zoneKey topology.kubernetes.io/zone, schedule anywhere).
func placementValues(zoneKey string, nodeSelector map[string]string, tolerations []map[string]any) map[string]any {
	out := map[string]any{}
	if zoneKey != "" {
		out["zoneKey"] = zoneKey
	}
	if len(nodeSelector) > 0 {
		sel := map[string]any{}
		for k, v := range nodeSelector {
			sel[k] = v
		}
		out["nodeSelector"] = sel
	}
	if len(tolerations) > 0 {
		list := make([]any, 0, len(tolerations))
		for _, t := range tolerations {
			list = append(list, t)
		}
		out["tolerations"] = list
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
