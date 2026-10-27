package paging

import (
	"net/url"
	"testing"
)

func idOf(s string) string { return s }

func ptr(s string) *string { return &s }

func eqPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// trimForward: DESC page fetched with limit+1.
func TestTrimForward(t *testing.T) {
	cases := []struct {
		name     string
		docs     []string
		limit    int
		hasAfter bool
		want     []string
		next     *string
		prev     *string
	}{
		{"full page has next", []string{"e", "d", "c", "b", "a"}, 4, false, []string{"e", "d", "c", "b"}, ptr("b"), nil},
		{"last page no next", []string{"c", "b", "a"}, 4, false, []string{"c", "b", "a"}, nil, nil},
		{"exact fill no next", []string{"d", "c", "b", "a"}, 4, false, []string{"d", "c", "b", "a"}, nil, nil},
		{"page 2 carries prev", []string{"e", "d", "c", "b", "a"}, 4, true, []string{"e", "d", "c", "b"}, ptr("b"), ptr("e")},
		{"empty", []string{}, 4, true, []string{}, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, next, prev := trimForward(c.docs, c.limit, c.hasAfter, idOf)
			if len(got) != len(c.want) {
				t.Fatalf("len=%d want %d (%v)", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("row %d = %q want %q", i, got[i], c.want[i])
				}
			}
			if !eqPtr(next, c.next) {
				t.Fatalf("next=%v want %v", next, c.next)
			}
			if !eqPtr(prev, c.prev) {
				t.Fatalf("prev=%v want %v", prev, c.prev)
			}
		})
	}
}

// trimBackward: ASC page (Before cursor) reversed to DESC.
func TestTrimBackward(t *testing.T) {
	// ids ascending as fetched: a,b,c,d,e ; limit 4 → keep 4 closest to cursor (b,c,d,e), reverse → e,d,c,b
	got, next, prev := trimBackward([]string{"a", "b", "c", "d", "e"}, 4, idOf)
	want := []string{"e", "d", "c", "b"}
	if len(got) != 4 {
		t.Fatalf("len=%d want 4 (%v)", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q want %q", i, got[i], want[i])
		}
	}
	if !eqPtr(next, ptr("b")) {
		t.Fatalf("next=%v want b", next)
	}
	if !eqPtr(prev, ptr("e")) { // hasMore → there is a page above
		t.Fatalf("prev=%v want e", prev)
	}

	// no overflow: ascending a,b,c (< limit) → reversed c,b,a ; no prev (top reached)
	got2, next2, prev2 := trimBackward([]string{"a", "b", "c"}, 4, idOf)
	if len(got2) != 3 || got2[0] != "c" || got2[2] != "a" {
		t.Fatalf("got2=%v", got2)
	}
	if !eqPtr(next2, ptr("a")) {
		t.Fatalf("next2=%v want a", next2)
	}
	if prev2 != nil {
		t.Fatalf("prev2=%v want nil", prev2)
	}
}

// KeysetSlice: in-memory keyset over an unsorted slice (live-cloud lists).
func TestKeysetSlice(t *testing.T) {
	items := []string{"b", "e", "a", "d", "c"} // unsorted; DESC order = e,d,c,b,a

	// page 1 (no cursor): limit 2 → e,d ; next=d
	p1, next1, prev1 := KeysetSlice(items, Params{Limit: 2}, idOf)
	if len(p1) != 2 || p1[0] != "e" || p1[1] != "d" {
		t.Fatalf("page1=%v", p1)
	}
	if !eqPtr(next1, ptr("d")) || prev1 != nil {
		t.Fatalf("next1=%v prev1=%v", next1, prev1)
	}

	// page 2 (after=d): id<d → c,b,a ; limit 2 → c,b ; next=b ; prev=c
	p2, next2, prev2 := KeysetSlice(items, Params{Limit: 2, After: "d"}, idOf)
	if len(p2) != 2 || p2[0] != "c" || p2[1] != "b" {
		t.Fatalf("page2=%v", p2)
	}
	if !eqPtr(next2, ptr("b")) || !eqPtr(prev2, ptr("c")) {
		t.Fatalf("next2=%v prev2=%v", next2, prev2)
	}

	// last page (after=b): id<b → a ; next=nil (no more)
	p3, next3, _ := KeysetSlice(items, Params{Limit: 2, After: "b"}, idOf)
	if len(p3) != 1 || p3[0] != "a" {
		t.Fatalf("page3=%v", p3)
	}
	if next3 != nil {
		t.Fatalf("next3=%v want nil", next3)
	}
}

func TestParse(t *testing.T) {
	must := func(raw string) Params {
		p, err := Parse(mustValues(t, raw))
		if err != nil {
			t.Fatalf("Parse(%q) err: %v", raw, err)
		}
		return p
	}

	if p := must(""); p.Active || p.Limit != DefaultLimit {
		t.Fatalf("empty: active=%v limit=%d (want inactive, default)", p.Active, p.Limit)
	}
	if p := must("limit=25"); !p.Active || p.Limit != 25 {
		t.Fatalf("limit=25: %+v", p)
	}
	if p := must("limit=99999"); p.Limit != MaxLimit {
		t.Fatalf("clamp: limit=%d want %d", p.Limit, MaxLimit)
	}
	if p := must("after=abc"); !p.Active || p.After != "abc" {
		t.Fatalf("after: %+v", p)
	}
	if p := must("offset=100"); !p.Active || p.Offset != 100 {
		t.Fatalf("offset: %+v", p)
	}
	if p := must("search=alice"); p.Search != "alice" || p.Active {
		t.Fatalf("search alone must not activate paging: %+v", p)
	}
	if _, err := Parse(mustValues(t, "limit=abc")); err == nil {
		t.Fatal("non-numeric limit must error")
	}
	if _, err := Parse(mustValues(t, "offset=-1")); err == nil {
		t.Fatal("negative offset must error")
	}
}

func mustValues(t *testing.T, raw string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
