package project

// cloud_dbaas.go — the client write surface for Managed Databases. A dbaas provider has no
// compute tenant of its own, so DATABASE_CLUSTER writes bypass the generic WriteService entirely
// (kamaji precedent) and go through dbaas.Service: Application CR on the DB cluster in, status
// via the sync. The databases run on OPS-owned nodes; the only tenant-facing artifact is the
// MetalLB-assigned LB address, restricted by loadBalancerSourceRanges (no tenant network is
// involved, so there is no placement leg in the create path).

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/dbaas"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// newDatabaseID mints the platform identifier every k8s-side object is named by (Application,
// helm release, engine CR, LB Service). Customer names are display-only — the std- prefix keeps
// billing rows, audit logs and Argo app names unambiguous next to kamaji's clusters.
func newDatabaseID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "std-" + hex.EncodeToString(b)
}

// dbaasCreate handles the DATABASE_CLUSTER leg of cloudCreate for a dbaas provider.
func (h *Handler) dbaasCreate(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, req providers.CreateRequest) {
	if req.Type != cloud.TypeDatabaseCluster {
		h.fail(w, httpx.BadRequest("A Managed Database service only provisions DATABASE_CLUSTER resources"))
		return
	}
	if h.dbaasFor == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "managed databases are not configured")
		return
	}
	ds, err := h.dbaasFor(es)
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "managed databases are not available")
		return
	}
	spec, err := dbaasSpecFromData(proj.ID, req.Data)
	if err != nil {
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	cfg := ds.Config()
	if err := spec.Validate(cfg); err != nil {
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	if msg := h.dbaasStorageRefusal(r.Context(), spec.StorageClass); msg != "" {
		h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, msg))
		return
	}
	// Restore reads another database's backups, so PROVE the source is one of this project's
	// databases and shares the engine. Without this check a customer could name any database id
	// on the platform and recover someone else's data into their own instance.
	if spec.RestoreFrom != nil {
		src, err := h.dbaasSourceForRestore(r.Context(), proj, spec)
		if err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return
		}
		// True binlog PITR is decided here, not by the customer: mariadb-operator archives
		// binlogs only on the replication topology, which this platform runs only when the
		// SOURCE had more than one instance. Without it the restore still honours the
		// timestamp, just to the nearest physical backup — coarser, never wrong.
		if spec.Engine == dbaas.EngineMariaDB && spec.RestoreFrom.TargetTime != "" {
			d, _ := src.Data["database"].(map[string]any)
			spec.RestoreFrom.PITR = intAny(d["replicas"]) > 1
		}
	}
	data, err := ds.CreateDatabase(r.Context(), spec)
	if err != nil {
		h.fail(w, err)
		return
	}
	now := time.Now().UTC()
	region := proj.ServiceRegion(es.ID)
	if region == "" {
		region = es.DbaasRegion()
	}
	cr := &cloud.CloudResource{
		ServiceID: es.ID, Region: region, ProjectID: proj.ID,
		Type: cloud.TypeDatabaseCluster, ExternalID: spec.ID,
		Data: data, CreatedAt: &now, UpdatedAt: &now,
	}
	// Respond with the STORED resource, not the struct we built: the id is generated on insert,
	// and callers navigate to it.
	saved, err := h.cloud.Insert(r.Context(), cr)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", saved)
	httpx.OK(w, *saved)
}

// dbaasStorageRefusal refuses a database on a storage class that is a Proxmox storage profile
// closed to databases (plan S18/S7: never on "capacity" HDD). Kubernetes storage classes on
// Proxmox are named after the storage profile they provision on (proxmox-csi, one class per
// profile), so the class name is looked up as a profile slug on every Proxmox provider. "" (the
// DB cluster's default class) and classes that are no profile pass.
func (h *Handler) dbaasStorageRefusal(ctx context.Context, storageClass string) string {
	if storageClass == "" || h.esSvc == nil {
		return ""
	}
	services, err := h.esSvc.ListByType(ctx, externalservice.TypeCloud)
	if err != nil {
		return ""
	}
	for i := range services {
		if !services[i].IsProxmox() {
			continue
		}
		if msg := dbaasProfileRefusal(services[i].PVEStorageProfiles(), storageClass); msg != "" {
			return msg
		}
	}
	return ""
}

// dbaasProfileRefusal is the pure check behind dbaasStorageRefusal.
func dbaasProfileRefusal(profiles map[string]externalservice.PVEStorageProfile, storageClass string) string {
	p, ok := profiles[storageClass]
	if !ok || p.DBaaS() {
		return ""
	}
	if p.Capacity() {
		return fmt.Sprintf("Storage class %q (%s) is HDD capacity storage — not for databases. Choose another storage class", storageClass, p.DisplayName())
	}
	return fmt.Sprintf("Storage class %q (%s) does not allow databases. Choose another storage class", storageClass, p.DisplayName())
}

// dbaasDelete handles the DATABASE_CLUSTER leg of cloudDelete: Application delete (ArgoCD's
// resources-finalizer cascades the chart, LB Service included) + cache archive. The orphan
// sweep reaps the marker and auth secrets once the cascade is done.
func (h *Handler) dbaasDelete(ctx context.Context, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource) error {
	if h.dbaasFor == nil {
		return fmt.Errorf("managed databases are not configured")
	}
	ds, err := h.dbaasFor(es)
	if err != nil {
		return err
	}
	if err := ds.DeleteDatabase(ctx, proj.ID, cr.ExternalID); err != nil {
		return err
	}
	return h.cloud.DeleteAndArchive(ctx, cr, time.Now().UTC())
}

// dbaasAction serves the per-database actions; reports whether it handled the request
// (kamajiAction precedent). Engine-gated via dbaas.Capabilities — the engine is read off the
// LIVE values, never the cache. Actions:
//   - GET_CONNECTION_INFO      → {result:{host,port,dbname,username,password,uri,engine}} —
//     fetched on demand (secret + LB Service), never stored
//   - RESIZE {cpu,memoryGiB}   → per-instance resources; the operator rolls the pods
//   - RESIZE_STORAGE {storageGiB} → grow-only PVC expansion (validated vs LIVE values)
//   - SCALE_REPLICAS {replicas}   → engine-semantic instance count, catalog-gated
//   - UPGRADE {version}        → engine version bump (catalog-gated, upward-only vs LIVE
//     values); the operator rolls the pods, the LB ingress IP never changes
//   - RESTART {}               → stamps values.restartedAt; the chart maps it per engine
//   - RESET_PASSWORD {}        → server-generated, applied to the engine secret, returned once
//   - SET_ALLOWED_CIDRS {allowedCidrs} → LB source ranges (loadBalancerSourceRanges)
//   - MANAGE_ACCESS {databases,users,roles?} → declarative logical databases / users (new
//     passwords returned once)
//   - RESET_USER_PASSWORD {username} → rotates a customer-created user's password, returned once
//   - GET_LOGS {lines?}        → tail of every pod's engine log
//   - SET_PARAMETERS {parameters} → whole desired runtime-settings set (allowlisted)
//   - SET_READ_ENDPOINT {enabled} → second, read-only endpoint
//   - SET_BACKUP {enabled?,schedule,retentionDays} / CREATE_BACKUP / LIST_BACKUPS → object-store
//     backups (create-with-restore: see dbaasSpecFromData restoreFrom)
//   - SET_INDEX_POLICIES {policies} → OpenSearch retention (ISM)
//   - SET_CUSTOM_DOMAIN {domain,certPem,keyPem,caPem?} → opensearch BYO domain + certificate
func (h *Handler) dbaasAction(w http.ResponseWriter, r *http.Request, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	if !es.IsDbaas() || cr.Type != cloud.TypeDatabaseCluster {
		return false
	}
	if h.dbaasFor == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "managed databases are not configured")
		return true
	}
	ds, err := h.dbaasFor(es)
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "managed databases are not available")
		return true
	}

	if action == "GET_CONNECTION_INFO" {
		info, err := ds.ConnectionInfo(r.Context(), proj.ID, cr.ExternalID)
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": info})
		return true
	}

	// Every mutating action is capability-gated on the LIVE engine. ponytail: one extra
	// GetApplication per action — cheap, and it removes the stale-cache class of bug entirely.
	engine, err := h.dbaasLiveEngine(r.Context(), ds, cr.ExternalID)
	if err != nil {
		h.fail(w, err)
		return true
	}
	known, gated := dbaas.Capabilities[engine]
	if !gated {
		h.fail(w, httpx.BadRequest(fmt.Sprintf("unknown engine %q", engine)))
		return true
	}

	switch action {
	case "RESIZE", "RESIZE_STORAGE", "SCALE_REPLICAS", "RESTART", "RESET_PASSWORD", "UPGRADE", "SET_SSO", "SET_CUSTOM_DOMAIN", "SET_AUTOSCALE", "MANAGE_ACCESS", "SET_INDEX_POLICIES", "SET_READ_ENDPOINT", "SET_PARAMETERS":
		if !known[action] {
			h.fail(w, httpx.BadRequest(fmt.Sprintf("engine %s does not support %s", engine, action)))
			return true
		}
	case "SET_BACKUP", "CREATE_BACKUP", "LIST_BACKUPS":
		// Two gates, not one: the engine must have a backup mechanism AND the provider must
		// actually have an object store. A toggle that writes nowhere is worse than no toggle.
		if !known["BACKUP"] {
			h.fail(w, httpx.BadRequest(fmt.Sprintf("engine %s does not support backups", engine)))
			return true
		}
		if !ds.Config().Backup.Enabled() {
			h.fail(w, httpx.BadRequest("this location has no backup object store configured"))
			return true
		}
	case "RESET_USER_PASSWORD":
		// Rotating a customer-created user's password is part of the same mechanism as
		// declaring that user, so it rides MANAGE_ACCESS rather than owning a capability key.
		if !known["MANAGE_ACCESS"] {
			h.fail(w, httpx.BadRequest(fmt.Sprintf("engine %s does not support %s", engine, action)))
			return true
		}
	}

	cfg := ds.Config()
	switch action {
	case "RESIZE":
		cpu, memory := intAny(data["cpu"]), intAny(data["memoryGiB"])
		if cpu < 1 || (cfg.Limits.MaxCPU > 0 && cpu > cfg.Limits.MaxCPU) {
			h.fail(w, httpx.BadRequest(fmt.Sprintf("cpu must be 1..%d", max(cfg.Limits.MaxCPU, 1))))
			return true
		}
		if memory < 1 || (cfg.Limits.MaxMemoryGiB > 0 && memory > cfg.Limits.MaxMemoryGiB) {
			h.fail(w, httpx.BadRequest(fmt.Sprintf("memoryGiB must be 1..%d", max(cfg.Limits.MaxMemoryGiB, 1))))
			return true
		}
		err := ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			values["resources"] = map[string]any{"cpu": cpu, "memoryGi": memory}
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "RESIZING"})
		return true

	case "RESIZE_STORAGE":
		size := intAny(data["storageGiB"])
		if size < 1 || (cfg.Limits.MaxStorageGiB > 0 && size > cfg.Limits.MaxStorageGiB) {
			h.fail(w, httpx.BadRequest(fmt.Sprintf("storageGiB must be 1..%d", max(cfg.Limits.MaxStorageGiB, 1))))
			return true
		}
		err := ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			// Grow-only, validated against the LIVE values, not the cache — a stale sync must
			// not let a shrink (data loss via PVC semantics) through.
			storage, _ := values["storage"].(map[string]any)
			if storage == nil {
				return fmt.Errorf("dbaas: database %s: values carry no storage block", cr.ExternalID)
			}
			if current := intAny(storage["sizeGi"]); size <= current {
				return httpx.BadRequest(fmt.Sprintf("storage can only grow (currently %d GiB)", current))
			}
			storage["sizeGi"] = size
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "RESIZING"})
		return true

	case "SCALE_REPLICAS":
		replicas := intAny(data["replicas"])
		offer, ok := cfg.Engines[engine]
		if !ok {
			h.fail(w, httpx.BadRequest(fmt.Sprintf("engine %s is no longer offered by this provider", engine)))
			return true
		}
		allowed := offer.ReplicaChoices()
		valid := false
		for _, n := range allowed {
			if n == replicas {
				valid = true
				break
			}
		}
		if !valid {
			h.fail(w, httpx.BadRequest(fmt.Sprintf("engine %s offers %v replicas, not %d", engine, allowed, replicas)))
			return true
		}
		err := ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			values["instances"] = replicas
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "SCALING"})
		return true

	case "UPGRADE":
		version, _ := data["version"].(string)
		if version == "" {
			h.fail(w, httpx.BadRequest("version is required"))
			return true
		}
		offer, ok := cfg.Engines[engine]
		if !ok || !slices.Contains(offer.Versions, version) {
			h.fail(w, httpx.BadRequest(fmt.Sprintf("version %q is not offered for engine %s", version, engine)))
			return true
		}
		backedUp := false
		err := ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			// Upward-only, validated against the LIVE values, not the cache — a stale sync must
			// not let a downgrade through (the kamaji UPGRADE rule). The chart resolves the new
			// engine image from its version map and the operator performs its own rolling
			// upgrade; the LB endpoint never changes.
			current, _ := values["engineVersion"].(string)
			if err := dbaas.ValidateUpgradePath(current, version); err != nil {
				return httpx.BadRequest(err.Error())
			}
			values["engineVersion"] = version
			// One patch, so the safety backup cannot be lost to a failed second call.
			if dbaasWantsBackupFirst(data) {
				backedUp = dbaas.StampBackupRun(values, dbaasBackupStamp())
			}
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPGRADING", "backupStarted": backedUp})
		return true

	case "RESTART":
		stamp := time.Now().UTC().Format(time.RFC3339)
		err := ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			values["restartedAt"] = stamp
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "RESTARTING"})
		return true

	case "RESET_PASSWORD":
		password, err := ds.ResetPassword(r.Context(), proj.ID, cr.ExternalID, engine)
		if err != nil {
			h.fail(w, err)
			return true
		}
		// ferretdb's stateless frontends resolve the backend password from an env var ONCE at
		// container start — the rotation must roll them or the running pods keep the old
		// credential until their connection pool recycles. The password IS rotated at this
		// point, so a failed stamp still returns it (with the restart left to the customer).
		if engine == dbaas.EngineFerretDB {
			stamp := time.Now().UTC().Format(time.RFC3339)
			if serr := ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
				values["restartedAt"] = stamp
				return nil
			}); serr != nil {
				slog.Warn("dbaas: restart stamp after ferretdb password rotation", "database", cr.ExternalID, "err", serr)
			}
		}
		httpx.OK(w, map[string]any{"result": map[string]any{"password": password}})
		return true

	case "SET_AUTOSCALE":
		// Opt-in vertical autoscale with customer-set ceilings (surcharge-priced — billing
		// keys on autoscale_enabled). enabled:false (or an empty body) turns it off.
		enabled := false
		if raw, has := data["enabled"]; has && raw != nil {
			b, ok := raw.(bool)
			if !ok {
				h.fail(w, httpx.BadRequest("enabled must be true or false"))
				return true
			}
			enabled = b
		}
		maxCPU, maxMem, maxStorage := intAny(data["maxCpu"]), intAny(data["maxMemoryGiB"]), intAny(data["maxStorageGiB"])
		if enabled {
			if maxCPU < 1 || (cfg.Limits.MaxCPU > 0 && maxCPU > cfg.Limits.MaxCPU) {
				h.fail(w, httpx.BadRequest(fmt.Sprintf("maxCpu must be 1..%d", max(cfg.Limits.MaxCPU, 1))))
				return true
			}
			if maxMem < 1 || (cfg.Limits.MaxMemoryGiB > 0 && maxMem > cfg.Limits.MaxMemoryGiB) {
				h.fail(w, httpx.BadRequest(fmt.Sprintf("maxMemoryGiB must be 1..%d", max(cfg.Limits.MaxMemoryGiB, 1))))
				return true
			}
			// maxStorageGiB 0 = disk leg off (also implied for engines without RESIZE_STORAGE).
			if maxStorage < 0 || (cfg.Limits.MaxStorageGiB > 0 && maxStorage > cfg.Limits.MaxStorageGiB) {
				h.fail(w, httpx.BadRequest(fmt.Sprintf("maxStorageGiB must be 0..%d", cfg.Limits.MaxStorageGiB)))
				return true
			}
		}
		err := ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			if !enabled {
				delete(values, "autoscale")
				return nil
			}
			// Ceilings must sit at or above the CURRENT declared size (live values — the
			// tick only scales up, a ceiling below current would wedge it).
			res, _ := values["resources"].(map[string]any)
			if res != nil {
				if cur := intAny(res["cpu"]); maxCPU < cur {
					return httpx.BadRequest(fmt.Sprintf("maxCpu must be >= the current %d vCPU", cur))
				}
				if cur := intAny(res["memoryGi"]); maxMem < cur {
					return httpx.BadRequest(fmt.Sprintf("maxMemoryGiB must be >= the current %d GiB", cur))
				}
			}
			if storage, _ := values["storage"].(map[string]any); storage != nil && maxStorage > 0 {
				if cur := intAny(storage["sizeGi"]); maxStorage < cur {
					return httpx.BadRequest(fmt.Sprintf("maxStorageGiB must be >= the current %d GiB", cur))
				}
			}
			values["autoscale"] = map[string]any{
				"enabled": true, "maxCpu": maxCPU, "maxMemoryGi": maxMem, "maxStorageGi": maxStorage,
			}
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING"})
		return true

	case "SET_SSO":
		// OpenSearch Dashboards OIDC against a PUBLIC IdP client — no client secret anywhere
		// (the Application CR is argocd-readable, values must never carry secrets; register a
		// public client with PKCE in the IdP). Empty connectUrl disables SSO.
		sso, err := dbaasSSOFields(data)
		if err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return true
		}
		err = ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			block, _ := values["opensearch"].(map[string]any)
			if block == nil {
				block = map[string]any{}
			}
			if len(sso) == 0 {
				delete(block, "sso")
			} else {
				sso["enabled"] = true
				block["sso"] = sso
				// SSO rides Dashboards: the chart reads the sso block only when dashboards are
				// enabled, so setting SSO on a dashboards-less database deploys them too.
				// Disabling SSO deliberately does NOT tear the Dashboards down.
				block["dashboards"] = map[string]any{"enabled": true}
			}
			values["opensearch"] = block
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING"})
		return true

	case "MANAGE_ACCESS":
		// Declarative: the client sends the FULL desired list of logical databases and users,
		// so a dropped entry is a removal. Passwords for newly declared users come back once,
		// in this response only — nothing stores them.
		dbs, users, roles, err := dbaasAccessFromData(data)
		if err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return true
		}
		// Validate before anything is written so a rejected list cannot leave half-created
		// Secrets behind (SetAccess re-checks; this is what turns a bad list into a 400).
		if err := dbaas.ValidateAccess(engine, dbs, users, roles); err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return true
		}
		// Removing a database DROPS it (the reclaim policy is delete), so the same safety
		// backup applies here — this is the other action that can lose data outright.
		backedUp := false
		if dbaasWantsBackupFirst(data) {
			if err := ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
				backedUp = dbaas.StampBackupRun(values, dbaasBackupStamp())
				return nil
			}); err != nil {
				h.fail(w, err)
				return true
			}
		}
		created, err := ds.SetAccess(r.Context(), proj.ID, cr.ExternalID, engine, dbs, users, roles)
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING", "credentials": created, "backupStarted": backedUp})
		return true

	case "GET_LOGS":
		// Read-only and bounded; nothing is stored. Not capability-gated: every engine logs to
		// stdout, and a database whose values are unreadable is exactly the one someone needs
		// the log for.
		logs, err := ds.Logs(r.Context(), proj.ID, cr.ExternalID, engine, intAny(data["lines"]))
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": logs})
		return true

	case "SET_PARAMETERS":
		// The WHOLE desired set: a parameter dropped here returns to the engine default.
		raw, _ := data["parameters"].(map[string]any)
		params := make(map[string]string, len(raw))
		for k, v := range raw {
			s, ok := v.(string)
			if !ok {
				h.fail(w, httpx.BadRequest(fmt.Sprintf("parameter %q must be a string", k)))
				return true
			}
			params[k] = strings.TrimSpace(s)
		}
		if err := dbaas.ValidateParameters(engine, params); err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return true
		}
		if err := ds.SetParameters(r.Context(), cr.ExternalID, engine, params); err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{
			"result": "UPDATING",
			// The operators decide this, not proxcloud — the flag is so the UI can say what will
			// happen rather than the customer discovering it when connections drop.
			"restarts": dbaas.ParametersNeedRestart(engine, params),
		})
		return true

	case "SET_READ_ENDPOINT":
		// Toggling after create: same rules as the create path, checked against the LIVE values
		// so a stale cache cannot let a single-instance database claim a reader.
		on, ok := data["enabled"].(bool)
		if !ok {
			h.fail(w, httpx.BadRequest("enabled must be true or false"))
			return true
		}
		if on && !dbaas.ReadEndpointEngines[engine] {
			h.fail(w, httpx.BadRequest(fmt.Sprintf("engine %s has no separate read endpoint", engine)))
			return true
		}
		err := ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			if on && intAny(values["instances"]) < 2 {
				return httpx.BadRequest("a read-only endpoint needs more than one instance")
			}
			net, _ := values["network"].(map[string]any)
			if net == nil {
				return fmt.Errorf("dbaas: database %s: values carry no network block", cr.ExternalID)
			}
			if on {
				net["readEndpoint"] = true
			} else {
				delete(net, "readEndpoint")
			}
			values["network"] = net
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING"})
		return true

	case "SET_BACKUP":
		// Enabled/schedule/retention only — the object store itself is operator config, never a
		// customer choice, so nothing here names a bucket.
		spec := dbaas.BackupSpec{
			Schedule:      strings.TrimSpace(strAny(data["schedule"])),
			RetentionDays: intAny(data["retentionDays"]),
		}
		if raw, has := data["enabled"]; has && raw != nil {
			b, ok := raw.(bool)
			if !ok {
				h.fail(w, httpx.BadRequest("enabled must be true or false"))
				return true
			}
			spec.Enabled = b
		}
		if err := spec.Validate(); err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return true
		}
		if err := ds.SetBackup(r.Context(), proj.ID, cr.ExternalID, spec); err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING"})
		return true

	case "CREATE_BACKUP":
		// Declarative "back up now": stamp a timestamp the chart turns into one extra run. Same
		// path as every other change, so it survives a restart mid-flight.
		if err := ds.TriggerBackup(r.Context(), cr.ExternalID, dbaasBackupStamp()); err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "BACKING_UP"})
		return true

	case "LIST_BACKUPS":
		backups, err := ds.ListBackups(r.Context(), proj.ID, cr.ExternalID, engine)
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": backups})
		return true

	case "SET_INDEX_POLICIES":
		// OpenSearch ISM, narrowed to what customers actually ask for: retention, with optional
		// rollover. Declarative like MANAGE_ACCESS — the whole list, every time.
		policies, err := dbaasIndexPoliciesFromData(data)
		if err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return true
		}
		if err := dbaas.ValidateOSIndexPolicies(policies); err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return true
		}
		if err := ds.SetIndexPolicies(r.Context(), cr.ExternalID, policies); err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING"})
		return true

	case "RESET_USER_PASSWORD":
		username := strAny(data["username"])
		if !dbaas.ValidIdent(username) {
			h.fail(w, httpx.BadRequest("username is required"))
			return true
		}
		password, err := ds.ResetUserPassword(r.Context(), proj.ID, cr.ExternalID, username)
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING", "username": username, "password": password})
		return true

	case "SET_CUSTOM_DOMAIN":
		// BYO domain + certificate (opensearch): the customer certifies their own name — no
		// ACME anywhere. Their DNS points at the endpoint (CNAME to the platform name, or an A
		// record to the VIP); we mount their cert on the http layer + Dashboards. PEM material
		// goes straight into the cluster secret — never into proxcloud storage, values, or logs.
		// Empty domain = remove (values key + secret; the operator falls back per the chart's
		// tls precedence).
		domain := strings.ToLower(strings.TrimSpace(strAny(data["domain"])))
		if domain == "" {
			if err := ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
				if block, _ := values["opensearch"].(map[string]any); block != nil {
					delete(block, "customDomain")
				}
				return nil
			}); err != nil {
				h.fail(w, err)
				return true
			}
			if err := ds.DeleteCustomDomainTLS(r.Context(), proj.ID, cr.ExternalID); err != nil {
				h.fail(w, err)
				return true
			}
			httpx.OK(w, map[string]any{"result": "UPDATING"})
			return true
		}
		certPEM, keyPEM := strAny(data["certPem"]), strAny(data["keyPem"])
		if err := validateCustomDomainTLS(domain, certPEM, keyPEM); err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return true
		}
		// Secret BEFORE the values flip — the wrong order points the operator at a secret that
		// does not exist yet and wedges the rollout.
		if err := ds.SetCustomDomainTLS(r.Context(), proj.ID, cr.ExternalID, certPEM, keyPEM, strAny(data["caPem"])); err != nil {
			h.fail(w, err)
			return true
		}
		err := ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			block, _ := values["opensearch"].(map[string]any)
			if block == nil {
				block = map[string]any{}
			}
			block["customDomain"] = domain
			values["opensearch"] = block
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING"})
		return true

	case "SET_ALLOWED_CIDRS":
		cidrs, err := dbaasCIDRs(data["allowedCidrs"])
		if err != nil {
			h.fail(w, httpx.BadRequest(err.Error()))
			return true
		}
		err = ds.PatchDatabaseValues(r.Context(), cr.ExternalID, func(values map[string]any) error {
			network, _ := values["network"].(map[string]any)
			if network == nil {
				return fmt.Errorf("dbaas: database %s: values carry no network block", cr.ExternalID)
			}
			network["allowedCidrs"] = cidrs
			return nil
		})
		if err != nil {
			h.fail(w, err)
			return true
		}
		httpx.OK(w, map[string]any{"result": "UPDATING"})
		return true
	}
	return false
}

// dbaasLiveEngine reads the database's engine off the live Application values.
func (h *Handler) dbaasLiveEngine(ctx context.Context, ds *dbaas.Service, dbID string) (string, error) {
	engine := ""
	err := ds.PatchDatabaseApp(ctx, dbID, func(app map[string]any) error {
		engine = digAnyStr(app, "spec", "source", "helm", "valuesObject", "engine")
		return errDbaasReadOnly // abort before the re-apply — read-only walk
	})
	if err != nil && err != errDbaasReadOnly {
		return "", err
	}
	if engine == "" {
		return "", fmt.Errorf("dbaas: database %s carries no engine", dbID)
	}
	return engine, nil
}

// errDbaasReadOnly aborts a PatchDatabaseApp walk that only wanted to read.
var errDbaasReadOnly = fmt.Errorf("dbaas: read-only")

// dbaasSpecFromData maps the client create body → DatabaseSpec. Body shape:
//
//	{name, engine, version, replicas, cpu, memoryGiB, storageGiB, storageClass?,
//	 allowedCidrs:[...], beta?, readEndpoint?, dashboards?, sso?,
//	 restoreFrom?: {sourceDatabaseId, targetTime?, backupName?}}
//
// Strict on what it reads (numbers must be numbers — intAny's zero would otherwise turn a typo
// into "replicas 0" and a confusing catalog error); unknown keys are ignored like every other
// create body.
func dbaasSpecFromData(projectID string, d map[string]any) (dbaas.DatabaseSpec, error) {
	spec := dbaas.DatabaseSpec{
		ID:           newDatabaseID(),
		ProjectID:    projectID,
		DisplayName:  strAny(d["name"]),
		Engine:       strAny(d["engine"]),
		Version:      strAny(d["version"]),
		StorageClass: strAny(d["storageClass"]),
	}
	if spec.DisplayName == "" {
		return spec, fmt.Errorf("name is required")
	}
	for key, dst := range map[string]*int{
		"replicas": &spec.Replicas, "cpu": &spec.CPU,
		"memoryGiB": &spec.MemoryGiB, "storageGiB": &spec.StorageGiB,
	} {
		v, has := d[key]
		if !has || v == nil {
			return spec, fmt.Errorf("%s is required", key)
		}
		n := intAny(v)
		if n == 0 {
			if _, isNum := v.(float64); !isNum {
				return spec, fmt.Errorf("%s must be a number", key)
			}
		}
		*dst = n
	}
	if cidrs, has := d["allowedCidrs"]; has && cidrs != nil {
		list, err := dbaasCIDRsStrings(cidrs)
		if err != nil {
			return spec, err
		}
		spec.AllowedCIDRs = list
	}
	// Beta acknowledgment is STRICT: a non-bool value is a 400, not a silent false — the whole
	// point of the gate is that nobody lands on a pre-GA operator by accident.
	if raw, has := d["beta"]; has && raw != nil {
		b, ok := raw.(bool)
		if !ok {
			return spec, fmt.Errorf("beta must be true or false")
		}
		spec.BetaAck = b
	}
	if raw, has := d["readEndpoint"]; has && raw != nil {
		b, ok := raw.(bool)
		if !ok {
			return spec, fmt.Errorf("readEndpoint must be true or false")
		}
		spec.ReadEndpoint = b
	}
	if raw, has := d["dashboards"]; has && raw != nil {
		b, ok := raw.(bool)
		if !ok {
			return spec, fmt.Errorf("dashboards must be true or false")
		}
		spec.DashboardsEnabled = b
	}
	if raw, has := d["restoreFrom"]; has && raw != nil {
		obj, ok := raw.(map[string]any)
		if !ok {
			return spec, fmt.Errorf("restoreFrom must be an object")
		}
		spec.RestoreFrom = &dbaas.RestoreSource{
			SourceID:   strings.TrimSpace(strAny(obj["sourceDatabaseId"])),
			TargetTime: strings.TrimSpace(strAny(obj["targetTime"])),
			BackupName: strings.TrimSpace(strAny(obj["backupName"])),
		}
	}
	if raw, has := d["sso"]; has && raw != nil {
		block, ok := raw.(map[string]any)
		if !ok {
			return spec, fmt.Errorf("sso must be an object")
		}
		sso, err := dbaasSSOFields(block)
		if err != nil {
			return spec, err
		}
		spec.SSO = sso
	}
	return spec, nil
}

// dbaasWantsBackupFirst reads the opt-in safety-backup flag. Absent = no backup: a caller that
// did not ask must never have one taken behind its back, since backups cost storage.
func dbaasWantsBackupFirst(d map[string]any) bool {
	b, _ := d["backupFirst"].(bool)
	return b
}

// dbaasBackupStamp is the value CREATE_BACKUP and the safety backups share — a changed stamp is
// what produces exactly one extra backup.
func dbaasBackupStamp() string { return time.Now().UTC().Format("20060102-150405") }

// dbaasAccessFromData decodes a MANAGE_ACCESS body:
//
//	{databases: [{name, owner?}], users: [{name, databases?: [...], roles?: [...]}]}
//
// Shape only — dbaas.ValidateAccess owns the rules (identifier charset, reserved names,
// cross-references, the OpenSearch role allowlist).
func dbaasAccessFromData(d map[string]any) ([]dbaas.DBDatabase, []dbaas.DBUser, []dbaas.OSRole, error) {
	strList := func(v any, what string) ([]string, error) {
		if v == nil {
			return nil, nil
		}
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of names", what)
		}
		out := make([]string, 0, len(items))
		for _, raw := range items {
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be an array of names", what)
			}
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	}
	objList := func(v any, what string) ([]map[string]any, error) {
		if v == nil {
			return nil, nil
		}
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of objects", what)
		}
		out := make([]map[string]any, 0, len(items))
		for _, raw := range items {
			obj, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s must be an array of objects", what)
			}
			out = append(out, obj)
		}
		return out, nil
	}

	rawDBs, err := objList(d["databases"], "databases")
	if err != nil {
		return nil, nil, nil, err
	}
	dbs := make([]dbaas.DBDatabase, 0, len(rawDBs))
	for _, obj := range rawDBs {
		dbs = append(dbs, dbaas.DBDatabase{
			Name:  strings.TrimSpace(strAny(obj["name"])),
			Owner: strings.TrimSpace(strAny(obj["owner"])),
		})
	}
	rawUsers, err := objList(d["users"], "users")
	if err != nil {
		return nil, nil, nil, err
	}
	users := make([]dbaas.DBUser, 0, len(rawUsers))
	for _, obj := range rawUsers {
		grants, err := strList(obj["databases"], "user databases")
		if err != nil {
			return nil, nil, nil, err
		}
		userRoles, err := strList(obj["roles"], "user roles")
		if err != nil {
			return nil, nil, nil, err
		}
		users = append(users, dbaas.DBUser{
			Name:      strings.TrimSpace(strAny(obj["name"])),
			Databases: grants,
			Roles:     userRoles,
		})
	}
	rawRoles, err := objList(d["roles"], "roles")
	if err != nil {
		return nil, nil, nil, err
	}
	roles := make([]dbaas.OSRole, 0, len(rawRoles))
	for _, obj := range rawRoles {
		patterns, err := strList(obj["indexPatterns"], "role index patterns")
		if err != nil {
			return nil, nil, nil, err
		}
		actions, err := strList(obj["actions"], "role permissions")
		if err != nil {
			return nil, nil, nil, err
		}
		roles = append(roles, dbaas.OSRole{
			Name:          strings.TrimSpace(strAny(obj["name"])),
			IndexPatterns: patterns,
			Actions:       actions,
		})
	}
	return dbs, users, roles, nil
}

// dbaasIndexPoliciesFromData decodes a SET_INDEX_POLICIES body:
//
//	{policies: [{name, indexPatterns: [...], deleteAfterDays, rolloverGiB?, rolloverDays?}]}
func dbaasIndexPoliciesFromData(d map[string]any) ([]dbaas.OSIndexPolicy, error) {
	items, ok := d["policies"].([]any)
	if d["policies"] != nil && !ok {
		return nil, fmt.Errorf("policies must be an array of objects")
	}
	out := make([]dbaas.OSIndexPolicy, 0, len(items))
	for _, raw := range items {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("policies must be an array of objects")
		}
		patternsRaw, ok := obj["indexPatterns"].([]any)
		if obj["indexPatterns"] != nil && !ok {
			return nil, fmt.Errorf("indexPatterns must be an array of patterns")
		}
		patterns := make([]string, 0, len(patternsRaw))
		for _, p := range patternsRaw {
			s, ok := p.(string)
			if !ok {
				return nil, fmt.Errorf("indexPatterns must be an array of patterns")
			}
			if s = strings.TrimSpace(s); s != "" {
				patterns = append(patterns, s)
			}
		}
		out = append(out, dbaas.OSIndexPolicy{
			Name:            strings.TrimSpace(strAny(obj["name"])),
			IndexPatterns:   patterns,
			DeleteAfterDays: intAny(obj["deleteAfterDays"]),
			RolloverGiB:     intAny(obj["rolloverGiB"]),
			RolloverDays:    intAny(obj["rolloverDays"]),
		})
	}
	return out, nil
}

// dbaasSSOFields pulls the Dashboards OIDC block out of a request body. Shared by create and
// SET_SSO so the two can never drift on the "connectUrl + clientId, or nothing at all" rule.
// Empty map = SSO off. No client secret is ever accepted: the block rides the argocd-readable
// Application, so the IdP client must be a public (PKCE) one.
func dbaasSSOFields(d map[string]any) (map[string]any, error) {
	sso := map[string]any{}
	for _, k := range []string{"connectUrl", "clientId", "scope", "baseRedirectUrl", "logoutUrl"} {
		if v := strAny(d[k]); v != "" {
			sso[k] = v
		}
	}
	if len(sso) > 0 && (sso["connectUrl"] == nil || sso["clientId"] == nil) {
		return nil, fmt.Errorf("sso needs at least connectUrl and clientId (or send nothing to disable)")
	}
	return sso, nil
}

// dbaasDomainRE: lowercase DNS name, at least two labels — the shape a public certificate can
// actually be issued for.
var dbaasDomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// validateCustomDomainTLS rejects a BYO cert bundle that could not serve the domain: key must
// match the cert, the leaf must cover the name and must not be expired. Chain/CA trust is NOT
// verified — the customer chose their issuer; browsers judge the chain, not us.
func validateCustomDomainTLS(domain, certPEM, keyPEM string) error {
	if !dbaasDomainRE.MatchString(domain) || len(domain) > 253 {
		return fmt.Errorf("domain %q is not a valid DNS name", domain)
	}
	if certPEM == "" || keyPEM == "" {
		return fmt.Errorf("certPem and keyPem are required")
	}
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return fmt.Errorf("certificate/key pair rejected: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("certificate rejected: %v", err)
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		return fmt.Errorf("certificate does not cover %s: %v", domain, err)
	}
	if time.Now().After(leaf.NotAfter) {
		return fmt.Errorf("certificate expired %s", leaf.NotAfter.Format(time.RFC3339))
	}
	return nil
}

// dbaasCIDRsStrings decodes an allowedCidrs array; blanks are skipped, non-strings rejected.
func dbaasCIDRsStrings(v any) ([]string, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("allowedCidrs must be an array of CIDR strings")
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, fmt.Errorf("allowedCidrs must be an array of CIDR strings")
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// dbaasCIDRs is the []any twin for values patches (SET_ALLOWED_CIDRS), with syntax validation.
func dbaasCIDRs(v any) ([]any, error) {
	list, err := dbaasCIDRsStrings(v)
	if err != nil {
		return nil, err
	}
	spec := dbaas.DatabaseSpec{AllowedCIDRs: list}
	if err := dbaasValidateCIDRs(spec.AllowedCIDRs); err != nil {
		return nil, err
	}
	out := make([]any, 0, len(list))
	for _, s := range list {
		out = append(out, s)
	}
	return out, nil
}

// refreshDbaasClusters live-reconciles the project's DATABASE_CLUSTER cache off the DB cluster —
// the client list's read-through (refreshKamajiClusters precedent). Best-effort.
func (h *Handler) refreshDbaasClusters(ctx context.Context, p *Project) {
	if h.dbaasFor == nil {
		return
	}
	for _, svcID := range p.ServiceIDs() {
		es, err := h.esSvc.Get(ctx, svcID)
		if err != nil || es == nil || !es.IsDbaas() {
			continue
		}
		ds, err := h.dbaasFor(es)
		if err != nil {
			continue
		}
		if _, err := providers.Reconcile(ctx, ds.SyncProvider(es.DbaasRegion(), p.ID), h.cloud, es.ID, time.Now().UTC()); err != nil {
			slog.Warn("dbaas: live database list refresh", "project", p.ID, "service", svcID, "err", err)
		}
	}
}
