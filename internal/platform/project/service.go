package project

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/platformconfig"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Service is the project business layer (platform subset; cloud bootstrap
// deferred).
type Service struct {
	repo        *Repo
	orgRepo     *org.Repo
	billing     *billing.Repo
	users       *user.Repo
	platformcfg *platformconfig.Repo // project-provisioning-quota (default config); nil-safe
}

func NewService(repo *Repo, orgRepo *org.Repo, b *billing.Repo, users *user.Repo, platformcfg *platformconfig.Repo) *Service {
	return &Service{repo: repo, orgRepo: orgRepo, billing: b, users: users, platformcfg: platformcfg}
}

// CreateProject builds memberships from the org's members, defaults to DISABLED,
// and flips to ENABLED only when billing is off or the org's billing profile is
// ACTIVE. (Cloud bootstrap on ENABLED is deferred.)
func (s *Service) CreateProject(ctx context.Context, creatorSub, name, orgID string, memberSubs []string) (*Project, error) {
	if name == "" {
		return nil, httpx.BadRequest("The project name must not be null ")
	}
	o, err := s.orgRepo.FindByID(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, httpx.NotFound("Organization not found")
	}
	members, err := s.orgRepo.Members(ctx, orgID)
	if err != nil {
		return nil, err
	}
	p := &Project{
		Name:           name,
		Owner:          creatorSub,
		CustomInfo:     map[string]any{},
		Status:         StatusDisabled,
		Memberships:    toMemberships(members, creatorSub, memberSubs),
		Services:       []any{},
		OrganizationID: orgID,
	}
	// ENABLE is gated on whether billing is configured: when a billingConfiguration exists, a
	// project only ENABLEs once its org's billing profile is ACTIVE; with billing unconfigured it
	// ENABLEs immediately.
	_, _, billingConfigured, err := s.billing.Configuration(ctx)
	if err != nil {
		return nil, err
	}
	if billingConfigured {
		// Enforce the project provisioning quota: the per-bp quota wins when both it AND the
		// platform default are enabled, else the platform default applies; count is the org's
		// projects (all share the org's bp).
		exceeded, err := s.isProjectsQuotaExceeded(ctx, o.BillingProfileID, orgID)
		if err != nil {
			return nil, err
		}
		if exceeded {
			return nil, httpx.BadRequest("Your projects limit has been reached")
		}
		active, err := s.billing.IsActive(ctx, o.BillingProfileID)
		if err != nil {
			return nil, err
		}
		if active {
			p.Status = StatusEnabled
		}
	} else {
		p.Status = StatusEnabled
	}
	return s.repo.Insert(ctx, p)
	// NOTE: if status == ENABLED, cloud external services are bootstrapped here.
}

// isProjectsQuotaExceeded: the per-bp quota applies only when BOTH the platform default AND the bp
// quota are enabled (count ≥ bp.limit); otherwise the platform default applies (count ≥
// platform.limit); else no limit. The count is the org's projects (each org has one bp, so this
// equals the per-billing-profile project count).
func (s *Service) isProjectsQuotaExceeded(ctx context.Context, billingProfileID, orgID string) (bool, error) {
	var platformQuota *platformconfig.ProjectProvisioningQuota
	if s.platformcfg != nil {
		cfg, err := s.platformcfg.FindDefault(ctx)
		if err != nil {
			return false, err
		}
		if cfg != nil {
			platformQuota = cfg.ProjectProvisioningQuota
		}
	}
	platformOn := platformQuota != nil && platformQuota.Enabled
	var bpQuota *billing.ProjectProvisioningQuota
	if bp, err := s.billing.FindByID(ctx, billingProfileID); err == nil && bp != nil {
		bpQuota = bp.ProjectProvisioningQuota
	} else if err != nil {
		return false, err
	}
	// Resolve the active limit: bp override (needs platform+bp enabled) else platform default.
	limit, active := 0, false
	switch {
	case bpQuota != nil && bpQuota.Enabled && platformOn:
		limit, active = bpQuota.Limit, true
	case platformOn:
		limit, active = platformQuota.Limit, true
	}
	if !active {
		return false, nil
	}
	existing, err := s.repo.ListByOrganizationID(ctx, orgID)
	if err != nil {
		return false, err
	}
	return len(existing) >= limit, nil
}

// HiddenServices returns the service slugs the operator hides from tenants
// (platformConfiguration.hiddenServices). A read error hides nothing.
func (s *Service) HiddenServices(ctx context.Context) []string {
	if s.platformcfg == nil {
		return nil
	}
	cfg, err := s.platformcfg.FindDefault(ctx)
	if err != nil || cfg == nil {
		return nil
	}
	return cfg.HiddenServices
}

// Save persists a project's fields other than its status (services from cloud bootstrap).
func (s *Service) Save(ctx context.Context, p *Project) error { return s.repo.Save(ctx, p) }

// GetProject loads a project the user can access: either an explicit member, or an
// OWNER/ADMIN of the owning organization (the same visibility ListForSub grants, and
// which the RBAC project:* on those roles already authorizes). Else 404.
func (s *Service) GetProject(ctx context.Context, sub, id string) (*Project, error) {
	p, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil || (!p.IsMember(sub) && !s.orgAdminOrOwner(ctx, sub, p.OrganizationID)) {
		return nil, httpx.NotFound(fmt.Sprintf("The project with id %s was not found. ", id))
	}
	return p, nil
}

// orgAdminOrOwner reports whether sub is an OWNER/ADMIN of the org, who can reach every
// project in it — parity with visibleOrgIDs (the project list already shows them these).
func (s *Service) orgAdminOrOwner(ctx context.Context, sub, orgID string) bool {
	if orgID == "" {
		return false
	}
	m, err := s.orgRepo.FindMember(ctx, orgID, sub)
	if err != nil || m == nil {
		return false
	}
	r := m.Role()
	return r == "OWNER" || r == "ADMIN"
}

// GetProjectByID loads a project by id regardless of membership, else 404.
func (s *Service) GetProjectByID(ctx context.Context, id string) (*Project, error) {
	p, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, httpx.NotFound(fmt.Sprintf("The project with id %s was not found. ", id))
	}
	return p, nil
}

// ListForSub returns the projects the user is a member of, plus projects of orgs
// where the user is OWNER/ADMIN.
func (s *Service) ListForSub(ctx context.Context, sub string) ([]ProjectView, error) {
	visOrgIDs, err := s.visibleOrgIDs(ctx, sub)
	if err != nil {
		return nil, err
	}
	projects, err := s.repo.ListForMember(ctx, sub, visOrgIDs)
	if err != nil {
		return nil, err
	}
	views := make([]ProjectView, 0, len(projects))
	for i := range projects {
		views = append(views, toView(&projects[i]))
	}
	return views, nil
}

// visibleOrgIDs returns org ids where sub has OWNER/ADMIN (project visibility).
func (s *Service) visibleOrgIDs(ctx context.Context, sub string) ([]string, error) {
	ms, err := s.orgRepo.MembersForSub(ctx, sub)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if r := m.Role(); r == "OWNER" || r == "ADMIN" {
			out = append(out, m.OrganizationID)
		}
	}
	return out, nil
}

// Rename sets a new name.
func (s *Service) Rename(ctx context.Context, sub, id, name string) (*Project, error) {
	p, err := s.GetProject(ctx, sub, id)
	if err != nil {
		return nil, err
	}
	p.Name = name
	if err := s.repo.Save(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// BillingAllowsEnabled reports whether nothing on the billing side holds the project DISABLED: billing is not configured, or the
// profile that pays for it (its own, else the organization's) is ACTIVE.
func (s *Service) BillingAllowsEnabled(ctx context.Context, p *Project) (bool, error) {
	_, _, configured, err := s.billing.Configuration(ctx)
	if err != nil || !configured {
		return !configured, err
	}
	bpID := p.BillingProfileID
	if bpID == "" {
		o, err := s.orgRepo.FindByID(ctx, p.OrganizationID)
		if err != nil || o == nil {
			return false, err
		}
		bpID = o.BillingProfileID
	}
	return s.billing.IsActive(ctx, bpID)
}

// ScheduleDeletion marks the project for deferred deletion. The cloud-side
// deletability check is deferred.
func (s *Service) ScheduleDeletion(ctx context.Context, p *Project, _ bool) (*Project, error) {
	if err := CheckTransition(ActionSchedule, p.Status); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	held := p.Status
	p.StatusBeforeDeletion = held
	p.Status = StatusScheduledForDeletion
	p.ScheduledForDeletionAt = &now
	return s.SaveIfStatus(ctx, p, held)
}

// SaveIfStatus writes a status change decided on a read of status expect; a change that won meanwhile is a 409.
func (s *Service) SaveIfStatus(ctx context.Context, p *Project, expect string) (*Project, error) {
	ok, err := s.repo.SaveIfStatus(ctx, p, expect)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errProjectChanged
	}
	return p, nil
}

var errProjectChanged = httpx.NewError(http.StatusConflict, http.StatusConflict, "The project changed meanwhile. Try again")

// EnableIfDisabled enables a DISABLED project; a change that won meanwhile, or an operator's disable when the operator is not
// the one asking, is a 409.
func (s *Service) EnableIfDisabled(ctx context.Context, p *Project, byOperator bool) error {
	ok, err := s.repo.EnableIfDisabled(ctx, p, byOperator)
	if err != nil {
		return err
	}
	if !ok {
		return errProjectChanged
	}
	return nil
}

// DeleteNow flags the project for immediate deletion: the deletion job's next sweep (every
// minute) tears it down, with the grace window already over.
func (s *Service) DeleteNow(ctx context.Context, id string) (*Project, error) {
	p, err := s.GetProjectByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := CheckTransition(ActionDeleteNow, p.Status); err != nil {
		return nil, err
	}
	expect := p.Status
	p.Status = StatusDeleteInProgress
	past := time.Now().UTC().Add(-deletionGrace)
	p.ScheduledForDeletionAt = &past
	return s.SaveIfStatus(ctx, p, expect)
}

// CancelDeletion returns a project scheduled for deletion to the status it held before.
func (s *Service) CancelDeletion(ctx context.Context, id string) (*Project, error) {
	p, err := s.GetProjectByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := CheckTransition(ActionCancel, p.Status); err != nil {
		return nil, err
	}
	p.Status = StatusAfterCancel(p.StatusBeforeDeletion)
	p.StatusBeforeDeletion = ""
	p.ScheduledForDeletionAt = nil
	return s.SaveIfStatus(ctx, p, StatusScheduledForDeletion)
}

// AddMember adds a user to the project (with the org-membership and
// project-state validations).
func (s *Service) AddMember(ctx context.Context, projID, userSub, role string) (*Project, error) {
	p, err := s.GetProjectByID(ctx, projID)
	if err != nil {
		return nil, err
	}
	u, err := s.users.FindBySub(ctx, userSub)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, httpx.NotFound("User not found with sub: " + userSub)
	}
	om, err := s.orgRepo.FindMember(ctx, p.OrganizationID, u.Sub)
	if err != nil {
		return nil, err
	}
	if om == nil {
		return nil, httpx.BadRequest("User must be a member of the organization before being added to a project")
	}
	if p.IsDisabled() {
		return nil, httpx.BadRequest("Project is suspended. Cannot add user to project")
	}
	if p.IsMember(u.Sub) {
		return nil, httpx.BadRequest("User is already added to project")
	}
	p.Memberships = append(p.Memberships, Membership{Sub: u.Sub, Role: role})
	if err := s.repo.Save(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// AddMemberToOrgProjects propagates a new org member onto the organization's projects:
// projectIDs==nil → ALL org projects; else the subset (400 if any id is not an org project). Each
// project the user isn't already in gets a MEMBER membership. Best-effort per project (a single
// project failure is logged, not fatal).
func (s *Service) AddMemberToOrgProjects(ctx context.Context, orgID, userSub string, projectIDs []string) error {
	projects, err := s.repo.ListByOrganizationID(ctx, orgID)
	if err != nil {
		return err
	}
	if projectIDs != nil {
		valid := map[string]bool{}
		for i := range projects {
			valid[projects[i].ID] = true
		}
		var invalid []string
		for _, id := range projectIDs {
			if !valid[id] {
				invalid = append(invalid, id)
			}
		}
		if len(invalid) > 0 {
			return httpx.BadRequest("Project IDs do not belong to this organization: " + strings.Join(invalid, ", "))
		}
		want := map[string]bool{}
		for _, id := range projectIDs {
			want[id] = true
		}
		filtered := projects[:0]
		for i := range projects {
			if want[projects[i].ID] {
				filtered = append(filtered, projects[i])
			}
		}
		projects = filtered
	}
	for i := range projects {
		if projects[i].IsMember(userSub) {
			continue
		}
		if _, err := s.AddMember(ctx, projects[i].ID, userSub, RoleMember); err != nil {
			slog.Error("add org member to project failed", "project", projects[i].ID, "sub", userSub, "err", err)
		}
	}
	return nil
}

// RemoveMember removes a user from the project.
func (s *Service) RemoveMember(ctx context.Context, projID, sub string) (*Project, error) {
	p, err := s.GetProjectByID(ctx, projID)
	if err != nil {
		return nil, err
	}
	u, err := s.users.FindBySub(ctx, sub)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, httpx.NotFound("User not found with sub: " + sub)
	}
	idx := -1
	for i, m := range p.Memberships {
		if m.Sub == u.Sub {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, httpx.BadRequest("User is already removed from project")
	}
	if p.Memberships[idx].Role == RoleOwner {
		return nil, httpx.BadRequest("Project owner cannot be removed from project")
	}
	p.Memberships = append(p.Memberships[:idx], p.Memberships[idx+1:]...)
	if err := s.repo.Save(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// UpdateMemberRole changes an existing project member's role. Project roles are only
// OWNER (treated as project ADMIN by RBAC) or MEMBER; it refuses an unknown role and
// won't demote the project's last remaining OWNER.
func (s *Service) UpdateMemberRole(ctx context.Context, projID, sub, role string) (*Project, error) {
	if role != RoleOwner && role != RoleMember {
		return nil, httpx.BadRequest("Role must be OWNER or MEMBER")
	}
	p, err := s.GetProjectByID(ctx, projID)
	if err != nil {
		return nil, err
	}
	idx, owners := -1, 0
	for i, m := range p.Memberships {
		if m.Role == RoleOwner {
			owners++
		}
		if m.Sub == sub {
			idx = i
		}
	}
	if idx < 0 {
		return nil, httpx.NotFound("User is not a member of this project")
	}
	if p.Memberships[idx].Role == role {
		return p, nil
	}
	if p.Memberships[idx].Role == RoleOwner && owners <= 1 {
		return nil, httpx.BadRequest("Project must keep at least one owner")
	}
	p.Memberships[idx].Role = role
	if err := s.repo.Save(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Members resolves each membership to a ProjectUser profile; members without a
// User row are skipped.
func (s *Service) Members(ctx context.Context, p *Project) ([]ProjectUser, error) {
	out := make([]ProjectUser, 0, len(p.Memberships))
	for _, m := range p.Memberships {
		u, err := s.users.FindBySub(ctx, m.Sub)
		if err != nil {
			return nil, err
		}
		if u == nil {
			continue
		}
		out = append(out, ProjectUser{
			UserID:    u.ID,
			Sub:       u.Sub,
			FirstName: u.FirstName,
			LastName:  u.LastName,
			Email:     u.Email,
			Role:      m.Role,
		})
	}
	return out, nil
}

// toMemberships maps org members to project memberships. When memberSubs is set,
// only the requester + those subs are included. Org OWNER/ADMIN map to project
// OWNER, everyone else to MEMBER.
func toMemberships(members []org.Member, requesterSub string, memberSubs []string) []Membership {
	var sel map[string]bool
	if len(memberSubs) > 0 {
		sel = map[string]bool{requesterSub: true}
		for _, s := range memberSubs {
			sel[s] = true
		}
	}
	out := make([]Membership, 0, len(members))
	for _, m := range members {
		if sel != nil && !sel[m.Sub] {
			continue
		}
		out = append(out, Membership{Sub: m.Sub, Role: mapRole(m.Role())})
	}
	return out
}

func mapRole(orgRole string) string {
	switch orgRole {
	case "OWNER", "ADMIN":
		return RoleOwner
	default:
		return RoleMember
	}
}

// toView projects a Project into a ProjectView (resourcesCount empty for now —
// the per-type cloud-resource counts land with the cloud slice).
func toView(p *Project) ProjectView {
	return ProjectView{
		ID:               p.ID,
		Name:             p.Name,
		Status:           p.Status,
		BillingProfileID: p.BillingProfileID,
		OrganizationID:   p.OrganizationID,
		Memberships:      p.Memberships,
		CreatedAt:        p.CreatedAt,
		ResourcesCount:   []ResourceCount{},

		PublicNetworksVisible: p.PublicNetworksVisible,
		GpuCapacityVisible:    p.GpuCapacityVisible,
	}
}
