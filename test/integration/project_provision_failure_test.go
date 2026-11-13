//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/cephcred"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// TestBadProviderDoesNotBlockTheOthers: a provider that fails leaves the others provisioned, says so on the project and
// on its own row, and gets the project on the next try once it works.
func TestBadProviderDoesNotBlockTheOthers(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	rgw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin/user") {
			_, _ = w.Write([]byte(`{"user_id":"u","keys":[{"user":"u","access_key":"a","secret_key":"s"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer rgw.Close()

	objectRow := func(id, status, endpoint string) pgdoc.M {
		return pgdoc.M{
			"_id": id, "name": id, "type": externalservice.TypeCloud, "status": status,
			"config": pgdoc.M{"provider": "ceph-s3", "s3Endpoint": endpoint, "adminApiUrl": endpoint + "/admin", "region": "r1"},
			"secret": pgdoc.M{"adminAccessKey": enc.Encrypt("AK"), "adminSecretKey": enc.Encrypt("SK")},
		}
	}
	mustInsert(t, db, "externalService", objectRow("bad", externalservice.StatusPrivate, "http://127.0.0.1:1"))
	mustInsert(t, db, "externalService", pgdoc.M{
		"_id": "dns", "name": "dns", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
		"config": pgdoc.M{"provider": "powerdns", "apiUrl": "http://127.0.0.1:1", "region": "r1"},
	})
	mustInsert(t, db, "project", pgdoc.M{"_id": "p1", "name": "p", "status": project.StatusDisabled, "services": []any{}})

	esSvc := externalservice.NewService(externalservice.NewRepo(db), enc)
	repo := project.NewRepo(db)
	h := project.NewHandler(project.NewService(repo, nil, nil, nil, nil), nil, nil, nil, nil, nil, nil, esSvc, nil, nil, "r1")
	h.SetCephCreds(cephcred.New(db, enc), cephcred.NewKeyRepo(db, enc))
	load := func() *project.Project {
		p, err := repo.FindByID(ctx, "p1")
		if err != nil || p == nil {
			t.Fatalf("read project: %v %v", p, err)
		}
		return p
	}
	services := func(p *project.Project) string { return strings.Join(p.ServiceIDs(), ",") }

	if err := h.EnableAndBootstrap(ctx, load()); err != nil {
		t.Fatalf("one good provider must be enough: %v", err)
	}
	p := load()
	if services(p) != "dns" || p.Status != project.StatusEnabled {
		t.Fatalf("after the first try: services %q, status %s", services(p), p.Status)
	}
	failed, ok := p.ProvisionFailures["bad"]
	if !ok || failed.Message == "" || failed.At.IsZero() {
		t.Fatalf("no failure on the project: %+v", p.ProvisionFailures)
	}
	row, err := esSvc.Get(ctx, "bad")
	if err != nil || row.ProvisionError == nil || row.ProvisionError.ProjectID != "p1" || row.ProvisionError.Message == "" {
		t.Fatalf("no failure on the provider: %+v %v", row, err)
	}

	// Inside the retry window nothing is tried, so a member opening a project does not wait on a dead provider.
	if _, err := db.C("externalService").SetByID(ctx, "bad", pgdoc.M{"config.s3Endpoint": rgw.URL, "config.adminApiUrl": rgw.URL + "/admin"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.EnableAndBootstrap(ctx, load()); err != nil {
		t.Fatal(err)
	}
	if services(load()) != "dns" {
		t.Fatalf("retried inside the window: %q", services(load()))
	}

	// Past it, the provider now works: attached, and both records gone. (The bootstrap works from the failures as stored,
	// so the stored one is backdated.)
	if err := repo.SetProvisionFailures(ctx, "p1", map[string]project.ProvisionFailure{"bad": {Message: "old", At: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if err := h.EnableAndBootstrap(ctx, load()); err != nil {
		t.Fatal(err)
	}
	p = load()
	if !p.HasService("bad") || len(p.ProvisionFailures) != 0 {
		t.Fatalf("after the retry: services %q, failures %+v", services(p), p.ProvisionFailures)
	}
	if row, err := esSvc.Get(ctx, "bad"); err != nil || row.ProvisionError != nil {
		t.Fatalf("the provider's record must clear: %+v %v", row, err)
	}

	// A provider added after the project's bootstrap is the operator's to attach, as before.
	mustInsert(t, db, "externalService", objectRow("later", externalservice.StatusPublic, rgw.URL))
	if err := h.EnableAndBootstrap(ctx, load()); err != nil {
		t.Fatal(err)
	}
	if load().HasService("later") {
		t.Fatal("a re-entry must not attach a new provider")
	}

	t.Run("no provider at all is an error, and the retry still happens", func(t *testing.T) {
		_, _ = db.C("externalService").DeleteMany(ctx, pgdoc.M{})
		mustInsert(t, db, "externalService", objectRow("bad", externalservice.StatusPublic, "http://127.0.0.1:1"))
		mustInsert(t, db, "project", pgdoc.M{"_id": "p2", "name": "p", "status": project.StatusEnabled, "services": []any{}})
		q, _ := repo.FindByID(ctx, "p2")
		if err := h.EnableAndBootstrap(ctx, q); err == nil {
			t.Fatal("want an error")
		}
		q, _ = repo.FindByID(ctx, "p2")
		if _, ok := q.ProvisionFailures["bad"]; !ok {
			t.Fatalf("failure not stored: %+v", q)
		}
		// Inside the retry window nothing is tried, and the project still does not work.
		if err := h.EnableAndBootstrap(ctx, q); err == nil {
			t.Fatal("want an error inside the window too")
		}
	})

	t.Run("a hung provider shows on its row", func(t *testing.T) {
		stop := make(chan struct{})
		hung := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-stop }))
		defer hung.Close()
		defer close(stop)
		_, _ = db.C("externalService").DeleteMany(ctx, pgdoc.M{})
		mustInsert(t, db, "externalService", objectRow("hung", externalservice.StatusPublic, hung.URL))
		mustInsert(t, db, "project", pgdoc.M{"_id": "p3", "name": "p", "status": project.StatusEnabled, "services": []any{}})
		q, _ := repo.FindByID(ctx, "p3")
		if err := h.EnableAndBootstrap(ctx, q); err == nil {
			t.Fatal("want an error")
		}
		row, err := esSvc.Get(ctx, "hung")
		if err != nil || row.ProvisionError == nil {
			t.Fatalf("the attempt that ran out must still be recorded on the provider: %+v %v", row, err)
		}
	})

	t.Run("a failure on a provider that has gone is dropped", func(t *testing.T) {
		_, _ = db.C("externalService").DeleteMany(ctx, pgdoc.M{})
		mustInsert(t, db, "externalService", pgdoc.M{
			"_id": "dns", "name": "dns", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
			"config": pgdoc.M{"provider": "powerdns", "apiUrl": "http://127.0.0.1:1", "region": "r1"},
		})
		// Recorded long enough ago that it is due: the next open looks at it and drops it.
		if err := repo.SetProvisionFailures(ctx, "p1", map[string]project.ProvisionFailure{"gone": {Message: "x", At: time.Now().Add(-time.Hour)}}); err != nil {
			t.Fatal(err)
		}
		q, _ := repo.FindByID(ctx, "p1")
		if err := h.EnableAndBootstrap(ctx, q); err != nil {
			t.Fatal(err)
		}
		q, _ = repo.FindByID(ctx, "p1")
		if len(q.ProvisionFailures) != 0 {
			t.Fatalf("still there: %+v", q.ProvisionFailures)
		}
	})
}
