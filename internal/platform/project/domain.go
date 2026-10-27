// Package project implements the Project slice: project CRUD + memberships +
// RBAC. Unlike the org slice (computed OrganizationDto), the single-project
// endpoints return the RAW Project domain JSON, so the on-the-wire shape is the
// Project document exactly (memberships are embedded in the project doc, not a
// side collection). Cloud/billing-usage endpoints are deferred.
package project

import (
	"encoding/json"
	"time"
)

// ProjectStatus values.
const (
	StatusEnabled              = "ENABLED"
	StatusDisabled             = "DISABLED"
	StatusScheduledForDeletion = "SCHEDULED_FOR_DELETION"
	StatusDeleteInProgress     = "DELETE_IN_PROGRESS"
)

// MembershipRole values (project memberships only know OWNER/MEMBER — no ADMIN
// at project level).
const (
	RoleOwner  = "OWNER"
	RoleMember = "MEMBER"
)

// Membership is one embedded entry in Project.memberships.
type Membership struct {
	Sub  string `json:"sub"`
	Role string `json:"role"`
}

// Project is the `project` document. Memberships + services are embedded.
// Money-free at this layer (billing lands elsewhere), so no money fields here.
type Project struct {
	ID               string         `json:"id,omitempty"`
	Name             string         `json:"name"`
	Status           string         `json:"status"`
	Data             any            `json:"data,omitempty"`
	Owner            string         `json:"owner,omitempty"` // deprecated but still serialized
	Memberships      []Membership   `json:"memberships"`
	OrganizationID   string         `json:"organizationId"`
	BillingProfileID string         `json:"billingProfileId,omitempty"`
	CustomInfo       map[string]any `json:"customInfo,omitempty"`
	Services         []any          `json:"services"`
	// PublicNetworkIds is the admin-managed external-network allow-list: nil/absent = ALL
	// router:external networks allowed (the default); non-nil = only these Neutron network ids
	// (empty = none).
	PublicNetworkIds []string `json:"publicNetworkIds,omitempty"`
	// PublicNetworksVisible controls whether the client may CHOOSE the external network for a
	// floating IP / router gateway. false (the default, incl. absent) = the client sees no picker
	// and the server auto-selects an allowed external network on the user's behalf; true = the
	// client picks explicitly.
	PublicNetworksVisible bool `json:"publicNetworksVisible"`
	// GpuCapacityVisible controls whether the client dashboard shows the region's cluster GPU
	// capacity (available/total per model, from Placement). false (the default, incl. absent) =
	// hidden; the client still sees its own GPU quota/usage regardless.
	GpuCapacityVisible bool `json:"gpuCapacityVisible"`
	// Quota is the admin-managed per-project quota config (stored JSON; enforced at the
	// ProxCloud cloud gate, never pushed to OpenStack). Shape: {"gpu": {"<model>": n, "*": n}}.
	Quota                  map[string]any `json:"quota,omitempty"`
	ScheduledForDeletionAt *time.Time     `json:"scheduledForDeletionAt,omitempty"`
	CreatedAt              *time.Time     `json:"createdAt,omitempty"`
	UpdatedAt              *time.Time     `json:"updatedAt,omitempty"`
}

// MarshalJSON serializes Project with NULL fields OMITTED (data, billingProfileId,
// scheduledForDeletionAt when unset), while non-null EMPTY collections are KEPT —
// customInfo:{}, services:[], memberships:[] are always present (omit null, not
// empty).
func (p Project) MarshalJSON() ([]byte, error) {
	ci := p.CustomInfo
	if ci == nil {
		ci = map[string]any{}
	}
	ms := p.Memberships
	if ms == nil {
		ms = []Membership{}
	}
	svc := p.Services
	if svc == nil {
		svc = []any{}
	}
	return json.Marshal(struct {
		ID                     string         `json:"id,omitempty"`
		Name                   string         `json:"name"`
		Status                 string         `json:"status"`
		Data                   any            `json:"data,omitempty"`
		Owner                  string         `json:"owner,omitempty"`
		Memberships            []Membership   `json:"memberships"`
		OrganizationID         string         `json:"organizationId"`
		BillingProfileID       string         `json:"billingProfileId,omitempty"`
		CustomInfo             map[string]any `json:"customInfo"`
		Services               []any          `json:"services"`
		PublicNetworkIds       []string       `json:"publicNetworkIds,omitempty"`
		PublicNetworksVisible  bool           `json:"publicNetworksVisible"`
		Quota                  map[string]any `json:"quota,omitempty"`
		ScheduledForDeletionAt *time.Time     `json:"scheduledForDeletionAt,omitempty"`
		CreatedAt              *time.Time     `json:"createdAt,omitempty"`
		UpdatedAt              *time.Time     `json:"updatedAt,omitempty"`
	}{
		ID: p.ID, Name: p.Name, Status: p.Status, Data: p.Data, Owner: p.Owner,
		Memberships: ms, OrganizationID: p.OrganizationID, BillingProfileID: p.BillingProfileID,
		CustomInfo: ci, Services: svc, PublicNetworkIds: p.PublicNetworkIds,
		PublicNetworksVisible: p.PublicNetworksVisible, Quota: p.Quota,
		ScheduledForDeletionAt: p.ScheduledForDeletionAt,
		CreatedAt:              p.CreatedAt, UpdatedAt: p.UpdatedAt,
	})
}

// IsUserOwner reports whether sub is a member with the OWNER role.
func (p *Project) IsUserOwner(sub string) bool {
	for _, m := range p.Memberships {
		if m.Sub == sub && m.Role == RoleOwner {
			return true
		}
	}
	return false
}

// IsMember reports whether sub is in the memberships (any role).
func (p *Project) IsMember(sub string) bool {
	for _, m := range p.Memberships {
		if m.Sub == sub {
			return true
		}
	}
	return false
}

// IsDisabled / IsEnabled are the status helpers.
func (p *Project) IsDisabled() bool { return p.Status == StatusDisabled }
func (p *Project) IsEnabled() bool  { return p.Status == StatusEnabled }

// HasServices reports whether the project has any attached external services
// (non-nil and non-empty).
func (p *Project) HasServices() bool { return len(p.Services) > 0 }

// ServiceIDs returns the serviceId of each attached ProjectExternalService (the link to
// ExternalService.id). Services are embedded free-form (map[string]any, whether built in code or
// decoded from the store), so the id is read by key.
func (p *Project) ServiceIDs() []string {
	out := make([]string, 0, len(p.Services))
	for _, svc := range p.Services {
		if t, ok := svc.(map[string]any); ok {
			if id, _ := t["serviceId"].(string); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// svcField reads a string field from one embedded ProjectExternalService element (a free-form
// map[string]any, like ServiceIDs).
func svcField(svc any, key string) string {
	if t, ok := svc.(map[string]any); ok {
		s, _ := t[key].(string)
		return s
	}
	return ""
}

// ExternalProjectID returns the provisioned OpenStack tenant id (externalProjectId) for the given
// external service, or "" when the project is not (yet) bootstrapped on it. Bulk-imported
// projects carry the tenant under config.openstackProjectId instead (the import shape) —
// read that as the fallback so sync/tenant-scoping work for both shapes.
func (p *Project) ExternalProjectID(serviceID string) string {
	for _, svc := range p.Services {
		if svcField(svc, "serviceId") == serviceID {
			if id := svcField(svc, "externalProjectId"); id != "" {
				return id
			}
			if t, ok := svc.(map[string]any); ok {
				if cfg, ok := t["config"].(map[string]any); ok {
					s, _ := cfg["openstackProjectId"].(string)
					return s
				}
			}
			return ""
		}
	}
	return ""
}

// ServiceRegion returns the region the project is provisioned in for the given service.
func (p *Project) ServiceRegion(serviceID string) string {
	for _, svc := range p.Services {
		if svcField(svc, "serviceId") == serviceID {
			return svcField(svc, "region")
		}
	}
	return ""
}

// ServicePool is the persisted PVE pool of a proxmox service binding (bootstrap stamps it;
// teardown prefers it over re-deriving — the RGWUIDFor discipline).
func (p *Project) ServicePool(serviceID string) string {
	for _, svc := range p.Services {
		if svcField(svc, "serviceId") == serviceID {
			return svcField(svc, "pool")
		}
	}
	return ""
}

// HasService reports whether the project is attached to the given external service id.
func (p *Project) HasService(serviceID string) bool {
	for _, id := range p.ServiceIDs() {
		if id == serviceID {
			return true
		}
	}
	return false
}
