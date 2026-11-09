//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

func artifactPVE(t *testing.T, db *pgdoc.DB) (*pve.Service, *atomic.Int32, *atomic.Int32, func()) {
	t.Helper()
	deletes, uploads := new(atomic.Int32), new(atomic.Int32)
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/access/permissions", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		priv := "VM.Audit"
		if p == "/" || p == "/nodes" || p == "/access" {
			priv = "Sys.Audit"
		}
		if p == "/storage" {
			priv = "Datastore.Audit"
		}
		reply(w, map[string]any{p: map[string]int{priv: 1}})
	})
	mux.HandleFunc("/api2/json/access/acl", func(w http.ResponseWriter, r *http.Request) { reply(w, []any{}) })
	mux.HandleFunc("/api2/json/cluster/resources", func(w http.ResponseWriter, r *http.Request) { reply(w, []any{}) })
	mux.HandleFunc("/api2/json/cluster/status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, []map[string]any{{"type": "cluster", "name": "test", "quorate": 1}, {"type": "node", "name": "n1", "nodeid": 1, "ip": "192.0.2.10"}})
	})
	mux.HandleFunc("/api2/json/storage/iso", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"type": "nfs", "shared": 1, "server": "192.0.2.1", "export": "/seeds"})
	})
	mux.HandleFunc("/api2/json/storage", func(w http.ResponseWriter, r *http.Request) {
		reply(w, []map[string]any{{"storage": "iso", "type": "nfs", "shared": 1, "content": "iso"}})
	})
	mux.HandleFunc("/api2/json/nodes", func(w http.ResponseWriter, r *http.Request) { reply(w, []pve.Node{{Node: "n1", Status: "online"}}) })
	mux.HandleFunc("/api2/json/nodes/n1/storage/iso/content/", func(w http.ResponseWriter, r *http.Request) { deletes.Add(1); http.Error(w, "denied", 403) })
	mux.HandleFunc("/api2/json/nodes/n1/storage/iso/upload", func(w http.ResponseWriter, r *http.Request) {
		f, h, err := r.FormFile("filename")
		if err != nil {
			http.Error(w, "bad multipart", 400)
			return
		}
		defer f.Close()
		// Every upload must already have its exact artifact journal committed.
		var rows []pve.SeedArtifact
		if err := db.C("pveSeedArtifact").Find(r.Context(), pgdoc.M{"volume": "iso:iso/" + h.Filename}, &rows); err != nil || len(rows) != 1 {
			t.Error("upload preceded journal")
			http.Error(w, "no journal", 500)
			return
		}
		uploads.Add(1)
		reply(w, "UPID:n1:0:0:0:imgcopy::t:")
	})
	mux.HandleFunc("/api2/json/nodes/n1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
	})
	srv := httptest.NewServer(mux)
	svc, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	svc.SeedReader = svc.C
	svc.SeedArtifacts = pve.NewSeedArtifactRepo(db)
	return svc, deletes, uploads, srv.Close
}
func TestSeedJournalBeforeUploadAndGuardedFallback(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	svc, deletes, uploads, done := artifactPVE(t, db)
	defer done()
	a, err := svc.SeedArtifacts.Prepare(ctx, svc.C, pve.SeedArtifact{ServiceID: "svc", Node: "n1", SeedID: "source", ServerID: "7", InstanceID: "iid-a"}, "iso")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(a.Volume, "-7-") || len(a.ID) != 32 || a.InstallationID == "" {
		t.Fatal("artifact lacks immutable random ownership")
	}
	if err := svc.DeleteSeedVolume(ctx, "n1", a.Volume); !errors.Is(err, pve.ErrSeedCleanupPending) {
		t.Fatal("first no-ref scan must wait")
	}
	if deletes.Load() != 0 || uploads.Load() != 0 {
		t.Fatal("grace was bypassed")
	}
	old := time.Now().Add(-25 * time.Hour)
	if _, err := db.C("pveSeedArtifact").SetByID(ctx, a.ID, pgdoc.M{"noRefsAt": old}, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSeedVolume(ctx, "n1", a.Volume); err != nil {
		t.Fatal(err)
	}
	if deletes.Load() != 1 || uploads.Load() != 1 {
		t.Fatal("confirmed no-ref owned file should use denied-delete empty fallback")
	}
	var saved pve.SeedArtifact
	if ok, err := db.C("pveSeedArtifact").Get(ctx, a.ID, &saved); err != nil || !ok || saved.State != "emptied" {
		t.Fatal("empty stub lost durable status")
	}
}
func TestSeedJournalUnknownMutationAndForeignRowsNeverErase(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	svc, deletes, uploads, done := artifactPVE(t, db)
	defer done()
	a, err := svc.SeedArtifacts.Prepare(ctx, svc.C, pve.SeedArtifact{ServiceID: "svc", Node: "n1"}, "iso")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if _, err := db.C("pveSeedArtifact").SetByID(ctx, a.ID, pgdoc.M{"noRefsAt": old, "mutationPending": true}, nil); err != nil {
		t.Fatal(err)
	}
	for _, vol := range []string{a.Volume, "iso:iso/pcseed-7-x.iso", strings.Replace(a.Volume, a.InstallationID, strings.Repeat("f", 32), 1)} {
		if err := svc.DeleteSeedVolume(ctx, "n1", vol); err == nil {
			t.Fatal("unsafe candidate accepted")
		}
	}
	if deletes.Load() != 0 || uploads.Load() != 0 {
		t.Fatal("unproven candidate mutated PVE")
	}
}

// Simulate crashes between prepared source and upload/attach or final source
// pointer persistence: ISO=""/old cannot authorize destroying the only source.
func TestSeedSourceSurvivesAllLinkedArtifactFailures(t *testing.T) {
	for _, oldPointer := range []bool{false, true} {
		t.Run(fmt.Sprint(oldPointer), func(t *testing.T) {
			ctx := context.Background()
			db := freshPG(t)
			svc, deletes, uploads, done := artifactPVE(t, db)
			defer done()
			enc := textcrypt.New("test-key")
			source := pgdoc.M{"_id": "source", "serviceId": "svc", "projectId": "p", "serverId": "7", "instanceId": "iid-current", "retired": true, "mode": "iso", "userData": enc.Encrypt("source-sentinel")}
			if oldPointer {
				source["iso"] = "iso:iso/pcseed-7-old.iso"
			}
			mustInsert(t, db, "pveServerSeed", source)
			for _, iid := range []string{"iid-old", "iid-current"} {
				a, err := svc.SeedArtifacts.Prepare(ctx, svc.C, pve.SeedArtifact{ServiceID: "svc", Node: "n1", SeedID: "source", ServerID: "7", InstanceID: iid}, "iso")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.C("pveSeedArtifact").SetByID(ctx, a.ID, pgdoc.M{"mutationPending": true}, nil); err != nil {
					t.Fatal(err)
				}
			}
			h := project.NewHandler(project.NewService(project.NewRepo(db), nil, nil, nil, nil), nil, nil, nil, nil, nil, cloud.NewRepo(db), nil, nil, nil, "r")
			h.SetSeeds(project.NewSeedRepo(db, enc))
			if err := h.DropStaleSeeds(ctx, svc, &externalservice.ExternalService{ID: "svc"}); err != nil {
				t.Fatal(err)
			}
			var kept project.ServerSeed
			found, err := db.C("pveServerSeed").Get(ctx, "source", &kept)
			if err != nil || !found || enc.Decrypt(kept.UserData) != "source-sentinel" {
				t.Fatal("encrypted source lost after upload/final-pointer crash")
			}
			if rows, err := svc.SeedArtifacts.ForSeed(ctx, "svc", "source"); err != nil || len(rows) != 2 {
				t.Fatal("superseded artifact generation lost")
			}
			if deletes.Load() != 0 || uploads.Load() != 0 {
				t.Fatal("unknown task race erased an artifact")
			}
		})
	}
}

func TestSeedRecoveryPinBlocksEarlierAndLaterGenerations(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	svc, deletes, uploads, done := artifactPVE(t, db)
	defer done()
	before, err := svc.SeedArtifacts.Prepare(ctx, svc.C, pve.SeedArtifact{ServiceID: "svc", Node: "n1", ServerID: "7"}, "iso")
	if err != nil {
		t.Fatal(err)
	}
	// Even a dispatch lost before PVE replies leaves the pin committed.
	if err := svc.SeedArtifacts.PinRecovery(ctx, "svc", "7"); err != nil {
		t.Fatal(err)
	}
	after, err := svc.SeedArtifacts.Prepare(ctx, svc.C, pve.SeedArtifact{ServiceID: "svc", Node: "n1", ServerID: "7"}, "iso")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []*pve.SeedArtifact{before, after} {
		if _, err := db.C("pveSeedArtifact").SetByID(ctx, a.ID, pgdoc.M{"noRefsAt": time.Now().Add(-48 * time.Hour)}, nil); err != nil {
			t.Fatal(err)
		}
		if err := svc.DeleteSeedVolume(ctx, "n1", a.Volume); !errors.Is(err, pve.ErrSeedCleanupPending) {
			t.Fatal("recovery pin did not retain generation")
		}
	}
	if deletes.Load() != 0 || uploads.Load() != 0 {
		t.Fatal("recovery-pinned seed was erased")
	}
}

func TestSeedLockConnectionLossKeepsDurableFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db := freshPG(t)
	r := pve.NewSeedArtifactRepo(db)
	started, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- r.Locked(ctx, "svc", func(context.Context) error { close(started); <-release; return nil })
	}()
	<-started
	// Only the session advisory holder on this test's unique DB; callbacks use
	// independent primary connections, exactly like a transport loss in production.
	_, err := db.Pool.Exec(ctx, `SELECT pg_terminate_backend(a.pid) FROM pg_stat_activity a JOIN pg_locks l ON a.pid=l.pid WHERE a.datname=current_database() AND a.pid<>pg_backend_pid() AND l.locktype='advisory' AND l.granted`)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	called := false
	err = r.Locked(ctx, "svc", func(context.Context) error { called = true; return nil })
	close(release)
	if first := <-finished; first != nil {
		t.Fatal(first)
	}
	if err == nil || called {
		t.Fatal("lost session lock allowed another callback through durable operation fence")
	}
	if err := r.Locked(ctx, "svc", func(context.Context) error { return nil }); err != nil {
		t.Fatal("known completed operation did not release durable claim")
	}
}

func TestSeedAttachWriteAheadCrashBoundaries(t *testing.T) {
	for _, failAt := range []string{"success", "upload", "attach", "upload-null", "attach-null"} {
		t.Run(failAt, func(t *testing.T) {
			ctx := context.Background()
			db := freshPG(t)
			var cfg = pve.VMConfig{"name": "fresh", "digest": "d"}
			reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
			mux := http.NewServeMux()
			mux.HandleFunc("/api2/json/cluster/status", func(w http.ResponseWriter, r *http.Request) {
				reply(w, []map[string]any{{"type": "cluster", "name": "test", "quorate": 1}, {"type": "node", "name": "n1", "nodeid": 1, "ip": "192.0.2.10"}})
			})
			mux.HandleFunc("/api2/json/storage/iso", func(w http.ResponseWriter, r *http.Request) { reply(w, map[string]any{"type": "dir", "path": "/test"}) })
			mux.HandleFunc("/api2/json/nodes/n1/storage/iso/upload", func(w http.ResponseWriter, r *http.Request) {
				f, h, err := r.FormFile("filename")
				if err != nil {
					t.Error(err)
					http.Error(w, "bad multipart", 400)
					return
				}
				defer f.Close()
				var rows []pve.SeedArtifact
				if err := db.C("pveSeedArtifact").Find(ctx, pgdoc.M{"volume": "iso:iso/" + h.Filename}, &rows); err != nil || len(rows) != 1 || !rows[0].MutationPending {
					t.Error("upload before durable journal+mutation fence")
				}
				if failAt == "upload" {
					http.Error(w, "refused", 403)
					return
				}
				if failAt == "upload-null" {
					reply(w, nil)
					return
				}
				reply(w, "UPID:n1:0:0:0:imgcopy::t:")
			})
			mux.HandleFunc("/api2/json/nodes/n1/qemu/7/config", func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					reply(w, cfg)
					return
				}
				var rows []pve.SeedArtifact
				if err := db.C("pveSeedArtifact").Find(ctx, pgdoc.M{"seedId": "source"}, &rows); err != nil || len(rows) != 1 || !rows[0].MutationPending {
					t.Error("attach before durable journal")
				}
				if failAt == "attach" {
					http.Error(w, "refused", 403)
					return
				}
				if failAt == "attach-null" {
					reply(w, nil)
					return
				}
				_ = r.ParseForm()
				cfg["ide2"] = r.Form.Get("ide2")
				reply(w, "UPID:n1:0:0:0:qmconfig:7:t:")
			})
			mux.HandleFunc("/api2/json/nodes/n1/tasks/", func(w http.ResponseWriter, r *http.Request) {
				reply(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			svc, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
			if err != nil {
				t.Fatal(err)
			}
			svc.SeedArtifacts = pve.NewSeedArtifactRepo(db)
			volume, err := svc.AttachSeedISO(ctx, "n1", 7, "iso", pve.Seed{MetaData: `{"instance-id":"iid-a"}`, UserData: "#cloud-config\n{}\n"}, pve.SeedArtifact{SeedID: "source", ProjectID: "p", InstanceID: "iid-a"})
			if (err == nil) != (failAt == "success") {
				t.Fatalf("unexpected attach outcome: %v", err)
			}
			rows, err := svc.SeedArtifacts.ForSeed(ctx, "svc", "source")
			if err != nil || len(rows) != 1 || rows[0].Volume != volume {
				t.Fatal("artifact lost at crash boundary")
			}
			if failAt == "success" {
				if rows[0].State != "attached" || rows[0].MutationPending {
					t.Fatal("successful task not committed")
				}
			} else if !rows[0].MutationPending {
				t.Fatal("uncertain mutation must require reconciliation")
			}
		})
	}
}

func TestSeedArtifactLocksCommitIndependentlyAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	r1, r2 := pve.NewSeedArtifactRepo(db), pve.NewSeedArtifactRepo(db)
	var inside atomic.Int32
	errs := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := r1
			if i%2 == 0 {
				r = r2
			}
			errs <- r.Locked(ctx, "svc", func(ctx context.Context) error {
				if inside.Add(1) != 1 {
					return fmt.Errorf("seed operations overlapped")
				}
				defer inside.Add(-1)
				return r.Locked(ctx, "svc", func(ctx context.Context) error {
					return db.C("seedLockProgress").Upsert(ctx, fmt.Sprint(i), pgdoc.M{"done": true})
				})
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count, err := db.C("seedLockProgress").Count(ctx, pgdoc.M{}); err != nil || count != 12 {
		t.Fatal("progress was not committed under advisory lock")
	}
}
