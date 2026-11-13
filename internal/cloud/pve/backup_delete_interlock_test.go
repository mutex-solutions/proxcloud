package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const (
	backupDeleteConfigPath = "/api2/json/nodes/pve1/qemu/101/config"
	backupDeleteTasksPath  = "/api2/json/nodes/pve1/tasks"
	backupDeleteListPath   = "/api2/json/nodes/pve1/storage/nas-backup/content"
	backupDeleteVolID      = "nas-backup:backup/old"
	backupDeleteUPID       = "UPID:pve1:0:0:0:x:101:u:"
)

// Count every outgoing HTTP request, including an unexpected path the fake server
// rejects. A refusal cannot pass merely because its attempted DELETE was unhandled.
type backupDeleteWire struct {
	base  http.RoundTripper
	mu    sync.Mutex
	calls []string
}

func (p *backupDeleteWire) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.calls = append(p.calls, r.Method+" "+r.URL.EscapedPath())
	p.mu.Unlock()
	return p.base.RoundTrip(r)
}
func (p *backupDeleteWire) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}
func (p *backupDeleteWire) count(method, path string) int {
	n := 0
	for _, call := range p.requests() {
		if call == method+" "+path {
			n++
		}
	}
	return n
}
func (p *backupDeleteWire) mutations() []string {
	var out []string
	for _, call := range p.requests() {
		if !strings.HasPrefix(call, "GET ") {
			out = append(out, call)
		}
	}
	return out
}
func newBackupDeleteWire(t *testing.T) (*backupCluster, *backupDeleteWire) {
	t.Helper()
	b := newBackupCluster(t)
	p := &backupDeleteWire{base: b.s.C.http.Transport}
	b.s.C.http.Transport = p
	return b, p
}
func refuseBackupDelete(t *testing.T, b *backupCluster, p *backupDeleteWire) error {
	t.Helper()
	err := b.s.DeleteServerBackup(t.Context(), "p1", "101", "nas-backup", backupDeleteVolID)
	if err == nil {
		t.Fatal("single delete did not refuse")
	}
	if calls := p.mutations(); len(calls) != 0 {
		t.Fatalf("refusal dispatched mutation(s): %v", calls)
	}
	if len(b.deleted) != 0 || b.stopped != 0 || b.started != 0 || b.restored != nil {
		t.Fatal("refusal changed archive or VM")
	}
	return err
}
func requireBackupDeleteReads(t *testing.T, p *backupDeleteWire, tasks int) {
	t.Helper()
	if got := p.count("GET", backupDeleteListPath); got != 1 {
		t.Fatalf("ownership listing count %d, want 1", got)
	}
	if got := p.count("GET", backupDeleteConfigPath); got != 1 {
		t.Fatalf("current config count %d, want 1", got)
	}
	if got := p.count("GET", backupDeleteTasksPath); got != tasks {
		t.Fatalf("active restore listing count %d, want %d", got, tasks)
	}
}

func TestDeleteServerBackupInterlockKnownActivity(t *testing.T) {
	for _, lock := range []string{"backup", "restore", "migrate", "arbitrary-operator-lock", " "} {
		t.Run("config-lock-"+lock, func(t *testing.T) {
			b, p := newBackupDeleteWire(t)
			b.cfg = fmt.Sprintf(`{"lock":%q,"scsi0":"z:vm-101-disk-0,size=8G"}`, lock)
			err := refuseBackupDelete(t, b, p)
			wantConflict(t, err, "busy with another operation")
			requireBackupDeleteReads(t, p, 0)
		})
	}
	t.Run("unlocked-with-active-restore", func(t *testing.T) {
		b, p := newBackupDeleteWire(t)
		b.active = `[{"upid":"UPID:pve1:0:0:0:qmrestore:101:u:","type":"qmrestore","id":"101","endtime":0}]`
		err := refuseBackupDelete(t, b, p)
		wantConflict(t, err, "being restored right now")
		requireBackupDeleteReads(t, p, 1)
	})
	t.Run("unreadable-status-does-not-bypass-current-lock", func(t *testing.T) {
		b, p := newBackupDeleteWire(t)
		b.cfg = `{"lock":"backup"}`
		b.requestHook = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/api2/json/nodes/pve1/qemu/101/status/current" {
				http.Error(w, "unavailable", http.StatusForbidden)
				return true
			}
			return false
		}
		wantConflict(t, refuseBackupDelete(t, b, p), "busy with another operation")
		requireBackupDeleteReads(t, p, 0)
	})
}

func TestDeleteServerBackupInterlockUnreadableCurrentState(t *testing.T) {
	for _, endpoint := range []string{"config", "tasks"} {
		for _, fault := range []string{"403", "500", "malformed-json", "wrong-type", "null", "missing-data"} {
			t.Run(endpoint+"/"+fault, func(t *testing.T) {
				b, p := newBackupDeleteWire(t)
				path := backupDeleteConfigPath
				if endpoint == "tasks" {
					path = backupDeleteTasksPath
				}
				b.requestHook = func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path != path {
						return false
					}
					if path == backupDeleteTasksPath {
						if got := r.URL.Query(); got.Get("source") != "active" || got.Get("vmid") != "101" || got.Get("typefilter") != "qmrestore" {
							t.Errorf("active restore query changed: %v", got)
						}
					}
					switch fault {
					case "403":
						http.Error(w, "forbidden", http.StatusForbidden)
					case "500":
						http.Error(w, "unavailable", http.StatusInternalServerError)
					case "malformed-json":
						fmt.Fprint(w, `{"data":`)
					case "wrong-type":
						if endpoint == "config" {
							fmt.Fprint(w, `{"data":[]}`)
						} else {
							fmt.Fprint(w, `{"data":{}}`)
						}
					case "null":
						fmt.Fprint(w, `{"data":null}`)
					case "missing-data":
						fmt.Fprint(w, `{}`)
					}
					return true
				}
				err := refuseBackupDelete(t, b, p)
				tasks := 0
				if endpoint == "tasks" {
					tasks = 1
				}
				requireBackupDeleteReads(t, p, tasks)
				if fault == "403" || fault == "500" {
					var ae *APIError
					want := http.StatusForbidden
					if fault == "500" {
						want = http.StatusInternalServerError
					}
					if !errors.As(err, &ae) || ae.Status != want {
						t.Fatalf("read error classification changed: %T %v", err, err)
					}
				}
				if endpoint == "config" && (fault == "null" || fault == "missing-data") {
					wantConflict(t, err, "current configuration")
				}
				if endpoint == "tasks" && (fault == "null" || fault == "missing-data") && err.Error() != "pve: active restore task list is unavailable" {
					t.Fatalf("unexpected unavailable task-list error: %v", err)
				}
			})
		}
	}
}

func TestDeleteServerBackupInterlockPreservesOwnership(t *testing.T) {
	for _, mode := range []string{"foreign-notes", "manual-notes", "unknown-archive", "wrong-store-volid", "foreign-archive", "foreign-pool", "foreign-project", "other-vmid", "storage-read-error"} {
		t.Run(mode, func(t *testing.T) {
			b, p := newBackupDeleteWire(t)
			project, vmid, volume := "p1", "101", backupDeleteVolID
			switch mode {
			case "foreign-notes", "manual-notes":
				b.requestHook = func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path != backupDeleteListPath {
						return false
					}
					if got := r.URL.Query(); got.Get("content") != "backup" || got.Get("vmid") != "101" {
						t.Errorf("ownership listing lost VM/content filter: %v", got)
					}
					notes := "proxcloud project=p11 server=web"
					if mode == "manual-notes" {
						notes = "manual backup"
					}
					fmt.Fprintf(w, `{"data":[{"volid":%q,"notes":%q,"vmid":101}]}`, backupDeleteVolID, notes)
					return true
				}
			case "unknown-archive":
				volume = "nas-backup:backup/missing"
			case "wrong-store-volid":
				volume = "other-store:backup/old"
			case "foreign-archive":
				volume = "nas-backup:backup/theirs"
			case "foreign-pool":
				b.requestHook = func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path == "/api2/json/cluster/resources" && r.URL.Query().Get("type") == "vm" {
						fmt.Fprint(w, `{"data":[{"type":"qemu","vmid":101,"node":"pve1","pool":"tp-other","name":"foreign"}]}`)
						return true
					}
					return false
				}
			case "foreign-project":
				project = "p2"
			case "other-vmid":
				vmid = "102"
			case "storage-read-error":
				b.storeDown = true
			}
			if err := b.s.DeleteServerBackup(t.Context(), project, vmid, "nas-backup", volume); err == nil {
				t.Fatal("unowned/unreadable archive reached delete")
			}
			if len(p.mutations()) != 0 || len(b.deleted) != 0 {
				t.Fatalf("ownership refusal sent mutation(s): %v", p.mutations())
			}
			if p.count("GET", backupDeleteConfigPath) != 0 || p.count("GET", backupDeleteTasksPath) != 0 {
				t.Fatal("archive ownership was not checked before current-state interlock")
			}
		})
	}
}

func TestDeleteServerBackupInterlockIdleDeletesExactArchiveAndWaits(t *testing.T) {
	for _, fault := range []string{"", "delete-403", "delete-500", "wait-failed"} {
		t.Run(fault, func(t *testing.T) {
			b, p := newBackupDeleteWire(t)
			b.requestHook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method == http.MethodDelete && fault == "delete-403" {
					http.Error(w, "forbidden", http.StatusForbidden)
					return true
				}
				if r.Method == http.MethodDelete && fault == "delete-500" {
					http.Error(w, "unavailable", http.StatusInternalServerError)
					return true
				}
				return false
			}
			if fault == "wait-failed" {
				b.taskErr = "fixture delete failed"
			}
			err := b.s.DeleteServerBackup(t.Context(), "p1", "101", "nas-backup", backupDeleteVolID)
			switch fault {
			case "":
				if err != nil || !reflect.DeepEqual(b.deleted, []string{"old"}) {
					t.Fatalf("own idle archive did not delete successfully: %v %v", b.deleted, err)
				}
			case "delete-403":
				wantConflict(t, err, "VM.Backup")
			case "delete-500":
				var ae *APIError
				if !errors.As(err, &ae) || ae.Status != http.StatusInternalServerError {
					t.Fatalf("delete error classification changed: %v", err)
				}
			case "wait-failed":
				if err == nil || !strings.Contains(err.Error(), "fixture delete failed") {
					t.Fatalf("failed task was not propagated: %v", err)
				}
			}
			requireBackupDeleteReads(t, p, 1)
			deleteCall := "DELETE /api2/json/nodes/pve1/storage/nas-backup/content/" + url.PathEscape(backupDeleteVolID)
			if got := p.mutations(); !reflect.DeepEqual(got, []string{deleteCall}) {
				t.Fatalf("want one exact archive DELETE, got %v", got)
			}
			waits := p.count("GET", "/api2/json/nodes/pve1/tasks/"+backupDeleteUPID+"/status")
			wantWaits := 1
			if strings.HasPrefix(fault, "delete-") {
				wantWaits = 0
			}
			if waits != wantWaits {
				t.Fatalf("WaitTask status count %d, want %d", waits, wantWaits)
			}
			calls := strings.Join(p.requests(), "\n")
			if strings.Index(calls, "GET "+backupDeleteConfigPath) > strings.Index(calls, "GET "+backupDeleteTasksPath) || strings.Index(calls, "GET "+backupDeleteTasksPath) > strings.Index(calls, deleteCall) {
				t.Fatalf("interlock request order changed: %s", calls)
			}
		})
	}
}

func TestRestoreRunningRequiresReadableTaskList(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		running   bool
		wantErr   bool
	}{
		{"empty-list", `{"data":[]}`, false, false},
		{"active", `{"data":[{"endtime":0}]}`, true, false},
		{"ended", `{"data":[{"endtime":1700000000}]}`, false, false},
		{"null", `{"data":null}`, false, true},
		{"missing-data", `{}`, false, true},
		{"malformed-json", `{"data":`, false, true},
		{"unreadable-type", `{"data":{}}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, p := newBackupDeleteWire(t)
			b.requestHook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != backupDeleteTasksPath {
					return false
				}
				if got := r.URL.Query(); got.Get("source") != "active" || got.Get("vmid") != "101" || got.Get("typefilter") != "qmrestore" {
					t.Errorf("task filters changed: %v", got)
				}
				fmt.Fprint(w, tc.raw)
				return true
			}
			running, err := b.s.C.RestoreRunning(context.Background(), "pve1", 101)
			if running != tc.running || (err != nil) != tc.wantErr {
				t.Fatalf("running=%v err=%v, want running=%v error=%v", running, err, tc.running, tc.wantErr)
			}
			if p.count("GET", backupDeleteTasksPath) != 1 || len(p.requests()) != 1 || len(p.mutations()) != 0 {
				t.Fatalf("nonvacuous helper request count: %v", p.requests())
			}
		})
	}
}

// CloneRunning reads the token's active qmclone tasks of a source VM; an unreadable task list is an error, never "no clone".
func TestCloneRunningRequiresReadableTaskList(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		running   bool
		wantErr   bool
	}{
		{"empty-list", `{"data":[]}`, false, false},
		{"active", `{"data":[{"endtime":0}]}`, true, false},
		{"ended", `{"data":[{"endtime":1700000000}]}`, false, false},
		{"null", `{"data":null}`, false, true},
		{"malformed-json", `{"data":`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, p := newBackupDeleteWire(t)
			b.requestHook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != backupDeleteTasksPath {
					return false
				}
				if got := r.URL.Query(); got.Get("source") != "active" || got.Get("vmid") != "101" || got.Get("typefilter") != "qmclone" {
					t.Errorf("task filters changed: %v", got)
				}
				fmt.Fprint(w, tc.raw)
				return true
			}
			running, err := b.s.C.CloneRunning(context.Background(), "pve1", 101)
			if running != tc.running || (err != nil) != tc.wantErr {
				t.Fatalf("running=%v err=%v, want running=%v error=%v", running, err, tc.running, tc.wantErr)
			}
			if len(p.mutations()) != 0 {
				t.Fatalf("a read mutated: %v", p.requests())
			}
		})
	}
}
