package admin

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// crud.go holds the shared pgdoc helpers every admin mutation handler reuses.
// Ids are plain strings (the id column); the JSON of the domain emits `id`, never `_id`/`_class`.
// Raw documents are decoded into pgdoc.M (the pgdoc codec is the same JSON codec, so nested
// docs/arrays/decimals keep their pgdoc.M / pgdoc.A / decimal.Decimal dynamic types), and
// shapeDoc maps the stored doc back to the API shape (`_id`→`id`, drop `_class`).

// c returns the pgdoc Store for a collection.
func (r *Repo) c(collection string) *pgdoc.Store { return r.db.C(collection) }

// normFilter converts a pgdoc.M filter literal (possibly nesting pgdoc.M/pgdoc.A) into the
// pgdoc.M shape the filter translator type-switches on. Values (strings, times, decimals,
// slices of strings, …) pass through untouched.
func normFilter(f pgdoc.M) pgdoc.M {
	out := pgdoc.M{}
	for k, v := range f {
		out[k] = normVal(v)
	}
	return out
}

func normVal(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := pgdoc.M{}
		for k, val := range t {
			out[k] = normVal(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = normVal(t[i])
		}
		return out
	case []pgdoc.M:
		out := make([]any, len(t))
		for i := range t {
			out[i] = normVal(t[i])
		}
		return out
	default:
		return v
	}
}

// sortKeyFor picks the sort expression kind for a field: `_id` sorts the id column, `*At`
// fields are stored {"$date"} wrappers, `order`/`orderNumber` are numbers, everything else text.
// ponytail: suffix heuristic covers every current call site; extend the switch if a new sort field appears.
func sortKeyFor(field string, dir int) pgdoc.SortKey {
	kind := pgdoc.KText
	switch {
	case field == "_id":
	case strings.HasSuffix(field, "At"):
		kind = pgdoc.KTime
	case field == "order" || field == "orderNumber":
		kind = pgdoc.KNum
	}
	if dir < 0 {
		return pgdoc.DescK(field, kind)
	}
	return pgdoc.AscK(field, kind)
}

// shapeDoc maps a stored doc to its API JSON shape: `_id` → `id` (a plain string) and drops the
// legacy `_class` discriminator (not part of the JSON output). Mutates and returns the same map (nil-safe).
func shapeDoc(doc pgdoc.M) pgdoc.M {
	if doc == nil {
		return nil
	}
	if v, ok := doc["_id"]; ok {
		doc["id"] = v
		delete(doc, "_id")
	}
	delete(doc, "_class")
	return doc
}

// FindDoc looks up a document by id → raw pgdoc.M (still carrying `_id`, injected as a string), or
// (nil,nil) when absent. Callers shapeDoc() before writing the response.
func (r *Repo) FindDoc(ctx context.Context, collection, id string) (pgdoc.M, error) {
	var doc pgdoc.M
	found, err := r.c(collection).Get(ctx, id, &doc)
	if err != nil || !found {
		return nil, err
	}
	return doc, nil
}

// FindOneBy finds the first document matching filter → raw pgdoc.M, or (nil,nil) when none.
func (r *Repo) FindOneBy(ctx context.Context, collection string, filter pgdoc.M) (pgdoc.M, error) {
	var doc pgdoc.M
	found, err := r.c(collection).FindOne(ctx, normFilter(filter), &doc)
	if err != nil || !found {
		return nil, err
	}
	return doc, nil
}

// ListSorted lists a whole collection sorted by one field (dir 1 asc / -1 desc), never nil.
func (r *Repo) ListSorted(ctx context.Context, collection, sortField string, dir int) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := r.c(collection).Find(ctx, nil, &out, pgdoc.Sort(sortKeyFor(sortField, dir))); err != nil {
		return nil, err
	}
	return out, nil
}

// InsertDoc inserts a new document (pgdoc assigns the id) and returns it with
// `_id` set. Any caller-supplied `id`/`_id` is dropped so the store generates the key (save with a
// null id). Callers shapeDoc() the result.
// InsertDocKeepID inserts a document PRESERVING the caller's string `_id` (no id is
// generated). Used where the id is meaningful and looked up verbatim later — e.g. hmac_keys
// (`_id` = the "pk<md5>" SigV4 access-key id the verifier resolves by).
func (r *Repo) InsertDocKeepID(ctx context.Context, collection string, doc pgdoc.M) error {
	_, err := r.c(collection).InsertOne(ctx, doc)
	return err
}

func (r *Repo) InsertDoc(ctx context.Context, collection string, doc pgdoc.M) (pgdoc.M, error) {
	delete(doc, "id")
	delete(doc, "_id")
	id, err := r.c(collection).InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	doc["_id"] = id
	return doc, nil
}

// ReplaceDoc replaces a document by id (id-preserving — the id column is the key). The
// caller's map is not mutated (id/_id/_class are stripped from a copy before the replace).
func (r *Repo) ReplaceDoc(ctx context.Context, collection, id string, doc pgdoc.M) error {
	cp := pgdoc.M{}
	for k, v := range doc {
		if k == "id" || k == "_id" || k == "_class" {
			continue
		}
		cp[k] = v
	}
	_, err := r.c(collection).Replace(ctx, id, cp)
	return err
}

// SetFields applies `$set` to a document by id; returns the matched count (0 → not found).
func (r *Repo) SetFields(ctx context.Context, collection, id string, set pgdoc.M) (int64, error) {
	ok, err := r.c(collection).SetByID(ctx, id, set, nil)
	if err != nil {
		return 0, err
	}
	if ok {
		return 1, nil
	}
	return 0, nil
}

// SetAndUnsetFields applies `$set` + `$unset` in one update — the faithful shape for an
// entity-save that nulls a field (null fields are dropped from the stored doc, not
// stored as literal nulls; a stored null would serialize as `"field":null` and break null-omission).
func (r *Repo) SetAndUnsetFields(ctx context.Context, collection, id string, set, unset pgdoc.M) (int64, error) {
	keys := make([]string, 0, len(unset))
	for k := range unset {
		keys = append(keys, k)
	}
	ok, err := r.c(collection).SetByID(ctx, id, set, keys)
	if err != nil {
		return 0, err
	}
	if ok {
		return 1, nil
	}
	return 0, nil
}

// CountBy counts documents matching filter (the faithful "is it referenced?" guard some deletes need).
func (r *Repo) CountBy(ctx context.Context, collection string, filter pgdoc.M) (int64, error) {
	return r.c(collection).Count(ctx, normFilter(filter))
}

// DeleteDoc deletes a document by id; returns the deleted count (0 → not found).
func (r *Repo) DeleteDoc(ctx context.Context, collection, id string) (int64, error) {
	ok, err := r.c(collection).DeleteByID(ctx, id)
	if err != nil {
		return 0, err
	}
	if ok {
		return 1, nil
	}
	return 0, nil
}

// asInt reads a stored numeric value (int32/int64/float64/int or a json.Number that survived a
// request decode) as an int; 0 when absent or non-numeric.
func asInt(v any) int {
	switch n := v.(type) {
	case int32:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
		if f, err := n.Float64(); err == nil {
			return int(f)
		}
		return 0
	default:
		return 0
	}
}
