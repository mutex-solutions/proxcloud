package externalservice

import (
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
)

func TestKamajiConfig(t *testing.T) {
	es := &ExternalService{
		ID:   "svc-1",
		Type: TypeCloud,
		Config: map[string]any{
			"provider": "kamaji",
			"regions":  map[string]any{"az1": map[string]any{}},
			"argocd": map[string]any{
				"chartRepo":    "ghcr.io/mutex-solutions/charts",
				"chartName":    "kamaji-cluster",
				"chartVersion": "0.2.3",
				"project":      "proxcloud-k8s",
			},
			"cluster": map[string]any{
				"dataStoreName": "default",
				"dnsZone":       "k8s.example.com",
				"versions":      map[string]any{"1.35.4": "img-1"},
				"imageVariants": map[string]any{
					"nvidia": map[string]any{"1.35.4": "img-nv"},
					"empty":  map[string]any{"1.35.4": ""}, // blank ids are dropped → variant dropped
					"junk":   "not-a-map",                  // malformed entries are skipped
				},
			},
		},
		Secret: map[string]any{"kubeconfig": "KC"},
	}
	if !es.IsKamaji() {
		t.Fatal("IsKamaji = false")
	}
	cfg := es.KamajiConfig()
	if cfg.Kubeconfig != "KC" || cfg.Region != "az1" {
		t.Errorf("kubeconfig/region = %q/%q", cfg.Kubeconfig, cfg.Region)
	}
	// Defaults fill in when the doc omits them.
	if cfg.ArgoNamespace != "argocd" || cfg.ChartName != "kamaji-cluster" {
		t.Errorf("namespace default/chart = %q/%q", cfg.ArgoNamespace, cfg.ChartName)
	}
	if cfg.ArgoProject != "proxcloud-k8s" || cfg.ChartVersion != "0.2.3" {
		t.Errorf("argo = %q/%q", cfg.ArgoProject, cfg.ChartVersion)
	}
	if cfg.Defaults.Versions["1.35.4"] != "img-1" || cfg.Defaults.DNSZone != "k8s.example.com" {
		t.Errorf("cluster defaults = %+v", cfg.Defaults)
	}
	if cfg.Defaults.ImageVariants["nvidia"]["1.35.4"] != "img-nv" || len(cfg.Defaults.ImageVariants) != 1 {
		t.Errorf("imageVariants = %+v", cfg.Defaults.ImageVariants)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// A proxmox provider is not kamaji; a kamaji doc without chart pin or chart name fails
	// validation (no default chart until the Proxmox cluster chart lands).
	pve := &ExternalService{Config: map[string]any{"provider": "proxmox"}}
	if pve.IsKamaji() {
		t.Error("proxmox IsKamaji = true")
	}
	unpinned := es.KamajiConfig()
	unpinned.ChartVersion = ""
	if err := unpinned.Validate(); err == nil {
		t.Error("unpinned chart version must fail validation (plan §9: never latest)")
	}
	unnamed := es.KamajiConfig()
	unnamed.ChartName = ""
	if err := unnamed.Validate(); err == nil {
		t.Error("missing chart name must fail validation")
	}
}

// Stage 60 sends "{{ x | int }}", which reaches the API as a string: read as 0, a site's rwx_gib silently meant no
// StorageClass, its worker_mtu the guest default on a 1450 vnet, and vlan_min/vlan_max no tags at all.
func TestIntOfTakesTheInstallersStrings(t *testing.T) {
	for _, c := range []struct {
		in   any
		want int
	}{{float64(100), 100}, {100, 100}, {int64(100), 100}, {"100", 100}, {" 1450 ", 1450}, {"", 0}, {"lots", 0}, {nil, 0}, {true, 0}} {
		if got := intOf(c.in); got != c.want {
			t.Errorf("intOf(%#v) = %d, want %d", c.in, got, c.want)
		}
	}
	es := &ExternalService{Config: map[string]any{"cluster": map[string]any{"rootVolumeGiB": "30", "workerMtu": "1450", "rwxGiB": "100"}}}
	d := es.KamajiConfig().Defaults
	if d.RootVolumeGiB != 30 || d.NodeGroupMTU != 1450 || d.RWXGiB != 100 {
		t.Errorf("defaults from strings: %+v", d)
	}
}

// The tenant-side controller's keys are named whatever their value, false or empty included: a config that still
// carries one is refused rather than silently doing nothing.
// The automatic replacement of a worker that never joined is on with a default timeout when the provider says
// nothing, and 0 turns it off: an absent key and a 0 are different answers.
func TestKamajiConfigWorkerRepair(t *testing.T) {
	for name, tc := range map[string]struct {
		cluster        map[string]any
		timeout, guard int
	}{
		"absent":       {map[string]any{}, kamaji.DefaultNodeStartupTimeoutSeconds, 0},
		"off":          {map[string]any{"nodeStartupTimeoutSeconds": float64(0)}, 0, 0},
		"set":          {map[string]any{"nodeStartupTimeoutSeconds": float64(900), "repairMaxUnhealthy": float64(2)}, 900, 2},
		"from ansible": {map[string]any{"nodeStartupTimeoutSeconds": "2700"}, 2700, 0},
		// Values the save refuses, stored before it did: the default, never the value as sent.
		"negative":        {map[string]any{"nodeStartupTimeoutSeconds": float64(-5)}, kamaji.DefaultNodeStartupTimeoutSeconds, 0},
		"under the floor": {map[string]any{"nodeStartupTimeoutSeconds": float64(30)}, kamaji.DefaultNodeStartupTimeoutSeconds, 0},
		"not a number":    {map[string]any{"nodeStartupTimeoutSeconds": "15m", "repairMaxUnhealthy": "40%"}, kamaji.DefaultNodeStartupTimeoutSeconds, 0},
		"guard of 0":      {map[string]any{"repairMaxUnhealthy": float64(0)}, kamaji.DefaultNodeStartupTimeoutSeconds, 0},
	} {
		d := (&ExternalService{Config: map[string]any{"cluster": tc.cluster}}).KamajiConfig().Defaults
		if d.NodeStartupTimeoutSeconds != tc.timeout || d.RepairMaxUnhealthy != tc.guard {
			t.Errorf("%s: timeout %d guard %d", name, d.NodeStartupTimeoutSeconds, d.RepairMaxUnhealthy)
		}
	}
}

// The four values the lab round's provider save answered 200 to (30, -5, "40%", "yes") are named with their field, and
// so is every other value that is not of the key's type or range. What is valid is not named.
func TestKamajiClusterProblems(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value any
		bad   bool
	}{
		{"nodeStartupTimeoutSeconds", float64(30), true},
		{"nodeStartupTimeoutSeconds", float64(-5), true},
		{"nodeStartupTimeoutSeconds", float64(MinNodeStartupTimeoutSeconds - 1), true},
		// The floor as literals: lowering the constant must fail here.
		{"nodeStartupTimeoutSeconds", float64(600), true},
		{"nodeStartupTimeoutSeconds", float64(899), true},
		{"nodeStartupTimeoutSeconds", float64(900), false},
		{"nodeStartupTimeoutSeconds", float64(MaxNodeStartupTimeoutSeconds + 1), true},
		{"nodeStartupTimeoutSeconds", float64(1800.5), true},
		{"nodeStartupTimeoutSeconds", "15m", true},
		{"nodeStartupTimeoutSeconds", true, true},
		{"nodeStartupTimeoutSeconds", nil, true},
		{"nodeStartupTimeoutSeconds", float64(0), false},
		{"nodeStartupTimeoutSeconds", float64(MinNodeStartupTimeoutSeconds), false},
		{"nodeStartupTimeoutSeconds", float64(1800), false},
		{"nodeStartupTimeoutSeconds", "2700", false},
		{"repairMaxUnhealthy", "40%", true},
		{"repairMaxUnhealthy", float64(0), true},
		{"repairMaxUnhealthy", float64(-1), true},
		{"repairMaxUnhealthy", float64(1.5), true},
		{"repairMaxUnhealthy", float64(1), false},
		{"repairMaxUnhealthy", "2", false},
		{"workerFirewall", "yes", true},
		{"workerFirewall", "true", true},
		{"workerFirewall", float64(1), true},
		{"workerFirewall", nil, true},
		{"workerFirewall", true, false},
		{"workerFirewall", false, false},
	} {
		got := KamajiClusterProblems(map[string]any{"cluster": map[string]any{tc.key: tc.value}})
		if tc.bad && (len(got) != 1 || !strings.HasPrefix(got[0], "config.cluster."+tc.key+" must be ")) {
			t.Errorf("%s = %#v: %q", tc.key, tc.value, got)
		}
		if !tc.bad && len(got) != 0 {
			t.Errorf("%s = %#v is valid: %q", tc.key, tc.value, got)
		}
	}
	if got := KamajiClusterProblems(map[string]any{"cluster": map[string]any{"dnsZone": "k8s.example"}}); len(got) != 0 {
		t.Errorf("absent keys: %q", got)
	}
	// "yes" never turns the firewall on.
	if (&ExternalService{Config: map[string]any{"cluster": map[string]any{"workerFirewall": "yes"}}}).KamajiConfig().Defaults.WorkerFirewall {
		t.Error(`workerFirewall "yes" read as on`)
	}
}

// The notes of a stored config: a value that is not valid, and repair keys on a chart that renders no health check.
func TestKamajiConfigNotes(t *testing.T) {
	cfg := func(chart string, cluster map[string]any) map[string]any {
		return map[string]any{"argocd": map[string]any{"chartVersion": chart}, "cluster": cluster}
	}
	if got := KamajiConfigNotes(cfg("0.1.13", map[string]any{})); len(got) != 0 {
		t.Errorf("no key set: %q", got)
	}
	if got := KamajiConfigNotes(cfg("0.1.14", map[string]any{"nodeStartupTimeoutSeconds": float64(1800), "repairMaxUnhealthy": float64(1), "workerFirewall": true})); len(got) != 0 {
		t.Errorf("valid on 0.1.14: %q", got)
	}
	got := KamajiConfigNotes(cfg("0.1.13", map[string]any{"nodeStartupTimeoutSeconds": float64(1800)}))
	if len(got) != 1 || !strings.Contains(got[0], "have no effect on cluster chart 0.1.13") || !strings.Contains(got[0], "0.1.14 or later") {
		t.Errorf("old chart: %q", got)
	}
	// The worker firewall needs no chart: no note for it.
	if got := KamajiConfigNotes(cfg("0.1.13", map[string]any{"workerFirewall": true})); len(got) != 0 {
		t.Errorf("firewall on an old chart: %q", got)
	}
	got = KamajiConfigNotes(cfg("0.1.14", map[string]any{"nodeStartupTimeoutSeconds": float64(30), "workerFirewall": "yes"}))
	if len(got) != 2 || !strings.Contains(got[0], "Stored value ignored") || !strings.Contains(got[0], "nodeStartupTimeoutSeconds") || !strings.Contains(got[1], "workerFirewall") {
		t.Errorf("stored invalid: %q", got)
	}
}

func TestKamajiRemovedKeys(t *testing.T) {
	for _, c := range []struct {
		cluster map[string]any
		want    string
	}{
		{nil, ""},
		{map[string]any{"pveApiUrl": "https://192.0.2.11:8006"}, ""},
		{map[string]any{"csiSharedStores": false}, "csiSharedStores"},
		{map[string]any{"pveCsiUrl": ""}, "pveCsiUrl"},
		{map[string]any{"csiSharedStores": true, "pveCsiUrl": "https://192.0.2.60:8006"}, "csiSharedStores,pveCsiUrl"},
	} {
		cfg := map[string]any{}
		if c.cluster != nil {
			cfg["cluster"] = c.cluster
		}
		if got := strings.Join(KamajiRemovedKeys(cfg), ","); got != c.want {
			t.Errorf("%v: %q, want %q", c.cluster, got, c.want)
		}
	}
}

// config.cluster.allowedCidrs is the allow-list of a new cluster's API endpoint when the tenant names none. The save
// refuses a new entry Kubernetes 1.36 would refuse and passes what the stored block already has; a stored entry with
// host bits is named on the provider, never rewritten, and read without its host bits for a cluster about to be made.
func TestKamajiAllowedCIDRs(t *testing.T) {
	block := func(c ...any) map[string]any { return map[string]any{"cluster": map[string]any{"allowedCidrs": c}} }
	if err := KamajiAllowedCIDRsErr(block("10.0.0.0/8"), nil); err != nil {
		t.Errorf("canonical: %v", err)
	}
	err := KamajiAllowedCIDRsErr(block("10.0.0.0/8", "192.168.0.5/24"), nil)
	if err == nil || err.Error() != "config.cluster.allowedCidrs: 192.168.0.5/24 has host bits set: use 192.168.0.0/24" {
		t.Errorf("host bits on a create: %v", err)
	}
	stored := block("192.168.0.5/24")["cluster"]
	if err := KamajiAllowedCIDRsErr(block("192.168.0.5/24", "10.0.0.0/8"), stored); err != nil {
		t.Errorf("the stored entry beside a new one: %v", err)
	}
	if err := KamajiAllowedCIDRsErr(block("192.168.0.5/24", "10.1.2.3/8"), stored); err == nil {
		t.Error("a new entry with host bits passed beside the stored one")
	}
	if err := KamajiAllowedCIDRsErr(map[string]any{}, nil); err != nil {
		t.Errorf("no block: %v", err)
	}

	cfg := block("192.168.0.5/24", "10.0.0.0/8")
	notes := KamajiConfigNotes(cfg)
	if len(notes) != 1 || !strings.Contains(notes[0], "192.168.0.5/24 has host bits set: use 192.168.0.0/24") || !strings.Contains(notes[0], "Correct the stored value") {
		t.Errorf("note of a stored entry with host bits: %q", notes)
	}
	if got := kamajiDefaultCIDRs(cfg["cluster"].(map[string]any)); len(got) != 2 || got[0] != "192.168.0.0/24" || got[1] != "10.0.0.0/8" {
		t.Errorf("read for a new cluster: %v", got)
	}
	if stored := cfg["cluster"].(map[string]any)["allowedCidrs"].([]any); stored[0] != "192.168.0.5/24" {
		t.Errorf("the stored value was rewritten: %v", stored)
	}
	if notes := KamajiConfigNotes(block("10.0.0.0/8")); len(notes) != 0 {
		t.Errorf("notes of a canonical list: %q", notes)
	}
}

// config.cluster.loadBalancer: absent or incomplete is off; a block of another shape is refused on save.
func TestKamajiLoadBalancer(t *testing.T) {
	es := func(lb any) *ExternalService {
		return &ExternalService{Config: map[string]any{"provider": "kamaji", "cluster": map[string]any{"loadBalancer": lb}}}
	}
	if n, f, ok := es(map[string]any{"publicNetworkId": "vmbr1", "flavorId": "lb-small"}).KamajiLoadBalancer(); !ok || n != "vmbr1" || f != "lb-small" {
		t.Fatalf("set: %q %q %v", n, f, ok)
	}
	for _, off := range []any{nil, map[string]any{"publicNetworkId": "vmbr1"}, "vmbr1"} {
		if _, _, ok := es(off).KamajiLoadBalancer(); ok {
			t.Errorf("%v reads as on", off)
		}
	}
	for _, bad := range []any{"vmbr1", map[string]any{"publicNetworkId": "vmbr1"}, map[string]any{"publicNetworkId": "vmbr1", "flavorId": "x", "vip": "203.0.113.9"}} {
		if p := KamajiClusterProblems(es(bad).Config); len(p) != 1 || !strings.Contains(p[0], "config.cluster.loadBalancer") {
			t.Errorf("%v: %q", bad, p)
		}
	}
	if p := KamajiClusterProblems(es(map[string]any{"publicNetworkId": "vmbr1", "flavorId": "lb-small"}).Config); len(p) != 0 {
		t.Errorf("a valid block: %q", p)
	}
}
