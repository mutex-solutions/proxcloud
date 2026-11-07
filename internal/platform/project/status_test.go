package project

import "testing"

// A member's cancel and billing's resume are the two routes back to ENABLED: neither may pass through a state it does not own.
func TestCheckTransition(t *testing.T) {
	all := []string{StatusEnabled, StatusDisabled, StatusScheduledForDeletion, StatusDeleteInProgress, StatusDeleted}
	allowed := map[StatusAction][]string{
		ActionEnable:        {StatusDisabled},
		ActionDisable:       {StatusEnabled},
		ActionSchedule:      {StatusEnabled, StatusDisabled},
		ActionCancel:        {StatusScheduledForDeletion},
		ActionDeleteNow:     {StatusEnabled, StatusDisabled, StatusScheduledForDeletion},
		ActionChangeBilling: {StatusEnabled, StatusDisabled},
	}
	for action, ok := range allowed {
		for _, st := range all {
			want := false
			for _, s := range ok {
				want = want || s == st
			}
			if got := CheckTransition(action, st) == nil; got != want {
				t.Errorf("%q from %s: allowed=%v want %v", action, st, got, want)
			}
		}
	}
	for action := range allowedFrom {
		for _, st := range []string{StatusDeleteInProgress, StatusDeleted} {
			if CheckTransition(action, st) == nil {
				t.Errorf("%q must be refused on %s", action, st)
			}
		}
	}
}

func TestStatusAfterCancel(t *testing.T) {
	for held, want := range map[string]string{StatusEnabled: StatusEnabled, StatusDisabled: StatusDisabled, "": StatusDisabled} {
		if got := StatusAfterCancel(held); got != want {
			t.Errorf("held %q: got %s want %s", held, got, want)
		}
	}
}

func TestApplyBillingSuspension(t *testing.T) {
	cases := []struct {
		name                 string
		p                    Project
		suspend, changed     bool
		wantStatus, wantHeld string
	}{
		{"suspend enabled", Project{Status: StatusEnabled}, true, true, StatusDisabled, ""},
		{"resume disabled", Project{Status: StatusDisabled}, false, true, StatusEnabled, ""},
		{"resume leaves an operator's disable", Project{Status: StatusDisabled, DisabledByOperator: true}, false, false, StatusDisabled, ""},
		{"suspend while scheduled moves what a cancel restores", Project{Status: StatusScheduledForDeletion, StatusBeforeDeletion: StatusEnabled}, true, true, StatusScheduledForDeletion, StatusDisabled},
		{"resume while scheduled moves it back", Project{Status: StatusScheduledForDeletion, StatusBeforeDeletion: StatusDisabled}, false, true, StatusScheduledForDeletion, StatusEnabled},
		{"resume while scheduled leaves an operator's disable", Project{Status: StatusScheduledForDeletion, StatusBeforeDeletion: StatusDisabled, DisabledByOperator: true}, false, false, StatusScheduledForDeletion, StatusDisabled},
		{"teardown is left alone", Project{Status: StatusDeleteInProgress}, false, false, StatusDeleteInProgress, ""},
		{"suspend teardown is left alone", Project{Status: StatusDeleteInProgress}, true, false, StatusDeleteInProgress, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.p
			if got := p.ApplyBillingSuspension(c.suspend); got != c.changed {
				t.Errorf("changed=%v want %v", got, c.changed)
			}
			if p.Status != c.wantStatus || p.StatusBeforeDeletion != c.wantHeld {
				t.Errorf("status=%s held=%s, want %s %s", p.Status, p.StatusBeforeDeletion, c.wantStatus, c.wantHeld)
			}
		})
	}
}
