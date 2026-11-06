//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/sso"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

const ssoIssuer = "https://id.test/realms/customers"

// fakeKeycloak answers the small part of the Admin API the service uses and remembers instances.
type fakeKeycloak struct {
	*httptest.Server
	mu        sync.Mutex
	instances map[string]map[string]any
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	f := &fakeKeycloak{instances: map[string]map[string]any{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		const prefix = "/admin/realms/customers/identity-provider/instances"
		switch {
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":300}`))
		case strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration"):
			_, _ = w.Write([]byte(`{"issuer":"https://customer.test","authorization_endpoint":"https://customer.test/a","token_endpoint":"https://customer.test/t"}`))
		case strings.HasSuffix(r.URL.Path, "/identity-provider/import-config"):
			_, _ = w.Write([]byte(`{"issuer":"https://customer.test","tokenUrl":"https://customer.test/t"}`))
		case strings.HasSuffix(r.URL.Path, "/mappers"):
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`[]`))
			}
		case r.URL.Path == prefix && r.Method == http.MethodPost:
			var rep map[string]any
			_ = json.NewDecoder(r.Body).Decode(&rep)
			f.instances[rep["alias"].(string)] = rep
		case strings.HasPrefix(r.URL.Path, prefix+"/"):
			alias := strings.TrimPrefix(r.URL.Path, prefix+"/")
			switch r.Method {
			case http.MethodGet:
				if rep, ok := f.instances[alias]; ok {
					_ = json.NewEncoder(w).Encode(rep)
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			case http.MethodPut:
				var rep map[string]any
				_ = json.NewDecoder(r.Body).Decode(&rep)
				f.instances[alias] = rep
			case http.MethodDelete:
				delete(f.instances, alias)
			}
		case strings.Contains(r.URL.Path, "/users"):
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

type ssoEnv struct {
	db    *pgdoc.DB
	svc   *sso.Service
	orgs  *org.Repo
	users *user.Repo
	repo  *sso.Repo
	kc    *fakeKeycloak
	txt   map[string][]string
	mu    sync.Mutex
}

func newSSOEnv(t *testing.T) *ssoEnv {
	t.Helper()
	ctx := context.Background()
	db := freshPG(t)
	e := &ssoEnv{db: db, orgs: org.NewRepo(db), users: user.NewRepo(db), repo: sso.NewRepo(db), kc: newFakeKeycloak(t), txt: map[string][]string{}}
	for _, ensure := range []func(context.Context) error{e.orgs.EnsureIndexes, e.users.EnsureIndexes, e.repo.EnsureIndexes} {
		if err := ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.C("client_api_keys").Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	kc, err := sso.NewKCAdmin(ssoIssuer, e.kc.URL+"/realms/customers", "proxcloud-idp-admin", "s")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.svc = sso.NewService(e.repo, kc, textcrypt.New("test-key"), e.orgs, org.NewPolicy(e.orgs), e.users,
		audit.NewService(audit.NewRepo(db), log), db.C("client_api_keys"), nil, log, sso.Options{
			MainIssuer:      ssoIssuer,
			OperatorClients: []string{"proxcloud-admin"},
			ReservedDomains: []string{"ops.test"},
			AllowInternal:   true,
			LookupTXT: func(_ context.Context, name string) ([]string, error) {
				e.mu.Lock()
				defer e.mu.Unlock()
				return e.txt[name], nil
			},
		})
	return e
}

func (e *ssoEnv) setTXT(domain, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.txt["_proxcloud-challenge."+domain] = append(e.txt["_proxcloud-challenge."+domain], value)
}

func (e *ssoEnv) org(t *testing.T, name, ownerSub string) string {
	t.Helper()
	o, err := e.orgs.Insert(context.Background(), &org.Organization{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.orgs.AddMember(context.Background(), o.ID, ownerSub, rbac.RoleOwner); err != nil {
		t.Fatal(err)
	}
	return o.ID
}

// verified claims and verifies a domain for an org.
func (e *ssoEnv) verified(t *testing.T, orgID, domain string) {
	t.Helper()
	ctx := context.Background()
	d, err := e.svc.ClaimDomain(ctx, &user.User{Sub: "actor"}, orgID, domain)
	if err != nil {
		t.Fatal(err)
	}
	e.setTXT(domain, d.Token)
	if _, err := e.svc.VerifyDomain(ctx, &user.User{Sub: "actor"}, orgID, domain); err != nil {
		t.Fatal(err)
	}
}

func (e *ssoEnv) configure(t *testing.T, orgID string, mutate func(*sso.PutRequest)) *sso.Provider {
	t.Helper()
	req := sso.PutRequest{Protocol: "oidc", DiscoveryURL: e.kc.URL + "/realms/x/.well-known/openid-configuration", ClientID: "c", ClientSecret: "s3cret",
		ProviderName: "IdP", AutoProvision: true, TrustEmail: true, Enabled: true, DefaultRole: rbac.RoleMember}
	if mutate != nil {
		mutate(&req)
	}
	p, err := e.svc.Put(context.Background(), &user.User{Sub: "actor"}, orgID, req)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func brokered(sub, alias, email string, groups ...string) *httpx.RequestContext {
	return &httpx.RequestContext{Sub: sub, Email: email, EmailVerified: true, Issuer: ssoIssuer, Azp: "proxcloud-ui", IdP: alias, IdPGroups: groups, SSOScope: true}
}

func local(sub, email string) *httpx.RequestContext {
	return &httpx.RequestContext{Sub: sub, Email: email, EmailVerified: true, Issuer: ssoIssuer, Azp: "proxcloud-ui", SSOScope: true}
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	he, ok := err.(*httpx.HTTPError)
	if !ok || he.Status != status {
		t.Fatalf("want status %d, got %v", status, err)
	}
}

func TestSSODomainClaims(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	actor := &user.User{Sub: "actor"}
	a, b := e.org(t, "A", "owner-a"), e.org(t, "B", "owner-b")

	if _, err := e.svc.ClaimDomain(ctx, actor, a, "ops.test"); err == nil {
		t.Fatal("reserved domain accepted")
	}
	if _, err := e.svc.ClaimDomain(ctx, actor, a, "x.ops.test"); err == nil {
		t.Fatal("subdomain of a reserved domain accepted")
	}
	da, err := e.svc.ClaimDomain(ctx, actor, a, "shared.test")
	if err != nil {
		t.Fatal(err)
	}
	db, err := e.svc.ClaimDomain(ctx, actor, b, "shared.test")
	if err != nil {
		t.Fatal("a pending claim must not block another org:", err)
	}
	if da.Token == db.Token {
		t.Fatal("tokens must differ per claim")
	}
	if _, err := e.svc.VerifyDomain(ctx, actor, a, "shared.test"); err == nil {
		t.Fatal("verified without a record")
	}
	e.setTXT("shared.test", db.Token)
	if _, err := e.svc.VerifyDomain(ctx, actor, a, "shared.test"); err == nil {
		t.Fatal("verified with another org's token")
	}
	e.setTXT("shared.test", da.Token)

	// Both orgs have a valid record: the unique index lets exactly one win, even concurrently.
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, id := range []string{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = e.svc.VerifyDomain(ctx, actor, id, "shared.test")
		}()
	}
	wg.Wait()
	if (results[0] == nil) == (results[1] == nil) {
		t.Fatalf("exactly one org must win: %v / %v", results[0], results[1])
	}
	loser := a
	if results[0] == nil {
		loser = b
	}
	if _, err := e.svc.ClaimDomain(ctx, actor, loser, "shared.test"); err == nil {
		// the loser already holds a pending row; the verified owner still refuses it
		t.Fatal("verified domain must refuse another org")
	} else {
		wantStatus(t, err, http.StatusConflict)
		if strings.Contains(err.Error(), "A") && strings.Contains(err.Error(), a) {
			t.Fatal("error names the other org")
		}
	}
}

func TestSSOConfigGuardsRoles(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	id := e.org(t, "A", "owner")
	if _, err := e.orgs.InsertRole(ctx, &org.Role{OrganizationID: id, Name: "WILD", Permissions: []string{"organization:*"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.orgs.InsertRole(ctx, &org.Role{OrganizationID: id, Name: "OK", Permissions: []string{"organization:read"}}); err != nil {
		t.Fatal(err)
	}
	base := sso.PutRequest{Protocol: "oidc", DiscoveryURL: e.kc.URL + "/x/.well-known/openid-configuration", ClientID: "c", ClientSecret: "s", Enabled: true}
	for name, mutate := range map[string]func(*sso.PutRequest){
		"owner default":  func(r *sso.PutRequest) { r.DefaultRole = rbac.RoleOwner },
		"admin default":  func(r *sso.PutRequest) { r.DefaultRole = rbac.RoleAdmin },
		"wildcard group": func(r *sso.PutRequest) { r.GroupRoleMap = map[string]string{"g": "WILD"} },
		"admin group":    func(r *sso.PutRequest) { r.GroupRoleMap = map[string]string{"g": rbac.RoleAdmin} },
		"unknown role":   func(r *sso.PutRequest) { r.DefaultRole = "NOPE" },
		"too many glass": func(r *sso.PutRequest) { r.BreakGlassSubs = []string{"a", "b", "c"} },
		"glass non owner": func(r *sso.PutRequest) {
			r.BreakGlassSubs = []string{"stranger"}
		},
		"enforce no domain": func(r *sso.PutRequest) { r.EnforceSSO, r.BreakGlassSubs = true, []string{"owner"} },
	} {
		req := base
		mutate(&req)
		if _, err := e.svc.Put(ctx, &user.User{Sub: "actor"}, id, req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	e.verified(t, id, "a.test")
	if _, err := e.svc.Put(ctx, &user.User{Sub: "actor"}, id, sso.PutRequest{Protocol: "oidc", DiscoveryURL: base.DiscoveryURL, ClientID: "c", ClientSecret: "s", Enabled: true, EnforceSSO: true}); err == nil {
		t.Error("enforce without break-glass accepted")
	}
	p := e.configure(t, id, func(r *sso.PutRequest) { r.GroupRoleMap = map[string]string{"g": "OK"} })
	if strings.Contains(p.ClientSecretEnc, "s3cret") || p.ClientSecretEnc == "" {
		t.Fatalf("secret not encrypted: %q", p.ClientSecretEnc)
	}
	var stored struct {
		ClientSecretEnc string `json:"clientSecretEnc"`
	}
	if _, err := e.db.C("org_identity_provider").FindOne(ctx, pgdoc.M{"alias": p.Alias}, &stored); err != nil || strings.Contains(stored.ClientSecretEnc, "s3cret") {
		t.Fatalf("stored secret: %q %v", stored.ClientSecretEnc, err)
	}
	if e.kc.instances[p.Alias]["hideOnLogin"] != true {
		t.Fatal("broker must be hidden on the login page")
	}
}

func TestSSOGuardDomainGateAndBinding(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	acme, evil := e.org(t, "Acme", "acme-owner"), e.org(t, "Evil", "evil-owner")
	e.verified(t, acme, "acme.test")
	e.verified(t, evil, "evil.test")
	pa, pe := e.configure(t, acme, nil), e.configure(t, evil, nil)

	if c := e.svc.Guard(ctx, brokered("u1", pa.Alias, "alice@acme.test")); c != "" {
		t.Fatalf("own domain refused: %s", c)
	}
	if c := e.svc.Guard(ctx, brokered("u2", pe.Alias, "ceo@acme.test")); c != sso.RefusedDomain {
		t.Fatalf("foreign domain: %q", c)
	}
	unverified := brokered("u1", pa.Alias, "alice@acme.test")
	unverified.EmailVerified = false
	if c := e.svc.Guard(ctx, unverified); c != sso.RefusedDomain {
		t.Fatalf("unverified email: %q", c)
	}
	if c := e.svc.Guard(ctx, brokered("u3", "org-unknown", "a@acme.test")); c != sso.RefusedIdPDisabled {
		t.Fatalf("unknown alias: %q", c)
	}

	// Bound to acme: the evil IdP may not reach this account, even asserting a clean evil address.
	if _, err := e.users.FromClaims(ctx, user.Claims{Sub: "bound", Email: "bob@acme.test", EmailVerified: true, IdP: pa.Alias}); err != nil {
		t.Fatal(err)
	}
	if c := e.svc.Guard(ctx, brokered("bound", pe.Alias, "bob@evil.test")); c != sso.RefusedIdPMismatch {
		t.Fatalf("bound account, other IdP: %q", c)
	}
	// Unbound local account outside the IdP's domains: the rewritten claim must not unlock it, and
	// this has to hold in the very first minute of the account's life (no cached negative).
	if c := e.svc.Guard(ctx, local("fresh", "fresh@plain.test")); c != "" {
		t.Fatalf("local user: %q", c)
	}
	if _, err := e.users.FromClaims(ctx, user.Claims{Sub: "fresh", Email: "fresh@plain.test", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	if c := e.svc.Guard(ctx, brokered("fresh", pe.Alias, "fresh@evil.test")); c != sso.RefusedIdPMismatch {
		t.Fatalf("unbound local account, foreign IdP: %q", c)
	}
	// A local account inside the org's own domain may be claimed by that org's IdP.
	if _, err := e.users.FromClaims(ctx, user.Claims{Sub: "linked", Email: "carol@acme.test", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	if c := e.svc.Guard(ctx, brokered("linked", pa.Alias, "carol@acme.test")); c != "" {
		t.Fatalf("own domain, local account: %q", c)
	}

	// Operator client: a brokered session is refused, a local one passes. Other realms are untouched.
	op := brokered("u1", pa.Alias, "alice@acme.test")
	op.Azp = "proxcloud-admin"
	if c := e.svc.Guard(ctx, op); c != sso.RefusedOperator {
		t.Fatalf("operator client: %q", c)
	}
	other := brokered("u1", pe.Alias, "x@acme.test")
	other.Issuer = "https://id.test/realms/operators"
	if c := e.svc.Guard(ctx, other); c != "" {
		t.Fatalf("other realm: %q", c)
	}
	// A token that cannot say whether it was brokered is refused.
	noScope := local("u9", "z@plain.test")
	noScope.SSOScope = false
	if c := e.svc.Guard(ctx, noScope); c != sso.RefusedScopeMissing {
		t.Fatalf("missing scope: %q", c)
	}
}

func TestSSOProvisionNeverOverwritesAndRechecksRole(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	id := e.org(t, "Acme", "owner")
	e.verified(t, id, "acme.test")
	role, err := e.orgs.InsertRole(ctx, &org.Role{OrganizationID: id, Name: "DEV", Permissions: []string{"organization:read"}})
	if err != nil {
		t.Fatal(err)
	}
	p := e.configure(t, id, func(r *sso.PutRequest) { r.GroupRoleMap = map[string]string{"devs": "DEV"} })

	alice := &user.User{Sub: "alice", Email: "alice@acme.test"}
	if err := e.svc.Provision(ctx, brokered("alice", p.Alias, alice.Email, "devs"), alice); err != nil {
		t.Fatal(err)
	}
	m, _ := e.orgs.FindMember(ctx, id, "alice")
	if m == nil || m.Role() != "DEV" {
		t.Fatalf("member: %+v", m)
	}
	if err := e.orgs.UpdateMemberRole(ctx, id, "alice", rbac.RoleMember); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Provision(ctx, brokered("alice", p.Alias, alice.Email, "devs"), alice); err != nil {
		t.Fatal(err)
	}
	if m, _ := e.orgs.FindMember(ctx, id, "alice"); m.Role() != rbac.RoleMember {
		t.Fatalf("existing membership overwritten: %s", m.Role())
	}

	// The mapped role turns into an admin role after configuration: nobody new gets it.
	role.Permissions = []string{"organization:read", "organization:manage_members"}
	if err := e.orgs.SaveRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	bob := &user.User{Sub: "bob", Email: "bob@acme.test"}
	if err := e.svc.Provision(ctx, brokered("bob", p.Alias, bob.Email, "devs"), bob); err != nil {
		t.Fatal(err)
	}
	if m, _ := e.orgs.FindMember(ctx, id, "bob"); m != nil {
		t.Fatalf("role that became admin was granted: %+v", m)
	}
	// A local login provisions nothing.
	carol := &user.User{Sub: "carol", Email: "carol@acme.test"}
	if err := e.svc.Provision(ctx, local("carol", carol.Email), carol); err != nil {
		t.Fatal(err)
	}
	if m, _ := e.orgs.FindMember(ctx, id, "carol"); m != nil {
		t.Fatal("local login was provisioned")
	}
}

func TestSSOEnforceAndBreakGlass(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	id := e.org(t, "Acme", "owner")
	if _, err := e.orgs.AddMember(ctx, id, "outsider", rbac.RoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err := e.orgs.AddMember(ctx, id, "second-owner", rbac.RoleOwner); err != nil {
		t.Fatal(err)
	}
	e.verified(t, id, "acme.test")
	if _, err := e.users.FromClaims(ctx, user.Claims{Sub: "patuser", Email: "pat@acme.test", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.orgs.AddMember(ctx, id, "patuser", rbac.RoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.C("client_api_keys").InsertOne(ctx, pgdoc.M{"_id": "pkabc", "sub": "patuser", "secretKey": "x"}); err != nil {
		t.Fatal(err)
	}
	p := e.configure(t, id, func(r *sso.PutRequest) { r.EnforceSSO, r.BreakGlassSubs = true, []string{"owner"} })

	pat := &httpx.RequestContext{Sub: "patuser", Issuer: ssoIssuer, PAT: true}
	cases := []struct {
		name string
		rc   *httpx.RequestContext
		want string
	}{
		{"through the IdP", brokered("sso-user", p.Alias, "alice@acme.test"), ""},
		{"local in the domain", local("loc", "loc@acme.test"), sso.RefusedRequired},
		{"local member with an outside address", local("outsider", "x@plain.test"), sso.RefusedRequired},
		{"unrelated local user", local("other", "x@plain.test"), ""},
		{"break-glass owner", local("owner", "x@plain.test"), ""},
		{"second owner, not break-glass", local("second-owner", "y@plain.test"), sso.RefusedRequired},
		{"API token of a member", pat, sso.RefusedRequired},
	}
	for _, c := range cases {
		if got := e.svc.Guard(ctx, c.rc); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if n, _ := e.db.C("client_api_keys").Count(ctx, pgdoc.M{"sub": "patuser"}); n != 0 {
		t.Error("turning enforcement on must revoke member API tokens")
	}

	// A demoted break-glass owner loses the bypass immediately.
	if err := e.orgs.UpdateMemberRole(ctx, id, "owner", rbac.RoleMember); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.Guard(ctx, local("owner", "x@plain.test")); got != sso.RefusedRequired {
		t.Errorf("demoted owner: %q", got)
	}

	// Operator emergency stop: logins refused, enforcement lifted, and the org cannot undo it.
	if err := e.svc.OperatorSetDisabled(ctx, "op", id, true); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.Guard(ctx, brokered("sso-user", p.Alias, "alice@acme.test")); got != sso.RefusedIdPDisabled {
		t.Errorf("disabled IdP: %q", got)
	}
	if got := e.svc.Guard(ctx, local("loc", "loc@acme.test")); got != "" {
		t.Errorf("enforcement must not outlive the IdP: %q", got)
	}
	if d := e.svc.Discover(ctx, "alice@acme.test"); d.Found {
		t.Error("disabled IdP discovered")
	}
	if e.kc.instances[p.Alias]["enabled"] != false {
		t.Error("Keycloak instance still on")
	}
	_, err := e.svc.Put(ctx, &user.User{Sub: "actor"}, id, sso.PutRequest{Protocol: "oidc", DiscoveryURL: e.kc.URL + "/x/.well-known/openid-configuration", ClientID: "c", Enabled: true})
	wantStatus(t, err, http.StatusForbidden)
}

func TestSSODiscoverReconcileDelete(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	id := e.org(t, "Acme", "owner")
	if d := e.svc.Discover(ctx, "a@acme.test"); d.Found {
		t.Fatal("found before anything exists")
	}
	if _, err := e.svc.ClaimDomain(ctx, &user.User{Sub: "actor"}, id, "acme.test"); err != nil {
		t.Fatal(err)
	}
	p := e.configure(t, id, nil)
	if d := e.svc.Discover(ctx, "a@acme.test"); d.Found {
		t.Fatal("unverified domain discovered")
	}
	e.verified(t, id, "acme.test")
	d := e.svc.Discover(ctx, "a@ACME.test")
	if !d.Found || d.Alias != p.Alias || strings.Contains(d.Alias, id) {
		t.Fatalf("discover: %+v", d)
	}

	// A broker lost from Keycloak comes back from the stored secret.
	e.kc.mu.Lock()
	delete(e.kc.instances, p.Alias)
	e.kc.mu.Unlock()
	if err := e.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, ok := e.kc.instances[p.Alias]["config"].(map[string]any)
	if !ok || cfg["clientSecret"] != "s3cret" {
		t.Fatalf("recreated with config %v", e.kc.instances[p.Alias])
	}

	// Delete removes the broker and releases the bindings.
	if _, err := e.users.FromClaims(ctx, user.Claims{Sub: "bound", Email: "b@acme.test", EmailVerified: true, IdP: p.Alias}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Delete(ctx, &user.User{Sub: "actor"}, id); err != nil {
		t.Fatal(err)
	}
	if _, found := e.kc.instances[p.Alias]; found {
		t.Fatal("broker left in Keycloak")
	}
	if u, _ := e.users.FindBySub(ctx, "bound"); u.SSOAlias != "" {
		t.Fatalf("binding kept: %s", u.SSOAlias)
	}
}
