package billingjob

import (
	"testing"
	"time"
)

func TestCycleIdentityUsesOriginalCanonicalCutoff(t *testing.T) {
	at := time.Date(2026, 10, 31, 23, 59, 58, 0, time.UTC)
	a, err := NewCycle("p1", "minute", at)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewCycle("p1", "minute", at.Add(time.Second))
	if a.Key() != b.Key() {
		t.Fatalf("same original minute differs: %q %q", a.Key(), b.Key())
	}
	other, _ := NewCycle("p1", "minute", at.Add(2*time.Minute))
	if a.Key() == other.Key() {
		t.Fatal("different original cycles collapsed")
	}
	start, end, cutoff := chargeCycle(a.CycleAt, a.TimeUnit)
	if start.Month() != time.October || end.Month() != time.November || cutoff.Minute() != 59 {
		t.Fatalf("delayed cycle moved: %v %v %v", start, end, cutoff)
	}
	for _, bad := range []Cycle{{}, {Version: 1, ProfileID: "p", TimeUnit: "bad", CycleAt: at}, {Version: 1, ProfileID: "p|other", TimeUnit: "hour", CycleAt: at}, {Version: 1, ProfileID: "p", TimeUnit: "minute", CycleAt: at}} {
		if bad.Validate() == nil {
			t.Fatalf("bad cycle accepted: %+v", bad)
		}
	}
}
