//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestUserRequireGetOrCreate guards the login-race fix: Require must get-or-create the platform user
// from the validated request-context claims on first sight (so the dashboard works on the first call
// after a social login, not only after the FE's one-shot POST /user init), be idempotent, and still
// 400 when the context carries no usable claims.
func TestUserRequireGetOrCreate(t *testing.T) {
	db := freshPG(t)
	repo := user.NewRepo(db)
	if err := repo.EnsureIndexes(context.Background()); err != nil {
		t.Fatal(err)
	}

	rcCtx := httpx.WithRC(context.Background(), &httpx.RequestContext{
		Sub: "race-sub", Email: "race@x.io", GivenName: "Race", FamilyName: "User", Issuer: "iss",
	})

	// first sight → created from claims
	u, err := repo.Require(rcCtx, "race-sub")
	if err != nil || u == nil {
		t.Fatalf("Require get-or-create: u=%v err=%v", u, err)
	}
	if u.Email != "race@x.io" || u.Sub != "race-sub" {
		t.Fatalf("created user claims not applied: %+v", u)
	}

	// idempotent → same doc, no duplicate
	u2, err := repo.Require(rcCtx, "race-sub")
	if err != nil || u2 == nil || u2.ID != u.ID {
		t.Fatalf("Require not idempotent: u2=%v err=%v (want id %s)", u2, err, u.ID)
	}
	n, _ := db.C("users").Count(context.Background(), pgdoc.M{"sub": "race-sub"})
	if n != 1 {
		t.Fatalf("want exactly 1 user doc, got %d", n)
	}

	// no claims in context → the old 400 (a non-request ctx, e.g. background jobs)
	if _, err := repo.Require(context.Background(), "unknown-sub"); err == nil {
		t.Fatal("Require without context claims should error (User is not initialized)")
	}
}
