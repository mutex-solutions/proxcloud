package dbaas

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

// K8sAPI is the DB-cluster surface Service consumes — implemented by *kamajik8s.Client,
// fake-able in tests.
type K8sAPI interface {
	EnsureNamespace(ctx context.Context, name string, labels map[string]string) error
	GetNamespace(ctx context.Context, name string) (map[string]any, error)
	DeleteNamespace(ctx context.Context, name string) error
	ApplySecret(ctx context.Context, ns, name string, stringData map[string]string, labels, annotations map[string]string) error
	PatchSecretData(ctx context.Context, ns, name string, stringData map[string]string) error
	ApplyNetworkPolicy(ctx context.Context, np map[string]any) error
	GetSecretData(ctx context.Context, ns, name string) (map[string][]byte, error)
	ListSecrets(ctx context.Context, ns, labelSelector string) ([]map[string]any, error)
	ListSecretsAllNamespaces(ctx context.Context, labelSelector string) ([]map[string]any, error)
	DeleteSecret(ctx context.Context, ns, name string) error
	ApplyApplication(ctx context.Context, app map[string]any) error
	GetApplication(ctx context.Context, ns, name string) (map[string]any, error)
	ListApplications(ctx context.Context, ns, labelSelector string) ([]map[string]any, error)
	DeleteApplication(ctx context.Context, ns, name string) error
	GetService(ctx context.Context, ns, name string) (map[string]any, error)
	GetVPA(ctx context.Context, ns, name string) (map[string]any, error)
	ListCRs(ctx context.Context, group, version, plural, ns, labelSelector string) ([]map[string]any, error)
	ListPods(ctx context.Context, ns, labelSelector string) ([]map[string]any, error)
	PodLogs(ctx context.Context, ns, pod, container string, tailLines int) (string, error)
}

// defaultFinalizeGrace: a marker secret younger than this is never treated as an orphan. Closes
// the create-race: CreateDatabase applies the marker BEFORE the Application, so a concurrent
// finalize pass could otherwise see a fresh secret with no Application — the exact signature of
// a finished delete cascade — and reap the database's secrets mid-create.
const defaultFinalizeGrace = 30 * time.Minute

// Service drives one dbaas provider (one DB cluster). Built per external service via New
// (live) or NewWithAPI (tests).
type Service struct {
	api           K8sAPI
	cfg           Config
	serviceID     string
	finalizeGrace time.Duration
}

// New builds a live Service from the provider config.
func New(cfg Config, serviceID string) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	kc, err := kamajik8s.New(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("dbaas: db-cluster kubeconfig: %w", err)
	}
	return &Service{api: kc, cfg: cfg, serviceID: serviceID, finalizeGrace: defaultFinalizeGrace}, nil
}

// NewWithAPI builds a Service over a fake API (tests).
func NewWithAPI(api K8sAPI, cfg Config, serviceID string) *Service {
	return &Service{api: api, cfg: cfg, serviceID: serviceID, finalizeGrace: defaultFinalizeGrace}
}

// Config exposes the provider config (read-only use: engine catalog, chart pin, limits).
func (s *Service) Config() Config { return s.cfg }

// EnsureProjectNamespace creates/labels the project's namespace on the DB cluster and stamps
// the namespace-wide default-deny NetworkPolicy — the dbaas bootstrap leg (BootstrapDbaasOnto).
// The default-deny lives HERE, not in the chart: multiple releases share one pc-* namespace and
// would collide on its name, and the isolation promise must hold for any pod that ever lands in
// the namespace, not only chart-rendered ones. The chart's per-database policy then opens the
// engine-specific holes.
func (s *Service) EnsureProjectNamespace(ctx context.Context, projectID string) error {
	ns := NamespaceFor(projectID)
	if err := s.api.EnsureNamespace(ctx, ns, map[string]string{
		LabelProject:   projectID,
		LabelService:   s.serviceID,
		LabelManagedBy: ManagedByValue,
	}); err != nil {
		return err
	}
	return s.api.ApplyNetworkPolicy(ctx, map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":      "proxcloud-default-deny",
			"namespace": ns,
			"labels": map[string]any{
				LabelProject:   projectID,
				LabelService:   s.serviceID,
				LabelManagedBy: ManagedByValue,
			},
		},
		"spec": map[string]any{
			"podSelector": map[string]any{},
			"policyTypes": []any{"Ingress"},
		},
	})
}

// CreateDatabase provisions one database: marker secret FIRST (the durable record), then the
// proxcloud-owned auth secret for engines that need one, then the Application CR. Marker-first
// ordering is load-bearing: with the marker already applied, any half-finished create converges
// via FinalizeOrphans (the 30-minute grace guards the mid-create window). Returns the initial
// cached data payload (status PENDING until the first sync reads real health).
func (s *Service) CreateDatabase(ctx context.Context, spec DatabaseSpec) (map[string]any, error) {
	if err := spec.Validate(s.cfg); err != nil {
		return nil, err
	}
	ns := NamespaceFor(spec.ProjectID)
	if err := s.EnsureProjectNamespace(ctx, spec.ProjectID); err != nil {
		return nil, fmt.Errorf("dbaas: ensure namespace: %w", err)
	}
	// The marker secret is the only k8s-side object that outlives the Application, so the sweep
	// keys on it (kamaji cloud-credentials precedent).
	if err := s.api.ApplySecret(ctx, ns, MarkerSecretName(spec.ID), nil,
		map[string]string{LabelProject: spec.ProjectID, LabelService: s.serviceID, LabelManagedBy: ManagedByValue},
		nil); err != nil {
		return nil, fmt.Errorf("dbaas: apply marker: %w", err)
	}
	// App-credential secret for engines whose operator does not mint one (mariadb, valkey).
	// ProxCloud-owned and OUTSIDE the Application: a chart-generated password would re-roll on
	// every ArgoCD render, and values must never carry secrets.
	if NeedsAuthSecret(spec.Engine) {
		password, err := generatePassword()
		if err != nil {
			return nil, err
		}
		if err := s.api.ApplySecret(ctx, ns, AuthSecretName(spec.ID),
			map[string]string{"password": password},
			map[string]string{LabelProject: spec.ProjectID, LabelService: s.serviceID, LabelManagedBy: ManagedByValue},
			nil); err != nil {
			return nil, fmt.Errorf("dbaas: apply auth secret: %w", err)
		}
	}
	// Object-store credentials. The condition MUST mirror BuildValues' backup block: whenever
	// the values wire an object store, the Secret those CRs name has to exist — otherwise CNPG
	// fails every backup AND continuous WAL archiving with `secrets "<id>-backup-s3" not found`,
	// silently, from the moment the database comes up. A restoring database also reads the
	// object store during BOOTSTRAP, so this has to land before the Application does.
	if s.cfg.Backup.Enabled() && Capabilities[spec.Engine]["BACKUP"] {
		if err := s.applyBackupSecret(ctx, spec.ProjectID, spec.ID); err != nil {
			return nil, err
		}
	}
	values := BuildValues(s.cfg, spec)
	app := BuildApplication(s.cfg, spec, s.serviceID, s.cfg.ChartVersion, values)
	if err := s.api.ApplyApplication(ctx, app); err != nil {
		return nil, fmt.Errorf("dbaas: apply application: %w", err)
	}
	return databaseData(app, nil), nil
}

// DeleteDatabase removes the database: Application delete only (the resources-finalizer
// cascades the rendered chart, LB Service included). The marker secret deliberately STAYS —
// FinalizeOrphans (sync-driven) reaps it and the auth secret once the cascade has finished.
// Idempotent; ownership-guarded like kamaji.
func (s *Service) DeleteDatabase(ctx context.Context, projectID, dbID string) error {
	app, err := s.api.GetApplication(ctx, s.cfg.ArgoNamespace, dbID)
	if err != nil {
		return err
	}
	if app != nil {
		if !managedBy(app) {
			return fmt.Errorf("dbaas: database %s is not managed by proxcloud — refusing to delete", dbID)
		}
		if err := s.api.DeleteApplication(ctx, s.cfg.ArgoNamespace, dbID); err != nil {
			return fmt.Errorf("dbaas: delete application: %w", err)
		}
	}
	return nil
}

// FinalizeOrphans completes asynchronous database deletions for ONE project (teardown + tests);
// the periodic reaper is FinalizeAllOrphans. Returns how many markers still await the cascade.
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
// managed marker of this provider across all namespaces — so leftovers of projects whose
// proxcloud doc is already gone are still reaped — deletes finished-cascade markers (and auth
// secrets) and GCs emptied project namespaces. Returns the number of markers still awaiting
// their cascade.
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

// finalizeNamespace reaps finished-cascade database markers in one namespace. A marker is an
// orphan only when ALL of: it is older than the finalize grace window (create-race guard), a
// point-in-time GetApplication finds no Application, and the LB Service is gone (the cascade's
// last tenant-facing object). The marker is then reaped, and the auth secret with it.
func (s *Service) finalizeNamespace(ctx context.Context, ns, projectID string, secrets []map[string]any) (int, error) {
	var errs []error
	pending, reaped := 0, 0
	for _, sec := range secrets {
		name := digStr(sec, "metadata", "name")
		dbID, isMarker := strings.CutSuffix(name, markerSuffix)
		if !isMarker {
			continue
		}
		if created, err := time.Parse(time.RFC3339, digStr(sec, "metadata", "creationTimestamp")); err == nil {
			if time.Since(created) < s.finalizeGrace {
				pending++ // possibly mid-create — the Application may not be applied yet
				continue
			}
		}
		app, err := s.api.GetApplication(ctx, s.cfg.ArgoNamespace, dbID)
		if err != nil {
			return pending, err
		}
		if app != nil {
			if dig(app, "metadata", "deletionTimestamp") != nil {
				pending++ // delete cascade in flight
			}
			continue // database alive (or deleting) — its marker is in use
		}
		// The marker records no engine, so probe BOTH tenant-facing LB names: the chart-owned
		// `<id>-lb` and Strimzi's `<id>-kafka-external-bootstrap`. Either alive = the cascade is
		// still winding down.
		lbGone := true
		for _, svcName := range []string{LBServiceName(dbID), LBServiceNameFor(EngineKafka, dbID)} {
			if svc, err := s.api.GetService(ctx, ns, svcName); err != nil {
				return pending, err
			} else if svc != nil {
				lbGone = false
				break
			}
		}
		if !lbGone {
			pending++ // LB Service still winding down
			continue
		}
		// Auth secret FIRST, marker LAST: the marker is the only name the sweep keys on, so it
		// must survive any partial failure to stay the retry driver for both deletions —
		// reversed, a transient auth-delete failure would strand a live credential secret with
		// nothing ever revisiting it.
		if err := s.api.DeleteSecret(ctx, ns, AuthSecretName(dbID)); err != nil {
			errs = append(errs, fmt.Errorf("database %s: delete auth secret: %w", dbID, err))
			pending++
			continue
		}
		// The rest of the database's own secrets (backup credentials, customer-user passwords,
		// the BYO certificate): all `<id>-…`, all proxcloud-labelled, so they are in this very
		// listing. Without this they would outlive the database whenever the project still
		// holds a sibling and the namespace is not collected.
		if err := s.deleteSiblingSecrets(ctx, ns, dbID, name, secrets); err != nil {
			errs = append(errs, fmt.Errorf("database %s: %w", dbID, err))
			pending++
			continue
		}
		if err := s.api.DeleteSecret(ctx, ns, name); err != nil {
			errs = append(errs, fmt.Errorf("database %s: delete marker: %w", dbID, err))
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

// deleteSiblingSecrets removes every listed secret named `<dbID>-…` except the marker itself.
func (s *Service) deleteSiblingSecrets(ctx context.Context, ns, dbID, markerName string, secrets []map[string]any) error {
	for _, sec := range secrets {
		n := digStr(sec, "metadata", "name")
		if n == markerName || !strings.HasPrefix(n, dbID+"-") {
			continue
		}
		if err := s.api.DeleteSecret(ctx, ns, n); err != nil {
			return fmt.Errorf("delete secret %s: %w", n, err)
		}
	}
	return nil
}

// RemoveProjectNamespace deletes the namespace of a project being torn down, which its bootstrap made whether or not it
// ever held any databases. finalizeNamespace leaves that one alone on purpose, since a live project keeps it. The caller has
// seen FinalizeOrphans report nothing pending. An Application still there keeps the namespace.
func (s *Service) RemoveProjectNamespace(ctx context.Context, projectID string) error {
	apps, err := s.api.ListApplications(ctx, s.cfg.ArgoNamespace, LabelProject+"="+projectID+","+LabelManagedBy+"="+ManagedByValue)
	if err != nil || len(apps) > 0 {
		return err
	}
	return s.gcNamespace(ctx, NamespaceFor(projectID))
}

// gcNamespace deletes the project namespace once it demonstrably holds no database remnants —
// and only if proxcloud created it (ownership label on the namespace).
func (s *Service) gcNamespace(ctx context.Context, ns string) error {
	nsObj, err := s.api.GetNamespace(ctx, ns)
	if err != nil || nsObj == nil {
		return err
	}
	if !managedBy(nsObj) {
		return nil
	}
	return s.api.DeleteNamespace(ctx, ns)
}

// ConnInfo is the on-demand connection bundle GET_CONNECTION_INFO returns — read from the
// engine's secret + the LB Service, streamed to the caller, NEVER stored (kamaji kubeconfig
// discipline).
type ConnInfo struct {
	Engine   string `json:"engine"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	DBName   string `json:"dbname,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password"`
	URI      string `json:"uri"`
}

// ConnectionInfo assembles the database's connection bundle: engine off the live values,
// credentials off the engine's secret, host off the LB Service's ingress IP.
func (s *Service) ConnectionInfo(ctx context.Context, projectID, dbID string) (ConnInfo, error) {
	app, err := s.api.GetApplication(ctx, s.cfg.ArgoNamespace, dbID)
	if err != nil {
		return ConnInfo{}, err
	}
	if app == nil {
		return ConnInfo{}, fmt.Errorf("dbaas: database %s not found", dbID)
	}
	engine := digStr(app, "spec", "source", "helm", "valuesObject", "engine")
	ns := NamespaceFor(projectID)
	secretName, userKey, passKey, dbKey := ConnectionSecret(engine, dbID)
	if secretName == "" {
		return ConnInfo{}, fmt.Errorf("dbaas: database %s: unknown engine %q", dbID, engine)
	}
	data, err := s.api.GetSecretData(ctx, ns, secretName)
	if err != nil {
		return ConnInfo{}, err
	}
	// A postgresql database provisioned with superuserAccess off has no `<id>-superuser`
	// Secret; serve the account it was built with rather than telling a running database its
	// credentials are not ready.
	if data == nil {
		if pn, pu, pp, pd := PriorConnectionSecret(engine, dbID); pn != "" {
			if prior, perr := s.api.GetSecretData(ctx, ns, pn); perr == nil && prior != nil {
				data, userKey, passKey, dbKey = prior, pu, pp, pd
			}
		}
	}
	if data == nil {
		return ConnInfo{}, fmt.Errorf("dbaas: database %s: credentials not ready yet", dbID)
	}
	info := ConnInfo{
		Engine:   engine,
		Port:     Port(engine),
		Username: DefaultUser(engine, dbID),
		DBName:   DefaultDB(engine),
		Password: string(data[passKey]),
	}
	if userKey != "" {
		info.Username = string(data[userKey])
	}
	if dbKey != "" {
		info.DBName = string(data[dbKey])
	}
	if info.Password == "" {
		return ConnInfo{}, fmt.Errorf("dbaas: database %s: credentials not ready yet", dbID)
	}
	info.Host, err = s.lbHost(ctx, ns, LBServiceNameFor(engine, dbID))
	if err != nil {
		return ConnInfo{}, err
	}
	if info.Host == "" {
		return ConnInfo{}, fmt.Errorf("dbaas: database %s: endpoint not ready yet (load balancer still provisioning)", dbID)
	}
	// Same spelling the cached row uses (PublicHost) — one helper, so the databases list and
	// this panel can never disagree about a database's endpoint.
	info.Host = PublicHost(digStr(app, "spec", "source", "helm", "valuesObject", "opensearch", "customDomain"), info.Host)
	info.URI = URI(engine, info.Username, info.Password, info.Host, info.Port, info.DBName)
	return info, nil
}

// ResetPassword rotates the exposed account's password: server-generated, merge-patched onto
// the engine's connection secret; the operator reconciles the role from it (CNPG app secret,
// Percona system-users secret, mariadb passwordSecretKeyRef). A merge-patch, NEVER an SSA
// apply: CNPG mints its app secrets as kubernetes.io/basic-auth and Secret.type is immutable
// (ApplySecret's hardcoded Opaque would 422 every time), and a same-field-manager re-apply
// would retract the ownership labels proxcloud stamped on its own auth secrets. The patch also
// 404s when the operator has not minted the secret yet — surfaced as "not ready" rather than
// SSA-creating a conflicting secret under the operator. Returned ONCE, never stored.
func (s *Service) ResetPassword(ctx context.Context, projectID, dbID, engine string) (string, error) {
	secretName, _, passKey, _ := ConnectionSecret(engine, dbID)
	if secretName == "" || passKey == "" {
		return "", fmt.Errorf("dbaas: engine %q does not support password reset", engine)
	}
	password, err := generatePassword()
	if err != nil {
		return "", err
	}
	if err := s.api.PatchSecretData(ctx, NamespaceFor(projectID), secretName,
		map[string]string{passKey: password}); err != nil {
		if !kamajik8s.NotFound(err) {
			return "", fmt.Errorf("dbaas: rotate password: %w", err)
		}
		// Same fallback as ConnectionInfo: rotate the account the database actually has.
		pn, _, pp, _ := PriorConnectionSecret(engine, dbID)
		if pn == "" {
			return "", fmt.Errorf("dbaas: database %s: credentials not ready yet", dbID)
		}
		if err := s.api.PatchSecretData(ctx, NamespaceFor(projectID), pn,
			map[string]string{pp: password}); err != nil {
			if kamajik8s.NotFound(err) {
				return "", fmt.Errorf("dbaas: database %s: credentials not ready yet", dbID)
			}
			return "", fmt.Errorf("dbaas: rotate password: %w", err)
		}
	}
	return password, nil
}

// SetAccess reconciles a database's customer-managed logical databases and users to the given
// desired state. Declarative on purpose: the caller sends the whole list, so a dropped entry is
// a removal and the values document is always the truth — the same posture as every other dbaas
// mutation.
//
// Order is load-bearing. Secrets are written BEFORE the values patch (a User CR pointing at a
// Secret that does not exist yet wedges the operator) and deleted AFTER it (an operator still
// reconciling a removed user must not find its Secret gone mid-flight). Passwords for NEW users
// are generated here and returned ONCE, exactly like ResetPassword — nothing stores them.
func (s *Service) SetAccess(ctx context.Context, projectID, dbID, engine string, dbs []DBDatabase, users []DBUser, roles []OSRole) (map[string]string, error) {
	if err := ValidateAccess(engine, dbs, users, roles); err != nil {
		return nil, err
	}
	existing, err := s.accessUsernames(ctx, dbID)
	if err != nil {
		return nil, err
	}
	ns := NamespaceFor(projectID)
	created := map[string]string{}
	wanted := map[string]bool{}
	for _, u := range users {
		wanted[u.Name] = true
		if existing[u.Name] {
			continue
		}
		password, err := generatePassword()
		if err != nil {
			return nil, err
		}
		if err := s.api.ApplySecret(ctx, ns, UserSecretName(dbID, u.Name),
			map[string]string{"username": u.Name, "password": password},
			map[string]string{LabelProject: projectID, LabelService: s.serviceID, LabelManagedBy: ManagedByValue},
			nil); err != nil {
			return nil, fmt.Errorf("dbaas: apply user secret: %w", err)
		}
		created[u.Name] = password
	}
	if err := s.PatchDatabaseValues(ctx, dbID, func(values map[string]any) error {
		values["databases"] = databasesValues(dbs, users)
		values["users"] = usersValues(dbs, users)
		if engine == EngineOpenSearch {
			block, _ := values["opensearch"].(map[string]any)
			if block == nil {
				block = map[string]any{}
			}
			block["roles"] = osRolesValues(roles)
			values["opensearch"] = block
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// Best-effort: a leftover Secret is inert (nothing references it) and the namespace GC
	// reaps it with the database, so a failure here must not fail an applied change.
	for name := range existing {
		if !wanted[name] {
			_ = s.api.DeleteSecret(ctx, ns, UserSecretName(dbID, name))
		}
	}
	return created, nil
}

// SetIndexPolicies reconciles a database's OpenSearch retention policies (ISM). Declarative
// like SetAccess: the caller sends the whole list. No secrets involved, so this is a plain
// values patch — the chart expands each entry into an OpensearchISMPolicy state machine.
func (s *Service) SetIndexPolicies(ctx context.Context, dbID string, policies []OSIndexPolicy) error {
	if err := ValidateOSIndexPolicies(policies); err != nil {
		return err
	}
	return s.PatchDatabaseValues(ctx, dbID, func(values map[string]any) error {
		block, _ := values["opensearch"].(map[string]any)
		if block == nil {
			block = map[string]any{}
		}
		block["indexPolicies"] = osIndexPoliciesValues(policies)
		values["opensearch"] = block
		return nil
	})
}

// ResetUserPassword rotates a customer-created user's password. ProxCloud owns that Secret, so
// this is a plain re-apply — unlike the engine's own account (ResetPassword), whose Secret the
// operator mints and must be merge-patched.
func (s *Service) ResetUserPassword(ctx context.Context, projectID, dbID, username string) (string, error) {
	existing, err := s.accessUsernames(ctx, dbID)
	if err != nil {
		return "", err
	}
	if !existing[username] {
		return "", fmt.Errorf("user %q does not exist on this database", username)
	}
	password, err := generatePassword()
	if err != nil {
		return "", err
	}
	if err := s.api.ApplySecret(ctx, NamespaceFor(projectID), UserSecretName(dbID, username),
		map[string]string{"username": username, "password": password},
		map[string]string{LabelProject: projectID, LabelService: s.serviceID, LabelManagedBy: ManagedByValue},
		nil); err != nil {
		return "", fmt.Errorf("dbaas: rotate user password: %w", err)
	}
	return password, nil
}

// accessUsernames reads the users currently declared in the LIVE values — the same source the
// operator reconciles from, so "does this user exist" can never disagree with what is deployed.
func (s *Service) accessUsernames(ctx context.Context, dbID string) (map[string]bool, error) {
	app, err := s.api.GetApplication(ctx, s.cfg.ArgoNamespace, dbID)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, fmt.Errorf("dbaas: database %s not found", dbID)
	}
	out := map[string]bool{}
	list, _ := dig(app, "spec", "source", "helm", "valuesObject", "users").([]any)
	for _, raw := range list {
		if u, ok := raw.(map[string]any); ok {
			if name, _ := u["name"].(string); name != "" {
				out[name] = true
			}
		}
	}
	return out, nil
}

// Logs returns the tail of the database's own log, newest pod last. Every pinned engine writes
// its log to stdout, so this is a plain pods/log read — no sidecar, no shipper, and nothing
// proxcloud stores.
//
// The pod filter is the ownership boundary: one namespace holds every database in a project, so
// a selector that leaked would hand one customer another's log. PodSelectorFor knows which label
// each operator actually stamps cluster-wide — app.kubernetes.io/instance was assumed here and
// matched NOTHING for valkey (per-node values), opensearch and kafka (own cluster labels), so a
// healthy database's Logs tab returned an internal error.
func (s *Service) Logs(ctx context.Context, projectID, dbID, engine string, tailLines int) ([]map[string]any, error) {
	container := LogContainerFor(engine)
	if container == "" {
		return nil, fmt.Errorf("dbaas: engine %q has no readable log", engine)
	}
	ns := NamespaceFor(projectID)
	pods, err := s.api.ListPods(ctx, ns, PodSelectorFor(engine, dbID))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(pods))
	namePrefix := PodNamePrefixFor(engine, dbID)
	for _, pod := range pods {
		name := digStr(pod, "metadata", "name")
		if name == "" || !strings.HasPrefix(name, namePrefix) {
			continue
		}
		// A pod that has not started yet has no log; report it rather than failing the batch,
		// because during a rolling change that is the normal state of one of them.
		text, err := s.api.PodLogs(ctx, ns, name, container, tailLines)
		entry := map[string]any{"pod": name, "container": container}
		if err != nil {
			entry["error"] = err.Error()
		} else {
			entry["log"] = text
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("dbaas: database %s has no running pods yet", dbID)
	}
	return out, nil
}

// SetParameters writes a database's runtime configuration. Declarative like every other
// mutation: the caller sends the WHOLE desired set, so a parameter dropped from the map returns
// to the engine default rather than lingering.
//
// ProxCloud does not restart anything here. Each operator decides for itself whether a change is
// a reload or a roll (CNPG reloads a sighup GUC and rolls a postmaster one; Percona and mariadb
// hash the config and roll the StatefulSet; Strimzi applies dynamically-updatable broker keys
// through the Admin API with no restart at all). ParametersNeedRestart only exists so the UI can
// warn first.
func (s *Service) SetParameters(ctx context.Context, dbID, engine string, params map[string]string) error {
	if err := ValidateParameters(engine, params); err != nil {
		return err
	}
	block := ParamBlockFor(engine)
	if block == "" {
		return fmt.Errorf("dbaas: engine %q has no tunable settings", engine)
	}
	return s.PatchDatabaseValues(ctx, dbID, func(values map[string]any) error {
		cur, _ := values[block].(map[string]any)
		if cur == nil {
			cur = map[string]any{}
		}
		next := map[string]any{}
		for k, v := range params {
			next[k] = v
		}
		cur["parameters"] = next
		values[block] = cur
		return nil
	})
}

// SetBackup turns object-store backups on or off for one database and sets its schedule and
// retention. The credential Secret is written BEFORE the values flip and removed AFTER it — the
// same ordering discipline as the BYO certificate, and for the same reason: an operator that
// reconciles a backup CR pointing at a Secret which does not exist yet wedges, and one still
// draining a disabled schedule must not lose its credential mid-run.
func (s *Service) SetBackup(ctx context.Context, projectID, dbID string, spec BackupSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if !s.cfg.Backup.Enabled() {
		return fmt.Errorf("dbaas: this location has no backup object store configured")
	}
	// The object store stays wired either way. Unwiring it would remove the barman sidecar and
	// ROLL the whole cluster, and it would end continuous archiving — so "no schedule" means
	// exactly that: on-demand backups still work and point-in-time recovery still reaches back
	// as far as the archive goes.
	spec.Enabled = true
	if err := s.applyBackupSecret(ctx, projectID, dbID); err != nil {
		return err
	}
	return s.PatchDatabaseValues(ctx, dbID, func(values map[string]any) error {
		// Preserve any pending on-demand stamp: a schedule change must not cancel a backup the
		// customer started seconds earlier.
		prev, _ := values["backup"].(map[string]any)
		engine, _ := values["engine"].(string)
		next := backupValues(s.cfg, engine, dbID, spec)
		if runAt, _ := prev["runAt"].(string); runAt != "" {
			next["runAt"] = runAt
		}
		values["backup"] = next
		return nil
	})
}

// StampBackupRun adds an out-of-schedule backup to a values document a caller is ALREADY
// mutating, so a risky change and its safety backup land in ONE patch instead of racing two.
// No-op when backups are off — a change must never fail because the customer has no object
// store; the caller decides whether to say so.
//
// This does NOT hold the change until the backup finishes, and pretending otherwise would be
// the dangerous version: gating on completion would mean a stuck backup silently blocks every
// later change to the database. The real guarantee is continuous archiving — with WAL/binlog
// shipping on, recovery can land on the second BEFORE the change regardless of when the base
// backup completed. The extra base backup just shortens the replay.
func StampBackupRun(values map[string]any, stamp string) bool {
	block, _ := values["backup"].(map[string]any)
	if block == nil || block["enabled"] != true {
		return false
	}
	block["runAt"] = stamp
	values["backup"] = block
	return true
}

// TriggerBackup runs an out-of-schedule backup by stamping values.backup.runAt. Every engine
// template turns a CHANGED value into one extra backup (a new CNPG Backup CR named after the
// stamp, mariadb's schedule.onDemand identifier, a Percona backup CR), so "back up now" needs
// no imperative API against the DB cluster — it is the same declarative path as everything else
// and it survives a proxcloud restart mid-flight.
func (s *Service) TriggerBackup(ctx context.Context, dbID, stamp string) error {
	return s.PatchDatabaseValues(ctx, dbID, func(values map[string]any) error {
		block, _ := values["backup"].(map[string]any)
		if block == nil || block["enabled"] != true {
			return fmt.Errorf("dbaas: backups are not enabled for this database")
		}
		block["runAt"] = stamp
		values["backup"] = block
		return nil
	})
}

// ListBackups reports the backup objects the engine's operator has produced, newest first. Read
// straight off the DB cluster rather than cached: a backup list is small, read rarely, and a
// stale one is the kind of thing someone plans a restore around.
//
// Deliberately NOT filtered on the proxcloud managed-by label. Only the ON-DEMAND backups are ours
// to label — the chart renders those. The SCHEDULED ones are created by the engine's operator from
// the ScheduledBackup we render, and every operator stamps its own labels on that child instead of
// inheriting ours (CNPG: cnpg.io/cluster + cnpg.io/scheduled-backup, no passthrough). Requiring the
// label therefore hid every unattended backup a database has ever taken, which made the whole
// schedule decorative: the backups ran, retention consumed them, and the console could never offer
// one as a restore source.
//
// Ownership is the NAMESPACE plus the `<dbID>-` name prefix, which is the real test either way:
// the namespace is per project, ids are proxcloud-generated, and nothing outside proxcloud and the
// operators can write there.
func (s *Service) ListBackups(ctx context.Context, projectID, dbID, engine string) ([]map[string]any, error) {
	plural, group, version := BackupCRFor(engine)
	if plural == "" {
		return nil, fmt.Errorf("dbaas: engine %q does not support backups", engine)
	}
	items, err := s.api.ListCRs(ctx, group, version, plural, NamespaceFor(projectID), "")
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		// One namespace holds every database in the project, so a backup belonging to a sibling
		// must never appear here.
		name := digStr(it, "metadata", "name")
		if !strings.HasPrefix(name, dbID+"-") {
			continue
		}
		out = append(out, map[string]any{
			"name":       name,
			"phase":      backupPhase(it),
			"createdAt":  digStr(it, "metadata", "creationTimestamp"),
			"startedAt":  digStr(it, "status", "startedAt"),
			"finishedAt": digStr(it, "status", "stoppedAt"),
			"error":      digStr(it, "status", "error"),
		})
	}
	return out, nil
}

// backupPhase normalises the per-operator status field into one word for the UI.
func backupPhase(obj map[string]any) string {
	for _, path := range [][]string{{"status", "phase"}, {"status", "state"}} {
		if v := digStr(obj, path...); v != "" {
			return v
		}
	}
	return "UNKNOWN"
}

// SetCustomDomainTLS writes the proxcloud-owned BYO-certificate secret <id>-custom-tls (keys
// tls.crt/tls.key, plus ca.crt when a chain is supplied) that the chart mounts on opensearch's
// http layer and Dashboards. PEM material goes straight to the cluster — never into proxcloud
// storage, values, or logs. ProxCloud-owned like the auth secret, so a same-manager re-apply on
// rotation is safe.
func (s *Service) SetCustomDomainTLS(ctx context.Context, projectID, dbID, certPEM, keyPEM, caPEM string) error {
	data := map[string]string{"tls.crt": certPEM, "tls.key": keyPEM}
	if caPEM != "" {
		data["ca.crt"] = caPEM
	}
	return s.api.ApplySecret(ctx, NamespaceFor(projectID), CustomTLSSecretName(dbID), data,
		map[string]string{LabelProject: projectID, LabelService: s.serviceID, LabelManagedBy: ManagedByValue}, nil)
}

// DeleteCustomDomainTLS removes the BYO-certificate secret (custom domain turned off).
func (s *Service) DeleteCustomDomainTLS(ctx context.Context, projectID, dbID string) error {
	err := s.api.DeleteSecret(ctx, NamespaceFor(projectID), CustomTLSSecretName(dbID))
	if kamajik8s.NotFound(err) {
		return nil
	}
	return err
}

// applyBackupSecret writes the per-database object-store credential Secret with every key alias
// the engines expect (CNPG, Percona, mariadb — see BackupCredKeys). ProxCloud-owned and outside
// the Application, so values never carry the credentials.
func (s *Service) applyBackupSecret(ctx context.Context, projectID, dbID string) error {
	if err := s.api.ApplySecret(ctx, NamespaceFor(projectID), BackupSecretName(dbID), map[string]string{
		BackupCredKeys.CNPGAccess: s.cfg.Backup.AccessKey,
		BackupCredKeys.CNPGSecret: s.cfg.Backup.SecretKey,
		BackupCredKeys.AWSAccess:  s.cfg.Backup.AccessKey,
		BackupCredKeys.AWSSecret:  s.cfg.Backup.SecretKey,
	}, map[string]string{LabelProject: projectID, LabelService: s.serviceID, LabelManagedBy: ManagedByValue},
		nil); err != nil {
		return fmt.Errorf("dbaas: apply backup credentials: %w", err)
	}
	return nil
}

// PublicHost is how a database's endpoint is SPELLED for the customer: their own domain beats
// the raw LB address. Empty address in = empty out — the LB address stays the readiness signal
// AND the billing eligibility gate (internal/cloud/billingresource/databasecluster.go keys on a
// non-empty endpoint), so a name must never appear before the load balancer has an address. Both
// the cache (syncprovider) and the live read (ConnectionInfo) go through here.
func PublicHost(customDomain, ip string) string {
	if ip == "" {
		return ""
	}
	if customDomain != "" {
		return customDomain
	}
	return ip
}

// lbHost reads the ingress IP off the named LB Service ("" while still provisioning).
func (s *Service) lbHost(ctx context.Context, ns, svcName string) (string, error) {
	svc, err := s.api.GetService(ctx, ns, svcName)
	if err != nil || svc == nil {
		return "", err
	}
	ingress, _ := dig(svc, "status", "loadBalancer", "ingress").([]any)
	for _, raw := range ingress {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if ip, _ := entry["ip"].(string); ip != "" {
			return ip, nil
		}
		if host, _ := entry["hostname"].(string); host != "" {
			return host, nil
		}
	}
	return "", nil
}

// PatchDatabaseApp mutates the database's Application (read → guard ownership → mutate → full
// re-apply). The one reconcile path every post-create change goes through. Same SSA discipline
// as kamaji's PatchClusterApp: the Application was created via server-side apply by this same
// field manager, so a partial apply is an ownership retraction — every owned field absent from
// the patch is REMOVED and the api server rejects the result with 422 (or silently drops the
// resources-finalizer). Hence the full metadata + spec re-apply.
func (s *Service) PatchDatabaseApp(ctx context.Context, dbID string, mutate func(app map[string]any) error) error {
	app, err := s.api.GetApplication(ctx, s.cfg.ArgoNamespace, dbID)
	if err != nil {
		return err
	}
	if app == nil {
		return fmt.Errorf("dbaas: database %s not found", dbID)
	}
	if !managedBy(app) {
		return fmt.Errorf("dbaas: database %s is not managed by proxcloud — refusing to modify", dbID)
	}
	if err := mutate(app); err != nil {
		return err
	}
	meta := map[string]any{
		"name":      dbID,
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

// PatchDatabaseValues mutates the Application's helm values in place (RESIZE, SCALE_REPLICAS…):
// same chart pin, same full re-apply.
func (s *Service) PatchDatabaseValues(ctx context.Context, dbID string, mutate func(values map[string]any) error) error {
	return s.PatchDatabaseApp(ctx, dbID, func(app map[string]any) error {
		values, _ := dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
		if values == nil {
			return fmt.Errorf("dbaas: database %s: application carries no values", dbID)
		}
		return mutate(values)
	})
}

// SetChartVersion re-pins the database's Application onto a chart version (the "platform
// update"). ArgoCD re-renders the SAME desired state with the new chart.
func (s *Service) SetChartVersion(ctx context.Context, dbID, version string) error {
	if version == "" {
		return fmt.Errorf("dbaas: chart version is required")
	}
	return s.PatchDatabaseApp(ctx, dbID, func(app map[string]any) error {
		src, _ := dig(app, "spec", "source").(map[string]any)
		if src == nil {
			return fmt.Errorf("dbaas: database %s: application carries no source", dbID)
		}
		src["targetRevision"] = version
		return nil
	})
}

// DatabasePin is one managed database's chart pin — the admin bump surface's row.
type DatabasePin struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ProjectID    string `json:"projectId"`
	Engine       string `json:"engine"`
	ChartVersion string `json:"chartVersion"`
}

// ListDatabasePins lists every proxcloud-managed database on this provider with its chart pin
// (ownership-labelled Applications only).
func (s *Service) ListDatabasePins(ctx context.Context) ([]DatabasePin, error) {
	apps, err := s.api.ListApplications(ctx, s.cfg.ArgoNamespace, LabelService+"="+s.serviceID+","+LabelManagedBy+"="+ManagedByValue)
	if err != nil {
		return nil, err
	}
	out := make([]DatabasePin, 0, len(apps))
	for _, app := range apps {
		if dig(app, "metadata", "deletionTimestamp") != nil {
			continue
		}
		id := digStr(app, "metadata", "name")
		if id == "" {
			continue
		}
		out = append(out, DatabasePin{
			ID:           id,
			Name:         digStr(app, "metadata", "annotations", AnnotationDisplayName),
			ProjectID:    digStr(app, "metadata", "labels", LabelProject),
			Engine:       digStr(app, "spec", "source", "helm", "valuesObject", "engine"),
			ChartVersion: digStr(app, "spec", "source", "targetRevision"),
		})
	}
	return out, nil
}

// generatePassword mints a 24-char alphanumeric secret (~143 bits) — URI-safe without escaping.
func generatePassword() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, 24)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", fmt.Errorf("dbaas: generate password: %w", err)
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}
