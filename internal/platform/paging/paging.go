// Package paging is the shared BE-driven pagination helper for the client + admin
// list surfaces. It parses the paging query params, runs keyset (cursor on _id,
// newest-first) or offset (limit/skip/count) pgdoc reads, and leaves emission to
// pkg/httpx (CursorList / Page — the camelCase envelope the FE already parses).
//
// Back-compat: Params.Active is false when the request carries NO paging param, so
// a handler can keep its current full-list behavior until the FE opts in by sending
// ?limit=/?after=/?offset=. This lets BE + FE convert one surface at a time.
package paging

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const (
	DefaultLimit = 50  // page size when ?limit is absent
	MaxLimit     = 200 // ?limit is clamped down to this
)

// Params is the parsed paging/query state.
type Params struct {
	Limit  int
	After  string // keyset forward cursor: _id < After (DESC / newest-first)
	Before string // keyset backward cursor: _id > Before
	Offset int64  // offset mode
	Search string
	Active bool // any of limit/after/before/offset was present → paginate
}

// Parse reads the paging params from a query string. Pure (no http) so it is unit
// testable; FromRequest wraps it for handlers. A non-numeric limit/offset is an error.
func Parse(q url.Values) (Params, error) {
	p := Params{Limit: DefaultLimit, After: q.Get("after"), Before: q.Get("before"), Search: q.Get("search")}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return p, httpx.BadRequest(`invalid "limit": ` + s)
		}
		if n > 0 {
			p.Limit = n
		}
		p.Active = true
	}
	if p.Limit > MaxLimit {
		p.Limit = MaxLimit
	}
	if s := q.Get("offset"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return p, httpx.BadRequest(`invalid "offset": ` + s)
		}
		p.Offset = n
		p.Active = true
	}
	if p.After != "" || p.Before != "" {
		p.Active = true
	}
	return p, nil
}

// FromRequest parses paging params off the request; on a bad param it writes a 400
// and returns ok=false.
func FromRequest(w http.ResponseWriter, r *http.Request) (Params, bool) {
	p, err := Parse(r.URL.Query())
	if err != nil {
		httpx.WriteError(w, err)
		return p, false
	}
	return p, true
}

// Keyset runs a cursor page over col (sort _id DESC = newest-first). Forward via
// After (_id < cursor), backward via Before (_id > cursor, fetched ASC then reversed).
// idOf extracts a document's _id. Returns the page + next/prev markers (nil = no more).
// The caller's filter is not mutated.
func Keyset[T any](ctx context.Context, col *pgdoc.Store, filter pgdoc.M, p Params, idOf func(T) string) ([]T, *string, *string, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	fetch := int64(limit + 1)

	if p.Before != "" {
		q := cloneM(filter)
		q["_id"] = pgdoc.M{"$gt": p.Before}
		var docs []T
		if err := col.Find(ctx, q, &docs, pgdoc.Sort(pgdoc.Asc("_id")), pgdoc.Limit(fetch)); err != nil {
			return nil, nil, nil, err
		}
		docs, next, prev := trimBackward(docs, limit, idOf)
		return docs, next, prev, nil
	}

	q := cloneM(filter)
	if p.After != "" {
		q["_id"] = pgdoc.M{"$lt": p.After}
	}
	var docs []T
	if err := col.Find(ctx, q, &docs, pgdoc.Sort(pgdoc.Desc("_id")), pgdoc.Limit(fetch)); err != nil {
		return nil, nil, nil, err
	}
	docs, next, prev := trimForward(docs, limit, p.After != "", idOf)
	return docs, next, prev, nil
}

// trimForward trims a DESC page fetched with limit+1: the +1 sentinel's id becomes
// next; prev is the first row's id when paging past the first page (After set).
func trimForward[T any](docs []T, limit int, hasAfter bool, idOf func(T) string) ([]T, *string, *string) {
	var next, prev *string
	if len(docs) > limit {
		id := idOf(docs[limit-1])
		next = &id
		docs = docs[:limit]
	}
	if hasAfter && len(docs) > 0 {
		id := idOf(docs[0])
		prev = &id
	}
	return docs, next, prev
}

// trimBackward trims an ASC page (Before cursor) and reverses it back to DESC order.
func trimBackward[T any](docs []T, limit int, idOf func(T) string) ([]T, *string, *string) {
	hasMore := len(docs) > limit
	if hasMore {
		docs = docs[len(docs)-limit:] // keep the `limit` closest to the cursor
	}
	reverse(docs)
	var next, prev *string
	if hasMore && len(docs) > 0 {
		id := idOf(docs[0])
		prev = &id
	}
	if len(docs) > 0 {
		id := idOf(docs[len(docs)-1])
		next = &id
	}
	return docs, next, prev
}

// Offset runs an offset page over col (Find + Limit + Skip) plus a Count for the
// total. sort defaults to _id DESC (newest-first) when empty.
func Offset[T any](ctx context.Context, col *pgdoc.Store, filter pgdoc.M, sort []pgdoc.SortKey, p Params) ([]T, int64, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if len(sort) == 0 {
		sort = []pgdoc.SortKey{pgdoc.Desc("_id")}
	}
	var out []T
	if err := col.Find(ctx, filter, &out, pgdoc.Sort(sort...), pgdoc.Limit(int64(limit)), pgdoc.Skip(p.Offset)); err != nil {
		return nil, 0, err
	}
	total, err := col.Count(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// KeysetSlice paginates an already-materialized slice by a cursor on idOf, sorting DESC by id
// (newest-first when ids are time-prefixed). Forward-only "Load more": After keeps id < cursor.
// For live-cloud lists the provider returns the whole collection per call, so this pages it in
// memory (cheap — already materialized; tolerate cross-page drift, same as the audit UX).
func KeysetSlice[T any](items []T, p Params, idOf func(T) string) ([]T, *string, *string) {
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	sorted := make([]T, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool { return idOf(sorted[i]) > idOf(sorted[j]) })
	if p.After != "" {
		kept := make([]T, 0, len(sorted))
		for _, it := range sorted {
			if idOf(it) < p.After {
				kept = append(kept, it)
			}
		}
		sorted = kept
	}
	var next, prev *string
	if p.After != "" && len(sorted) > 0 {
		id := idOf(sorted[0])
		prev = &id
	}
	if len(sorted) > limit {
		id := idOf(sorted[limit-1])
		next = &id
		sorted = sorted[:limit]
	}
	return sorted, next, prev
}

// OffsetPaging builds the httpx.Paging for an offset page.
func OffsetPaging(p Params, total int64) httpx.Paging {
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	off := p.Offset
	return httpx.Paging{Limit: &limit, Offset: &off, Total: &total}
}

func cloneM(m pgdoc.M) pgdoc.M {
	// Capacity is just a hint (the map grows as needed); use len(m) directly — len(m)+1 trips
	// CodeQL's allocation-size-overflow and buys nothing.
	out := make(pgdoc.M, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
