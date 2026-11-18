package pve

import (
	"context"
	"testing"
)

// The API access rule: written once, left alone when it is there, replaced when the settings changed, and removed by
// its marker only. Rules of an operator, of the isolation set and of another installation are never touched.
func TestAPIAccessRule(t *testing.T) {
	ops := FirewallRule{Type: "in", Action: "ACCEPT", Dest: "10.9.9.9", Comment: "ops", Enable: 1}
	drop := FirewallRule{Type: "in", Action: "DROP", Source: "10.100.0.0/16", Dest: "10.100.0.0/16", Comment: isolationComment, Enable: 1}
	other := APIAccessRule("pi-other", []string{"10.7.0.10/32"}, "10.0.0.2", "8006")
	srv, rules, writes := isolationFake(t, []FirewallRule{ops, drop, other})
	ctx := context.Background()
	src := []string{"10.200.200.10/32", "10.200.200.30/31"}
	mine := func() (out []FirewallRule) {
		for _, r := range *rules {
			if r.Comment == APIAccessComment("pi-1") {
				out = append(out, r)
			}
		}
		return out
	}

	if changed, err := srv.EnsureAPIAccess(ctx, "pi-1", src, "10.0.0.2", "8006"); err != nil || !changed {
		t.Fatalf("first write: changed %v, err %v", changed, err)
	}
	got := mine()
	if len(got) != 1 || got[0].Type != "in" || got[0].Action != "ACCEPT" || got[0].Proto != "tcp" || got[0].Source != "10.200.200.10/32,10.200.200.30/31" ||
		got[0].Dest != "10.0.0.2" || got[0].DPort != "8006" || got[0].Enable != 1 {
		t.Fatalf("rule: %+v", got)
	}
	if has, err := srv.HasAPIAccess(ctx, "pi-1", src, "10.0.0.2", "8006"); err != nil || !has {
		t.Fatalf("HasAPIAccess: %v %v", has, err)
	}

	before := *writes
	if changed, err := srv.EnsureAPIAccess(ctx, "pi-1", src, "10.0.0.2", "8006"); err != nil || changed || *writes != before {
		t.Fatalf("second call wrote: changed %v, err %v, writes %d to %d", changed, err, before, *writes)
	}

	// Changed settings: one rule, the new one; a rule disabled by hand reads as missing.
	if changed, err := srv.EnsureAPIAccess(ctx, "pi-1", src[:1], "", "443"); err != nil || !changed {
		t.Fatalf("changed settings: changed %v, err %v", changed, err)
	}
	if got := mine(); len(got) != 1 || got[0].Source != "10.200.200.10/32" || got[0].Dest != "" || got[0].DPort != "443" {
		t.Fatalf("after the change: %+v", got)
	}
	for i := range *rules {
		if (*rules)[i].Comment == APIAccessComment("pi-1") {
			(*rules)[i].Enable = 0
		}
	}
	if has, _ := srv.HasAPIAccess(ctx, "pi-1", src[:1], "", "443"); has {
		t.Fatal("a disabled rule counts as present")
	}
	if changed, err := srv.EnsureAPIAccess(ctx, "pi-1", src[:1], "", "443"); err != nil || !changed || len(mine()) != 1 || mine()[0].Enable != 1 {
		t.Fatalf("disabled by hand: changed %v, err %v, rules %+v", changed, err, mine())
	}

	if n, err := srv.RemoveAPIAccess(ctx, "pi-1"); err != nil || n != 1 || len(mine()) != 0 {
		t.Fatalf("remove: %d, %v, left %+v", n, err, mine())
	}
	if n, err := srv.RemoveAPIAccess(ctx, "pi-1"); err != nil || n != 0 {
		t.Fatalf("second remove: %d, %v", n, err)
	}
	if len(*rules) != 3 {
		t.Fatalf("foreign rules changed: %+v", *rules)
	}
	for _, want := range []FirewallRule{ops, drop, other} {
		found := false
		for _, r := range *rules {
			found = found || sameLiveRule(r, want)
		}
		if !found {
			t.Errorf("rule %q is gone", want.Comment)
		}
	}
}
