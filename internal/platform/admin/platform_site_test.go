package admin

import (
	"slices"
	"testing"
)

// The blocked networks of an installation: a save adds what is missing and records it; a later save takes off the
// recorded entries the settings no longer need, and never one the operator wrote; the order of the list is kept.
func TestFence(t *testing.T) {
	need := func(want ...string) func([]string) []string {
		return func(fenced []string) []string {
			return slices.DeleteFunc(slices.Clone(want), func(c string) bool { return slices.Contains(fenced, c) })
		}
	}
	for _, tt := range []struct {
		name                                        string
		blocked, own, need                          []string
		wantNext, wantRecord, wantAdded, wantRemove []string
	}{
		{name: "a first save", blocked: []string{"op/24"}, need: []string{"a/32", "b/28"},
			wantNext: []string{"op/24", "a/32", "b/28"}, wantRecord: []string{"a/32", "b/28"}, wantAdded: []string{"a/32", "b/28"}},
		{name: "the same settings again", blocked: []string{"op/24", "a/32"}, own: []string{"a/32"}, need: []string{"a/32"},
			wantNext: []string{"op/24", "a/32"}, wantRecord: []string{"a/32"}},
		{name: "a mistyped prefix set right", blocked: []string{"lan/32", "op/24", "lan/28"}, own: []string{"lan/32", "lan/28"}, need: []string{"a/32"},
			wantNext: []string{"op/24", "a/32"}, wantRecord: []string{"a/32"}, wantAdded: []string{"a/32"}, wantRemove: []string{"lan/32", "lan/28"}},
		{name: "the operator's entry is never removed", blocked: []string{"op/24"}, need: nil,
			wantNext: []string{"op/24"}},
		{name: "an entry the operator wrote as well is not recorded", blocked: []string{"a/32"}, need: []string{"a/32"},
			wantNext: []string{"a/32"}},
		{name: "a recorded entry taken off by hand comes back when needed", blocked: []string{"op/24"}, own: []string{"a/32"}, need: []string{"a/32"},
			wantNext: []string{"op/24", "a/32"}, wantRecord: []string{"a/32"}, wantAdded: []string{"a/32"}},
		{name: "nothing is needed any more", blocked: []string{"a/32", "op/24"}, own: []string{"a/32"},
			wantNext: []string{"op/24"}, wantRemove: []string{"a/32"}},
	} {
		next, record, added, removed := fence(tt.blocked, tt.own, need(tt.need...))
		if !slices.Equal(next, tt.wantNext) || !slices.Equal(record, tt.wantRecord) || !slices.Equal(added, tt.wantAdded) || !slices.Equal(removed, tt.wantRemove) {
			t.Errorf("%s: list %v, record %v, added %v, removed %v", tt.name, next, record, added, removed)
		}
	}
}
