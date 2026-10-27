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
}

// defaultFinalizeGrace: a net-share marker secret younger than this is never treated as an
// orphan. Closes the create-race: CreateDatabase applies the marker BEFORE the Application, so
// a concurrent finalize pass could otherwise see a fresh secret with no Application — the exact
// signature of a finished delete cascade — and revoke the network share mid-create.
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

// CreateDatabase provisions one database: net-share marker secret FIRST (the durable
// neutron-RBAC record), then ensureShare (the caller's neutron RBAC create), then the
// proxcloud-owned auth secret for engines that need one, then the Application CR. Marker-first
// ordering is load-bearing: a crash after the RBAC create but before the marker would leave a
// share no sweep can ever find — with the marker already applied, any half-finished create
// converges via FinalizeOrphans (the 30-minute grace guards the mid-create window, and a revoke
// of a never-created share is "already gone"). Returns the initial cached data payload (status
// PENDING until the first sync reads real health).
func (s *Service) CreateDatabase(ctx context.Context, spec DatabaseSpec, share NetShare, ensureShare func(context.Context) error) (map[string]any, error) {
	if err := spec.Validate(s.cfg); err != nil {
		return nil, err
	}
	ns := NamespaceFor(spec.ProjectID)
	if err := s.EnsureProjectNamespace(ctx, spec.ProjectID); err != nil {
		return nil, fmt.Errorf("dbaas: ensure namespace: %w", err)
	}
	// The annotations are the durable revocation record — the marker secret is the only k8s-side
	// object that outlives the Application, so the record rides on it (kamaji appcred precedent).
	if err := s.api.ApplySecret(ctx, ns, NetShareSecretName(spec.ID),
		map[string]string{"network-id": share.NetworkID},
		map[string]string{LabelProject: spec.ProjectID, LabelService: s.serviceID, LabelManagedBy: ManagedByValue},
		map[string]string{
			AnnotationNetworkID: share.NetworkID,
			AnnotationSubnetID:  share.SubnetID,
			AnnotationOSService: share.OSServiceID,
			AnnotationOSProject: share.OSProjectID,
			AnnotationOSRegion:  share.OSRegion,
		}); err != nil {
		return nil, fmt.Errorf("dbaas: apply net-share marker: %w", err)
	}
	if ensureShare != nil {
		if err := ensureShare(ctx); err != nil {
			// The share was never (or not verifiably) created — reap our marker best-effort; a
			// leftover one converges via the sweep ("already gone" revoke, then reap).
			_ = s.api.DeleteSecret(ctx, ns, NetShareSecretName(spec.ID))
			return nil, err
		}
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
	values := BuildValues(s.cfg, spec)
	app := BuildApplication(s.cfg, spec, s.serviceID, s.cfg.ChartVersion, values)
	if err := s.api.ApplyApplication(ctx, app); err != nil {
		return nil, fmt.Errorf("dbaas: apply application: %w", err)
	}
	return databaseData(app, nil), nil
}

// DeleteDatabase removes the database: Application delete only (the resources-finalizer
// cascades the rendered chart; deleting the LB Service makes the OCCM tear down the Octavia LB
// and its port on the tenant subnet). The net-share marker secret deliberately STAYS — the
// neutron RBAC policy cannot be revoked while that port exists, so FinalizeOrphans
// (sync-driven) revokes it and reaps the secret once the cascade has finished. Idempotent;
// ownership-guarded like kamaji.
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

// RevokeNetShareResolver revokes one neutron RBAC network share recorded on a marker secret.
// osRegion names the region whose neutron holds the share (recorded at create; "" on legacy
// records → the resolver picks a fallback). Contract is FAIL-CLOSED: any error keeps the secret
// — the only revocation record — for a later pass. This package deliberately never builds
// OpenStack clients; the caller resolves the recorded service id to an admin client.
type RevokeNetShareResolver func(ctx context.Context, osServiceID, osProjectID, osRegion, networkID string) error

// FinalizeOrphans completes asynchronous database deletions for ONE project (teardown + tests);
// the periodic reaper is FinalizeAllOrphans. Returns how many markers still await the cascade.
func (s *Service) FinalizeOrphans(ctx context.Context, projectID string, revoke RevokeNetShareResolver) (int, error) {
	ns := NamespaceFor(projectID)
	sel := LabelProject + "=" + projectID + "," + LabelManagedBy + "=" + ManagedByValue
	secrets, err := s.api.ListSecrets(ctx, ns, sel)
	if err != nil {
		return 0, err
	}
	return s.finalizeNamespace(ctx, ns, projectID, secrets, revoke)
}

// FinalizeAllOrphans is the service-level orphan sweep, run once per sync cycle: it scans EVERY
// managed net-share marker of this provider across all namespaces — so leftovers of projects
// whose proxcloud doc is already gone are still reaped — revokes recorded network shares, deletes
// finished-cascade markers and GCs emptied project namespaces. Returns the number of markers
// still awaiting their cascade.
func (s *Service) FinalizeAllOrphans(ctx context.Context, revoke RevokeNetShareResolver) (int, error) {
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
		p, err := s.finalizeNamespace(ctx, ns, projectID, secs, revoke)
		pending += p
		if err != nil {
			errs = append(errs, err)
		}
	}
	return pending, errors.Join(errs...)
}

// finalizeNamespace reaps finished-cascade database markers in one namespace. A marker is an
// orphan only when ALL of: it is older than the finalize grace window (create-race guard), a
// point-in-time GetApplication finds no Application, and the LB Service is gone — that Service
// is the last thing holding an Octavia port on the tenant subnet, and revoking the network
// share while the port exists 409s (the normal first-pass outcome while the amphora winds
// down). The share is revoked only when no LIVE sibling database of the project still uses the
// same network; either way the marker is then reaped, and the auth secret with it.
func (s *Service) finalizeNamespace(ctx context.Context, ns, projectID string, secrets []map[string]any, revoke RevokeNetShareResolver) (int, error) {
	var errs []error
	pending, reaped := 0, 0
	for _, sec := range secrets {
		name := digStr(sec, "metadata", "name")
		dbID, isMarker := strings.CutSuffix(name, netShareSuffix)
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
		// `<id>-lb` and Strimzi's `<id>-kafka-external-bootstrap`. Either alive = an Octavia
		// port may still sit on the tenant subnet.
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
			pending++ // Octavia LB (and its tenant-subnet port) still winding down
			continue
		}
		networkID := digStr(sec, "metadata", "annotations", AnnotationNetworkID)
		if networkID != "" {
			inUse, err := s.networkInUse(ctx, ns, projectID, networkID, dbID)
			if err != nil {
				return pending, err
			}
			if !inUse {
				osSvc := digStr(sec, "metadata", "annotations", AnnotationOSService)
				osProj := digStr(sec, "metadata", "annotations", AnnotationOSProject)
				osRegion := digStr(sec, "metadata", "annotations", AnnotationOSRegion)
				if revoke == nil {
					errs = append(errs, fmt.Errorf("database %s: network share %s not revoked (no revoker)", dbID, networkID))
					pending++
					continue
				}
				if err := revoke(ctx, osSvc, osProj, osRegion, networkID); err != nil {
					// Fail closed: the marker's annotations are the only revocation record — keep it.
					errs = append(errs, fmt.Errorf("database %s: revoke network share: %w", dbID, err))
					pending++
					continue
				}
			}
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
		if err := s.api.DeleteSecret(ctx, ns, name); err != nil {
			errs = append(errs, fmt.Errorf("database %s: delete net-share marker: %w", dbID, err))
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

// networkInUse reports whether the network is still needed by another database of the project —
// in which case the neutron share must stay. Two signals, both fresh-listed: (1) a LIVE sibling
// Application whose values ride the network; (2) another net-share MARKER for the same network
// that is either inside its grace window or backed by an existing Application. Signal (2)
// closes the mid-create race: a sibling create applies its marker before its Application, so
// during that multi-round-trip window only the marker proves the network is spoken for —
// without it, a sweep pass finalizing an older orphan on the same network would revoke the
// share under the sibling. excludeDBID is the marker being finalized (its own records must not
// keep the share alive).
func (s *Service) networkInUse(ctx context.Context, ns, projectID, networkID, excludeDBID string) (bool, error) {
	apps, err := s.api.ListApplications(ctx, s.cfg.ArgoNamespace,
		LabelProject+"="+projectID+","+LabelManagedBy+"="+ManagedByValue)
	if err != nil {
		return false, err
	}
	for _, app := range apps {
		if dig(app, "metadata", "deletionTimestamp") != nil {
			continue
		}
		if digStr(app, "metadata", "name") == excludeDBID {
			continue
		}
		if digStr(app, "spec", "source", "helm", "valuesObject", "network", "networkId") == networkID {
			return true, nil
		}
	}
	secrets, err := s.api.ListSecrets(ctx, ns, LabelProject+"="+projectID+","+LabelManagedBy+"="+ManagedByValue)
	if err != nil {
		return false, err
	}
	for _, sec := range secrets {
		otherID, isMarker := strings.CutSuffix(digStr(sec, "metadata", "name"), netShareSuffix)
		if !isMarker || otherID == excludeDBID {
			continue
		}
		if digStr(sec, "metadata", "annotations", AnnotationNetworkID) != networkID {
			continue
		}
		if created, err := time.Parse(time.RFC3339, digStr(sec, "metadata", "creationTimestamp")); err == nil {
			if time.Since(created) < s.finalizeGrace {
				return true, nil // sibling mid-create — its Application may not be applied yet
			}
		}
		app, err := s.api.GetApplication(ctx, s.cfg.ArgoNamespace, otherID)
		if err != nil {
			return false, err
		}
		if app != nil {
			return true, nil
		}
	}
	return false, nil
}

// NetworkSafeToRevoke is the failed-create rollback's guard: true only when NO other database
// of the project (live Application or fresh/backed marker) still rides the network — the same
// decision the sweep makes. When false the caller must skip the revoke; the sweep converges the
// leftover marker later.
func (s *Service) NetworkSafeToRevoke(ctx context.Context, projectID, networkID, excludeDBID string) bool {
	inUse, err := s.networkInUse(ctx, NamespaceFor(projectID), projectID, networkID, excludeDBID)
	if err != nil {
		return false // fail closed: cannot prove it safe → leave the share for the sweep
	}
	return !inUse
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
// credentials off the engine's secret, host off the LB Service's Octavia VIP.
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
		if kamajik8s.NotFound(err) {
			return "", fmt.Errorf("dbaas: database %s: credentials not ready yet", dbID)
		}
		return "", fmt.Errorf("dbaas: rotate password: %w", err)
	}
	return password, nil
}

// lbHost reads the Octavia VIP off the named LB Service ("" while still provisioning).
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
	apps, err := s.api.ListApplications(ctx, s.cfg.ArgoNamespace, LabelManagedBy+"="+ManagedByValue)
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
