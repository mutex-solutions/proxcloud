package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

type fakeRoutes = map[string]func(http.ResponseWriter, *http.Request)

const (
	sharedDisk   = "nas:101/vm-101-disk-1.raw"
	movePath     = "POST /api2/json/nodes/pve1/qemu/101/move_disk"
	carrierPath  = "/api2/json/nodes/pve1/qemu/900"
	moveUPID     = "UPID:pve1:0:0:0:qmmove2:101:u:"
	moveStatus   = "GET /api2/json/nodes/pve1/tasks/" + moveUPID + "/status"
	carrierEmpty = `{"data":{"tags":"pc-holder;pc-carrier;proxcloud"}}`
)

func say(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }
}

func refuse(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprint(w, `{"errors":{"x":"refused"}}`)
}

// The destroy takes whatever the carrier holds when it runs, so it must only follow a move known to have stopped.
func TestCarrierKeptUnlessTheMoveStopped(t *testing.T) {
	lostTaskWait = 50 * time.Millisecond
	t.Cleanup(func() { lostTaskWait = time.Minute })
	moveStarts := say(fmt.Sprintf(`{"data":%q}`, moveUPID))
	polls := func(answers ...func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			answers[0](w, r)
			if len(answers) > 1 {
				answers = answers[1:]
			}
		}
	}
	running := say(`{"data":{"status":"running"}}`)
	failed := say(`{"data":{"status":"stopped","exitstatus":"boom"}}`)
	done := say(`{"data":{"status":"stopped","exitstatus":"OK"}}`)
	tests := []struct {
		name       string
		move, poll func(http.ResponseWriter, *http.Request)
		carrier    string
		wantDrop   int
	}{
		{"no answer to the move request", refuse, nil, carrierEmpty, 0},
		{"PVE refused the move: the source is locked", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"errors":{"x":"VM is locked (backup)"}}`)
		}, nil, carrierEmpty, 1},
		{"status unreadable", moveStarts, refuse, carrierEmpty, 0},
		{"status lost once, task still running", moveStarts, polls(refuse, running), carrierEmpty, 0},
		{"status lost once, the disk landed", moveStarts, polls(refuse, done), `{"data":{"tags":"pc-holder;pc-carrier","unused0":"nas:900/vm-900-disk-0.raw"}}`, 0},
		{"the move failed: nothing left to wait for", moveStarts, failed, carrierEmpty, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writes := map[string][]url.Values{}
			routes := fakeRoutes{
				movePath:                         tt.move,
				"GET " + carrierPath + "/config": say(tt.carrier),
				"DELETE " + carrierPath: func(w http.ResponseWriter, _ *http.Request) {
					writes["drop"] = append(writes["drop"], nil)
					fmt.Fprintf(w, `{"data":%q}`, volUPID)
				},
			}
			if tt.poll != nil {
				routes[moveStatus] = tt.poll
			}
			s := volCluster(t, cfgSharedUnused, writes, routes)
			if err := s.AttachVolume(context.Background(), "p1", 101, sharedDisk, "103"); err == nil {
				t.Fatal("want the attach to fail")
			}
			if len(writes["drop"]) != tt.wantDrop {
				t.Fatalf("carrier deletes = %d, want %d", len(writes["drop"]), tt.wantDrop)
			}
		})
	}
}

// A carrier is about to leave its node, and another operation's disk would go with it.
func TestHolderOnSkipsCarrier(t *testing.T) {
	rows := func(tags string) fakeRoutes {
		return fakeRoutes{"GET /api2/json/cluster/resources": say(fmt.Sprintf(`{"data":[
			{"id":"qemu/900","type":"qemu","vmid":900,"name":"pc-holder-pve1","node":"pve1","pool":"tp-p1","tags":%q}]}`, tags))}
	}
	for tags, wantCreates := range map[string]int{"pc-holder;proxcloud": 0, "pc-holder;pc-carrier;proxcloud": 1} {
		writes := map[string][]url.Values{}
		s := volCluster(t, cfgLocalUnused, writes, rows(tags))
		if _, err := s.holderOn(context.Background(), "p1", "pve1"); err != nil {
			t.Fatal(err)
		}
		if len(writes["create"]) != wantCreates {
			t.Fatalf("tags %q: creates = %d, want %d", tags, len(writes["create"]), wantCreates)
		}
	}
}

// Untagged it would count as a server: a bill and a row the customer never asked for.
func TestNewHolderRemovesTheVMOnFailure(t *testing.T) {
	var deletes int
	s := volCluster(t, cfgSharedUnused, map[string][]url.Values{}, fakeRoutes{
		"POST " + carrierPath + "/config": refuse,
		"DELETE " + carrierPath: func(w http.ResponseWriter, _ *http.Request) {
			deletes++
			fmt.Fprintf(w, `{"data":%q}`, volUPID)
		},
	})
	if err := s.AttachVolume(context.Background(), "p1", 101, sharedDisk, "103"); err == nil {
		t.Fatal("want the attach to fail")
	}
	if deletes != 1 {
		t.Fatalf("deletes = %d, want the half-made VM gone", deletes)
	}
}

// A create POST with no answer may have made the VM, and only the POST's own blank VM may be removed for it.
func TestNewHolderLostCreateAnswer(t *testing.T) {
	lostTaskWait = 50 * time.Millisecond
	t.Cleanup(func() { lostTaskWait = time.Minute })
	const createPath = "POST /api2/json/nodes/pve1/qemu"
	answer := func(status int, body string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		}
	}
	blank := `{"data":{"boot":" ","meta":"creation-qemu=11.1.3,ctime=1","smbios1":"uuid=x","vmgenid":"x"}}` // what PVE 9.2 leaves
	tests := []struct {
		name     string
		create   func(http.ResponseWriter, *http.Request)
		cfg      string
		wantDrop int
	}{
		{"no answer, blank VM in the pool", refuse, blank, 1},
		{"no answer, the vmid went to someone's named VM", refuse, `{"data":{"name":"web"}}`, 0},
		{"no answer, the vmid went to a tagged VM", refuse, `{"data":{"tags":"proxcloud"}}`, 0},
		{"no answer, the vmid went to a VM with a disk", refuse, `{"data":{"scsi0":"local-zfs:vm-900-disk-0,size=1G"}}`, 0},
		{"refused: nothing was created", answer(http.StatusForbidden, `{"errors":{"x":"denied"}}`), blank, 0},
		{"vmid taken by a racing create", answer(http.StatusInternalServerError, `{"errors":{"x":"VM 900 already exists on node pve2"}}`), blank, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var deletes int
			s := volCluster(t, cfgLocalUnused, map[string][]url.Values{}, fakeRoutes{
				createPath:                       tt.create,
				"GET " + carrierPath + "/config": say(tt.cfg),
				"DELETE " + carrierPath: func(w http.ResponseWriter, _ *http.Request) {
					deletes++
					fmt.Fprintf(w, `{"data":%q}`, volUPID)
				},
			})
			if _, err := s.newHolder(context.Background(), "p1", "pve1", tagHolder); err == nil {
				t.Fatal("want the create to fail")
			}
			if deletes != tt.wantDrop {
				t.Fatalf("deletes = %d, want %d", deletes, tt.wantDrop)
			}
		})
	}
}

// Another operation could see the holder empty and destroy it as the disk lands, so a move in doubt keeps the lock.
func TestKeepServerVolumesHoldsTheLockWhileAMoveIsInDoubt(t *testing.T) {
	lostTaskWait = 50 * time.Millisecond
	t.Cleanup(func() { lostTaskWait = time.Minute })
	moveStarts := say(fmt.Sprintf(`{"data":%q}`, moveUPID))
	tests := []struct {
		name        string
		move, poll  func(http.ResponseWriter, *http.Request)
		wantRelease bool
	}{
		{"no answer to the move request", refuse, nil, false},
		{"PVE refused the move: the source is locked", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"errors":{"x":"VM is locked (backup)"}}`)
		}, nil, true},
		{"status unreadable", moveStarts, refuse, false},
		{"task still running at the deadline", moveStarts, say(`{"data":{"status":"running"}}`), false},
		{"the move failed: nothing left to wait for", moveStarts, say(`{"data":{"status":"stopped","exitstatus":"boom"}}`), true},
		{"the move worked", moveStarts, say(`{"data":{"status":"stopped","exitstatus":"OK"}}`), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routes := fakeRoutes{
				movePath:                         tt.move,
				"GET " + carrierPath + "/config": say(`{"data":{"tags":"pc-holder;proxcloud"}}`),
			}
			if tt.poll != nil {
				routes[moveStatus] = tt.poll
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			s := volCluster(t, cfgLocalUnused, map[string][]url.Values{}, routes)
			released := false
			s.HolderLock = func(context.Context, string, time.Duration, time.Duration) (func(), bool, error) {
				return func() { released = true }, true, nil
			}
			err := s.KeepServerVolumes(ctx, "p1", "101")
			if released != tt.wantRelease {
				t.Fatalf("released = %v, want %v (err %v)", released, tt.wantRelease, err)
			}
		})
	}
}

// Everything that picks, fills or drops a holder runs under the lock, and refuses when it is busy.
func TestHolderOperationsTakeTheLock(t *testing.T) {
	ops := []struct {
		name, cfg string
		run       func(*Service) error
	}{
		{"attach", cfgSharedUnused, func(s *Service) error {
			return s.AttachVolume(context.Background(), "p1", 101, sharedDisk, "103")
		}},
		{"delete", cfgLocalUnused, func(s *Service) error {
			return s.DeleteVolume(context.Background(), "p1", 101, "local-zfs:vm-101-disk-1")
		}},
		{"create detached", cfgLocalUnused, func(s *Service) error {
			_, err := s.CreateDetachedVolume(context.Background(), "p1", "pve1", "local-zfs", 1, 0)
			return err
		}},
		{"keep", cfgLocalUnused, func(s *Service) error {
			return s.KeepServerVolumes(context.Background(), "p1", "101")
		}},
	}
	holderCfg := fakeRoutes{"GET " + carrierPath + "/config": say(`{"data":{"tags":"pc-holder;proxcloud"}}`)}
	count := func(w map[string][]url.Values) (n int) {
		for _, v := range w {
			n += len(v)
		}
		return n
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			writes := map[string][]url.Values{}
			s := volCluster(t, op.cfg, writes, holderCfg)
			s.HolderLock = func(context.Context, string, time.Duration, time.Duration) (func(), bool, error) {
				return nil, false, nil
			}
			var ce *ConflictError
			if err := op.run(s); !errors.As(err, &ce) || count(writes) != 0 {
				t.Fatalf("busy lock: err = %v, writes = %v", err, writes)
			}

			atRelease := -1
			s.HolderLock = func(_ context.Context, name string, _, _ time.Duration) (func(), bool, error) {
				if name != "pve-holders-p1" || count(writes) != 0 {
					t.Errorf("lock %q taken after %d writes", name, count(writes))
				}
				return func() { atRelease = count(writes) }, true, nil
			}
			if err := op.run(s); err != nil {
				t.Fatal(err)
			}
			if atRelease != count(writes) || atRelease == 0 {
				t.Fatalf("writes at release = %d, total = %d", atRelease, count(writes))
			}
		})
	}
}

// A dropped holder stays listed for a moment, and its rows go. Any other VM that cannot be read keeps its rows, because
// Reconcile archives what the list leaves out.
func TestVolumeListWithVMsGone(t *testing.T) {
	gone := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"errors":{"vmid":"Configuration file 'nodes/pve1/qemu-server/1.conf' does not exist"}}`)
	}
	vols := func(s *Service, list func(*VolumeSyncProvider) ([]string, error)) ([]string, error) {
		return list(s.VolumeSyncProvider("r", "p1", nil))
	}
	strict := func(p *VolumeSyncProvider) ([]string, error) {
		rows, err := p.List(context.Background())
		return volIDs(rows), err
	}
	readable := func(p *VolumeSyncProvider) ([]string, error) {
		rows, err := p.ListReadable(context.Background())
		return volIDs(rows), err
	}

	nodes := say(`{"data":[{"node":"pve1","status":"online"},{"node":"pve2","status":"online"}]}`)
	const pve2Holder = "GET /api2/json/nodes/pve2/qemu/900/config"
	s := volCluster(t, cfgLocalUnused, map[string][]url.Values{}, fakeRoutes{
		"GET /api2/json/nodes": nodes, "GET " + carrierPath + "/config": gone, pve2Holder: gone,
	})
	if got, err := vols(s, strict); err != nil || len(got) != 4 {
		t.Fatalf("holder gone: %v, %v", got, err)
	}

	// Mid-migrate the list still names the old node. Left out, the disk's row would be archived and go unbilled.
	s = volCluster(t, cfgLocalUnused, map[string][]url.Values{}, fakeRoutes{
		"GET /api2/json/nodes": nodes, "GET " + carrierPath + "/config": gone,
		pve2Holder: say(`{"data":{"tags":"pc-holder;proxcloud","unused0":"nas:900/vm-900-disk-0.raw"}}`),
	})
	rows, err := s.VolumeSyncProvider("r", "p1", nil).List(context.Background())
	if err != nil || len(rows) != 5 || rows[4].Data["volume"].(map[string]any)["availability_zone"] != "pve2" {
		t.Fatalf("holder migrating: %v, %v", rows, err)
	}

	// The node that could have it cannot say, so nothing may be archived.
	s = volCluster(t, cfgLocalUnused, map[string][]url.Values{}, fakeRoutes{
		"GET /api2/json/nodes": nodes, "GET " + carrierPath + "/config": gone, pve2Holder: refuse,
	})
	prov := s.VolumeSyncProvider("r", "p1", nil)
	if got, err := strict(prov); err != nil || len(got) != 4 {
		t.Fatalf("node unreadable: List = %v, %v", got, err)
	}
	if keep, _ := prov.KeepUnread(&cloud.CloudResource{Data: map[string]any{"volume": map[string]any{"vmid": 900}}}); !keep {
		t.Fatal("node unreadable: the holder's rows must stay")
	}
	if got, err := vols(s, readable); err != nil || len(got) != 4 {
		t.Fatalf("node unreadable: ListReadable = %v, %v", got, err)
	}

	s = volCluster(t, cfgLocalUnused, map[string][]url.Values{}, fakeRoutes{
		"GET /api2/json/nodes": nodes, "GET " + carrierPath + "/config": gone, pve2Holder: gone,
		"GET /api2/json/nodes/pve1/qemu/102/config": gone,
	})
	// Its rows stay this pass: Reconcile takes them for unread rather than gone (next pass, unlisted, they go).
	if got, err := vols(s, strict); err != nil || len(got) != 3 {
		t.Fatalf("server unread: List = %v, %v", got, err)
	}
	if got, err := vols(s, readable); err != nil || len(got) != 3 {
		t.Fatalf("server gone: ListReadable = %v, %v", got, err)
	}
}

func volIDs(rows []cloud.CloudResource) (out []string) {
	for _, r := range rows {
		out = append(out, r.ExternalID)
	}
	return out
}
