package sandbox

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func policyJSON(t *testing.T, n *Network) string {
	t.Helper()
	p, err := BuildEgressPolicy("sb-p1", "sbx-00000001", "uid-1", n)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func TestEgressPolicy(t *testing.T) {
	open := policyJSON(t, nil)
	if !strings.Contains(open, `"cidr":"0.0.0.0/0"`) || strings.Contains(open, "egressDeny") {
		t.Errorf("default policy must allow the internet and deny nothing extra: %s", open)
	}
	if !strings.Contains(open, `"uid":"uid-1"`) || !strings.Contains(open, `"proxcloud.io/sandbox-id":"sbx-00000001"`) {
		t.Errorf("policy must be owned by the CR and select the sandbox: %s", open)
	}

	// Deny-list: an allowed CIDR inside a denied one is carved out (allow wins, like E2B).
	dl := policyJSON(t, &Network{DenyOut: []string{"1.0.0.0/8"}, AllowOut: []string{"1.2.3.4"}})
	if !strings.Contains(dl, `"egressDeny":[{"toCIDRSet":[{"cidr":"1.0.0.0/8","except":["1.2.3.4/32"]}]}]`) {
		t.Errorf("deny-list policy: %s", dl)
	}

	// Allow-list: denyOut 0.0.0.0/0 drops the open rule; domains go to toFQDNs plus 8.8.8.8.
	al := policyJSON(t, &Network{DenyOut: []string{"0.0.0.0/0"}, AllowOut: []string{"8.8.4.0/24", "*.example.com", "api.x.io"}})
	for _, want := range []string{`"cidr":"8.8.4.0/24"`, `"matchPattern":"*.example.com"`, `"matchName":"api.x.io"`, `"cidr":"8.8.8.8/32"`} {
		if !strings.Contains(al, want) {
			t.Errorf("allow-list policy lacks %s: %s", want, al)
		}
	}
	if strings.Contains(al, `"cidr":"0.0.0.0/0"`) {
		t.Errorf("allow-list must not allow everything: %s", al)
	}
	none := &Network{DenyOut: []string{"0.0.0.0/0"}}
	if none.AllowsInternet() {
		t.Error("deny-all must report no internet")
	}

	for _, bad := range []*Network{
		{DenyOut: []string{"example.com"}},
		{AllowOut: []string{"not a host"}},
		{AllowOut: []string{"::1"}},
		{DNSServers: &[]string{"dns.google"}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v must be rejected", bad)
		}
	}
}

func TestBuildCREnvd(t *testing.T) {
	cfg := Config{Envd: true, Templates: map[string]Template{"py": {Image: "python:3.12", CPU: 1, MemoryGiB: 2, Command: []string{"python", "-m", "http.server"}}}}
	pub := false
	s := Spec{ID: "sbx-00000001", ProjectID: "p1", Template: "py", CPU: 1, MemoryGiB: 2,
		Env: map[string]string{"A": "1"}, Metadata: map[string]string{"k": "v"}, AllowPublicTraffic: &pub,
		envdToken: "tok", trafficToken: "ttok"}
	b, _ := json.Marshal(BuildCR(cfg, "svc", s, time.Unix(0, 0)))
	got := string(b)
	for _, want := range []string{
		`"command":["/.proxcloud/sandbox-init"]`,
		`"name":"PROXCLOUD_ENVD_ACCESS_TOKEN","value":"tok"`,
		`"name":"PROXCLOUD_START_CMD","value":"'python' '-m' 'http.server'"`,
		`"proxcloud.io/sandbox-traffic-token":"ttok"`,
		`"path":"/opt/proxcloud-sandbox/bin"`,
		`"readOnly":true`,
		`"proxcloud.io/sandbox-id":"sbx-00000001"`,
		`"proxcloud.io/metadata":"{\"k\":\"v\"}"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CR lacks %s:\n%s", want, got)
		}
	}
	// The cached payload never carries the token or PROXCLOUD_* names.
	d, _ := json.Marshal(sandboxData(BuildCR(cfg, "svc", s, time.Unix(0, 0))))
	if strings.Contains(string(d), "tok") || strings.Contains(string(d), "PROXCLOUD_") {
		t.Errorf("payload leaks internals: %s", d)
	}
	if bad := (Spec{Template: "py", Env: map[string]string{"PROXCLOUD_X": "1"}}); bad.Validate(cfg) == nil {
		t.Error("PROXCLOUD_ env names must be reserved")
	}
}

func TestValidIDAndLifecycle(t *testing.T) {
	if !ValidID(newSandboxID()) || !ValidID("sbx-1a2b3c4d") || ValidID("sbx-1a2b3c4d/../x") || ValidID("../sbx-1") {
		t.Error("ValidID")
	}
	// A pinned start keeps the sandbox RUNNING and billed from the first start, even while a
	// restarted container is not Ready.
	obj := map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnotationRunningSince: "2026-11-06T01:00:00Z"}},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "lastTransitionTime": "2026-11-06T01:30:00Z"}}},
	}
	if d := sandboxData(obj); d["status"] != StatusRunning || d["runningSince"] != "2026-11-06T01:00:00Z" {
		t.Errorf("pinned start lost: %v", d)
	}

	p := &SyncProvider{}
	now := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)
	never := &cloud.CloudResource{Data: map[string]any{"status": StatusPending}}
	if p.Linger(never, now) {
		t.Error("a sandbox that never ran has nothing to bill: archive at once")
	}
	expired := &cloud.CloudResource{Data: map[string]any{"status": StatusRunning, "runningSince": "2026-11-06T01:00:00Z", "shutdownTime": "2026-11-06T01:59:00Z"}}
	if !p.Linger(expired, now) || expired.Data["status"] != StatusDeleted || expired.Data["endedAt"] != "2026-11-06T01:59:00Z" {
		t.Errorf("a sandbox the controller removed ends at its shutdownTime: %v", expired.Data)
	}
	if p.Linger(expired, now.Add(5*time.Minute)) {
		t.Error("an ended sandbox is archived after the linger window")
	}
}
