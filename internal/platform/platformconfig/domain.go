// Package platformconfig is the PlatformConfiguration slice: the single
// per-deployment platform config doc (branding, date format, regions, project
// quota, login config) that the UI bootstraps from and that gates project quota.
// Admin CRUD (master-realm auth) is deferred.
package platformconfig

import "time"

// Branding is the platform branding config. The nested links/theme/scripts objects
// are kept as opaque maps for now (the seed leaves them unset → omitted).
type Branding struct {
	Name       string         `json:"name,omitempty"`
	FaviconURL string         `json:"faviconUrl,omitempty"`
	Logo       string         `json:"logo,omitempty"`
	DarkLogo   string         `json:"darkLogo,omitempty"`
	EmailLogo  string         `json:"emailLogo,omitempty"`
	Code       string         `json:"code,omitempty"`
	Color      string         `json:"color,omitempty"`
	Links      map[string]any `json:"links,omitempty"`
	Theme      map[string]any `json:"theme,omitempty"`
	Scripts    map[string]any `json:"scripts,omitempty"`
}

// DateFormat is the platform date-format config.
type DateFormat struct {
	DateFormat string `json:"dateFormat,omitempty"`
}

// RegionDisplayConfig is one region's display entry.
type RegionDisplayConfig struct {
	ServiceID string `json:"serviceId"`
	Region    string `json:"region"`
	Order     int    `json:"order"`
}

// ProvisioningQuota is an {enabled, limit} gate (server-side only; not in the
// client DTO). limit 0 with enabled=true blocks self-service creation entirely
// (operators create/assign instead).
type ProvisioningQuota struct {
	Enabled bool `json:"enabled"`
	Limit   int  `json:"limit"`
}

// ProjectProvisioningQuota keeps the original name for the project gate.
type ProjectProvisioningQuota = ProvisioningQuota

// PlatformConfiguration is the document in the "platformConfiguration" collection.
type PlatformConfiguration struct {
	ID                       string                    `json:"id,omitempty"`
	Name                     string                    `json:"name,omitempty"`
	Language                 string                    `json:"language,omitempty"`
	MailGatewayID            string                    `json:"mailGatewayId,omitempty"`
	Branding                 *Branding                 `json:"branding,omitempty"`
	ContactIntegrationID     string                    `json:"contactIntegrationId,omitempty"`
	SegmentIntegrationID     string                    `json:"segmentIntegrationId,omitempty"`
	DefaultConfiguration     bool                      `json:"defaultConfiguration"`
	Regions                  []RegionDisplayConfig     `json:"regions,omitempty"`
	DateConfiguration        *DateFormat               `json:"dateConfiguration,omitempty"`
	ProjectProvisioningQuota *ProjectProvisioningQuota `json:"projectProvisioningQuota,omitempty"`
	// Per-user cap on self-created organizations (enabled + limit; 0 = only
	// operators create orgs and assign members). Read by org.CreateOrganization.
	OrganizationProvisioningQuota *ProvisioningQuota `json:"organizationProvisioningQuota,omitempty"`
	LoginConfiguration            map[string]any     `json:"loginConfiguration,omitempty"`
	// HiddenServices are service slugs (kubernetes, database, object-store, …) the operator
	// hides from every tenant: /init drops them from the menu, so the client nav and create
	// pages disappear. The providers keep syncing; this is a display switch only.
	HiddenServices []string   `json:"hiddenServices,omitempty"`
	CreatedAt      *time.Time `json:"createdAt,omitempty"`
	UpdatedAt      *time.Time `json:"updatedAt,omitempty"`
}
