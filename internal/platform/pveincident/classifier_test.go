package pveincident

import (
	"testing"
	"time"
)

func trusted() SnapshotResult {
	return SnapshotResult{Trust: SourceTrust{Verified, Verified, Verified, Verified}, LinksComplete: true}
}
func findObservation(t *testing.T, x SnapshotResult, kind string, now time.Time) observation {
	t.Helper()
	facts, err := classify(x, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range facts {
		if o.kind == kind {
			return o
		}
	}
	t.Fatalf("no %s fact", kind)
	return observation{}
}
func TestClassifyHonestUnknownAndIndependentSources(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	for _, trust := range []Trust{Untrusted, Unreachable, Malformed} {
		x := trusted()
		x.Trust.VMs = trust
		x.Nodes = []NodeFact{{"node1", Offline}}
		if o := findObservation(t, x, KindNode, now); o.health != -1 {
			t.Fatalf("VM visibility suppressed independently verified node: %+v", o)
		}
		x.Trust.Nodes = trust
		if o := findObservation(t, x, KindNode, now); o.health != 0 {
			t.Fatalf("untrusted node proved down: %+v", o)
		}
	}
	x := trusted()
	x.Nodes = []NodeFact{{"node1", NodeUnknown}}
	if o := findObservation(t, x, KindNode, now); o.health != 0 {
		t.Fatal(o)
	}
	x.Trust.Provider = Unreachable
	if o := findObservation(t, x, KindProvider, now); o.health != -1 {
		t.Fatal(o)
	}
	x.Trust.Provider = Untrusted
	if o := findObservation(t, x, KindProvider, now); o.health != 0 {
		t.Fatal(o)
	}
	if o := findObservation(t, x, KindVisibility, now); o.health != -1 {
		t.Fatal(o)
	}
}
func TestProtectionPureFacts(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		p      ProtectionFact
		health int
		reason string
	}{
		{"initial", ProtectionFact{ServerID: "row", InstanceID: "uuid", Enabled: true, Mode: "replicated", FailCount: 4}, 0, "initial_syncing"},
		{"stale", ProtectionFact{ServerID: "row", InstanceID: "uuid", Enabled: true, Mode: "replicated", HA: true, LastSync: now.Add(-31 * time.Minute)}, -1, "replication_stale"},
		{"future", ProtectionFact{ServerID: "row", InstanceID: "uuid", Enabled: true, Mode: "replicated", HA: true, LastSync: now.Add(time.Second)}, 0, "replication_malformed"},
		{"active", ProtectionFact{ServerID: "row", InstanceID: "uuid", Enabled: true, Mode: "replicated", HA: true, LastSync: now}, 1, "protection_active"},
		{"sharedmissing", ProtectionFact{ServerID: "row", InstanceID: "uuid", Enabled: true, Mode: "shared"}, -1, "shared_ha_missing"},
		{"disabled", ProtectionFact{ServerID: "row", InstanceID: "uuid"}, 1, "protection_disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := trusted()
			x.Protection = []ProtectionFact{tc.p}
			o := findObservation(t, x, KindProtection, now)
			if o.health != tc.health || o.reason != tc.reason {
				t.Fatal(o)
			}
		})
	}
}
func TestReducerConsecutiveDebounceUnknownRecovery(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	s := state{Incident: Incident{State: StatePending}}
	bad := observation{health: -1, quality: Verified}
	good := observation{health: 1, quality: Verified}
	var action string
	for epoch := int64(1); epoch <= 3; epoch++ {
		s, action = advance(s, bad, epoch, now.Add(time.Duration(epoch-1)*time.Minute))
		if (action == "OPEN") != (epoch == 3) {
			t.Fatal(epoch, action)
		}
	}
	before := s
	s, action = advance(s, bad, 3, now.Add(3*time.Minute))
	if action != "" || s.Version != before.Version {
		t.Fatal("duplicate advanced")
	}
	s, _ = advance(s, good, 4, now.Add(3*time.Minute))
	s, action = advance(s, observation{quality: Untrusted}, 5, now.Add(4*time.Minute))
	if action != "" || s.State != StateOpen {
		t.Fatal("unknown closed")
	}
	s, action = advance(s, good, 6, now.Add(5*time.Minute))
	if action != "" {
		t.Fatal("unknown counted healthy")
	}
	s, action = advance(s, good, 7, now.Add(6*time.Minute))
	if action != "RECOVERED" || s.Episode != 1 {
		t.Fatal(s, action)
	}
	s, action = advance(s, good, 8, now.Add(7*time.Minute))
	if action != "" {
		t.Fatal("duplicate recovery")
	}
	for epoch := int64(9); epoch <= 11; epoch++ {
		s, action = advance(s, bad, epoch, now.Add(time.Duration(epoch-1)*time.Minute))
	}
	if action != "OPEN" || s.Episode != 2 {
		t.Fatal(s, action)
	}
}
func TestVisibilityCannotRecoverFromMalformedVerifiedFacts(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*SnapshotResult)
	}{
		{"node malformed", func(x *SnapshotResult) { x.Nodes = []NodeFact{{"node1", "invalid"}} }},
		{"node unknown", func(x *SnapshotResult) { x.Nodes = []NodeFact{{"node1", NodeUnknown}} }},
		{"future sync", func(x *SnapshotResult) {
			x.Protection = []ProtectionFact{{ServerID: "row", InstanceID: "uuid", Enabled: true, Mode: "replicated", LastSync: now.Add(time.Hour)}}
		}},
		{"negative failures", func(x *SnapshotResult) {
			x.Protection = []ProtectionFact{{ServerID: "row", InstanceID: "uuid", Enabled: true, Mode: "replicated", FailCount: -1}}
		}},
		{"bad mode", func(x *SnapshotResult) {
			x.Protection = []ProtectionFact{{ServerID: "row", InstanceID: "uuid", Enabled: true, Mode: "invalid"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := state{Incident: Incident{State: StateOpen, Episode: 1}}
			for epoch := int64(1); epoch <= 2; epoch++ {
				x := trusted()
				tc.mutate(&x)
				o := findObservation(t, x, KindVisibility, now)
				if o.health == 1 {
					t.Fatal("malformed fact became healthy visibility", o)
				}
				var action string
				s, action = advance(s, o, epoch, now.Add(time.Duration(epoch)*time.Minute))
				if action != "" || s.State != StateOpen {
					t.Fatal("warning falsely recovered", s, action)
				}
			}
			for epoch := int64(3); epoch <= 4; epoch++ {
				var action string
				s, action = advance(s, findObservation(t, trusted(), KindVisibility, now), epoch, now.Add(time.Duration(epoch)*time.Minute))
				if (action == "RECOVERED") != (epoch == 4) {
					t.Fatal(s, action)
				}
			}
		})
	}
}

func TestReducerGapAndMalformedIdentity(t *testing.T) {
	now := time.Now()
	s := state{Incident: Incident{State: StatePending}}
	for i := int64(1); i < 5; i++ {
		s, _ = advance(s, observation{health: -1}, i, now.Add(time.Duration(i)*3*time.Minute))
	}
	if s.State == StateOpen {
		t.Fatal("gaps confirmed offline")
	}
	x := trusted()
	x.Nodes = []NodeFact{{"https://credentials@host", Offline}}
	if _, err := classify(x, now); err != ErrInvalid {
		t.Fatal("unsafe metadata accepted")
	}
	x = trusted()
	x.Nodes = []NodeFact{{"n", Offline}, {"n", Online}}
	if _, err := classify(x, now); err != ErrInvalid {
		t.Fatal("duplicate facts accepted")
	}
}
