package cloud

import "time"

// History is the cloudResourceHistory archive record written on
// delete. `cloudResourceId` is the ORIGINAL CloudResource.id (the dedupe key for the
// idempotent archive). archive() copies only: cloudResourceId, region, serviceId, type,
// data, createdAt, externalId, projectId (NOT pricePlan/ephemeralData). `deletedAt` is the
// archive timestamp (set at insert).
type History struct {
	ID              string         `json:"id,omitempty"`
	CloudResourceID string         `json:"cloudResourceId,omitempty"`
	ProjectID       string         `json:"projectId,omitempty"`
	ExternalID      string         `json:"externalId,omitempty"`
	ServiceID       string         `json:"serviceId,omitempty"`
	Type            string         `json:"type,omitempty"`
	Region          string         `json:"region,omitempty"`
	Data            map[string]any `json:"data,omitempty"`
	CreatedAt       *time.Time     `json:"createdAt,omitempty"`
	DeletedAt       *time.Time     `json:"deletedAt,omitempty"`
}
