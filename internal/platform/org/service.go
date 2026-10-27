package org

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/platformconfig"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type Service struct {
	repo        *Repo
	billing     *billing.Repo
	platformcfg *platformconfig.Repo // organization-provisioning-quota (default config); nil-safe
}

func NewService(repo *Repo, b *billing.Repo, pc *platformconfig.Repo) *Service {
	return &Service{repo: repo, billing: b, platformcfg: pc}
}

// CreateOrganization:
// quota gate → save org → add creator as OWNER → create a BillingProfile
// (owner-populated) → set its id → save.
func (s *Service) CreateOrganization(ctx context.Context, creator *user.User, name, description string) (*Organization, error) {
	if name == "" {
		return nil, httpx.BadRequest("Organization name must not be null")
	}
	exceeded, err := s.isOrganizationsQuotaExceeded(ctx, creator.Sub)
	if err != nil {
		return nil, err
	}
	if exceeded {
		return nil, httpx.BadRequest("Your organizations limit has been reached")
	}
	o, err := s.repo.Insert(ctx, &Organization{Name: name, Description: description, CustomInfo: map[string]any{}})
	if err != nil {
		return nil, err
	}
	if _, err := s.repo.AddMember(ctx, o.ID, creator.Sub, rbac.RoleOwner); err != nil {
		return nil, err
	}
	bpID, err := s.billing.CreateForOrganization(ctx, o.ID, billing.Owner{
		Sub: creator.Sub, Email: creator.Email, FirstName: creator.FirstName,
		LastName: creator.LastName, FullName: creator.FullName(),
	})
	if err != nil {
		return nil, err
	}
	o.BillingProfileID = bpID
	if err := s.repo.Save(ctx, o); err != nil {
		return nil, err
	}
	return o, nil
}

// CanCreateOrganization reports whether the user may self-create another organization
// under the platform org-provisioning quota — the read the client onboarding/UI gates on
// (same check CreateOrganization enforces, so the form and the API can't disagree).
func (s *Service) CanCreateOrganization(ctx context.Context, sub string) (bool, error) {
	exceeded, err := s.isOrganizationsQuotaExceeded(ctx, sub)
	return !exceeded, err
}

// isOrganizationsQuotaExceeded gates self-service org creation on the platform
// default config's organizationProvisioningQuota: when enabled, a user may OWN
// at most `limit` organizations (0 = self-service creation off — operators
// create orgs and assign members). Disabled/absent = unlimited (the default).
// Memberships in orgs the user does not own never count against the quota.
func (s *Service) isOrganizationsQuotaExceeded(ctx context.Context, sub string) (bool, error) {
	if s.platformcfg == nil {
		return false, nil
	}
	cfg, err := s.platformcfg.FindDefault(ctx)
	if err != nil {
		return false, err
	}
	if cfg == nil || cfg.OrganizationProvisioningQuota == nil || !cfg.OrganizationProvisioningQuota.Enabled {
		return false, nil
	}
	ms, err := s.repo.MembersForSub(ctx, sub)
	if err != nil {
		return false, err
	}
	owned := 0
	for _, m := range ms {
		if m.Role() == rbac.RoleOwner {
			owned++
		}
	}
	return owned >= cfg.OrganizationProvisioningQuota.Limit, nil
}

func (s *Service) GetOrganization(ctx context.Context, id string) (*Organization, error) {
	o, err := s.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, httpx.NotFound("Organization not found")
	}
	return o, nil
}

// FindOrganization returns the org by id, or (nil,nil) if absent — lets a caller supply
// its own not-found message (e.g. the billing "has no organization associated" 404).
func (s *Service) FindOrganization(ctx context.Context, id string) (*Organization, error) {
	return s.repo.FindByID(ctx, id)
}

// GetOrganizationForUser: 404 if missing, 400 if the user is not a member.
func (s *Service) GetOrganizationForUser(ctx context.Context, id, userSub string) (*Organization, error) {
	o, err := s.GetOrganization(ctx, id)
	if err != nil {
		return nil, err
	}
	m, err := s.repo.FindMember(ctx, id, userSub)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, httpx.BadRequest("User is not a member of this organization")
	}
	return o, nil
}

// GetOrganizationForBillingProfile resolves the org owning a billing profile:
// find every org for the profile id → keep those the user is a member of → take the first → 404 otherwise. So a
// missing profile AND a non-member BOTH yield 404 "Billing Profile not found" (capital
// P, no 400) — there is no separate membership-400 here.
func (s *Service) GetOrganizationForBillingProfile(ctx context.Context, bpID, userSub string) (*Organization, error) {
	orgs, err := s.repo.FindAllByBillingProfileID(ctx, bpID)
	if err != nil {
		return nil, err
	}
	for i := range orgs {
		m, err := s.repo.FindMember(ctx, orgs[i].ID, userSub)
		if err != nil {
			return nil, err
		}
		if m != nil {
			return &orgs[i], nil
		}
	}
	return nil, httpx.NotFound("Billing Profile not found")
}

// Members returns an org's members (delegate for the billing-profile create flow,
// which resolves the org OWNER to populate a new profile).
func (s *Service) Members(ctx context.Context, orgID string) ([]Member, error) {
	return s.repo.Members(ctx, orgID)
}

func (s *Service) GetOrganizationsForUser(ctx context.Context, userSub string) ([]Organization, error) {
	ids, err := s.repo.OrgIDsForSub(ctx, userSub)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []Organization{}, nil
	}
	return s.repo.FindByIDs(ctx, ids)
}

func (s *Service) UpdateOrganization(ctx context.Context, id, userSub, name, description string) (*Organization, error) {
	o, err := s.GetOrganizationForUser(ctx, id, userSub)
	if err != nil {
		return nil, err
	}
	m, _ := s.repo.FindMember(ctx, id, userSub)
	if m == nil || !(m.Role() == rbac.RoleAdmin || m.Role() == rbac.RoleOwner) {
		return nil, httpx.BadRequest("User must be an owner or admin to update organization")
	}
	if name != "" {
		o.Name = name
	}
	if description != "" {
		o.Description = description
	}
	if err := s.repo.Save(ctx, o); err != nil {
		return nil, err
	}
	return o, nil
}

func (s *Service) DeleteOrganization(ctx context.Context, id, userSub string) error {
	_, err := s.GetOrganizationForUser(ctx, id, userSub)
	if err != nil {
		return err
	}
	m, _ := s.repo.FindMember(ctx, id, userSub)
	if m == nil || m.Role() != rbac.RoleOwner {
		return httpx.BadRequest("Only organization owner can delete the organization")
	}
	n, err := s.repo.CountProjects(ctx, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return httpx.BadRequest("Cannot delete organization with associated projects. Please delete or move all projects first.")
	}
	if err := s.repo.DeleteAllMembers(ctx, id); err != nil {
		return err
	}
	return s.repo.Delete(ctx, id)
}
