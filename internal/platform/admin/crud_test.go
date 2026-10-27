package admin

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// TestNormFilter pins the stored.M→pgdoc.M filter normalization: nested pgdoc.M / pgdoc.A convert to
// the map/slice shapes the pgdoc filter translator type-switches on; plain values pass through.
func TestNormFilter(t *testing.T) {
	f := normFilter(pgdoc.M{"a": pgdoc.M{"$in": pgdoc.A{"x", "y"}}, "b": "z"})
	in, ok := f["a"].(map[string]any)
	if !ok {
		t.Fatalf("nested pgdoc.M should become map[string]any, got %T", f["a"])
	}
	list, ok := in["$in"].([]any)
	if !ok || len(list) != 2 || list[0] != "x" {
		t.Errorf("pgdoc.A should become []any, got %#v", in["$in"])
	}
	if f["b"] != "z" {
		t.Errorf("plain values must pass through, got %#v", f["b"])
	}
}

func TestShapeDoc(t *testing.T) {
	oid := pgdoc.NewID()
	doc := pgdoc.M{"_id": oid, "_class": "CustomMenuItem", "displayName": "Docs"}
	shapeDoc(doc)
	if _, ok := doc["_id"]; ok {
		t.Error("_id should be removed")
	}
	if _, ok := doc["_class"]; ok {
		t.Error("_class should be dropped")
	}
	if doc["id"] != oid {
		t.Errorf("id should carry the original _id value, got %#v", doc["id"])
	}
	if doc["displayName"] != "Docs" {
		t.Error("other fields must be preserved")
	}
	if shapeDoc(nil) != nil {
		t.Error("shapeDoc(nil) must be nil")
	}
}

func TestAsInt(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{int32(3), 3}, {int64(4), 4}, {int(5), 5}, {float64(6), 6}, {nil, 0}, {"x", 0},
	}
	for _, c := range cases {
		if got := asInt(c.in); got != c.want {
			t.Errorf("asInt(%#v)=%d want %d", c.in, got, c.want)
		}
	}
}
