package dbaas

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Tests for the day-2 surface ported with the DBaaS actions: customer access, logs, runtime
// parameters, object-store backups, restore into a new database.

// TestPodSelectorFor pins the per-engine pod ownership label — the derivation twin of the chart's
// networkpolicy.yaml podSelector switch. app.kubernetes.io/instance matched NOTHING for the three
// operators that stamp their own cluster label, which surfaced as issue #170: a healthy valkey's
// Logs tab returned an internal error because the pod list came back empty.
func TestPodSelectorFor(t *testing.T) {
	cases := map[string]string{
		EnginePostgreSQL: "app.kubernetes.io/instance=std-1",
		EngineMySQL:      "app.kubernetes.io/instance=std-1",
		EngineMariaDB:    "app.kubernetes.io/instance=std-1",
		EngineFerretDB:   "app.kubernetes.io/instance=std-1",
		EngineValkey:     "valkey.io/cluster=std-1",
		EngineOpenSearch: "opensearch.org/opensearch-cluster=std-1",
		EngineKafka:      "strimzi.io/cluster=std-1",
	}
	for engine, want := range cases {
		if got := PodSelectorFor(engine, "std-1"); got != want {
			t.Errorf("%s: selector %q, want %q", engine, got, want)
		}
	}
}

// TestLogs drives the log read end to end against pods named and labelled EXACTLY the way each
// operator does it. The valkey pod is the operator's real shape (v0.4.0): the workload name is
// `valkey-<cluster>-<shard>-<node>` (its own resourcePrefix — it does NOT start with the
// database id), app.kubernetes.io/instance carries the PER-NODE value, and the main container
// is `server`. Any of those three assumed wrong made the Logs tab of a healthy valkey fail.
func TestLogs(t *testing.T) {
	api := newFakeAPI()
	s := NewWithAPI(api, testConfig(), "svc-dbaas")
	ns := NamespaceFor("p1")
	seed := func(name string, labels map[string]any) {
		api.pods[key(ns, name)] = map[string]any{
			"metadata": map[string]any{"name": name, "labels": labels},
		}
	}
	seed("valkey-std-v-0-0-0", map[string]any{
		"valkey.io/cluster":          "std-v",
		"app.kubernetes.io/instance": "std-v-0-0", // per-node, NOT the cluster id
	})
	seed("std-p-1", map[string]any{"app.kubernetes.io/instance": "std-p"})
	// A sibling database in the same namespace must never leak into the read.
	seed("std-x-1", map[string]any{"app.kubernetes.io/instance": "std-x"})

	logs, err := s.Logs(context.Background(), "p1", "std-v", EngineValkey, 100)
	if err != nil {
		t.Fatalf("valkey logs: %v", err)
	}
	if len(logs) != 1 || logs[0]["pod"] != "valkey-std-v-0-0-0" || logs[0]["container"] != "server" {
		t.Fatalf("valkey logs = %v", logs)
	}
	logs, err = s.Logs(context.Background(), "p1", "std-p", EnginePostgreSQL, 100)
	if err != nil || len(logs) != 1 || logs[0]["pod"] != "std-p-1" {
		t.Fatalf("postgres logs = %v (%v)", logs, err)
	}
}

// TestValidateAccess pins the identifier gate. This is a SECURITY test, not a UX one:
// mariadb-operator interpolates these names straight into SQL, so anything that gets past
// ValidateAccess reaches a SQL parser unescaped.
func TestValidateAccess(t *testing.T) {
	ok := []DBUser{{Name: "alice", Databases: []string{"orders"}}}
	okDBs := []DBDatabase{{Name: "orders", Owner: "alice"}}
	if err := ValidateAccess(EnginePostgreSQL, okDBs, ok, nil); err != nil {
		t.Fatalf("valid access rejected: %v", err)
	}
	bad := map[string]struct {
		dbs   []DBDatabase
		users []DBUser
	}{
		"sql injection in user":     {nil, []DBUser{{Name: "alice; DROP TABLE x"}}},
		"sql injection in database": {[]DBDatabase{{Name: "a`; DROP"}}, nil},
		"quote in name":             {nil, []DBUser{{Name: "a'b"}}},
		"backslash in name":         {nil, []DBUser{{Name: `a\b`}}},
		"upper case":                {nil, []DBUser{{Name: "Alice"}}},
		"leading digit":             {nil, []DBUser{{Name: "1alice"}}},
		"empty":                     {nil, []DBUser{{Name: ""}}},
		"too long":                  {nil, []DBUser{{Name: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}},
		"reserved user":             {nil, []DBUser{{Name: "postgres"}}},
		"reserved app user":         {nil, []DBUser{{Name: "app"}}},
		"reserved database":         {[]DBDatabase{{Name: "postgres"}}, nil},
		"duplicate user":            {nil, []DBUser{{Name: "alice"}, {Name: "alice"}}},
		"duplicate database":        {[]DBDatabase{{Name: "orders"}, {Name: "orders"}}, nil},
		"owner not a listed user":   {[]DBDatabase{{Name: "orders", Owner: "ghost"}}, nil},
		"grant on unknown database": {nil, []DBUser{{Name: "alice", Databases: []string{"ghost"}}}},
	}
	for name, tc := range bad {
		if err := ValidateAccess(EnginePostgreSQL, tc.dbs, tc.users, nil); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	// OpenSearch roles are an allowlist — an unknown role is a silent privilege grant otherwise.
	if err := ValidateAccess(EngineOpenSearch, nil, []DBUser{{Name: "alice", Roles: []string{"readall"}}}, nil); err != nil {
		t.Errorf("built-in role rejected: %v", err)
	}
	if err := ValidateAccess(EngineOpenSearch, nil, []DBUser{{Name: "alice", Roles: []string{"superuser"}}}, nil); err == nil {
		t.Error("unknown opensearch role must be rejected")
	}
	// valkey is users-only: ACL entries on the ValkeyCluster, no logical databases to declare or
	// grant, and `default` is the engine's own account (the connection-panel credential).
	if err := ValidateAccess(EngineValkey, nil, []DBUser{{Name: "cache_writer"}}, nil); err != nil {
		t.Errorf("valkey user rejected: %v", err)
	}
	if err := ValidateAccess(EngineValkey, []DBDatabase{{Name: "orders"}}, nil, nil); err == nil {
		t.Error("valkey must reject logical databases")
	}
	if err := ValidateAccess(EngineValkey, nil, []DBUser{{Name: "alice", Databases: []string{"orders"}}}, nil); err == nil {
		t.Error("valkey must reject database grants")
	}
	if err := ValidateAccess(EngineValkey, nil, []DBUser{{Name: "default"}}, nil); err == nil {
		t.Error("the default valkey ACL user must be reserved")
	}
	// The '_'->'-' mapping is injective per ident but NOT across the '-'-joined grant pair:
	// a_b×c and a×b_c derive one object name (mariadb's Grant CR) — two CRs with the same
	// identity wedge the sync, so the list is refused up front. Same-pair duplicates too.
	collide := []DBDatabase{{Name: "c"}, {Name: "b_c"}}
	if err := ValidateAccess(EngineMariaDB, collide,
		[]DBUser{{Name: "a_b", Databases: []string{"c"}}, {Name: "a", Databases: []string{"b_c"}}}, nil); err == nil {
		t.Error("colliding derived grant names must be rejected")
	}
	if err := ValidateAccess(EngineMariaDB, []DBDatabase{{Name: "c"}},
		[]DBUser{{Name: "a", Databases: []string{"c", "c"}}}, nil); err == nil {
		t.Error("a duplicated grant must be rejected")
	}
	if err := ValidateAccess(EngineMariaDB, collide,
		[]DBUser{{Name: "a_b", Databases: []string{"c"}}, {Name: "a", Databases: []string{"c"}}}, nil); err != nil {
		t.Errorf("distinct derived grant names rejected: %v", err)
	}
}

// TestCapabilityGateKeys pins that every handler-gated action is declared by the engines that
// serve it — SET_INDEX_POLICIES was gated but declared by NO engine, which killed opensearch
// retention policies end-to-end (the handler 400'd before its dispatch could run).
func TestCapabilityGateKeys(t *testing.T) {
	if !Capabilities[EngineOpenSearch]["SET_INDEX_POLICIES"] {
		t.Error("opensearch must declare SET_INDEX_POLICIES")
	}
	if !Capabilities[EngineValkey]["MANAGE_ACCESS"] {
		t.Error("valkey must declare MANAGE_ACCESS")
	}
	for engine, caps := range Capabilities {
		if caps["SET_INDEX_POLICIES"] && engine != EngineOpenSearch {
			t.Errorf("%s: index policies are an opensearch surface", engine)
		}
	}
}

// TestSetAccess drives the whole reconcile: new users get a Secret and a one-time password,
// the values document carries names only, and a user dropped from the list loses its Secret.
func TestSetAccess(t *testing.T) {
	api := newFakeAPI()
	cfg := testConfig()
	svc := NewWithAPI(api, cfg, "svc-1")
	spec := testSpec(EnginePostgreSQL, "17")
	if _, err := svc.CreateDatabase(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	ns := NamespaceFor(spec.ProjectID)

	created, err := svc.SetAccess(context.Background(), spec.ProjectID, spec.ID, EnginePostgreSQL,
		[]DBDatabase{{Name: "orders", Owner: "alice"}},
		[]DBUser{{Name: "alice", Databases: []string{"orders"}}, {Name: "bob", Databases: []string{"orders"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || created["alice"] == "" || created["bob"] == "" {
		t.Fatalf("both new users must come back with a password once, got %d", len(created))
	}
	for _, u := range []string{"alice", "bob"} {
		data, _ := api.GetSecretData(context.Background(), ns, UserSecretName(spec.ID, u))
		if string(data["password"]) != created[u] {
			t.Errorf("%s: secret does not hold the returned password", u)
		}
	}
	app, _ := api.GetApplication(context.Background(), cfg.ArgoNamespace, spec.ID)
	values, _ := dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
	if len(values["users"].([]any)) != 2 || len(values["databases"].([]any)) != 1 {
		t.Fatalf("values do not carry the access lists: %v", values["users"])
	}
	// Passwords must never reach the Application — it is argocd-readable.
	if strings.Contains(fmt.Sprint(values), created["alice"]) {
		t.Fatal("a password leaked into values")
	}
	// bob reaches orders through its owner alice: postgres has no per-database grant.
	first, _ := values["users"].([]any)[0].(map[string]any)
	second, _ := values["users"].([]any)[1].(map[string]any)
	byName := map[string]map[string]any{first["name"].(string): first, second["name"].(string): second}
	if _, has := byName["alice"]["inRoles"]; has {
		t.Error("the owner must not be granted membership of itself")
	}
	if roles, _ := byName["bob"]["inRoles"].([]any); len(roles) != 1 || roles[0] != "alice" {
		t.Errorf("bob.inRoles = %v, want [alice]", byName["bob"]["inRoles"])
	}

	// A database declared with NO owner must still be usable: the first user that asked for it
	// becomes the owner, otherwise postgres hands it to the app role and the grant is a no-op.
	if _, err := svc.SetAccess(context.Background(), spec.ProjectID, spec.ID, EnginePostgreSQL,
		[]DBDatabase{{Name: "logs"}},
		[]DBUser{{Name: "alice", Databases: []string{"logs"}}, {Name: "bob", Databases: []string{"logs"}}}, nil); err != nil {
		t.Fatal(err)
	}
	app, _ = api.GetApplication(context.Background(), cfg.ArgoNamespace, spec.ID)
	values, _ = dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
	logs, _ := values["databases"].([]any)[0].(map[string]any)
	if logs["owner"] != "alice" {
		t.Errorf("owner-less database must default to its first user, got %v", logs["owner"])
	}

	// Re-declaring without bob removes bob's Secret and keeps alice's password stable.
	alicePassword := created["alice"]
	created2, err := svc.SetAccess(context.Background(), spec.ProjectID, spec.ID, EnginePostgreSQL,
		[]DBDatabase{{Name: "orders", Owner: "alice"}}, []DBUser{{Name: "alice", Databases: []string{"orders"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(created2) != 0 {
		t.Errorf("an existing user must not be re-issued a password, got %v", created2)
	}
	if data, _ := api.GetSecretData(context.Background(), ns, UserSecretName(spec.ID, "alice")); string(data["password"]) != alicePassword {
		t.Error("alice's password must survive an unrelated change")
	}
	if data, _ := api.GetSecretData(context.Background(), ns, UserSecretName(spec.ID, "bob")); data != nil {
		t.Error("a removed user must lose its secret")
	}

	// Rotation is scoped to the named user and returns the new password once.
	rotated, err := svc.ResetUserPassword(context.Background(), spec.ProjectID, spec.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if rotated == alicePassword {
		t.Error("rotation must change the password")
	}
	if _, err := svc.ResetUserPassword(context.Background(), spec.ProjectID, spec.ID, "bob"); err == nil {
		t.Error("rotating a user that does not exist must fail")
	}
	// A rejected list must not write anything: bob stays gone.
	if _, err := svc.SetAccess(context.Background(), spec.ProjectID, spec.ID, EnginePostgreSQL, nil,
		[]DBUser{{Name: "bob"}, {Name: "DROP TABLE"}}, nil); err == nil {
		t.Fatal("an invalid list must be rejected")
	}
	if data, _ := api.GetSecretData(context.Background(), ns, UserSecretName(spec.ID, "bob")); data != nil {
		t.Error("a rejected list must not create secrets")
	}

	// Underscore idents are legal SQL names; their SECRET must land under the k8s-safe mapped
	// name while the values keep the real identifier (issue #168's exact shape).
	created3, err := svc.SetAccess(context.Background(), spec.ProjectID, spec.ID, EnginePostgreSQL,
		[]DBDatabase{{Name: "keycloak_stag", Owner: "keycloak_stag"}},
		[]DBUser{{Name: "keycloak_stag", Databases: []string{"keycloak_stag"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if created3["keycloak_stag"] == "" {
		t.Fatal("underscore user must be created")
	}
	if data, _ := api.GetSecretData(context.Background(), ns, spec.ID+"-u-keycloak-stag"); string(data["password"]) != created3["keycloak_stag"] {
		t.Error("the user secret must live under the '_'->'-' mapped name")
	}
	app, _ = api.GetApplication(context.Background(), cfg.ArgoNamespace, spec.ID)
	values, _ = dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
	u0, _ := values["users"].([]any)[0].(map[string]any)
	if u0["name"] != "keycloak_stag" {
		t.Errorf("values must keep the real identifier, got %v", u0["name"])
	}
}

// TestSetAccessValkey pins the users-only surface: an ACL user gets its Secret and lands in
// values by name; a database list is refused before anything is written.
func TestSetAccessValkey(t *testing.T) {
	api := newFakeAPI()
	cfg := testConfig()
	svc := NewWithAPI(api, cfg, "svc-1")
	spec := testSpec(EngineValkey, cfg.Engines[EngineValkey].Default)
	spec.BetaAck = true
	if _, err := svc.CreateDatabase(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	created, err := svc.SetAccess(context.Background(), spec.ProjectID, spec.ID, EngineValkey,
		nil, []DBUser{{Name: "cache_writer"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if created["cache_writer"] == "" {
		t.Fatal("new valkey user must come back with a password once")
	}
	ns := NamespaceFor(spec.ProjectID)
	if data, _ := api.GetSecretData(context.Background(), ns, UserSecretName(spec.ID, "cache_writer")); string(data["password"]) != created["cache_writer"] {
		t.Error("valkey user secret missing or wrong")
	}
	app, _ := api.GetApplication(context.Background(), cfg.ArgoNamespace, spec.ID)
	values, _ := dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
	users, _ := values["users"].([]any)
	if len(users) != 1 || users[0].(map[string]any)["name"] != "cache_writer" {
		t.Fatalf("values users = %v", values["users"])
	}
	if _, err := svc.SetAccess(context.Background(), spec.ProjectID, spec.ID, EngineValkey,
		[]DBDatabase{{Name: "orders"}}, nil, nil); err == nil {
		t.Fatal("valkey must reject logical databases")
	}
}

// TestBackupSpecValidate pins the cron arity gate. This matters more than it looks: CNPG's
// ScheduledBackup takes SIX fields and reads a five-field string as seconds-first, so it runs at
// the wrong time instead of failing. One canonical arity is stored and the chart converts.
func TestBackupSpecValidate(t *testing.T) {
	for _, ok := range []string{"", "0 2 * * *", "*/15 * * * *", "0 2 * * 0", "0 0 1 1 *"} {
		if err := (BackupSpec{Schedule: ok, RetentionDays: 30}).Validate(); err != nil {
			t.Errorf("schedule %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"0 0 2 * * *", "0 2 * *", "@daily", "0 2 * * * *", "not a cron"} {
		if err := (BackupSpec{Schedule: bad}).Validate(); err == nil {
			t.Errorf("schedule %q must be rejected (arity/format)", bad)
		}
	}
	if err := (BackupSpec{RetentionDays: -1}).Validate(); err == nil {
		t.Error("negative retention must be rejected")
	}
	if err := (BackupSpec{RetentionDays: 4000}).Validate(); err == nil {
		t.Error("absurd retention must be rejected")
	}
}

// TestBackupDefaultsAtCreate pins the two things that make a new database actually recoverable:
// a base-backup SCHEDULE exists from the start (continuous archiving alone has nothing to replay
// onto), and it is the weekly one, because every run is a full copy.
func TestBackupDefaultsAtCreate(t *testing.T) {
	cfg := testConfig()
	cfg.Backup = BackupConfig{
		Endpoint: "https://s3.example", Bucket: "backups",
		AccessKey: "AKIA-TEST", SecretKey: "s3cr3t-key", PathStyle: true,
	}
	for engine, version := range map[string]string{
		EnginePostgreSQL: "17", EngineMySQL: "8.4", EngineMariaDB: "11.4", EngineFerretDB: "2",
	} {
		values := BuildValues(cfg, testSpec(engine, version))
		block, _ := values["backup"].(map[string]any)
		if block["enabled"] != true {
			t.Fatalf("%s: store not wired at create: %v", engine, block)
		}
		if block["schedule"] != DefaultBackupSchedule {
			t.Errorf("%s: schedule = %v, want the weekly default %q — without one there is no base "+
				"backup and the archived WAL/binlog is unrestorable", engine, block["schedule"], DefaultBackupSchedule)
		}
		if block["retentionDays"] != DefaultBackupRetentionDays {
			t.Errorf("%s: retentionDays = %v", engine, block["retentionDays"])
		}
		// 30 days of a WEEKLY schedule is four backups, not thirty.
		if block["keepBackups"] != 4 {
			t.Errorf("%s: keepBackups = %v, want 4", engine, block["keepBackups"])
		}
		// Only Percona can chain increments; the others must not carry the key at all.
		incr := block["incrementalSchedule"]
		if engine == EngineMySQL {
			if incr != "0 2 * * 1,2,3,4,5,6" {
				t.Errorf("mysql: incrementalSchedule = %v", incr)
			}
		} else if incr != nil {
			t.Errorf("%s: must not get an incremental schedule, got %v", engine, incr)
		}
	}
	// An engine without the BACKUP capability gets no block at all.
	if _, ok := BuildValues(cfg, testSpec(EngineKafka, "4.0"))["backup"]; ok {
		t.Error("kafka must not get a backup block")
	}
}

func TestIncrementalScheduleAndKeep(t *testing.T) {
	// Only a base pinned to ONE weekday leaves whole days free to fill.
	if got := IncrementalSchedule(EngineMySQL, "0 2 * * 0"); got != "0 2 * * 1,2,3,4,5,6" {
		t.Errorf("weekly base -> %q", got)
	}
	if got := IncrementalSchedule(EngineMySQL, "30 4 * * 3"); got != "30 4 * * 0,1,2,4,5,6" {
		t.Errorf("midweek base -> %q", got)
	}
	// No gap, no clean complement, or not Percona: no second entry. An increment colliding with
	// its base would chain onto the PREVIOUS week's full.
	for _, base := range []string{"", "0 2 * * *", "0 * * * *", "0 2 1 * 0", "0 2 * * 1,3", "0 2 * *"} {
		if got := IncrementalSchedule(EngineMySQL, base); got != "" {
			t.Errorf("base %q -> %q, want none", base, got)
		}
	}
	if got := IncrementalSchedule(EnginePostgreSQL, "0 2 * * 0"); got != "" {
		t.Errorf("postgres has no incremental base backup, got %q", got)
	}

	// keepBackups converts days->count for the weekly shape only; anything it cannot read
	// confidently over-keeps rather than pruning a backup someone still needs.
	for _, c := range []struct {
		schedule string
		days     int
		want     int
	}{
		{"0 2 * * 0", 30, 4}, {"0 2 * * 0", 7, 1}, {"0 2 * * 0", 3, 1},
		{"0 2 * * *", 30, 30}, {"0 * * * *", 30, 30}, {"", 30, 30},
	} {
		if got := keepBackups(c.schedule, c.days); got != c.want {
			t.Errorf("keepBackups(%q, %d) = %d, want %d", c.schedule, c.days, got, c.want)
		}
	}
}

// TestSetBackup covers the whole posture change: credentials land in a Secret (never values),
// the values carry the provider's store, and disabling takes the credential away again.
func TestSetBackup(t *testing.T) {
	api := newFakeAPI()
	cfg := testConfig()
	cfg.Backup = BackupConfig{
		Endpoint: "https://s3.example", Bucket: "backups", Prefix: "az1",
		AccessKey: "AKIA-TEST", SecretKey: "s3cr3t-key", PathStyle: true,
	}
	svc := NewWithAPI(api, cfg, "svc-1")
	spec := testSpec(EnginePostgreSQL, "17")
	if _, err := svc.CreateDatabase(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	ns := NamespaceFor(spec.ProjectID)

	if err := svc.SetBackup(context.Background(), spec.ProjectID, spec.ID,
		BackupSpec{Enabled: true, Schedule: "0 2 * * *", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	data, _ := api.GetSecretData(context.Background(), ns, BackupSecretName(spec.ID))
	if string(data[BackupCredKeys.CNPGAccess]) != "AKIA-TEST" || string(data[BackupCredKeys.AWSSecret]) != "s3cr3t-key" {
		t.Fatalf("credential secret missing an alias: %v", data)
	}
	app, _ := api.GetApplication(context.Background(), cfg.ArgoNamespace, spec.ID)
	values, _ := dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
	block, _ := values["backup"].(map[string]any)
	if block["enabled"] != true || block["schedule"] != "0 2 * * *" || block["retentionDays"] != 30 {
		t.Fatalf("backup values = %v", block)
	}
	s3, _ := block["s3"].(map[string]any)
	if s3["bucket"] != "backups" || s3["credsSecret"] != BackupSecretName(spec.ID) {
		t.Fatalf("s3 block = %v", s3)
	}
	// The whole point: no key material may reach the Application.
	if strings.Contains(fmt.Sprint(values), "s3cr3t-key") || strings.Contains(fmt.Sprint(values), "AKIA-TEST") {
		t.Fatal("backup credentials leaked into values")
	}

	// On-demand stamps a value the chart turns into one extra run.
	if err := svc.TriggerBackup(context.Background(), spec.ID, "20260803-101500"); err != nil {
		t.Fatal(err)
	}
	app, _ = api.GetApplication(context.Background(), cfg.ArgoNamespace, spec.ID)
	values, _ = dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
	if got := dig(values, "backup", "runAt"); got != "20260803-101500" {
		t.Errorf("runAt = %v", got)
	}

	// Clearing the SCHEDULE must not unwire the object store: doing that would pull the barman
	// sidecar out of every instance pod and roll the whole cluster, and it would end continuous
	// archiving. On-demand backups and PITR keep working with no schedule.
	if err := svc.SetBackup(context.Background(), spec.ProjectID, spec.ID, BackupSpec{}); err != nil {
		t.Fatal(err)
	}
	if data, _ := api.GetSecretData(context.Background(), ns, BackupSecretName(spec.ID)); data == nil {
		t.Error("the credential secret must survive a schedule change")
	}
	app, _ = api.GetApplication(context.Background(), cfg.ArgoNamespace, spec.ID)
	values, _ = dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
	if got := dig(values, "backup", "enabled"); got != true {
		t.Errorf("backup.enabled = %v, want it to stay true", got)
	}
	if got := dig(values, "backup", "schedule"); got != nil {
		t.Errorf("no schedule must mean no schedule key, got %v", got)
	}
	// On-demand still works without a schedule — that is the whole point of the split.
	if err := svc.TriggerBackup(context.Background(), spec.ID, "20260804-120000"); err != nil {
		t.Errorf("on-demand backup must work without a schedule: %v", err)
	}
	// And a pending on-demand run survives a later schedule change.
	if err := svc.SetBackup(context.Background(), spec.ProjectID, spec.ID,
		BackupSpec{Schedule: "0 3 * * *", RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	app, _ = api.GetApplication(context.Background(), cfg.ArgoNamespace, spec.ID)
	values, _ = dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
	if got := dig(values, "backup", "runAt"); got != "20260804-120000" {
		t.Errorf("a schedule change must not cancel a backup already started, runAt = %v", got)
	}

	// A provider with no object store has no backup surface at all.
	noStore := NewWithAPI(newFakeAPI(), testConfig(), "svc-2")
	if err := noStore.SetBackup(context.Background(), spec.ProjectID, spec.ID, BackupSpec{Enabled: true}); err == nil {
		t.Error("a location without an object store must refuse backup settings")
	}
	// Creating with a store wires backups from the start, so nothing has to be turned on later.
	atCreate := BuildValues(cfg, testSpec(EnginePostgreSQL, "17"))
	if got := dig(atCreate, "backup", "enabled"); got != true {
		t.Errorf("a new database must have its object store wired at create, got %v", got)
	}
	if got := dig(BuildValues(testConfig(), testSpec(EnginePostgreSQL, "17")), "backup"); got != nil {
		t.Errorf("without a provider store there must be no backup block, got %v", got)
	}
}

// TestListBackups pins the ownership filter: one namespace holds every database in a project,
// so a sibling's backups must never show up in this database's history.
func TestListBackups(t *testing.T) {
	api := newFakeAPI()
	cfg := testConfig()
	svc := NewWithAPI(api, cfg, "svc-1")
	ns := NamespaceFor("p1")
	mine := map[string]any{
		"metadata": map[string]any{
			"name": "std-mine-ondemand-1", "namespace": ns,
			"labels":            map[string]any{LabelManagedBy: ManagedByValue},
			"creationTimestamp": "2026-08-03T10:00:00Z",
		},
		"status": map[string]any{"phase": "completed"},
	}
	sibling := map[string]any{
		"metadata": map[string]any{
			"name": "std-other-ondemand-1", "namespace": ns,
			"labels": map[string]any{LabelManagedBy: ManagedByValue},
		},
		"status": map[string]any{"phase": "completed"},
	}
	// A SCHEDULED backup: created by the engine's operator from our ScheduledBackup, so it carries
	// the operator's labels and NOT ours. It is still this database's backup and must be listed —
	// filtering on the proxcloud label hid every unattended backup and left the schedule decorative.
	scheduled := map[string]any{
		"metadata": map[string]any{
			"name": "std-mine-schedule-20260805165026", "namespace": ns,
			"labels":            map[string]any{"cnpg.io/cluster": "std-mine", "cnpg.io/scheduled-backup": "std-mine-schedule"},
			"creationTimestamp": "2026-08-05T16:50:26Z",
		},
		"status": map[string]any{"phase": "completed"},
	}
	api.crs["backups"] = map[string]map[string]any{
		key(ns, "std-mine-ondemand-1"):              mine,
		key(ns, "std-other-ondemand-1"):             sibling,
		key(ns, "std-mine-schedule-20260805165026"): scheduled,
	}
	got, err := svc.ListBackups(context.Background(), "p1", "std-mine", EnginePostgreSQL)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, b := range got {
		names[b["name"].(string)] = b["phase"].(string)
	}
	if len(got) != 2 || names["std-mine-ondemand-1"] != "completed" || names["std-mine-schedule-20260805165026"] != "completed" {
		t.Fatalf("list = %v, want this database's on-demand AND scheduled backups only", got)
	}
	if _, err := svc.ListBackups(context.Background(), "p1", "std-mine", EngineValkey); err == nil {
		t.Error("an engine with no backup CR must report that, not an empty list")
	}
}

// TestRestoreFrom pins the recovery contract: values name the SOURCE's folder, the credential
// Secret exists before the Application (the bootstrap reads S3 immediately), and engines whose
// restore targets a running cluster are refused rather than half-wired.
func TestRestoreFrom(t *testing.T) {
	api := newFakeAPI()
	cfg := testConfig()
	cfg.Backup = BackupConfig{
		Endpoint: "https://s3.example", Bucket: "backups", Prefix: "az1",
		AccessKey: "AK", SecretKey: "SK", PathStyle: true,
	}
	svc := NewWithAPI(api, cfg, "svc-1")
	spec := testSpec(EnginePostgreSQL, "17")
	spec.RestoreFrom = &RestoreSource{SourceID: "std-source01", TargetTime: "2026-08-03T10:00:00Z"}
	if _, err := svc.CreateDatabase(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	// Ordering is the whole point: the credential must exist by the time the Application does.
	secretAt, appAt := -1, -1
	for i, op := range api.ops {
		if op == "secret:"+BackupSecretName(spec.ID) {
			secretAt = i
		}
		if op == "app:"+spec.ID {
			appAt = i
		}
	}
	if secretAt < 0 || appAt < 0 || secretAt > appAt {
		t.Fatalf("restore credentials must be applied before the Application: %v", api.ops)
	}
	app, _ := api.GetApplication(context.Background(), cfg.ArgoNamespace, spec.ID)
	values, _ := dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
	restore, _ := values["restore"].(map[string]any)
	if restore["sourceId"] != "std-source01" || restore["targetTime"] != "2026-08-03T10:00:00Z" {
		t.Fatalf("restore values = %v", restore)
	}
	if restore["credsSecret"] != BackupSecretName(spec.ID) {
		t.Errorf("restore must read through this database's own credential secret, got %v", restore["credsSecret"])
	}
	if strings.Contains(fmt.Sprint(values), "SK") && strings.Contains(fmt.Sprint(values), "AK") {
		t.Error("restore credentials leaked into values")
	}

	// A normal create carries no restore block at all.
	plain := BuildValues(cfg, testSpec(EnginePostgreSQL, "17"))
	if _, has := plain["restore"]; has {
		t.Error("a non-restoring database must not carry a restore block")
	}

	// mysql backs up but cannot bootstrap from a backup — refuse rather than half-wire it.
	my := testSpec(EngineMySQL, "8.4")
	my.RestoreFrom = &RestoreSource{SourceID: "std-source01"}
	if err := my.Validate(cfg); err == nil {
		t.Error("mysql restore-into-new-database must be refused")
	}
	// Malformed sources are rejected before anything is written.
	bad := testSpec(EnginePostgreSQL, "17")
	bad.RestoreFrom = &RestoreSource{SourceID: "not-an-id"}
	if err := bad.Validate(cfg); err == nil {
		t.Error("a source id that is not a database id must be rejected")
	}
	bad.RestoreFrom = &RestoreSource{SourceID: "std-source01", TargetTime: "yesterday"}
	if err := bad.Validate(cfg); err == nil {
		t.Error("a non-RFC3339 target time must be rejected")
	}
}

// TestStampBackupRun pins the safety-backup helper: it rides an existing values patch, and it
// is a NO-OP when backups are off — a risky change must never fail because the customer has no
// object store configured.
func TestStampBackupRun(t *testing.T) {
	off := map[string]any{}
	if StampBackupRun(off, "20260804-090000") {
		t.Error("no backup block: must report that nothing was taken")
	}
	if _, has := off["backup"]; has {
		t.Error("no backup block: must not invent one")
	}
	disabled := map[string]any{"backup": map[string]any{"enabled": false}}
	if StampBackupRun(disabled, "20260804-090000") {
		t.Error("backups disabled: must report that nothing was taken")
	}
	if _, has := disabled["backup"].(map[string]any)["runAt"]; has {
		t.Error("backups disabled: must not stamp a run")
	}
	on := map[string]any{"backup": map[string]any{"enabled": true, "schedule": "0 2 * * *"}}
	if !StampBackupRun(on, "20260804-090000") {
		t.Fatal("backups on: must take one")
	}
	block := on["backup"].(map[string]any)
	if block["runAt"] != "20260804-090000" {
		t.Errorf("runAt = %v", block["runAt"])
	}
	if block["schedule"] != "0 2 * * *" {
		t.Error("the rest of the backup posture must survive")
	}
}

// A postgres database created before the superuser switch has no <id>-superuser Secret. It is
// serving traffic, so the connection panel must show the account it actually has instead of
// "credentials not ready", and RESET_PASSWORD must rotate that same account.
func TestConnectionInfoFallsBackToAppRole(t *testing.T) {
	api := newFakeAPI()
	cfg := testConfig()
	svc := NewWithAPI(api, cfg, "svc-1")
	spec := testSpec(EnginePostgreSQL, "17")
	ctx := context.Background()
	if _, err := svc.CreateDatabase(ctx, spec); err != nil {
		t.Fatal(err)
	}
	ns := NamespaceFor(spec.ProjectID)
	// Only the legacy secret exists — exactly the shape of a pre-switch database. Registered in
	// both maps because the fake mirrors the real API: a patch 404s when the object is absent.
	api.secrets[key(ns, spec.ID+"-app")] = map[string]any{
		"metadata": map[string]any{"name": spec.ID + "-app", "namespace": ns},
	}
	api.secretData[key(ns, spec.ID+"-app")] = map[string][]byte{
		"username": []byte("app"), "password": []byte("legacy-pw"), "dbname": []byte("app"),
	}
	api.services[key(ns, LBServiceName(spec.ID))] = map[string]any{
		"status": map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": "192.0.2.9"}}}},
	}

	info, err := svc.ConnectionInfo(ctx, spec.ProjectID, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Username != "app" || info.Password != "legacy-pw" || info.DBName != "app" {
		t.Fatalf("fallback conn info = %+v", info)
	}

	pw, err := svc.ResetPassword(ctx, spec.ProjectID, spec.ID, EnginePostgreSQL)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(api.secretData[key(ns, spec.ID+"-app")]["password"]); got != pw {
		t.Fatalf("reset must rotate the legacy secret: %q vs %q", got, pw)
	}
}

// The cached row must carry the backup posture. It did not, and the cost was not a wrong label:
// the console builds its restore-source list from `backup.enabled`, so with the key absent no
// database was ever offered as a recovery source and restore-from-backup could not be reached
// from the UI at all.
func TestDatabaseDataCarriesBackupPosture(t *testing.T) {
	cfg := testConfig()
	cfg.Backup = BackupConfig{Endpoint: "https://s3.example", Bucket: "b", AccessKey: "a", SecretKey: "s"}
	spec := testSpec(EnginePostgreSQL, "17")
	values := BuildValues(cfg, spec)
	app := BuildApplication(cfg, spec, "svc-1", cfg.ChartVersion, values)

	d, _ := databaseData(app, nil)["database"].(map[string]any)
	block, ok := d["backup"].(map[string]any)
	if !ok {
		t.Fatalf("cached row has no backup block: %v", d["backup"])
	}
	if block["enabled"] != true {
		t.Errorf("backup.enabled = %v, want true (the store is wired at create)", block["enabled"])
	}
	if block["schedule"] != DefaultBackupSchedule {
		t.Errorf("backup.schedule = %v, want %q", block["schedule"], DefaultBackupSchedule)
	}
	if block["retentionDays"] != DefaultBackupRetentionDays {
		t.Errorf("backup.retentionDays = %v, want %d", block["retentionDays"], DefaultBackupRetentionDays)
	}

	// An engine the platform cannot back up carries no block rather than a false one.
	valkey := testSpec(EngineValkey, "9")
	vApp := BuildApplication(cfg, valkey, "svc-1", cfg.ChartVersion, BuildValues(cfg, valkey))
	vd, _ := databaseData(vApp, nil)["database"].(map[string]any)
	if _, has := vd["backup"]; has {
		t.Errorf("valkey must not carry a backup block: %v", vd["backup"])
	}
}

// A database whose desired state never reached the cluster must not be shown as ready. ArgoCD
// computes health over the resources it can see and has no health check for most operator CRs, so
// an engine CR the API server rejected — or one the operator marked failed — leaves the app
// Healthy while nothing runs. Live-caught: valkey (UsersACLError) and opensearch (admission
// webhook refusal) both reported `ready` with a published endpoint and zero pods.
func TestDatabaseStatusNotReadyWhenOutOfSync(t *testing.T) {
	app := func(health, sync string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"name": "std-1"},
			"spec": map[string]any{"source": map[string]any{"helm": map[string]any{
				"valuesObject": map[string]any{"engine": EngineValkey, "engineVersion": "7.2"},
			}}},
			"status": map[string]any{
				"health": map[string]any{"status": health},
				"sync":   map[string]any{"status": sync},
			},
		}
	}
	for _, tc := range []struct {
		health, sync, want string
	}{
		{"Healthy", "Synced", "READY"},
		{"Healthy", "OutOfSync", "PROGRESSING"},
		{"Progressing", "OutOfSync", "PROGRESSING"},
		// A real failure keeps its own word — OutOfSync must not soften Degraded into
		// "still working on it".
		{"Degraded", "OutOfSync", "DEGRADED"},
	} {
		db, _ := databaseDataWithHost(app(tc.health, tc.sync), "192.0.2.5", "")["database"].(map[string]any)
		got := db["status"]
		if got != tc.want {
			t.Errorf("health=%s sync=%s → status=%v, want %s", tc.health, tc.sync, got, tc.want)
		}
	}
}
