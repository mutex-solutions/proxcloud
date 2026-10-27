package platformconfig

// Dto is the client-facing view of the platform config. Null fields are omitted;
// regions are only included for authenticated callers.
type Dto struct {
	ID                string                `json:"id"`
	Branding          *Branding             `json:"branding,omitempty"`
	Regions           []RegionDisplayConfig `json:"regions,omitempty"`
	DateConfiguration *DateFormat           `json:"dateConfiguration,omitempty"`
	AuthStrategy      string                `json:"authStrategy,omitempty"`
}

// toDto builds the DTO: id="default",
// dateConfiguration + branding + authStrategy always; regions only when authed.
func toDto(c *PlatformConfiguration, authenticated bool, authStrategy string) Dto {
	d := Dto{
		ID:                "default",
		Branding:          c.Branding,
		DateConfiguration: c.DateConfiguration,
		AuthStrategy:      authStrategy,
	}
	if authenticated {
		d.Regions = c.Regions
	}
	return d
}
