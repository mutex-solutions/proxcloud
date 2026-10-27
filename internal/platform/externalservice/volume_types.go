package externalservice

import "strings"

// VolumeTypeConfig is the public, non-secret storage class an operator exposes
// to projects in one region. Name is the real Cinder volume-type name while
// DisplayName is the product label rendered by clients.
type VolumeTypeConfig struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

// SoleRegion returns the provider's only declared region (config.regions),
// or "" when none or several are declared. Callers use it as the fallback
// when a request carries no region: project service rows do not record one
// and the deploy-level default is a dev-bootstrap value that production
// leaves unset.
func (e *ExternalService) SoleRegion() string {
	if e == nil {
		return ""
	}
	regions, _ := e.Config["regions"].(map[string]any)
	if len(regions) != 1 {
		return ""
	}
	for name := range regions {
		return name
	}
	return ""
}

// HasRegion reports whether the provider declares the region in config.regions.
func (e *ExternalService) HasRegion(name string) bool {
	if e == nil || name == "" {
		return false
	}
	regions, _ := e.Config["regions"].(map[string]any)
	_, ok := regions[name]
	return ok
}

// CatalogRegion picks the region whose curated catalog a request should see.
// A requested region is honored only when the provider declares it (the value
// arrives from the browser's x-region-id header — never trusted raw); anything
// else falls back to the provider's sole region, and a multi-region provider
// with no usable region resolves to "" so catalog lookups fail closed.
func (e *ExternalService) CatalogRegion(requested string) string {
	if e.HasRegion(requested) {
		return requested
	}
	return e.SoleRegion()
}

// EnabledVolumeTypes returns only config.features.volumeTypes[region] entries
// explicitly enabled by an operator. It intentionally fails closed: a missing
// region, an absent catalog, or an all-disabled catalog exposes no storage
// types to tenant-facing create paths.
func (e *ExternalService) EnabledVolumeTypes(region string) []VolumeTypeConfig {
	if e == nil || region == "" {
		return []VolumeTypeConfig{}
	}
	features, _ := e.Config["features"].(map[string]any)
	byRegion, _ := features["volumeTypes"].(map[string]any)
	raw := byRegion[region]

	entries := make([]map[string]any, 0)
	switch rows := raw.(type) {
	case []any:
		for _, row := range rows {
			if item, ok := row.(map[string]any); ok {
				entries = append(entries, item)
			}
		}
	case []map[string]any:
		entries = append(entries, rows...)
	}

	out := make([]VolumeTypeConfig, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, row := range entries {
		enabled, _ := row["enabled"].(bool)
		name, _ := row["name"].(string)
		name = strings.TrimSpace(name)
		if !enabled || name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		displayName, _ := row["displayName"].(string)
		displayName = strings.TrimSpace(displayName)
		if displayName == "" {
			displayName = name
		}
		seen[name] = struct{}{}
		out = append(out, VolumeTypeConfig{Name: name, DisplayName: displayName})
	}
	return out
}
