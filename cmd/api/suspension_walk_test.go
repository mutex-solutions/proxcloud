//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// walkPVE is a one-node PVE that holds power states and can refuse a verb on a VM. It proves the calls the walk makes, not what
// real PVE would refuse.
type walkPVE struct {
	mu     sync.Mutex
	status map[int]string // running | paused | stopped
	pool   map[int]string
	calls  []string
	refuse map[string]bool // "resume 101"
	noRead map[int]bool    // status/current answers 500 for these
	down   bool
	onVerb func(verb string, id int)
}

var walkQemu = regexp.MustCompile(`/nodes/n1/qemu/(\d+)/status/(\w+)`)

func (f *walkPVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "gone", http.StatusInternalServerError)
		return
	}
	switch {
	case strings.HasPrefix(path, "/cluster/resources"):
		if r.URL.Query().Get("type") == "node" {
			reply([]map[string]any{{"type": "node", "node": "n1", "status": "online"}})
			return
		}
		out := []map[string]any{}
		for id, st := range f.status {
			if st == "paused" {
				st = "running" // cluster/resources does not tell
			}
			out = append(out, map[string]any{"type": "qemu", "vmid": id, "node": "n1", "pool": f.pool[id], "status": st, "name": fmt.Sprint("vm", id)})
		}
		reply(out)
	case strings.Contains(path, "/tasks/"):
		reply(map[string]any{"status": "stopped", "exitstatus": "OK"})
	default:
		m := walkQemu.FindStringSubmatch(path)
		if m == nil {
			http.Error(w, "unknown "+path, http.StatusNotFound)
			return
		}
		id, _ := strconv.Atoi(m[1])
		verb := m[2]
		if verb == "current" {
			if f.noRead[id] {
				http.Error(w, "read failed", http.StatusInternalServerError)
				return
			}
			st := f.status[id]
			power := st
			if st == "paused" {
				power = "running"
			}
			reply(map[string]any{"status": power, "qmpstatus": st, "name": fmt.Sprint("vm", id)})
			return
		}
		f.calls = append(f.calls, fmt.Sprintf("%s %d", verb, id))
		if f.refuse[fmt.Sprintf("%s %d", verb, id)] {
			http.Error(w, "VM is locked (snapshot)", http.StatusInternalServerError)
			return
		}
		switch verb {
		case "suspend":
			f.status[id] = "paused"
		case "resume":
			f.status[id] = "running"
		}
		if f.onVerb != nil {
			f.onVerb(verb, id)
		}
		reply("UPID:n1:0:0:0:qm:" + m[1] + ":root@pam:")
	}
}

func (f *walkPVE) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *walkPVE) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

func (f *walkPVE) st(id int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[id]
}

type walkEnv struct {
	t    *testing.T
	db   *pgdoc.DB
	pve  *walkPVE
	sus  billingCloudSuspender
	bill *billing.Repo
	proj *project.Repo
	rows *cloud.Repo
}

func newWalkEnv(t *testing.T) *walkEnv {
	t.Helper()
	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("walk"), tcpostgres.WithUsername("walk"),
		tcpostgres.WithPassword("walk"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(pgc) })
	dsn, err := pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(ctx) })
	enc := textcrypt.New("test-key")
	fp := &walkPVE{status: map[int]string{}, pool: map[int]string{}, refuse: map[string]bool{}, noRead: map[int]bool{}}
	srv := httptest.NewServer(fp)
	t.Cleanup(srv.Close)
	e := &walkEnv{t: t, db: db, pve: fp, bill: billing.NewRepo(db), proj: project.NewRepo(db), rows: cloud.NewRepo(db)}
	e.insert("externalService", pgdoc.M{"_id": "pv", "name": "pv", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
		"config": pgdoc.M{"provider": "proxmox", "apiUrl": srv.URL, "tokenId": "t@pam!x", "region": "r1"}, "secret": enc.EncryptObject(map[string]any{"tokenSecret": "x"})})
	e.sus = billingCloudSuspender{orgs: org.NewRepo(db), projects: e.proj, cloud: e.rows, services: externalservice.NewService(externalservice.NewRepo(db), enc), log: slog.Default()}
	e.insert("billingProfile", pgdoc.M{"_id": "bp1", "organizationId": "o1", "status": billing.StatusActive})
	e.insert("organization", pgdoc.M{"_id": "o1", "name": "o1", "billingProfileId": "bp1"})
	return e
}

func (e *walkEnv) insert(coll string, doc pgdoc.M) {
	e.t.Helper()
	if _, err := e.db.C(coll).InsertOne(context.Background(), doc); err != nil {
		e.t.Fatal(err)
	}
}

// project adds a project and its VMs, all running.
func (e *walkEnv) project(id, status string, vms ...int) {
	e.t.Helper()
	e.insert("project", pgdoc.M{"_id": id, "name": id, "organizationId": "o1", "status": status,
		"services": []any{pgdoc.M{"serviceId": "pv", "provider": "proxmox", "region": "r1"}}})
	for _, v := range vms {
		e.pve.set(func() { e.pve.status[v], e.pve.pool[v] = "running", "pc-"+id })
		e.insert("cloudResource", pgdoc.M{"_id": fmt.Sprint("r", v), "projectId": id, "serviceId": "pv", "externalId": fmt.Sprint(v), "type": cloud.TypeServer})
	}
}

func (e *walkEnv) act() *billing.ActivationService {
	a := billing.NewActivationService(e.bill, nil, slog.Default())
	a.SetClouds(e.sus)
	return a
}

func (e *walkEnv) suspend() {
	e.t.Helper()
	bp, err := e.bill.FindByID(context.Background(), "bp1")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.act().Suspend(context.Background(), bp, billing.SourceAdmin); err != nil {
		e.t.Fatal(err)
	}
	e.pve.take()
}

func (e *walkEnv) unsuspend(ctx context.Context) {
	e.t.Helper()
	bp, err := e.bill.FindByID(context.Background(), "bp1")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.act().Unsuspend(ctx, bp, billing.SourceAdmin); err != nil {
		e.t.Fatal(err)
	}
}

func (e *walkEnv) pass() {
	e.t.Helper()
	j := billing.NewSuspensionJob(e.bill, nil)
	j.SetCloudSuspender(e.sus)
	if _, err := j.ExecuteDunning(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *walkEnv) marked(v int) bool {
	e.t.Helper()
	r, err := e.rows.FindByServiceIDAndExternalID(context.Background(), "pv", fmt.Sprint(v))
	if err != nil || r == nil {
		e.t.Fatalf("row %d: %v %v", v, r, err)
	}
	return r.PausedBySuspension
}

func (e *walkEnv) projectStatus(id string) string {
	e.t.Helper()
	p, err := e.proj.FindByID(context.Background(), id)
	if err != nil || p == nil {
		e.t.Fatalf("project %s: %v %v", id, p, err)
	}
	return p.Status
}

// A VM whose resume is refused stays paused for now, and a later pass brings it back; then the pass has nothing to do.
func TestResumeFailureRecoversOnALaterPass(t *testing.T) {
	e := newWalkEnv(t)
	e.project("p1", project.StatusEnabled, 101, 102)
	e.suspend()
	e.pve.set(func() { e.pve.refuse["resume 102"] = true })
	e.unsuspend(context.Background())
	if e.pve.st(101) != "running" || e.pve.st(102) != "paused" || e.marked(101) || !e.marked(102) {
		t.Fatalf("after the failed resume: 101=%s 102=%s marks %v %v", e.pve.st(101), e.pve.st(102), e.marked(101), e.marked(102))
	}
	if e.projectStatus("p1") != project.StatusEnabled {
		t.Fatalf("project %s", e.projectStatus("p1"))
	}
	e.pve.take()
	e.pass() // still refused: nothing changes, nothing is lost
	if e.pve.st(102) != "paused" || !e.marked(102) {
		t.Fatalf("refused again: %s %v", e.pve.st(102), e.marked(102))
	}
	e.pve.set(func() { delete(e.pve.refuse, "resume 102") })
	e.pve.take()
	e.pass()
	if c := e.pve.take(); !slices.Equal(c, []string{"resume 102"}) {
		t.Fatalf("the pass should resume 102 only: %v", c)
	}
	if e.pve.st(102) != "running" || e.marked(102) {
		t.Fatalf("not recovered: %s %v", e.pve.st(102), e.marked(102))
	}
	e.pass()
	if c := e.pve.take(); len(c) != 0 {
		t.Fatalf("a settled pass must only read: %v", c)
	}

	// PVE away for the whole resume.
	e.suspend()
	e.pve.set(func() { e.pve.down = true })
	e.unsuspend(context.Background())
	e.pve.set(func() { e.pve.down = false })
	if e.pve.st(101) != "paused" {
		t.Fatalf("101 %s", e.pve.st(101))
	}
	e.pass()
	if e.pve.st(101) != "running" || e.pve.st(102) != "running" {
		t.Fatalf("after PVE came back: %s %s", e.pve.st(101), e.pve.st(102))
	}
}

// One failed status read during the resume makes a paused VM look running: the mark stays, and a later pass resumes it.
func TestFailedReadDuringResumeKeepsTheMark(t *testing.T) {
	e := newWalkEnv(t)
	e.project("p1", project.StatusEnabled, 101)
	e.suspend()
	e.pve.set(func() { e.pve.noRead[101] = true })
	e.unsuspend(context.Background())
	if e.pve.st(101) != "paused" || !e.marked(101) {
		t.Fatalf("after the failed read: %s mark %v", e.pve.st(101), e.marked(101))
	}
	e.pve.set(func() { delete(e.pve.noRead, 101) })
	e.pve.take()
	e.pass()
	if c := e.pve.take(); !slices.Equal(c, []string{"resume 101"}) {
		t.Fatalf("calls %v", c)
	}
	if e.pve.st(101) != "running" || e.marked(101) {
		t.Fatalf("not recovered: %s %v", e.pve.st(101), e.marked(101))
	}
}

// One failed status read during the suspend makes a hand-paused VM look running: the walk must not take it as its own. A VM
// really running waits for the next pass.
func TestFailedReadDuringSuspendMarksNothing(t *testing.T) {
	e := newWalkEnv(t)
	e.project("p1", project.StatusEnabled, 101, 102)
	e.pve.set(func() {
		e.pve.status[101] = "paused"
		e.pve.noRead[101], e.pve.noRead[102] = true, true
	})
	e.suspend()
	if e.marked(101) || e.marked(102) || e.pve.st(102) != "running" {
		t.Fatalf("marks %v %v, 102 %s", e.marked(101), e.marked(102), e.pve.st(102))
	}
	e.pve.set(func() { e.pve.noRead = map[int]bool{} })
	e.pass()
	if e.marked(101) || !e.marked(102) || e.pve.st(102) != "paused" {
		t.Fatalf("after the pass: marks %v %v, 102 %s", e.marked(101), e.marked(102), e.pve.st(102))
	}
	e.unsuspend(context.Background())
	if e.pve.st(101) != "paused" || e.pve.st(102) != "running" {
		t.Fatalf("after the resume: 101 %s 102 %s", e.pve.st(101), e.pve.st(102))
	}
}

// The resume brings back what the walk paused and nothing else.
func TestResumeLeavesWhatTheWalkDidNotPause(t *testing.T) {
	e := newWalkEnv(t)
	e.project("p1", project.StatusEnabled, 101, 102, 103, 104)
	e.pve.set(func() {
		e.pve.status[102] = "stopped" // stopped by the tenant before the suspension
		e.pve.status[103] = "paused"  // paused by someone else before it
	})
	e.suspend()
	if e.marked(102) || e.marked(103) || !e.marked(101) {
		t.Fatalf("marks: 101 %v 102 %v 103 %v", e.marked(101), e.marked(102), e.marked(103))
	}
	e.pve.set(func() { e.pve.status[104] = "stopped" }) // a member stops a held VM
	e.unsuspend(context.Background())
	e.pass()
	for vm, want := range map[int]string{101: "running", 102: "stopped", 103: "paused", 104: "stopped"} {
		if got := e.pve.st(vm); got != want {
			t.Errorf("vm %d: %s, want %s", vm, got, want)
		}
		if e.marked(vm) {
			t.Errorf("vm %d keeps its mark", vm)
		}
	}
	if c := e.pve.take(); !slices.Equal(c, []string{"resume 101"}) {
		t.Errorf("calls %v", c)
	}
}

// A resume fails, then a member stops the VM: the later pass must not bring it back.
func TestLaterPassLeavesWhatAMemberStopped(t *testing.T) {
	e := newWalkEnv(t)
	e.project("p1", project.StatusEnabled, 101)
	e.suspend()
	e.pve.set(func() { e.pve.refuse["resume 101"] = true })
	e.unsuspend(context.Background())
	e.pve.set(func() { e.pve.status[101] = "stopped"; delete(e.pve.refuse, "resume 101") })
	e.pass()
	if e.pve.st(101) != "stopped" || e.marked(101) {
		t.Fatalf("%s mark %v", e.pve.st(101), e.marked(101))
	}
}

// The request going away mid-walk stops nothing.
func TestResumeOutlivesTheRequest(t *testing.T) {
	e := newWalkEnv(t)
	e.project("p1", project.StatusEnabled, 101)
	e.project("p2", project.StatusEnabled, 102)
	e.suspend()
	rctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.pve.set(func() { e.pve.onVerb = func(string, int) { cancel() } })
	e.unsuspend(rctx)
	e.pve.set(func() { e.pve.onVerb = nil })
	if e.pve.st(101) != "running" || e.pve.st(102) != "running" || e.projectStatus("p1") != project.StatusEnabled || e.projectStatus("p2") != project.StatusEnabled {
		t.Fatalf("walk stopped: %s %s %s %s", e.pve.st(101), e.pve.st(102), e.projectStatus("p1"), e.projectStatus("p2"))
	}
}

// What a resume that died leaves: a project still DISABLED with a marked VM, on a profile that pays. The pass lifts it. Held ones stay.
func TestPassFinishesAResumeThatDied(t *testing.T) {
	e := newWalkEnv(t)
	e.sus.allowsEnabled = func(ctx context.Context, p *project.Project) (bool, error) {
		return e.bill.IsActive(ctx, "bp1")
	}
	e.project("died", project.StatusEnabled, 101)
	e.project("pinned", project.StatusEnabled, 102)
	e.suspend()
	if _, err := e.db.C("project").SetByID(context.Background(), "pinned", pgdoc.M{"disabledByOperator": true}, nil); err != nil {
		t.Fatal(err)
	}
	e.pass() // profile still SUSPENDED: nothing comes back
	if e.pve.st(101) != "paused" || e.projectStatus("died") != project.StatusDisabled {
		t.Fatalf("a held project came back: %s %s", e.pve.st(101), e.projectStatus("died"))
	}
	if _, err := e.db.C("billingProfile").SetByID(context.Background(), "bp1", pgdoc.M{"status": billing.StatusActive}, nil); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if e.pve.st(101) != "running" || e.projectStatus("died") != project.StatusEnabled || e.marked(101) {
		t.Fatalf("not finished: %s %s %v", e.pve.st(101), e.projectStatus("died"), e.marked(101))
	}
	if e.pve.st(102) != "paused" || e.projectStatus("pinned") != project.StatusDisabled {
		t.Fatalf("what an operator disabled came back: %s %s", e.pve.st(102), e.projectStatus("pinned"))
	}
}

// A sync rewrites the row from what PVE says, and must not take the mark with it.
func TestSyncKeepsTheMark(t *testing.T) {
	e := newWalkEnv(t)
	e.project("p1", project.StatusEnabled, 101)
	e.suspend()
	ctx := context.Background()
	r, err := e.rows.FindByServiceIDAndExternalID(ctx, "pv", "101")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r.UpdatedAt = &now
	r.Data = map[string]any{"name": "after-sync"}
	if got, err := e.rows.Update(ctx, r); err != nil || got == nil {
		t.Fatalf("%v %v", got, err)
	}
	if !e.marked(101) {
		t.Fatal("the sync took the mark")
	}
}
