package audit

import (
	"context"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/shopspring/decimal"
)

func findChange(cs []PropertyChange, field string) *PropertyChange {
	for i := range cs {
		if cs[i].Field == field {
			return &cs[i]
		}
	}
	return nil
}

func TestDiffSnapshots(t *testing.T) {
	t.Run("scalar change", func(t *testing.T) {
		cs := DiffSnapshots(map[string]any{"name": "a"}, map[string]any{"name": "b"})
		if len(cs) != 1 || cs[0].Field != "name" || cs[0].OldValue != "a" || cs[0].NewValue != "b" {
			t.Fatalf("got %#v", cs)
		}
	})

	t.Run("nested map → dotted path", func(t *testing.T) {
		cs := DiffSnapshots(
			map[string]any{"cfg": map[string]any{"limit": int32(2)}},
			map[string]any{"cfg": map[string]any{"limit": int32(5)}},
		)
		c := findChange(cs, "cfg.limit")
		if c == nil || c.OldValue != "2" || c.NewValue != "5" {
			t.Fatalf("got %#v", cs)
		}
	})

	t.Run("volatile keys skipped", func(t *testing.T) {
		before := map[string]any{"_id": "x", "updatedAt": "t1", "createdAt": "c", "status": "ACTIVE"}
		after := map[string]any{"_id": "x", "updatedAt": "t2", "createdAt": "c", "status": "EXPIRED"}
		cs := DiffSnapshots(before, after)
		if len(cs) != 1 || cs[0].Field != "status" {
			t.Fatalf("only status should change, got %#v", cs)
		}
	})

	t.Run("list order-insensitive", func(t *testing.T) {
		cs := DiffSnapshots(map[string]any{"t": []any{"a", "b"}}, map[string]any{"t": []any{"b", "a"}})
		if len(cs) != 0 {
			t.Fatalf("reordered list should be equal, got %#v", cs)
		}
	})

	t.Run("list real change", func(t *testing.T) {
		cs := DiffSnapshots(map[string]any{"t": []any{"a"}}, map[string]any{"t": []any{"a", "b"}})
		if findChange(cs, "t") == nil {
			t.Fatalf("got %#v", cs)
		}
	})

	t.Run("added key", func(t *testing.T) {
		cs := DiffSnapshots(map[string]any{"a": int32(1)}, map[string]any{"a": int32(1), "b": int32(2)})
		c := findChange(cs, "b")
		if c == nil || c.OldValue != nil || c.NewValue != "2" {
			t.Fatalf("got %#v", cs)
		}
	})

	t.Run("numeric width lenient", func(t *testing.T) {
		cs := DiffSnapshots(map[string]any{"n": int32(5)}, map[string]any{"n": int64(5)})
		if len(cs) != 0 {
			t.Fatalf("int32 vs int64 same value should be equal, got %#v", cs)
		}
	})

	t.Run("sensitive keys never diffed (no secret leak)", func(t *testing.T) {
		cs := DiffSnapshots(
			map[string]any{"name": "stripe", "secret": map[string]any{"apiKey": "sk_OLD"}, "password": "p1"},
			map[string]any{"name": "stripe2", "secret": map[string]any{"apiKey": "sk_NEW"}, "password": "p2"},
		)
		if findChange(cs, "name") == nil {
			t.Fatalf("name should diff: %#v", cs)
		}
		for _, c := range cs {
			if strings.Contains(c.Field, "secret") || strings.Contains(c.Field, "password") || strings.Contains(c.Field, "apiKey") {
				t.Errorf("sensitive field leaked into audit diff: %#v", c)
			}
		}
	})

	t.Run("decimal + pgdoc.M nested", func(t *testing.T) {
		d1, _ := decimal.NewFromString("0.50")
		d2, _ := decimal.NewFromString("0.60")
		cs := DiffSnapshots(
			map[string]any{"q": pgdoc.M{"amount": d1}},
			map[string]any{"q": pgdoc.M{"amount": d2}},
		)
		// decimal.String() normalizes trailing zeros (0.50 → 0.5): numerically identical.
		c := findChange(cs, "q.amount")
		if c == nil || c.OldValue != "0.5" || c.NewValue != "0.6" {
			t.Fatalf("got %#v", cs)
		}
	})
}

func TestRecordSnapshotsFlow(t *testing.T) {
	// no capture in context → no-op, Changes nil
	RecordSnapshots(context.Background(), map[string]any{"a": "1"}, map[string]any{"a": "2"})

	ctx, holder := WithSnapshotCapture(context.Background())
	if holder.Changes() != nil {
		t.Errorf("nothing recorded → nil changes")
	}
	RecordSnapshots(ctx, map[string]any{"status": "ACTIVE"}, map[string]any{"status": "CANCELLED"})
	cs := holder.Changes()
	if len(cs) != 1 || cs[0].Field != "status" || cs[0].NewValue != "CANCELLED" {
		t.Fatalf("got %#v", cs)
	}

	// no-change snapshots → nil
	_, h2 := WithSnapshotCapture(context.Background())
	RecordSnapshots(context.WithValue(context.Background(), ctxKeyDiff{}, h2), map[string]any{"a": "1"}, map[string]any{"a": "1"})
	if h2.Changes() != nil {
		t.Errorf("identical snapshots → nil changes, got %#v", h2.Changes())
	}
}
