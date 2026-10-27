package admin

// externalservicemut.go serves the MUTATIONS of the external-service surface
// (/api/v1/admin/service). Every READ + cloud-live-read stub of this surface is ALREADY
// registered in handler.go (serviceList / serviceByID / openstackServices / projectServices /
// userServices / serviceOpenstackAuth, and the emptyCloudList stubs for os-images | volume/types |
// share/protocols | availability-zones | vhi/placement-quotas | public-networks). NONE of those is
// re-registered here — only the missing mutations are added.
//
// Flow:
//
//	create(es)                = validate (live keystone auto-fill) → encrypt → save → decrypt,
//	                            THEN post-create provisioning against OpenStack     [CLOUD, not wired]
//	update(id,es,prov)        = look up → validate (live) → re-provision,
//	                            THEN provisioning when requested                     [CLOUD, not wired]
//	delete(id)                = in-use guards (project/user/cloudResource → exact 400s)
//	                            THEN look up (404 if absent) → delete  [pure datastore — faithful]
//	PUT /{id}/update          = look up (404 if absent) → encrypt-and-save
//	                            [pure datastore no-op re-save — faithful]
//	the field-set PUTs        = look up (404 if absent) → mutate one OpenstackConfig (or secret) field →
//	                            encrypt-and-save  [pure datastore $set/replace — IN SCOPE, faithful]:
//	    /{id}/quota               provisioning.quota = body
//	    /{id}/features            config.features = body (+ enabledConsoleTypes when present)
//	    /{id}/configuration       name,status + config.openstackReseller = body's
//	    /{id}/volume/types        config.features.volumeTypes = body
//	    /{id}/share/protocols     config.features.shareProtocols = body
//	    /{id}/vhi/placement-quotas provisioning.quota.placementQuotas = body
//	    /{id}/reseller            config.openstackReseller = body
//	    /{id}/availability-zones  config.availabilityZones = body
//	    /{id}/gnocchi-granularity config.gnocchiGranularity = body.granularity
//	    /{id}/vhi-ostor           config.vhiOstorConfig (+ secret.vhiOstorAuth when present) = body
//
// EXTERNAL INTEGRATION POINTS (create / update): create and update both run a service validation →
// OpenStack field auto-fill, which performs a LIVE keystone authenticate against the configured
// region (sets adminUserId / adminDomainId from the live token, validates the domain) — there is
// NO clean datastore-only effect to persist before that live call (the validation IS the live call,
// and it mutates the doc with live-derived ids before save). Per the cloud-write rule these are not
// wired: 501, never live. The post-create/update provisioning against OpenStack is likewise live.
// The persisted field-set PUTs above touch ONLY stored config and are handled faithfully.
//
// The externalService doc carries secret.adminPassword (the OpenStack OS_PASSWORD); every response
// that returns the doc strips it via the existing shapeExternalService helper (handler.go).
//
// Mutations gate on ADMIN_SERVICE_MANAGE (admin:service:manage). Audit events are also written
// — deferred this pass (// TODO(audit)).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
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
	// create / update / delete — all CLOUD writes not wired except DELETE (pure datastore).
	r.Post("/service", h.externalServiceCreate)
	r.Put("/service/{id}", h.externalServiceUpdate)
	r.Delete("/service/{id}", h.externalServiceDelete)
	// live keystone discovery: populate config.regions + config.services from the token's catalog.
	r.Post("/service/{id}/discover", h.externalServiceDiscover)
	// the pure no-op re-save (PUT /{id}/update) — get-or-404 then resave.
	r.Put("/service/{id}/update", h.externalServiceUpdateNoProvision)
	// field-set PUTs — persist one stored config (or secret) field, then return the shaped doc.
	r.Put("/service/{id}/quota", h.externalServiceUpdateQuota)
	r.Put("/service/{id}/features", h.externalServiceUpdateFeatures)
	r.Put("/service/{id}/configuration", h.externalServiceUpdateConfiguration)
	r.Put("/service/{id}/volume/types", h.externalServiceUpdateVolumeTypes)
	r.Put("/service/{id}/share/protocols", h.externalServiceUpdateShareProtocols)
	r.Put("/service/{id}/vhi/placement-quotas", h.externalServiceUpdateVHIPlacementQuota)
	r.Put("/service/{id}/reseller", h.externalServiceUpdateReseller)
	r.Put("/service/{id}/availability-zones", h.externalServiceUpdateAvailabilityZones)
	r.Put("/service/{id}/gnocchi-granularity", h.externalServiceUpdateGnocchiGranularity)
	r.Put("/service/{id}/vhi-ostor", h.externalServiceUpdateVhiOstor)
	// usage-metrics source (gnocchi | prometheus | none) + prometheus connection config; the
	// POST probes the configured prometheus endpoint live (metricstest.go).
	r.Put("/service/{id}/metrics-config", h.externalServiceUpdateMetricsConfig)
	r.Post("/service/{id}/metrics-test", h.externalServiceMetricsTest)
}

// serviceNotFoundErr is the 404 returned when a service id is not found
// ("Service not found: %s", interpolated, no trailing space). All the field-set PUTs (and PUT
// /{id}/update) resolve the doc by id → this 404 when absent.
func serviceNotFoundErr(id string) *httpx.HTTPError {
	return httpx.NotFound(fmt.Sprintf("Service not found: %s", id))
}

// externalServiceCreate is the create path: validate → OpenStack field auto-fill (LIVE keystone
// authenticate, sets live-derived adminUserId/adminDomainId on the doc) → encrypt → save → decrypt,
// THEN live provisioning. The validation IS the live cloud call and it mutates the doc with
// live-derived ids before persisting, so there is no clean datastore-only effect to commit first →
// CLOUD write not wired (501).
func (h *Handler) externalServiceCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	// No-live adaptation (mirrors externalServiceUpdate): the create path would run a LIVE keystone
	// field auto-fill + provisioning. We PERSIST the operator-supplied doc and SKIP the live
	// validate+provision — the connection is then exercised by the detail page's Test connection +
	// the live cloud reads (os-images etc.) using the stored creds. ⚠ No live auto-fill of
	// adminUserId/adminDomainId.
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
	// Stable string _id (String-id convention — same shape as the seeded svc-openstack-dev
	// doc; projects reference the service by this id string).
	doc["_id"] = pgdoc.NewID()
	doc["createdAt"] = time.Now().UTC()
	// Encrypt the whole (all-new, plaintext) secret sub-document before it reaches the datastore so cloud
	// credentials are never stored at rest — symmetric with the decrypt on the read path.
	if sec, ok := doc["secret"]; ok && h.esSvc != nil {
		doc["secret"] = h.esSvc.EncryptSecret(sec)
	}
	if err := h.repo.InsertDocKeepID(r.Context(), externalServiceCollection, doc); httpx.WriteError(w, err) {
		return
	}
	// Best-effort live discovery: populate config.regions + config.services from the keystone
	// catalog so a UI-created provider is immediately usable (client menu + Location dropdown). A
	// cloud-unreachable / bad-cred failure is swallowed — create still succeeds and the operator can
	// re-run it via the Connection-tab Sync button (POST /service/{id}/discover).
	if id, _ := doc["_id"].(string); id != "" {
		h.enrichNewServiceFromCloud(r.Context(), doc, id)
	}
	// TODO(audit): write an admin audit event when a service is created.
	shapeExternalService(doc) // strips secret, _id→id
	httpx.OK(w, doc)
}

// externalServiceUpdate is the update path (PUT /service/{id} — the Connection-tab Save).
// The original re-runs a LIVE keystone auto-fill + re-provisioning; per the no-live constraint we
// PERSIST the edited connection fields (name/status/defaultPricePlan/config/secret) and SKIP the live
// validate+provision. This lets the admin change the OpenStack account/region via the UI; the real
// auth is then exercised by the live cloud reads (os-images etc.) using the stored creds.
// ⚠ No live auto-fill of adminUserId/adminDomainId — no-live adaptation.
func (h *Handler) externalServiceUpdate(w http.ResponseWriter, r *http.Request) {
	// TODO(audit): UPDATE PLATFORM audit event
	// TODO: live keystone field auto-fill + provisioning.
	h.serviceFieldSet(w, r, h.applyServiceConnectionBody)
}

// applyServiceConnectionBody merges the editable ExternalService fields from the request body onto
// the stored doc: name/status/defaultPricePlan, config (overlay top-level keys so fields the form
// omits — e.g. services/features — are preserved), and secret (merge only NON-blank values so a
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
		cfg := ensureConfig(doc)
		for k, v := range c {
			// `auth` carries the admin keystone SCOPE (adminProjectId/Name, domain, username). The
			// recovered admin FE re-sends this sub-object on every Services/Connection save, but with
			// BLANK scope fields when its form didn't load them — a wholesale replace then wipes the
			// stored scope (→ unscoped token → empty live catalog → the Services page goes blank, and
			// new-project bootstrap fails). Merge auth sub-keys instead, skipping blank strings (same
			// posture as the secret password field) so an omitted/blank field keeps its stored value.
			if k == "auth" {
				if newAuth, isMap := v.(map[string]any); isMap {
					existingAuth := ensureMap(cfg, "auth")
					for ak, av := range newAuth {
						if s, isStr := av.(string); isStr && s == "" {
							continue
						}
						existingAuth[ak] = av
					}
					continue
				}
			}
			cfg[k] = v
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

// externalServiceUpdateQuota handles PUT /{id}/quota: config.provisioning.quota = body. The whole
// quota object is stored (the live OpenStack quota push is a separate provider path NOT triggered
// by this endpoint → nothing deferred here).
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
// name = body.name, status = body.status, config.openstackReseller = body.config.openstackReseller.
// (Only these three are copied; the rest of the existing config is preserved — faithful.)
func (h *Handler) externalServiceUpdateConfiguration(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, func(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return httpx.BadRequest("Invalid request body")
		}
		doc["name"] = body["name"]
		doc["status"] = body["status"]
		cfg := ensureConfig(doc)
		var reseller any
		if bc, ok := body["config"].(map[string]any); ok {
			reseller = bc["openstackReseller"]
		}
		cfg["openstackReseller"] = reseller
		return nil
	})
}

// externalServiceUpdateVolumeTypes handles PUT /{id}/volume/types:
// config.features.volumeTypes = body (a map of region → volume-type list). The original writes into
// config.features directly (NPE if features is null — preserved: we set only when features exists,
// else create it, matching the practical post-create state where features is always initialized).
func (h *Handler) externalServiceUpdateVolumeTypes(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, func(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
		var body json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return httpx.BadRequest("Invalid request body")
		}
		cfg := ensureConfig(doc)
		feat := ensureMap(cfg, "features")
		feat["volumeTypes"] = rawJSON(body)
		return nil
	})
}

// externalServiceUpdateShareProtocols handles PUT /{id}/share/protocols:
// config.features.shareProtocols = body (creating features when null).
func (h *Handler) externalServiceUpdateShareProtocols(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, func(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
		var body json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return httpx.BadRequest("Invalid request body")
		}
		cfg := ensureConfig(doc)
		feat := ensureMap(cfg, "features")
		feat["shareProtocols"] = rawJSON(body)
		return nil
	})
}

// externalServiceUpdateVHIPlacementQuota handles PUT /{id}/vhi/placement-quotas:
// config.provisioning.quota.placementQuotas = body (creating the quota object when null). This
// stores the quotas; it does NOT push to OpenStack from this endpoint (the live VHI placement push
// is a separate provider path) → nothing deferred.
func (h *Handler) externalServiceUpdateVHIPlacementQuota(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, func(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
		var body json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return httpx.BadRequest("Invalid request body")
		}
		cfg := ensureConfig(doc)
		prov := ensureMap(cfg, "provisioning")
		quota := ensureMap(prov, "quota")
		quota["placementQuotas"] = rawJSON(body)
		return nil
	})
}

// externalServiceUpdateReseller handles PUT /{id}/reseller: config.openstackReseller = body.
func (h *Handler) externalServiceUpdateReseller(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, func(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
		var body json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return httpx.BadRequest("Invalid request body")
		}
		cfg := ensureConfig(doc)
		cfg["openstackReseller"] = rawJSON(body)
		return nil
	})
}

// externalServiceUpdateAvailabilityZones handles PUT /{id}/availability-zones:
// config.availabilityZones = body (the stored map; distinct from the live GET
// /{id}/availability-zones which queries OpenStack — that read is the emptyCloudList stub in
// handler.go). Stores only → nothing deferred.
func (h *Handler) externalServiceUpdateAvailabilityZones(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, func(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
		var body json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return httpx.BadRequest("Invalid request body")
		}
		cfg := ensureConfig(doc)
		cfg["availabilityZones"] = rawJSON(body)
		return nil
	})
}

// externalServiceUpdateGnocchiGranularity handles PUT /{id}/gnocchi-granularity:
// config.gnocchiGranularity = body.granularity (an int).
func (h *Handler) externalServiceUpdateGnocchiGranularity(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, func(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
		var body struct {
			Granularity int `json:"granularity"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return httpx.BadRequest("Invalid request body")
		}
		cfg := ensureConfig(doc)
		cfg["gnocchiGranularity"] = body.Granularity
		return nil
	})
}

// externalServiceUpdateVhiOstor handles PUT /{id}/vhi-ostor:
// config.vhiOstorConfig = body.vhiOstorConfig, and when body.vhiOstorAuth is present, merge its
// non-blank accessKey/secretKey into secret.vhiOstorAuth (creating it when null). ⚠ This writes the
// stored secret; the response strips `secret` via shapeExternalService (no credential leak).
func (h *Handler) externalServiceUpdateVhiOstor(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, func(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
		var body struct {
			VhiOstorConfig json.RawMessage `json:"vhiOstorConfig"`
			VhiOstorAuth   *struct {
				AccessKey string `json:"accessKey"`
				SecretKey string `json:"secretKey"`
			} `json:"vhiOstorAuth"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return httpx.BadRequest("Invalid request body")
		}
		cfg := ensureConfig(doc)
		cfg["vhiOstorConfig"] = rawJSON(body.VhiOstorConfig)
		// When the body carries vhiOstorAuth, merge its non-blank keys into the secret. Encrypt each
		// newly-supplied value before it lands in the datastore (symmetric with the decrypt on read —
		// externalservice.Service.decrypt), matching the create + connection-save secret paths above.
		if body.VhiOstorAuth != nil {
			secret := ensureMap(doc, "secret")
			auth := ensureMap(secret, "vhiOstorAuth")
			if body.VhiOstorAuth.AccessKey != "" {
				if h.esSvc != nil {
					auth["accessKey"] = h.esSvc.EncryptSecret(body.VhiOstorAuth.AccessKey)
				} else {
					auth["accessKey"] = body.VhiOstorAuth.AccessKey
				}
			}
			if body.VhiOstorAuth.SecretKey != "" {
				if h.esSvc != nil {
					auth["secretKey"] = h.esSvc.EncryptSecret(body.VhiOstorAuth.SecretKey)
				} else {
					auth["secretKey"] = body.VhiOstorAuth.SecretKey
				}
			}
		}
		return nil
	})
}

// externalServiceUpdateMetricsConfig handles PUT /{id}/metrics-config:
// config.metrics = {source, prometheus:{…}} plus, when supplied, the encrypted credential
// leaves secret.prometheusBasicPassword / secret.prometheusBearerToken (blank = leave the
// stored value unchanged, the vhi-ostor convention; "-" = clear). The response strips
// `secret` via shapeExternalService like every other field-set PUT.
func (h *Handler) externalServiceUpdateMetricsConfig(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, h.applyMetricsConfig)
}

// applyMetricsConfig is the metrics-config apply closure, named so the validation matrix is
// unit-testable without the repo. Semantics: config.metrics is MERGED, not replaced — a
// source-only toggle (the natural payload for switching to none/gnocchi and back) must not
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
	case externalservice.MetricsSourceGnocchi, externalservice.MetricsSourcePrometheus, externalservice.MetricsSourceNone:
	default:
		return httpx.BadRequest("source must be one of: gnocchi, prometheus, none")
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
		case "", metrics.PromSchemaLibvirtExporter, metrics.PromSchemaCeilometerPushgw, metrics.PromSchemaCeilometerExporter:
		default:
			return httpx.BadRequest("prometheus.schema must be one of: libvirt-exporter, ceilometer-pushgateway, ceilometer-exporter")
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
	if aerr := apply(r, doc); aerr != nil {
		httpx.WriteError(w, aerr)
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
