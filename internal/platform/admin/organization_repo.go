package admin

// organization_repo.go holds the Organization-specific repo methods the generic crud.go / admin.go
// helpers do not cover: org reads, the organization_members membership operations
// (the members live in a SEPARATE `organization_members` collection — {organizationId, sub, roles}),
// project-count, and a users-by-_id resolve. Covers the organization, membership, project, and
// user reads the organization admin endpoints need.

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// OrgFindByID loads an organization by id;
// returns (nil,nil) when absent (the caller maps that to 404 "Organization not found").
func (r *Repo) OrgFindByID(ctx context.Context, id string) (pgdoc.M, error) {
	return r.FindDoc(ctx, "organization", id)
}

// OrgInsert inserts a new organization doc (the store assigns the id) and returns it
// with `_id` set. Any caller-supplied id is dropped so the store generates the key.
func (r *Repo) OrgInsert(ctx context.Context, doc pgdoc.M) (pgdoc.M, error) {
	return r.InsertDoc(ctx, "organization", doc)
}

// OrgReplace replaces an organization doc by id (id-preserving): the caller's map is written as-is
// minus id/_id/_class.
func (r *Repo) OrgReplace(ctx context.Context, id string, doc pgdoc.M) error {
	return r.ReplaceDoc(ctx, "organization", id, doc)
}

// OrgDelete deletes an organization doc by id.
func (r *Repo) OrgDelete(ctx context.Context, id string) error {
	_, err := r.c("organization").DeleteByID(ctx, id)
	return err
}

// ProjectCountByOrganizationID counts projects by organizationId (the organizationId on
// a project is the org's hex string id).
func (r *Repo) ProjectCountByOrganizationID(ctx context.Context, orgID string) (int64, error) {
	return r.c("project").Count(ctx, pgdoc.M{"organizationId": orgID})
}

// OrgMembers returns all membership docs for an organization.
func (r *Repo) OrgMembers(ctx context.Context, orgID string) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := r.c("organization_members").Find(ctx, pgdoc.M{"organizationId": orgID}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// OrgMemberCount returns the membership count for an organization.
func (r *Repo) OrgMemberCount(ctx context.Context, orgID string) (int64, error) {
	return r.c("organization_members").Count(ctx, pgdoc.M{"organizationId": orgID})
}

// OrgMember returns the membership for (org,sub); (nil,nil) when absent.
func (r *Repo) OrgMember(ctx context.Context, orgID, sub string) (pgdoc.M, error) {
	var doc pgdoc.M
	found, err := r.c("organization_members").FindOne(ctx,
		pgdoc.M{"organizationId": orgID, "sub": sub}, &doc)
	if err != nil || !found {
		return nil, err
	}
	return doc, nil
}

// orgMemberRole returns the membership's first role (roles[0], else "").
func orgMemberRole(member pgdoc.M) string {
	roles, ok := member["roles"].(pgdoc.A)
	if !ok || len(roles) == 0 {
		return ""
	}
	if s, ok := roles[0].(string); ok {
		return s
	}
	return ""
}

// OrgAddMember inserts a membership doc with roles=[role]
// (the single role wrapped in a list).
func (r *Repo) OrgAddMember(ctx context.Context, orgID, sub, role string) error {
	roles := pgdoc.A{}
	if role != "" {
		roles = pgdoc.A{role}
	}
	_, err := r.c("organization_members").InsertOne(ctx,
		pgdoc.M{"organizationId": orgID, "sub": sub, "roles": roles})
	return err
}

// OrgUpdateMemberRole sets roles=[newRole] for the
// (org,sub) membership.
func (r *Repo) OrgUpdateMemberRole(ctx context.Context, orgID, sub, newRole string) error {
	roles := pgdoc.A{}
	if newRole != "" {
		roles = pgdoc.A{newRole}
	}
	_, err := r.c("organization_members").SetFieldsOne(ctx,
		pgdoc.M{"organizationId": orgID, "sub": sub},
		pgdoc.M{"roles": roles}, nil)
	return err
}

// OrgRemoveMember deletes the (org,sub) membership.
func (r *Repo) OrgRemoveMember(ctx context.Context, orgID, sub string) error {
	_, err := r.c("organization_members").DeleteOne(ctx,
		pgdoc.M{"organizationId": orgID, "sub": sub})
	return err
}

// OrgDeleteAllMembers deletes every membership for an organization.
func (r *Repo) OrgDeleteAllMembers(ctx context.Context, orgID string) error {
	_, err := r.c("organization_members").DeleteMany(ctx, pgdoc.M{"organizationId": orgID})
	return err
}

// UserByID loads a user by id; returns (nil,nil) for a malformed/absent id.
func (r *Repo) UserByID(ctx context.Context, id string) (pgdoc.M, error) {
	return r.FindDoc(ctx, "users", id)
}

// BillingProfileByIDRaw loads a billing profile by id,
// used to populate OrganizationDto.billingProfile and to validate a supplied billingProfileId.
// (nil,nil) when absent.
func (r *Repo) BillingProfileByIDRaw(ctx context.Context, id string) (pgdoc.M, error) {
	if id == "" {
		return nil, nil
	}
	return r.FindDoc(ctx, "billingProfile", id)
}

// userSub reads the sub from a raw users doc (nil-safe).
func userSub(doc pgdoc.M) string {
	if doc == nil {
		return ""
	}
	if s, ok := doc["sub"].(string); ok {
		return s
	}
	return ""
}
