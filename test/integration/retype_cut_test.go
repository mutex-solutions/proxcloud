//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

const (
	cutUPID   = "UPID:pve1:0:0:0:qmmove:101:u:"
	cutOldVol = "local-zfs:vm-101-disk-1"
	cutNewVol = "nas:101/vm-101-disk-1.raw"
)

// cutPVE fakes one VM whose scsi1 moves from local-zfs to nas: once started, the move runs until done is set, then
// ends with failed (if given) or OK. lost answers the first status read with a 502.
func cutPVE(t *testing.T, done *atomic.Bool, lost bool, polled chan<- struct{}, failed ...string) *pve.Service {
	t.Helper()
	exit := "OK"
	if len(failed) > 0 {
		exit = failed[0]
	}
	const g = 1 << 30
	var polls atomic.Int32
	var started atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api2/json/cluster/resources", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") == "storage" {
			fmt.Fprintf(w, `{"data":[
				{"type":"storage","storage":"local-zfs","node":"pve1","status":"available","maxdisk":%d,"disk":0},
				{"type":"storage","storage":"nas","node":"pve1","status":"available","maxdisk":%d,"disk":0}]}`, 100*g, 100*g)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"qemu/101","type":"qemu","vmid":101,"name":"a","node":"pve1","pool":"pc-p1"}]}`)
	})
	mux.HandleFunc("GET /api2/json/storage", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"storage":"local-zfs","type":"zfspool","content":"images"},{"storage":"nas","type":"nfs","content":"images"}]}`)
	})
	mux.HandleFunc("GET /api2/json/nodes/pve1/qemu/101/config", func(w http.ResponseWriter, _ *http.Request) {
		vol := cutOldVol
		if started.Load() && done.Load() && exit == "OK" {
			vol = cutNewVol
		}
		fmt.Fprintf(w, `{"data":{"name":"a","scsi0":"local-zfs:vm-101-disk-0,size=10G","scsi1":"%s,size=20G"}}`, vol)
	})
	mux.HandleFunc("POST /api2/json/nodes/pve1/qemu/101/move_disk", func(w http.ResponseWriter, _ *http.Request) {
		started.Store(true)
		fmt.Fprintf(w, `{"data":%q}`, cutUPID)
	})
	mux.HandleFunc("GET /api2/json/nodes/pve1/tasks/"+cutUPID+"/status", func(w http.ResponseWriter, _ *http.Request) {
		select {
		case polled <- struct{}{}:
		default:
		}
		if polls.Add(1) == 1 && lost {
			http.Error(w, "refused", http.StatusBadGateway)
			return
		}
		if done.Load() {
			fmt.Fprintf(w, `{"data":{"status":"stopped","exitstatus":%q}}`, exit)
			return
		}
		fmt.Fprint(w, `{"data":{"status":"running"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// The ingress cuts a retype's request after 60 s while the disk is still copying. The wait, the follow and the mark
// clear must not die with it: a row left on the old volid with its mark on is billed next to the new disk's row.
func TestRetypeOutlivesItsRequest(t *testing.T) {
	db := freshPG(t)
	repo := cloud.NewRepo(db)
	h := project.NewHandler(project.NewService(project.NewRepo(db), nil, nil, nil, nil), nil, nil, nil, nil, nil, repo, nil, nil, nil, "r")
	es := &externalservice.ExternalService{ID: "svc"}
	proj := &project.Project{ID: "p1"}

	seed := func(t *testing.T) *cloud.CloudResource {
		if _, err := db.C("cloudResource").DeleteMany(context.Background(), pgdoc.M{}); err != nil {
			t.Fatal(err)
		}
		past := time.Now().UTC().Add(-time.Hour)
		row, err := repo.Insert(context.Background(), &cloud.CloudResource{ServiceID: "svc", ProjectID: "p1", Type: cloud.TypeVolume, ExternalID: cutOldVol,
			CreatedAt: &past, UpdatedAt: &past, Data: map[string]any{"volume": map[string]any{"vmid": 101, "slot": "scsi1"}}})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if err := repo.MarkMoving(context.Background(), row, &now); err != nil {
			t.Fatal(err)
		}
		return row
	}
	followed := func(t *testing.T, row *cloud.CloudResource) {
		t.Helper()
		got, err := repo.FindByServiceAndType(context.Background(), "svc", cloud.TypeVolume)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != row.ID || got[0].ExternalID != cutNewVol || got[0].MovingSince != nil {
			t.Fatalf("want row %s on %s, unmarked: %+v", row.ID, cutNewVol, got)
		}
	}
	run := func(ctx context.Context, ps *pve.Service, row *cloud.CloudResource) <-chan error {
		errc := make(chan error, 1)
		go func() { errc <- h.RetypeVolumeRow(ctx, ps, es, proj, row, 101, "scsi1", "nas") }()
		return errc
	}
	wait := func(t *testing.T, errc <-chan error) error {
		t.Helper()
		select {
		case err := <-errc:
			return err
		case <-time.After(30 * time.Second):
			t.Fatal("the retype did not return")
			return nil
		}
	}

	t.Run("request cut mid-move", func(t *testing.T) {
		var done atomic.Bool
		polled := make(chan struct{}, 1)
		ps := cutPVE(t, &done, false, polled)
		row := seed(t)
		ctx, cancel := context.WithCancel(context.Background())
		errc := run(ctx, ps, row)
		<-polled // the move runs
		cancel()
		time.Sleep(500 * time.Millisecond) // well past the wait's next poll
		done.Store(true)
		if err := wait(t, errc); err != nil {
			t.Fatalf("a move that ended answered %v", err)
		}
		followed(t, row)
	})

	// One status read refused mid-move, the move ends fine: the tenant gets a success, not a retype that failed.
	t.Run("lost status read", func(t *testing.T) {
		var done atomic.Bool
		done.Store(true)
		ps := cutPVE(t, &done, true, make(chan struct{}, 1))
		row := seed(t)
		if err := wait(t, run(context.Background(), ps, row)); err != nil {
			t.Fatalf("the disk is at its new place, yet: %v", err)
		}
		followed(t, row)
	})

	// A move that failed leaves the disk where it was: the error stays, and the row stays on the old volid, unmarked.
	t.Run("move failed", func(t *testing.T) {
		var done atomic.Bool
		done.Store(true)
		ps := cutPVE(t, &done, false, make(chan struct{}, 1), "storage migration failed")
		row := seed(t)
		if err := wait(t, run(context.Background(), ps, row)); err == nil {
			t.Fatal("a failed move answered success")
		}
		got, _ := repo.FindByID(context.Background(), row.ID)
		if got == nil || got.ExternalID != cutOldVol || got.MovingSince != nil {
			t.Fatalf("row after a failed move: %+v", got)
		}
	})
}

// A retype's follow that lands between a sync's listing and its read of the rows: the list still shows the old volid,
// the row has the new one. The row must stay, and no row may come back for the old volid.
func TestReconcileAfterAFollowMidSync(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := cloud.NewRepo(db)
	past := time.Now().UTC().Add(-time.Hour)
	vol := func(ext string) cloud.CloudResource {
		return cloud.CloudResource{ServiceID: "svc", ProjectID: "p", Type: cloud.TypeVolume, ExternalID: ext,
			Data: map[string]any{"volume": map[string]any{"vmid": 101, "slot": "scsi1"}}}
	}
	seed := vol("fast:vm-101-disk-1")
	seed.CreatedAt, seed.UpdatedAt = &past, &past
	row, err := repo.Insert(ctx, &seed)
	if err != nil {
		t.Fatal(err)
	}
	p := &followMidList{reconcileProvScoped: reconcileProvScoped{typ: cloud.TypeVolume, projectID: "p", list: []cloud.CloudResource{vol("fast:vm-101-disk-1")}},
		follow: func() error { return repo.ChangeExternalID(ctx, row, "slow:vm-101-disk-0") }}
	if _, err := providers.Reconcile(ctx, p, repo, "svc", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err := repo.FindByServiceAndType(ctx, "svc", cloud.TypeVolume)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != row.ID || got[0].ExternalID != "slow:vm-101-disk-0" {
		t.Fatalf("want one row %s on the new volid: %+v", row.ID, got)
	}
	if n, _ := db.C("cloudResourceHistory").Count(ctx, pgdoc.M{"cloudResourceId": row.ID}); n != 0 {
		t.Fatalf("the retyped row was archived (%d)", n)
	}
	// The next sync lists the new volid and keeps the row as it is.
	p.follow, p.list = nil, []cloud.CloudResource{vol("slow:vm-101-disk-0")}
	if _, err := providers.Reconcile(ctx, p, repo, "svc", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.FindByServiceAndType(ctx, "svc", cloud.TypeVolume); len(got) != 1 || got[0].ID != row.ID {
		t.Fatalf("after the next sync: %+v", got)
	}
}

// followMidList runs a retype's follow right after it lists, before Reconcile reads the rows.
type followMidList struct {
	reconcileProvScoped
	follow func() error
}

func (p *followMidList) List(ctx context.Context) ([]cloud.CloudResource, error) {
	list, err := p.reconcileProvScoped.List(ctx)
	if err == nil && p.follow != nil {
		err = p.follow()
	}
	return list, err
}
