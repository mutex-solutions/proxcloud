package externalservice

import (
	"reflect"
	"testing"
)

func TestEnabledVolumeTypesIsRegionScopedAndFailClosed(t *testing.T) {
	es := &ExternalService{Config: map[string]any{
		"features": map[string]any{
			"volumeTypes": map[string]any{
				"RegionOne": []any{
					map[string]any{"name": "multiattach", "enabled": false},
					map[string]any{"name": "ceph-ssd1", "displayName": " SSD ", "enabled": true},
					map[string]any{"name": "archive", "displayName": "", "enabled": true},
					map[string]any{"name": "ceph-ssd1", "displayName": "duplicate", "enabled": true},
				},
			},
		},
	}}

	want := []VolumeTypeConfig{
		{Name: "ceph-ssd1", DisplayName: "SSD"},
		{Name: "archive", DisplayName: "archive"},
	}
	if got := es.EnabledVolumeTypes("RegionOne"); !reflect.DeepEqual(got, want) {
		t.Fatalf("EnabledVolumeTypes() = %#v, want %#v", got, want)
	}
	if got := es.EnabledVolumeTypes("RegionTwo"); len(got) != 0 {
		t.Fatalf("missing region must fail closed, got %#v", got)
	}
	if got := (&ExternalService{}).EnabledVolumeTypes("RegionOne"); len(got) != 0 {
		t.Fatalf("missing catalog must fail closed, got %#v", got)
	}
}

func TestSoleRegion(t *testing.T) {
	one := &ExternalService{Config: map[string]any{
		"regions": map[string]any{"RegionOne": map[string]any{"name": "RegionOne"}},
	}}
	if got := one.SoleRegion(); got != "RegionOne" {
		t.Fatalf("SoleRegion() = %q, want RegionOne", got)
	}
	two := &ExternalService{Config: map[string]any{
		"regions": map[string]any{"RegionOne": map[string]any{}, "RegionTwo": map[string]any{}},
	}}
	if got := two.SoleRegion(); got != "" {
		t.Fatalf("two regions must be ambiguous, got %q", got)
	}
	if got := (&ExternalService{}).SoleRegion(); got != "" {
		t.Fatalf("no regions must yield empty, got %q", got)
	}
	var nilES *ExternalService
	if got := nilES.SoleRegion(); got != "" {
		t.Fatalf("nil receiver must yield empty, got %q", got)
	}
}

func TestCatalogRegionValidatesRequestedRegion(t *testing.T) {
	multi := &ExternalService{Config: map[string]any{
		"regions": map[string]any{"RegionOne": map[string]any{}, "RegionTwo": map[string]any{}},
	}}
	if got := multi.CatalogRegion("RegionTwo"); got != "RegionTwo" {
		t.Fatalf("declared region must be honored, got %q", got)
	}
	if got := multi.CatalogRegion("evil-region"); got != "" {
		t.Fatalf("undeclared region on a multi-region provider must fail closed, got %q", got)
	}
	if got := multi.CatalogRegion(""); got != "" {
		t.Fatalf("blank region on a multi-region provider must fail closed, got %q", got)
	}
	sole := &ExternalService{Config: map[string]any{
		"regions": map[string]any{"RegionOne": map[string]any{}},
	}}
	if got := sole.CatalogRegion(""); got != "RegionOne" {
		t.Fatalf("blank region must fall back to the sole region, got %q", got)
	}
	if got := sole.CatalogRegion("bogus"); got != "RegionOne" {
		t.Fatalf("bogus region must fall back to the sole region, got %q", got)
	}
}
