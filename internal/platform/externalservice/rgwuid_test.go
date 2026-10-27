package externalservice

import "testing"

// RGW users live on the external Ceph cluster: the default prefix is "proxcloud-", but a configured
// uidPrefix (including an explicit "") always wins so existing users keep resolving.
func TestRGWUIDForPrefix(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"nil config → default", nil, "proxcloud-p1"},
		{"absent key → default", map[string]any{}, "proxcloud-p1"},
		{"configured legacy prefix", map[string]any{"uidPrefix": "legacy-"}, "legacy-p1"},
		{"configured custom prefix", map[string]any{"uidPrefix": "dev_"}, "dev_p1"},
		{"explicit empty = no prefix", map[string]any{"uidPrefix": ""}, "p1"},
	}
	for _, c := range cases {
		if got := (&ExternalService{Config: c.cfg}).RGWUIDFor("p1"); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestPVEPoolPrefixDefault(t *testing.T) {
	if got := (&ExternalService{}).PoolFor("p1"); got != "pc-p1" {
		t.Errorf("default pool = %q, want pc-p1", got)
	}
	if got := (&ExternalService{Config: map[string]any{"poolPrefix": "st-"}}).PoolFor("p1"); got != "st-p1" {
		t.Errorf("configured pool = %q, want st-p1", got)
	}
}
