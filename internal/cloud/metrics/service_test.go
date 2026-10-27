package metrics

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func TestIsPublicTraffic(t *testing.T) {
	ports := []cloud.CloudResource{
		{ExternalID: "abcd1234-port-uuid", Data: map[string]any{"port": map[string]any{"networkId": "net-public"}}},
		{ExternalID: "eeee5678-port-uuid", Data: map[string]any{"port": map[string]any{"networkId": "net-private"}}},
	}
	publicNets := []cloud.CloudResource{{ExternalID: "net-public"}}

	cases := []struct {
		name string
		tap  string
		want bool
	}{
		{"public interface", "tapabcd1234", true},   // → prefix abcd1234 → net-public ∈ publicNets
		{"private interface", "tapeeee5678", false}, // → net-private ∉ publicNets
		{"no matching port", "tapffff9999", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isPublicTraffic(Resource{Name: c.tap}, ports, publicNets)
			if got != c.want {
				t.Fatalf("isPublicTraffic(%q) = %v, want %v", c.tap, got, c.want)
			}
		})
	}
}
