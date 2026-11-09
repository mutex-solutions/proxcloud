package sandbox

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func ptr(v uint64) *uint64 { return &v }
func sampleAt(t time.Time, cpu uint64) Sample {
	return Sample{ProjectID: "p", SandboxID: "sbx-1234567890123456", CRUID: "cr", PodUID: "pod", ContainerID: "containerd://original", Node: "n", At: t, CPUAt: t, NetworkAt: t, CPU: ptr(cpu), RX: ptr(5), TX: ptr(5), Cores: 1}
}
func TestAbuseConsecutiveAndReset(t *testing.T) {
	now := time.Now().UTC()
	p := AbusePolicy{WindowSeconds: 60}
	var o Observation
	o, flag := o.Next(sampleAt(now, 0), p, now)
	if flag {
		t.Fatal("first sample flagged")
	}
	o, flag = o.Next(sampleAt(now.Add(30*time.Second), 30e9), p, now.Add(30*time.Second))
	if flag {
		t.Fatal("short window flagged")
	}
	o, flag = o.Next(sampleAt(now.Add(60*time.Second), 60e9), p, now.Add(60*time.Second))
	if !flag {
		t.Fatal("consecutive CPU not flagged")
	}
	_, flag = o.Next(sampleAt(now.Add(90*time.Second), 90e9), p, now.Add(90*time.Second))
	if flag {
		t.Fatal("duplicate alert")
	}
	for _, kind := range []string{"gap", "missing", "activity", "uid", "container", "restart", "rollback", "stale", "policy"} {
		t.Run(kind, func(t *testing.T) {
			base, _ := Observation{}.Next(sampleAt(now, 0), p, now)
			s := sampleAt(now.Add(60*time.Second), 60e9)
			pol := p
			switch kind {
			case "gap":
				s.At = now.Add(3 * time.Minute)
			case "missing":
				s.RX = nil
			case "activity":
				s.LastExec = s.At
			case "uid":
				s.PodUID = "other"
			case "container":
				s.ContainerID = "containerd://replacement"
			case "restart":
				s.RestartCount = 1
			case "rollback":
				base.Previous.CPU = ptr(100e9)
			case "stale":
				s.CPUAt = now.Add(-time.Hour)
			case "policy":
				pol.CPUPercent = 95
			}
			_, f := base.Next(s, pol, now.Add(60*time.Second))
			if f {
				t.Fatal("unsafe sample flagged")
			}
		})
	}
	// Exactly one minute is supported, but requires two counters separated by a minute.
	first, _ := Observation{}.Next(sampleAt(now, 0), p, now)
	_, flag = first.Next(sampleAt(now.Add(time.Minute), 60e9), p, now.Add(time.Minute))
	if !flag {
		t.Fatal("one minute window")
	}
}
func TestEgressDefaultTrustedTemplateAndNoCallerOverride(t *testing.T) {
	cfg := testConfig()
	cfg.EgressMbps = 12
	tpl := cfg.Templates["python"]
	tpl.EgressMbps = 7
	cfg.Templates["python"] = tpl
	for _, custom := range []bool{false, true} {
		s := Spec{ID: "sbx-1234567890123456", ProjectID: "p", Template: "python", CPU: 1, MemoryGiB: 1}
		if custom {
			s.Custom = &tpl
		}
		cr := BuildCR(cfg, "svc", s, time.Now())
		b, _ := json.Marshal(cr)
		want := "7M"
		if custom {
			want = "12M"
		}
		if !strings.Contains(string(b), `"kubernetes.io/egress-bandwidth":"`+want+`"`) {
			t.Fatal(string(b))
		}
	}
}
func TestAutomaticActionsRequireSupportedProvider(t *testing.T) {
	for _, tc := range []struct {
		envd    bool
		action  string
		storage bool
		wantErr bool
	}{
		{true, "alert", false, false}, {true, "pause", true, true}, {true, "kill", true, true},
		{false, "pause", false, true}, {false, "pause", true, false}, {false, "kill", false, false},
	} {
		cfg := Config{Envd: tc.envd, Abuse: AbusePolicy{Action: tc.action}}
		if tc.storage {
			cfg.TemplateRegistry = "registry/t"
			cfg.BuildStorage.Bucket = "b"
			cfg.BuildStorage.Endpoint = "https://bucket.example"
			cfg.BuildStorage.AccessKey = "a"
			cfg.BuildStorage.SecretKey = "s"
		}
		if err := cfg.ValidateAutomaticActions(); (err != nil) != tc.wantErr {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}

func TestAutomaticPauseRejectsEachMissingStoragePrerequisite(t *testing.T) {
	base := Config{TemplateRegistry: "registry/t", BuildStorage: BuildStorage{Endpoint: "https://bucket.example", Bucket: "b", AccessKey: "a", SecretKey: "s"}, Abuse: AbusePolicy{Action: "pause"}}
	for _, missing := range []string{"registry", "endpoint", "bucket", "accessKey", "secretKey"} {
		cfg := base
		switch missing {
		case "registry":
			cfg.TemplateRegistry = ""
		case "endpoint":
			cfg.BuildStorage.Endpoint = ""
		case "bucket":
			cfg.BuildStorage.Bucket = ""
		case "accessKey":
			cfg.BuildStorage.AccessKey = ""
		case "secretKey":
			cfg.BuildStorage.SecretKey = ""
		}
		if err := cfg.ValidateAutomaticActions(); err == nil {
			t.Fatalf("accepted pause without %s", missing)
		}
	}
}

func TestNativeActionTargetRejectsOldSDKAndLegacyPods(t *testing.T) {
	cfg := testConfig()
	s := Spec{ID: "sbx-1234567890123456", ProjectID: "p", Template: "python", CPU: 1, MemoryGiB: 1}
	cr := BuildCR(cfg, "svc", s, time.Now())
	if !NativeActionTarget(cr) {
		t.Fatal("native runtime rejected")
	}
	cfg.Envd = true
	if NativeActionTarget(BuildCR(cfg, "svc", s, time.Now())) {
		t.Fatal("SDK pod accepted")
	}
	firstContainer(cr)["command"] = []any{"sleep", "infinity"}
	if NativeActionTarget(cr) {
		t.Fatal("legacy native pod accepted")
	}
}

func TestActionRejectsFrozenUnderlyingMetrics(t *testing.T) {
	now := time.Now()
	old := sampleAt(now, 100)
	cur := old
	cur.At = now.Add(time.Minute)
	cur.CPUAt = now.Add(-time.Hour)
	if StillInactive(old, cur, AbusePolicy{}, now.Add(time.Minute)) {
		t.Fatal("fresh envelope masked stale CPU")
	}
	for _, kind := range []string{"container", "restart", "missing"} {
		cur = old
		switch kind {
		case "container":
			cur.ContainerID = "containerd://replacement"
		case "restart":
			cur.RestartCount = 1
		case "missing":
			cur.ContainerID = ""
		}
		if StillInactive(old, cur, AbusePolicy{}, now) {
			t.Fatalf("accepted changed filesystem: %s", kind)
		}
	}
	cur = old
	cur.NetworkAt = time.Time{}
	if StillInactive(old, cur, AbusePolicy{}, now) {
		t.Fatal("missing network time accepted")
	}
}
func TestAbuseRejectsInvalidPolicy(t *testing.T) {
	for _, p := range []AbusePolicy{{Action: "delete"}, {WindowSeconds: 1}, {CPUPercent: 101}, {MaxGapSeconds: 200}} {
		if p.Validate() == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
}
