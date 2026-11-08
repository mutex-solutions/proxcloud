package sandbox

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func ptr(v uint64) *uint64 { return &v }
func sampleAt(t time.Time, cpu uint64) Sample {
	return Sample{ProjectID: "p", SandboxID: "sbx-1234567890123456", CRUID: "cr", PodUID: "pod", Node: "n", At: t, CPUAt: t, NetworkAt: t, CPU: ptr(cpu), RX: ptr(5), TX: ptr(5), Cores: 1}
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
	for _, kind := range []string{"gap", "missing", "activity", "uid", "rollback", "stale", "policy"} {
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
func TestAbuseRejectsInvalidPolicy(t *testing.T) {
	for _, p := range []AbusePolicy{{Action: "delete"}, {WindowSeconds: 1}, {CPUPercent: 101}, {MaxGapSeconds: 200}} {
		if p.Validate() == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
}
