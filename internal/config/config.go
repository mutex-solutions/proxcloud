// Package config loads ProxCloud runtime configuration in a way that is
// wire-compatible with the deployment contract: a mounted application.yml
// overlaid with a small set of injected environment variables.
//
// The Helm chart (deploy/charts/proxcloud) mounts application.yml at
// /opt/proxcloud/api/application.yml and injects PROXCLOUD_DB_URL,
// PROXCLOUD_RABBITMQ_USERNAME/PASSWORD and PROXCLOUD_ENCRYPTION_DEFAULT_KEY as env
// vars. We reproduce exactly that merge so the chart values/secrets work
// unchanged.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/billingprovider"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// DefaultConfigPath is where the chart mounts application.yml.
const DefaultConfigPath = "/opt/proxcloud/api/application.yml"

type OAuth2Realm struct {
	IssuerURI string
	ClientID  string
	// DiscoveryURI is where THIS process fetches the OIDC metadata + JWKS when that
	// differs from the issuer the browser (and therefore the token's `iss`) uses —
	// split-horizon DNS, a reverse proxy, or a container network where the IdP is
	// reachable under an internal name. Empty = fetch from IssuerURI (the default).
	// The issuer in the token is still enforced against IssuerURI.
	DiscoveryURI string
}

type Config struct {
	Server struct {
		Port int
	}
	Management struct {
		Port int
	}
	DB struct {
		URL string // PostgreSQL DSN (primary datastore)
	}
	Rabbit struct {
		Host     string
		Port     int
		Username string
		Password string
	}
	Encryption struct {
		DefaultKey string
	}
	Self struct {
		BaseURL      string
		APIBaseURL   string
		UIBaseURL    string
		AdminBaseURL string
	}
	Auth struct {
		Main     OAuth2Realm
		Admin    OAuth2Realm
		AdminAPI OAuth2Realm
	}
	// SSO is per-organization single sign-on. An empty ClientSecret leaves the feature off: it needs
	// a Keycloak service account in the customer realm to create identity providers.
	SSO struct {
		ClientID        string
		ClientSecret    string
		ReservedDomains []string // never claimable by an organization
		AllowInternal   bool     // development only: IdPs on http and on private addresses
	}
	// DefaultRegion is the region assumed when a provider doc names none
	// (PROXCLOUD_DEFAULT_REGION / proxcloud.default-region, default "default").
	DefaultRegion string
	// Jobs gates the in-process scheduled jobs (charge cron + cloud metrics ingestion).
	// OFF by default: a plain deploy stays dormant until the gated live rating run flips
	// PROXCLOUD_JOBS_SCHEDULER_ENABLED=true (so deploys never charge bills unexpectedly).
	Jobs struct {
		SchedulerEnabled bool // auto-start the charge/metrics crons (charges bills on a timer)
		// DebugTriggers exposes the on-demand mgmt triggers (/debug/run-{sync,metrics,charge})
		// WITHOUT auto-starting the crons — so a deploy can be driven deterministically for the
		// golden run while staying dormant (no timed bill charging).
		DebugTriggers bool
		// RabbitFanout routes the charge cron through RabbitMQ (one message per ACTIVE
		// profile → a consumer per pod) instead of the in-process loop. Default off.
		RabbitFanout bool
	}
	// Billing selects which engine charges bills, and — when that is an external service — how to
	// reach it.
	Billing struct {
		// Provider is "native" (default) or "external".
		//
		// native   charges bills with the billing engine in this repository. It needs nothing but
		//          PostgreSQL, and it is what a plain checkout of proxcloud runs: a complete billing
		//          system out of the box.
		// external delegates rating to a separate billing service. ProxCloud still resolves what to
		//          bill — it owns the cloud resource cache — and POSTs the resolved resources
		//          to URL. Requires URL and Token.
		//
		// Only the charge step changes. Every other billing surface behaves identically.
		Provider string
		// URL is the external billing service (in-cluster, e.g.
		// http://cloud-billing.billing.svc.cluster.local:8080). Ignored by the native provider.
		URL string
		// Timeout bounds one billing call. A charge carries every billable resource for a profile,
		// so it is deliberately generous compared to an interactive request.
		Timeout time.Duration
		// Token is the shared bearer token for the billing service's internal API. It must match
		// that service's CLOUD_INTERNAL_TOKEN; without it the charge calls get a 401.
		Token string
		// CallbackToken is the shared secret the billing service presents when calling back to
		// suspend/resume clouds, activate projects or send mail. Empty → those callbacks are not
		// mounted at all. They can pause a customer's whole fleet, so they are never exposed
		// unauthenticated, network policy notwithstanding.
		CallbackToken string
	}
	// CSIGateway is the listener proxmox-csi controllers of tenant clusters call instead of PVE (internal/cloud/csigw).
	// Empty Addr: not served. TLS only, so a cert and key are required with an address.
	CSIGateway struct {
		Addr    string
		TLSCert string
		TLSKey  string
	}
	LogLevel string
}

// Load reads application.yml (if present) and overlays the env vars the chart
// injects. Env always wins over the file.
func Load() (*Config, error) {
	k := koanf.New(".")

	path := envOr("PROXCLOUD_CONFIG_FILE", DefaultConfigPath)
	if _, err := os.Stat(path); err == nil {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("load %s: %w", path, err)
		}
	}

	c := &Config{}

	// Ports (yaml: server.port / management.server.port).
	c.Server.Port = intOr(k.Int("server.port"), 8080)
	c.Management.Port = intOr(k.Int("management.server.port"), 8081)

	// PostgreSQL — the primary datastore; injected purely via env by the chart.
	c.DB.URL = envOr("PROXCLOUD_DB_URL", k.String("db.url"))

	// RabbitMQ — host/port from yaml, creds from env.
	c.Rabbit.Host = envOr("PROXCLOUD_RABBITMQ_HOST", k.String("rabbitmq.host"))
	c.Rabbit.Port = intOr(envInt("PROXCLOUD_RABBITMQ_PORT"), intOr(k.Int("rabbitmq.port"), 5672))
	c.Rabbit.Username = envOr("PROXCLOUD_RABBITMQ_USERNAME", k.String("rabbitmq.username"))
	c.Rabbit.Password = envOr("PROXCLOUD_RABBITMQ_PASSWORD", k.String("rabbitmq.password"))

	// Data-at-rest key (env). The config binds proxcloud.encryption.default.key (config
	// prefix "proxcloud.encryption.default", field "key") — read that path too, then the
	// dashed alias, so the key resolves from the same chart value either way.
	c.Encryption.DefaultKey = envOr("PROXCLOUD_ENCRYPTION_DEFAULT_KEY",
		firstNonEmpty(k.String("proxcloud.encryption.default.key"), k.String("proxcloud.encryption.default-key")))

	// Per-organization SSO.
	c.SSO.ClientID = envOr("PROXCLOUD_SSO_KC_CLIENT_ID", "proxcloud-idp-admin")
	c.SSO.ClientSecret = os.Getenv("PROXCLOUD_SSO_KC_CLIENT_SECRET")
	c.SSO.ReservedDomains = strings.Split(os.Getenv("PROXCLOUD_SSO_RESERVED_DOMAINS"), ",")
	c.SSO.AllowInternal = os.Getenv("PROXCLOUD_SSO_ALLOW_INTERNAL_IDP") == "true"

	// Self URLs.
	c.Self.BaseURL = envOr("PROXCLOUD_SELF_BASE_URL", k.String("proxcloud.self.base-url"))
	c.Self.APIBaseURL = envOr("PROXCLOUD_SELF_API_BASE_URL", k.String("proxcloud.self.api-base-url"))
	c.Self.UIBaseURL = envOr("PROXCLOUD_SELF_UI_BASE_URL", k.String("proxcloud.self.ui-base-url"))
	c.Self.AdminBaseURL = envOr("PROXCLOUD_SELF_ADMIN_BASE_URL", k.String("proxcloud.self.admin-base-url"))

	// Auth realms (yaml; issuer-uri may be empty when a realm uses LOCAL_IDP).
	// Env overrides let the test point Go at the in-cluster Keycloak service
	// URL so the token's `iss` (Host-derived) matches what the pod resolves.
	c.Auth.Main = realmEnv(k, "auth.main.oauth2", "AUTH_MAIN_OAUTH2")
	c.Auth.Admin = realmEnv(k, "auth.admin.oauth2", "AUTH_ADMIN_OAUTH2")
	c.Auth.AdminAPI = realmEnv(k, "auth.admin-api.oauth2", "AUTH_ADMIN_API_OAUTH2")

	c.DefaultRegion = firstNonEmpty(envOr("PROXCLOUD_DEFAULT_REGION", k.String("proxcloud.default-region")), "default")

	// Scheduled jobs (charge cron + cloud metrics) — OFF unless explicitly enabled.
	c.Jobs.SchedulerEnabled = boolEnv("PROXCLOUD_JOBS_SCHEDULER_ENABLED") || k.Bool("proxcloud.jobs.scheduler-enabled")
	c.Jobs.DebugTriggers = boolEnv("PROXCLOUD_JOBS_DEBUG_TRIGGERS") || k.Bool("proxcloud.jobs.debug-triggers")
	c.Jobs.RabbitFanout = boolEnv("PROXCLOUD_JOBS_RABBIT_FANOUT") || k.Bool("proxcloud.jobs.rabbit-fanout")
	// Billing provider. Unset → native, so an existing deployment keeps charging exactly as before.
	c.Billing.Provider = billingprovider.Normalize(
		envOr("PROXCLOUD_BILLING_PROVIDER", k.String("proxcloud.billing.provider")))

	c.Billing.URL = firstNonEmpty(os.Getenv("PROXCLOUD_BILLING_URL"), k.String("proxcloud.billing.url"))
	c.Billing.Timeout = 60 * time.Second
	if s := os.Getenv("PROXCLOUD_BILLING_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			c.Billing.Timeout = d
		}
	}
	c.Billing.Token = envOr("PROXCLOUD_BILLING_TOKEN", k.String("proxcloud.billing.token"))
	c.Billing.CallbackToken = envOr("PROXCLOUD_BILLING_CALLBACK_TOKEN", k.String("proxcloud.billing.callback-token"))

	c.CSIGateway.Addr = envOr("PROXCLOUD_CSI_GATEWAY_ADDR", k.String("proxcloud.csi-gateway.addr"))
	c.CSIGateway.TLSCert = envOr("PROXCLOUD_CSI_GATEWAY_TLS_CERT", k.String("proxcloud.csi-gateway.tls-cert"))
	c.CSIGateway.TLSKey = envOr("PROXCLOUD_CSI_GATEWAY_TLS_KEY", k.String("proxcloud.csi-gateway.tls-key"))

	c.LogLevel = strings.ToUpper(firstNonEmpty(k.String("logging.level.root"), "INFO"))

	return c, nil
}

// Validate fails fast on missing required config — follows the fail-closed
// bootstrap principle (a half-configured service must not serve traffic).
func (c *Config) Validate() error {
	var missing []string
	if c.DB.URL == "" {
		missing = append(missing, "PROXCLOUD_DB_URL")
	}
	if c.Rabbit.Host == "" {
		missing = append(missing, "rabbitmq.host / PROXCLOUD_RABBITMQ_HOST")
	}
	// Remote charging without a billing URL would silently stop charging bills altogether — the
	// cron would fire, find nowhere to send the work, and log. Fail closed instead.
	// An unknown provider name is a typo, and silently defaulting one would charge bills with an
	// engine the operator did not choose.
	if err := billingprovider.Validate(c.Billing.Provider); err != nil {
		return fmt.Errorf("invalid billing configuration: %w", err)
	}
	// The external provider cannot work without somewhere to send the charge, and a cron that
	// fires, finds nowhere to send it and logs is a silent billing outage.
	if c.Billing.Provider == billingprovider.External {
		if c.Billing.URL == "" {
			missing = append(missing, `PROXCLOUD_BILLING_URL (required when PROXCLOUD_BILLING_PROVIDER=external)`)
		}
		if c.Billing.Token == "" {
			missing = append(missing, `PROXCLOUD_BILLING_TOKEN (required when PROXCLOUD_BILLING_PROVIDER=external)`)
		}
	}
	if c.CSIGateway.Addr != "" && (c.CSIGateway.TLSCert == "" || c.CSIGateway.TLSKey == "") {
		missing = append(missing, "PROXCLOUD_CSI_GATEWAY_TLS_CERT and PROXCLOUD_CSI_GATEWAY_TLS_KEY (required with PROXCLOUD_CSI_GATEWAY_ADDR)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	return nil
}

func realmEnv(k *koanf.Koanf, yamlPrefix, envPrefix string) OAuth2Realm {
	return OAuth2Realm{
		IssuerURI:    envOr(envPrefix+"_ISSUER_URI", k.String(yamlPrefix+".issuer-uri")),
		ClientID:     envOr(envPrefix+"_CLIENT_ID", k.String(yamlPrefix+".client-id")),
		DiscoveryURI: envOr(envPrefix+"_DISCOVERY_URI", k.String(yamlPrefix+".discovery-uri")),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string) int {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	var n int
	_, _ = fmt.Sscanf(v, "%d", &n)
	return n
}

func boolEnv(key string) bool {
	return strings.EqualFold(os.Getenv(key), "true")
}

func intOr(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
