package externalservice

import "testing"

func TestServiceEnabledInRegion(t *testing.T) {
	svc := func(cfg map[string]any) *ExternalService { return &ExternalService{Config: cfg} }
	cases := []struct {
		name string
		es   *ExternalService
		slug string
		want bool
	}{
		{"no services map = everything on", svc(map[string]any{}), "key-manager", true},
		{"nil config = everything on", &ExternalService{}, "key-manager", true},
		{"enabled region", svc(map[string]any{"services": map[string]any{"key-manager": map[string]any{"RegionOne": true}}}), "key-manager", true},
		{"disabled region", svc(map[string]any{"services": map[string]any{"key-manager": map[string]any{"RegionOne": false}}}), "key-manager", false},
		{"slug absent from a non-empty map", svc(map[string]any{"services": map[string]any{"compute": map[string]any{"RegionOne": true}}}), "key-manager", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.es.ServiceEnabledInRegion(c.slug, "RegionOne"); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}
