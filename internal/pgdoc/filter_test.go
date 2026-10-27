package pgdoc

import (
	"reflect"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func tr(t *testing.T, f M) (string, []any) {
	t.Helper()
	var args []any
	sql, err := translateFilter(f, &args)
	if err != nil {
		t.Fatalf("translate %v: %v", f, err)
	}
	return sql, args
}

func TestTranslateBasics(t *testing.T) {
	cases := []struct {
		name string
		f    M
		sql  string
		args []any
	}{
		{"empty", nil, "TRUE", nil},
		{"eq string", M{"sub": "u1"}, "doc->>'sub' = $1", []any{"u1"}},
		{"eq two fields sorted", M{"organizationId": "o1", "sub": "u1"},
			"doc->>'organizationId' = $1 AND doc->>'sub' = $2", []any{"o1", "u1"}},
		{"eq bool", M{"enabled": true}, "doc->>'enabled' = $1", []any{"true"}},
		{"eq int", M{"n": 5}, "(doc->>'n')::numeric = $1", []any{int64(5)}},
		{"eq nil", M{"deletedAt": nil},
			"(doc->'deletedAt' IS NULL OR doc->'deletedAt' = 'null'::jsonb)", nil},
		{"nested path", M{"customInfo.lang": "ro-ro"},
			`doc#>>'{customInfo,lang}' = $1`, []any{"ro-ro"}},
		{"ne", M{"status": M{"$ne": "DELETED"}},
			"doc->>'status' IS DISTINCT FROM $1", []any{"DELETED"}},
		{"ne nil", M{"email": M{"$ne": nil}},
			"NOT (doc->'email' IS NULL OR doc->'email' = 'null'::jsonb)", nil},
		{"exists true", M{"externalId": M{"$exists": true}}, "doc->'externalId' IS NOT NULL", nil},
		{"exists false", M{"externalId": M{"$exists": false}}, "doc->'externalId' IS NULL", nil},
		{"in strings", M{"status": M{"$in": []string{"A", "B"}}},
			"doc->>'status' = ANY($1)", []any{[]string{"A", "B"}}},
		{"nin strings", M{"status": M{"$nin": []string{"A"}}},
			"(doc->>'status' IS NULL OR NOT (doc->>'status' = ANY($1)))", []any{[]string{"A"}}},
		{"in any-list", M{"status": M{"$in": []any{"A", "B"}}},
			"doc->>'status' = ANY($1)", []any{[]string{"A", "B"}}},
		{"gt int", M{"count": M{"$gt": 3}}, "(doc->>'count')::numeric > $1", []any{int64(3)}},
		{"gte lte combined sorted", M{"n": M{"$gte": 1, "$lte": 9}},
			"(doc->>'n')::numeric >= $1 AND (doc->>'n')::numeric <= $2", []any{int64(1), int64(9)}},
		{"id eq", M{"_id": "abc"}, "id = $1", []any{"abc"}},
		{"id in", M{"_id": M{"$in": []string{"a", "b"}}}, "id = ANY($1)", []any{[]string{"a", "b"}}},
		{"id gt keyset", M{"_id": M{"$gt": "marker"}}, "id > $1", []any{"marker"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args := tr(t, c.f)
			if sql != c.sql {
				t.Errorf("sql:\n got  %s\n want %s", sql, c.sql)
			}
			if len(args) != len(c.args) {
				t.Fatalf("args: got %v want %v", args, c.args)
			}
			for i := range args {
				if !reflect.DeepEqual(args[i], c.args[i]) {
					t.Errorf("arg %d: got %#v want %#v", i, args[i], c.args[i])
				}
			}
		})
	}
}

func TestTranslateTyped(t *testing.T) {
	ts := time.Date(2026, 7, 6, 1, 2, 3, 0, time.UTC)
	sql, args := tr(t, M{"createdAt": M{"$lt": ts}})
	want := "(doc->>'createdAt')::timestamptz < $1"
	if sql != want {
		t.Errorf("time sql: got %s want %s", sql, want)
	}
	if !args[0].(time.Time).Equal(ts) {
		t.Errorf("time arg: %v", args[0])
	}

	d := decimal.RequireFromString("10.50")
	sql, args = tr(t, M{"amount": M{"$gte": d}})
	want = "(doc->>'amount')::numeric >= $1"
	if sql != want {
		t.Errorf("decimal sql: got %s want %s", sql, want)
	}
	if args[0] != "10.5" {
		t.Errorf("decimal arg: %v", args[0])
	}
}

func TestTranslateOrAnd(t *testing.T) {
	sql, args := tr(t, M{"$or": []M{{"a": "x"}, {"b": "y"}}})
	want := "((doc->>'a' = $1) OR (doc->>'b' = $2))"
	if sql != want {
		t.Errorf("or: got %s want %s", sql, want)
	}
	if args[0] != "x" || args[1] != "y" {
		t.Errorf("or args: %v", args)
	}

	sql, _ = tr(t, M{"$and": []M{{"a": "x"}, {"b": M{"$exists": false}}}})
	want = "((doc->>'a' = $1) AND (doc->'b' IS NULL))"
	if sql != want {
		t.Errorf("and: got %s want %s", sql, want)
	}
}

func TestTranslateRejects(t *testing.T) {
	for name, f := range map[string]M{
		"unknown op":      {"a": M{"$mod": 2}},
		"top-level op":    {"$where": "1"},
		"bad in":          {"a": M{"$in": []any{M{}}}},
		"id non-string":   {"_id": 5},
		"exists non-bool": {"a": M{"$exists": "yes"}},
	} {
		var args []any
		if _, err := translateFilter(f, &args); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestTranslateRegexContains(t *testing.T) {
	sql, args := tr(t, M{"email": M{"$regex": "foo", "$options": "i"}})
	if sql != "doc->>'email' ~* $1" || args[0] != "foo" {
		t.Errorf("regex i: %s %v", sql, args)
	}
	sql, _ = tr(t, M{"email": M{"$regex": "foo"}})
	if sql != "doc->>'email' ~ $1" {
		t.Errorf("regex: %s", sql)
	}
	// case-insensitive $or search shape (audit repo).
	sql, _ = tr(t, M{"$or": []M{
		{"actor.displayName": M{"$regex": "x", "$options": "i"}},
		{"action": M{"$regex": "x", "$options": "i"}},
	}})
	want := `((doc#>>'{actor,displayName}' ~* $1) OR (doc->>'action' ~* $2))`
	if sql != want {
		t.Errorf("or-regex:\n got  %s\n want %s", sql, want)
	}

	// array containment: object subset + scalar.
	sql, args = tr(t, M{"memberships": M{"$contains": M{"sub": "u1"}}})
	if sql != "doc->'memberships' @> $1::jsonb" || args[0] != `[{"sub":"u1"}]` {
		t.Errorf("contains obj: %s %v", sql, args)
	}
	sql, args = tr(t, M{"pricePlanIds": M{"$contains": "p1"}})
	if sql != "doc->'pricePlanIds' @> $1::jsonb" || args[0] != `["p1"]` {
		t.Errorf("contains scalar: %s %v", sql, args)
	}
	sql, _ = tr(t, M{"adjustments": M{"$elemMatch": M{"priceAdjustmentRuleId": "r1"}}})
	if sql != "doc->'adjustments' @> $1::jsonb" {
		t.Errorf("elemMatch: %s", sql)
	}
}

func TestQuoting(t *testing.T) {
	sql, _ := tr(t, M{"a'b": "v"})
	if sql != "doc->>'a''b' = $1" {
		t.Errorf("quote escape: %s", sql)
	}
}
