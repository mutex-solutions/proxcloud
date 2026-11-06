package externalservice

import "testing"

func TestMetricsSourceDefaults(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"proxmox absent → native counters", map[string]any{"provider": "proxmox"}, MetricsSourcePVE},
		{"proxmox explicit none", map[string]any{"provider": "proxmox", "metrics": map[string]any{"source": "none"}}, MetricsSourceNone},
		{"proxmox explicit prometheus", map[string]any{"provider": "proxmox", "metrics": map[string]any{"source": "prometheus"}}, MetricsSourcePrometheus},
		{"other absent → none", map[string]any{"provider": "kamaji"}, MetricsSourceNone},
	}
	for _, c := range cases {
		if got := (&ExternalService{Config: c.cfg}).MetricsSource(); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
