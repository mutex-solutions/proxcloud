package catalog

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Golden tests for the ImageGroupController projection (sort by orderNumber + enabled-group
// category filter) over shaped docs (int32/float64 orderNumbers = the codec round-trip).

func TestSortImageGroups(t *testing.T) {
	groups := []pgdoc.M{
		{"id": "g2", "orderNumber": int32(2), "images": []any{
			pgdoc.M{"name": "b", "orderNumber": float64(2)},
			pgdoc.M{"name": "a", "orderNumber": int32(1)},
		}},
		{"id": "g1", "orderNumber": int64(1)},
		{"id": "g0"}, // no orderNumber → 0 → first
	}
	got := SortImageGroups(groups)
	if got[0]["id"] != "g0" || got[1]["id"] != "g1" || got[2]["id"] != "g2" {
		t.Fatalf("group order wrong: %v %v %v", got[0]["id"], got[1]["id"], got[2]["id"])
	}
	imgs := got[2]["images"].([]any)
	if imgs[0].(pgdoc.M)["name"] != "a" || imgs[1].(pgdoc.M)["name"] != "b" {
		t.Fatalf("image order wrong: %v", imgs)
	}
}

// The codec hands nested arrays back as a NAMED []any type (pgdoc.A) — the in-place
// sort must still stick to the original document through the shared backing array.
type namedList []any

func TestSortImageGroupsNamedArrayType(t *testing.T) {
	groups := []pgdoc.M{
		{"id": "g1", "images": namedList{
			pgdoc.M{"name": "b", "orderNumber": int32(2)},
			pgdoc.M{"name": "a", "orderNumber": int32(1)},
		}},
	}
	got := SortImageGroups(groups)
	imgs := got[0]["images"].(namedList)
	if imgs[0].(pgdoc.M)["name"] != "a" || imgs[1].(pgdoc.M)["name"] != "b" {
		t.Fatalf("image order wrong: %v", imgs)
	}
}

func TestFilterCategoriesWithEnabledGroup(t *testing.T) {
	groups := []pgdoc.M{
		{"id": "g1", "enabled": true, "categoryId": "cat-linux"},
		{"id": "g2", "enabled": false, "categoryId": "cat-windows"}, // disabled → its category drops
		{"id": "g3", "categoryId": "cat-bsd"},                       // enabled absent → false → drops
	}
	cats := []pgdoc.M{
		{"id": "cat-linux", "name": "Linux"},
		{"id": "cat-windows", "name": "Windows"},
		{"id": "cat-bsd", "name": "BSD"},
		{"id": "cat-orphan", "name": "Orphan"}, // no group at all
	}
	got := FilterCategoriesWithEnabledGroup(cats, groups)
	if len(got) != 1 || got[0]["id"] != "cat-linux" {
		t.Fatalf("filtered = %v, want only cat-linux", got)
	}
}
