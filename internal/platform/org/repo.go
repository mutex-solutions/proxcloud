package org

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Repo backs the organization + organization_members tables + a project
// count (the project domain lands in a later slice; we only count here).
type Repo struct {
	orgs     *pgdoc.Store
	members  *pgdoc.Store
	projects *pgdoc.Store
	roles    *pgdoc.Store
}

func NewRepo(db *pgdoc.DB) *Repo {
	return &Repo{
		orgs:     db.C("organization"),
		members:  db.C("organization_members"),
		projects: db.C("project"),
		roles:    db.C("roleDefinition"),
	}
}

func (r *Repo) EnsureIndexes(ctx context.Context) error {
	for _, s := range []*pgdoc.Store{r.orgs, r.members, r.projects, r.roles} {
		if err := s.Ensure(ctx); err != nil {
			return err
		}
	}
	if err := r.members.EnsureIndex(ctx, "org_sub_unique", true,
		pgdoc.F("organizationId"), pgdoc.F("sub")); err != nil {
		return err
	}
	if err := r.members.EnsureIndex(ctx, "sub", false, pgdoc.F("sub")); err != nil {
		return err
	}
	// roleDefinition: unique compound index on (organizationId, name).
	return r.roles.EnsureIndex(ctx, "org_name_unique", true,
		pgdoc.F("organizationId"), pgdoc.F("name"))
}

// --- organizations ---

func (r *Repo) Insert(ctx context.Context, o *Organization) (*Organization, error) {
	now := time.Now().UTC()
	o.CreatedAt, o.UpdatedAt = &now, &now
	id, err := r.orgs.InsertOne(ctx, o)
	if err != nil {
		return nil, err
	}
	o.ID = id
	return o, nil
}

func (r *Repo) Save(ctx context.Context, o *Organization) error {
	now := time.Now().UTC()
	o.UpdatedAt = &now
	_, err := r.orgs.SetByID(ctx, o.ID, pgdoc.M{
		"name": o.Name, "description": o.Description, "billingProfileId": o.BillingProfileID,
		"customInfo": o.CustomInfo, "updatedAt": now,
	}, nil)
	return err
}

func (r *Repo) FindByID(ctx context.Context, id string) (*Organization, error) {
	var o Organization
	found, err := r.orgs.Get(ctx, id, &o)
	if err != nil || !found {
		return nil, err // unknown/malformed id => not found
	}
	return &o, nil
}

// FindAllByBillingProfileID returns every org referencing a billing profile.
// Normally 0 or 1; the caller filters by membership and takes the first
// (see getOrganizationForBillingProfile).
func (r *Repo) FindAllByBillingProfileID(ctx context.Context, bpID string) ([]Organization, error) {
	var out []Organization
	if err := r.orgs.Find(ctx, pgdoc.M{"billingProfileId": bpID}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repo) FindByIDs(ctx context.Context, ids []string) ([]Organization, error) {
	if len(ids) == 0 {
		return []Organization{}, nil
	}
	var out []Organization
	if err := r.orgs.Find(ctx, pgdoc.M{"_id": pgdoc.M{"$in": ids}}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repo) Delete(ctx context.Context, id string) error {
	_, err := r.orgs.DeleteByID(ctx, id)
	return err
}

func (r *Repo) CountProjects(ctx context.Context, orgID string) (int64, error) {
	return r.projects.Count(ctx, pgdoc.M{"organizationId": orgID})
}

// --- members ---

func (r *Repo) AddMember(ctx context.Context, orgID, sub, role string) (*Member, error) {
	now := time.Now().UTC()
	m := &Member{OrganizationID: orgID, Sub: sub, Roles: []string{role}, CreatedAt: &now, UpdatedAt: &now}
	id, err := r.members.InsertOne(ctx, m)
	if err != nil {
		return nil, err
	}
	m.ID = id
	return m, nil
}

func (r *Repo) FindMember(ctx context.Context, orgID, sub string) (*Member, error) {
	var m Member
	found, err := r.members.FindOne(ctx, pgdoc.M{"organizationId": orgID, "sub": sub}, &m)
	if err != nil || !found {
		return nil, err
	}
	return &m, nil
}

func (r *Repo) Members(ctx context.Context, orgID string) ([]Member, error) {
	var out []Member
	if err := r.members.Find(ctx, pgdoc.M{"organizationId": orgID}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repo) OrgIDsForSub(ctx context.Context, sub string) ([]string, error) {
	ms, err := r.MembersForSub(ctx, sub)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.OrganizationID)
	}
	return ids, nil
}

// MembersForSub returns every membership of sub (across orgs), so callers can
// filter by role (e.g. the project list's OWNER/ADMIN org-visibility rule).
func (r *Repo) MembersForSub(ctx context.Context, sub string) ([]Member, error) {
	var ms []Member
	if err := r.members.Find(ctx, pgdoc.M{"sub": sub}, &ms); err != nil {
		return nil, err
	}
	return ms, nil
}

func (r *Repo) UpdateMemberRole(ctx context.Context, orgID, sub, role string) error {
	_, err := r.members.SetFieldsOne(ctx, pgdoc.M{"organizationId": orgID, "sub": sub},
		pgdoc.M{"roles": []string{role}, "updatedAt": time.Now().UTC()}, nil)
	return err
}

func (r *Repo) RemoveMember(ctx context.Context, orgID, sub string) error {
	_, err := r.members.DeleteOne(ctx, pgdoc.M{"organizationId": orgID, "sub": sub})
	return err
}

func (r *Repo) DeleteAllMembers(ctx context.Context, orgID string) error {
	_, err := r.members.DeleteMany(ctx, pgdoc.M{"organizationId": orgID})
	return err
}

// --- custom roles (roleDefinition) ---

func (r *Repo) InsertRole(ctx context.Context, role *Role) (*Role, error) {
	now := time.Now().UTC()
	role.CreatedAt, role.UpdatedAt = &now, &now
	id, err := r.roles.InsertOne(ctx, role)
	if err != nil {
		return nil, err
	}
	role.ID = id
	return role, nil
}

// SaveRole persists the mutable role fields (description + permissions).
func (r *Repo) SaveRole(ctx context.Context, role *Role) error {
	now := time.Now().UTC()
	role.UpdatedAt = &now
	_, err := r.roles.SetByID(ctx, role.ID, pgdoc.M{
		"description": role.Description, "permissions": role.Permissions, "updatedAt": now,
	}, nil)
	return err
}

func (r *Repo) FindRoleByID(ctx context.Context, id string) (*Role, error) {
	var role Role
	found, err := r.roles.Get(ctx, id, &role)
	if err != nil || !found {
		return nil, err
	}
	return &role, nil
}

func (r *Repo) FindRoleByOrgAndName(ctx context.Context, orgID, name string) (*Role, error) {
	var role Role
	found, err := r.roles.FindOne(ctx, pgdoc.M{"organizationId": orgID, "name": name}, &role)
	if err != nil || !found {
		return nil, err
	}
	return &role, nil
}

func (r *Repo) ExistsRoleByOrgAndName(ctx context.Context, orgID, name string) (bool, error) {
	return r.roles.Exists(ctx, pgdoc.M{"organizationId": orgID, "name": name})
}

func (r *Repo) RolesByOrg(ctx context.Context, orgID string) ([]Role, error) {
	var out []Role
	if err := r.roles.Find(ctx, pgdoc.M{"organizationId": orgID}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repo) DeleteRole(ctx context.Context, id string) error {
	_, err := r.roles.DeleteByID(ctx, id)
	return err
}

func (r *Repo) DeleteAllRoles(ctx context.Context, orgID string) error {
	_, err := r.roles.DeleteMany(ctx, pgdoc.M{"organizationId": orgID})
	return err
}

// RolePermissions returns the raw permission patterns of a custom role by name,
// or [] if not found.
func (r *Repo) RolePermissions(ctx context.Context, orgID, name string) ([]string, error) {
	role, err := r.FindRoleByOrgAndName(ctx, orgID, name)
	if err != nil || role == nil {
		return []string{}, err
	}
	return role.Permissions, nil
}
