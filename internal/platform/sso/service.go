package sso

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

const (
	protoOIDC = "oidc"
	protoSAML = "saml"

	defaultGroupsClaim = "groups"
	defaultScopes      = "openid email profile"
	maxBreakGlass      = 2
	maxClaimsPerOrg    = 20
	challengePrefix    = "_proxcloud-challenge."
	reconcileEvery     = 5 * time.Minute

	// Audit vocabulary of this package.
	resourceSSO            = "SSO"
	actionUpdated          = "sso.updated"
	actionRemoved          = "sso.removed"
	actionTested           = "sso.tested"
	actionDomainClaimed    = "sso.domain_claimed"
	actionDomainVerified   = "sso.domain_verified"
	actionDomainRemoved    = "sso.domain_removed"
	actionOperatorDisabled = "sso.operator_disabled"
	actionOperatorEnabled  = "sso.operator_enabled"
	actionLoginRefused     = "sso.login_refused"
	actionBreakGlassUsed   = "sso.break_glass_used"
	actionForeignRemoved   = "sso.foreign_user_removed"
	actionAutoProvisioned  = "member.auto_provisioned"
)

// Options configures a Service.
type Options struct {
	MainIssuer      string                                                   // the customer realm; tokens from other realms are not this package's business
	OperatorClients []string                                                 // operator client ids; a brokered session must never use them
	ReservedDomains []string                                                 // never claimable, subdomains and parents included
	AllowInternal   bool                                                     // development: IdPs on http and private addresses
	LookupTXT       func(ctx context.Context, name string) ([]string, error) // nil means DNS
}

// Service holds the SSO business rules.
type Service struct {
	repo     *Repo
	kc       *KCAdmin
	crypt    *textcrypt.Encryptor
	orgs     *org.Repo
	policy   *org.Policy
	users    *user.Repo
	audit    *audit.Service
	pat      *pgdoc.Store
	projects org.ProjectMemberAdder
	log      *slog.Logger
	opt      Options
	fetch    *fetcher
	lookup   func(ctx context.Context, name string) ([]string, error)

	snap     snapshotCache
	seen     *ttlCache[bool] // de-duplicates audit events written from the request path
	reserved []string
}

// NewService builds a Service. kc must be non-nil: without a service account the feature stays off
// and nothing constructs a Service at all.
func NewService(repo *Repo, kc *KCAdmin, crypt *textcrypt.Encryptor, orgs *org.Repo, policy *org.Policy, users *user.Repo,
	auditSvc *audit.Service, pat *pgdoc.Store, projects org.ProjectMemberAdder, log *slog.Logger, opt Options) *Service {
	reserved := make([]string, 0, len(opt.ReservedDomains))
	for _, d := range opt.ReservedDomains {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			reserved = append(reserved, d)
		}
	}
	lookup := opt.LookupTXT
	if lookup == nil {
		lookup = net.DefaultResolver.LookupTXT
	}
	return &Service{
		repo: repo, kc: kc, crypt: crypt, orgs: orgs, policy: policy, users: users, audit: auditSvc, pat: pat,
		projects: projects, log: log, opt: opt, fetch: newFetcher(opt.AllowInternal),
		lookup:   lookup,
		seen:     newTTLCache[bool](time.Hour),
		reserved: reserved,
	}
}

// ReservedFromURLs derives the operator's own domain from its public URLs: the host without its
// first label, or the host itself when it has only two. IP hosts yield nothing.
func ReservedFromURLs(urls ...string) []string {
	var out []string
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || net.ParseIP(u.Hostname()) != nil {
			continue
		}
		labels := strings.Split(u.Hostname(), ".")
		if len(labels) < 2 {
			continue
		}
		if len(labels) > 2 {
			labels = labels[1:]
		}
		if d := strings.Join(labels, "."); !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

func newAlias() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "org-" + hex.EncodeToString(b)
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "proxcloud-verify=" + hex.EncodeToString(b)
}

// domainOf returns the normalized domain of an email address, or "".
func domainOf(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}

var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*\.[a-z]{2,}$`)

func normalizeDomain(raw string) (string, error) {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if len(d) > 253 || !domainRe.MatchString(d) {
		return "", httpx.BadRequest("Invalid domain name")
	}
	return d, nil
}

func (s *Service) isReserved(domain string) bool {
	for _, r := range s.reserved {
		if domain == r || strings.HasSuffix(domain, "."+r) || strings.HasSuffix(r, "."+domain) {
			return true
		}
	}
	return false
}

// roleGrantable is the "SSO never creates admins" rule. It runs when the mapping is configured and
// again when it is used, because a custom role can gain permissions in between.
func (s *Service) roleGrantable(ctx context.Context, orgID, role string) error {
	if rbac.IsStaticRole(role) {
		if role != rbac.RoleMember {
			return httpx.BadRequest("Role '" + role + "' cannot be granted through SSO")
		}
		return nil
	}
	def, err := s.orgs.FindRoleByOrgAndName(ctx, orgID, role)
	if err != nil {
		return err
	}
	if def == nil {
		return httpx.BadRequest("No role named '" + role + "' in this organization")
	}
	if rbac.Matches(def.Permissions, rbac.OrganizationManageMembers) || rbac.Matches(def.Permissions, rbac.OrganizationManageRoles) {
		return httpx.BadRequest("Role '" + role + "' can manage members or roles and cannot be granted through SSO")
	}
	return nil
}

// pickRole maps groups to a role: groups in sorted order, first mapped one wins, else the default.
func pickRole(p *Provider, groups []string) string {
	sorted := slices.Sorted(slices.Values(groups))
	for _, g := range sorted {
		if r, ok := p.GroupRoleMap[g]; ok {
			return r
		}
	}
	return p.DefaultRole
}

// PutRequest is the org admin's desired configuration. An empty ClientSecret keeps the stored one.
type PutRequest struct {
	Protocol          string            `json:"protocol"`
	DiscoveryURL      string            `json:"discoveryUrl"`
	ClientID          string            `json:"clientId"`
	ClientSecret      string            `json:"clientSecret"`
	Scopes            string            `json:"scopes"`
	ProviderName      string            `json:"providerName"`
	DefaultRole       string            `json:"defaultRole"`
	GroupsClaim       string            `json:"groupsClaim"`
	GroupRoleMap      map[string]string `json:"groupRoleMap"`
	DefaultProjectIDs []string          `json:"defaultProjectIds"`
	AutoProvision     bool              `json:"autoProvision"`
	TrustEmail        bool              `json:"trustEmail"`
	Enabled           bool              `json:"enabled"`
	EnforceSSO        bool              `json:"enforceSso"`
	BreakGlassSubs    []string          `json:"breakGlassSubs"`
}

// Draft returns the org's provider, creating the draft that reserves its alias on first sight.
func (s *Service) Draft(ctx context.Context, orgID string) (*Provider, error) {
	p, err := s.repo.ProviderForOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if p != nil {
		if gone, err := s.repo.aliasRetired(ctx, p.Alias); err != nil {
			return nil, err
		} else if gone {
			return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "SSO deletion has started; retry deleting it before configuring a new provider")
		}
		return p, nil
	}
	for {
		alias := newAlias()
		if gone, err := s.repo.aliasRetired(ctx, alias); err != nil {
			return nil, err
		} else if gone {
			continue // even a random collision must not reuse a permanently revoked alias
		}
		p = &Provider{OrganizationID: orgID, Alias: alias}
		break
	}
	if err := s.repo.InsertProvider(ctx, p); err != nil {
		if pgdoc.IsDup(err) { // a concurrent first read won
			return s.repo.ProviderForOrg(ctx, orgID)
		}
		return nil, err
	}
	return p, nil
}

// Put validates and applies the configuration: Keycloak first, the database after, undoing a newly
// created Keycloak instance if the rest fails.
func (s *Service) Put(ctx context.Context, actor *user.User, orgID string, req PutRequest) (*Provider, error) {
	if !s.crypt.HasKey() {
		return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "SSO needs an encryption key to store the client secret")
	}
	if req.Protocol != protoOIDC && req.Protocol != protoSAML {
		return nil, httpx.BadRequest("Protocol must be oidc or saml")
	}
	if strings.TrimSpace(req.DiscoveryURL) == "" {
		return nil, httpx.BadRequest("A discovery or metadata URL is required")
	}
	if req.Protocol == protoOIDC && strings.TrimSpace(req.ClientID) == "" {
		return nil, httpx.BadRequest("A client id is required")
	}
	if req.DefaultRole == "" {
		req.DefaultRole = rbac.RoleMember
	}
	for _, role := range append(slices.Collect(maps.Values(req.GroupRoleMap)), req.DefaultRole) {
		if err := s.roleGrantable(ctx, orgID, role); err != nil {
			return nil, err
		}
	}
	if len(req.BreakGlassSubs) > maxBreakGlass {
		return nil, httpx.BadRequest("At most two break-glass owners are allowed")
	}
	for _, sub := range req.BreakGlassSubs {
		if !s.policy.IsOwner(ctx, orgID, sub) {
			return nil, httpx.BadRequest("Break-glass accounts must be owners of this organization")
		}
	}

	p, err := s.Draft(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if p.OperatorDisabled && req.Enabled {
		return nil, httpx.Forbidden("This identity provider was disabled by the platform operator")
	}
	if req.EnforceSSO {
		if !req.Enabled || p.OperatorDisabled {
			return nil, httpx.BadRequest("Enforcing SSO needs an enabled identity provider")
		}
		verified, err := s.hasVerifiedDomain(ctx, orgID)
		if err != nil {
			return nil, err
		}
		if !verified {
			return nil, httpx.BadRequest("Enforcing SSO needs at least one verified domain")
		}
		if len(req.BreakGlassSubs) == 0 {
			return nil, httpx.BadRequest("Enforcing SSO needs a break-glass owner, or a broken IdP locks everyone out")
		}
	}

	secret := req.ClientSecret
	if secret == "" && p.ClientSecretEnc != "" {
		secret = s.crypt.Decrypt(p.ClientSecretEnc)
	}
	if req.Protocol == protoOIDC && secret == "" {
		return nil, httpx.BadRequest("A client secret is required")
	}

	wasEnforcing := p.Active() && p.EnforceSSO
	p.Protocol, p.DiscoveryURL, p.ClientID = req.Protocol, strings.TrimSpace(req.DiscoveryURL), strings.TrimSpace(req.ClientID)
	p.Scopes, p.ProviderName, p.DefaultRole = strings.TrimSpace(req.Scopes), strings.TrimSpace(req.ProviderName), req.DefaultRole
	p.GroupsClaim, p.GroupRoleMap, p.DefaultProjectIDs = strings.TrimSpace(req.GroupsClaim), req.GroupRoleMap, req.DefaultProjectIDs
	p.AutoProvision, p.TrustEmail, p.Enabled, p.EnforceSSO = req.AutoProvision, req.TrustEmail, req.Enabled, req.EnforceSSO
	p.BreakGlassSubs = req.BreakGlassSubs
	if p.ProviderName == "" {
		p.ProviderName = "Company SSO"
	}
	if p.GroupsClaim == "" {
		p.GroupsClaim = defaultGroupsClaim
	}
	if p.Scopes == "" {
		p.Scopes = defaultScopes
	}
	if secret != "" {
		p.ClientSecretEnc = s.crypt.Encrypt(secret)
	}

	created, err := s.push(ctx, p, secret)
	if err != nil {
		return nil, err
	}
	if err := s.repo.SaveProvider(ctx, p); err != nil {
		if created {
			if delErr := s.kc.DeleteIdP(ctx, p.Alias); delErr != nil {
				s.log.Error("sso: could not undo a keycloak instance", "alias", p.Alias, "err", delErr)
			}
		}
		return nil, err
	}
	s.invalidate()
	if p.Active() && p.EnforceSSO && !wasEnforcing {
		if err := s.revokeMemberPATs(ctx, orgID, p.BreakGlassSubs); err != nil {
			s.log.Error("sso: revoking member API tokens", "org", orgID, "err", err)
		}
	}
	s.auditOrg(actor, orgID, actionUpdated, map[string]any{
		"alias": p.Alias, "protocol": p.Protocol, "enabled": p.Enabled, "enforceSso": p.EnforceSSO,
		"autoProvision": p.AutoProvision, "defaultRole": p.DefaultRole, "secretChanged": req.ClientSecret != "",
	})
	return p, nil
}

func (s *Service) hasVerifiedDomain(ctx context.Context, orgID string) (bool, error) {
	ds, err := s.repo.DomainsForOrg(ctx, orgID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(ds, func(d Domain) bool { return d.VerifiedAt != nil }), nil
}

// push writes the provider to Keycloak. created tells whether the instance is new.
func (s *Service) push(ctx context.Context, p *Provider, secret string) (created bool, err error) {
	doc, err := s.fetch.get(ctx, p.DiscoveryURL)
	if err != nil {
		return false, httpx.BadRequest("Identity provider metadata: " + err.Error())
	}
	imported, err := s.kc.ImportConfig(ctx, p.Protocol, doc)
	if err != nil {
		s.log.Warn("sso: metadata import refused", "alias", p.Alias, "err", err)
		return false, httpx.BadRequest("Identity provider metadata could not be parsed")
	}
	if len(imported) == 0 {
		return false, httpx.BadRequest("Identity provider metadata could not be parsed")
	}
	// These are the URLs Keycloak will call at login, chosen by the tenant through the document.
	for _, key := range []string{"authorizationUrl", "tokenUrl", "userInfoUrl", "jwksUrl", "logoutUrl",
		"singleSignOnServiceUrl", "singleLogoutServiceUrl"} {
		if v := imported[key]; v != "" {
			if err := s.fetch.checkURL(ctx, v); err != nil {
				return false, httpx.BadRequest("Identity provider metadata: " + key + ": " + err.Error())
			}
		}
	}
	rep := representation(p, imported, secret)
	existing, found, err := s.kc.GetIdP(ctx, p.Alias)
	if err != nil {
		return false, keycloakError(err)
	}
	if found {
		rep["internalId"] = existing["internalId"]
		err = s.kc.UpdateIdP(ctx, p.Alias, rep)
	} else {
		err = s.kc.CreateIdP(ctx, rep)
	}
	if err != nil {
		return false, keycloakError(err)
	}
	if err := s.kc.EnsureGroupsMapper(ctx, p.Alias, p.Protocol, p.GroupsClaim); err != nil {
		if !found {
			_ = s.kc.DeleteIdP(ctx, p.Alias)
		}
		return false, keycloakError(err)
	}
	return !found, nil
}

func keycloakError(err error) error {
	return httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, "The identity service refused the change: "+err.Error())
}

// representation builds the Keycloak instance. The IdP stays hidden from the shared login page and
// goes through the stock first-broker-login flow, which never links an existing account without
// re-authentication or an email round trip.
func representation(p *Provider, imported map[string]string, secret string) map[string]any {
	cfg := map[string]string{}
	for k, v := range imported {
		cfg[k] = v
	}
	cfg["syncMode"] = "FORCE"
	if p.Protocol == protoSAML {
		cfg["nameIDPolicyFormat"] = "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"
		cfg["principalType"] = "SUBJECT"
	} else {
		cfg["clientId"] = p.ClientID
		cfg["clientSecret"] = secret
		cfg["clientAuthMethod"] = "client_secret_post"
		cfg["defaultScope"] = p.Scopes
		cfg["pkceEnabled"] = "true"
		cfg["pkceMethod"] = "S256"
		cfg["validateSignature"] = "true"
		cfg["useJwksUrl"] = "true"
	}
	return map[string]any{
		"alias":                     p.Alias,
		"providerId":                p.Protocol,
		"displayName":               p.ProviderName,
		"enabled":                   p.Active(),
		"hideOnLogin":               true,
		"trustEmail":                p.TrustEmail,
		"storeToken":                false,
		"addReadTokenRoleOnCreate":  false,
		"authenticateByDefault":     false,
		"linkOnly":                  false,
		"firstBrokerLoginFlowAlias": "first broker login",
		"config":                    cfg,
	}
}

// Delete permanently retires the alias before removing the broker and the org's configuration.
// A partial failure keeps the alias refused; retry Delete to finish cleanup. Verified domains stay
// claimed, and users are released from the binding so a future IdP (with a new alias) can provision.
func (s *Service) Delete(ctx context.Context, actor *user.User, orgID string) error {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return err // the retirement must commit before any external effect, not with a caller's tx
	}
	p, err := s.repo.ProviderForOrg(ctx, orgID)
	if err != nil {
		return err
	}
	if p == nil {
		return httpx.NotFound("SSO is not configured")
	}
	// Only a stored, platform-issued alias is retired, never an arbitrary token claim. Commit the
	// fence first: deleting a Keycloak broker need not invalidate its existing or refreshable sessions.
	err = s.repo.retireAlias(ctx, p.Alias)
	s.invalidate() // do not keep a cached allow on this replica, including after a failed/uncertain write
	if err != nil {
		return err
	}
	if p.Configured() {
		if err := s.kc.DeleteIdP(ctx, p.Alias); err != nil {
			return keycloakError(err)
		}
	}
	if err := s.users.ClearSSOAlias(ctx, p.Alias); err != nil {
		return err
	}
	if err := s.repo.DeleteProvider(ctx, p.ID); err != nil {
		return err
	}
	s.invalidate()
	s.auditOrg(actor, orgID, actionRemoved, map[string]any{"alias": p.Alias})
	return nil
}

// TestResult is the outcome of a connectivity test; failures are data, never a 500.
type TestResult struct {
	OK     bool   `json:"ok"`
	Issuer string `json:"issuer,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Test fetches the discovery document or metadata behind the SSRF guard and checks its shape.
func (s *Service) Test(ctx context.Context, actor *user.User, orgID, protocol, rawURL string) TestResult {
	res := func() TestResult {
		doc, err := s.fetch.get(ctx, strings.TrimSpace(rawURL))
		if err != nil {
			return TestResult{Error: err.Error()}
		}
		if protocol == protoSAML {
			if !strings.Contains(string(doc), "EntityDescriptor") {
				return TestResult{Error: "not SAML metadata"}
			}
			return TestResult{OK: true}
		}
		var d struct {
			Issuer string `json:"issuer"`
			Auth   string `json:"authorization_endpoint"`
			Token  string `json:"token_endpoint"`
		}
		if json.Unmarshal(doc, &d) != nil || d.Issuer == "" || d.Auth == "" || d.Token == "" {
			return TestResult{Error: "not an OpenID Connect discovery document"}
		}
		return TestResult{OK: true, Issuer: d.Issuer}
	}()
	s.auditOrg(actor, orgID, actionTested, map[string]any{"url": rawURL, "ok": res.OK})
	return res
}

// ClaimDomain records a pending claim. Reserved domains and domains another organization already
// verified are refused without naming that organization.
func (s *Service) ClaimDomain(ctx context.Context, actor *user.User, orgID, raw string) (*Domain, error) {
	domain, err := normalizeDomain(raw)
	if err != nil {
		return nil, err
	}
	if s.isReserved(domain) {
		return nil, httpx.BadRequest("This domain is reserved")
	}
	owner, err := s.repo.VerifiedOwner(ctx, domain)
	if err != nil {
		return nil, err
	}
	if owner != "" && owner != orgID {
		return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "This domain is already claimed")
	}
	if existing, err := s.repo.FindClaim(ctx, orgID, domain); err != nil || existing != nil {
		return existing, err
	}
	claims, err := s.repo.DomainsForOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if len(claims) >= maxClaimsPerOrg {
		return nil, httpx.BadRequest("Too many domains")
	}
	d := &Domain{Domain: domain, OrganizationID: orgID, Token: newToken()}
	if err := s.repo.InsertClaim(ctx, d); err != nil {
		if pgdoc.IsDup(err) {
			return s.repo.FindClaim(ctx, orgID, domain)
		}
		return nil, err
	}
	s.auditOrg(actor, orgID, actionDomainClaimed, map[string]any{"domain": domain})
	return d, nil
}

// VerifyDomain checks the TXT record; a second organization loses to the unique index.
func (s *Service) VerifyDomain(ctx context.Context, actor *user.User, orgID, raw string) (*Domain, error) {
	domain, err := normalizeDomain(raw)
	if err != nil {
		return nil, err
	}
	d, err := s.repo.FindClaim(ctx, orgID, domain)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, httpx.NotFound("This domain is not claimed by this organization")
	}
	if d.VerifiedAt != nil {
		return d, nil
	}
	if s.isReserved(domain) { // reserved after the claim was made
		return nil, httpx.BadRequest("This domain is reserved")
	}
	records, err := s.lookup(ctx, challengePrefix+domain)
	if err != nil || !slices.Contains(records, d.Token) {
		return nil, httpx.BadRequest("The TXT record was not found at " + challengePrefix + domain)
	}
	if err := s.repo.MarkVerified(ctx, d); err != nil {
		if pgdoc.IsDup(err) {
			return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "This domain is already claimed")
		}
		return nil, err
	}
	s.invalidate()
	s.auditOrg(actor, orgID, actionDomainVerified, map[string]any{"domain": domain})
	return d, nil
}

// RemoveDomain drops a claim. The last verified domain of an enforcing org stays: without it the
// org's own IdP could no longer log anyone in while enforcement keeps everyone else out.
func (s *Service) RemoveDomain(ctx context.Context, actor *user.User, orgID, raw string) error {
	domain, err := normalizeDomain(raw)
	if err != nil {
		return err
	}
	p, err := s.repo.ProviderForOrg(ctx, orgID)
	if err != nil {
		return err
	}
	if p != nil && p.EnforceSSO {
		ds, err := s.repo.DomainsForOrg(ctx, orgID)
		if err != nil {
			return err
		}
		verified := slices.DeleteFunc(ds, func(d Domain) bool { return d.VerifiedAt == nil || d.Domain == domain })
		if len(verified) == 0 {
			return httpx.BadRequest("Turn off SSO enforcement before removing the last verified domain")
		}
	}
	removed, err := s.repo.DeleteClaim(ctx, orgID, domain)
	if err != nil {
		return err
	}
	if !removed {
		return httpx.NotFound("This domain is not claimed by this organization")
	}
	s.invalidate()
	s.auditOrg(actor, orgID, actionDomainRemoved, map[string]any{"domain": domain})
	return nil
}

// Discovery is what the login page learns about an email: nothing that identifies the org.
type Discovery struct {
	Found        bool   `json:"found"`
	Alias        string `json:"alias,omitempty"`
	ProviderName string `json:"providerName,omitempty"`
}

// Discover maps an email to its organization's IdP. Only verified domains and active IdPs count.
func (s *Service) Discover(ctx context.Context, email string) Discovery {
	snap, err := s.snapshot(ctx)
	if err != nil {
		return Discovery{}
	}
	p := snap.byOrg[snap.domainOrg[domainOf(email)]]
	if p == nil || !p.Active() {
		return Discovery{}
	}
	return Discovery{Found: true, Alias: p.Alias, ProviderName: p.ProviderName}
}

// Provision is the JIT step, run from POST /user after the guard already accepted the token.
// An existing membership is never touched. A role that stopped being grantable provisions nothing.
func (s *Service) Provision(ctx context.Context, rc *httpx.RequestContext, u *user.User) error {
	if rc == nil || rc.IdP == "" {
		return nil
	}
	snap, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	p := snap.byAlias[rc.IdP]
	if p == nil || !p.AutoProvision {
		return nil
	}
	// A guard may have accepted an older snapshot just before deletion on another replica. Do not
	// grant membership from that snapshot once the primary has a durable retirement fence.
	if gone, err := s.repo.aliasRetired(ctx, p.Alias); err != nil {
		return err
	} else if gone {
		return nil
	}
	member, err := s.orgs.FindMember(ctx, p.OrganizationID, u.Sub)
	if err != nil || member != nil {
		return err
	}
	role := pickRole(p, rc.IdPGroups)
	if err := s.roleGrantable(ctx, p.OrganizationID, role); err != nil {
		ev := s.orgEvent(u, p.OrganizationID, actionAutoProvisioned, map[string]any{"alias": p.Alias, "role": role})
		ev.Outcome, ev.ErrorMessage = audit.OutcomeFailure, err.Error()
		s.audit.LogAsync(ev)
		return nil
	}
	if _, err := s.orgs.AddMember(ctx, p.OrganizationID, u.Sub, role); err != nil {
		if pgdoc.IsDup(err) { // a concurrent login provisioned it
			return nil
		}
		return err
	}
	if len(p.DefaultProjectIDs) > 0 && s.projects != nil {
		if err := s.projects.AddMemberToOrgProjects(ctx, p.OrganizationID, u.Sub, p.DefaultProjectIDs); err != nil {
			s.log.Warn("sso: default projects not applied", "org", p.OrganizationID, "err", err)
		}
	}
	s.audit.LogAsync(s.orgEvent(u, p.OrganizationID, actionAutoProvisioned, map[string]any{
		"alias": p.Alias, "role": role, "email": u.Email,
	}))
	return nil
}

// OnMemberRemoved revokes the API tokens of someone removed from an enforcing org.
func (s *Service) OnMemberRemoved(ctx context.Context, orgID, sub string) {
	p, err := s.repo.ProviderForOrg(ctx, orgID)
	if err != nil || p == nil || !p.Active() || !p.EnforceSSO {
		return
	}
	if _, err := s.pat.DeleteMany(ctx, pgdoc.M{"sub": sub}); err != nil {
		s.log.Error("sso: revoking API tokens", "sub", sub, "err", err)
	}
}

// revokeMemberPATs runs when enforcement turns on. The guard already refuses these tokens; deleting
// them means they do not come back to life if enforcement is ever turned off.
func (s *Service) revokeMemberPATs(ctx context.Context, orgID string, keep []string) error {
	members, err := s.orgs.Members(ctx, orgID)
	if err != nil {
		return err
	}
	subs := make([]string, 0, len(members))
	for _, m := range members {
		if !slices.Contains(keep, m.Sub) {
			subs = append(subs, m.Sub)
		}
	}
	if len(subs) == 0 {
		return nil
	}
	_, err = s.pat.DeleteMany(ctx, pgdoc.M{"sub": pgdoc.M{"$in": subs}})
	return err
}

// OperatorView is one row of the operator's list.
type OperatorView struct {
	OrganizationID   string   `json:"organizationId"`
	OrganizationName string   `json:"organizationName,omitempty"`
	Alias            string   `json:"alias"`
	Protocol         string   `json:"protocol"`
	ProviderName     string   `json:"providerName"`
	Enabled          bool     `json:"enabled"`
	EnforceSSO       bool     `json:"enforceSso"`
	OperatorDisabled bool     `json:"operatorDisabled"`
	Domains          []string `json:"domains"`
}

// OperatorList returns every configured provider, drafts excluded, with its verified domains.
func (s *Service) OperatorList(ctx context.Context) ([]OperatorView, error) {
	providers, err := s.repo.AllProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]OperatorView, 0, len(providers))
	for i := range providers {
		p := &providers[i]
		if !p.Configured() {
			continue
		}
		v := OperatorView{OrganizationID: p.OrganizationID, Alias: p.Alias, Protocol: p.Protocol, ProviderName: p.ProviderName,
			Enabled: p.Enabled, EnforceSSO: p.EnforceSSO, OperatorDisabled: p.OperatorDisabled, Domains: []string{}}
		if o, err := s.orgs.FindByID(ctx, p.OrganizationID); err == nil && o != nil {
			v.OrganizationName = o.Name
		}
		ds, err := s.repo.DomainsForOrg(ctx, p.OrganizationID)
		if err != nil {
			return nil, err
		}
		for _, d := range ds {
			if d.VerifiedAt != nil {
				v.Domains = append(v.Domains, d.Domain)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// OperatorSetDisabled is the emergency stop: the broker is switched off in Keycloak, the guard
// refuses its sessions, enforcement stops applying, and the org cannot re-enable it.
func (s *Service) OperatorSetDisabled(ctx context.Context, actorSub, orgID string, disabled bool) error {
	p, err := s.repo.ProviderForOrg(ctx, orgID)
	if err != nil {
		return err
	}
	if p == nil || !p.Configured() {
		return httpx.NotFound("SSO is not configured")
	}
	if gone, err := s.repo.aliasRetired(ctx, p.Alias); err != nil {
		return err
	} else if gone {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "SSO deletion has started; retry deleting it before configuring a new provider")
	}
	p.OperatorDisabled = disabled
	if err := s.syncEnabled(ctx, p); err != nil {
		return keycloakError(err)
	}
	if err := s.repo.SaveProvider(ctx, p); err != nil {
		return err
	}
	s.invalidate()
	action := actionOperatorEnabled
	if disabled {
		action = actionOperatorDisabled
	}
	ev := audit.AdminEvent(actorSub, "")
	ev.EventContext, ev.Action, ev.ResourceType, ev.ResourceID = audit.ContextOrganization, action, resourceSSO, p.Alias
	ev.OrganizationID, ev.Outcome = orgID, audit.OutcomeSuccess
	s.audit.LogAsync(ev)
	return nil
}

// OperatorReleaseDomain frees a verified domain after a dispute.
func (s *Service) OperatorReleaseDomain(ctx context.Context, actorSub, raw string) error {
	domain, err := normalizeDomain(raw)
	if err != nil {
		return err
	}
	owner, err := s.repo.VerifiedOwner(ctx, domain)
	if err != nil {
		return err
	}
	removed, err := s.repo.DeleteVerified(ctx, domain)
	if err != nil {
		return err
	}
	if !removed {
		return httpx.NotFound("No organization holds this domain")
	}
	s.invalidate()
	ev := audit.AdminEvent(actorSub, "")
	ev.EventContext, ev.Action, ev.ResourceType, ev.ResourceID = audit.ContextOrganization, actionDomainRemoved, resourceSSO, domain
	ev.OrganizationID, ev.Outcome = owner, audit.OutcomeSuccess
	s.audit.LogAsync(ev)
	return nil
}

func (s *Service) syncEnabled(ctx context.Context, p *Provider) error {
	rep, found, err := s.kc.GetIdP(ctx, p.Alias)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("instance is missing")
	}
	if rep["enabled"] == p.Active() {
		return nil
	}
	rep["enabled"] = p.Active()
	return s.kc.UpdateIdP(ctx, p.Alias, rep)
}

// Reconcile makes Keycloak match the database (instances lost in a Keycloak restore come back from
// the stored secret) and removes the accounts foreign IdPs left behind.
func (s *Service) Reconcile(ctx context.Context) error {
	providers, err := s.repo.AllProviders(ctx)
	if err != nil {
		return err
	}
	retired, err := s.repo.retiredAliases(ctx)
	if err != nil {
		return err // never recreate a deletion-in-progress when its durable fence cannot be read
	}
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for i := range providers {
		p := &providers[i]
		if _, gone := retired[p.Alias]; gone || !p.Configured() {
			continue
		}
		_, found, err := s.kc.GetIdP(ctx, p.Alias)
		switch {
		case err != nil:
			keep(err)
			continue
		case !found:
			s.log.Warn("sso: recreating a missing keycloak instance", "alias", p.Alias)
			_, err = s.push(ctx, p, s.crypt.Decrypt(p.ClientSecretEnc))
		default:
			err = s.syncEnabled(ctx, p)
		}
		keep(err)
	}
	keep(s.SweepForeignUsers(ctx))
	return firstErr
}

// SweepForeignUsers deletes Keycloak accounts created when an IdP asserted an email outside its own
// verified domains. The guard already refuses their tokens, but left alone such an account blocks
// the address's real owner, whose own IdP would hit "account already exists". An account is only
// removed when it has nothing else: no ProxCloud user, no credential, no second IdP link.
func (s *Service) SweepForeignUsers(ctx context.Context) error {
	snap, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	for _, p := range snap.byAlias {
		users, err := s.kc.UsersOfIdP(ctx, p.Alias)
		if err != nil {
			return err
		}
		for _, ku := range users {
			if snap.domainOrg[domainOf(ku.Email)] == p.OrganizationID {
				continue
			}
			if existing, err := s.users.FindBySub(ctx, ku.ID); err != nil || existing != nil {
				continue
			}
			ok, err := s.kc.IsDisposable(ctx, ku.ID, p.Alias)
			if err != nil || !ok {
				continue
			}
			if err := s.kc.DeleteUser(ctx, ku.ID); err != nil {
				return err
			}
			ev := audit.SystemEvent()
			ev.EventContext, ev.Action, ev.ResourceType, ev.ResourceID = audit.ContextOrganization, actionForeignRemoved, resourceSSO, p.Alias
			ev.OrganizationID, ev.Outcome = p.OrganizationID, audit.OutcomeSuccess
			ev.ResourceMetadata = map[string]any{"email": ku.Email}
			s.audit.LogAsync(ev)
		}
	}
	return nil
}

// Run keeps Keycloak reconciled; it also covers Keycloak coming up after the API.
func (s *Service) Run(ctx context.Context) {
	for {
		if err := s.Reconcile(ctx); err != nil {
			s.log.Warn("sso: reconcile", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconcileEvery):
		}
	}
}

func (s *Service) orgEvent(u *user.User, orgID, action string, metadata map[string]any) audit.AuditEvent {
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext, ev.Action, ev.ResourceType = audit.ContextOrganization, action, resourceSSO
	ev.ResourceID, ev.OrganizationID, ev.Outcome = orgID, orgID, audit.OutcomeSuccess
	ev.ResourceMetadata = metadata
	return ev
}

func (s *Service) auditOrg(u *user.User, orgID, action string, metadata map[string]any) {
	s.audit.LogAsync(s.orgEvent(u, orgID, action, metadata))
}
