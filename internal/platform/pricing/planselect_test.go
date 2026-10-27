package pricing

import "testing"

type memPlanSource struct {
	byID   map[string]PricePlan
	public []PricePlan
}

func (m memPlanSource) FindByID(id string) (PricePlan, bool) { pp, ok := m.byID[id]; return pp, ok }
func (m memPlanSource) PublicPricePlans() []PricePlan        { return m.public }

type memRuleSource map[string][]PricePlanRule // key = pricePlanID + "|" + timeUnit

func (m memRuleSource) RulesByPricePlanIDAndTimeUnit(id, tu string) []PricePlanRule {
	return m[id+"|"+tu]
}

func plan(id string, enabled bool, access string, providers ...string) PricePlan {
	pp := PricePlan{ID: id, Enabled: enabled, AccessMode: access}
	for _, s := range providers {
		pp.ServiceProviders = append(pp.ServiceProviders, PricePlanServiceProvider{ServiceID: s})
	}
	return pp
}

func ids(plans []PricePlan) []string {
	out := make([]string, len(plans))
	for i, p := range plans {
		out[i] = p.ID
	}
	return out
}

func eqIDs(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSelectPricePlans(t *testing.T) {
	src := memPlanSource{
		byID: map[string]PricePlan{
			"s1":       plan("s1", true, AccessScoped),
			"s2":       plan("s2", true, AccessScoped),
			"disabled": plan("disabled", false, AccessScoped),
			"pubScope": plan("pubScope", true, AccessPublic), // not SCOPED → excluded from scoped pick
		},
		public: []PricePlan{plan("pub1", true, AccessPublic), plan("pub2", true, AccessPublic)},
	}

	t.Run("not_scoped_returns_public", func(t *testing.T) {
		if got := ids(SelectPricePlans(src, nil, false)); !eqIDs(got, "pub1", "pub2") {
			t.Errorf("got %v, want [pub1 pub2]", got)
		}
	})
	t.Run("scoped_include_public", func(t *testing.T) {
		got := ids(SelectPricePlans(src, []string{"s1", "s2"}, true))
		if !eqIDs(got, "s1", "s2", "pub1", "pub2") {
			t.Errorf("got %v, want [s1 s2 pub1 pub2]", got)
		}
	})
	t.Run("scoped_exclude_public", func(t *testing.T) {
		if got := ids(SelectPricePlans(src, []string{"s1"}, false)); !eqIDs(got, "s1") {
			t.Errorf("got %v, want [s1]", got)
		}
	})
	t.Run("scoped_filters_disabled_nonscoped_missing", func(t *testing.T) {
		got := ids(SelectPricePlans(src, []string{"s1", "disabled", "pubScope", "ghost"}, false))
		if !eqIDs(got, "s1") { // disabled (not enabled), pubScope (PUBLIC), ghost (missing) all dropped
			t.Errorf("got %v, want [s1]", got)
		}
	})
}

func TestSelectPricePlansForService(t *testing.T) {
	src := memPlanSource{
		byID: map[string]PricePlan{
			"any":  plan("any", true, AccessScoped),       // no providers → matches all services
			"svcA": plan("svcA", true, AccessScoped, "A"), // only service A
		},
	}
	t.Run("includes_unscoped_and_matching", func(t *testing.T) {
		got := ids(SelectPricePlansForService(src, []string{"any", "svcA"}, false, "A"))
		if !eqIDs(got, "any", "svcA") {
			t.Errorf("got %v, want [any svcA]", got)
		}
	})
	t.Run("excludes_nonmatching_service", func(t *testing.T) {
		got := ids(SelectPricePlansForService(src, []string{"any", "svcA"}, false, "B"))
		if !eqIDs(got, "any") { // svcA scoped to A, not B
			t.Errorf("got %v, want [any]", got)
		}
	})
}

func TestApplicableRules(t *testing.T) {
	rules := memRuleSource{
		"p1|hour":  {{ID: "r1"}, {ID: "r2"}},
		"p2|hour":  {{ID: "r3"}},
		"p1|month": {{ID: "rm"}},
	}
	plans := []PricePlan{plan("p1", true, AccessPublic), plan("p2", true, AccessPublic)}
	got := ApplicableRules(plans, rules, "hour")
	if len(got) != 3 || got[0].ID != "r1" || got[1].ID != "r2" || got[2].ID != "r3" {
		t.Errorf("got %v, want [r1 r2 r3]", got)
	}
	if len(ApplicableRules(plans, rules, "minute")) != 0 {
		t.Error("no minute rules expected")
	}
}
