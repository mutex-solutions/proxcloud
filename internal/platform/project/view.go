package project

import (
	"encoding/json"
	"time"
)

// ResourceCount is a {type,count} entry on ProjectView. Populated from
// the per-type cloud-resource counts; empty for now (no cloud resources yet).
type ResourceCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// ProjectView is the projection returned by the list endpoint (a trimmed
// Project + resource counts). resourcesCount + memberships are always present
// arrays (never null).
type ProjectView struct {
	ID               string          `json:"id,omitempty"`
	Name             string          `json:"name"`
	Status           string          `json:"status"`
	BillingProfileID string          `json:"billingProfileId,omitempty"`
	OrganizationID   string          `json:"organizationId"`
	Memberships      []Membership    `json:"memberships"`
	CreatedAt        *time.Time      `json:"createdAt,omitempty"`
	ResourcesCount   []ResourceCount `json:"resourcesCount,omitempty"`
	// PublicNetworksVisible mirrors the project flag so the client UI knows whether to show the
	// external-network picker (true) or leave it to the server's auto-pick (false/default).
	PublicNetworksVisible bool `json:"publicNetworksVisible"`
	// GpuCapacityVisible mirrors the project flag so the client dashboard only fetches/shows the
	// region GPU-capacity panel when the operator enabled it (false/default = hidden).
	GpuCapacityVisible bool `json:"gpuCapacityVisible"`
}

// MarshalJSON omits null fields: null billingProfileId is OMITTED, but the
// non-null empty arrays memberships:[] and resourcesCount:[] are KEPT (omit
// null, not empty).
func (v ProjectView) MarshalJSON() ([]byte, error) {
	ms := v.Memberships
	if ms == nil {
		ms = []Membership{}
	}
	rc := v.ResourcesCount
	if rc == nil {
		rc = []ResourceCount{}
	}
	return json.Marshal(struct {
		ID                    string          `json:"id,omitempty"`
		Name                  string          `json:"name"`
		Status                string          `json:"status"`
		BillingProfileID      string          `json:"billingProfileId,omitempty"`
		OrganizationID        string          `json:"organizationId"`
		Memberships           []Membership    `json:"memberships"`
		CreatedAt             *time.Time      `json:"createdAt,omitempty"`
		ResourcesCount        []ResourceCount `json:"resourcesCount"`
		PublicNetworksVisible bool            `json:"publicNetworksVisible"`
		GpuCapacityVisible    bool            `json:"gpuCapacityVisible"`
	}{
		ID: v.ID, Name: v.Name, Status: v.Status, BillingProfileID: v.BillingProfileID,
		OrganizationID: v.OrganizationID, Memberships: ms, CreatedAt: v.CreatedAt, ResourcesCount: rc,
		PublicNetworksVisible: v.PublicNetworksVisible, GpuCapacityVisible: v.GpuCapacityVisible,
	})
}

// ProjectUser is the member response (member with profile fields). Null profile
// fields are omitted via omitempty (a resolved member has all fields set anyway).
type ProjectUser struct {
	UserID    string `json:"userId,omitempty"`
	Sub       string `json:"sub"`
	FirstName string `json:"firstName,omitempty"`
	LastName  string `json:"lastName,omitempty"`
	Email     string `json:"email,omitempty"`
	Role      string `json:"role,omitempty"`
}
