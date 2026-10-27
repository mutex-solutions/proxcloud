//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
)

// An ORG OWNER/ADMIN can reach every project in their org even without an explicit
// project membership (parity with the project list), while a non-owner org member or a
// stranger is still denied. Also covers editing a project member's role.
func TestProjectAccess_OrgOwnerInheritsAndRoleEdit(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	orgRepo := org.NewRepo(db)
	projRepo := project.NewRepo(db)
	if err := orgRepo.EnsureIndexes(ctx); err != nil {
		t.Fatalf("ensure indexes: %v", err)
	}
	svc := project.NewService(projRepo, orgRepo, billing.NewRepo(db), user.NewRepo(db), nil)

	o, err := orgRepo.Insert(ctx, &org.Organization{Name: "Acme", CustomInfo: map[string]any{}})
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := orgRepo.AddMember(ctx, o.ID, "owner1", rbac.RoleOwner); err != nil { // org owner, NOT on the project
		t.Fatalf("add org owner: %v", err)
	}
	if _, err := orgRepo.AddMember(ctx, o.ID, "outsider", rbac.RoleMember); err != nil { // org member, NOT on the project
		t.Fatalf("add org member: %v", err)
	}
	p, err := projRepo.Insert(ctx, &project.Project{
		Name: "Proj", Status: project.StatusEnabled, OrganizationID: o.ID,
		Memberships: []project.Membership{
			{Sub: "pmember", Role: project.RoleMember},
			{Sub: "powner", Role: project.RoleOwner},
		},
		Services: []any{}, CustomInfo: map[string]any{},
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	// Fix: org OWNER not listed on the project can still open it.
	if got, err := svc.GetProject(ctx, "owner1", p.ID); err != nil || got == nil {
		t.Fatalf("org owner should reach the project, got err=%v", err)
	}
	// An explicit project member still works.
	if got, err := svc.GetProject(ctx, "pmember", p.ID); err != nil || got == nil {
		t.Fatalf("project member should reach the project, got err=%v", err)
	}
	// A non-owner org member who is not on the project is still denied (404).
	if got, _ := svc.GetProject(ctx, "outsider", p.ID); got != nil {
		t.Error("a non-owner org member not on the project must be denied")
	}
	// A total stranger is denied.
	if got, _ := svc.GetProject(ctx, "stranger", p.ID); got != nil {
		t.Error("a stranger must be denied")
	}

	// Role edit: promote a MEMBER to project OWNER, then demote back.
	if _, err := svc.UpdateMemberRole(ctx, p.ID, "pmember", project.RoleOwner); err != nil {
		t.Fatalf("promote member to owner: %v", err)
	}
	if upd, _ := svc.GetProjectByID(ctx, p.ID); upd == nil || !upd.IsUserOwner("pmember") {
		t.Error("pmember should now be a project OWNER")
	}
	if _, err := svc.UpdateMemberRole(ctx, p.ID, "pmember", project.RoleMember); err != nil { // 2 owners, demote is fine
		t.Fatalf("demote back to member: %v", err)
	}
	// An unknown role is rejected.
	if _, err := svc.UpdateMemberRole(ctx, p.ID, "powner", "SUPERUSER"); err == nil {
		t.Error("an unknown role must be rejected")
	}
	// The last remaining owner cannot be demoted.
	if _, err := svc.UpdateMemberRole(ctx, p.ID, "powner", project.RoleMember); err == nil {
		t.Error("the last remaining project owner must not be demotable")
	}
}
