package admin

// organizationrole_repo.go holds the OrganizationRole-specific repo methods the generic crud.go
// helpers do not cover: list-by-organization and the exists-by-org+name uniqueness check (an EXACT
// match — the name is already upper-cased by the caller before the check).

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// OrganizationRolesByOrganization returns every role
// document for the organization, as raw documents (never nil). The caller shapes each one for the
// response.
func (r *Repo) OrganizationRolesByOrganization(ctx context.Context, organizationID string) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := r.c(organizationRoleCollection).Find(ctx, pgdoc.M{"organizationId": organizationID}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// OrganizationRoleExistsByName reports true
// when a role with the exact (organizationId, name) pair exists. The name is matched
// literally (case-sensitive) because the caller upper-cases it before calling.
func (r *Repo) OrganizationRoleExistsByName(ctx context.Context, organizationID, name string) (bool, error) {
	return r.c(organizationRoleCollection).Exists(ctx, pgdoc.M{"organizationId": organizationID, "name": name})
}
