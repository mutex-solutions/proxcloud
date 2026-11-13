package externalservice

import "testing"

// The attach policy defaults to on: every provider stored before the setting existed (no key) keeps attaching new
// projects; only an explicit false turns it off.
func TestAttachToNewProjects(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  map[string]any
		want bool
	}{
		{"no config", nil, true},
		{"absent", map[string]any{"provider": "proxmox"}, true},
		{"null", map[string]any{AttachPolicyKey: nil}, true},
		{"true", map[string]any{AttachPolicyKey: true}, true},
		{"false", map[string]any{AttachPolicyKey: false}, false},
		// Never stored (the PUTs refuse it), but a hand-edited row reads as the default rather than as off.
		{"not a bool", map[string]any{AttachPolicyKey: "false"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&ExternalService{Config: tc.cfg}).AttachToNewProjects(); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateAttachPolicy(t *testing.T) {
	for _, ok := range []map[string]any{nil, {}, {AttachPolicyKey: nil}, {AttachPolicyKey: true}, {AttachPolicyKey: false}, {"provider": "x"}} {
		if err := ValidateAttachPolicy(ok); err != nil {
			t.Fatalf("%v refused: %v", ok, err)
		}
	}
	for _, bad := range []map[string]any{{AttachPolicyKey: "true"}, {AttachPolicyKey: 1.0}, {AttachPolicyKey: map[string]any{}}, {AttachPolicyKey: []any{true}}} {
		if err := ValidateAttachPolicy(bad); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}
