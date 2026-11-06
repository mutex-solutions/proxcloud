// Package externalservice backs the `externalService` table: each provider's connection
// (proxmox, ceph-s3, kamaji, dbaas) plus the encrypted `secret` (decrypted on read).
//
// config/secret are polymorphic free-form sub-documents, modeled as map[string]any with typed
// per-provider accessors over the known keys.
package externalservice

import (
	"github.com/mutex-solutions/proxcloud/internal/cloud/client"
	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
)

// ExternalServiceType values. Only CLOUD matters to the cloud slice.
const (
	TypeCloud   = "CLOUD"
	TypeCPanel  = "CPANEL"
	TypePayment = "PAYMENT"
)

// ExternalServiceStatus values.
const (
	StatusPublic   = "PUBLIC"
	StatusPrivate  = "PRIVATE"
	StatusDisabled = "DISABLED"
)

// ExternalService is a document in the "externalService" collection. `config` and `secret`
// are free-form sub-documents; `secret` is stored encrypted and is decrypted in-place by the
// Service on read (Service.decrypt → textcrypt.DecryptObject).
type ExternalService struct {
	ID               string         `json:"id,omitempty"`
	Name             string         `json:"name,omitempty"`
	DefaultPricePlan string         `json:"defaultPricePlan,omitempty"`
	Type             string         `json:"type,omitempty"`
	Status           string         `json:"status,omitempty"`
	Config           map[string]any `json:"config,omitempty"`
	// Secret is stored (encrypted at rest) and decrypted into memory on read. It is NEVER put on the
	// wire: this struct is not serialized to any response — admin reads go through a secret-stripped
	// path and cloud ops use it only in-memory.
	Secret any `json:"secret,omitempty"`
}

// IsDisabled / IsNotDisabled are the status helpers.
func (e *ExternalService) IsDisabled() bool    { return e.Status == StatusDisabled }
func (e *ExternalService) IsNotDisabled() bool { return !e.IsDisabled() }

// secretMap returns the decrypted secret as a map (nil if absent/not a map).
func (e *ExternalService) secretMap() map[string]any {
	m, _ := e.Secret.(map[string]any)
	return m
}

// Provider is config.provider; Shared is config.shared.
func (e *ExternalService) Provider() string { return str(e.Config["provider"]) }
func (e *ExternalService) Shared() bool     { return boolean(e.Config["shared"]) }

// IsCephS3 reports whether this is a Ceph RGW (S3) object-store provider (config.provider == "ceph-s3").
// Object-store is the only service it serves, driven by the S3 + Admin Ops endpoints below.
func (e *ExternalService) IsCephS3() bool { return e.Provider() == "ceph-s3" }

// S3Endpoint / AdminAPIURL are the ceph-s3 data + Admin Ops endpoints (config.s3Endpoint / config.adminApiUrl).
func (e *ExternalService) S3Endpoint() string  { return str(e.Config["s3Endpoint"]) }
func (e *ExternalService) AdminAPIURL() string { return str(e.Config["adminApiUrl"]) }

// S3WebsiteEndpoint is the RGW s3website endpoint (config.s3WebsiteEndpoint), i.e. the host behind
// rgw_dns_s3website_name. Buckets are served virtual-hosted at <bucket>.<thisHost>. Empty = the provider
// does not offer static website hosting.
func (e *ExternalService) S3WebsiteEndpoint() string { return str(e.Config["s3WebsiteEndpoint"]) }

// CephRegion is config.region (the RGW zonegroup used for SigV4), falling back to the first configured region.
func (e *ExternalService) CephRegion() string {
	if r := str(e.Config["region"]); r != "" {
		return r
	}
	if rs := e.RegionNames(); len(rs) > 0 {
		return rs[0]
	}
	return ""
}

// DefaultRGWUIDPrefix is the per-project RGW user id prefix used when config.uidPrefix is absent.
const DefaultRGWUIDPrefix = "proxcloud-"

// UIDPrefix is the prefix on the per-project RGW user id: config.uidPrefix when the key is set (an
// explicit "" means no prefix, e.g. users already provisioned without one), else DefaultRGWUIDPrefix.
func (e *ExternalService) UIDPrefix() string {
	if v, ok := e.Config["uidPrefix"]; ok && v != nil {
		return str(v)
	}
	return DefaultRGWUIDPrefix
}

// DefaultQuotaGiB is the per-project storage quota to set at provision (config.defaultQuotaGiB; 0 = unset).
func (e *ExternalService) DefaultQuotaGiB() int {
	n, _ := intFrom(e.Config["defaultQuotaGiB"])
	return n
}

// RGWUIDFor is the canonical per-project RGW user id: config.uidPrefix + projectID. This is the ONE place
// it is derived — bootstrap persists this exact value on the binding + credential, and every ceph client
// scopes its admin-ops calls (user + bucket list) by it. Buckets live in RGW's DEFAULT tenant and are
// isolated by OWNERSHIP by this user, not by an RGW tenant.
func (e *ExternalService) RGWUIDFor(projectID string) string {
	return e.UIDPrefix() + projectID
}

// cephAdminKeys returns the decrypted RGW admin S3 keys (secret.adminAccessKey / secret.adminSecretKey).
func (e *ExternalService) cephAdminKeys() (access, secret string) {
	m := e.secretMap()
	return str(m["adminAccessKey"]), str(m["adminSecretKey"])
}

// CephConfig assembles the ceph-s3 connection config for client.NewCephS3. projectAccess/Secret are the
// per-project RGW user keys (empty → an admin-only client: list/stat/provision, no data I/O).
//
// rgwUID must ALREADY be the final user id — use RGWUIDFor(projectID) to derive it, or pass the value
// persisted on the credential/binding. The uidPrefix is NOT re-applied here, or a caller passing a stored
// (already-prefixed) uid would double-prefix it.
func (e *ExternalService) CephConfig(region, projectAccess, projectSecret, rgwUID string) client.CephConfig {
	if region == "" {
		region = e.CephRegion()
	}
	adminAccess, adminSecret := e.cephAdminKeys()
	var quotaBytes int64
	if g := e.DefaultQuotaGiB(); g > 0 {
		quotaBytes = int64(g) * 1073741824
	}
	return client.CephConfig{
		S3Endpoint:        e.S3Endpoint(),
		S3WebsiteEndpoint: e.S3WebsiteEndpoint(),
		AdminURL:          e.AdminAPIURL(),
		Region:            region,
		AdminAccessKey:    adminAccess,
		AdminSecretKey:    adminSecret,
		ProjectAccessKey:  projectAccess,
		ProjectSecretKey:  projectSecret,
		RGWUID:            rgwUID,
		DefaultQuotaBytes: quotaBytes,
	}
}

// Metrics-source values (config.metrics.source). Absent/blank = none: the metrics job skips
// the provider's servers silently instead of failing per-server per-hour against a
// telemetry-less cloud.
const (
	MetricsSourcePrometheus = "prometheus"
	MetricsSourceNone       = "none"
	// MetricsSourcePVE is the Proxmox default: public-NIC counters off status/current.
	MetricsSourcePVE = "pve"
)

// metricsConfig returns the config.metrics sub-map (nil-safe).
func (e *ExternalService) metricsConfig() map[string]any {
	m, _ := e.Config["metrics"].(map[string]any)
	return m
}

// MetricsSource returns config.metrics.source, defaulting to the native counters on a proxmox
// provider and to none on every other.
func (e *ExternalService) MetricsSource() string {
	if s := str(e.metricsConfig()["source"]); s != "" {
		return s
	}
	if e.IsProxmox() {
		return MetricsSourcePVE
	}
	return MetricsSourceNone
}

// PrometheusMetricsConfig assembles the Prometheus usage-source config from
// config.metrics.prometheus.* plus the DECRYPTED credential leaves
// (secret.prometheusBasicPassword / secret.prometheusBearerToken — encrypted at rest and
// stripped from admin reads like every other secret field; the basic-auth USERNAME is not a
// secret and lives in config).
func (e *ExternalService) PrometheusMetricsConfig() metrics.PrometheusConfig {
	p, _ := e.metricsConfig()["prometheus"].(map[string]any)
	headers := map[string]string{}
	if hs, ok := p["headers"].(map[string]any); ok {
		for k, v := range hs {
			if s := str(v); k != "" && s != "" {
				headers[k] = s
			}
		}
	}
	timeout, _ := intFrom(p["timeoutSeconds"])
	secret := e.secretMap()
	return metrics.PrometheusConfig{
		URL:            str(p["url"]),
		Schema:         str(p["schema"]),
		Headers:        headers,
		BasicUser:      str(p["basicUser"]),
		BasicPassword:  str(secret["prometheusBasicPassword"]),
		BearerToken:    str(secret["prometheusBearerToken"]),
		InsecureTLS:    boolean(p["insecureTls"]),
		CACert:         str(p["caCert"]),
		TimeoutSeconds: timeout,
	}
}

// RegionNames returns the keys of config.regions (the regions this service serves).
func (e *ExternalService) RegionNames() []string {
	regions, ok := e.Config["regions"].(map[string]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(regions))
	for k := range regions {
		out = append(out, k)
	}
	return out
}

// ServiceEnabledInRegion reports whether config.services[slug][region] is toggled on —
// the same admin Services-tab map the client menu gates on. A provider with NO services
// map at all says yes to everything (legacy docs predate the map; sync everything rather
// than nothing).
func (e *ExternalService) ServiceEnabledInRegion(slug, region string) bool {
	svcs, ok := e.Config["services"].(map[string]any)
	if !ok || len(svcs) == 0 {
		return true
	}
	regions, _ := svcs[slug].(map[string]any)
	b, _ := regions[region].(bool)
	return b
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolean(v any) bool {
	b, _ := v.(bool)
	return b
}

func intFrom(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}
