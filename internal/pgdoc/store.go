package pgdoc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Store is one document table: (id text primary key, doc jsonb not null).
type Store struct {
	db    *DB
	table string
}

func (s *Store) Name() string { return s.table }

// ident quotes the table name (collection names carry camelCase).
func (s *Store) ident() string {
	return `"` + strings.ReplaceAll(s.table, `"`, `""`) + `"`
}

// Ensure creates the table if missing (documents appear on first write, like
// the previous datastore's implicit collections).
func (s *Store) Ensure(ctx context.Context) error {
	_, err := s.db.q(ctx).Exec(ctx,
		"CREATE TABLE IF NOT EXISTS "+s.ident()+" (id text PRIMARY KEY, doc jsonb NOT NULL)")
	return err
}

// undefined-table (42P01) → auto-create + retry once, mirroring implicit
// collection creation. Skipped inside a transaction (aborted after any error).
func (s *Store) canAutoCreate(ctx context.Context, err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42P01" {
		return false
	}
	if _, inTx := ctx.Value(txKey{}).(pgx.Tx); inTx {
		return false
	}
	return s.Ensure(ctx) == nil
}

func (s *Store) exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := s.db.q(ctx).Exec(ctx, sql, args...)
	if err != nil && s.canAutoCreate(ctx, err) {
		tag, err = s.db.q(ctx).Exec(ctx, sql, args...)
	}
	return tag, err
}

func (s *Store) rawQuery(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := s.db.q(ctx).Query(ctx, sql, args...)
	if err != nil && s.canAutoCreate(ctx, err) {
		rows, err = s.db.q(ctx).Query(ctx, sql, args...)
	}
	return rows, err
}

// scanRow runs a single-row query; missing-table behaves as no-rows.
func (s *Store) scanRow(ctx context.Context, sql string, args []any, dest ...any) error {
	err := s.db.q(ctx).QueryRow(ctx, sql, args...).Scan(dest...)
	if err != nil && s.canAutoCreate(ctx, err) {
		err = s.db.q(ctx).QueryRow(ctx, sql, args...).Scan(dest...)
	}
	return err
}

// Kind selects the index/sort expression shape for a field.
type Kind int

const (
	KText Kind = iota // doc->>'f'
	KNum              // (doc->>'f')::numeric
	KTime             // pgdoc_ts(doc->>'f')  (RFC3339 → timestamptz, immutable)
	KDec              // (doc->>'f')::numeric  (money is a decimal string)
)

func kindExpr(field string, k Kind) string {
	switch k {
	case KNum, KDec:
		// Money is stored as a decimal STRING; doc->>'f' extracts its text form and
		// ::numeric parses it at full precision, so the same cast serves KNum and KDec.
		return "((" + textExpr(field) + ")::numeric)"
	case KTime:
		// Time is an RFC3339 string. A raw ::timestamptz cast is NOT IMMUTABLE
		// (it depends on the TimeZone GUC) and Postgres rejects non-immutable
		// expressions in an index; pgdoc_ts() (a migration-defined IMMUTABLE
		// wrapper, safe because RFC3339 strings carry their own offset) casts
		// for both indexing and sorting. Range comparisons (typedExpr) cast
		// inline — a WHERE clause has no immutability requirement.
		return "pgdoc_ts(" + textExpr(field) + ")"
	default:
		return "(" + textExpr(field) + ")"
	}
}

// IndexField is one component of an expression index.
type IndexField struct {
	Field string
	Kind  Kind
}

// F is shorthand for a text index field.
func F(field string) IndexField { return IndexField{Field: field} }

// EnsureIndex creates an expression index (replaces the driver-level
// EnsureIndexes bootstrap; runs at startup, idempotent).
func (s *Store) EnsureIndex(ctx context.Context, name string, unique bool, fields ...IndexField) error {
	if len(fields) == 0 {
		return fmt.Errorf("pgdoc: EnsureIndex %s: no fields", name)
	}
	exprs := make([]string, len(fields))
	for i, f := range fields {
		exprs[i] = kindExpr(f.Field, f.Kind)
	}
	u := ""
	if unique {
		u = "UNIQUE "
	}
	idx := `"` + strings.ReplaceAll(s.table+"_"+name, `"`, `""`) + `"`
	_, err := s.db.q(ctx).Exec(ctx, "CREATE "+u+"INDEX IF NOT EXISTS "+idx+
		" ON "+s.ident()+" ("+strings.Join(exprs, ", ")+")")
	return err
}

// --- writes ---

// InsertOne stores v as a new document. When v's _id is empty a new id is
// generated. Returns the id (callers assign it back to the struct).
func (s *Store) InsertOne(ctx context.Context, v any) (string, error) {
	body, id, err := Marshal(v)
	if err != nil {
		return "", err
	}
	if id == "" {
		id = NewID()
	}
	_, err = s.exec(ctx, "INSERT INTO "+s.ident()+" (id, doc) VALUES ($1, $2)", id, body)
	if err != nil {
		return "", err
	}
	return id, nil
}

// Replace fully replaces the document with the given id. Returns false when
// no such document exists.
func (s *Store) Replace(ctx context.Context, id string, v any) (bool, error) {
	body, _, err := Marshal(v)
	if err != nil {
		return false, err
	}
	tag, err := s.exec(ctx, "UPDATE "+s.ident()+" SET doc = $2 WHERE id = $1", id, body)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Upsert inserts or fully replaces the document with the given id.
func (s *Store) Upsert(ctx context.Context, id string, v any) error {
	body, _, err := Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, "INSERT INTO "+s.ident()+
		" (id, doc) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET doc = EXCLUDED.doc", id, body)
	return err
}

// setUnsetSQL builds "doc = …" for a $set/$unset-style partial update.
// Top-level set keys merge via ||; dotted set keys use jsonb_set (parents must
// exist — same constraint the repos already respected). Unset removes keys
// (top-level via -, dotted via #-). Set values marshal to the stored form.
func (s *Store) setUnsetSQL(set M, unset []string, args *[]any) (string, error) {
	expr := "doc"
	flat := M{}
	for k, v := range set {
		if strings.Contains(k, ".") {
			raw, err := encodeValue(v)
			if err != nil {
				return "", err
			}
			*args = append(*args, raw)
			expr = "jsonb_set(" + expr + ", " + pathLit(strings.Split(k, ".")) +
				", $" + fmt.Sprint(len(*args)) + "::jsonb, true)"
		} else {
			flat[k] = v
		}
	}
	if len(flat) > 0 {
		b, err := marshalPatch(flat)
		if err != nil {
			return "", err
		}
		*args = append(*args, string(b))
		expr = "(" + expr + " || $" + fmt.Sprint(len(*args)) + "::jsonb)"
	}
	var flatUnset []string
	for _, k := range unset {
		if strings.Contains(k, ".") {
			expr = "(" + expr + " #- " + pathLit(strings.Split(k, ".")) + ")"
		} else {
			flatUnset = append(flatUnset, k)
		}
	}
	if len(flatUnset) > 0 {
		*args = append(*args, flatUnset)
		expr = "(" + expr + " - $" + fmt.Sprint(len(*args)) + "::text[])"
	}
	return expr, nil
}

// SetFields applies a partial update ($set/$unset) to every matching
// document; returns the number updated.
func (s *Store) SetFields(ctx context.Context, filter M, set M, unset []string) (int64, error) {
	var args []any
	docExpr, err := s.setUnsetSQL(set, unset, &args)
	if err != nil {
		return 0, err
	}
	where, err := translateFilter(filter, &args)
	if err != nil {
		return 0, err
	}
	tag, err := s.exec(ctx, "UPDATE "+s.ident()+" SET doc = "+docExpr+" WHERE "+where, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SetFieldsOne applies a partial update to (at most) one matching document.
func (s *Store) SetFieldsOne(ctx context.Context, filter M, set M, unset []string) (bool, error) {
	var args []any
	docExpr, err := s.setUnsetSQL(set, unset, &args)
	if err != nil {
		return false, err
	}
	where, err := translateFilter(filter, &args)
	if err != nil {
		return false, err
	}
	tag, err := s.exec(ctx, "UPDATE "+s.ident()+" SET doc = "+docExpr+
		" WHERE id IN (SELECT id FROM "+s.ident()+" WHERE "+where+" LIMIT 1)", args...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetByID applies a partial update to the document with the given id.
func (s *Store) SetByID(ctx context.Context, id string, set M, unset []string) (bool, error) {
	return s.SetFieldsOne(ctx, M{"_id": id}, set, unset)
}

// DeleteByID removes one document; reports whether it existed.
func (s *Store) DeleteByID(ctx context.Context, id string) (bool, error) {
	tag, err := s.exec(ctx, "DELETE FROM "+s.ident()+" WHERE id = $1", id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteMany removes every matching document; returns the count.
func (s *Store) DeleteMany(ctx context.Context, filter M) (int64, error) {
	var args []any
	where, err := translateFilter(filter, &args)
	if err != nil {
		return 0, err
	}
	tag, err := s.exec(ctx, "DELETE FROM "+s.ident()+" WHERE "+where, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteOne removes (at most) one matching document.
func (s *Store) DeleteOne(ctx context.Context, filter M) (bool, error) {
	var args []any
	where, err := translateFilter(filter, &args)
	if err != nil {
		return false, err
	}
	tag, err := s.exec(ctx, "DELETE FROM "+s.ident()+
		" WHERE id IN (SELECT id FROM "+s.ident()+" WHERE "+where+" LIMIT 1)", args...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// --- reads ---

// Get loads the document with the given id into out; false when absent.
// A malformed/empty id is “not found” (mirrors the old id-coercion leniency).
func (s *Store) Get(ctx context.Context, id string, out any) (bool, error) {
	if id == "" {
		return false, nil
	}
	var body []byte
	err := s.scanRow(ctx, "SELECT doc FROM "+s.ident()+" WHERE id = $1", []any{id}, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, Unmarshal(body, id, out)
}

// FindOne loads the first matching document into out; false when none match.
// Without an explicit sort it picks the lowest id (≈ oldest) so the previously
// unordered "first document" reads become deterministic.
func (s *Store) FindOne(ctx context.Context, filter M, out any, opts ...FindOpt) (bool, error) {
	o := applyOpts(opts)
	o.limit = 1
	if len(o.sort) == 0 {
		o.sort = []SortKey{{Field: "_id"}}
	}
	rows, err := s.query(ctx, filter, o)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, rows.Err()
	}
	var id string
	var body []byte
	if err := rows.Scan(&id, &body); err != nil {
		return false, err
	}
	return true, Unmarshal(body, id, out)
}

// Find loads every matching document into out (*[]T or *[]*T).
func (s *Store) Find(ctx context.Context, filter M, out any, opts ...FindOpt) error {
	o := applyOpts(opts)
	rows, err := s.query(ctx, filter, o)
	if err != nil {
		return err
	}
	defer rows.Close()

	slicePtr := reflect.ValueOf(out)
	if slicePtr.Kind() != reflect.Ptr || slicePtr.Elem().Kind() != reflect.Slice {
		return fmt.Errorf("pgdoc: Find wants *[]T, got %T", out)
	}
	sliceVal := slicePtr.Elem()
	elemT := sliceVal.Type().Elem()
	isPtr := elemT.Kind() == reflect.Ptr
	baseT := elemT
	if isPtr {
		baseT = elemT.Elem()
	}
	acc := reflect.MakeSlice(sliceVal.Type(), 0, 8)
	for rows.Next() {
		var id string
		var body []byte
		if err := rows.Scan(&id, &body); err != nil {
			return err
		}
		item := reflect.New(baseT)
		if err := Unmarshal(body, id, item.Interface()); err != nil {
			return err
		}
		if isPtr {
			acc = reflect.Append(acc, item)
		} else {
			acc = reflect.Append(acc, item.Elem())
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sliceVal.Set(acc)
	return nil
}

// Count returns the number of matching documents.
func (s *Store) Count(ctx context.Context, filter M) (int64, error) {
	var args []any
	where, err := translateFilter(filter, &args)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.scanRow(ctx, "SELECT count(*) FROM "+s.ident()+" WHERE "+where, args, &n)
	return n, err
}

// Exists reports whether any document matches.
func (s *Store) Exists(ctx context.Context, filter M) (bool, error) {
	var args []any
	where, err := translateFilter(filter, &args)
	if err != nil {
		return false, err
	}
	var ok bool
	err = s.scanRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+s.ident()+" WHERE "+where+")", args, &ok)
	return ok, err
}

func (s *Store) query(ctx context.Context, filter M, o findOpts) (pgx.Rows, error) {
	var args []any
	where, err := translateFilter(filter, &args)
	if err != nil {
		return nil, err
	}
	sql := "SELECT id, doc FROM " + s.ident() + " WHERE " + where
	if len(o.sort) > 0 {
		var terms []string
		for _, k := range o.sort {
			expr := kindExpr(k.Field, k.Kind)
			if k.Field == "_id" {
				expr = "id"
			}
			dir := " ASC"
			if k.Desc {
				dir = " DESC"
			}
			terms = append(terms, expr+dir)
		}
		sql += " ORDER BY " + strings.Join(terms, ", ")
	}
	if o.limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", o.limit)
	}
	if o.skip > 0 {
		sql += fmt.Sprintf(" OFFSET %d", o.skip)
	}
	return s.rawQuery(ctx, sql, args...)
}

// GetForUpdate loads and row-locks the document (use inside WithTx — the
// replacement for the single-document atomic claim/OCC patterns).
func (s *Store) GetForUpdate(ctx context.Context, id string, out any) (bool, error) {
	if id == "" {
		return false, nil
	}
	var body []byte
	err := s.scanRow(ctx, "SELECT doc FROM "+s.ident()+" WHERE id = $1 FOR UPDATE", []any{id}, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, Unmarshal(body, id, out)
}

// FindOneForUpdate loads and row-locks the first matching document.
func (s *Store) FindOneForUpdate(ctx context.Context, filter M, out any) (string, bool, error) {
	var args []any
	where, err := translateFilter(filter, &args)
	if err != nil {
		return "", false, err
	}
	var id string
	var body []byte
	err = s.scanRow(ctx, "SELECT id, doc FROM "+s.ident()+" WHERE "+where+
		" ORDER BY id LIMIT 1 FOR UPDATE", args, &id, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, Unmarshal(body, id, out)
}

// Distinct returns the distinct non-null text values of a field.
func (s *Store) Distinct(ctx context.Context, field string, filter M) ([]string, error) {
	var args []any
	where, err := translateFilter(filter, &args)
	if err != nil {
		return nil, err
	}
	expr := textExpr(field)
	rows, err := s.rawQuery(ctx, "SELECT DISTINCT "+expr+" FROM "+s.ident()+
		" WHERE "+where+" AND "+expr+" IS NOT NULL ORDER BY 1", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PushToArray appends value to the array field of every matching document
// (missing field becomes a one-element array).
func (s *Store) PushToArray(ctx context.Context, filter M, field string, value any) (int64, error) {
	frag, err := encodeValue(value)
	if err != nil {
		return 0, err
	}
	var args []any
	args = append(args, frag)
	path := pathLit(strings.Split(field, "."))
	docExpr := "jsonb_set(doc, " + path + ", coalesce(" + jsonExpr(field) +
		", '[]'::jsonb) || jsonb_build_array($1::jsonb), true)"
	where, err := translateFilter(filter, &args)
	if err != nil {
		return 0, err
	}
	tag, err := s.exec(ctx, "UPDATE "+s.ident()+" SET doc = "+docExpr+" WHERE "+where, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PullFromArray removes every element matching the given subset (object
// fields) from the array field of every matching document.
func (s *Store) PullFromArray(ctx context.Context, filter M, field string, match M) (int64, error) {
	frag, err := encodeValue(match)
	if err != nil {
		return 0, err
	}
	var args []any
	args = append(args, frag)
	path := pathLit(strings.Split(field, "."))
	docExpr := "jsonb_set(doc, " + path + ", coalesce((SELECT jsonb_agg(e) FROM jsonb_array_elements(" +
		jsonExpr(field) + ") e WHERE NOT (e @> $1::jsonb)), '[]'::jsonb), false)"
	where, err := translateFilter(filter, &args)
	if err != nil {
		return 0, err
	}
	tag, err := s.exec(ctx, "UPDATE "+s.ident()+" SET doc = "+docExpr+
		" WHERE ("+where+") AND "+jsonExpr(field)+" IS NOT NULL", args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// --- find options ---

// SortKey orders results by a document field (Kind picks the cast; "_id"
// sorts on the id column).
type SortKey struct {
	Field string
	Desc  bool
	Kind  Kind
}

type findOpts struct {
	sort  []SortKey
	limit int64
	skip  int64
}

type FindOpt func(*findOpts)

func Sort(keys ...SortKey) FindOpt       { return func(o *findOpts) { o.sort = keys } }
func Asc(field string) SortKey           { return SortKey{Field: field} }
func Desc(field string) SortKey          { return SortKey{Field: field, Desc: true} }
func AscK(field string, k Kind) SortKey  { return SortKey{Field: field, Kind: k} }
func DescK(field string, k Kind) SortKey { return SortKey{Field: field, Desc: true, Kind: k} }
func Limit(n int64) FindOpt              { return func(o *findOpts) { o.limit = n } }
func Skip(n int64) FindOpt               { return func(o *findOpts) { o.skip = n } }

func applyOpts(opts []FindOpt) findOpts {
	var o findOpts
	for _, f := range opts {
		f(&o)
	}
	return o
}
