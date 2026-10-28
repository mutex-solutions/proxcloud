//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/platformconfig"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
)

func TestOrgRepo_MembersAndRoles(t *testing.T) {
	ctx := context.Background()
	repo := org.NewRepo(freshPG(t))
	if err := repo.EnsureIndexes(ctx); err != nil {
		t.Fatalf("ensure indexes: %v", err)
	}
	o, err := repo.Insert(ctx, &org.Organization{Name: "Acme", CustomInfo: map[string]any{}})
	if err != nil || o.ID == "" {
		t.Fatalf("insert org: %v id=%q", err, o.ID)
	}
	if _, err := repo.AddMember(ctx, o.ID, "subA", rbac.RoleOwner); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if _, err := repo.AddMember(ctx, o.ID, "subB", rbac.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	members, _ := repo.Members(ctx, o.ID)
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	m, _ := repo.FindMember(ctx, o.ID, "subA")
	if m == nil || m.Role() != rbac.RoleOwner {
		t.Errorf("subA role = %v, want OWNER", m)
	}
	if ids, _ := repo.OrgIDsForSub(ctx, "subA"); len(ids) != 1 || ids[0] != o.ID {
		t.Errorf("OrgIDsForSub = %v", ids)
	}
	if ms, _ := repo.MembersForSub(ctx, "subB"); len(ms) != 1 || ms[0].Role() != rbac.RoleMember {
		t.Errorf("MembersForSub = %v", ms)
	}

	// custom roles + unique (organizationId, name) index
	if _, err := repo.InsertRole(ctx, &org.Role{OrganizationID: o.ID, Name: "FINANCE", Permissions: []string{"billing_profile:read", "organization:read"}}); err != nil {
		t.Fatalf("insert role: %v", err)
	}
	if _, err := repo.InsertRole(ctx, &org.Role{OrganizationID: o.ID, Name: "FINANCE", Permissions: []string{"organization:read"}}); err == nil {
		t.Error("duplicate role name should violate the unique index")
	}
	perms, _ := repo.RolePermissions(ctx, o.ID, "FINANCE")
	if len(perms) != 2 {
		t.Errorf("RolePermissions = %v, want 2", perms)
	}
}

func TestOrgPolicy_CustomRoleResolution(t *testing.T) {
	ctx := context.Background()
	repo := org.NewRepo(freshPG(t))
	policy := org.NewPolicy(repo)
	o, _ := repo.Insert(ctx, &org.Organization{Name: "Acme"})
	// member assigned a CUSTOM role (not a static OWNER/ADMIN/MEMBER)
	_, _ = repo.AddMember(ctx, o.ID, "subC", "FINANCE")
	_, _ = repo.InsertRole(ctx, &org.Role{OrganizationID: o.ID, Name: "FINANCE", Permissions: []string{"billing_profile:read"}})

	if !policy.HasPermission(ctx, "subC", o.ID, "billing_profile:read") {
		t.Error("custom role should grant billing_profile:read")
	}
	if policy.HasPermission(ctx, "subC", o.ID, "project:create") {
		t.Error("custom role should NOT grant project:create")
	}
	keys := policy.UserPermissionKeys(ctx, "subC", o.ID)
	if len(keys) != 1 || keys[0] != "billing_profile:read" {
		t.Errorf("UserPermissionKeys = %v, want [billing_profile:read]", keys)
	}
}

func TestProjectRepo_EmbeddedMemberships(t *testing.T) {
	ctx := context.Background()
	repo := project.NewRepo(freshPG(t))
	p, err := repo.Insert(ctx, &project.Project{
		Name: "Proj", Status: project.StatusEnabled, OrganizationID: "org1",
		Memberships: []project.Membership{{Sub: "subA", Role: project.RoleOwner}},
		Services:    []any{}, CustomInfo: map[string]any{},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got, _ := repo.FindForMember(ctx, p.ID, "subA"); got == nil {
		t.Error("member subA should find the project")
	}
	if got, _ := repo.FindForMember(ctx, p.ID, "subZ"); got != nil {
		t.Error("non-member subZ should NOT find the project")
	}
	// list: member sees it
	if list, _ := repo.ListForMember(ctx, "subA", nil); len(list) != 1 {
		t.Errorf("ListForMember(member) = %d, want 1", len(list))
	}
	// list: non-member with org-visibility sees it
	if list, _ := repo.ListForMember(ctx, "subZ", []string{"org1"}); len(list) != 1 {
		t.Errorf("ListForMember(org-vis) = %d, want 1", len(list))
	}
	// list: non-member without visibility does not
	if list, _ := repo.ListForMember(ctx, "subZ", nil); len(list) != 0 {
		t.Errorf("ListForMember(none) = %d, want 0", len(list))
	}
}

func TestAuditRepo_CursorPagination(t *testing.T) {
	ctx := context.Background()
	repo := audit.NewRepo(freshPG(t))
	for i := 0; i < 5; i++ {
		ev := audit.ClientUserEvent("subA", "Ada")
		ev.OrganizationID = "org1"
		ev.Action = audit.ActionCreate
		ev.ResourceType = audit.ResourceOrganization
		ev.Outcome = audit.OutcomeSuccess
		if err := repo.Insert(ctx, ev); err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
	}
	f := audit.Filter{OrganizationID: "org1"}

	// all, to learn the desc-sorted id order
	all, _, _, err := repo.Query(ctx, f, "", "", 50)
	if err != nil || len(all) != 5 {
		t.Fatalf("query all: %v len=%d", err, len(all))
	}
	// page 1 (limit 2): newest two, nextMarker set, prevMarker nil
	p1, next1, prev1, _ := repo.Query(ctx, f, "", "", 2)
	if len(p1) != 2 || next1 == nil || prev1 != nil {
		t.Fatalf("page1 len=%d next=%v prev=%v", len(p1), next1, prev1)
	}
	if p1[0].ID != all[0].ID || p1[1].ID != all[1].ID {
		t.Error("page1 not in _id-desc order")
	}
	// page 2 via after=nextMarker
	p2, next2, prev2, _ := repo.Query(ctx, f, *next1, "", 2)
	if len(p2) != 2 || next2 == nil || prev2 == nil {
		t.Fatalf("page2 len=%d next=%v prev=%v", len(p2), next2, prev2)
	}
	if p2[0].ID != all[2].ID || p2[1].ID != all[3].ID {
		t.Error("page2 wrong slice / overlap")
	}
	// page 3: last item, nextMarker nil (end)
	p3, next3, _, _ := repo.Query(ctx, f, *next2, "", 2)
	if len(p3) != 1 || next3 != nil {
		t.Fatalf("page3 len=%d next=%v (want 1, nil)", len(p3), next3)
	}
	if p3[0].ID != all[4].ID {
		t.Error("page3 not the oldest event")
	}
	// before cursor: items newer than all[2], reversed to desc
	pb, _, _, _ := repo.Query(ctx, f, "", all[2].ID, 50)
	if len(pb) != 2 || pb[0].ID != all[0].ID || pb[1].ID != all[1].ID {
		t.Errorf("before-cursor page = %v, want [all[0],all[1]]", ids(pb))
	}
}

func TestPlatformConfig_FindDefault(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := platformconfig.NewRepo(db)
	if c, _ := repo.FindDefault(ctx); c != nil {
		t.Error("empty collection should yield nil default config")
	}
	_, err := db.C("platformConfiguration").InsertOne(ctx, pgdoc.M{
		"defaultConfiguration": true,
		"branding":             pgdoc.M{"name": "ProxCloud"},
		"dateConfiguration":    pgdoc.M{"dateFormat": "DD/MM/YYYY"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	c, err := repo.FindDefault(ctx)
	if err != nil || c == nil {
		t.Fatalf("find default: %v", err)
	}
	if c.Branding == nil || c.Branding.Name != "ProxCloud" || c.DateConfiguration == nil || c.DateConfiguration.DateFormat != "DD/MM/YYYY" {
		t.Errorf("config mapping wrong: %+v", c)
	}
}

func TestUserRepo_FromClaimsGetOrCreate(t *testing.T) {
	ctx := context.Background()
	repo := user.NewRepo(freshPG(t))
	c := user.Claims{Sub: "subX", Email: "x@example.com", GivenName: "Ada", FamilyName: "Lovelace", Issuer: "iss"}
	u1, err := repo.FromClaims(ctx, c)
	if err != nil || u1 == nil || u1.ID == "" {
		t.Fatalf("create: %v", err)
	}
	u2, err := repo.FromClaims(ctx, c)
	if err != nil || u2.ID != u1.ID {
		t.Errorf("second FromClaims should return same user: %v vs %v", u2.ID, u1.ID)
	}
	if got, _ := repo.FindBySub(ctx, "subX"); got == nil || got.FullName() != "Ada Lovelace" {
		t.Errorf("FindBySub/FullName wrong: %v", got)
	}
}

// TestUserRepo_EmailVerification covers SSO-0: emailConfirmedAt follows the IdP's
// email_verified claim for the current email, and email matching (org add-member,
// project invites) only finds verified users — an unverified look-alike sign-up
// with the same address never resolves.
func TestUserRepo_EmailVerification(t *testing.T) {
	ctx := context.Background()
	repo := user.NewRepo(freshPG(t))
	if err := repo.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	const email = "victim@example.com"
	// An attacker signs up first with the victim's address, unverified.
	att, err := repo.FromClaims(ctx, user.Claims{Sub: "attacker", Email: email})
	if err != nil || att.EmailConfirmedAt != nil || att.EmailVerified() {
		t.Fatalf("unverified sign-up must not be confirmed: %+v %v", att, err)
	}
	if got, err := repo.FindVerifiedByEmail(ctx, email); err != nil || got != nil {
		t.Fatalf("unverified user must not match: %+v %v", got, err)
	}
	// The real owner signs up, verified.
	own, err := repo.FromClaims(ctx, user.Claims{Sub: "owner", Email: email, EmailVerified: true})
	if err != nil || own.EmailConfirmedAt == nil {
		t.Fatalf("verified sign-up must be confirmed: %+v %v", own, err)
	}
	if got, _ := repo.FindVerifiedByEmail(ctx, email); got == nil || got.Sub != "owner" {
		t.Fatalf("verified match = %+v, want owner", got)
	}
	// An existing user becomes verified on a later login.
	if u, _ := repo.FromClaims(ctx, user.Claims{Sub: "attacker", Email: email, EmailVerified: true}); u.EmailConfirmedAt == nil {
		t.Fatal("verified claim on login must stamp emailConfirmedAt")
	}
	// The owner changes to a new, not yet verified address: confirmation is cleared.
	if u, _ := repo.FromClaims(ctx, user.Claims{Sub: "owner", Email: "new@example.com"}); u.EmailConfirmedAt != nil {
		t.Fatal("unverified new email must clear emailConfirmedAt")
	}
	if got, _ := repo.FindBySub(ctx, "owner"); got.EmailConfirmedAt != nil || got.Email != "new@example.com" {
		t.Fatalf("stored owner = %+v, want new email unconfirmed", got)
	}
	// No email claim (e.g. a PAT session) leaves verification alone.
	if u, _ := repo.FromClaims(ctx, user.Claims{Sub: "attacker"}); u.EmailConfirmedAt == nil {
		t.Fatal("a claim set without email must not clear emailConfirmedAt")
	}
}

func ids(evs []audit.AuditEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

// TestPlatformConfigByID covers the fix for the admin config edit page's record fetch: the
// config-by-id lookup returns the config for a VALID id (was a broken always-500 stub →
// the FE's post-save refresh failed → "save doesn't stick"). Bogus id → nil (handler 500s).
func TestPlatformConfigByID(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := platformconfig.NewRepo(db)

	oid := pgdoc.NewID()
	if _, err := db.C("platformConfiguration").InsertOne(ctx, pgdoc.M{
		"_id": oid, "name": "ProxCloud", "defaultConfiguration": true,
		"dateConfiguration":        pgdoc.M{"dateFormat": "DD/MM/YYYY"},
		"projectProvisioningQuota": pgdoc.M{"enabled": true, "limit": 2},
		"mailGatewayId":            "6a40d69eaaaaaaaaaaaaaaaa", // the selected mail gateway integration
		"contactIntegrationId":     "6a40d69ebbbbbbbbbbbbbbbb",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	// found by the string id → AdminDto with the persisted fields.
	got, err := repo.ByIDAdminConfiguration(ctx, oid)
	if err != nil {
		t.Fatalf("by-id: %v", err)
	}
	if got == nil {
		t.Fatal("valid id must resolve (was the always-500 bug)")
	}
	if got.Name != "ProxCloud" || got.ProjectProvisioningQuota == nil || got.ProjectProvisioningQuota.Limit != 2 {
		t.Fatalf("by-id dto wrong: %+v", got)
	}
	// mailGatewayId/contactIntegrationId must round-trip (was dropped → the dropdown showed nothing).
	if got.MailGatewayID != "6a40d69eaaaaaaaaaaaaaaaa" || got.ContactIntegrationID != "6a40d69ebbbbbbbbbbbbbbbb" {
		t.Fatalf("mail/contact integration ids dropped: %+v", got)
	}
	// non-null empties preserved (regions/loginConfiguration) — the FE binds these.
	if got.Regions == nil || got.LoginConfiguration == nil {
		t.Fatalf("regions/loginConfiguration must be non-null: %+v", got)
	}

	// bogus id → nil (the handler turns this into a not-found 500).
	if miss, err := repo.ByIDAdminConfiguration(ctx, "000000000000000000000099"); err != nil || miss != nil {
		t.Fatalf("bogus id must be nil,nil; got %+v / %v", miss, err)
	}
}
