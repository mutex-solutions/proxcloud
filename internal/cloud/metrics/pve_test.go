package metrics

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

func TestCounterDelta(t *testing.T) {
	for _, c := range []struct{ prev, cur, want int64 }{
		{100, 150, 50},
		{100, 100, 0},
		{100, 30, 30}, // the tap was recreated (stop/start, migration): all of it is new
		{0, 42, 42},
	} {
		if got := counterDelta(c.prev, c.cur); got != c.want {
			t.Errorf("delta(%d,%d) = %d, want %d", c.prev, c.cur, got, c.want)
		}
	}
}

func TestNICCounterApply(t *testing.T) {
	var c NICCounter
	c.apply("2026-11", 100, 10) // first sample counts in full: the tap started with the NIC
	c.apply("2026-11", 150, 15)
	c.apply("2026-11", 20, 5) // reset
	if c.InBytes != 170 || c.OutBytes != 20 {
		t.Fatalf("after a reset: in=%d out=%d, want 170/20", c.InBytes, c.OutBytes)
	}
	c.apply("2026-12", 50, 25) // the new month starts at 0 and counts only what moved since
	if c.InBytes != 30 || c.OutBytes != 20 || c.Month != "2026-12" {
		t.Fatalf("after rollover: in=%d out=%d month=%s, want 30/20", c.InBytes, c.OutBytes, c.Month)
	}
}

func TestTapSample(t *testing.T) {
	nics := map[string]pve.NICCounters{"tap105i1": {NetIn: 7, NetOut: 9}}
	tap, s, ok := tapSample("105", "net1", nics)
	if !ok || tap != "tap105i1" || s.NetIn != 7 || s.NetOut != 9 {
		t.Fatalf("got %s %+v %v", tap, s, ok)
	}
	if _, _, ok := tapSample("105", "net0", nics); ok {
		t.Fatal("net0 has no tap here")
	}
	if _, _, ok := tapSample("105", "scsi0", nics); ok {
		t.Fatal("not a NIC")
	}
}

func TestIsPublicTrafficSourceClassified(t *testing.T) {
	yes, no := true, false
	if !isPublicTraffic(Resource{Name: "tap105i1", Public: &yes}, nil, nil) {
		t.Fatal("the source said public")
	}
	if isPublicTraffic(Resource{Name: "tap105i1", Public: &no}, nil, nil) {
		t.Fatal("the source said private")
	}
}
