//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/sso"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// A new Service and Repo have no process-local knowledge of a previous deletion.
func restartedSSOService(t *testing.T, e *ssoEnv, kcURL string) *sso.Service {
	t.Helper()
	kc, err := sso.NewKCAdmin(ssoIssuer, kcURL+"/realms/customers", "proxcloud-idp-admin", "s")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return sso.NewService(sso.NewRepo(e.db), kc, textcrypt.New("test-key"), e.orgs, org.NewPolicy(e.orgs), e.users,
		audit.NewService(audit.NewRepo(e.db), log), e.db.C("client_api_keys"), nil, log,
		sso.Options{MainIssuer: ssoIssuer, OperatorClients: []string{"proxcloud-admin"}, AllowInternal: true})
}

func requireSSORetiredRow(t *testing.T, e *ssoEnv, alias string) {
	t.Helper()
	ctx := context.Background()
	var row pgdoc.M
	found, err := e.db.C("org_sso_retired_alias").Get(ctx, alias, &row)
	if err != nil || !found || row["id"] != alias {
		t.Fatalf("retirement row for %q: found=%v row=%v err=%v", alias, found, row, err)
	}
	delete(row, "id")
	delete(row, "_id")
	if len(row) != 0 {
		t.Fatalf("retirement should store only the alias as the unique id, not provider payload: %v", row)
	}
	if n, err := e.db.C("org_sso_retired_alias").Count(ctx, pgdoc.M{}); err != nil || n != 1 {
		t.Fatalf("retirement row count=%d, err=%v", n, err)
	}
}

func requireSSOEmptyProviderPositives(t *testing.T, svc *sso.Service) {
	t.Helper()
	ctx := context.Background()
	for _, alias := range []string{"", "github", "operator-directory", "org-fedcba987654"} {
		rc := brokered("unrelated", alias, "person@plain.test")
		if got := svc.Guard(ctx, rc); got != "" {
			t.Errorf("zero org providers, native local/social alias %q refused: %s", alias, got)
		}
	}
}

func TestSSORetiredAliasSurvivesLastProviderDeletionAndRestart(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	id := e.org(t, "Acme", "owner")
	e.verified(t, id, "acme.test")
	p := e.configure(t, id, nil)
	if _, err := e.users.FromClaims(ctx, user.Claims{Sub: "bound", Email: "bound@acme.test", EmailVerified: true, IdP: p.Alias}); err != nil {
		t.Fatal(err)
	}
	rc := brokered("bound", p.Alias, "bound@acme.test")
	if got := e.svc.Guard(ctx, rc); got != "" {
		t.Fatalf("active provider refused: %s", got)
	}
	staleReplica := restartedSSOService(t, e, e.kc.URL)
	if got := staleReplica.Guard(ctx, rc); got != "" {
		t.Fatalf("initial replica snapshot: %s", got)
	}
	if err := e.svc.Delete(ctx, &user.User{Sub: "actor"}, id); err != nil {
		t.Fatal(err)
	}
	requireSSORetiredRow(t, e, p.Alias)
	if providers, err := e.repo.AllProviders(ctx); err != nil || len(providers) != 0 {
		t.Fatalf("last provider was not removed: %v, %v", providers, err)
	}
	if u, err := e.users.FindBySub(ctx, "bound"); err != nil || u == nil || u.SSOAlias != "" {
		t.Fatalf("binding was not released: %+v, %v", u, err)
	}
	if owner, err := e.repo.VerifiedOwner(ctx, "acme.test"); err != nil || owner != id {
		t.Fatalf("verified domain was lost: %q, %v", owner, err)
	}
	fresh := restartedSSOService(t, e, e.kc.URL)
	for _, svc := range []*sso.Service{e.svc, fresh} {
		if got := svc.Guard(ctx, rc); got != sso.RefusedIdPDisabled {
			t.Fatalf("retired alias allowed with zero providers: %q", got)
		}
		requireSSOEmptyProviderPositives(t, svc)
	}

	// Other replicas retain the existing lazy snapshot contract, but JIT must not grant membership
	// using an old active provider after the durable fence is present on the primary.
	if err := staleReplica.Provision(ctx, brokered("new-person", p.Alias, "new@acme.test"), &user.User{Sub: "new-person", Email: "new@acme.test"}); err != nil {
		t.Fatal(err)
	}
	if member, err := e.orgs.FindMember(ctx, id, "new-person"); err != nil || member != nil {
		t.Fatalf("retired provider granted membership through old snapshot: %+v, %v", member, err)
	}

	draft, err := e.svc.Draft(ctx, id)
	if err != nil || draft.Alias == p.Alias {
		t.Fatalf("new draft reused retired alias: %+v, %v", draft, err)
	}
	replacement := e.configure(t, id, nil)
	if got := e.svc.Guard(ctx, rc); got != sso.RefusedIdPDisabled {
		t.Fatalf("retired alias allowed with another provider: %q", got)
	}
	if got := e.svc.Guard(ctx, brokered("bound", replacement.Alias, "bound@acme.test")); got != "" {
		t.Fatalf("replacement provider refused after binding release: %s", got)
	}
}

func TestSSORetirementPersistsBeforeExternalDeleteAndSurvivesCleanupFailures(t *testing.T) {
	for _, phase := range []string{"keycloak", "user binding", "provider document"} {
		t.Run(phase, func(t *testing.T) {
			e := newSSOEnv(t)
			ctx := context.Background()
			id := e.org(t, "Acme", "owner")
			e.verified(t, id, "acme.test")
			p := e.configure(t, id, func(r *sso.PutRequest) { r.EnforceSSO, r.BreakGlassSubs = true, []string{"owner"} })
			if _, err := e.users.FromClaims(ctx, user.Claims{Sub: "bound", Email: "bound@acme.test", EmailVerified: true, IdP: p.Alias}); err != nil {
				t.Fatal(err)
			}
			rc := brokered("bound", p.Alias, "bound@acme.test")
			var failKC, fenceObserved atomic.Bool
			var adminCalls atomic.Int64
			failKC.Store(phase == "keycloak")
			bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/identity-provider/instances") {
					adminCalls.Add(1)
				}
				if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/instances/"+p.Alias) {
					var row struct{}
					found, err := e.db.C("org_sso_retired_alias").Get(context.Background(), p.Alias, &row)
					if err != nil || !found || e.svc.Guard(context.Background(), rc) != sso.RefusedIdPDisabled {
						t.Errorf("external delete preceded durable retirement/local invalidation: found=%v err=%v", found, err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					fenceObserved.Store(true)
					if failKC.Load() {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
				}
				e.kc.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(bridge.Close)
			e.svc = restartedSSOService(t, e, bridge.URL)
			if got := e.svc.Guard(ctx, rc); got != "" {
				t.Fatalf("pre-delete provider: %s", got)
			}
			var triggerTable string
			switch phase {
			case "user binding":
				triggerTable = "users"
			case "provider document":
				triggerTable = "org_identity_provider"
			}
			if triggerTable != "" {
				if _, err := e.db.Pool.Exec(ctx, `CREATE FUNCTION fail_sso_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected SSO cleanup failure'; END; $$`); err != nil {
					t.Fatal(err)
				}
				op := "UPDATE"
				if phase == "provider document" {
					op = "DELETE"
				}
				if _, err := e.db.Pool.Exec(ctx, `CREATE TRIGGER fail_sso_cleanup BEFORE `+op+` ON `+triggerTable+` FOR EACH ROW EXECUTE FUNCTION fail_sso_cleanup()`); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.svc.Delete(ctx, &user.User{Sub: "actor"}, id); err == nil {
				t.Fatal("injected deletion failure was ignored")
			}
			if !fenceObserved.Load() {
				t.Fatal("Keycloak did not observe the already-committed fence")
			}
			requireSSORetiredRow(t, e, p.Alias)
			for _, svc := range []*sso.Service{e.svc, restartedSSOService(t, e, bridge.URL)} {
				if got := svc.Guard(ctx, rc); got != sso.RefusedIdPDisabled {
					t.Errorf("partial deletion re-enabled session: %q", got)
				}
				if got := svc.Guard(ctx, local("owner", "owner@acme.test")); got != "" {
					t.Errorf("retired provider still enforces: %q", got)
				}
				if d := svc.Discover(ctx, "person@acme.test"); d.Found {
					t.Errorf("retired provider still discovered: %+v", d)
				}
			}
			if stored, err := e.repo.ProviderForOrg(ctx, id); err != nil || stored == nil {
				t.Fatalf("failure should leave provider for retry: %+v, %v", stored, err)
			}
			if _, err := e.svc.Draft(ctx, id); err == nil {
				t.Error("deletion-in-progress alias offered for reconfiguration")
			}
			if err := e.svc.OperatorSetDisabled(ctx, "operator", id, false); err == nil {
				t.Error("operator re-enabled deletion-in-progress alias")
			}
			before := adminCalls.Load()
			if err := e.svc.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if adminCalls.Load() != before {
				t.Error("reconcile touched a retired broker")
			}

			failKC.Store(false)
			if triggerTable != "" {
				if _, err := e.db.Pool.Exec(ctx, `DROP TRIGGER fail_sso_cleanup ON `+triggerTable); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.svc.Delete(ctx, &user.User{Sub: "actor"}, id); err != nil {
				t.Fatal("retry cleanup:", err)
			}
			requireSSORetiredRow(t, e, p.Alias) // retry did not duplicate the alias or remove its fence
			if stored, err := e.repo.ProviderForOrg(ctx, id); err != nil || stored != nil {
				t.Fatalf("retry left provider: %+v, %v", stored, err)
			}
			if u, err := e.users.FindBySub(ctx, "bound"); err != nil || u == nil || u.SSOAlias != "" {
				t.Fatalf("retry left binding: %+v, %v", u, err)
			}
			if got := e.svc.Guard(ctx, rc); got != sso.RefusedIdPDisabled {
				t.Fatalf("successful retry resurrected alias: %q", got)
			}
			requireSSOEmptyProviderPositives(t, e.svc)
		})
	}
}

func TestSSORetirementReadFailureIsNotAnEmptyRevocationSet(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	id := e.org(t, "Acme", "owner")
	p := e.configure(t, id, nil)
	if err := e.svc.Delete(ctx, &user.User{Sub: "actor"}, id); err != nil {
		t.Fatal(err)
	}
	rc := brokered("person", p.Alias, "person@acme.test")
	if got := e.svc.Guard(ctx, rc); got != sso.RefusedIdPDisabled {
		t.Fatalf("warm retired snapshot: %q", got)
	}
	// A schema error makes just the retirement read fail without relying on timing, a replica, or
	// pgdoc's missing-table auto-creation. This is limited to this test's fresh PostgreSQL database.
	if _, err := e.db.Pool.Exec(ctx, `ALTER TABLE org_sso_retired_alias RENAME COLUMN doc TO unavailable_doc`); err != nil {
		t.Fatal(err)
	}
	fresh := restartedSSOService(t, e, e.kc.URL)
	if got := fresh.Guard(ctx, rc); got != sso.RefusedUnavailable {
		t.Fatalf("fresh snapshot accepted unavailable retirement data: %q", got)
	}
	if got := fresh.Guard(ctx, local("person", "person@plain.test")); got != sso.RefusedUnavailable {
		t.Fatalf("empty-provider shortcut hid retirement read failure: %q", got)
	}
	if got := e.svc.Guard(ctx, rc); got != sso.RefusedIdPDisabled {
		t.Fatalf("known retirement disappeared from cached rules: %q", got)
	}
	if _, err := e.db.Pool.Exec(ctx, `ALTER TABLE org_sso_retired_alias RENAME COLUMN unavailable_doc TO doc`); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Guard(ctx, rc); got != sso.RefusedIdPDisabled {
		t.Fatalf("restored primary retirement data not applied: %q", got)
	}
}

func TestSSODeleteRejectsUncommittedRetirementTransaction(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	id := e.org(t, "Acme", "owner")
	p := e.configure(t, id, nil)
	if err := e.db.WithTx(ctx, func(txctx context.Context) error {
		if err := e.svc.Delete(txctx, &user.User{Sub: "actor"}, id); !errors.Is(err, pgdoc.ErrInheritedTransaction) {
			t.Fatalf("Delete accepted inherited transaction: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.db.C("org_sso_retired_alias").Count(ctx, pgdoc.M{}); err != nil || n != 0 {
		t.Fatalf("rejected transaction wrote a retirement: %d, %v", n, err)
	}
	e.kc.mu.Lock()
	_, exists := e.kc.instances[p.Alias]
	e.kc.mu.Unlock()
	if !exists {
		t.Fatal("external broker was removed before a committed retirement")
	}
}
