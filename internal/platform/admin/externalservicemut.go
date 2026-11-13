package admin

// externalservicemut.go serves the MUTATIONS of the external-service surface
// (/api/v1/admin/service). The reads (serviceList / serviceByID / projectServices) live in
// handler.go and the live provider reads (os-images, os-flavors, volume/types, regions,
// public-networks, gpu-info …) in cloudadmin.go; none of those is re-registered here.
//
// Flow:
//
//	POST /object-store/test   = check an object-store endpoint and admin key, storing nothing
//	POST /{id}/object-store/test = the same check on a stored row, with its stored key
//	create(es)                = persist the operator-supplied doc (secret encrypted). Live validation only for a
//	                            PUBLIC ceph-s3 row; for others the Connection tab's Test button probes the provider
//	update(id,es)             = look up → merge connection fields → save (same check when the connection changed)
//	delete(id)                = in-use guards (project/user/cloudResource → exact 400s)
//	                            THEN look up (404 if absent) → delete
//	PUT /{id}/update          = look up (404 if absent) → same merge → save
//	the field-set PUTs        = look up (404 if absent) → mutate one stored config field → save:
//	    /{id}/quota               config.provisioning.quota = body
//	    /{id}/features            config.features = body (+ enabledConsoleTypes when present)
//	    /{id}/configuration       name, status
//	    /{id}/metrics-config      config.metrics (+ encrypted prometheus credentials)
//
// The externalService doc carries `secret` (provider credentials, encrypted at rest); every
// response that returns the doc strips it via shapeExternalService (handler.go).
//
// Mutations gate on ADMIN_SERVICE_MANAGE (admin:service:manage). Audit events are also written
// — deferred this pass (// TODO(audit)).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mutex-solutions/proxcloud/internal/cloud/client"
	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
	"github.com/mutex-solutions/proxcloud/internal/cloud/powerdns"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const externalServiceManagePerm = "admin:service:manage"

const externalServiceCollection = "externalService"

// routeExternalServiceMut registers ONLY the external-service mutation routes. The {id} param
// name reuses the one handler.go already uses on /service/{id} and its sub-paths (chi requires a
// single param name at a given path position).
func (h *Handler) routeExternalServiceMut(r chi.Router) {
	r.Post("/service", h.externalServiceCreate)
	r.Post("/service/object-store/test", h.objectStoreTest)
	r.Post("/service/{id}/object-store/test", h.objectStoreTestStored)
	r.Put("/service/{id}", h.externalServiceUpdate)
	r.Delete("/service/{id}", h.externalServiceDelete)
	// the no-provision re-save (PUT /{id}/update) — get-or-404 then merge + resave.
	r.Put("/service/{id}/update", h.externalServiceUpdateNoProvision)
	// field-set PUTs — persist one stored config field, then return the shaped doc.
	r.Put("/service/{id}/quota", h.externalServiceUpdateQuota)
	r.Put("/service/{id}/features", h.externalServiceUpdateFeatures)
	r.Put("/service/{id}/configuration", h.externalServiceUpdateConfiguration)
	// config.attachToNewProjects only (the rest of config is kept as stored).
	r.Put("/service/{id}/attach-policy", h.externalServiceUpdateAttachPolicy)
	// usage-metrics source (prometheus | none) + prometheus connection config; the
	// POST probes the configured prometheus endpoint live (metricstest.go).
	r.Put("/service/{id}/metrics-config", h.externalServiceUpdateMetricsConfig)
	r.Post("/service/{id}/metrics-test", h.externalServiceMetricsTest)
	h.routeSandboxProvider(r)
}

// serviceNotFoundErr is the 404 returned when a service id is not found
// ("Service not found: %s", interpolated, no trailing space). All the field-set PUTs (and PUT
// /{id}/update) resolve the doc by id → this 404 when absent.
func serviceNotFoundErr(id string) *httpx.HTTPError {
	return httpx.NotFound(fmt.Sprintf("Service not found: %s", id))
}

// kamajiRemovedKeysErr refuses a kamaji config that still sets a key of the tenant-side proxmox-csi controller, so an
// operator learns it no longer does anything instead of finding out from a cluster without volumes.
func kamajiRemovedKeysErr(cfg map[string]any) *httpx.HTTPError {
	if cfg["provider"] != "kamaji" {
		return nil
	}
	keys := externalservice.KamajiRemovedKeys(cfg)
	if len(keys) == 0 {
		return nil
	}
	return httpx.BadRequest("config.cluster." + strings.Join(keys, ", config.cluster.") + " no longer exist: tenant block " +
		"volumes go through the CSI gateway and no PVE address is given to tenants. Remove them from the cluster block " +
		"and send it again (docs/managed-k8s.md)")
}

// externalServiceCreate persists the operator-supplied doc (secret encrypted). Only a PUBLIC ceph-s3 row is checked
// live (checkPublicCeph); others are exercised by the detail page's Test connection and the live provider reads.
func (h *Handler) externalServiceCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	name, _ := body["name"].(string)
	if name == "" {
		httpx.WriteError(w, httpx.BadRequest("Name is required"))
		return
	}
	doc := pgdoc.M{}
	for k, v := range body {
		if k == "id" || k == "_id" || k == "_class" {
			continue
		}
		doc[k] = v
	}
	if _, ok := doc["type"]; !ok {
		doc["type"] = "CLOUD"
	}
	if _, ok := doc["status"]; !ok {
		doc["status"] = "PUBLIC"
	}
	if cfg, ok := doc["config"].(map[string]any); ok && cfg["provider"] == "truenas" {
		if _, err := externalservice.ParseSNATSources(cfg["snatSources"]); err != nil {
			httpx.WriteError(w, httpx.BadRequest(err.Error()))
			return
		}
	}
	if cfg, _ := doc["config"].(map[string]any); cfg["provider"] == "powerdns" {
		if _, err := powerdns.OwnershipResolvers(cfg["ownershipResolvers"]); err != nil {
			httpx.WriteError(w, httpx.BadRequest(err.Error()))
			return
		}
	}
	if cfg, _ := doc["config"].(map[string]any); kamajiRemovedKeysErr(cfg) != nil {
		httpx.WriteError(w, kamajiRemovedKeysErr(cfg))
		return
	}
	if cfg, _ := doc["config"].(map[string]any); externalservice.ValidateAttachPolicy(cfg) != nil {
		httpx.WriteError(w, httpx.BadRequest(externalservice.ValidateAttachPolicy(cfg).Error()))
		return
	}
	// Stable string _id (String-id convention — same shape as the seeded provider docs;
	// projects reference the service by this id string).
	doc["_id"] = pgdoc.NewID()
	doc["createdAt"] = time.Now().UTC()
	// Encrypt the whole (all-new, plaintext) secret sub-document before it reaches the datastore so cloud
	// credentials are never stored at rest — symmetric with the decrypt on the read path.
	if sec, ok := doc["secret"]; ok && h.esSvc != nil {
		doc["secret"] = h.esSvc.EncryptSecret(sec)
	}
	if herr := h.checkObjectStoreRow(r.Context(), doc); herr != nil {
		httpx.WriteError(w, herr)
		return
	}
	if herr := h.checkSandboxGoLive(r.Context(), doc); herr != nil {
		httpx.WriteError(w, herr)
		return
	}
	if err := h.repo.InsertDocKeepID(r.Context(), externalServiceCollection, doc); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a service is created.
	shapeExternalService(doc) // strips secret, _id→id
	httpx.OK(w, doc)
}

// objectStoreTest proves an object-store connection before it is stored (POST /service/object-store/test): the
// installer calls it ahead of registering a row. A failed check is a 400 with the reason, like the Proxmox connection
// test. A blank provider means ceph-s3, which is what the installer's callers send.
func (h *Handler) objectStoreTest(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	var body struct {
		Provider       string `json:"provider"`
		S3Endpoint     string `json:"s3Endpoint"`
		AdminAPIURL    string `json:"adminApiUrl"`
		Region         string `json:"region"`
		AdminAccessKey string `json:"adminAccessKey"`
		AdminSecretKey string `json:"adminSecretKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	if herr := checkObjectStore(r.Context(), body.Provider, client.ObjectStoreConfig{
		S3Endpoint: body.S3Endpoint, AdminURL: body.AdminAPIURL, Region: body.Region,
		AdminAccessKey: body.AdminAccessKey, AdminSecretKey: body.AdminSecretKey,
	}); herr != nil {
		httpx.WriteError(w, herr)
		return
	}
	httpx.OK(w, map[string]any{"ok": true})
}

// objectStoreTestStored runs the same check on a stored row (POST /service/{id}/object-store/test), with its stored key,
// which the API never returns to the console.
func (h *Handler) objectStoreTestStored(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	doc, err := h.repo.FindDoc(r.Context(), externalServiceCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, serviceNotFoundErr(id))
		return
	}
	es := h.objectStoreOf(doc)
	if es == nil {
		httpx.WriteError(w, httpx.BadRequest("This provider is not an object store"))
		return
	}
	if herr := checkObjectStore(r.Context(), es.Provider(), es.CephConfig("", "", "", "")); herr != nil {
		httpx.WriteError(w, herr)
		return
	}
	httpx.OK(w, map[string]any{"ok": true})
}

func checkObjectStore(ctx context.Context, provider string, cfg client.ObjectStoreConfig) *httpx.HTTPError {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.CheckObjectStore(ctx, provider, cfg); err != nil {
		return httpx.BadRequest("Object store check failed: " + err.Error())
	}
	return nil
}

// objectStoreOf reads a doc as an object-store provider with its secret decrypted; nil for any other provider.
func (h *Handler) objectStoreOf(doc pgdoc.M) *externalservice.ExternalService {
	cfg, _ := doc["config"].(map[string]any)
	es := &externalservice.ExternalService{Config: cfg, Secret: doc["secret"]}
	if !es.IsObjectStore() {
		return nil
	}
	if h.esSvc != nil {
		h.esSvc.Decrypt(es)
	}
	return es
}

// checkObjectStoreRow proves an object-store row of any status: bootstrap provisions every one that is not disabled, and
// a DISABLED row can be turned on later.
func (h *Handler) checkObjectStoreRow(ctx context.Context, doc pgdoc.M) *httpx.HTTPError {
	es := h.objectStoreOf(doc)
	if es == nil {
		return nil
	}
	return checkObjectStore(ctx, es.Provider(), es.CephConfig("", "", "", ""))
}

// objectStoreConnection is what checkObjectStoreRow's answer depends on, so a write that leaves it alone (quota, features)
// still works while the store is down.
func objectStoreConnection(doc pgdoc.M) string {
	cfg, _ := doc["config"].(map[string]any)
	b, _ := json.Marshal([]any{cfg["provider"], cfg["s3Endpoint"], cfg["adminApiUrl"], cfg["region"], doc["secret"]})
	return string(b)
}

// externalServiceUpdate is the update path (PUT /service/{id} — the Connection-tab Save): persist
// the edited connection fields (name/status/defaultPricePlan/config/secret); no live validation.
func (h *Handler) externalServiceUpdate(w http.ResponseWriter, r *http.Request) {
	// TODO(audit): UPDATE PLATFORM audit event
	h.serviceFieldSet(w, r, h.applyServiceConnectionBody)
}

// applyServiceConnectionBody merges the editable ExternalService fields from the request body onto
// the stored doc: name/status/defaultPricePlan, config (overlay top-level keys so fields the form
// omits — e.g. features/templates — are preserved), and secret (merge only NON-blank values so a
// masked/empty password field never clobbers the stored credential).
func (h *Handler) applyServiceConnectionBody(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
	var body map[string]any
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return httpx.BadRequest("Invalid request body")
	}
	for _, k := range []string{"name", "status", "defaultPricePlan"} {
		if v, ok := body[k]; ok {
			doc[k] = v
		}
	}
	if c, ok := body["config"].(map[string]any); ok {
		// Checked before the merge: a bad value must leave the stored doc as it was.
		if err := externalservice.ValidateAttachPolicy(c); err != nil {
			return httpx.BadRequest(err.Error())
		}
		cfg := ensureConfig(doc)
		for k, v := range c {
			cfg[k] = v
		}
		if _, ok := c["cluster"]; ok { // the body's cluster block replaced the stored one
			if herr := kamajiRemovedKeysErr(cfg); herr != nil {
				return herr
			}
		}
		if raw, ok := c["storageProfiles"]; ok && cfg["provider"] == "proxmox" {
			if herr := h.validatePVEStorageProfiles(req, cfg, raw); herr != nil {
				return herr
			}
		}
		if cfg["provider"] == "powerdns" {
			if _, err := powerdns.OwnershipResolvers(cfg["ownershipResolvers"]); err != nil {
				return httpx.BadRequest(err.Error())
			}
		}
		if cfg["provider"] == "truenas" {
			if _, err := externalservice.ParseSNATSources(c["snatSources"]); err != nil {
				return httpx.BadRequest(err.Error())
			}
		}
	}
	if s, ok := body["secret"].(map[string]any); ok {
		secret := ensureMap(doc, "secret")
		for k, v := range s {
			if str, isStr := v.(string); isStr && str == "" {
				continue // skip blank (masked password field) — keep the stored value
			}
			// Encrypt each NEWLY-supplied value before it lands in the datastore. Only new values are
			// encrypted — the stored (already-ciphertext) values are left untouched above, so a
			// re-save never double-encrypts them. Symmetric with the decrypt on read.
			if h.esSvc != nil {
				secret[k] = h.esSvc.EncryptSecret(v)
			} else {
				secret[k] = v
			}
		}
	}
	return nil
}

// externalServiceDelete runs the three in-use guards (exact 400 strings, in order: projects →
// users → cloud resources), then looks up by id (404 if absent) and deletes. The deletion is pure
// datastore (delete has NO live cloud teardown) → faithful. Returns a 200 with an empty body on
// success.
func (h *Handler) externalServiceDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	// In use if any project references this service via services.serviceId.
	inUse, err := h.repo.externalServiceInUse(r.Context(), "project", id)
	if httpx.WriteError(w, err) {
		return
	}
	if inUse {
		httpx.WriteError(w, httpx.BadRequest("External service is in use for some projects"))
		return
	}
	// In use if any user references this service via services.serviceId.
	inUse, err = h.repo.externalServiceInUse(r.Context(), "users", id)
	if httpx.WriteError(w, err) {
		return
	}
	if inUse {
		httpx.WriteError(w, httpx.BadRequest("External service is in use for some users"))
		return
	}
	// In use if any cloud resource references this service.
	inUse, err = h.repo.externalServiceInUse(r.Context(), "cloudResource", id)
	if httpx.WriteError(w, err) {
		return
	}
	if inUse {
		httpx.WriteError(w, httpx.BadRequest("External service is in use for some cloud resources"))
		return
	}
	// Look up by id (404 if absent), then delete (pure datastore, faithful).
	existing, err := h.repo.FindDoc(r.Context(), externalServiceCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, serviceNotFoundErr(id))
		return
	}
	if _, err := h.repo.DeleteDoc(r.Context(), externalServiceCollection, id); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): DELETE PLATFORM audit event
	// a 200 with no body.
	w.WriteHeader(http.StatusOK)
}

// externalServiceUpdateNoProvision handles PUT /{id}/update: look up by id (404 if absent) →
// persist the edited connection fields (same merge as the Connection-tab Save) → save. No cloud
// call (this is the no-provision update path).
func (h *Handler) externalServiceUpdateNoProvision(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, h.applyServiceConnectionBody)
}

// externalServiceUpdateQuota handles PUT /{id}/quota: config.provisioning.quota = body (the whole
// quota object is stored; nothing is pushed to the provider from here).
func (h *Handler) externalServiceUpdateQuota(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, func(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
		var body json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return httpx.BadRequest("Invalid request body")
		}
		cfg := ensureConfig(doc)
		prov := ensureMap(cfg, "provisioning")
		prov["quota"] = rawJSON(body)
		return nil
	})
}

// externalServiceUpdateFeatures handles PUT /{id}/features:
// config.features = body (+ config.enabledConsoleTypes = features.enabledConsoleTypes when present).
func (h *Handler) externalServiceUpdateFeatures(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, func(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return httpx.BadRequest("Invalid request body")
		}
		cfg := ensureConfig(doc)
		cfg["features"] = body
		// Copy enabledConsoleTypes onto config when the body carries it.
		if v, ok := body["enabledConsoleTypes"]; ok && v != nil {
			cfg["enabledConsoleTypes"] = v
		}
		return nil
	})
}

// externalServiceUpdateConfiguration handles PUT /{id}/configuration:
// name = body.name, status = body.status (the rest of the stored doc is preserved).
func (h *Handler) externalServiceUpdateConfiguration(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, func(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return httpx.BadRequest("Invalid request body")
		}
		doc["name"] = body["name"]
		doc["status"] = body["status"]
		return nil
	})
}

// externalServiceUpdateAttachPolicy handles PUT /{id}/attach-policy: {"attachToNewProjects": true|false} sets
// config.attachToNewProjects and nothing else, as a single-key write in the database (no read-modify-replace of the
// doc: a connection save landing at the same time is neither reverted nor reverts this). It is not a connection or
// status change, so the object-store and sandbox go-live checks do not apply. A missing or non-boolean value is a 400
// and stores nothing.
func (h *Handler) externalServiceUpdateAttachPolicy(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	v, herr := decodeAttachPolicy(r)
	if herr != nil {
		httpx.WriteError(w, herr)
		return
	}
	id := chi.URLParam(r, "id")
	ok, err := h.repo.SetAttachPolicy(r.Context(), id, v)
	if httpx.WriteError(w, err) {
		return
	}
	if !ok {
		httpx.WriteError(w, serviceNotFoundErr(id))
		return
	}
	doc, err := h.repo.FindDoc(r.Context(), externalServiceCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, serviceNotFoundErr(id))
		return
	}
	shapeExternalService(doc)
	httpx.OK(w, doc)
}

// decodeAttachPolicy reads {"attachToNewProjects": bool} and nothing else.
func decodeAttachPolicy(req *http.Request) (bool, *httpx.HTTPError) {
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(req.Body, 4<<10)).Decode(&body); err != nil {
		return false, httpx.BadRequest("Invalid request body")
	}
	for k := range body {
		if k != externalservice.AttachPolicyKey {
			return false, httpx.BadRequest("Only " + externalservice.AttachPolicyKey + " can be set here")
		}
	}
	v, ok := body[externalservice.AttachPolicyKey].(bool)
	if !ok {
		return false, httpx.BadRequest(externalservice.AttachPolicyKey + " must be true or false")
	}
	return v, nil
}

// SetAttachPolicy writes config.attachToNewProjects alone (jsonb_set on the stored doc; a doc without a config object
// gets one). False when there is no such provider.
func (r *Repo) SetAttachPolicy(ctx context.Context, id string, on bool) (bool, error) {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE "externalService" SET doc=jsonb_set(
	   CASE WHEN jsonb_typeof(doc->'config')='object' THEN doc ELSE jsonb_set(doc,'{config}','{}'::jsonb,true) END,
	   '{config,`+externalservice.AttachPolicyKey+`}',to_jsonb($2::boolean),true) WHERE id=$1`, id, on)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return false, nil
		}
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// externalServiceUpdateMetricsConfig handles PUT /{id}/metrics-config:
// config.metrics = {source, prometheus:{…}} plus, when supplied, the encrypted credential
// leaves secret.prometheusBasicPassword / secret.prometheusBearerToken (blank = leave the
// stored value unchanged, the connection-save convention; "-" = clear). The response strips
// `secret` via shapeExternalService like every other field-set PUT.
func (h *Handler) externalServiceUpdateMetricsConfig(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, h.applyMetricsConfig)
}

// applyMetricsConfig is the metrics-config apply closure, named so the validation matrix is
// unit-testable without the repo. Semantics: config.metrics is MERGED, not replaced — a
// source-only toggle (the natural payload for switching to none and back) must not
// discard the stored prometheus connection config. When the effective source is prometheus,
// a usable URL must exist (supplied now or already stored) — otherwise the failure would
// surface as hourly per-server job errors with traffic silently unbilled, exactly what the
// /metrics-test probe exists to prevent.
func (h *Handler) applyMetricsConfig(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
	var body struct {
		Source     string `json:"source"`
		Prometheus *struct {
			URL            string            `json:"url"`
			Schema         string            `json:"schema"`
			Headers        map[string]string `json:"headers"`
			BasicUser      string            `json:"basicUser"`
			InsecureTLS    bool              `json:"insecureTls"`
			CACert         string            `json:"caCert"`
			TimeoutSeconds int               `json:"timeoutSeconds"`
		} `json:"prometheus"`
		PrometheusAuth *struct {
			BasicPassword string `json:"basicPassword"`
			BearerToken   string `json:"bearerToken"`
		} `json:"prometheusAuth"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return httpx.BadRequest("Invalid request body")
	}
	switch body.Source {
	case externalservice.MetricsSourcePrometheus, externalservice.MetricsSourceNone, externalservice.MetricsSourcePVE:
	default:
		return httpx.BadRequest("source must be one of: pve, prometheus, none")
	}
	cfg := ensureConfig(doc)
	metricsCfg := ensureMap(cfg, "metrics")
	metricsCfg["source"] = body.Source
	if body.Prometheus != nil {
		p := body.Prometheus
		if p.URL != "" && !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
			return httpx.BadRequest("prometheus.url must be an http(s) URL")
		}
		// Credentials must go through prometheusAuth (encrypted at rest, stripped from admin
		// reads) — config.* is returned to read-only admins, so plaintext side-doors are
		// rejected: no userinfo in the URL, no Authorization-class headers.
		if u, err := url.Parse(p.URL); err == nil && u.User != nil {
			return httpx.BadRequest("prometheus.url must not embed credentials — use prometheusAuth")
		}
		switch p.Schema {
		case "", metrics.PromSchemaLibvirtExporter:
		default:
			return httpx.BadRequest("prometheus.schema must be one of: libvirt-exporter")
		}
		if p.TimeoutSeconds < 0 {
			return httpx.BadRequest("prometheus.timeoutSeconds must be zero (default) or positive")
		}
		headers := pgdoc.M{}
		for k, v := range p.Headers {
			if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Proxy-Authorization") {
				return httpx.BadRequest("prometheus.headers must not carry Authorization — use prometheusAuth")
			}
			if k != "" && v != "" {
				headers[k] = v
			}
		}
		metricsCfg["prometheus"] = pgdoc.M{
			"url": p.URL, "schema": p.Schema, "headers": headers,
			"basicUser": p.BasicUser, "insecureTls": p.InsecureTLS,
			"caCert": p.CACert, "timeoutSeconds": p.TimeoutSeconds,
		}
	}
	if body.Source == externalservice.MetricsSourcePrometheus && storedPromURL(metricsCfg) == "" {
		return httpx.BadRequest("prometheus.url is required when source is prometheus (none stored)")
	}
	if body.PrometheusAuth != nil {
		secret := ensureMap(doc, "secret")
		setSecretLeaf(h, secret, "prometheusBasicPassword", body.PrometheusAuth.BasicPassword)
		setSecretLeaf(h, secret, "prometheusBearerToken", body.PrometheusAuth.BearerToken)
	}
	return nil
}

// storedPromURL reads metrics.prometheus.url out of the (merged) metrics config map
// (pgdoc.M is a map[string]any alias, so this covers fresh writes and stored docs alike).
func storedPromURL(metricsCfg pgdoc.M) string {
	if p, ok := metricsCfg["prometheus"].(map[string]any); ok {
		s, _ := p["url"].(string)
		return s
	}
	return ""
}

// setSecretLeaf applies the blank-keeps / "-"-clears convention to one encrypted secret
// leaf: a non-blank value is encrypted and stored, blank leaves the stored value untouched,
// and the literal "-" removes it (so an operator can revoke a credential without pasting a
// replacement).
func setSecretLeaf(h *Handler, secret pgdoc.M, key, value string) {
	switch value {
	case "":
	case "-":
		delete(secret, key)
	default:
		if h.esSvc != nil {
			secret[key] = h.esSvc.EncryptSecret(value)
		} else {
			secret[key] = value
		}
	}
}

// serviceFieldSet is the shared body for every persisted field-set PUT (and the no-op /update):
// gate ADMIN_SERVICE_MANAGE → look up by id (404, exact "Service not found: %s") → apply the
// per-field mutation onto the stored doc → ReplaceDoc → return the shaped doc (shapeExternalService)
// with the secret stripped. The `apply` closure decodes the body and mutates the doc; a returned
// *HTTPError (e.g. a bad body) short-circuits before the find. This is a look-up → mutate → save.
func (h *Handler) serviceFieldSet(w http.ResponseWriter, r *http.Request, apply func(*http.Request, pgdoc.M) *httpx.HTTPError) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	doc, err := h.repo.FindDoc(r.Context(), externalServiceCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, serviceNotFoundErr(id))
		return
	}
	connection, status := objectStoreConnection(doc), doc["status"]
	if aerr := apply(r, doc); aerr != nil {
		httpx.WriteError(w, aerr)
		return
	}
	// Turning a service off must work with the store down; turning one on proves it, as its connection may predate the check.
	newConnection := objectStoreConnection(doc) != connection
	if newConnection || (doc["status"] != status && doc["status"] != externalservice.StatusDisabled) {
		if herr := h.checkObjectStoreRow(r.Context(), doc); herr != nil {
			httpx.WriteError(w, herr)
			return
		}
	}
	if newConnection {
		delete(doc, "provisionError") // it was about the old connection
	}
	// A sandbox provider stays or goes live only while its release gates hold.
	if herr := h.checkSandboxGoLive(r.Context(), doc); herr != nil {
		httpx.WriteError(w, herr)
		return
	}
	if err := h.repo.ReplaceDoc(r.Context(), externalServiceCollection, id, doc); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): CONFIGURE/UPDATE PLATFORM audit event {setting:...}
	shapeExternalService(doc)
	httpx.OK(w, doc)
}

// ensureConfig returns the doc's `config` sub-map, creating an empty one when absent or non-map
// (the stored config is always present in the practical post-create state, but we create-on-write
// to stay nil-safe).
func ensureConfig(doc pgdoc.M) pgdoc.M {
	return ensureMap(doc, "config")
}

// ensureMap returns parent[key] as a pgdoc.M, creating (and storing) an empty one when absent or not
// already a map. Handles both pgdoc.M and map[string]any (the latter from a freshly-decoded body).
func ensureMap(parent pgdoc.M, key string) pgdoc.M {
	switch m := parent[key].(type) {
	case map[string]any:
		converted := pgdoc.M(m)
		parent[key] = converted
		return converted
	default:
		m2 := pgdoc.M{}
		parent[key] = m2
		return m2
	}
}
