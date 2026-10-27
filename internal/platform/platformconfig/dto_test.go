package platformconfig

import (
	"encoding/json"
	"testing"
)

func sampleConfig() *PlatformConfiguration {
	return &PlatformConfiguration{
		Branding:          &Branding{Name: "ProxCloud", Logo: "https://x/logo.png", Color: "#0066cc"},
		DateConfiguration: &DateFormat{DateFormat: "DD/MM/YYYY"},
		Regions:           []RegionDisplayConfig{{ServiceID: "svc1", Region: "r1", Order: 0}},
	}
}

func TestToDtoUnauthenticated(t *testing.T) {
	b, _ := json.Marshal(toDto(sampleConfig(), false, "REMOTE_OIDC"))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["id"] != "default" {
		t.Errorf("id = %v, want default", m["id"])
	}
	if m["authStrategy"] != "REMOTE_OIDC" {
		t.Errorf("authStrategy = %v", m["authStrategy"])
	}
	if _, ok := m["branding"]; !ok {
		t.Error("branding should be present")
	}
	if _, ok := m["dateConfiguration"]; !ok {
		t.Error("dateConfiguration should be present")
	}
	// regions omitted for unauthenticated callers
	if _, ok := m["regions"]; ok {
		t.Errorf("regions should be omitted when unauthenticated, got %v", m["regions"])
	}
}

func TestToDtoAuthenticated(t *testing.T) {
	b, _ := json.Marshal(toDto(sampleConfig(), true, "REMOTE_OIDC"))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	regions, ok := m["regions"].([]any)
	if !ok || len(regions) != 1 {
		t.Errorf("regions = %v, want 1 entry when authenticated", m["regions"])
	}
}
