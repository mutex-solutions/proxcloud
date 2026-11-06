package sso

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const adminPermission = "admin:sso:manage"

// Handler serves the SSO endpoints. A nil *Handler (no service account configured) mounts nothing,
// and the login page simply never finds an IdP.
type Handler struct {
	svc     *Service
	policy  *org.Policy
	users   *user.Repo
	require func(w http.ResponseWriter, r *http.Request, key string) bool
	limiter *rateLimiter
	issuer  string
}

// NewHandler builds the handler. requireAdmin is the admin console's permission gate.
func NewHandler(svc *Service, policy *org.Policy, users *user.Repo, issuer string,
	requireAdmin func(w http.ResponseWriter, r *http.Request, key string) bool) *Handler {
	return &Handler{svc: svc, policy: policy, users: users, require: requireAdmin, issuer: strings.TrimRight(issuer, "/"),
		limiter: &rateLimiter{limit: discoverPerMinute, hits: map[string]*window{}}}
}

func adminSub(r *http.Request) string {
	rc := httpx.RC(r.Context())
	if rc.Sub != "" {
		return rc.Sub
	}
	return rc.SigV4KeyID
}

// Routes mounts everything under /api/v1.
func (h *Handler) Routes(r chi.Router) {
	r.Post("/auth/sso/discover", h.discover)

	r.Get("/organizations/{id}/sso", h.get)
	r.Put("/organizations/{id}/sso", h.put)
	r.Delete("/organizations/{id}/sso", h.delete)
	r.Post("/organizations/{id}/sso/test", h.test)
	r.Post("/organizations/{id}/sso/domains", h.claimDomain)
	r.Post("/organizations/{id}/sso/domains/{domain}/verify", h.verifyDomain)
	r.Delete("/organizations/{id}/sso/domains/{domain}", h.removeDomain)

	r.Get("/admin/sso", h.adminList)
	r.Post("/admin/sso/{orgId}/disable", h.adminDisable)
	r.Post("/admin/sso/{orgId}/enable", h.adminEnable)
	r.Delete("/admin/sso/domains/{domain}", h.adminReleaseDomain)
}

func fail(w http.ResponseWriter, err error) {
	if !httpx.WriteError(w, err) {
		httpx.Err(w, http.StatusInternalServerError, 500, "internal.error")
	}
}

// orgAdmin gates org endpoints on organization:manage_roles: whoever can hand out roles is who may
// decide which IdP can hand them out.
func (h *Handler) orgAdmin(w http.ResponseWriter, r *http.Request) (*user.User, string, bool) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		fail(w, err)
		return nil, "", false
	}
	id := chi.URLParam(r, "id")
	if err := h.policy.RequirePermission(r.Context(), u.Sub, id, rbac.OrganizationManageRoles); err != nil {
		fail(w, err)
		return nil, "", false
	}
	return u, id, true
}

type domainView struct {
	Domain     string     `json:"domain"`
	Verified   bool       `json:"verified"`
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
	RecordName string     `json:"recordName"`
	RecordText string     `json:"recordValue"`
}

func toDomainView(d *Domain) domainView {
	return domainView{Domain: d.Domain, Verified: d.VerifiedAt != nil, VerifiedAt: d.VerifiedAt,
		RecordName: challengePrefix + d.Domain, RecordText: d.Token}
}

type providerView struct {
	Configured        bool              `json:"configured"`
	Alias             string            `json:"alias"`
	RedirectURI       string            `json:"redirectUri"`
	SPMetadataURL     string            `json:"spMetadataUrl"`
	Protocol          string            `json:"protocol,omitempty"`
	DiscoveryURL      string            `json:"discoveryUrl,omitempty"`
	ClientID          string            `json:"clientId,omitempty"`
	HasSecret         bool              `json:"hasSecret"`
	Scopes            string            `json:"scopes,omitempty"`
	ProviderName      string            `json:"providerName,omitempty"`
	DefaultRole       string            `json:"defaultRole,omitempty"`
	GroupsClaim       string            `json:"groupsClaim,omitempty"`
	GroupRoleMap      map[string]string `json:"groupRoleMap"`
	DefaultProjectIDs []string          `json:"defaultProjectIds"`
	AutoProvision     bool              `json:"autoProvision"`
	TrustEmail        bool              `json:"trustEmail"`
	Enabled           bool              `json:"enabled"`
	EnforceSSO        bool              `json:"enforceSso"`
	BreakGlassSubs    []string          `json:"breakGlassSubs"`
	OperatorDisabled  bool              `json:"operatorDisabled"`
	Domains           []domainView      `json:"domains"`
}

func (h *Handler) view(r *http.Request, p *Provider) (*providerView, error) {
	ds, err := h.svc.repo.DomainsForOrg(r.Context(), p.OrganizationID)
	if err != nil {
		return nil, err
	}
	v := &providerView{
		Configured: p.Configured(), Alias: p.Alias,
		RedirectURI:   h.issuer + "/broker/" + p.Alias + "/endpoint",
		SPMetadataURL: h.issuer + "/broker/" + p.Alias + "/endpoint/descriptor",
		Protocol:      p.Protocol, DiscoveryURL: p.DiscoveryURL, ClientID: p.ClientID, HasSecret: p.ClientSecretEnc != "",
		Scopes: p.Scopes, ProviderName: p.ProviderName, DefaultRole: p.DefaultRole, GroupsClaim: p.GroupsClaim,
		GroupRoleMap: p.GroupRoleMap, DefaultProjectIDs: p.DefaultProjectIDs,
		AutoProvision: p.AutoProvision, TrustEmail: p.TrustEmail, Enabled: p.Enabled, EnforceSSO: p.EnforceSSO,
		BreakGlassSubs: p.BreakGlassSubs, OperatorDisabled: p.OperatorDisabled, Domains: []domainView{},
	}
	if v.GroupRoleMap == nil {
		v.GroupRoleMap = map[string]string{}
	}
	if v.DefaultProjectIDs == nil {
		v.DefaultProjectIDs = []string{}
	}
	if v.BreakGlassSubs == nil {
		v.BreakGlassSubs = []string{}
	}
	for i := range ds {
		v.Domains = append(v.Domains, toDomainView(&ds[i]))
	}
	return v, nil
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	_, id, ok := h.orgAdmin(w, r)
	if !ok {
		return
	}
	p, err := h.svc.Draft(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	v, err := h.view(r, p)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, v)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	u, id, ok := h.orgAdmin(w, r)
	if !ok {
		return
	}
	var req PutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Err(w, http.StatusBadRequest, 400, "Malformed request")
		return
	}
	p, err := h.svc.Put(r.Context(), u, id, req)
	if err != nil {
		fail(w, err)
		return
	}
	v, err := h.view(r, p)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, v)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	u, id, ok := h.orgAdmin(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), u, id); err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, "Successful operation")
}

func (h *Handler) test(w http.ResponseWriter, r *http.Request) {
	u, id, ok := h.orgAdmin(w, r)
	if !ok {
		return
	}
	var req struct {
		Protocol     string `json:"protocol"`
		DiscoveryURL string `json:"discoveryUrl"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	httpx.OK(w, h.svc.Test(r.Context(), u, id, req.Protocol, req.DiscoveryURL))
}

func (h *Handler) claimDomain(w http.ResponseWriter, r *http.Request) {
	u, id, ok := h.orgAdmin(w, r)
	if !ok {
		return
	}
	var req struct {
		Domain string `json:"domain"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	d, err := h.svc.ClaimDomain(r.Context(), u, id, req.Domain)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, toDomainView(d))
}

func (h *Handler) verifyDomain(w http.ResponseWriter, r *http.Request) {
	u, id, ok := h.orgAdmin(w, r)
	if !ok {
		return
	}
	d, err := h.svc.VerifyDomain(r.Context(), u, id, chi.URLParam(r, "domain"))
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, toDomainView(d))
}

func (h *Handler) removeDomain(w http.ResponseWriter, r *http.Request) {
	u, id, ok := h.orgAdmin(w, r)
	if !ok {
		return
	}
	if err := h.svc.RemoveDomain(r.Context(), u, id, chi.URLParam(r, "domain")); err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, "Successful operation")
}

func (h *Handler) adminList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, adminPermission) {
		return
	}
	out, err := h.svc.OperatorList(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	httpx.List(w, out)
}

func (h *Handler) adminDisable(w http.ResponseWriter, r *http.Request) { h.adminSet(w, r, true) }
func (h *Handler) adminEnable(w http.ResponseWriter, r *http.Request)  { h.adminSet(w, r, false) }

func (h *Handler) adminSet(w http.ResponseWriter, r *http.Request, disabled bool) {
	if !h.require(w, r, adminPermission) {
		return
	}
	if err := h.svc.OperatorSetDisabled(r.Context(), adminSub(r), chi.URLParam(r, "orgId"), disabled); err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, "Successful operation")
}

func (h *Handler) adminReleaseDomain(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, adminPermission) {
		return
	}
	if err := h.svc.OperatorReleaseDomain(r.Context(), adminSub(r), chi.URLParam(r, "domain")); err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, "Successful operation")
}

const discoverPerMinute = 30

func (h *Handler) discover(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.allow(clientIP(r)) {
		httpx.Err(w, http.StatusTooManyRequests, http.StatusTooManyRequests, "Too many requests")
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	httpx.OK(w, h.svc.Discover(r.Context(), req.Email))
}

// clientIP takes the last X-Forwarded-For hop, the one our own ingress added; earlier ones are
// whatever the caller chose to send.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter is a fixed window per key.
// ponytail: per replica, so the effective limit is limit*replicas; a shared store if abuse shows up.
type rateLimiter struct {
	mu    sync.Mutex
	limit int
	hits  map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.hits) > maxCacheEntries {
		l.hits = map[string]*window{}
	}
	w := l.hits[key]
	if w == nil || now.Sub(w.start) >= time.Minute {
		l.hits[key] = &window{start: now, n: 1}
		return true
	}
	w.n++
	return w.n <= l.limit
}
