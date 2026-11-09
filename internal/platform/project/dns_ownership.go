package project

// DNS ownership authorizes ONE new zone; it is not a global reservation. Pending
// rows never enter an overlap check, and DNS/PowerDNS are read before any create.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/cloud/powerdns"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const (
	dnsChallengeTTL = 30 * time.Minute
	dnsVerifiedTTL  = 10 * time.Minute
	dnsVerifyDelay  = 10 * time.Second
	dnsProofPrefix  = "_proxcloud-ownership."
	dnsCreateLock   = "dns-ownership:create" // all replicas/providers/aliases in this database
)

type dnsOwnership struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"projectId"`
	ServiceID  string     `json:"serviceId"`
	Name       string     `json:"name"`
	TokenHash  string     `json:"tokenHash"`
	Binding    string     `json:"binding"`
	State      string     `json:"state"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	VerifiedAt *time.Time `json:"verifiedAt,omitempty"`
	VerifiedTo *time.Time `json:"verifiedUntil,omitempty"`
	NextVerify *time.Time `json:"nextVerifyAt,omitempty"`
}

// SetDNSOwnership enables new-zone creation only when its durable proof store is
// wired. A nil store fails closed; legacy record reads/writes/deletes stay usable.
func (h *Handler) SetDNSOwnership(s *pgdoc.Store) { h.dnsOwnership = s }

func dnsConflict(message string) error { return httpx.NewError(409, 409, message) }

func (d *dnsOwnership) state(now time.Time) string {
	if (d.State == "PENDING" && !now.Before(d.ExpiresAt)) ||
		(d.State == "VERIFIED" && (d.VerifiedTo == nil || !now.Before(*d.VerifiedTo))) {
		return "EXPIRED"
	}
	return d.State
}

// view deliberately omits the hash, provider internals and raw token. Only the
// challenge POST adds token/recordValue to its response, never status or audits.
func (d *dnsOwnership) view(now time.Time) map[string]any {
	return map[string]any{"id": d.ID, "serviceId": d.ServiceID, "name": d.Name, "state": d.state(now),
		"recordName": dnsProofPrefix + d.Name, "recordType": "TXT", "expiresAt": d.ExpiresAt,
		"verifiedUntil": d.VerifiedTo, "nextVerifyAt": d.NextVerify}
}

func proofHash(projectID, serviceID, name, token string) string {
	hash := sha256.Sum256([]byte(projectID + "\x00" + serviceID + "\x00" + name + "\x00" + token))
	return hex.EncodeToString(hash[:])
}

// dnsConfig binds the proof to the operator's current upstream and trust policy.
// Changing the API target, NS or verification resolvers invalidates old proofs.
func dnsConfig(es *externalservice.ExternalService) (*powerdns.Service, []string, string, error) {
	ds, err := es.PowerDNSService()
	if err != nil {
		return nil, nil, "", err
	}
	resolvers, err := powerdns.OwnershipResolvers(es.Config["ownershipResolvers"])
	if err != nil {
		return nil, nil, "", err
	}
	b, _ := json.Marshal([]any{ds.URL, ds.ServerID, ds.Nameservers, resolvers})
	hash := sha256.Sum256(b)
	return ds, resolvers, hex.EncodeToString(hash[:]), nil
}

func (h *Handler) dnsProvider(ctx context.Context, p *Project, serviceID string) (*externalservice.ExternalService, error) {
	// Do not reuse resolveServiceID: its fallback on foreign/unattached input is
	// convenient for old cloud routes but unsafe for a provider-scoped proof.
	if serviceID == "" || !p.HasService(serviceID) {
		return nil, httpx.NotFound("DNS service not found on this project")
	}
	es, err := h.esSvc.Get(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	if es == nil || !es.IsPowerDNS() || es.IsDisabled() || !es.ServiceEnabledInRegion("dns", es.CephRegion()) {
		return nil, httpx.NotFound("DNS service not available on this project")
	}
	return es, nil
}

func (h *Handler) dnsPrincipal(w http.ResponseWriter, r *http.Request) (*user.User, *Project, bool) {
	u, ok := h.principal(w, r)
	if !ok {
		return nil, nil, false
	}
	p, ok := h.project(w, r, u, rbac.ProjectCloudResourceManage)
	if !ok {
		return nil, nil, false
	}
	if h.dnsOwnership == nil {
		h.fail(w, httpx.NewError(503, 503, "DNS ownership verification is unavailable. Contact your operator."))
		return nil, nil, false
	}
	return u, p, true
}

func (h *Handler) dnsLoad(ctx context.Context, p *Project, serviceID, id string) (*dnsOwnership, error) {
	var d dnsOwnership
	found, err := h.dnsOwnership.Get(ctx, id, &d)
	if err != nil {
		return nil, err
	}
	if !found || d.ProjectID != p.ID || d.ServiceID != serviceID {
		return nil, httpx.NotFound("DNS ownership challenge not found")
	}
	return &d, nil
}

// Each project has its own quota/backoff lock: a tenant can delay only its own
// verification, not every domain or another tenant's creates while DNS times out.
func (h *Handler) dnsProjectLock(ctx context.Context, projectID string, fn func() error) error {
	acquired, err := h.dnsOwnership.DB().WithAdvisoryLock(ctx, "dns-ownership:project:"+projectID, true, fn)
	if err == nil && !acquired {
		return httpx.NewError(429, 429, "Another DNS ownership request is running for this project. Wait and retry.")
	}
	return err
}

func (h *Handler) dnsOwnershipChallenge(w http.ResponseWriter, r *http.Request) {
	u, p, ok := h.dnsPrincipal(w, r)
	if !ok {
		return
	}
	if err := refuseNewUsage(p); err != nil {
		h.fail(w, err)
		return
	}
	var req struct {
		ServiceID string `json:"serviceId"`
		Name      string `json:"name"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		h.fail(w, httpx.BadRequest("Send serviceId and domain name only"))
		return
	}
	es, err := h.dnsProvider(r.Context(), p, req.ServiceID)
	if err != nil {
		h.fail(w, err)
		return
	}
	name, err := powerdns.CanonicalZone(req.Name)
	if err != nil {
		h.fail(w, zoneErr(err))
		return
	}
	if len(dnsProofPrefix+name) > 254 {
		h.fail(w, httpx.BadRequest("Domain is too long for the ownership TXT record"))
		return
	}
	_, _, binding, err := dnsConfig(es)
	if err != nil {
		h.fail(w, upstreamErr("DNS verification configuration", err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	// A challenge is cheap and nonexclusive: no upstream lookup or global
	// namespace lock on this path. Availability is checked before verification
	// and again before creation; pending rows cannot reserve a domain.
	var token string
	d := &dnsOwnership{ID: pgdoc.NewID(), ProjectID: p.ID, ServiceID: es.ID, Name: name, Binding: binding, State: "PENDING"}
	err = h.dnsProjectLock(ctx, p.ID, func() error {
		now := time.Now().UTC()
		// Keep bounded quota history for 24h; pending expiry frees capacity, but
		// cancel/consume does not reset the hourly creation budget.
		if _, err := h.dnsOwnership.DeleteMany(ctx, pgdoc.M{"projectId": p.ID, "createdAt": pgdoc.M{"$lt": now.Add(-24 * time.Hour)}}); err != nil {
			return err
		}
		var rows []dnsOwnership
		if err := h.dnsOwnership.Find(ctx, pgdoc.M{"projectId": p.ID}, &rows); err != nil {
			return err
		}
		live, recent := 0, 0
		for _, row := range rows {
			if row.CreatedAt.After(now.Add(-time.Hour)) {
				recent++
			}
			if state := row.state(now); state == "PENDING" || state == "VERIFIED" {
				live++
			}
		}
		if live >= 10 || recent >= 20 {
			return httpx.NewError(429, 429, "DNS challenge limit reached. Cancel unused challenges or wait before starting another.")
		}
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		token = "proxcloud-domain-verification=" + base64.RawURLEncoding.EncodeToString(random[:])
		d.TokenHash = proofHash(p.ID, es.ID, name, token)
		d.CreatedAt, d.ExpiresAt = now, now.Add(dnsChallengeTTL)
		_, err := h.dnsOwnership.InsertOne(ctx, d)
		return err
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	h.dnsAudit(u, p, d, "CHALLENGE", audit.OutcomeSuccess)
	view := d.view(time.Now().UTC())
	view["recordValue"] = token
	w.Header().Set("Cache-Control", "no-store")
	httpx.OK(w, view)
}

func (h *Handler) dnsOwnershipStatus(w http.ResponseWriter, r *http.Request) {
	_, p, ok := h.dnsPrincipal(w, r)
	if !ok {
		return
	}
	serviceID := r.URL.Query().Get("serviceId")
	if _, err := h.dnsProvider(r.Context(), p, serviceID); err != nil {
		h.fail(w, err)
		return
	}
	d, err := h.dnsLoad(r.Context(), p, serviceID, chi.URLParam(r, "ownershipId"))
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.OK(w, d.view(time.Now().UTC()))
}

func (h *Handler) dnsOwnershipVerify(w http.ResponseWriter, r *http.Request) {
	u, p, ok := h.dnsPrincipal(w, r)
	if !ok {
		return
	}
	if err := refuseNewUsage(p); err != nil {
		h.fail(w, err)
		return
	}
	var req struct {
		ServiceID string `json:"serviceId"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		h.fail(w, httpx.BadRequest("Send serviceId only; DNS servers and tokens are not accepted"))
		return
	}
	es, err := h.dnsProvider(r.Context(), p, req.ServiceID)
	if err != nil {
		h.fail(w, err)
		return
	}
	ds, resolvers, binding, err := dnsConfig(es)
	if err != nil {
		h.fail(w, upstreamErr("DNS verification configuration", err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var d *dnsOwnership
	err = h.dnsProjectLock(ctx, p.ID, func() error {
		// Finalize/create do not share the project session lock. Re-read and
		// persist backoff under a row lock, never overwrite a stale PENDING row
		// over a concurrently VERIFIED/CONSUMED/CANCELLED one.
		if err := h.dnsUpdate(ctx, p, es.ID, chi.URLParam(r, "ownershipId"), func(tc context.Context, fresh *dnsOwnership) error {
			d = fresh
			now := time.Now().UTC()
			if d.Binding != binding {
				return dnsConflict("DNS provider settings changed. Start a new ownership challenge.")
			}
			if d.state(now) != "PENDING" {
				return dnsConflict("Only a pending, unexpired challenge can be verified. Check status or start a new challenge.")
			}
			if d.NextVerify != nil && now.Before(*d.NextVerify) {
				return httpx.NewError(429, 429, "Wait 10 seconds between DNS verification attempts.")
			}
			next := now.Add(dnsVerifyDelay)
			d.NextVerify = &next
			return nil
		}); err != nil {
			return err
		}
		if err := ds.CheckAvailable(ctx, p.ID, d.Name); err != nil {
			return zoneErr(err)
		}
		txt, err := powerdns.LookupProofTXT(ctx, resolvers, dnsProofPrefix+d.Name)
		if err != nil {
			return httpx.NewError(502, 502, "Ownership DNS lookup is unavailable. No zone was created. Wait and retry.")
		}
		matched := false
		for _, v := range txt {
			if subtle.ConstantTimeCompare([]byte(proofHash(p.ID, es.ID, d.Name, v)), []byte(d.TokenHash)) == 1 {
				matched = true
			}
		}
		if !matched {
			return dnsConflict("The exact TXT value is not visible yet. Publish it at your current DNS host, wait for DNS caches, and retry. CNAME records are not accepted.")
		}
		return nil
	})
	// Verification reserves nothing and never needs the global CREATE lock.
	// Release the project session lock, recheck live availability, then finalize
	// atomically against cancel/create. The create rechecks overlap independently.
	if err == nil {
		err = func() error {
			freshProject, err := h.svc.GetProject(ctx, u.Sub, p.ID)
			if err != nil {
				return err
			}
			if err := h.policy.RequireProjectPermission(ctx, u.Sub, freshProject, rbac.ProjectCloudResourceManage); err != nil {
				return err
			}
			if err := refuseNewUsage(freshProject); err != nil {
				return err
			}
			current, err := h.dnsProvider(ctx, freshProject, es.ID)
			if err != nil {
				return err
			}
			ds, _, currentBinding, err := dnsConfig(current)
			if err != nil {
				return upstreamErr("DNS verification configuration", err)
			}
			if currentBinding != binding {
				return dnsConflict("DNS provider settings changed. Start a new challenge.")
			}
			if err := ds.CheckAvailable(ctx, p.ID, d.Name); err != nil {
				return zoneErr(err)
			}
			return h.dnsUpdate(ctx, p, es.ID, d.ID, func(tc context.Context, fresh *dnsOwnership) error {
				if err := h.dnsReauthorize(tc, u, p.ID, es.ID, binding); err != nil {
					return err
				}
				now := time.Now().UTC()
				if fresh.state(now) != "PENDING" || fresh.TokenHash != d.TokenHash || fresh.Binding != binding {
					return dnsConflict("Challenge expired or changed. Check status or start a new challenge.")
				}
				until := now.Add(dnsVerifiedTTL)
				fresh.State, fresh.VerifiedAt, fresh.VerifiedTo = "VERIFIED", &now, &until
				d = fresh
				return nil
			})
		}()
	}
	if d != nil {
		outcome := audit.OutcomeSuccess
		if err != nil {
			outcome = audit.OutcomeFailure
		}
		h.dnsAudit(u, p, d, "VERIFY", outcome)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.OK(w, d.view(time.Now().UTC()))
}

func (h *Handler) dnsOwnershipCancel(w http.ResponseWriter, r *http.Request) {
	u, p, ok := h.dnsPrincipal(w, r)
	if !ok {
		return
	}
	serviceID := r.URL.Query().Get("serviceId")
	if _, err := h.dnsProvider(r.Context(), p, serviceID); err != nil {
		h.fail(w, err)
		return
	}
	var d *dnsOwnership
	err := h.dnsProjectLock(r.Context(), p.ID, func() error {
		var err error
		d, err = h.dnsLoad(r.Context(), p, serviceID, chi.URLParam(r, "ownershipId"))
		if err != nil {
			return err
		}
		return h.dnsUpdate(r.Context(), p, serviceID, d.ID, func(tc context.Context, fresh *dnsOwnership) error {
			if fresh.State != "CONSUMED" {
				fresh.State, fresh.TokenHash = "CANCELLED", ""
			}
			d = fresh
			return nil
		})
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	h.dnsAudit(u, p, d, "CANCEL", audit.OutcomeSuccess)
	httpx.OK(w, d.view(time.Now().UTC()))
}

func (h *Handler) dnsAudit(u *user.User, p *Project, d *dnsOwnership, verb, outcome string) {
	if h.audit == nil {
		return
	}
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext, ev.Action, ev.ResourceType = audit.ContextProject, "DNS_OWNERSHIP_"+verb, "DNS_OWNERSHIP"
	ev.OrganizationID, ev.ProjectID, ev.ResourceID, ev.ResourceDisplayName = p.OrganizationID, p.ID, d.ID, d.Name
	ev.ResourceMetadata = map[string]any{"serviceId": d.ServiceID, "name": d.Name, "state": d.state(time.Now().UTC())}
	ev.Outcome = outcome
	h.audit.LogAsync(ev)
}

// consumeDNSProof persists CONSUMED before the upstream request (not in a SQL
// transaction that could roll back on HTTP failure). A timeout is an uncertain
// create, not permission to replay the proof. The caller holds the global create
// lock through live preflight, consume, upstream create and cache insert.
func (h *Handler) consumeDNSProof(ctx context.Context, u *user.User, p *Project, es *externalservice.ExternalService, name, id, binding string) error {
	return h.dnsUpdate(ctx, p, es.ID, id, func(tc context.Context, d *dnsOwnership) error {
		if err := h.dnsReauthorize(tc, u, p.ID, es.ID, binding); err != nil {
			return err
		}
		if d.Name != name || d.Binding != binding || d.state(time.Now().UTC()) != "VERIFIED" {
			return dnsConflict("A fresh verified ownership challenge for this project, DNS provider and exact domain is required. Start or verify a challenge, then retry.")
		}
		d.State, d.TokenHash = "CONSUMED", ""
		return nil
	})
}

// dnsReauthorize is DB-only and runs after ALL external preflight I/O inside
// the final state transition. Administrative changes after committed CONSUMED
// are beyond the point of no return of an already authorized external request.
func (h *Handler) dnsReauthorize(ctx context.Context, u *user.User, projectID, serviceID, binding string) error {
	p, err := h.svc.GetProject(ctx, u.Sub, projectID)
	if err != nil {
		return err
	}
	if err := h.policy.RequireProjectPermission(ctx, u.Sub, p, rbac.ProjectCloudResourceManage); err != nil {
		return err
	}
	if err := refuseNewUsage(p); err != nil {
		return err
	}
	es, err := h.dnsProvider(ctx, p, serviceID)
	if err != nil {
		return err
	}
	_, _, current, err := dnsConfig(es)
	if err != nil {
		return upstreamErr("DNS verification configuration", err)
	}
	if current != binding {
		return dnsConflict("DNS provider settings changed. Start a new ownership challenge.")
	}
	return nil
}

// Short transactions protect each state transition. No transaction holds a pool
// connection while DNS/PowerDNS runs, and returning an upstream failure cannot
// undo an already committed proof consumption.
func (h *Handler) dnsUpdate(ctx context.Context, p *Project, serviceID, id string, fn func(context.Context, *dnsOwnership) error) error {
	return h.dnsOwnership.DB().WithTx(ctx, func(tc context.Context) error {
		var d dnsOwnership
		found, err := h.dnsOwnership.GetForUpdate(tc, id, &d)
		if err != nil {
			return err
		}
		if !found || d.ProjectID != p.ID || d.ServiceID != serviceID {
			return httpx.NotFound("DNS ownership challenge not found")
		}
		if err := fn(tc, &d); err != nil {
			return err
		}
		_, err = h.dnsOwnership.Replace(tc, id, &d)
		return err
	})
}

// Proof-looking TXT writes stay auditable by name/type, without copying the
// published token into the general audit stream (including legacy SSO proofs).
func proofRecord(name, typ string) bool {
	n := strings.ToLower(name)
	return strings.EqualFold(typ, "TXT") && (strings.Contains(n, "_proxcloud") || strings.Contains(n, "_stratos"))
}
