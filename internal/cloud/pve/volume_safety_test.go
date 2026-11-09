package pve

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const safetyVM = "/api2/json/nodes/pve1/qemu/101"

func TestExtendVolumeLiveGrowthHeadroom(t *testing.T) {
	const g = int64(1) << 30
	for _, tt := range []struct {
		name, disk, metrics, snapshots string
		size                           int
		headroom                       float64
		configStatus, storageStatus    int
		pending, merged                string
		wantOK                         bool
	}{
		{name: "only delta must fit not full target", disk: "size=32G", metrics: `"maxdisk":107374182400,"disk":85899345920`, size: 40, headroom: 10, wantOK: true},
		{name: "byte delta not rounded GiB", disk: "size=1536M", metrics: `"maxdisk":10737418240,"disk":10200547328`, size: 2, wantOK: true},
		{name: "delta consumes configured headroom", disk: "size=32G", metrics: `"maxdisk":107374182400,"disk":96636764160`, size: 40, headroom: 10},
		{name: "delta exceeds available", disk: "size=32G", metrics: `"maxdisk":107374182400,"disk":106300440576`, size: 40},
		{name: "missing total", disk: "size=32G", metrics: `"disk":0`, size: 40},
		{name: "missing used not fake zero", disk: "size=32G", metrics: `"maxdisk":107374182400`, size: 40},
		{name: "null used", disk: "size=32G", metrics: `"maxdisk":107374182400,"disk":null`, size: 40},
		{name: "negative used", disk: "size=32G", metrics: `"maxdisk":107374182400,"disk":-1`, size: 40},
		{name: "used exceeds total", disk: "size=32G", metrics: `"maxdisk":1,"disk":2`, size: 40},
		{name: "overflow capacity", disk: "size=32G", metrics: `"maxdisk":9223372036854775808,"disk":0`, size: 40},
		{name: "string capacity", disk: "size=32G", metrics: `"maxdisk":"107374182400","disk":0`, size: 40},
		{name: "zero total", disk: "size=32G", metrics: `"maxdisk":0,"disk":0`, size: 40},
		{name: "unknown capacity", disk: "size=32G", size: 40},
		{name: "capacity read forbidden", disk: "size=32G", size: 40, storageStatus: 403},
		{name: "current config forbidden", disk: "size=32G", size: 40, configStatus: 403},
		{name: "pending config", disk: "size=32G", size: 40, pending: `[{"key":"scsi1","pending":"nas:other,size=40G"}]`},
		{name: "null pending inventory", disk: "size=32G", size: 40, pending: `null`},
		{name: "current merged ambiguity", disk: "size=32G", size: 40, merged: "size=40G"},
		{name: "missing live size", size: 40},
		{name: "malformed live size", disk: "size=no", size: 40},
		{name: "zero live size", disk: "size=0G", size: 40},
		{name: "overflow live size", disk: "size=17179869185G", size: 40},
		{name: "duplicate size", disk: "size=32G,size=1G", size: 40},
		{name: "equal target stale cache", disk: "size=40G", size: 40},
		{name: "shrink target stale cache", disk: "size=50G", size: 40},
		{name: "negative target", disk: "size=32G", size: -1},
		{name: "target byte overflow", disk: "size=32G", size: int(math.MaxInt64>>30) + 1},
		{name: "invalid headroom", disk: "size=32G", size: 40, headroom: -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var writes, configQueries, storageReads int
			store := newMemHolder()
			liveSize := "local-zfs:vm-101-disk-1"
			if tt.disk != "" {
				liveSize += "," + tt.disk
			}
			s := volCluster(t, cfgAttached, map[string][]url.Values{}, fakeRoutes{
				"GET " + safetyVM + "/config": func(w http.ResponseWriter, r *http.Request) {
					configQueries++
					if tt.configStatus != 0 {
						w.WriteHeader(tt.configStatus)
						return
					}
					v := liveSize
					if r.URL.Query().Get("current") == "0" && tt.merged != "" {
						v = "local-zfs:vm-101-disk-1," + tt.merged
					}
					if r.URL.Query().Get("current") != "1" && r.URL.Query().Get("current") != "0" {
						t.Error("growth used merged-default config")
					}
					fmt.Fprintf(w, `{"data":{"smbios1":"uuid=safety","scsi1":%q}}`, v)
				},
				"GET " + safetyVM + "/pending": func(w http.ResponseWriter, _ *http.Request) {
					pending := tt.pending
					if pending == "" {
						pending = "[]"
					}
					fmt.Fprintf(w, `{"data":%s}`, pending)
				},
				"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != cAuthForTest() {
						t.Error("inventory did not use configured mutation credential")
					}
					if r.URL.Query().Get("type") != "storage" {
						fmt.Fprint(w, `{"data":[{"type":"qemu","vmid":101,"node":"pve1","pool":"tp-p1"}]}`)
						return
					}
					storageReads++
					if tt.storageStatus != 0 {
						w.WriteHeader(tt.storageStatus)
						return
					}
					if tt.metrics == "" {
						fmt.Fprint(w, `{"data":null}`)
						return
					}
					fmt.Fprintf(w, `{"data":[{"type":"storage","storage":"local-zfs","node":"pve1","status":"available",%s}]}`, tt.metrics)
				},
				"GET /api2/json/pools/tp-p1": say(`{"data":{"poolid":"tp-p1","members":[{"id":"qemu/101","type":"qemu","node":"pve1","vmid":101}]}}`),
				"PUT " + safetyVM + "/resize": func(w http.ResponseWriter, r *http.Request) {
					writes++
					if store.begins != 1 {
						t.Error("resize dispatched without exactly one durable intent")
					}
					_ = r.ParseForm()
					if r.Form.Get("disk") != "scsi1" || r.Form.Get("size") != fmt.Sprintf("%dG", tt.size) {
						t.Errorf("resize form %v", r.Form)
					}
					liveSize = fmt.Sprintf("local-zfs:vm-101-disk-1,size=%d", int64(tt.size)*g)
					fmt.Fprintf(w, `{"data":%q}`, volUPID)
				},
			})
			s.HolderOperations, s.HolderBudgets = store, smallHolderBudgets()
			err := s.ExtendVolume(context.Background(), "p1", 101, "local-zfs:vm-101-disk-1", tt.size, tt.headroom)
			if tt.wantOK {
				if err != nil || writes != 1 || store.begins != 1 || store.pending != nil || configQueries < 2 || storageReads != 1 {
					t.Fatalf("err=%v writes=%d begins=%d pending=%v reads=%d/%d", err, writes, store.begins, store.pending, configQueries, storageReads)
				}
				return
			}
			var ce *ConflictError
			if !errors.As(err, &ce) || writes != 0 || store.begins != 0 || store.pending != nil {
				t.Fatalf("err=%v writes=%d begins=%d pending=%v", err, writes, store.begins, store.pending)
			}
		})
	}
}

func cAuthForTest() string { return "PVEAPIToken=proxcloud@pve!t1=sec" }

func TestRetypeSnapshotPreflight(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		wantOK     bool
	}{
		{"current only real shape", `{"data":[{"name":"current","description":"You are here!","digest":"abc","running":1}]}`, 200, true},
		{"explicit empty list", `{"data":[]}`, 200, true},
		{"real snapshot with parent", `{"data":[{"name":"base","snaptime":1794222000,"vmstate":0},{"name":"after","parent":"base","snaptime":1794222010},{"name":"current","parent":"after","description":"You are here!"}]}`, 200, false},
		{"null array", `{"data":null}`, 200, false},
		{"missing array", `{}`, 200, false},
		{"object not list", `{"data":{}}`, 200, false},
		{"malformed", `{"data":[`, 200, false},
		{"null row", `{"data":[null]}`, 200, false},
		{"missing name", `{"data":[{}]}`, 200, false},
		{"duplicate current", `{"data":[{"name":"current"},{"name":"current"}]}`, 200, false},
		{"forbidden", `{}`, 403, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writes := map[string][]url.Values{}
			store := newMemHolder()
			s := volCluster(t, cfgAttached, writes, fakeRoutes{
				"GET " + safetyVM + "/snapshot": func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.status)
					fmt.Fprint(w, tt.body)
				},
			})
			// First prove the API's pre-mark check; it has no mutation intent.
			err := s.CheckRetypeVolumeSnapshots(context.Background(), "p1", 101)
			if (err == nil) != tt.wantOK {
				t.Fatalf("public preflight: %v", err)
			}
			if tt.wantOK {
				if err := s.RetypeVolume(context.Background(), "p1", 101, "local-zfs:vm-101-disk-1", "nas", 10); err != nil || len(writes["move"]) != 1 {
					t.Fatalf("err=%v writes=%v", err, writes)
				}
				return
			}
			s.HolderOperations, s.HolderBudgets = store, smallHolderBudgets()
			err = s.RetypeVolume(context.Background(), "p1", 101, "local-zfs:vm-101-disk-1", "nas", 10)
			var ce *ConflictError
			if !errors.As(err, &ce) || len(writes) != 0 || store.begins != 0 {
				t.Fatalf("err=%v writes=%v intents=%d", err, writes, store.begins)
			}
		})
	}
}

func TestDiskSizeBytesBounds(t *testing.T) {
	for _, raw := range []string{"", "d,size=-1G", "d,size=9223372036854775808", "d,size=8589934592G", "d,size=0", "d,size=1G,size=2G"} {
		if n, ok := diskSizeBytes(raw); ok {
			t.Errorf("accepted %q: %d", raw, n)
		}
	}
	if n, ok := diskSizeBytes("nas:d,size=1536M"); !ok || n != 1536<<20 {
		t.Fatalf("exact size: %d %v", n, ok)
	}
	if _, err := volumeTargetBytes(int(math.MaxInt64 >> 30)); err != nil {
		t.Fatal(err)
	}
}

func TestResizeServerGrowthRefusesBeforePowerOrConfig(t *testing.T) {
	for _, tt := range []struct {
		name, root, metrics string
		backendFailure      bool
	}{
		{"insufficient headroom", "size=32G", `"maxdisk":107374182400,"disk":96636764160`, false},
		{"unknown live root", "", `"maxdisk":107374182400,"disk":0`, false},
		{"backend refusal propagates", "size=32G", `"maxdisk":107374182400,"disk":0`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writes, grows := 0, 0
			routes := fakeRoutes{
				"GET " + safetyVM + "/config":         say(fmt.Sprintf(`{"data":{"scsi0":"local-zfs:root,%s"}}`, tt.root)),
				"GET " + safetyVM + "/pending":        say(`{"data":[]}`),
				"GET " + safetyVM + "/status/current": say(`{"data":{"status":"running"}}`),
				"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("type") == "storage" {
						fmt.Fprintf(w, `{"data":[{"type":"storage","storage":"local-zfs","node":"pve1","status":"available",%s}]}`, tt.metrics)
						return
					}
					fmt.Fprint(w, `{"data":[{"type":"qemu","vmid":101,"node":"pve1","pool":"tp-p1"}]}`)
				},
				"PUT " + safetyVM + "/resize": func(w http.ResponseWriter, _ *http.Request) {
					grows++
					w.WriteHeader(403)
				},
				"POST " + safetyVM + "/config":          func(http.ResponseWriter, *http.Request) { writes++ },
				"POST " + safetyVM + "/status/shutdown": func(http.ResponseWriter, *http.Request) { writes++ },
			}
			s := volCluster(t, cfgAttached, map[string][]url.Values{}, routes)
			err := s.ResizeServer(context.Background(), "p1", "101", ServerSpec{DiskGb: 40, Slug: "large", VCPUs: 2, RAMMb: 1024}, 10)
			if err == nil || writes != 0 || (grows != 0) != tt.backendFailure {
				t.Fatalf("err=%v power/config writes=%d grows=%d", err, writes, grows)
			}
			if !tt.backendFailure {
				var ce *ConflictError
				if !errors.As(err, &ce) || !strings.Contains(ce.Msg, "disk") {
					t.Fatalf("expected typed disk refusal: %v", err)
				}
			}
		})
	}
}
