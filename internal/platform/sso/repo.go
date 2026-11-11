// Package sso is per-organization single sign-on. There is one customer realm; every organization
// that configures SSO gets its own identity-provider broker in that realm, created through the
// Keycloak Admin API. ProxCloud stays a resource server (one issuer, SPA and MCP unchanged); what
// makes a broker safe to let a tenant control lives here:
//
//   - a domain only counts once its owner proved it with a DNS TXT record, and only one
//     organization can hold a verified domain (unique index);
//   - an IdP may only assert emails in its own organization's verified domains (Guard);
//   - a person stays bound to the IdP that first provisioned them, so another IdP that got linked
//     to the same Keycloak user cannot reach their ProxCloud account (Guard);
//   - SSO never grants OWNER/ADMIN or a role that can manage members or roles, checked when
//     configured and again when used;
//   - enforce-SSO blocks every other way in, except for at most two named break-glass owners.
package sso

import (
	"context"
	"errors"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Provider is a document in org_identity_provider, one per organization. A document without a
// Protocol is a draft: its alias exists so the customer can register the redirect URI at their IdP
// before they have a client id to enter here.
type Provider struct {
	ID                string            `json:"id,omitempty"`
	OrganizationID    string            `json:"organizationId"`
	Alias             string            `json:"alias"` // random, never derived from the org id
	Protocol          string            `json:"protocol,omitempty"`
	DiscoveryURL      string            `json:"discoveryUrl,omitempty"` // OIDC discovery or SAML metadata
	ClientID          string            `json:"clientId,omitempty"`
	ClientSecretEnc   string            `json:"clientSecretEnc,omitempty"` // textcrypt; never returned
	Scopes            string            `json:"scopes,omitempty"`          // OIDC scopes requested from the IdP
	ProviderName      string            `json:"providerName,omitempty"`
	DefaultRole       string            `json:"defaultRole,omitempty"`
	GroupsClaim       string            `json:"groupsClaim,omitempty"`
	GroupRoleMap      map[string]string `json:"groupRoleMap,omitempty"`
	DefaultProjectIDs []string          `json:"defaultProjectIds,omitempty"`
	AutoProvision     bool              `json:"autoProvision"`
	TrustEmail        bool              `json:"trustEmail"`
	Enabled           bool              `json:"enabled"`
	EnforceSSO        bool              `json:"enforceSso"`
	BreakGlassSubs    []string          `json:"breakGlassSubs,omitempty"`
	OperatorDisabled  bool              `json:"operatorDisabled"` // the org cannot re-enable until an operator lifts it
	CreatedAt         *time.Time        `json:"createdAt,omitempty"`
	UpdatedAt         *time.Time        `json:"updatedAt,omitempty"`
}

// Configured reports whether the provider is more than a draft.
func (p *Provider) Configured() bool { return p.Protocol != "" }

// Active reports whether logins through this provider are currently allowed.
func (p *Provider) Active() bool { return p.Configured() && p.Enabled && !p.OperatorDisabled }

// Domain is a document in org_sso_domain. Several organizations may hold a pending claim on the
// same domain, so nobody can squat a name they do not own; VerifiedDomain is set only after the TXT
// check and carries the unique index, which is what makes "one owner" true across API replicas.
type Domain struct {
	ID             string     `json:"id,omitempty"`
	Domain         string     `json:"domain"`
	OrganizationID string     `json:"organizationId"`
	Token          string     `json:"token"`
	VerifiedDomain string     `json:"verifiedDomain,omitempty"`
	VerifiedAt     *time.Time `json:"verifiedAt,omitempty"`
	CreatedAt      *time.Time `json:"createdAt,omitempty"`
}

// Repo persists providers, domains and permanently retired platform-issued aliases.
type Repo struct {
	providers *pgdoc.Store
	domains   *pgdoc.Store
	retired   *pgdoc.Store
}

// NewRepo binds the collections. pgdoc uses the primary-only pool for reads and writes.
func NewRepo(db *pgdoc.DB) *Repo {
	return &Repo{providers: db.C("org_identity_provider"), domains: db.C("org_sso_domain"), retired: db.C("org_sso_retired_alias")}
}

// EnsureIndexes creates the tables and the unique indexes the invariants rest on.
func (r *Repo) EnsureIndexes(ctx context.Context) error {
	// Retired aliases use the alias itself as the table's unique primary key.
	for _, s := range []*pgdoc.Store{r.providers, r.domains, r.retired} {
		if err := s.Ensure(ctx); err != nil {
			return err
		}
	}
	for _, ix := range []struct {
		s    *pgdoc.Store
		name string
		f    []pgdoc.IndexField
	}{
		{r.providers, "org_unique", []pgdoc.IndexField{pgdoc.F("organizationId")}},
		{r.providers, "alias_unique", []pgdoc.IndexField{pgdoc.F("alias")}},
		{r.domains, "org_domain_unique", []pgdoc.IndexField{pgdoc.F("organizationId"), pgdoc.F("domain")}},
		{r.domains, "verified_unique", []pgdoc.IndexField{pgdoc.F("verifiedDomain")}},
	} {
		if err := ix.s.EnsureIndex(ctx, ix.name, true, ix.f...); err != nil {
			return err
		}
	}
	return nil
}

// ProviderForOrg returns the org's provider or nil.
func (r *Repo) ProviderForOrg(ctx context.Context, orgID string) (*Provider, error) {
	var p Provider
	found, err := r.providers.FindOne(ctx, pgdoc.M{"organizationId": orgID}, &p)
	if err != nil || !found {
		return nil, err
	}
	return &p, nil
}

// AllProviders lists every provider, drafts included.
func (r *Repo) AllProviders(ctx context.Context) ([]Provider, error) {
	var out []Provider
	err := r.providers.Find(ctx, pgdoc.M{}, &out)
	return out, err
}

// InsertProvider stores a new provider.
func (r *Repo) InsertProvider(ctx context.Context, p *Provider) error {
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = &now, &now
	id, err := r.providers.InsertOne(ctx, p)
	if err != nil {
		return err
	}
	p.ID = id
	return nil
}

// SaveProvider replaces a provider document.
func (r *Repo) SaveProvider(ctx context.Context, p *Provider) error {
	now := time.Now().UTC()
	p.UpdatedAt = &now
	_, err := r.providers.Replace(ctx, p.ID, p)
	return err
}

// DeleteProvider removes a provider document. Its retired alias must remain separately.
func (r *Repo) DeleteProvider(ctx context.Context, id string) error {
	_, err := r.providers.DeleteByID(ctx, id)
	return err
}

// retireAlias durably fences a stored provider's alias before any external delete. Callers must
// supply only platform-issued aliases from provider documents, never a token's arbitrary IdP claim.
// The alias is the unique row id; the payload is empty and retries are idempotent. No expiry or
// automatic pruning: a Keycloak session may outlive its access tokens, and aliases are never reused.
func (r *Repo) retireAlias(ctx context.Context, alias string) error {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return err // the fence must commit before the caller contacts Keycloak
	}
	if alias == "" {
		return errors.New("sso: cannot retire an empty provider alias")
	}
	return r.retired.Upsert(ctx, alias, pgdoc.M{})
}

func (r *Repo) aliasRetired(ctx context.Context, alias string) (bool, error) {
	var row struct{}
	return r.retired.Get(ctx, alias, &row)
}

// retiredAliases is loaded only on the existing lazy snapshot refresh. Permanent retention means
// the small id-only rows and the snapshot map grow with provider deletions, not with login traffic.
func (r *Repo) retiredAliases(ctx context.Context) (map[string]struct{}, error) {
	var rows []struct {
		Alias string `json:"id"`
	}
	if err := r.retired.Find(ctx, pgdoc.M{}, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		out[row.Alias] = struct{}{}
	}
	return out, nil
}

// DomainsForOrg lists an org's claims, pending and verified.
func (r *Repo) DomainsForOrg(ctx context.Context, orgID string) ([]Domain, error) {
	var out []Domain
	err := r.domains.Find(ctx, pgdoc.M{"organizationId": orgID}, &out, pgdoc.Sort(pgdoc.Asc("domain")))
	return out, err
}

// VerifiedDomains lists every verified domain.
func (r *Repo) VerifiedDomains(ctx context.Context) ([]Domain, error) {
	var out []Domain
	err := r.domains.Find(ctx, pgdoc.M{"verifiedDomain": pgdoc.M{"$ne": nil}}, &out)
	return out, err
}

// FindClaim returns one org's claim on a domain or nil.
func (r *Repo) FindClaim(ctx context.Context, orgID, domain string) (*Domain, error) {
	var d Domain
	found, err := r.domains.FindOne(ctx, pgdoc.M{"organizationId": orgID, "domain": domain}, &d)
	if err != nil || !found {
		return nil, err
	}
	return &d, nil
}

// VerifiedOwner returns the org holding a verified domain, or "".
func (r *Repo) VerifiedOwner(ctx context.Context, domain string) (string, error) {
	var d Domain
	found, err := r.domains.FindOne(ctx, pgdoc.M{"verifiedDomain": domain}, &d)
	if err != nil || !found {
		return "", err
	}
	return d.OrganizationID, nil
}

// InsertClaim stores a pending claim.
func (r *Repo) InsertClaim(ctx context.Context, d *Domain) error {
	now := time.Now().UTC()
	d.CreatedAt = &now
	id, err := r.domains.InsertOne(ctx, d)
	if err != nil {
		return err
	}
	d.ID = id
	return nil
}

// MarkVerified flips a claim to verified. The unique index refuses a second organization, so the
// caller must treat pgdoc.IsDup as "someone else owns this".
func (r *Repo) MarkVerified(ctx context.Context, d *Domain) error {
	now := time.Now().UTC()
	if _, err := r.domains.SetByID(ctx, d.ID, pgdoc.M{"verifiedDomain": d.Domain, "verifiedAt": now}, nil); err != nil {
		return err
	}
	d.VerifiedDomain, d.VerifiedAt = d.Domain, &now
	return nil
}

// DeleteClaim removes one org's claim.
func (r *Repo) DeleteClaim(ctx context.Context, orgID, domain string) (bool, error) {
	return r.domains.DeleteOne(ctx, pgdoc.M{"organizationId": orgID, "domain": domain})
}

// DeleteVerified removes whichever claim holds a verified domain (operator dispute resolution).
func (r *Repo) DeleteVerified(ctx context.Context, domain string) (bool, error) {
	return r.domains.DeleteOne(ctx, pgdoc.M{"verifiedDomain": domain})
}
