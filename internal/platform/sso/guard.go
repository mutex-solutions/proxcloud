package sso

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Refusal codes. The client console matches on these.
const (
	RefusedRequired     = "sso_required"           // this person must sign in through their org's IdP
	RefusedIdPDisabled  = "sso_idp_disabled"       // the session's IdP is off or gone
	RefusedDomain       = "sso_domain_not_allowed" // the IdP asserted an email outside its verified domains
	RefusedIdPMismatch  = "sso_idp_mismatch"       // this account belongs to another IdP
	RefusedOperator     = "sso_operator_refused"   // a brokered session on an operator client
	RefusedScopeMissing = "sso_scope_missing"      // the token cannot tell whether the session was brokered
	RefusedUnavailable  = "sso_unavailable"        // the rules could not be loaded
)

// snapshot is the read side of the rules, cached for a minute so the guard costs nothing per request.
type snapshot struct {
	byAlias   map[string]*Provider // active and disabled, drafts and retired aliases excluded
	byOrg     map[string]*Provider
	domainOrg map[string]string   // verified domain -> org
	enforcing []*Provider         // active providers with enforcement on
	retired   map[string]struct{} // permanently retired platform-issued aliases, including when no providers remain
}

type snapshotCache struct {
	mu   sync.Mutex
	cur  *snapshot
	load time.Time
}

const snapshotTTL = time.Minute

func (s *Service) snapshot(ctx context.Context) (*snapshot, error) {
	s.snap.mu.Lock()
	defer s.snap.mu.Unlock()
	if s.snap.cur != nil && time.Since(s.snap.load) < snapshotTTL {
		return s.snap.cur, nil
	}
	providers, err := s.repo.AllProviders(ctx)
	var domains []Domain
	if err == nil {
		domains, err = s.repo.VerifiedDomains(ctx)
	}
	var retired map[string]struct{}
	if err == nil {
		retired, err = s.repo.retiredAliases(ctx)
	}
	if err == nil {
		next := &snapshot{byAlias: map[string]*Provider{}, byOrg: map[string]*Provider{}, domainOrg: map[string]string{}, retired: retired}
		for i := range providers {
			p := &providers[i]
			if _, gone := retired[p.Alias]; gone || !p.Configured() {
				continue
			}
			next.byAlias[p.Alias] = p
			next.byOrg[p.OrganizationID] = p
			if p.Active() && p.EnforceSSO {
				next.enforcing = append(next.enforcing, p)
			}
		}
		for _, d := range domains {
			next.domainOrg[d.Domain] = d.OrganizationID
		}
		s.snap.cur, s.snap.load = next, time.Now()
		return next, nil
	}
	if s.snap.cur != nil { // retain the existing stale-rule policy, including retired aliases
		// On other replicas an outage can delay new revocations beyond the normal one-minute TTL.
		// A fresh or locally invalidated cache has no fallback and fails closed instead.
		s.log.Warn("sso: keeping the previous rules", "err", err)
		return s.snap.cur, nil
	}
	return nil, err
}

func (s *Service) invalidate() {
	s.snap.mu.Lock()
	s.snap.cur = nil
	s.snap.mu.Unlock()
}

// Guard decides whether an authenticated request may proceed. It returns "" or a refusal code. It
// runs on every request, before any handler, so a refused identity never gets a ProxCloud user.
func (s *Service) Guard(ctx context.Context, rc *httpx.RequestContext) string {
	if rc == nil || rc.Sub == "" || rc.SigV4KeyID != "" || rc.Issuer != s.opt.MainIssuer {
		return "" // operators in their own realm and hmac keys are out of scope: tenant SSO cannot block them
	}
	operatorClient := slices.Contains(s.opt.OperatorClients, rc.Azp)
	if operatorClient && rc.IdP != "" {
		return s.refuse(rc, "", RefusedOperator) // only reachable when operators share the customer realm
	}
	snap, err := s.snapshot(ctx)
	if err != nil {
		return RefusedUnavailable
	}
	// identity_provider is a realm-wide session note: unknown aliases may be legitimate social
	// brokers added by the operator. Revoke only aliases durably retired from our provider docs,
	// before the no-org-providers shortcut; a prefix or an unknown alias is not a tombstone.
	if _, gone := snap.retired[rc.IdP]; rc.IdP != "" && gone {
		return s.refuse(rc, "", RefusedIdPDisabled)
	}
	if len(snap.byAlias) == 0 || operatorClient {
		return ""
	}
	// Without the scope a brokered session looks like a local one. Only tokens from a client that
	// opted out can lack it, and only /mcp accepts foreign clients at all.
	if !rc.PAT && !rc.SSOScope {
		return s.refuse(rc, "", RefusedScopeMissing)
	}

	// Read fresh, not cached: a binding or membership that is a minute old is exactly the window an
	// attacker would use. Both are indexed lookups and only happen when SSO is in play for this token.
	var account *user.User
	if rc.IdP != "" || rc.PAT {
		if account, err = s.users.FindBySub(ctx, rc.Sub); err != nil {
			return RefusedUnavailable
		}
	}
	email := rc.Email
	if rc.PAT {
		if account == nil {
			return s.refuse(rc, "", RefusedUnavailable)
		}
		email = account.Email
	}

	if rc.IdP != "" {
		p := snap.byAlias[rc.IdP]
		if p == nil || !p.Active() {
			return s.refuse(rc, "", RefusedIdPDisabled)
		}
		// An IdP may only speak for its own org's verified domains.
		if !rc.EmailVerified || snap.domainOrg[domainOf(email)] != p.OrganizationID {
			return s.refuse(rc, p.OrganizationID, RefusedDomain)
		}
		// An account belongs to the IdP that first provisioned it. A local account with no binding
		// yet can only be claimed when its existing email is already in this org's domains, since
		// the IdP can rewrite the current email claim at will.
		if account != nil {
			if account.SSOAlias != "" && account.SSOAlias != rc.IdP {
				return s.refuse(rc, p.OrganizationID, RefusedIdPMismatch)
			}
			if account.SSOAlias == "" && snap.domainOrg[domainOf(account.Email)] != p.OrganizationID {
				return s.refuse(rc, p.OrganizationID, RefusedIdPMismatch)
			}
		}
	}

	if len(snap.enforcing) == 0 {
		return ""
	}
	orgs, err := s.orgs.OrgIDsForSub(ctx, rc.Sub)
	if err != nil {
		return RefusedUnavailable
	}
	for _, p := range snap.enforcing {
		covered := snap.domainOrg[domainOf(email)] == p.OrganizationID || slices.Contains(orgs, p.OrganizationID)
		if !covered || rc.IdP == p.Alias {
			continue
		}
		if slices.Contains(p.BreakGlassSubs, rc.Sub) && s.policy.IsOwner(ctx, p.OrganizationID, rc.Sub) {
			s.auditOnce(rc, p.OrganizationID, actionBreakGlassUsed, audit.OutcomeSuccess, "")
			continue
		}
		return s.refuse(rc, p.OrganizationID, RefusedRequired)
	}
	return ""
}

func (s *Service) refuse(rc *httpx.RequestContext, orgID, code string) string {
	s.auditOnce(rc, orgID, actionLoginRefused, audit.OutcomeFailure, code)
	return code
}

// auditOnce writes at most one event per person, org and reason an hour: the guard sees every request.
func (s *Service) auditOnce(rc *httpx.RequestContext, orgID, action, outcome, code string) {
	key := rc.Sub + "|" + orgID + "|" + action + "|" + code
	if _, dup := s.seen.get(key); dup {
		return
	}
	s.seen.put(key, true)
	ev := audit.ClientUserEvent(rc.Sub, rc.Email)
	ev.EventContext, ev.Action, ev.ResourceType = audit.ContextOrganization, action, resourceSSO
	ev.ResourceID, ev.OrganizationID, ev.Outcome, ev.ErrorMessage = rc.IdP, orgID, outcome, code
	ev.ResourceMetadata = map[string]any{"email": rc.Email, "alias": rc.IdP}
	s.audit.LogAsync(ev)
}

// ttlCache is a small map with expiry.
// ponytail: wiped wholesale at maxCacheEntries, an LRU if that ever shows up in latency.
type ttlCache[V any] struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]ttlEntry[V]
}

type ttlEntry[V any] struct {
	v   V
	exp time.Time
}

const maxCacheEntries = 50000

func newTTLCache[V any](ttl time.Duration) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, m: map[string]ttlEntry[V]{}}
}

func (c *ttlCache[V]) get(k string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Now().After(e.exp) {
		var zero V
		return zero, false
	}
	return e.v, true
}

func (c *ttlCache[V]) put(k string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= maxCacheEntries {
		c.m = map[string]ttlEntry[V]{}
	}
	c.m[k] = ttlEntry[V]{v: v, exp: time.Now().Add(c.ttl)}
}
