package templateupdates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
)

// ── fakes ─────────────────────────────────────────────────────────────────────────────────────

type fakeVM struct {
	node     string
	cfg      pve.VMConfig
	template bool
}

// fakePVE records every external call; mutating calls are counted for the fencing assertions.
type fakePVE struct {
	mu        sync.Mutex
	files     map[string]int64 // volid → size
	vms       map[int]*fakeVM
	calls     []string
	mutations int
	ops       int   // every PVE call, reads included
	mutAt     []int // op index of each mutation
	// knobs
	uploadFail   string // task exit status for the upload ("" = OK)
	importFail   bool
	checkSum     string // the checksum PVE received
	readAll      bool   // read the whole upload body (PVE consumes the stream)
	createHook   func() // runs inside CreateVM
	uploadHook   func()
	failDeleteVM bool
	// finalWaitFails makes the WaitTask of the final name/tags write fail although it landed.
	finalWaitFails bool
	// importTransportErr makes the import-from SetVMConfig fail before PVE answered.
	importTransportErr bool
}

func newFakePVE() *fakePVE {
	return &fakePVE{files: map[string]int64{}, vms: map[int]*fakeVM{}, readAll: true}
}

func (f *fakePVE) call(s string, mutate bool) {
	f.ops++
	f.calls = append(f.calls, s)
	if mutate {
		f.mutations++
		f.mutAt = append(f.mutAt, f.ops)
	}
}

// read counts a non-mutating PVE call (the caller holds f.mu or none is needed).
func (f *fakePVE) read() { f.ops++ }

func (f *fakePVE) UploadSpace(context.Context) (pve.StorageNodeState, error) {
	f.read()
	return pve.StorageNodeState{Active: true, Total: 1 << 40, Avail: 1 << 39}, nil
}

func (f *fakePVE) UploadImportFile(_ context.Context, node, storage, filename string, src io.Reader, size int64, checksum, algorithm string) (pve.UPID, error) {
	f.mu.Lock()
	f.call("upload "+filename, true)
	f.checkSum = algorithm + ":" + checksum
	hook := f.uploadHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	n, err := io.Copy(io.Discard, src)
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("short body")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploadFail == "" {
		f.files[storage+":import/"+filename] = size
	}
	return pve.UPID("UPID:upload:" + filename), nil
}

func (f *fakePVE) TaskStopped(context.Context, pve.UPID, time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	return true
}

func (f *fakePVE) TaskStatus(_ context.Context, up pve.UPID) (*pve.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	if strings.HasPrefix(string(up), "UPID:upload:") && f.uploadFail != "" {
		return &pve.TaskStatus{Status: "stopped", ExitStatus: f.uploadFail}, nil
	}
	if string(up) == "UPID:import-fail" {
		return &pve.TaskStatus{Status: "stopped", ExitStatus: "import failed: invalid qcow2"}, nil
	}
	return &pve.TaskStatus{Status: "stopped", ExitStatus: "OK"}, nil
}

func (f *fakePVE) WaitTask(_ context.Context, up pve.UPID) error {
	f.mu.Lock()
	f.read()
	f.mu.Unlock()
	if string(up) == "UPID:import-fail" {
		return errors.New("pve: task failed: import")
	}
	if string(up) == "UPID:final" {
		return errors.New("pve: wait UPID:final: connection reset")
	}
	if id, ok := strings.CutPrefix(string(up), "UPID:create-forbidden:"); ok {
		return errors.New("pve: task failed: unable to create VM " + id + " - 403 Permission check failed (/vms/" + id + ", VM.Config.Options)")
	}
	return nil
}

func (f *fakePVE) StorageContents(_ context.Context, node, storage, content string, vmid int) ([]pve.StorageContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	var out []pve.StorageContent
	for v, size := range f.files {
		if strings.HasPrefix(v, storage+":import/") {
			out = append(out, pve.StorageContent{VolID: v, Size: size, Content: "import"})
		}
	}
	return out, nil
}

func (f *fakePVE) DeleteStorageVolume(_ context.Context, node, storage, vol string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("delete-file "+storage+":"+vol, true)
	delete(f.files, storage+":"+vol)
	return nil
}

func (f *fakePVE) ClusterResources(context.Context, string) ([]pve.ClusterResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	var out []pve.ClusterResource
	for id, vm := range f.vms {
		t := 0
		if vm.template {
			t = 1
		}
		out = append(out, pve.ClusterResource{Type: "qemu", VMID: id, Node: vm.node, Template: t, Tags: vm.cfg.Str("tags"), Name: vm.cfg.Str("name"), Pool: vm.cfg.Str("pool")})
	}
	return out, nil
}

func (f *fakePVE) CreateVM(_ context.Context, node string, params map[string]string) (pve.UPID, error) {
	f.mu.Lock()
	hook := f.createHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id, _ := strconv.Atoi(params["vmid"])
	f.call("create "+params["vmid"], true)
	// PVE 9.2 checks a create's tags with VM.Config.Options on /vms/<id> without the pool; the provider token only has
	// rights through pc-templates, so the create TASK fails and no VM exists (verified on PVE 9.2.21): the API call
	// itself answers with a UPID, and the 403 comes from the task.
	if _, ok := params["tags"]; ok {
		return pve.UPID("UPID:create-forbidden:" + params["vmid"]), nil
	}
	if _, ok := f.vms[id]; ok {
		return "", errors.New("VM " + params["vmid"] + " already exists")
	}
	cfg := pve.VMConfig{}
	for k, v := range params {
		cfg[k] = v
	}
	f.vms[id] = &fakeVM{node: node, cfg: cfg}
	return "UPID:create", nil
}

func (f *fakePVE) SetVMConfig(_ context.Context, node string, vmid int, params map[string]string, _ ...string) (pve.UPID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call(fmt.Sprintf("config %d", vmid), true)
	vm, ok := f.vms[vmid]
	if !ok {
		return "", &pve.APIError{Status: 500, Message: "Configuration file does not exist"}
	}
	if _, imp := params["scsi0"]; imp && f.importTransportErr {
		return "", errors.New("pve: POST /nodes/pve1/qemu/config: connection reset")
	}
	if _, imp := params["scsi0"]; imp && f.importFail {
		return "UPID:import-fail", nil
	}
	for k, v := range params {
		vm.cfg[k] = v
	}
	if _, final := params["description"]; final && f.finalWaitFails {
		return "UPID:final", nil
	}
	return "UPID:config", nil
}

func (f *fakePVE) GetVMConfig(_ context.Context, node string, vmid int) (pve.VMConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	vm, ok := f.vms[vmid]
	// The provider token reaches a VM only through pool pc-templates (no ACL on /vms): PVE 9.2 answers 403, not 404,
	// for a VMID that does not exist or lies outside that pool (verified on PVE 9.2.21).
	if !ok || vm.node != node || vm.cfg.Str("pool") != pve.TemplatePool {
		return nil, &pve.APIError{Status: 403, Message: fmt.Sprintf("Permission check failed (/vms/%d, VM.Audit)", vmid)}
	}
	out := pve.VMConfig{}
	for k, v := range vm.cfg {
		out[k] = v
	}
	if vm.template {
		out["template"] = 1
	}
	return out, nil
}

func (f *fakePVE) ToTemplate(_ context.Context, node string, vmid int) (pve.UPID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call(fmt.Sprintf("template %d", vmid), true)
	f.vms[vmid].template = true
	return "UPID:template", nil
}

func (f *fakePVE) GetPool(_ context.Context, poolID string) (*pve.PoolInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	out := &pve.PoolInfo{PoolID: poolID}
	for id, vm := range f.vms {
		if vm.cfg.Str("pool") == poolID {
			out.Members = append(out.Members, pve.PoolMember{ID: fmt.Sprintf("qemu/%d", id), Type: "qemu", VMID: id, Node: vm.node})
		}
	}
	return out, nil
}

func (f *fakePVE) DeleteVM(_ context.Context, node string, vmid int) (pve.UPID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call(fmt.Sprintf("destroy %d", vmid), true)
	if f.failDeleteVM {
		return "", errors.New("forbidden")
	}
	delete(f.vms, vmid)
	return "UPID:destroy", nil
}

type fakeBlobs struct {
	data  []byte
	opens int
}

func (b *fakeBlobs) OpenBlob(context.Context, string, string) (io.ReadCloser, error) {
	b.opens++
	return io.NopCloser(bytes.NewReader(b.data)), nil
}

type memReceipts struct {
	mu sync.Mutex
	m  map[string]Receipt
}

func (r *memReceipts) Load(_ context.Context, id string) (*Receipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.m[id]
	if !ok {
		return nil, nil
	}
	v.Warnings = append([]string(nil), v.Warnings...)
	return &v, nil
}

func (r *memReceipts) Save(_ context.Context, rec *Receipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[rec.ID] = *rec
	return nil
}

// testFence allows n checkpoints, then reports a lost lease forever.
type testFence struct {
	epoch  int64
	budget int // <0 = unlimited
	used   int
	// probe/atRefusal record an external mutation counter at the first refusal.
	probe     func() int
	atRefusal int
	refused   bool
	// lostAfter (when ops != nil) loses the lease once ops() exceeds it: time passes between
	// checkpoints, and the executor must ask again before every mutation.
	ops       func() int
	lostAfter int
	okAt      []int // ops() at each successful check
}

func (f *testFence) Epoch() int64 { return f.epoch }
func (f *testFence) Checkpoint(ctx context.Context, fn func(context.Context) error) error {
	if f.budget >= 0 && f.used >= f.budget || f.ops != nil && f.ops() > f.lostAfter {
		if !f.refused && f.probe != nil {
			f.refused, f.atRefusal = true, f.probe()
		}
		return durablejobs.ErrLeaseLost
	}
	f.used++
	if f.ops != nil {
		f.okAt = append(f.okAt, f.ops())
	}
	if fn == nil {
		return nil
	}
	return fn(ctx)
}

const testJob = "0b7c4f2e-1d2a-4c3b-9e8f-112233445566"

func testPayload(disk []byte) VMPayload {
	s := sha256.Sum256(disk)
	return VMPayload{Version: 1, Package: "proxcloud-vm-debian-13", Tag: "debian-13-os-20261111000000-aaaaaaaaaaaa", Digest: dg('9'),
		DiskDigest: "sha256:" + hex.EncodeToString(s[:]), DiskBytes: int64(len(disk)), VirtualSizeBytes: 20 << 30, OS: "debian-13", Variant: "os",
		BuildVersion: "20261111000000", ServiceID: "svc-pve", Node: "pve1", ImportStorage: "nas-import", DiskStorage: "local-zfs", Publish: true}
}

func setup(t *testing.T) (*Executor, *fakePVE, *fakeBlobs, *memReceipts, VMPayload) {
	t.Helper()
	disk := bytes.Repeat([]byte("q"), 4096)
	p := testPayload(disk)
	pv := newFakePVE()
	// An existing published template the job must never touch, and an unrelated VM at 9200.
	pv.vms[9200] = &fakeVM{node: "pve1", template: true, cfg: pve.VMConfig{"name": "pc-debian-13-202611010000", "tags": "proxcloud;pc-public;pc-img-debian-13;pc-v-202611010000", "pool": "pc-templates", "scsi0": "local-zfs:base-9200-disk-0"}}
	rec := &memReceipts{m: map[string]Receipt{testJob: {ID: testJob, Kind: "vm", Payload: p, Step: StepQueued}}}
	blobs := &fakeBlobs{data: disk}
	return &Executor{PVE: pv, Blobs: blobs, Receipts: rec}, pv, blobs, rec, p
}

// ── tests ─────────────────────────────────────────────────────────────────────────────────────

func TestExecutorHappyPathCreatesNewTemplate(t *testing.T) {
	x, pv, _, rec, p := setup(t)
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 1, budget: -1}); err != nil {
		t.Fatal(err)
	}
	r, _ := rec.Load(context.Background(), testJob)
	if r.Step != StepTemplated || r.VMID != 9201 {
		t.Fatalf("receipt: %+v", r)
	}
	vm := pv.vms[9201]
	if !vm.template || vm.cfg.Str("name") != "pc-ghcr-debian-13-os-20261111000000" || vm.cfg.Str("pool") != pve.TemplatePool {
		t.Fatalf("new template: %+v", vm)
	}
	// Every import lands STAGED (lifecycle): import marker + source + staged, no catalog tag at all,
	// even though this payload still says publish=true (an old payload; the field is ignored).
	tags := vm.cfg.Str("tags")
	if !sameTags(tags, "proxcloud;pc-src-999999999999;pc-tu-import;pc-tu-staged") {
		t.Fatalf("staged import tags: %s", tags)
	}
	if rows := pve.DiscoverTemplates([]pve.ClusterResource{{Type: "qemu", VMID: 9201, Node: "pve1", Template: 1, Pool: pve.TemplatePool, Tags: tags}}); len(rows) != 0 {
		t.Fatalf("a staged import entered the catalog: %+v", rows)
	}
	if strings.Contains(tags, OwnerTag(testJob)) || strings.Contains(tags, "pc-tu-"+"0b7c") {
		t.Errorf("the finished template still carries the destructive ownership tag: %s", tags)
	}
	if !strings.Contains(vm.cfg.Str("scsi0"), "import-from=nas-import:import/"+OwnerTag(testJob)+"-e1-") {
		t.Fatalf("import-from: %s", vm.cfg.Str("scsi0"))
	}
	if pv.checkSum != "sha256:"+strings.TrimPrefix(p.DiskDigest, "sha256:") {
		t.Fatalf("PVE was not asked to verify sha256: %q", pv.checkSum)
	}
	// The existing template is untouched, the upload file is removed.
	if old := pv.vms[9200]; old.cfg.Str("tags") != "proxcloud;pc-public;pc-img-debian-13;pc-v-202611010000" || !old.template {
		t.Fatal("existing template modified")
	}
	for _, c := range pv.calls {
		if strings.Contains(c, " 9200") {
			t.Fatalf("existing template touched: %s", c)
		}
	}
	if len(pv.files) != 0 {
		t.Fatalf("upload left behind: %v", pv.files)
	}
}

func TestExecutorStagedWithoutPublish(t *testing.T) {
	x, pv, _, rec, p := setup(t)
	p.Publish = false
	rec.m[testJob] = Receipt{ID: testJob, Payload: p, Step: StepQueued}
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 1, budget: -1}); err != nil {
		t.Fatal(err)
	}
	if tags := pv.vms[9201].cfg.Str("tags"); strings.Contains(tags, "pc-public") || strings.Contains(tags, "pc-img-") || !strings.Contains(tags, "pc-tu-staged") {
		t.Fatalf("staged tags: %s", tags)
	}
}

// A job that landed its final write on the pre-lifecycle version (publish-at-import tags) and
// resumes on this one completes instead of failing as "ownership lost".
func TestFinishAcceptsLegacyFinalTags(t *testing.T) {
	x, pv, _, rec, p := setup(t)
	pv.finalWaitFails = true
	_ = x.Run(context.Background(), testJob, p, &testFence{epoch: 1, budget: -1})
	r, _ := rec.Load(context.Background(), testJob)
	pv.vms[r.VMID].cfg["tags"] = legacyTemplateTags(p)
	pv.finalWaitFails = false
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 2, budget: -1}); err != nil {
		t.Fatalf("resume over legacy tags: %v", err)
	}
	if r, _ = rec.Load(context.Background(), testJob); r.Step != StepTemplated {
		t.Fatalf("receipt %+v", r)
	}
}

func TestExecutorK8sTagsNeverPublish(t *testing.T) {
	disk := []byte("k8s-disk")
	s := sha256.Sum256(disk)
	p := VMPayload{Version: 1, Package: "proxcloud-vm-ubuntu-2604-k8s", Tag: "ubuntu-2604-k8s-1.35.10-20261112010203-cccccccccccc", Digest: dg('8'),
		DiskDigest: "sha256:" + hex.EncodeToString(s[:]), DiskBytes: int64(len(disk)), VirtualSizeBytes: 20 << 30, OS: "ubuntu-2604", Variant: "kubernetes",
		KubernetesVersion: "1.35.10", BuildVersion: "20261112010203", ServiceID: "svc", Node: "pve1", ImportStorage: "imp", DiskStorage: "disk", Publish: true}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	tags := TemplateTags(p)
	if strings.Contains(tags, "pc-public") || strings.Contains(tags, "pc-img-") || !strings.Contains(tags, "pc-k8sver-1-35-10") || !strings.Contains(tags, TagStaged) {
		t.Fatalf("k8s tags: %s", tags)
	}
	if TemplateName(p) != "pc-ghcr-ubuntu-2604-k8s-1-35-10-20261112010203" {
		t.Fatalf("name: %s", TemplateName(p))
	}
}

func TestExecutorRefusesTamperedBytes(t *testing.T) {
	for name, data := range map[string][]byte{
		"different bytes": bytes.Repeat([]byte("x"), 4096),
		"longer":          bytes.Repeat([]byte("q"), 5000),
		"shorter":         bytes.Repeat([]byte("q"), 100),
	} {
		t.Run(name, func(t *testing.T) {
			x, pv, blobs, _, p := setup(t)
			blobs.data = data
			err := x.Run(context.Background(), testJob, p, &testFence{epoch: 1, budget: -1})
			var perm *Permanent
			if !errors.As(err, &perm) || perm.Code != "digest_mismatch" {
				t.Fatalf("want digest_mismatch, got %v", err)
			}
			for _, c := range pv.calls {
				if strings.HasPrefix(c, "create") {
					t.Fatal("a VM was created from unverified bytes")
				}
			}
		})
	}
}

func TestExecutorPVEChecksumRefusalIsPermanent(t *testing.T) {
	x, pv, _, _, p := setup(t)
	pv.uploadFail = "checksum mismatch"
	err := x.Run(context.Background(), testJob, p, &testFence{epoch: 1, budget: -1})
	var perm *Permanent
	if !errors.As(err, &perm) || perm.Code != "pve_checksum_refused" {
		t.Fatalf("got %v", err)
	}
}

// Lost lease: after the fence refuses, the executor performs NO further external mutation.
func TestExecutorStopsAtLostLease(t *testing.T) {
	stopped := 0
	for budget := 0; budget < 16; budget++ {
		x, pv, _, _, p := setup(t)
		f := &testFence{epoch: 1, budget: budget}
		f.probe = func() int { pv.mu.Lock(); defer pv.mu.Unlock(); return pv.mutations }
		err := x.Run(context.Background(), testJob, p, f)
		if err == nil {
			continue // the budget sufficed for the whole run
		}
		stopped++
		if !errors.Is(err, ErrStopped) {
			t.Fatalf("budget %d: want ErrStopped, got %v", budget, err)
		}
		// Not a single external mutation after the fence first refused.
		if pv.mutations != f.atRefusal {
			t.Fatalf("budget %d: %d mutations at refusal, %d at the end: %v", budget, f.atRefusal, pv.mutations, pv.calls)
		}
		if budget == 0 && pv.mutations != 0 {
			t.Fatalf("no lease, yet mutated: %v", pv.calls)
		}
	}
	if stopped < 8 {
		t.Fatalf("only %d budgets stopped the run: the test does not exercise the steps", stopped)
	}
}

// The lease can be lost at ANY moment (here: after the n-th PVE call, reads included). No PVE
// mutation may start after that moment: every mutation must be preceded by a fresh fence check.
func TestExecutorNoMutationAfterLeaseLossAtAnyPoint(t *testing.T) {
	x0, pv0, _, _, p0 := setup(t)
	if err := x0.Run(context.Background(), testJob, p0, &testFence{epoch: 1, budget: -1}); err != nil {
		t.Fatal(err)
	}
	total := pv0.ops
	for n := 0; n <= total; n++ {
		x, pv, _, _, p := setup(t)
		ops := func() int { pv.mu.Lock(); defer pv.mu.Unlock(); return pv.ops }
		f := &testFence{epoch: 1, budget: -1, ops: ops, lostAfter: n}
		_ = x.Run(context.Background(), testJob, p, f)
		// Invariant: every PVE mutation is the very next PVE call after a successful fence check
		// (no read in between), hence never after the lease was lost.
		for _, at := range pv.mutAt {
			ok := false
			for _, c := range f.okAt {
				ok = ok || c == at-1
			}
			if !ok || at > n+1 {
				t.Fatalf("lease lost after op %d: mutation op %d not immediately fenced (checks at %v): %v", n, at, f.okAt, pv.calls)
			}
		}
	}
}

// A new acquisition resumes from the last receipt and never repeats a committed step.
func TestExecutorResumesFromReceipt(t *testing.T) {
	x, pv, blobs, rec, p := setup(t)
	// First acquisition: lease lost right after the upload was recorded as verified.
	f1 := &testFence{epoch: 1, budget: -1}
	pv.createHook = func() { f1.budget = 0 }
	err := x.Run(context.Background(), testJob, p, f1)
	if !errors.Is(err, ErrStopped) && err != nil {
		t.Fatalf("first run: %v", err)
	}
	r, _ := rec.Load(context.Background(), testJob)
	if r.Step != StepCreating && r.Step != StepUploaded {
		t.Fatalf("after first run: %+v", r)
	}
	pv.createHook = nil
	uploadsBefore := blobs.opens
	// Second acquisition resumes.
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 2, budget: -1}); err != nil {
		t.Fatal(err)
	}
	if blobs.opens != uploadsBefore {
		t.Fatalf("the verified upload was downloaded again (%d → %d)", uploadsBefore, blobs.opens)
	}
	r, _ = rec.Load(context.Background(), testJob)
	if r.Step != StepTemplated {
		t.Fatalf("resume: %+v", r)
	}
	templates := 0
	for _, vm := range pv.vms {
		if strings.Contains(vm.cfg.Str("tags"), OwnerTag(testJob)) || vm.cfg.Str("name") == TemplateName(p) {
			templates++
		}
	}
	if templates != 1 {
		t.Fatalf("expected exactly one VM of this job, got %d: %v", templates, pv.calls)
	}
}

// An intent-only upload receipt (outcome unknown) is redone under a fresh name; the stale file
// of the earlier epoch is swept as this job's own.
func TestExecutorRedoesUnknownUpload(t *testing.T) {
	x, pv, blobs, rec, p := setup(t)
	stale := "nas-import:import/" + uploadName(testJob, 1, p.DiskDigest)
	pv.files[stale] = 10 // partial / unknown
	pv.files["nas-import:import/someone-else.qcow2"] = 99
	rec.m[testJob] = Receipt{ID: testJob, Payload: p, Step: StepUploading, UploadFile: uploadName(testJob, 1, p.DiskDigest)}
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 2, budget: -1}); err != nil {
		t.Fatal(err)
	}
	if blobs.opens != 1 {
		t.Fatalf("expected a fresh upload, opens=%d", blobs.opens)
	}
	if _, ok := pv.files[stale]; ok {
		t.Fatal("stale own upload not swept")
	}
	if _, ok := pv.files["nas-import:import/someone-else.qcow2"]; !ok {
		t.Fatal("a foreign import file was deleted")
	}
}

// A receipt VMID taken over by someone else (no ownership tag) is never touched.
func TestExecutorNeverTouchesForeignVMID(t *testing.T) {
	x, pv, _, rec, p := setup(t)
	name := uploadName(testJob, 1, p.DiskDigest)
	pv.files["nas-import:import/"+name] = p.DiskBytes
	pv.vms[9201] = &fakeVM{node: "pve1", cfg: pve.VMConfig{"name": "tenant-vm", "tags": "proxcloud", "scsi0": "x"}}
	rec.m[testJob] = Receipt{ID: testJob, Payload: p, Step: StepCreating, UploadFile: name, VMID: 9201}
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 2, budget: -1}); err != nil {
		t.Fatal(err)
	}
	for _, c := range pv.calls {
		if strings.HasSuffix(c, " 9201") {
			t.Fatalf("foreign VM 9201 touched: %v", pv.calls)
		}
	}
	r, _ := rec.Load(context.Background(), testJob)
	if r.VMID == 9201 || pv.vms[9201].cfg.Str("name") != "tenant-vm" {
		t.Fatalf("receipt %+v", r)
	}
}

// A receipt at StepCreating whose verified upload has vanished goes back to uploading.
func TestExecutorReuploadsVanishedFile(t *testing.T) {
	x, _, blobs, rec, p := setup(t)
	rec.m[testJob] = Receipt{ID: testJob, Payload: p, Step: StepCreating, UploadFile: uploadName(testJob, 1, p.DiskDigest)}
	err := x.Run(context.Background(), testJob, p, &testFence{epoch: 2, budget: -1})
	if err == nil || errors.As(err, new(*Permanent)) {
		t.Fatalf("want a retryable error, got %v", err)
	}
	r, _ := rec.Load(context.Background(), testJob)
	if r.Step != StepQueued {
		t.Fatalf("receipt: %+v", r)
	}
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 3, budget: -1}); err != nil || blobs.opens != 1 {
		t.Fatalf("second run %v opens %d", err, blobs.opens)
	}
}

// BLOCKER regression: PVE clones copy tags. A VM carrying this job's ownership tag OUTSIDE pool
// pc-templates (a CAPMOX worker or tenant clone) is never touched — not by the orphan sweep after a
// success, not by failure cleanup, not by a replay.
func TestClonesWithJobTagAreNeverTouched(t *testing.T) {
	x, pv, _, rec, p := setup(t)
	// A half-finished (unpublished, not yet template) build of this job got cloned by someone.
	for _, id := range []int{7001, 7002} {
		pv.vms[id] = &fakeVM{node: "pve1", cfg: pve.VMConfig{"name": "k8s-worker", "pool": "pc-tenant1", "tags": "proxcloud;" + OwnerTag(testJob) + ";pc-k8s-abc"}}
	}
	pv.vms[7003] = &fakeVM{node: "pve1", cfg: pve.VMConfig{"name": "no-pool", "tags": OwnerTag(testJob)}}
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 1, budget: -1}); err != nil {
		t.Fatal(err)
	}
	r, _ := rec.Load(context.Background(), testJob)
	x.Cleanup(context.Background(), &testFence{epoch: 1, budget: -1}, testJob, p, r) // e.g. a replayed cleanup
	pv.importFail = true
	r.Step, r.VMID = StepCreating, 7001 // a corrupted/replayed receipt pointing at a clone
	_ = rec.Save(context.Background(), r)
	_ = x.Run(context.Background(), testJob, p, &testFence{epoch: 2, budget: -1})
	x.Cleanup(context.Background(), &testFence{epoch: 2, budget: -1}, testJob, p, r)
	for _, id := range []int{7001, 7002, 7003} {
		if _, ok := pv.vms[id]; !ok {
			t.Fatalf("clone %d outside pc-templates was destroyed: %v", id, pv.calls)
		}
		for _, c := range pv.calls {
			if c == fmt.Sprintf("destroy %d", id) || c == fmt.Sprintf("config %d", id) {
				t.Fatalf("clone %d mutated: %v", id, pv.calls)
			}
		}
	}
}

// The finished template is not destroyable by its own job any more (no ownership tag), and a
// cleanup of a finished receipt only drops import files.
func TestFinishedTemplateSurvivesCleanup(t *testing.T) {
	x, pv, _, rec, p := setup(t)
	p.Publish = false // staged: no pc-public, the case the old ownership check missed
	rec.m[testJob] = Receipt{ID: testJob, Payload: p, Step: StepQueued}
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 1, budget: -1}); err != nil {
		t.Fatal(err)
	}
	r, _ := rec.Load(context.Background(), testJob)
	for _, step := range []Step{StepTemplated, StepImported, StepFailed} {
		r.Step = step
		x.Cleanup(context.Background(), &testFence{epoch: 2, budget: -1}, testJob, p, r)
		if _, ok := pv.vms[r.VMID]; !ok {
			t.Fatalf("finished template destroyed by a cleanup at step %s", step)
		}
	}
}

// Item 3: the final name/tags write landed but its task wait failed → the retry completes.
func TestFinishIsIdempotentWhenFinalWriteLanded(t *testing.T) {
	x, pv, _, rec, p := setup(t)
	pv.finalWaitFails = true
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 1, budget: -1}); err == nil {
		t.Fatal("the failed wait was not reported")
	} else if errors.As(err, new(*Permanent)) {
		t.Fatalf("must be retryable, got %v", err)
	}
	pv.finalWaitFails = false
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 2, budget: -1}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	r, _ := rec.Load(context.Background(), testJob)
	if r.Step != StepTemplated || !pv.vms[r.VMID].template {
		t.Fatalf("receipt %+v", r)
	}
	// Same for a published one (pc-public made the old ownership check fail).
	x2, pv2, _, rec2, p2 := setup(t)
	pv2.finalWaitFails = true
	_ = x2.Run(context.Background(), testJob, p2, &testFence{epoch: 1, budget: -1})
	pv2.finalWaitFails = false
	if err := x2.Run(context.Background(), testJob, p2, &testFence{epoch: 2, budget: -1}); err != nil {
		t.Fatalf("published retry: %v", err)
	}
	if r2, _ := rec2.Load(context.Background(), testJob); r2.Step != StepTemplated {
		t.Fatalf("published receipt %+v", r2)
	}
}

// Item 4: an import call that failed before PVE answered is retryable; only a task that finished
// with a non-OK exit status is a permanent refusal.
func TestImportTransportErrorIsRetryable(t *testing.T) {
	x, pv, _, _, p := setup(t)
	pv.importTransportErr = true
	err := x.Run(context.Background(), testJob, p, &testFence{epoch: 1, budget: -1})
	if err == nil || errors.As(err, new(*Permanent)) {
		t.Fatalf("want retryable, got %v", err)
	}
	pv.importTransportErr = false
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 2, budget: -1}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestCleanupOnlyOwnResources(t *testing.T) {
	x, pv, _, rec, p := setup(t)
	pv.importFail = true
	err := x.Run(context.Background(), testJob, p, &testFence{epoch: 1, budget: -1})
	var perm *Permanent
	if !errors.As(err, &perm) || perm.Code != "pve_import_refused" {
		t.Fatalf("got %v", err)
	}
	// Foreign resources that must survive: a VM with a similar name but no ownership tag, a
	// published template mistakenly carrying the tag, another job's file.
	pv.vms[9300] = &fakeVM{node: "pve1", cfg: pve.VMConfig{"name": "pc-tu-build-x", "tags": "proxcloud;pc-tu-other"}}
	pv.vms[9301] = &fakeVM{node: "pve1", template: true, cfg: pve.VMConfig{"tags": "proxcloud;pc-public;" + OwnerTag(testJob)}}
	pv.files["nas-import:import/pc-tu-otherjob000000-e1-x.qcow2"] = 5
	r, _ := rec.Load(context.Background(), testJob)
	warns := x.Cleanup(context.Background(), &testFence{epoch: 1, budget: -1}, testJob, p, r)
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	if _, ok := pv.vms[r.VMID]; ok {
		t.Fatalf("own half-created VM %d not removed", r.VMID)
	}
	for _, id := range []int{9200, 9300, 9301} {
		if _, ok := pv.vms[id]; !ok {
			t.Fatalf("foreign VM %d destroyed", id)
		}
	}
	if _, ok := pv.files["nas-import:import/pc-tu-otherjob000000-e1-x.qcow2"]; !ok {
		t.Fatal("another job's file deleted")
	}
	for v := range pv.files {
		if strings.Contains(v, OwnerTag(testJob)) {
			t.Fatalf("own file left: %s", v)
		}
	}
}

func TestPayloadValidation(t *testing.T) {
	_, _, _, _, p := setup(t)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(*VMPayload){
		func(q *VMPayload) {
			q.Tag = "debian-13-os-20261111000000-bbbbbbbbbbbb"
			q.BuildVersion = "20261112000000"
		},
		func(q *VMPayload) { q.Package = "proxcloud-vm-debian-12" },
		func(q *VMPayload) { q.DiskBytes = MaxDiskBytes },
		func(q *VMPayload) { q.Node = "pve1/../x" },
		func(q *VMPayload) { q.DiskDigest = "sha256:abc" },
		func(q *VMPayload) { q.Variant = "kubernetes" },
	}
	for i, fn := range bad {
		q := p
		fn(&q)
		if q.Validate() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	raw := []byte(`{"version":1,"package":"proxcloud-vm-debian-13","extra":1}`)
	if _, err := DecodeVMPayload(raw); err == nil {
		t.Fatal("unknown field accepted")
	}
}

// PVE 9.2: a create's tags are checked on /vms/<id> without the pool (the fake refuses them as PVE does), so the job
// creates the VM untagged and stamps its ownership tag with the import write. Between the two, the VM is ours only
// by exactly this job's build name, membership of pc-templates and the absence of every tag.
func TestOwnershipWindowBeforeTheImportWrite(t *testing.T) {
	x, pv, _, _, p := setup(t)
	other := "11111111-2222-3333-4444-555555555555"
	tpl := pve.TemplatePool
	for id, cfg := range map[int]pve.VMConfig{
		9201: {"name": buildName(testJob), "pool": tpl},
		9202: {"name": buildName(testJob), "pool": "pc-tenant1"},
		9203: {"name": buildName(other), "pool": tpl},
		9204: {"name": buildName(testJob), "pool": tpl, "tags": "proxcloud"},
		9205: {"name": buildName(testJob), "pool": tpl, "tags": OwnerTag(testJob) + ";" + pve.TagTemplatePub},
		9206: {"name": "pc-tu-build-x", "pool": tpl},
		9207: {"name": "anything", "pool": tpl, "tags": pve.TagManaged + ";" + OwnerTag(testJob)},
	} {
		pv.vms[id] = &fakeVM{node: "pve1", cfg: cfg}
	}
	want := map[int]bool{9201: true, 9202: false, 9203: false, 9204: false, 9205: false, 9206: false, 9207: true}
	for id, w := range want {
		// A VM outside pc-templates is out of the token's view (403): not ours, never touched.
		exists, own, _, err := x.owned(context.Background(), testJob, p, id)
		if err != nil || own != w || (w && !exists) {
			t.Errorf("VM %d: exists=%v own=%v err=%v, want own=%v", id, exists, own, err, w)
		}
	}
}

// A job that died between the untagged create and the import write finds its own empty VM by name, removes it and
// starts over; an untagged VM with another job's build name is never touched.
func TestExecutorRemovesItsUntaggedHalfCreatedVM(t *testing.T) {
	x, pv, _, rec, p := setup(t)
	name := uploadName(testJob, 1, p.DiskDigest)
	pv.files["nas-import:import/"+name] = p.DiskBytes
	other := "11111111-2222-3333-4444-555555555555"
	pv.vms[9201] = &fakeVM{node: "pve1", cfg: pve.VMConfig{"name": buildName(testJob), "pool": pve.TemplatePool}}
	pv.vms[9202] = &fakeVM{node: "pve1", cfg: pve.VMConfig{"name": buildName(other), "pool": pve.TemplatePool}}
	rec.m[testJob] = Receipt{ID: testJob, Payload: p, Step: StepCreating, UploadFile: name, VMID: 9201}
	if err := x.Run(context.Background(), testJob, p, &testFence{epoch: 2, budget: -1}); err != nil {
		t.Fatal(err)
	}
	destroyed := false
	for _, c := range pv.calls {
		destroyed = destroyed || c == "destroy 9201"
		if strings.HasSuffix(c, " 9202") {
			t.Fatalf("another job's VM touched: %v", pv.calls)
		}
	}
	if !destroyed {
		t.Fatalf("the half-created VM was not removed: %v", pv.calls)
	}
	r, _ := rec.Load(context.Background(), testJob)
	if r.Step != StepTemplated || pv.vms[r.VMID] == nil || !pv.vms[r.VMID].template {
		t.Fatalf("receipt %+v", r)
	}
	if pv.vms[9202].cfg.Str("name") != buildName(other) {
		t.Fatal("another job's VM changed")
	}
}

// A superseded acquisition whose untagged create landed after it lost its lease leaves an empty VM with only this
// job's build name: the orphan sweep removes it like a tagged one, and never an untagged VM of another job.
func TestSweepRemovesUntaggedOrphanOfThisJobOnly(t *testing.T) {
	x, pv, _, _, p := setup(t)
	other := "11111111-2222-3333-4444-555555555555"
	pv.vms[9201] = &fakeVM{node: "pve1", cfg: pve.VMConfig{"name": buildName(testJob), "pool": pve.TemplatePool}}
	pv.vms[9202] = &fakeVM{node: "pve1", cfg: pve.VMConfig{"name": buildName(other), "pool": pve.TemplatePool}}
	pv.vms[9203] = &fakeVM{node: "pve1", cfg: pve.VMConfig{"name": buildName(testJob), "pool": "pc-tenant1"}}
	if warns := x.sweepOrphans(context.Background(), &testFence{epoch: 1, budget: -1}, testJob, p, 0); len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	if pv.vms[9201] != nil {
		t.Fatal("this job's untagged orphan was left in pc-templates")
	}
	if pv.vms[9202] == nil || pv.vms[9203] == nil || pv.vms[9200] == nil {
		t.Fatalf("a VM that is not this job's was removed: %v", pv.calls)
	}
}
