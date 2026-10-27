package pgdoc

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// M is a filter / patch map and the free-form document map used across repos:
// {"field": value} or {"field": M{"$op": value}}.
type M = map[string]any

// A is a free-form document array (the ordered-list value type used in docs).
type A = []any

// Supported operators: $eq (implicit), $ne, $in, $nin, $exists, $gt, $gte,
// $lt, $lte, $or, $and. Paths may be dotted ("a.b.c"). "_id"/"id" address the id
// column. Semantics follow the document-database conventions the repos assume:
//   - {f: nil} matches “absent or JSON null”.
//   - $ne / $nin also match documents where the field is absent.
//   - comparisons on time.Time / decimal values read the field text directly
//     and cast (::timestamptz / ::numeric) for correct ordering.
//
// Deliberately NOT supported (hand-write SQL instead): $regex, $elemMatch,
// array-containment-by-scalar-equality, $push/$inc.

type whereBuilder struct {
	args []any
}

// translateFilter renders f to a SQL boolean expression over (id, doc).
// An empty/nil filter renders "TRUE".
func translateFilter(f M, args *[]any) (string, error) {
	b := &whereBuilder{}
	b.args = *args
	sql, err := b.filter(f)
	*args = b.args
	return sql, err
}

func (b *whereBuilder) filter(f M) (string, error) {
	if len(f) == 0 {
		return "TRUE", nil
	}
	// Deterministic clause order (stable SQL for tests + plan cache).
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		v := f[k]
		switch k {
		case "$or", "$and":
			list, ok := v.([]M)
			if !ok {
				if anyList, ok2 := v.([]any); ok2 {
					list = make([]M, 0, len(anyList))
					for _, e := range anyList {
						m, ok3 := e.(M)
						if !ok3 {
							return "", fmt.Errorf("pgdoc: %s element must be a filter map", k)
						}
						list = append(list, m)
					}
				} else {
					return "", fmt.Errorf("pgdoc: %s wants []M", k)
				}
			}
			if len(list) == 0 {
				return "", fmt.Errorf("pgdoc: empty %s", k)
			}
			var subs []string
			for _, sub := range list {
				s, err := b.filter(sub)
				if err != nil {
					return "", err
				}
				subs = append(subs, "("+s+")")
			}
			joiner := " OR "
			if k == "$and" {
				joiner = " AND "
			}
			parts = append(parts, "("+strings.Join(subs, joiner)+")")
		default:
			if strings.HasPrefix(k, "$") {
				return "", fmt.Errorf("pgdoc: unsupported top-level operator %q", k)
			}
			s, err := b.field(k, v)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " AND "), nil
}

func (b *whereBuilder) field(path string, v any) (string, error) {
	if ops, ok := v.(M); ok && isOpMap(ops) {
		opKeys := make([]string, 0, len(ops))
		for op := range ops {
			opKeys = append(opKeys, op)
		}
		sort.Strings(opKeys)
		var parts []string
		for _, op := range opKeys {
			if op == "$options" {
				continue // consumed by $regex
			}
			if op == "$regex" {
				s, err := b.regexOp(path, ops[op], ops["$options"])
				if err != nil {
					return "", err
				}
				parts = append(parts, s)
				continue
			}
			s, err := b.op(path, op, ops[op])
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, " AND "), nil
	}
	return b.op(path, "$eq", v)
}

func isOpMap(m M) bool {
	for k := range m {
		return strings.HasPrefix(k, "$")
	}
	return false
}

func (b *whereBuilder) op(path, op string, v any) (string, error) {
	if path == "_id" || path == "id" {
		return b.idOp(op, v)
	}
	switch op {
	case "$eq":
		if v == nil {
			p := jsonExpr(path)
			return "(" + p + " IS NULL OR " + p + " = 'null'::jsonb)", nil
		}
		expr, arg, err := typedExpr(path, v)
		if err != nil {
			return "", err
		}
		return expr + " = " + b.bind(arg), nil
	case "$ne":
		if v == nil {
			p := jsonExpr(path)
			return "NOT (" + p + " IS NULL OR " + p + " = 'null'::jsonb)", nil
		}
		expr, arg, err := typedExpr(path, v)
		if err != nil {
			return "", err
		}
		return expr + " IS DISTINCT FROM " + b.bind(arg), nil
	case "$gt", "$gte", "$lt", "$lte":
		cmp := map[string]string{"$gt": ">", "$gte": ">=", "$lt": "<", "$lte": "<="}[op]
		expr, arg, err := typedExpr(path, v)
		if err != nil {
			return "", err
		}
		return expr + " " + cmp + " " + b.bind(arg), nil
	case "$exists":
		want, ok := v.(bool)
		if !ok {
			return "", fmt.Errorf("pgdoc: $exists wants bool")
		}
		if want {
			return jsonExpr(path) + " IS NOT NULL", nil
		}
		return jsonExpr(path) + " IS NULL", nil
	case "$in", "$nin":
		expr, arr, err := b.inList(path, v)
		if err != nil {
			return "", err
		}
		if op == "$in" {
			return expr + " = ANY(" + b.bind(arr) + ")", nil
		}
		return "(" + expr + " IS NULL OR NOT (" + expr + " = ANY(" + b.bind(arr) + ")))", nil
	case "$contains", "$elemMatch":
		// Array containment: the field is a JSON array and some element
		// matches v (object = subset match, scalar = equality). This is the
		// explicit replacement for the document-db "scalar equality matches
		// array elements" implicit behavior — rewritten repos opt in.
		frag, err := encodeValue(v)
		if err != nil {
			return "", err
		}
		return jsonExpr(path) + " @> " + b.bind("["+frag+"]") + "::jsonb", nil
	default:
		return "", fmt.Errorf("pgdoc: unsupported operator %q on %q", op, path)
	}
}

// regexOp renders $regex (+"i" option → case-insensitive).
func (b *whereBuilder) regexOp(path string, pattern, options any) (string, error) {
	p, ok := pattern.(string)
	if !ok {
		return "", fmt.Errorf("pgdoc: $regex wants string on %q", path)
	}
	op := " ~ "
	if o, _ := options.(string); strings.Contains(o, "i") {
		op = " ~* "
	}
	return textExpr(path) + op + b.bind(p), nil
}

func (b *whereBuilder) idOp(op string, v any) (string, error) {
	switch op {
	case "$eq":
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("pgdoc: _id wants string")
		}
		return "id = " + b.bind(s), nil
	case "$ne":
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("pgdoc: _id wants string")
		}
		return "id <> " + b.bind(s), nil
	case "$gt", "$gte", "$lt", "$lte":
		cmp := map[string]string{"$gt": ">", "$gte": ">=", "$lt": "<", "$lte": "<="}[op]
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("pgdoc: _id wants string")
		}
		return "id " + cmp + " " + b.bind(s), nil
	case "$in", "$nin":
		ids, err := stringList(v)
		if err != nil {
			return "", fmt.Errorf("pgdoc: _id %s: %w", op, err)
		}
		if op == "$in" {
			return "id = ANY(" + b.bind(ids) + ")", nil
		}
		return "NOT (id = ANY(" + b.bind(ids) + "))", nil
	default:
		return "", fmt.Errorf("pgdoc: unsupported operator %q on _id", op)
	}
}

// inList renders the left-hand expression + collects the array argument for
// $in/$nin. Element type comes from the first element.
func (b *whereBuilder) inList(path string, v any) (string, any, error) {
	elems := anyList(v)
	if len(elems) == 0 {
		// Match-nothing $in: keep SQL valid with an empty text array.
		return textExpr(path), []string{}, nil
	}
	switch elems[0].(type) {
	case string:
		list, err := stringList(v)
		if err != nil {
			return "", nil, err
		}
		return textExpr(path), list, nil
	case int, int32, int64, float64:
		nums := make([]float64, 0, len(elems))
		for _, e := range elems {
			switch n := e.(type) {
			case int:
				nums = append(nums, float64(n))
			case int32:
				nums = append(nums, float64(n))
			case int64:
				nums = append(nums, float64(n))
			case float64:
				nums = append(nums, n)
			default:
				return "", nil, fmt.Errorf("pgdoc: mixed $in element types")
			}
		}
		return "(" + textExpr(path) + ")::numeric", nums, nil
	default:
		return "", nil, fmt.Errorf("pgdoc: unsupported $in element type %T", elems[0])
	}
}

func anyList(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	case []string:
		out := make([]any, len(l))
		for i, s := range l {
			out[i] = s
		}
		return out
	}
	return nil
}

func stringList(v any) ([]string, error) {
	switch l := v.(type) {
	case []string:
		return l, nil
	case []any:
		out := make([]string, 0, len(l))
		for _, e := range l {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("want strings, got %T", e)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("want a string list, got %T", v)
}

// typedExpr picks the SQL extraction + cast for a comparison based on the Go
// value's type, and returns the (possibly converted) bind argument.
func typedExpr(path string, v any) (expr string, arg any, err error) {
	switch tv := v.(type) {
	case string:
		return textExpr(path), tv, nil
	case bool:
		return textExpr(path), fmt.Sprintf("%t", tv), nil
	case int:
		return "(" + textExpr(path) + ")::numeric", int64(tv), nil
	case int32:
		return "(" + textExpr(path) + ")::numeric", int64(tv), nil
	case int64:
		return "(" + textExpr(path) + ")::numeric", tv, nil
	case float64:
		return "(" + textExpr(path) + ")::numeric", tv, nil
	case time.Time:
		return "(" + textExpr(path) + ")::timestamptz", tv.UTC(), nil
	case *time.Time:
		if tv == nil {
			return "", nil, fmt.Errorf("pgdoc: nil *time.Time in comparison on %q", path)
		}
		return "(" + textExpr(path) + ")::timestamptz", tv.UTC(), nil
	case decimal.Decimal:
		return "(" + textExpr(path) + ")::numeric", tv.String(), nil
	default:
		// Named scalar types (e.g. `type BillStatus string`, `type Port int`)
		// arrive here; compare by their underlying kind, matching how the codec
		// stores them.
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.String:
			return textExpr(path), rv.String(), nil
		case reflect.Bool:
			return textExpr(path), fmt.Sprintf("%t", rv.Bool()), nil
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return "(" + textExpr(path) + ")::numeric", rv.Int(), nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return "(" + textExpr(path) + ")::numeric", rv.Uint(), nil
		case reflect.Float32, reflect.Float64:
			return "(" + textExpr(path) + ")::numeric", rv.Float(), nil
		}
		return "", nil, fmt.Errorf("pgdoc: unsupported comparison value type %T on %q", v, path)
	}
}

func (b *whereBuilder) bind(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

// textExpr extracts a path as text: doc->>'k' or doc#>>'{a,b}'.
func textExpr(path string) string {
	parts := strings.Split(path, ".")
	if len(parts) == 1 {
		return "doc->>" + quoteLit(path)
	}
	return "doc#>>" + pathLit(parts)
}

// jsonExpr extracts a path as jsonb: doc->'k' or doc#>'{a,b}'.
func jsonExpr(path string) string {
	parts := strings.Split(path, ".")
	if len(parts) == 1 {
		return "doc->" + quoteLit(path)
	}
	return "doc#>" + pathLit(parts)
}

func quoteLit(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func pathLit(parts []string) string {
	esc := make([]string, len(parts))
	for i, p := range parts {
		esc[i] = strings.ReplaceAll(strings.ReplaceAll(p, `\`, `\\`), `"`, `\"`)
	}
	return quoteLit("{" + strings.Join(esc, ",") + "}")
}
