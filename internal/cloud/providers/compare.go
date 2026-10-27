package providers

import (
	"fmt"
	"reflect"
	"sort"
)

// asMap / asList normalize a value that may be a plain map[string]any / []any (a fresh
// build or a pgdoc decode) OR a NAMED map/slice type with the same underlying shape (the
// JSON codec's pgdoc.M/pgdoc.A round-trip shapes) — a bare type assertion misses
// named types, which made every keyed compare "differ" → update churn on every sync pass.
// LIVE-CAUGHT on the kolla cloud: run-sync #2 updated 3 unchanged resources. The reflect
// conversion covers both shapes without depending on the driver types.
var (
	tAnyMap  = reflect.TypeOf(map[string]any(nil))
	tAnyList = reflect.TypeOf([]any(nil))
)

func asMap(v any) (map[string]any, bool) {
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	if rv := reflect.ValueOf(v); rv.IsValid() && rv.Kind() == reflect.Map && rv.Type().ConvertibleTo(tAnyMap) {
		return rv.Convert(tAnyMap).Interface().(map[string]any), true
	}
	return nil, false
}

func asList(v any) ([]any, bool) {
	if l, ok := v.([]any); ok {
		return l, true
	}
	if rv := reflect.ValueOf(v); rv.IsValid() && rv.Kind() == reflect.Slice && rv.Type().ConvertibleTo(tAnyList) {
		return rv.Convert(tAnyList).Interface().([]any), true
	}
	return nil, false
}

// goString stringifies list elements — fmt %v is deterministic (Go sorts map
// keys when printing), so the sorted-projection list compare is stable.
func goString(v any) string { return fmt.Sprintf("%v", v) }

// compare.go implements the per-type isNeededToUpdate
// comparison. It compares only the type's data sub-key(s) (e.g. network→"network", router→
// "router"), recursively, with:
//   - NUMBER-WIDTH tolerance (compared as float64) — a stored int32/int64/float64
//     round-trip doesn't count as a change;
//   - ORDER-INSENSITIVE list compare (sorted toString) — a neutron list that comes
//     back reordered (security_group_rules, allocation_pools, dns_nameservers, host_routes) isn't
//     a change.
// Go's default Reconcile.dataEqual does a whole-data JSON string compare, which churns on both of
// those. A Provider that implements KeyedComparer opts into this keyed compare instead.

// KeyedComparer is an optional Provider capability declaring which data sub-keys define "changed"
// for isNeededToUpdate. When a
// Provider implements it, Reconcile compares only those sub-maps (compareMaps) instead of the
// whole-data JSON compare — so a reordered list or a number-width round-trip produces no spurious
// update/audit churn.
type KeyedComparer interface {
	CompareKeys() []string
}

// dataEqualKeyed reports whether a and b are equal across every declared key (per
// key, AND-ed — the resource is "unchanged" only if all keys match). NO keys (empty) = compare the
// WHOLE data map with compareMaps (the object-store provider
// passes a null dataKey → the whole map is used).
func dataEqualKeyed(a, b map[string]any, keys []string) bool {
	if len(keys) == 0 {
		return compareMaps(a, b)
	}
	for _, k := range keys {
		if !subMapEquals(a, b, k) {
			return false
		}
	}
	return true
}

// subMapEquals pulls data.<key> as a map from each; both-nil→equal,
// one-nil→not equal, else compareMaps.
func subMapEquals(a, b map[string]any, key string) bool {
	m1, ok1 := asMap(a[key])
	m2, ok2 := asMap(b[key])
	if ok1 && ok2 {
		return compareMaps(m1, m2)
	}
	// neither is a map → treat both as "nil" (equal); exactly one is a map → differ.
	return !ok1 && !ok2
}

// compareMaps: same-size, then per-key
// recursive compare with number-width tolerance, order-insensitive lists, nested-map recursion.
func compareMaps(m1, m2 map[string]any) bool {
	if len(m1) != len(m2) {
		return false
	}
	for k, val1 := range m1 {
		val2, present := m2[k]
		v1nil := val1 == nil
		v2nil := val2 == nil || !present
		if v1nil || v2nil {
			if v1nil != v2nil {
				return false // exactly one nil
			}
			continue // both nil
		}
		if n1, ok := asNumber(val1); ok {
			if n2, ok := asNumber(val2); ok {
				if n1 != n2 {
					return false
				}
				continue
			}
		}
		if l1, ok := asList(val1); ok {
			if l2, ok := asList(val2); ok {
				if !listEqual(l1, l2) {
					return false
				}
				continue
			}
		}
		if mm1, ok := asMap(val1); ok {
			if mm2, ok := asMap(val2); ok {
				if !compareMaps(mm1, mm2) {
					return false
				}
				continue
			}
		}
		if !reflect.DeepEqual(val1, val2) {
			return false
		}
	}
	return true
}

// listEqual: same length + equal sorted toString projections
// (order-insensitive). nil handling: both-nil→equal, one-nil→not.
func listEqual(l1, l2 []any) bool {
	if l1 == nil || l2 == nil {
		return l1 == nil && l2 == nil
	}
	if len(l1) != len(l2) {
		return false
	}
	s1 := toStringsSorted(l1)
	s2 := toStringsSorted(l2)
	for i := range s1 {
		if s1[i] != s2[i] {
			return false
		}
	}
	return true
}

func toStringsSorted(l []any) []string {
	out := make([]string, len(l))
	for i, v := range l {
		out[i] = goString(v)
	}
	sort.Strings(out)
	return out
}

// asNumber returns the float64 value of any Go numeric kind (int*/uint*/float*) — so
// int32/int64/float64 of the same value compare equal.
func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}
