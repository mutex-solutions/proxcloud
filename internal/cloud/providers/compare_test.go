package providers

import "testing"

// legacyM / legacyA emulate NAMED map/slice types with plain underlying shapes (the old
// JSON round-trip's pgdoc.M/pgdoc.A) — asMap/asList must catch them via the
// reflect fallback, not just bare type assertions.
type (
	legacyM map[string]any
	legacyA []any
)

// Golden tests for compareMaps: number-width tolerance,
// order-insensitive lists, nested-map recursion, size/nil rules.

func TestCompareMapsNumberWidth(t *testing.T) {
	// int32 (JSON round-trip) vs float64 (live JSON decode) of the same value → equal.
	a := map[string]any{"mtu": int32(1500), "ip_version": int64(4)}
	b := map[string]any{"mtu": float64(1500), "ip_version": float64(4)}
	if !compareMaps(a, b) {
		t.Error("number-width round-trip must compare equal")
	}
	if compareMaps(a, map[string]any{"mtu": float64(1501), "ip_version": float64(4)}) {
		t.Error("different numbers must differ")
	}
}

func TestCompareMapsListReorder(t *testing.T) {
	a := map[string]any{"dns_nameservers": []any{"8.8.8.8", "1.1.1.1"}}
	b := map[string]any{"dns_nameservers": []any{"1.1.1.1", "8.8.8.8"}}
	if !compareMaps(a, b) {
		t.Error("reordered list must compare equal (order-insensitive)")
	}
	if compareMaps(a, map[string]any{"dns_nameservers": []any{"1.1.1.1", "9.9.9.9"}}) {
		t.Error("different list contents must differ")
	}
	if compareMaps(a, map[string]any{"dns_nameservers": []any{"8.8.8.8"}}) {
		t.Error("different list length must differ")
	}
}

func TestCompareMapsNestedAndSize(t *testing.T) {
	a := map[string]any{"x": map[string]any{"a": int32(1)}}
	b := map[string]any{"x": map[string]any{"a": float64(1)}}
	if !compareMaps(a, b) {
		t.Error("nested map with number-width must compare equal")
	}
	// size differs
	if compareMaps(a, map[string]any{"x": map[string]any{"a": int32(1)}, "y": 2}) {
		t.Error("different size must differ")
	}
}

func TestCompareMapsNil(t *testing.T) {
	if !compareMaps(map[string]any{"a": nil}, map[string]any{"a": nil}) {
		t.Error("both-nil must be equal")
	}
	if compareMaps(map[string]any{"a": nil}, map[string]any{"a": 1}) {
		t.Error("one-nil must differ")
	}
}

func TestSubMapEquals(t *testing.T) {
	a := map[string]any{"network": map[string]any{"id": "n1", "mtu": int32(1500)}, "networkName": "pw"}
	b := map[string]any{"network": map[string]any{"id": "n1", "mtu": float64(1500)}, "networkName": "DIFFERENT"}
	// keyed on "network" only → networkName difference is ignored.
	if !subMapEquals(a, b, "network") {
		t.Error("keyed network compare must ignore networkName + tolerate number width")
	}
	c := map[string]any{"network": map[string]any{"id": "n2"}}
	if subMapEquals(a, c, "network") {
		t.Error("different network id must differ")
	}
	// both missing the key → equal (e.g. clusterInfo absent on both)
	if !subMapEquals(a, b, "clusterInfo") {
		t.Error("both-absent key must be equal")
	}
	// one has the key, the other doesn't → differ
	if subMapEquals(map[string]any{"clusterInfo": map[string]any{"x": 1}}, a, "clusterInfo") {
		t.Error("one-present-one-absent must differ")
	}
}

func TestDataEqualKeyedWholeMap(t *testing.T) {
	// Empty keys = whole-map compareMaps (null-key / bucket): number-width tolerant over the
	// flat bucket data so objectCount int32↔float64 doesn't churn.
	a := map[string]any{"bucketName": "pw", "objectCount": int32(3), "sizeInBytes": int64(1024)}
	b := map[string]any{"bucketName": "pw", "objectCount": float64(3), "sizeInBytes": float64(1024)}
	if !dataEqualKeyed(a, b, []string{}) {
		t.Error("empty keys must whole-map compare (number tolerant)")
	}
	if dataEqualKeyed(a, map[string]any{"bucketName": "pw", "objectCount": float64(4), "sizeInBytes": float64(1024)}, []string{}) {
		t.Error("changed objectCount must differ")
	}
}

func TestDataEqualKeyed(t *testing.T) {
	cached := map[string]any{"router": map[string]any{"id": "r1", "name": "pw"}, "routerName": "pw"}
	// routerName re-derived differently but router sub-map identical → unchanged.
	fresh := map[string]any{"router": map[string]any{"id": "r1", "name": "pw"}, "routerName": "pw-rederived"}
	if !dataEqualKeyed(cached, fresh, []string{"router"}) {
		t.Error("router keyed compare must ignore routerName churn")
	}
	changed := map[string]any{"router": map[string]any{"id": "r1", "name": "renamed"}}
	if dataEqualKeyed(cached, changed, []string{"router"}) {
		t.Error("changed router name must be detected")
	}
}

// LIVE-CAUGHT (kolla run-sync churn): the cached side's nested docs may decode as NAMED
// map/slice types — the compare must treat them as maps/lists, not fall to DeepEqual
// (always differ).
func TestCompareMapsRoundTrip(t *testing.T) {
	cached := map[string]any{
		"network": legacyM{"id": "n1", "mtu": int32(1500), "tags": legacyA{"a", "b"}},
	}
	fresh := map[string]any{
		"network": map[string]any{"id": "n1", "mtu": float64(1500), "tags": []any{"b", "a"}},
	}
	if !dataEqualKeyed(cached, fresh, []string{"network"}) {
		t.Fatal("named map/slice round-trip must compare equal (this was the churn bug)")
	}
	changed := map[string]any{"network": map[string]any{"id": "n2", "mtu": float64(1500), "tags": []any{"a", "b"}}}
	if dataEqualKeyed(cached, changed, []string{"network"}) {
		t.Fatal("real change must still be detected")
	}
}
