package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// diff.go computes the field-level before/after diff that turns two snapshot maps into a flat
// []PropertyChange the admin Audit-Log detail view renders. Handlers stash a before/after snapshot
// via RecordSnapshots; the admin audit middleware reads it and computes the diff onto the event's
// Changes.

// volatileSnapshotKeys are doc fields that are never a meaningful field change (document identity /
// discriminator / audit timestamps). We diff the raw docs and skip them here (at every
// nesting level) so they never surface as changes.
var volatileSnapshotKeys = map[string]bool{
	"_id": true, "id": true, "_class": true, "createdAt": true, "updatedAt": true,
}

// sensitiveSnapshotKeys are credential/secret fields that must NEVER appear in the audit log. We
// diff raw docs, so we skip them explicitly — at every nesting level — to uphold the secret-strip
// invariant. A change to one of these produces NO diff.
var sensitiveSnapshotKeys = map[string]bool{
	"secret": true, "secretKey": true, "password": true, "adminPassword": true,
	"payload": true, "privateKey": true, "apiKey": true, "token": true,
	"clientSecret": true, "accessKey": true, "hmacKeyId": true,
}

// skipDiffKey reports whether a snapshot key is excluded from the diff (volatile metadata OR a secret).
func skipDiffKey(k string) bool { return volatileSnapshotKeys[k] || sensitiveSnapshotKeys[k] }

// DiffSnapshots is a recursive field-by-field diff of two snapshot maps → a flat []PropertyChange
// with dotted field paths (nested maps recurse; a map-vs-nonmap or a leaf change is one entry).
// Values are compared after normalization (lists are order-insensitive; numeric width is ignored —
// a deliberate, documented choice, to avoid spurious diffs from the stored int32/int64/float64
// round-trip) and stored via sanitize (a readable string form). Never nil.
func DiffSnapshots(before, after map[string]any) []PropertyChange {
	changes := []PropertyChange{}
	diffSnapshotsRecursive("", before, after, &changes)
	return changes
}

func diffSnapshotsRecursive(prefix string, before, after map[string]any, out *[]PropertyChange) {
	for _, k := range orderedUnionKeys(before, after) {
		if skipDiffKey(k) {
			continue
		}
		o, n := before[k], after[k]
		if valuesEqual(o, n) {
			continue
		}
		full := k
		if prefix != "" {
			full = prefix + "." + k
		}
		om, oOK := asMap(o)
		nm, nOK := asMap(n)
		if oOK && nOK {
			diffSnapshotsRecursive(full, om, nm, out)
		} else {
			*out = append(*out, PropertyChange{Field: full, OldValue: sanitize(o), NewValue: sanitize(n)})
		}
	}
}

// orderedUnionKeys returns the union of both maps' keys, before's keys first then after-only keys;
// each group sorted for determinism.
func orderedUnionKeys(before, after map[string]any) []string {
	seen := map[string]bool{}
	keys := []string{}
	add := func(m map[string]any) {
		ks := make([]string, 0, len(m))
		for k := range m {
			if !seen[k] {
				seen[k] = true
				ks = append(ks, k)
			}
		}
		sort.Strings(ks)
		keys = append(keys, ks...)
	}
	add(before)
	add(after)
	return keys
}

// asMap unwraps a nested object (map[string]any) for recursion.
func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// valuesEqual reports equal iff the two values' normalized canonical forms match.
func valuesEqual(a, b any) bool { return canonical(a) == canonical(b) }

// canonical produces a stable, order-insensitive string for equality:
// maps → sorted key:canonical(val) (volatile keys skipped); lists → sorted canonical(elem); scalars →
// their string form (width-insensitive for numbers).
func canonical(v any) string {
	switch x := v.(type) {
	case nil:
		return "\x00nil"
	case map[string]any:
		return canonicalMap(x)
	case []any:
		return canonicalList(x)
	default:
		return scalarString(x)
	}
}

func canonicalMap(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if !skipDiffKey(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(canonical(m[k]))
		b.WriteByte(',')
	}
	b.WriteByte('}')
	return b.String()
}

func canonicalList(l []any) string {
	parts := make([]string, 0, len(l))
	for _, e := range l {
		parts = append(parts, canonical(e))
	}
	sort.Strings(parts)
	return "[" + strings.Join(parts, ",") + "]"
}

// scalarString renders a scalar to a canonical string (numbers width-insensitive: 5/5.0 match).
func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return "\x00nil"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.FormatInt(int64(x), 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 64)
	case decimal.Decimal:
		return x.String()
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case json.Number:
		return x.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// sanitize renders a value for storage: scalars → a readable string; everything else (maps, lists,
// typed structs/slices) → compact JSON so the stored old/new value is human-readable in the audit
// detail (NOT a Go %v dump). nil stays nil.
func sanitize(v any) any {
	switch v.(type) {
	case nil:
		return nil
	case string, bool, int, int32, int64, float64, float32,
		decimal.Decimal, time.Time, json.Number:
		return scalarString(v)
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", v)
	}
}

// --- request-scoped capture: a handler records before/after snapshots; the audit middleware reads
// them and computes the diff onto the event. ---

type ctxKeyDiff struct{}

// snapshotHolder is stashed in the request context by the middleware; a handler fills it.
type snapshotHolder struct {
	before, after map[string]any
	set           bool
}

// WithSnapshotCapture returns a context carrying a fresh snapshot holder + the holder (the caller —
// the middleware — keeps the holder to read after the handler runs).
func WithSnapshotCapture(ctx context.Context) (context.Context, *snapshotHolder) {
	h := &snapshotHolder{}
	return context.WithValue(ctx, ctxKeyDiff{}, h), h
}

// RecordSnapshots is called by a mutation handler with the doc state BEFORE and AFTER the change; the
// middleware diffs them into the audit event's Changes. Safe no-op when capture isn't active (e.g.
// non-admin paths) or either snapshot is nil.
func RecordSnapshots(ctx context.Context, before, after map[string]any) {
	h, _ := ctx.Value(ctxKeyDiff{}).(*snapshotHolder)
	if h == nil {
		return
	}
	h.before, h.after, h.set = before, after, true
}

// Changes returns the computed diff (nil when nothing was recorded or nothing changed).
func (h *snapshotHolder) Changes() []PropertyChange {
	if h == nil || !h.set {
		return nil
	}
	c := DiffSnapshots(h.before, h.after)
	if len(c) == 0 {
		return nil
	}
	return c
}
