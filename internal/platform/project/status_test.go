package project

import "testing"

// A member's cancel and billing's resume are the two routes back to ENABLED: neither may pass through a state it does not own.
func TestCheckTransition(t *testing.T) {
	all := []string{StatusEnabled, StatusDisabled, StatusScheduledForDeletion, StatusDeleteInProgress, StatusDeleted}
	allowed := map[StatusAction][]string{
		ActionEnable:        {StatusDisabled},
		ActionDisable:       {StatusEnabled, StatusDisabled},
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

func TestAllowedWhileDisabled(t *testing.T) {
	for action, want := range map[string]bool{
		"GET_SETTINGS": true, "LIST_BACKUPS": true, "SHOW_CONSOLE_OUTPUT": true, "IS_BUCKET_PUBLIC": true, "DOWNLOAD": true,
		"STOP": true, "SHUTOFF": true, "SHUTDOWN": true, "DETACH": true, "DELETE_SNAPSHOT": true, "DELETE_OBJECT": true,
		"START": false, "REBOOT": false, "REBOOT_HARD": false, "REMOTECONTROL": false, "SERIAL_CONSOLE": false, "RESIZE": false,
		"REBUILD": false, "ATTACH": false, "EXTEND": false, "CREATE_SNAPSHOT": false, "ROLLBACK_SNAPSHOT": false,
		"RESTORE_BACKUP": false, "CREATE_BACKUP": false, "ENABLE_PROTECTION": false, "ATTACH_PUBLIC_NETWORK": false,
		"SCALE_REPLICAS": false, "SET_NODE_GROUPS": false, "CREATE_FOLDER": false, "SET_QUOTA": false, "GRANT_KEY": false,
		"UPDATE_OBJECT": false, "ENABLE_WEBSITE": false, "SOME_FUTURE_ACTION": false,
	} {
		if got := allowedWhileDisabled(action, nil); got != want {
			t.Errorf("%s: %v", action, got)
		}
	}
	if allowedWhileDisabled("INVOKEACTION", map[string]any{"action": "start"}) {
		t.Error("INVOKEACTION must be judged by what it wraps")
	}
	if !allowedWhileDisabled("INVOKEACTION", map[string]any{"action": "stop"}) {
		t.Error("a wrapped stop stays open")
	}
}

func TestApplyBillingSuspension(t *testing.T) {
	cases := []struct {
		name                 string
		p                    Project
		suspend, changed     bool
		wantStatus, wantHeld string
	}{
		{"resume of another profile leaves one on its own", Project{Status: StatusDisabled, BillingProfileID: "other"}, false, false, StatusDisabled, ""},
		{"suspend of another profile leaves one on its own", Project{Status: StatusEnabled, BillingProfileID: "other"}, true, false, StatusEnabled, ""},
		{"resume of its own profile lifts it", Project{Status: StatusDisabled, BillingProfileID: "bp"}, false, true, StatusEnabled, ""},
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
			if got := p.ApplyBillingSuspension("bp", c.suspend); got != c.changed {
				t.Errorf("changed=%v want %v", got, c.changed)
			}
			if p.Status != c.wantStatus || p.StatusBeforeDeletion != c.wantHeld {
				t.Errorf("status=%s held=%s, want %s %s", p.Status, p.StatusBeforeDeletion, c.wantStatus, c.wantHeld)
			}
		})
	}
}
