package platformlife

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
)

// fakeJobs is the job store: every Checkpoint from the loseAt-th on (and every Renew after it) reports a lost lease
// without running its function, as a commit that fails its lease check rolls back.
type fakeJobs struct {
	mu          sync.Mutex
	loseAt      int
	checkpoints int
	lost        bool
	onLose      func()
	ends        []string // complete | defer:<code> | fail:<code> | retry:<code>
}

func (f *fakeJobs) ClaimKinds(context.Context, time.Duration, []string) (*durablejobs.Job, error) {
	return nil, nil
}
func (f *fakeJobs) Renew(context.Context, *durablejobs.Job, time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lost {
		return durablejobs.ErrLeaseLost
	}
	return nil
}
func (f *fakeJobs) Checkpoint(ctx context.Context, _ *durablejobs.Job, fn func(context.Context) error) error {
	f.mu.Lock()
	f.checkpoints++
	if f.loseAt > 0 && f.checkpoints >= f.loseAt && !f.lost {
		f.lost = true
		if f.onLose != nil {
			f.onLose()
		}
	}
	lost := f.lost
	f.mu.Unlock()
	if lost {
		return durablejobs.ErrLeaseLost
	}
	return fn(ctx)
}
func (f *fakeJobs) end(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lost {
		return durablejobs.ErrLeaseLost
	}
	f.ends = append(f.ends, s)
	return nil
}
func (f *fakeJobs) Complete(context.Context, *durablejobs.Job, func(context.Context) error) error {
	return f.end("complete")
}
func (f *fakeJobs) Fail(_ context.Context, _ *durablejobs.Job, _ time.Duration, permanent bool, code string) error {
	if !permanent {
		return f.end("retry:" + code)
	}
	return f.end("fail:" + code)
}
func (f *fakeJobs) Defer(_ context.Context, _ *durablejobs.Job, _ time.Duration, code string) error {
	return f.end("defer:" + code)
}
func (f *fakeJobs) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ends) == 0 {
		return ""
	}
	return f.ends[len(f.ends)-1]
}

// fakeState keeps the run, the installation and the secrets as JSON, so no write shares memory with the runner.
type fakeState struct {
	mu            sync.Mutex
	run, in, sec  []byte
	notFoundAfter bool
}

func (f *fakeState) put(run *Run, in *Installation, sec *Secrets) {
	f.run, _ = json.Marshal(run)
	f.in, _ = json.Marshal(in)
	f.sec, _ = json.Marshal(sec)
}

func (f *fakeState) get() (*Run, *Installation, *Secrets) {
	var run Run
	var in Installation
	var sec Secrets
	_ = json.Unmarshal(f.run, &run)
	_ = json.Unmarshal(f.in, &in)
	_ = json.Unmarshal(f.sec, &sec)
	return &run, &in, &sec
}

func (f *fakeState) Load(context.Context, string) (*Run, *Installation, *Secrets, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, in, sec := f.get()
	return run, in, sec, nil
}

func (f *fakeState) Mutate(_ context.Context, _ string, fn func(*Run, *Installation, *Secrets) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, in, sec := f.get()
	if err := fn(run, in, sec); err != nil {
		return err
	}
	f.put(run, in, sec)
	return nil
}

func (f *fakeState) snapshot() (*Run, *Installation, *Secrets) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.get()
}

// fakeImports is Template updates: receipts by node and digest.
type fakeImports struct {
	recs       []templateupdates.Receipt
	dead       map[string]bool
	enqueued   []templateupdates.VMRequest
	enqueueErr error
	markErr    error
}

func (f *fakeImports) ByKey(_ context.Context, svc, node, digest string) ([]templateupdates.Receipt, error) {
	var out []templateupdates.Receipt
	for _, r := range f.recs {
		if r.Payload.ServiceID == svc && r.Payload.Node == node && r.Payload.Digest == digest {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeImports) JobState(_ context.Context, id string) (string, error) {
	if f.dead[id] {
		return "dead", nil
	}
	return "pending", nil
}
func (f *fakeImports) EnqueueVM(_ context.Context, req templateupdates.VMRequest, actor string) (*templateupdates.Receipt, error) {
	if f.enqueueErr != nil {
		return nil, f.enqueueErr
	}
	f.enqueued = append(f.enqueued, req)
	rec := templateupdates.Receipt{ID: fmt.Sprintf("tu-%d", len(f.recs)+1), RequestedBy: actor, Step: templateupdates.StepQueued, CreatedAt: time.Now(),
		Payload: templateupdates.VMPayload{ServiceID: req.ServiceID, Node: req.Node, Digest: req.Digest, Package: req.Package}}
	f.recs = append(f.recs, rec)
	return &rec, nil
}
func (f *fakeImports) MarkTemplateGone(_ context.Context, _ templateupdates.PVEResolver, id string) error {
	if f.markErr != nil {
		return f.markErr
	}
	for i := range f.recs {
		if f.recs[i].ID == id {
			f.recs[i].TemplateGone = true
		}
	}
	return nil
}

// fakeVM is one VM of fakePVE.
type fakeVM struct {
	cfg     map[string]any
	running bool
	pooled  bool
}

// fakePVE is one node (pve1) with the seed pool, an ISO store "local" and the template 9100. It records every
// request that is not a GET, with the token it came with.
type fakePVE struct {
	mu        sync.Mutex
	t         *testing.T
	next      int
	vms       map[int]*fakeVM
	iso       []string // file names on local
	tasks     map[string]string
	rules     []pve.FirewallRule
	writes    []string
	failOnce  map[string]string // "METHOD path" → an error body answered once with 500
	keepISO   int               // deletes of an ISO that answer OK but leave the file
	agent     bool
	cloneBusy bool // a qmclone task is listed as running
	cloneTags string
	uploads   [][]byte
	templates []pve.ClusterResource // /cluster/resources?type=vm
}

func newFakePVE(t *testing.T) *fakePVE {
	return &fakePVE{t: t, next: 120, vms: map[int]*fakeVM{}, tasks: map[string]string{}, failOnce: map[string]string{}, cloneTags: "pc-template;ubuntu-2604"}
}

func (f *fakePVE) upid(typ string, id int) string {
	u := fmt.Sprintf("UPID:pve1:%08X:00000001:%08X:%s:%d:capmox@pve!capi:", len(f.tasks)+1, len(f.tasks)+1, typ, id)
	f.tasks[u] = "OK"
	return u
}

func (f *fakePVE) writeCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, w := range f.writes {
		if strings.HasPrefix(w, prefix) {
			n++
		}
	}
	return n
}

func (f *fakePVE) pooled() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for id, vm := range f.vms {
		if vm.pooled {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func (f *fakePVE) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+path)
	}
	if body, ok := f.failOnce[r.Method+" "+path]; ok {
		delete(f.failOnce, r.Method+" "+path)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, body)
		return
	}
	_ = r.ParseForm()
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		_ = r.ParseMultipartForm(1 << 20)
	}
	data := func(v any) {
		b, _ := json.Marshal(map[string]any{"data": v})
		_, _ = w.Write(b)
	}
	vmid := func() (int, *fakeVM) {
		parts := strings.Split(path, "/")
		id, _ := strconv.Atoi(parts[4])
		return id, f.vms[id]
	}
	switch {
	case path == "/pools/proxcloud-seed":
		var members []map[string]any
		for id, vm := range f.vms {
			if vm.pooled {
				members = append(members, map[string]any{"id": fmt.Sprintf("qemu/%d", id), "vmid": id, "node": "pve1", "type": "qemu"})
			}
		}
		data(map[string]any{"poolid": "proxcloud-seed", "members": members})
	case path == "/cluster/nextid":
		if v := r.Form.Get("vmid"); v != "" {
			id, _ := strconv.Atoi(v)
			if _, held := f.vms[id]; held {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"errors":{"vmid":"VM %d already exists"}}`, id)
				return
			}
			data(v)
			return
		}
		for f.vms[f.next] != nil {
			f.next++
		}
		data(strconv.Itoa(f.next))
		f.next++
	case path == "/storage":
		data([]map[string]any{{"storage": "local", "type": "dir", "content": "iso,import"}, {"storage": "local-lvm", "type": "lvmthin", "content": "images"},
			{"storage": "nas-templates", "type": "nfs", "content": "import", "shared": 1}})
	case path == "/cluster/resources" && r.Form.Get("type") == "storage":
		var rows []map[string]any
		for _, n := range []string{"pve1", "pve2", "pve3"} {
			for _, st := range []string{"local", "local-lvm", "nas-templates"} {
				rows = append(rows, map[string]any{"type": "storage", "storage": st, "node": n, "status": "available", "id": "storage/" + n + "/" + st})
			}
		}
		data(rows)
	case path == "/cluster/resources":
		data(f.templates)
	case path == "/nodes":
		data([]map[string]any{{"node": "pve1", "status": "online"}, {"node": "pve2", "status": "online"}, {"node": "pve3", "status": "online"}})
	case path == "/nodes/pve1/tasks":
		if f.cloneBusy {
			data([]map[string]any{{"upid": "UPID:pve1:1:1:1:qmclone:9100:x:", "type": "qmclone"}})
			return
		}
		data([]any{})
	case strings.HasPrefix(path, "/nodes/pve1/tasks/") && strings.HasSuffix(path, "/status"):
		up := strings.TrimSuffix(strings.TrimPrefix(path, "/nodes/pve1/tasks/"), "/status")
		data(map[string]any{"status": "stopped", "exitstatus": f.tasks[up]})
	case strings.HasPrefix(path, "/nodes/pve1/tasks/") && strings.HasSuffix(path, "/log"):
		data([]map[string]any{{"t": "starting"}, {"t": "clone failed: no space left"}})
	case path == "/nodes/pve1/qemu/9100/clone":
		id, _ := strconv.Atoi(r.Form.Get("newid"))
		if _, held := f.vms[id]; held {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"errors":{"newid":"VM %d already exists"}}`, id)
			return
		}
		f.vms[id] = &fakeVM{pooled: r.Form.Get("pool") == pve.SeedPool, cfg: map[string]any{
			"name": r.Form.Get("name"), "tags": f.cloneTags, "net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0",
			"scsi0": fmt.Sprintf("local-lvm:vm-%d-disk-0,size=6G", id), "ide2": fmt.Sprintf("local-lvm:vm-%d-cloudinit,media=cdrom", id)}}
		data(f.upid("qmclone", 9100))
	case strings.HasPrefix(path, "/nodes/pve1/qemu/"):
		id, vm := vmid()
		if vm == nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"errors":{"vmid":"Configuration file 'nodes/pve1/qemu-server/%d.conf' does not exist"}}`, id)
			return
		}
		rest := strings.Join(strings.Split(path, "/")[5:], "/")
		switch r.Method + " " + rest {
		case "GET config":
			data(vm.cfg)
		case "POST config":
			for k := range r.PostForm {
				if k == "delete" {
					for _, d := range strings.Split(r.PostForm.Get(k), ",") {
						delete(vm.cfg, d)
					}
				} else {
					vm.cfg[k] = r.PostForm.Get(k)
				}
			}
			data(f.upid("qmconfig", id))
		case "PUT resize":
			vm.cfg["scsi0"] = fmt.Sprintf("local-lvm:vm-%d-disk-0,size=%s", id, r.PostForm.Get("size"))
			data(f.upid("qmresize", id))
		case "GET status/current":
			st := "stopped"
			if vm.running {
				st = "running"
			}
			data(map[string]any{"status": st})
		case "POST status/start":
			vm.running = true
			data(f.upid("qmstart", id))
		case "POST status/stop":
			vm.running = false
			data(f.upid("qmstop", id))
		case "DELETE ":
			if vm.cfg["protection"] == "1" {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"errors":{"protection":"VM is protected"}}`)
				return
			}
			delete(f.vms, id)
			data(f.upid("qmdestroy", id))
		case "PUT firewall/options":
			data(nil)
		case "GET firewall/rules":
			for i := range f.rules {
				f.rules[i].Pos, f.rules[i].Digest = i, "d"
			}
			data(f.rules)
		case "POST firewall/rules":
			f.rules = append(f.rules, pve.FirewallRule{Type: r.PostForm.Get("type"), Action: r.PostForm.Get("action"), Proto: r.PostForm.Get("proto"),
				Source: r.PostForm.Get("source"), DPort: r.PostForm.Get("dport"), Comment: r.PostForm.Get("comment")})
			data(nil)
		case "POST agent/ping":
			if !f.agent {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"errors":{"agent":"QEMU guest agent is not running"}}`)
				return
			}
			data(nil)
		default:
			if r.Method == http.MethodDelete && strings.HasPrefix(rest, "firewall/rules/") {
				pos, _ := strconv.Atoi(strings.TrimPrefix(rest, "firewall/rules/"))
				f.rules = slices.Delete(f.rules, pos, pos+1)
				data(nil)
				return
			}
			f.t.Errorf("fake PVE: no route %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	case path == "/nodes/pve1/storage/local/upload":
		_, hdr, err := r.FormFile("filename")
		if err != nil {
			f.t.Errorf("upload without a file: %v", err)
			return
		}
		file, _ := hdr.Open()
		var b bytes.Buffer
		_, _ = b.ReadFrom(file)
		f.uploads = append(f.uploads, b.Bytes())
		f.iso = append(f.iso, hdr.Filename)
		u := fmt.Sprintf("UPID:pve1:%08X:00000001:%08X:imgcopy::capmox@pve!capi:", len(f.tasks)+1, len(f.tasks)+1)
		f.tasks[u] = "OK"
		data(u)
	case path == "/nodes/pve1/storage/local/content":
		var out []map[string]any
		for _, n := range f.iso {
			out = append(out, map[string]any{"volid": "local:iso/" + n, "content": "iso"})
		}
		data(out)
	case strings.HasPrefix(path, "/nodes/pve1/storage/local/content/iso/"):
		name := strings.TrimPrefix(path, "/nodes/pve1/storage/local/content/iso/")
		if f.keepISO > 0 {
			f.keepISO--
		} else {
			f.iso = slices.DeleteFunc(f.iso, func(n string) bool { return n == name })
		}
		data(f.upid("imgdel", 0))
	default:
		f.t.Errorf("fake PVE: no route %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// fakeSeed is the seed's API over TLS with a server certificate its CA signed (cert reads the CA when a client
// connects): readyz, the token Secret (bootstrap token only), the access review and the deletes (service account token
// only). calls records "METHOD path token-kind".
type fakeSeed struct {
	mu        sync.Mutex
	url       string
	ca        func() (certPEM, keyPEM string)
	boot      func() string
	sa        string
	ready     bool
	saAllowed bool
	calls     []string
}

func newFakeSeed(t *testing.T, ca func() (string, string), boot func() string) *fakeSeed {
	f := &fakeSeed{ca: ca, boot: boot, sa: runSA, ready: true, saAllowed: true}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve), ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = srv.Serve(tls.NewListener(ln, &tls.Config{GetCertificate: f.cert})) }()
	t.Cleanup(func() { _ = srv.Close() })
	f.url = "https://" + ln.Addr().String()
	return f
}

// staticCA is a seed CA made the way stage 3 makes one.
func staticCA(t *testing.T) func() (string, string) {
	s, err := NewSeedSecrets("pi-0a1b2c3d", 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return func() (string, string) { return s.CACert, s.CAKey }
}

func (f *fakeSeed) caPEM() string { c, _ := f.ca(); return c }

func (f *fakeSeed) cert(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	certPEM, keyPEM := f.ca()
	cb, _ := pem.Decode([]byte(certPEM))
	kb, _ := pem.Decode([]byte(keyPEM))
	if cb == nil || kb == nil {
		return nil, fmt.Errorf("fake seed: no CA yet")
	}
	ca, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	caKey, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func (f *fakeSeed) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	kind := ""
	switch {
	case tok == f.sa:
		kind = "sa"
	case tok != "" && tok == f.boot():
		kind = "boot"
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+kind)
	switch {
	case kind == "":
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Unauthorized"}`)
	case r.URL.Path == "/readyz":
		if !f.ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	case r.URL.Path == "/api/v1/namespaces/proxcloud-system/secrets/proxcloud-portal-token-g1" && kind == "boot":
		fmt.Fprintf(w, `{"data":{"token":%q}}`, base64.StdEncoding.EncodeToString([]byte(f.sa)))
	case r.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews" && kind == "sa":
		fmt.Fprintf(w, `{"status":{"allowed":%t}}`, f.saAllowed)
	case r.Method == http.MethodDelete && kind == "sa":
		fmt.Fprint(w, `{}`)
	default:
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"forbidden"}`)
	}
}

func (f *fakeSeed) callsOf(kind string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasSuffix(c, " "+kind) {
			out = append(out, strings.TrimSuffix(c, " "+kind))
		}
	}
	return out
}

const (
	runCAPI = "5e1f4c7a-0d2b-4c3e-9a8b-c0ffee00c4p1"
	runCSI  = "5e1f4c7a-0d2b-4c3e-9a8b-c0ffee00c5i1"
	runBoot = "abcdef.0123456789abcdef"
	runSA   = "eyJhbGciOiJSUzI1NiJ9.planted-service-account-token"
	runK3s  = "PLANTEDK3STOKEN0123456789"
)

// env is one installation with a run, on fakes.
type env struct {
	t       *testing.T
	pve     *fakePVE
	jobs    *fakeJobs
	state   *fakeState
	imports *fakeImports
	r       *Runner
	logs    *bytes.Buffer
	job     *durablejobs.Job
}

func newEnv(t *testing.T, kind string) *env {
	t.Helper()
	e := &env{t: t, pve: newFakePVE(t), jobs: &fakeJobs{}, state: &fakeState{}, imports: &fakeImports{dead: map[string]bool{}}, logs: &bytes.Buffer{}}
	ts := httptest.NewServer(http.HandlerFunc(e.pve.serve))
	t.Cleanup(ts.Close)
	es := &externalservice.ExternalService{ID: "svc-pve", Type: externalservice.TypeCloud,
		Config: map[string]any{"provider": "proxmox", "apiUrl": ts.URL, "tokenId": "proxcloud@pve!platform"},
		Secret: map[string]any{"tokenSecret": "portal-secret-0000"}}
	e.r = &Runner{Jobs: e.jobs, State: e.state, Imports: e.imports, Log: slog.New(slog.NewTextHandler(e.logs, nil)), Lease: time.Minute,
		Provider: func(context.Context, string) (*externalservice.ExternalService, error) { return es, nil },
		Preflight: func(context.Context, *Installation) ([]Check, error) {
			return []Check{{ID: "flags", Status: CheckOK}}, nil
		}}
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	in := validInstallation(t)
	in.ID, in.Status.Phase = "pi-0a1b2c3d", PhaseApplying
	run := &Run{ID: "pr-1", InstallationID: in.ID, Kind: kind, State: RunQueued, BOM: BOMRef{Release: cur.Release, SHA256: cur.SHA256}}
	for _, id := range stageIDs(kind) {
		run.Stages = append(run.Stages, Stage{ID: id, State: StagePending})
	}
	sec := &Secrets{ID: in.ID, CAPI: &TokenCredential{TokenID: pve.CAPITokenID, Secret: runCAPI}, CSI: &TokenCredential{TokenID: pve.CSITokenID, Secret: runCSI}}
	e.state.put(run, in, sec)
	raw, _ := json.Marshal(Payload{Version: 1, RunID: run.ID, InstallationID: in.ID})
	e.job = &durablejobs.Job{ID: "job-1", Kind: durablejobs.PlatformRun, Payload: raw, Attempts: 1}
	return e
}

// upTo marks the stages before id done (stage 2 with the seed template 9100 on pve1).
func (e *env) upTo(id string) {
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		for i := range run.Stages {
			if run.Stages[i].ID == id {
				return
			}
			run.Stages[i].State = StageDone
			if run.Stages[i].ID == StageImages {
				run.Stages[i].Receipt = &StageReceipt{Templates: map[string]int{"seed:pve1": 9100}}
			}
		}
	})
}

func (e *env) mutate(fn func(*Run, *Installation, *Secrets)) {
	_ = e.state.Mutate(context.Background(), "", func(r *Run, i *Installation, s *Secrets) error { fn(r, i, s); return nil })
}

// once runs one acquisition and answers how it gave the job back.
func (e *env) once() string {
	e.t.Helper()
	e.r.defaults()
	e.r.perform(context.Background(), e.job)
	return e.jobs.last()
}

// until runs acquisitions until the job is given back other than by a Defer (at most n).
func (e *env) until(n int) string {
	e.t.Helper()
	for range n {
		if end := e.once(); !strings.HasPrefix(end, "defer:") {
			return end
		}
	}
	return e.jobs.last()
}

func (e *env) stage(id string) Stage {
	run, _, _ := e.state.snapshot()
	for _, s := range run.Stages {
		if s.ID == id {
			return s
		}
	}
	e.t.Fatalf("no stage %s", id)
	return Stage{}
}

// readySeedAt puts a seed generation that stage 3 built at vmid on fake PVE and in the state, with the seed API at seed.
func (e *env) seedAt(vmid, gen int, seed *fakeSeed, running bool) {
	e.pve.vms[vmid] = &fakeVM{pooled: true, running: running, cfg: map[string]any{"name": SeedVMName("pi-0a1b2c3d", gen),
		"tags": "proxcloud;pc-platform;pc-platform-pi-0a1b2c3d", "net0": "virtio=BC:24:11:00:00:01,bridge=vmbr1,firewall=1",
		"ide2": "local:iso/" + SeedCDName("pi-0a1b2c3d", gen) + ",media=cdrom"}}
	e.pve.iso = append(e.pve.iso, SeedCDName("pi-0a1b2c3d", gen))
	server := ""
	ca := ""
	if seed != nil {
		server, ca = seed.url, seed.caPEM()
	}
	e.mutate(func(_ *Run, in *Installation, sec *Secrets) {
		in.Status.Seed = &SeedStatus{VMID: vmid, Node: "pve1", Generation: gen, APIServer: server, CACert: ca,
			CDVolume: "local:iso/" + SeedCDName("pi-0a1b2c3d", gen)}
		sec.BootstrapToken, sec.K3sToken = runBoot, runK3s
	})
}
