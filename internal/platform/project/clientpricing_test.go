package project

import "testing"

import "github.com/mutex-solutions/proxcloud/internal/cloud"

func TestTimeUnitSizeInMonth(t *testing.T) {
	// No config → per-unit defaults.
	defaults := map[string]int{"minute": 43200, "hour": 720, "month": 1, "day": 1}
	for tu, want := range defaults {
		if got := timeUnitSizeInMonth(tu, nil); got != want {
			t.Errorf("default %s = %d, want %d", tu, got, want)
		}
	}

	// billingConfiguration.settings.timeUnitLimits OVERRIDES the default (e.g. a 730-hr month).
	limits := map[string]int{"hour": 730, "minute": 43800}
	if got := timeUnitSizeInMonth("hour", limits); got != 730 {
		t.Errorf("override hour = %d, want 730", got)
	}
	if got := timeUnitSizeInMonth("minute", limits); got != 43800 {
		t.Errorf("override minute = %d, want 43800", got)
	}
	// a unit NOT in the override map falls back to its default.
	if got := timeUnitSizeInMonth("month", limits); got != 1 {
		t.Errorf("month (not overridden) = %d, want 1", got)
	}
}

func TestInstanceValuesFromVolumeBackedServerDoesNotPriceFlavorDisk(t *testing.T) {
	cr := &cloud.CloudResource{Data: map[string]any{
		"volumeBacked": true,
		"server": map[string]any{
			"flavor": map[string]any{"name": "m1.medium", "disk": 80, "ram": 4096, "vcpus": 2},
		},
	}}
	values := instanceValuesFromServer(cr)
	if values == nil || values["root_disk_gb"] != 0 {
		t.Fatalf("root_disk_gb = %#v, want 0 for volume-backed pricing", values)
	}
}
