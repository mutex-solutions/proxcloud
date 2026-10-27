//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

type pgThing struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Status    string          `json:"status,omitempty"`
	Amount    decimal.Decimal `json:"amount"`
	Tags      []string        `json:"tags,omitempty"`
	Members   []pgMember      `json:"members,omitempty"`
	Comment   string          `json:"comments,omitempty"`
	CreatedAt *time.Time      `json:"createdAt,omitempty"`
}

type pgMember struct {
	Sub  string `json:"sub"`
	Role string `json:"role"`
}

func TestPgdocStoreCRUD(t *testing.T) {
	db := freshPG(t)
	ctx := context.Background()
	col := db.C("thing")
	if err := col.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := col.EnsureIndex(ctx, "name_unique", true, pgdoc.F("name")); err != nil {
		t.Fatal(err)
	}
	// A time-field expression index must be creatable — the ::timestamptz cast
	// is not IMMUTABLE, so the index expression uses the $date text instead
	// (regression: projectInvite's expiresAt index failed with 42P17).
	if err := col.EnsureIndex(ctx, "created_idx", false,
		pgdoc.IndexField{Field: "createdAt", Kind: pgdoc.KTime}); err != nil {
		t.Fatalf("KTime index must create: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	th := pgThing{
		Name: "alpha", Status: "ACTIVE",
		Amount:    decimal.RequireFromString("12.345"),
		Tags:      []string{"x"},
		Members:   []pgMember{{Sub: "u1", Role: "OWNER"}},
		CreatedAt: &now,
	}
	id, err := col.InsertOne(ctx, th)
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 24 {
		t.Fatalf("id: %q", id)
	}

	// unique index + IsDup
	if _, err := col.InsertOne(ctx, pgThing{Name: "alpha"}); !pgdoc.IsDup(err) {
		t.Fatalf("expected dup, got %v", err)
	}

	// Get round-trips decimal + time exactly.
	var got pgThing
	found, err := col.Get(ctx, id, &got)
	if err != nil || !found {
		t.Fatalf("get: %v %v", found, err)
	}
	if got.ID != id || !got.Amount.Equal(th.Amount) || !got.CreatedAt.Equal(now) {
		t.Fatalf("round trip: %+v", got)
	}

	// FindOne by field; deterministic empty-filter pick.
	if _, err := col.InsertOne(ctx, pgThing{Name: "beta", Status: "DISABLED"}); err != nil {
		t.Fatal(err)
	}
	var first pgThing
	if ok, _ := col.FindOne(ctx, pgdoc.M{}, &first); !ok || first.Name != "alpha" {
		t.Fatalf("findone default order: %+v", first)
	}

	// Filters: time compare, decimal compare, contains, regex, in.
	var out []pgThing
	if err := col.Find(ctx, pgdoc.M{"createdAt": pgdoc.M{"$lte": time.Now().UTC()}}, &out); err != nil || len(out) != 1 {
		t.Fatalf("time filter: %d %v", len(out), err)
	}
	if err := col.Find(ctx, pgdoc.M{"amount": pgdoc.M{"$gt": decimal.RequireFromString("12")}}, &out); err != nil || len(out) != 1 {
		t.Fatalf("decimal filter: %d %v", len(out), err)
	}
	if err := col.Find(ctx, pgdoc.M{"members": pgdoc.M{"$contains": pgdoc.M{"sub": "u1"}}}, &out); err != nil || len(out) != 1 {
		t.Fatalf("contains filter: %d %v", len(out), err)
	}
	if err := col.Find(ctx, pgdoc.M{"name": pgdoc.M{"$regex": "^AL", "$options": "i"}}, &out); err != nil || len(out) != 1 {
		t.Fatalf("regex filter: %d %v", len(out), err)
	}
	if err := col.Find(ctx, pgdoc.M{"status": pgdoc.M{"$in": []string{"ACTIVE", "DISABLED"}}}, &out,
		pgdoc.Sort(pgdoc.Asc("name"))); err != nil || len(out) != 2 || out[0].Name != "alpha" {
		t.Fatalf("in+sort: %d %v", len(out), err)
	}

	// SetFields + unset (null-drop parity) + dotted set.
	if ok, err := col.SetByID(ctx, id, pgdoc.M{"status": "SUSPENDED", "comments": "will drop"}, nil); err != nil || !ok {
		t.Fatalf("set: %v", err)
	}
	if ok, err := col.SetByID(ctx, id, pgdoc.M{"name": "alpha2"}, []string{"comments"}); err != nil || !ok {
		t.Fatalf("set+unset: %v", err)
	}
	col.Get(ctx, id, &got)
	if got.Status != "SUSPENDED" || got.Name != "alpha2" || got.Comment != "" {
		t.Fatalf("after set/unset: %+v", got)
	}

	// Array push/pull.
	if n, err := col.PushToArray(ctx, pgdoc.M{"_id": id}, "members", pgMember{Sub: "u2", Role: "MEMBER"}); err != nil || n != 1 {
		t.Fatalf("push: %d %v", n, err)
	}
	if n, err := col.PullFromArray(ctx, pgdoc.M{"_id": id}, "members", pgdoc.M{"sub": "u1"}); err != nil || n != 1 {
		t.Fatalf("pull: %d %v", n, err)
	}
	col.Get(ctx, id, &got)
	if len(got.Members) != 1 || got.Members[0].Sub != "u2" {
		t.Fatalf("members after push/pull: %+v", got.Members)
	}

	// Distinct + Count + keyset.
	if vals, err := col.Distinct(ctx, "status", pgdoc.M{}); err != nil || len(vals) != 2 {
		t.Fatalf("distinct: %v %v", vals, err)
	}
	if n, _ := col.Count(ctx, pgdoc.M{"status": "SUSPENDED"}); n != 1 {
		t.Fatalf("count: %d", n)
	}
	if err := col.Find(ctx, pgdoc.M{"_id": pgdoc.M{"$gt": id}}, &out,
		pgdoc.Sort(pgdoc.Asc("_id"))); err != nil {
		t.Fatalf("keyset: %v", err)
	}

	// Tx: rollback leaves data untouched; GetForUpdate works inside.
	errBoom := errors.New("boom")
	err = db.WithTx(ctx, func(tc context.Context) error {
		var row pgThing
		if ok, err := col.GetForUpdate(tc, id, &row); err != nil || !ok {
			t.Fatalf("get for update: %v", err)
		}
		if ok, err := col.SetByID(tc, id, pgdoc.M{"status": "GONE"}, nil); err != nil || !ok {
			t.Fatalf("tx set: %v", err)
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("tx err: %v", err)
	}
	col.Get(ctx, id, &got)
	if got.Status != "SUSPENDED" {
		t.Fatalf("rollback failed: %s", got.Status)
	}

	// Deletes.
	if ok, _ := col.DeleteByID(ctx, id); !ok {
		t.Fatal("delete by id")
	}
	if n, _ := col.DeleteMany(ctx, pgdoc.M{}); n != 1 {
		t.Fatalf("delete many: %d", n)
	}
}
