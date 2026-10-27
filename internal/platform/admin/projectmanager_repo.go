package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// projectmanager_repo.go: the project-membership helpers + repo methods the ProjectManager admin
// mutations need. Memberships are EMBEDDED in the project doc as `memberships:[{sub,role}]`.

// projectHasMember reports whether a project doc already has a membership for sub.
func projectHasMember(proj pgdoc.M, sub string) bool {
	_, found := projectMemberRole(proj, sub)
	return found
}

// projectMemberRole returns the role of sub's membership in a project doc (and whether it exists).
func projectMemberRole(proj pgdoc.M, sub string) (string, bool) {
	raw, ok := proj["memberships"]
	if !ok {
		return "", false
	}
	arr, ok := raw.(pgdoc.A)
	if !ok {
		return "", false
	}
	for _, m := range arr {
		mm, ok := m.(pgdoc.M)
		if !ok {
			continue
		}
		if s, _ := mm["sub"].(string); s == sub {
			role, _ := mm["role"].(string)
			return role, true
		}
	}
	return "", false
}

// addProjectMembership appends a {sub,role} membership to the project's embedded memberships array
// (memberships += {sub,role}, then save).
func (r *Repo) addProjectMembership(ctx context.Context, projectID, sub, role string) error {
	_, err := r.c(projectCollection).PushToArray(ctx, pgdoc.M{"_id": projectID},
		"memberships", pgdoc.M{"sub": sub, "role": role})
	return err
}

// removeProjectMembership removes sub's membership from the project's embedded memberships array
// (removeIf(sub==user.sub), then save).
func (r *Repo) removeProjectMembership(ctx context.Context, projectID, sub string) error {
	_, err := r.c(projectCollection).PullFromArray(ctx, pgdoc.M{"_id": projectID},
		"memberships", pgdoc.M{"sub": sub})
	return err
}
