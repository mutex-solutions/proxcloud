package project

import (
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/function"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// The reaper's decision: idle past the idle time → the floor; below the floor → the floor;
// suspended → zero; anything else stays.
func TestFunctionTarget(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	fc := sandbox.FunctionsConfig{MinIdleSeconds: 60, MaxIdleSeconds: 3600}
	for _, tc := range []struct {
		name  string
		fn    functionDoc
		floor int
		want  int
	}{
		{"busy stays up", functionDoc{Instances: 1, LastActiveAt: ago(10 * time.Second)}, 0, 1},
		{"inside the idle window stays up", functionDoc{Instances: 1, LastActiveAt: ago(299 * time.Second)}, 0, 1},
		{"idle past the window scales to zero", functionDoc{Instances: 1, LastActiveAt: ago(301 * time.Second)}, 0, 0},
		{"idle with a floor of one stays up", functionDoc{Instances: 1, LastActiveAt: ago(time.Hour)}, 1, 1},
		{"below the floor is raised", functionDoc{Instances: 0, LastActiveAt: ago(time.Hour)}, 1, 1},
		{"at zero with no floor stays at zero", functionDoc{Instances: 0}, 0, 0},
		{"never active counts as idle", functionDoc{Instances: 1}, 0, 0},
		{"suspended goes to zero whatever the floor", functionDoc{Instances: 1, Suspended: true, LastActiveAt: ago(time.Second)}, 1, 0},
		{"suspended is not raised to the floor", functionDoc{Instances: 0, Suspended: true}, 1, 0},
	} {
		fn := tc.fn
		fn.IdleSeconds = 300
		if got := functionTarget(&fn, fc, tc.floor, now); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	// The location's idle bounds apply to a function's own idleSeconds without a redeploy.
	fn := functionDoc{Instances: 1, LastActiveAt: ago(90 * time.Second)}
	fn.IdleSeconds = 30 // below the minimum: 60 s applies
	if got := functionTarget(&fn, fc, 0, now); got != 0 {
		t.Errorf("idle 90s with idleSeconds clamped up to 60: %d, want 0", got)
	}
	fn.LastActiveAt = ago(45 * time.Second)
	if got := functionTarget(&fn, fc, 0, now); got != 1 {
		t.Errorf("idle 45s with idleSeconds clamped up to 60: %d, want 1", got)
	}
	fn.IdleSeconds, fn.LastActiveAt = 7200, ago(3601*time.Second) // above the maximum: 3600 s applies
	if got := functionTarget(&fn, fc, 0, now); got != 0 {
		t.Errorf("idleSeconds clamped down to 3600: %d, want 0", got)
	}
	// When the idle time ran out is what a late reaper ends the interval at: the last activity plus the idle time,
	// held to the location's bounds; never for a function that had no activity.
	late := functionDoc{Instances: 1, LastActiveAt: ago(2 * time.Hour)}
	late.IdleSeconds = 300
	if got := functionIdleOut(&late, fc); !got.Equal(now.Add(-2*time.Hour + 300*time.Second)) {
		t.Errorf("idle out of a function last active two hours ago: %v", got)
	}
	late.IdleSeconds = 5
	if got := functionIdleOut(&late, fc); !got.Equal(now.Add(-2*time.Hour + 60*time.Second)) {
		t.Errorf("idle out with idleSeconds below the location's minimum: %v", got)
	}
	if got := functionIdleOut(&functionDoc{Instances: 1}, fc); !got.IsZero() {
		t.Errorf("idle out of a function that never had activity: %v", got)
	}
}

// The load's decision: desired = ceil(peak / concurrency), held between max(floor, 1) and the
// cap; up at once to it, down one instance at a time and only when calm; never to zero.
func TestLoadTarget(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		instances, peak, concurrency, floor, maxN int
		calm                                      bool
		want                                      int
		busy                                      bool
	}{
		{"one instance, light load", 1, 10, 50, 0, 5, true, 1, true},
		{"exactly the concurrency is one instance", 1, 50, 50, 0, 5, true, 1, true},
		{"one more request is two", 1, 51, 50, 0, 5, true, 2, true},
		{"straight up to what the load asks", 1, 180, 50, 0, 5, false, 4, true},
		{"held to the cap", 2, 10000, 50, 0, 5, false, 5, true},
		{"a cap of one (the default) never scales out", 1, 10000, 50, 0, 1, true, 1, true},
		{"concurrency 1: one instance a request", 1, 3, 1, 0, 5, false, 3, true},
		{"less load, not calm: stays", 4, 60, 50, 0, 5, false, 4, false},
		{"less load, calm: one down", 4, 60, 50, 0, 5, true, 3, false},
		{"no load, calm: one down", 2, 0, 50, 0, 5, true, 1, false},
		{"never below one: zero is the idle rule's", 1, 0, 50, 0, 5, true, 1, true},
		{"never below the floor", 1, 0, 50, 1, 5, true, 1, true},
		{"above a lowered cap: down to it at once", 5, 1000, 50, 0, 2, false, 2, true},
		{"what the load asks of every instance is busy", 3, 101, 50, 0, 5, true, 3, true},
	} {
		got, busy := loadTarget(tc.instances, tc.peak, tc.concurrency, tc.floor, tc.maxN, tc.calm)
		if got != tc.want || busy != tc.busy {
			t.Errorf("%s: %d busy=%v, want %d busy=%v", tc.name, got, busy, tc.want, tc.busy)
		}
	}
	// The cap is the function's maxInstances held to the location's, and one for a function that names none.
	fc := sandbox.Config{Functions: sandbox.FunctionsConfig{MaxInstances: 3}}.WithDefaults().Functions
	for _, tc := range []struct {
		fn   functionDoc
		want int
	}{
		{functionDoc{}, 1},
		{functionDoc{Spec: function.Spec{MaxInstances: 2}}, 2},
		{functionDoc{Spec: function.Spec{MaxInstances: 5}}, 3}, // the location lowered its cap
	} {
		if got := functionMax(&tc.fn, fc); got != tc.want {
			t.Errorf("functionMax(%d) = %d, want %d", tc.fn.MaxInstances, got, tc.want)
		}
	}
	// The reaper: idle from three instances goes straight to the floor (unchanged by the load), and
	// a count above the cap comes down to it.
	now := time.Now()
	idle, busy := now.Add(-time.Hour), now.Add(-time.Second)
	three := functionDoc{Instances: 3, LastActiveAt: &idle, Spec: function.Spec{IdleSeconds: 300, MaxInstances: 3}}
	if got := functionTarget(&three, fc, 0, now); got != 0 {
		t.Errorf("idle at three instances: %d, want 0", got)
	}
	three.LastActiveAt = &busy
	if got := functionTarget(&three, fc, 0, now); got != 3 {
		t.Errorf("busy at three instances: %d, want 3 (the load's to change)", got)
	}
	three.MaxInstances = 2
	if got := functionTarget(&three, fc, 0, now); got != 2 {
		t.Errorf("busy at three with a cap of two: %d, want 2", got)
	}
}
