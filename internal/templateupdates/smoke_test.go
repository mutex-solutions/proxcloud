package templateupdates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
)

// ── a fake PVE for the smoke test: clones, power, guest agent ─────────────────────────────────

type smokeVM struct {
	node     string
	cfg      pve.VMConfig
	template bool
	running  bool
	grownGiB int
}

type smokePVE struct {
	mu        sync.Mutex
	vms       map[int]*smokeVM
	calls     []string
	mutations int
	ops       int
	mutAt     []int
	// knobs
	agentNever       bool   // the guest agent never answers
	osID, osVersion  string // guest-get-osinfo (defaults from the clone's source)
	ciStatus         string // cloud-init status ("done" default)
	ciBadJSON        bool
	noGrow           bool             // growpart did not run: the root fs keeps the template size
	noFSSize         bool             // guest-get-fsinfo without sizes, df fails too
	execForbidden    bool             // guest-exec is refused (403)
	cloneRunning     bool             // the clone task has not ended (PVE adds the clone to its pool only when it ends)
	cloneEndsNow     int              // the in-flight clone of this VMID ends at the next CloneRunning read
	poolErr          error            // GetPool fails with it
	poolMembersExtra []pve.PoolMember // listed as members of every pool read (a member the token still cannot read)
	cloneFails       bool             // the clone task fails
	cloneTransport   bool             // the clone call fails before PVE answered (retryable)
	startFails       bool
	kubelet          string // "" = missing; else `kubelet --version` output
	deleteFails      bool
	stopHook         func()
	cloneHook        func(id int)
	baseDiskBytes    int64
}

func newSmokePVE() *smokePVE {
	return &smokePVE{vms: map[int]*smokeVM{}, ciStatus: "done", baseDiskBytes: 20 << 30}
}

func (f *smokePVE) mut(s string) {
	f.ops++
	f.calls = append(f.calls, s)
	f.mutations++
	f.mutAt = append(f.mutAt, f.ops)
}
func (f *smokePVE) read() { f.ops++ }

func (f *smokePVE) ClusterResources(context.Context, string) ([]pve.ClusterResource, error) {
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

func (f *smokePVE) GetPool(_ context.Context, poolID string) (*pve.PoolInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	if f.poolErr != nil {
		return nil, f.poolErr
	}
	out := &pve.PoolInfo{PoolID: poolID}
	for id, vm := range f.vms {
		if vm.cfg.Str("pool") == poolID {
			out.Members = append(out.Members, pve.PoolMember{Type: "qemu", VMID: id, Node: vm.node})
		}
	}
	out.Members = append(out.Members, f.poolMembersExtra...)
	return out, nil
}

func (f *smokePVE) GetVMConfig(_ context.Context, node string, vmid int) (pve.VMConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	vm, ok := f.vms[vmid]
	// The provider token reaches a VM only through pool pc-templates (no ACL on /vms): PVE 9.2 answers 403, not 404,
	// for a VMID that does not exist or lies outside that pool (verified on PVE 9.2.21).
	if !ok || vm.node != node || vm.cfg.Str("pool") != pve.TemplatePool {
		return nil, &pve.APIError{Status: 403, Message: fmt.Sprintf("Permission check failed (/vms/%d, VM.Audit)", vmid)}
	}
	out := pve.VMConfig{"digest": "d"}
	for k, v := range vm.cfg {
		out[k] = v
	}
	if vm.template {
		out["template"] = 1
	}
	return out, nil
}

func (f *smokePVE) CloneVM(_ context.Context, node string, vmid int, o pve.CloneOpts) (pve.UPID, error) {
	f.mu.Lock()
	f.mut(fmt.Sprintf("clone %d->%d pool=%s", vmid, o.NewID, o.Pool))
	if f.cloneTransport {
		f.mu.Unlock()
		return "", errors.New("pve: POST /clone: connection reset")
	}
	src := f.vms[vmid]
	cfg := pve.VMConfig{}
	for k, v := range src.cfg {
		cfg[k] = v // tags are copied, like PVE
	}
	cfg["name"], cfg["pool"] = o.Name, o.Pool
	f.vms[o.NewID] = &smokeVM{node: node, cfg: cfg}
	hook := f.cloneHook
	f.mu.Unlock()
	if hook != nil {
		hook(o.NewID)
	}
	if f.cloneFails {
		return "UPID:fail", nil
	}
	return "UPID:clone", nil
}

func (f *smokePVE) SetVMConfig(_ context.Context, node string, vmid int, params map[string]string, del ...string) (pve.UPID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mut(fmt.Sprintf("config %d", vmid))
	vm, ok := f.vms[vmid]
	if !ok {
		return "", &pve.APIError{Status: 500, Message: "does not exist"}
	}
	for k, v := range params {
		if k != "digest" {
			vm.cfg[k] = v
		}
	}
	for _, d := range del {
		delete(vm.cfg, d)
	}
	return "UPID:config", nil
}

func (f *smokePVE) ResizeDisk(_ context.Context, node string, vmid int, disk, size string) (pve.UPID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mut(fmt.Sprintf("resize %d %s", vmid, size))
	var n int
	fmt.Sscanf(size, "+%dG", &n)
	f.vms[vmid].grownGiB += n
	return "UPID:resize", nil
}

func (f *smokePVE) StartVM(_ context.Context, node string, vmid int) (pve.UPID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mut(fmt.Sprintf("start %d", vmid))
	if f.startFails {
		return "UPID:fail", nil
	}
	f.vms[vmid].running = true
	return "UPID:start", nil
}

func (f *smokePVE) StopVM(_ context.Context, node string, vmid int) (pve.UPID, error) {
	f.mu.Lock()
	f.mut(fmt.Sprintf("stop %d", vmid))
	if vm, ok := f.vms[vmid]; ok {
		vm.running = false
	}
	hook := f.stopHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return "UPID:stop", nil
}

func (f *smokePVE) DeleteVM(_ context.Context, node string, vmid int) (pve.UPID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mut(fmt.Sprintf("destroy %d", vmid))
	if f.deleteFails {
		return "", errors.New("pve: DELETE: connection reset")
	}
	delete(f.vms, vmid)
	return "UPID:destroy", nil
}

func (f *smokePVE) GetVMStatus(_ context.Context, node string, vmid int) (*pve.VMStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	vm, ok := f.vms[vmid]
	if !ok {
		return nil, &pve.APIError{Status: 500, Message: "does not exist"}
	}
	if vm.running {
		return &pve.VMStatus{Status: "running"}, nil
	}
	return &pve.VMStatus{Status: "stopped"}, nil
}

// CloneRunning: a clone marked in flight (cloneRunning) is running; cloneEndsNow ends it on this read (the clone task
// stops and its VM joins the pool), as PVE can between owned()'s two reads.
func (f *smokePVE) CloneRunning(_ context.Context, node string, srcVMID int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	if f.cloneEndsNow != 0 {
		f.vms[f.cloneEndsNow].cfg["pool"] = pve.TemplatePool
		f.cloneEndsNow, f.cloneRunning = 0, false
		return false, nil
	}
	return f.cloneRunning, nil
}

func (f *smokePVE) WaitTask(_ context.Context, up pve.UPID) error {
	f.mu.Lock()
	f.read()
	f.mu.Unlock()
	if up == "UPID:fail" {
		return errors.New("pve: task UPID:fail failed: boom")
	}
	return nil
}

func (f *smokePVE) agentUp(vmid int) bool {
	vm, ok := f.vms[vmid]
	return ok && vm.running && !f.agentNever
}

func (f *smokePVE) AgentPing(_ context.Context, node string, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	if !f.agentUp(vmid) {
		return &pve.APIError{Status: 500, Message: "QEMU guest agent is not running"}
	}
	return nil
}

func (f *smokePVE) AgentOSInfo(_ context.Context, node string, vmid int) (*pve.AgentOSInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	if !f.agentUp(vmid) {
		return nil, &pve.APIError{Status: 500, Message: "QEMU guest agent is not running"}
	}
	return &pve.AgentOSInfo{ID: f.osID, VersionID: f.osVersion, PrettyName: "Debian GNU/Linux 13 (trixie)"}, nil
}

func (f *smokePVE) rootSize(vmid int) int64 {
	vm := f.vms[vmid]
	if f.noGrow {
		return f.baseDiskBytes - 512<<20
	}
	return f.baseDiskBytes + int64(vm.grownGiB)<<30 - 512<<20
}

func (f *smokePVE) AgentFSInfo(_ context.Context, node string, vmid int) ([]pve.AgentFS, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read()
	if f.noFSSize {
		return []pve.AgentFS{{Mountpoint: "/", Type: "ext4"}}, nil
	}
	n := f.rootSize(vmid)
	return []pve.AgentFS{{Mountpoint: "/boot/efi", Type: "vfat", TotalBytes: ptr64(100 << 20)}, {Mountpoint: "/", Type: "ext4", TotalBytes: &n}}, nil
}

func ptr64(n int64) *int64 { return &n }

func (f *smokePVE) GuestRun(_ context.Context, node string, vmid int, _ time.Duration, args ...string) (int, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mut(fmt.Sprintf("exec %d %s", vmid, args[0]))
	if f.execForbidden {
		return 0, "", "", &pve.APIError{Status: 403, Message: "Permission check failed"}
	}
	if !f.agentUp(vmid) {
		return 0, "", "", &pve.APIError{Status: 500, Message: "agent not running"}
	}
	switch args[0] {
	case "/usr/bin/cloud-init":
		if f.ciBadJSON {
			return 0, "not json", "", nil
		}
		b, _ := json.Marshal(map[string]any{"status": f.ciStatus, "extended_status": f.ciStatus})
		return 0, string(b), "", nil
	case "/usr/bin/df":
		if f.noFSSize {
			return 1, "", "df: no", nil
		}
		return 0, fmt.Sprintf("   1B-blocks\n%d\n", f.rootSize(vmid)), "", nil
	case "/bin/sh":
		if f.kubelet == "" {
			return 4, "", "", nil
		}
		return 0, f.kubelet + "\n", "", nil
	}
	return 127, "", "", nil
}

// ── fixture ───────────────────────────────────────────────────────────────────────────────────

type memSmokes struct {
	mu sync.Mutex
	m  map[string]SmokeReceipt
}

func (s *memSmokes) LoadSmoke(_ context.Context, id string) (*SmokeReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id]
	if !ok {
		return nil, nil
	}
	v.Warnings = append([]string(nil), v.Warnings...)
	return &v, nil
}

func (s *memSmokes) SaveSmoke(_ context.Context, r *SmokeReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[r.ID] = *r
	return nil
}

const smokeJob = "5a0e0c11-2233-4455-8899-aabbccddeeff"

// smokePayloadFor is the smoke of a staged debian-13 import (template 9300 on pve1).
func smokePayloadFor(variant string) SmokePayload {
	p := SmokePayload{Version: 1, ImportID: testJob, Package: "proxcloud-vm-debian-13", Tag: "debian-13-os-20261111000000-aaaaaaaaaaaa",
		Digest: dg('9'), OS: "debian-13", Variant: "os", BuildVersion: "20261111000000", ServiceID: "svc-pve", Node: "pve1",
		TemplateVMID: 9300, DiskStorage: "local-zfs", Network: SmokeNetwork{Bridge: "vmbr0", VLAN: 29, IP: "192.0.2.50/24", Gateway: "192.0.2.1"}}
	if variant == "kubernetes" {
		p.Package, p.Tag, p.OS, p.Variant, p.KubernetesVersion, p.Digest = "proxcloud-vm-ubuntu-2604-k8s", "ubuntu-2604-k8s-1.35.10-20261112010203-cccccccccccc",
			"ubuntu-2604", "kubernetes", "1.35.10", dg('8')
		p.BuildVersion = "20261112010203"
	}
	return p
}

func smokeSetup(t *testing.T, variant string) (*SmokeExecutor, *smokePVE, *memSmokes, SmokePayload) {
	t.Helper()
	p := smokePayloadFor(variant)
	pv := newSmokePVE()
	pv.osID, pv.osVersion = "debian", "13"
	if variant == "kubernetes" {
		pv.osID, pv.osVersion, pv.kubelet = "ubuntu", "26.04", "Kubernetes v1.35.10"
	}
	// The staged template under test, a published pipeline template, a tenant VM, a CAPMOX worker
	// that copied a smoke owner tag: only the test VM may ever be mutated.
	pv.vms[9300] = &smokeVM{node: "pve1", template: true, cfg: pve.VMConfig{"name": TemplateName(p.importPayload()), "pool": pve.TemplatePool,
		"tags": TemplateTags(p.importPayload()), "scsi0": "local-zfs:base-9300-disk-0,discard=on,size=20G"}}
	pv.vms[9200] = &smokeVM{node: "pve1", template: true, cfg: pve.VMConfig{"name": "pc-debian-13-202611010000", "pool": pve.TemplatePool,
		"tags": "proxcloud;pc-public;pc-img-debian-13;pc-v-202611010000", "scsi0": "local-zfs:base-9200-disk-0,size=3G"}}
	pv.vms[101] = &smokeVM{node: "pve1", running: true, cfg: pve.VMConfig{"name": "tenant", "pool": "pc-tenant1", "tags": "proxcloud"}}
	pv.vms[102] = &smokeVM{node: "pve1", running: true, cfg: pve.VMConfig{"name": SmokeVMName(smokeJob), "pool": "pc-tenant1", "tags": "proxcloud;" + SmokeOwnerTag(smokeJob)}}
	rec := &memSmokes{m: map[string]SmokeReceipt{smokeJob: {ID: smokeJob, ImportID: testJob, Payload: p, Step: SmokeQueued}}}
	x := &SmokeExecutor{PVE: pv, Receipts: rec, Poll: time.Millisecond, BootTimeout: 50 * time.Millisecond, CloudInitTimeout: 50 * time.Millisecond}
	return x, pv, rec, p
}

func protectedIntact(t *testing.T, pv *smokePVE) {
	t.Helper()
	for _, id := range []int{9300, 9200, 101, 102} {
		if _, ok := pv.vms[id]; !ok {
			t.Fatalf("VM %d was destroyed: %v", id, pv.calls)
		}
		for _, c := range pv.calls {
			if strings.HasSuffix(c, fmt.Sprintf(" %d", id)) && !strings.HasPrefix(c, "clone") {
				t.Fatalf("VM %d was mutated: %v", id, pv.calls)
			}
		}
	}
	if !sameTags(pv.vms[9300].cfg.Str("tags"), TemplateTags(smokePayloadFor("os").importPayload())) && !strings.Contains(pv.vms[9300].cfg.Str("tags"), "pc-k8s") {
		t.Fatalf("the template's tags changed: %s", pv.vms[9300].cfg.Str("tags"))
	}
}

func testVMsLeft(pv *smokePVE) []int {
	var out []int
	for id, vm := range pv.vms {
		if strings.Contains(vm.cfg.Str("tags"), "pc-tu-smoke;") || vm.cfg.Str("pool") == pve.TemplatePool && !vm.template {
			out = append(out, id)
		}
	}
	return out
}

// ── tests ─────────────────────────────────────────────────────────────────────────────────────

func TestSmokePassRecordsAndCleansUp(t *testing.T) {
	for _, variant := range []string{"os", "kubernetes"} {
		t.Run(variant, func(t *testing.T) {
			x, pv, rec, p := smokeSetup(t, variant)
			if err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 1, budget: -1}); err != nil {
				t.Fatal(err)
			}
			r, _ := rec.LoadSmoke(context.Background(), smokeJob)
			if r.Result != SmokePass || r.Step != SmokeFinished || !r.Cleaned || r.FinishedAt == nil || r.CleanedAt == nil || r.StartedAt == nil {
				t.Fatalf("receipt %+v", r)
			}
			if !strings.HasPrefix(r.KeyFingerprint, "SHA256:") || r.Checks.RootFSBytes <= r.Checks.TemplateDiskBytes || r.Checks.CloudInitStatus != "done" {
				t.Fatalf("checks %+v fp %q", r.Checks, r.KeyFingerprint)
			}
			if variant == "kubernetes" && r.Checks.Kubelet != "Kubernetes v1.35.10" {
				t.Fatalf("kubelet %q", r.Checks.Kubelet)
			}
			if left := testVMsLeft(pv); len(left) != 0 {
				t.Fatalf("test VM left: %v %v", left, pv.calls)
			}
			// The clone went into pc-templates, never a tenant pool; the key is public only.
			var cloned bool
			for _, c := range pv.calls {
				cloned = cloned || strings.HasPrefix(c, "clone 9300->") && strings.HasSuffix(c, "pool=pc-templates")
			}
			if !cloned {
				t.Fatalf("no clone into pc-templates: %v", pv.calls)
			}
			protectedIntact(t, pv)
			b, _ := json.Marshal(r)
			if strings.Contains(string(b), "PRIVATE KEY") || strings.Contains(string(b), "ssh-ed25519") {
				t.Fatal("key material in the receipt")
			}
		})
	}
}

// The test VM's cloud-init: ephemeral key, no password, the smoke network, owner tag replacing the
// template's copied tags.
func TestSmokeCloudInitShape(t *testing.T) {
	x, pv, _, p := smokeSetup(t, "os")
	var seen pve.VMConfig
	pv.stopHook = func() {
		pv.mu.Lock()
		defer pv.mu.Unlock()
		for _, vm := range pv.vms {
			if strings.Contains(vm.cfg.Str("tags"), "pc-tu-smoke;") {
				seen = vm.cfg
			}
		}
	}
	if err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 1, budget: -1}); err != nil {
		t.Fatal(err)
	}
	if seen == nil {
		t.Fatal("no test VM observed")
	}
	if seen.Str("net0") != "virtio,bridge=vmbr0,tag=29" || seen.Str("ipconfig0") != "ip=192.0.2.50/24,gw=192.0.2.1" ||
		!strings.HasPrefix(seen.Str("sshkeys"), "ssh-ed25519%20") || seen.Str("cipassword") != "" ||
		!sameTags(seen.Str("tags"), "proxcloud;pc-tu-smoke;"+SmokeOwnerTag(smokeJob)) {
		t.Fatalf("test VM config: %+v", seen)
	}
}

// Every FAIL path records its fixed code and removes the test VM; nothing else is touched.
func TestSmokeFailPathsCleanUp(t *testing.T) {
	cases := map[string]struct {
		knob func(*smokePVE)
		code string
	}{
		"agent never answers":     {func(f *smokePVE) { f.agentNever = true }, SmokeAgentTimeout},
		"wrong os":                {func(f *smokePVE) { f.osID, f.osVersion = "ubuntu", "24.04" }, SmokeOSInfoMismatch},
		"cloud-init error":        {func(f *smokePVE) { f.ciStatus = "error" }, SmokeCloudInitFailed},
		"cloud-init never done":   {func(f *smokePVE) { f.ciStatus = "running" }, SmokeCloudInitTimeout},
		"cloud-init unreadable":   {func(f *smokePVE) { f.ciBadJSON = true }, SmokeCloudInitBadOut},
		"root fs did not grow":    {func(f *smokePVE) { f.noGrow = true }, SmokeDiskNotGrown},
		"root fs size unreadable": {func(f *smokePVE) { f.noFSSize = true }, SmokeDiskUnverifiable},
		"clone task failed":       {func(f *smokePVE) { f.cloneFails = true }, SmokeCloneFailed},
		"start failed":            {func(f *smokePVE) { f.startFails = true }, SmokeStartFailed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			x, pv, rec, p := smokeSetup(t, "os")
			c.knob(pv)
			if err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 1, budget: -1}); err != nil {
				t.Fatal(err)
			}
			r, _ := rec.LoadSmoke(context.Background(), smokeJob)
			if r.Result != SmokeFail || r.Code != c.code || !r.Cleaned || r.Step != SmokeFinished {
				t.Fatalf("receipt %+v", r)
			}
			if left := testVMsLeft(pv); len(left) != 0 {
				t.Fatalf("test VM left: %v %v", left, pv.calls)
			}
			protectedIntact(t, pv)
		})
	}
	// Kubernetes: components missing / wrong version.
	for name, kubelet := range map[string]string{"missing": "", "version": "Kubernetes v1.34.1"} {
		t.Run("k8s "+name, func(t *testing.T) {
			x, pv, rec, p := smokeSetup(t, "kubernetes")
			pv.kubelet = kubelet
			if err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 1, budget: -1}); err != nil {
				t.Fatal(err)
			}
			r, _ := rec.LoadSmoke(context.Background(), smokeJob)
			want := SmokeK8sMissing
			if kubelet != "" {
				want = SmokeK8sVersion
			}
			if r.Result != SmokeFail || r.Code != want || len(testVMsLeft(pv)) != 0 {
				t.Fatalf("receipt %+v left %v", r, testVMsLeft(pv))
			}
		})
	}
}

// A template that is not this import any more (renamed, retagged, gone) is never cloned.
func TestSmokeRefusesForeignTemplate(t *testing.T) {
	x, pv, rec, p := smokeSetup(t, "os")
	pv.vms[9300].cfg["tags"] = "proxcloud;pc-src-000000000000;pc-tu-import;pc-tu-staged"
	if err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 1, budget: -1}); err != nil {
		t.Fatal(err)
	}
	r, _ := rec.LoadSmoke(context.Background(), smokeJob)
	if r.Result != SmokeFail || r.Code != SmokeTemplateMismatch || pv.mutations != 0 {
		t.Fatalf("receipt %+v calls %v", r, pv.calls)
	}
}

// A withdrawn digest is not smoke-tested.
func TestSmokeRefusesWithdrawnDigest(t *testing.T) {
	x, pv, rec, p := smokeSetup(t, "os")
	x.Blocked = func(context.Context, string) (bool, error) { return true, nil }
	_ = x.Run(context.Background(), smokeJob, p, &testFence{epoch: 1, budget: -1})
	if r, _ := rec.LoadSmoke(context.Background(), smokeJob); r.Code != SmokeDigestWithdrawn || pv.mutations != 0 {
		t.Fatalf("receipt %+v calls %v", r, pv.calls)
	}
}

// A retryable infrastructure error mid-test removes this attempt's test VM before returning.
func TestSmokeRetryableErrorCleansAttempt(t *testing.T) {
	x, pv, rec, p := smokeSetup(t, "os")
	calls := 0
	x.PVE = &flakyResize{smokePVE: pv, fail: func() bool { calls++; return calls == 1 }}
	err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 1, budget: -1})
	if err == nil || errors.As(err, new(*Permanent)) || errors.Is(err, ErrStopped) {
		t.Fatalf("want retryable, got %v", err)
	}
	if left := testVMsLeft(pv); len(left) != 0 {
		t.Fatalf("attempt's test VM left: %v %v", left, pv.calls)
	}
	r, _ := rec.LoadSmoke(context.Background(), smokeJob)
	if r.Result != "" || r.VMID != 0 {
		t.Fatalf("receipt %+v", r)
	}
	// The next attempt runs to PASS.
	if err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 2, budget: -1}); err != nil {
		t.Fatal(err)
	}
	if r, _ = rec.LoadSmoke(context.Background(), smokeJob); r.Result != SmokePass {
		t.Fatalf("second attempt %+v", r)
	}
	protectedIntact(t, pv)
}

// flakyResize fails the disk grow before PVE answered (a transport error: retryable).
type flakyResize struct {
	*smokePVE
	fail func() bool
}

func (f *flakyResize) ResizeDisk(ctx context.Context, node string, vmid int, disk, size string) (pve.UPID, error) {
	if f.fail() {
		return "", errors.New("pve: PUT resize: connection reset")
	}
	return f.smokePVE.ResizeDisk(ctx, node, vmid, disk, size)
}

type flakyExec struct {
	*smokePVE
	fail func() bool
}

func (f *flakyExec) GuestRun(ctx context.Context, node string, vmid int, d time.Duration, args ...string) (int, string, string, error) {
	if f.fail() {
		return 0, "", "", errors.New("pve: POST agent/exec: connection reset")
	}
	return f.smokePVE.GuestRun(ctx, node, vmid, d, args...)
}

// guest-exec refused (no VM.GuestAgent.Unrestricted) is permanent: the runner records FAIL.
func TestSmokeExecForbiddenIsPermanent(t *testing.T) {
	x, pv, _, p := smokeSetup(t, "os")
	pv.execForbidden = true
	err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 1, budget: -1})
	var perm *Permanent
	if !errors.As(err, &perm) || perm.Code != SmokeExecForbidden {
		t.Fatalf("got %v", err)
	}
}

// Lost lease at ANY point: no PVE mutation after it, every mutation immediately fenced.
func TestSmokeNoMutationAfterLeaseLossAtAnyPoint(t *testing.T) {
	x0, pv0, _, p0 := smokeSetup(t, "os")
	if err := x0.Run(context.Background(), smokeJob, p0, &testFence{epoch: 1, budget: -1}); err != nil {
		t.Fatal(err)
	}
	total := pv0.ops
	for n := 0; n <= total; n++ {
		x, pv, _, p := smokeSetup(t, "os")
		ops := func() int { pv.mu.Lock(); defer pv.mu.Unlock(); return pv.ops }
		f := &testFence{epoch: 1, budget: -1, ops: ops, lostAfter: n}
		_ = x.Run(context.Background(), smokeJob, p, f)
		for _, at := range pv.mutAt {
			ok := false
			for _, c := range f.okAt {
				ok = ok || c == at-1
			}
			if !ok || at > n+1 {
				t.Fatalf("lease lost after op %d: mutation op %d not immediately fenced (checks %v): %v", n, at, f.okAt, pv.calls)
			}
		}
		protectedIntact(t, pv)
	}
}

// A new acquisition removes the earlier attempt's test VM (ownership-checked) and starts over;
// a VMID in the receipt that someone else now holds is never touched.
func TestSmokeResumeRemovesOwnAndSparesForeign(t *testing.T) {
	x, pv, rec, p := smokeSetup(t, "os")
	pv.vms[9301] = &smokeVM{node: "pve1", running: true, cfg: pve.VMConfig{"name": SmokeVMName(smokeJob), "pool": pve.TemplatePool, "tags": "proxcloud;pc-tu-smoke;" + SmokeOwnerTag(smokeJob)}}
	rec.m[smokeJob] = SmokeReceipt{ID: smokeJob, ImportID: testJob, Payload: p, Step: SmokeBooting, VMID: 9301}
	if err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 2, budget: -1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := pv.vms[9301]; ok {
		t.Fatal("the earlier attempt's test VM survived")
	}
	// Foreign VMID in the receipt.
	x2, pv2, rec2, p2 := smokeSetup(t, "os")
	pv2.vms[9301] = &smokeVM{node: "pve1", running: true, cfg: pve.VMConfig{"name": "someone", "pool": pve.TemplatePool, "tags": "proxcloud"}}
	rec2.m[smokeJob] = SmokeReceipt{ID: smokeJob, ImportID: testJob, Payload: p2, Step: SmokeBooting, VMID: 9301}
	if err := x2.Run(context.Background(), smokeJob, p2, &testFence{epoch: 2, budget: -1}); err != nil {
		t.Fatal(err)
	}
	if vm := pv2.vms[9301]; vm == nil || vm.cfg.Str("name") != "someone" {
		t.Fatal("a foreign VM was touched")
	}
	for _, c := range pv2.calls {
		if strings.HasSuffix(c, " 9301") {
			t.Fatalf("foreign 9301 mutated: %v", pv2.calls)
		}
	}
}

// The clone named itself but the config write never landed (still the template's copied tags):
// the name proves ownership, so cleanup removes it. A same-named VM outside pc-templates or a
// template is never touched.
func TestSmokeCleanupByNameOnlyInsidePool(t *testing.T) {
	x, pv, rec, p := smokeSetup(t, "os")
	tags := TemplateTags(p.importPayload())
	pv.vms[9301] = &smokeVM{node: "pve1", cfg: pve.VMConfig{"name": SmokeVMName(smokeJob), "pool": pve.TemplatePool, "tags": tags}}
	pv.vms[9302] = &smokeVM{node: "pve1", template: true, cfg: pve.VMConfig{"name": SmokeVMName(smokeJob), "pool": pve.TemplatePool, "tags": tags}}
	r := &SmokeReceipt{ID: smokeJob, Payload: p, Step: SmokeCloning, VMID: 9301}
	rec.m[smokeJob] = *r
	warns := x.Cleanup(context.Background(), &testFence{epoch: 3, budget: -1}, smokeJob, p, r)
	if len(warns) != 0 {
		t.Fatalf("warnings %v", warns)
	}
	if _, ok := pv.vms[9301]; ok {
		t.Fatal("own half-configured clone survived")
	}
	if _, ok := pv.vms[9302]; !ok {
		t.Fatal("a template was destroyed")
	}
	protectedIntact(t, pv)
}

func TestSmokePayloadValidation(t *testing.T) {
	p := smokePayloadFor("os")
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*SmokePayload){
		"no bridge":     func(q *SmokePayload) { q.Network.Bridge = "" },
		"bad vlan":      func(q *SmokePayload) { q.Network.VLAN = 4095 },
		"gw outside":    func(q *SmokePayload) { q.Network.Gateway = "198.51.100.1" },
		"gw w/o ip":     func(q *SmokePayload) { q.Network.IP, q.Network.Gateway = "", "192.0.2.1" },
		"bad digest":    func(q *SmokePayload) { q.Digest = "sha256:x" },
		"tag mismatch":  func(q *SmokePayload) { q.OS = "debian-12" },
		"bad template":  func(q *SmokePayload) { q.TemplateVMID = 0 },
		"bridge inject": func(q *SmokePayload) { q.Network.Bridge = "vmbr0,firewall=0" },
	} {
		q := p
		mut(&q)
		if q.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := (SmokeNetwork{}).Validate(); !errors.Is(err, ErrSmokeNetworkUnset) {
		t.Fatalf("unset network: %v", err)
	}
	dhcp := SmokeNetwork{Bridge: "vmbr0"}
	if dhcp.Validate() != nil || dhcp.ipconfig0() != "ip=dhcp" || dhcp.net0() != "virtio,bridge=vmbr0" {
		t.Fatal("dhcp network")
	}
	raw, _ := json.Marshal(p)
	if _, err := DecodeSmokePayload(append(raw[:len(raw)-1], []byte(`,"x":1}`)...)); err == nil {
		t.Fatal("unknown field accepted")
	}
}

// cloud-init without --format (22.x): the plain status line is used instead.
func TestSmokeCloudInitPlainFallback(t *testing.T) {
	x, pv, rec, p := smokeSetup(t, "os")
	x.PVE = &oldCloudInit{smokePVE: pv}
	if err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 1, budget: -1}); err != nil {
		t.Fatal(err)
	}
	if r, _ := rec.LoadSmoke(context.Background(), smokeJob); r.Result != SmokePass || r.Checks.CloudInitStatus != "done" {
		t.Fatalf("receipt %+v", r)
	}
	for in, want := range map[string]string{"\nstatus: done\n": "done", "status: running": "running", "status: error\n": "error"} {
		if got, err := parsePlainCloudInitStatus(in); err != nil || got != want {
			t.Errorf("%q → %q %v", in, got, err)
		}
	}
	if _, err := parsePlainCloudInitStatus("usage: cloud-init"); err == nil {
		t.Error("garbage accepted")
	}
}

type oldCloudInit struct{ *smokePVE }

func (o *oldCloudInit) GuestRun(ctx context.Context, node string, vmid int, d time.Duration, args ...string) (int, string, string, error) {
	if args[0] == "/usr/bin/cloud-init" {
		o.smokePVE.mu.Lock()
		o.smokePVE.mut(fmt.Sprintf("exec %d %s", vmid, args[0]))
		o.smokePVE.mu.Unlock()
		if len(args) > 2 {
			return 2, "", "usage: cloud-init status [-h] [-l] [-w]\ncloud-init status: error: unrecognized arguments: --format json", nil
		}
		return 0, "status: done\n", "", nil
	}
	return o.smokePVE.GuestRun(ctx, node, vmid, d, args...)
}

func TestParseCloudInitStatus(t *testing.T) {
	for in, want := range map[string]string{
		`{"status":"done"}`: "done", `{"status":"running","extended_status":"running"}`: "running",
		`{"status":"done","extended_status":"degraded done"}`: "degraded done", `{"status":"error"}`: "error",
	} {
		if got, err := parseCloudInitStatus(in); err != nil || got != want {
			t.Errorf("%s → %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "x", `{}`, strings.Repeat("a", 70<<10)} {
		if _, err := parseCloudInitStatus(in); err == nil {
			t.Errorf("%q accepted", in[:min(len(in), 10)])
		}
	}
}

// ── runner: smoke jobs ────────────────────────────────────────────────────────────────────────

func smokeRunnerFixture(t *testing.T) (*Runner, *fakeJobs, *smokePVE, *memSmokes) {
	t.Helper()
	x, pv, rec, p := smokeSetup(t, "os")
	raw, _ := json.Marshal(p)
	jobs := &fakeJobs{job: &durablejobs.Job{ID: smokeJob, Kind: durablejobs.TemplateUpdateSmoke, Payload: raw, Attempts: 1, MaxAttempts: 3, Epoch: 1, State: "running"}}
	r := &Runner{Store: &smokeJobs{fakeJobs: jobs}, Receipts: &memReceipts{m: map[string]Receipt{}}, Lease: 30 * time.Second, Smokes: rec,
		Build: func(context.Context, VMPayload) (*Executor, error) {
			t.Fatal("import executor built for a smoke job")
			return nil, nil
		},
		BuildSmoke: func(context.Context, SmokePayload) (*SmokeExecutor, error) { return x, nil }}
	return r, jobs, pv, rec
}

// smokeJobs lets the fake claim the smoke kind.
type smokeJobs struct{ *fakeJobs }

func (s *smokeJobs) ClaimKinds(_ context.Context, _ time.Duration, kinds []string) (*durablejobs.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed || !contains(kinds, s.job.Kind) {
		return nil, nil
	}
	s.claimed = true
	j := *s.job
	return &j, nil
}

func TestRunnerSmokePassCompletes(t *testing.T) {
	r, jobs, pv, rec := smokeRunnerFixture(t)
	if did, err := r.RunOnce(context.Background()); !did || err != nil {
		t.Fatalf("did %v err %v", did, err)
	}
	got, _ := rec.LoadSmoke(context.Background(), smokeJob)
	if !jobs.completed || got.Result != SmokePass || len(testVMsLeft(pv)) != 0 {
		t.Fatalf("completed=%v receipt=%+v", jobs.completed, got)
	}
}

// A check FAIL completes the job (the smoke ran); a permanent infrastructure refusal records FAIL,
// cleans up, dead-letters.
func TestRunnerSmokeFailures(t *testing.T) {
	r, jobs, pv, rec := smokeRunnerFixture(t)
	pv.ciStatus = "error"
	_, _ = r.RunOnce(context.Background())
	got, _ := rec.LoadSmoke(context.Background(), smokeJob)
	if !jobs.completed || got.Result != SmokeFail || got.Code != SmokeCloudInitFailed || len(testVMsLeft(pv)) != 0 {
		t.Fatalf("check fail: completed=%v %+v", jobs.completed, got)
	}
	r2, jobs2, pv2, rec2 := smokeRunnerFixture(t)
	pv2.execForbidden = true
	_, _ = r2.RunOnce(context.Background())
	got2, _ := rec2.LoadSmoke(context.Background(), smokeJob)
	if !jobs2.permanent || jobs2.failCode != SmokeExecForbidden || got2.Result != SmokeFail || !got2.Cleaned || len(testVMsLeft(pv2)) != 0 {
		t.Fatalf("permanent: jobs=%+v receipt=%+v left=%v", jobs2, got2, testVMsLeft(pv2))
	}
	protectedIntact(t, pv2)
	// Retryable errors on the last attempt: attempts_exhausted, FAIL recorded, test VM removed.
	r3, jobs3, pv3, rec3 := smokeRunnerFixture(t)
	jobs3.job.Attempts = 3
	r3.BuildSmoke = func(context.Context, SmokePayload) (*SmokeExecutor, error) {
		x, _, _, _ := smokeSetup(t, "os")
		x.PVE, x.Receipts = &flakyResize{smokePVE: pv3, fail: func() bool { return true }}, rec3
		return x, nil
	}
	_, _ = r3.RunOnce(context.Background())
	got3, _ := rec3.LoadSmoke(context.Background(), smokeJob)
	if !jobs3.permanent || got3.Code != SmokeAttemptsExhausted || got3.Result != SmokeFail || len(testVMsLeft(pv3)) != 0 {
		t.Fatalf("exhausted: jobs=%+v receipt=%+v", jobs3, got3)
	}
}

// A busy per-provider smoke lock defers the job without spending an attempt or touching PVE.
func TestRunnerSmokeLockBusyDefers(t *testing.T) {
	r, jobs, pv, _ := smokeRunnerFixture(t)
	var key string
	r.SmokeLock = func(_ context.Context, k string) (func(), bool, error) { key = k; return nil, false, nil }
	_, _ = r.RunOnce(context.Background())
	if !jobs.deferred || jobs.permanent || jobs.failCode != "smoke_lock_busy" || key != "templateupdate-smoke:svc-pve" || pv.mutations != 0 {
		t.Fatalf("jobs %+v key %q calls %v", jobs, key, pv.calls)
	}
}

// A lost lease mid-smoke: no completion, no dead-letter, no cleanup by the stale acquisition.
func TestRunnerSmokeLostLease(t *testing.T) {
	r, jobs, pv, _ := smokeRunnerFixture(t)
	pv.cloneHook = func(int) { jobs.mu.Lock(); jobs.leaseLost = true; jobs.mu.Unlock() }
	_, _ = r.RunOnce(context.Background())
	if jobs.completed || jobs.failCode != "" {
		t.Fatalf("a lost acquisition acknowledged the job: %+v", jobs)
	}
	if last := pv.calls[len(pv.calls)-1]; !strings.HasPrefix(last, "clone") {
		t.Fatalf("external work after the lease was lost: %v", pv.calls)
	}
}

// Dead smoke jobs: the dead-job pass removes the test VM and records FAIL job_dead.
func TestRunnerCleansDeadSmokeJob(t *testing.T) {
	r, jobs, pv, rec := smokeRunnerFixture(t)
	jobs.claimed = true
	p := smokePayloadFor("os")
	pv.vms[9301] = &smokeVM{node: "pve1", running: true, cfg: pve.VMConfig{"name": SmokeVMName(smokeJob), "pool": pve.TemplatePool, "tags": "proxcloud;pc-tu-smoke;" + SmokeOwnerTag(smokeJob)}}
	rec.m[smokeJob] = SmokeReceipt{ID: smokeJob, Payload: p, Step: SmokeBooting, VMID: 9301}
	r.DeadSmoke = func(context.Context) ([]string, error) { return []string{smokeJob}, nil }
	r.ClaimDead = func(context.Context, string, time.Duration) (*durablejobs.Job, error) {
		j := *jobs.job
		j.Epoch = 7
		return &j, nil
	}
	if did, err := r.RunOnce(context.Background()); !did || err != nil {
		t.Fatalf("did %v err %v", did, err)
	}
	got, _ := rec.LoadSmoke(context.Background(), smokeJob)
	if _, ok := pv.vms[9301]; ok || !got.Cleaned || got.Result != SmokeFail || got.Code != SmokeJobDead || jobs.failCode != "cleaned_up" {
		t.Fatalf("receipt %+v jobs %+v", got, jobs)
	}
	protectedIntact(t, pv)
}

// OnImported fires once an import job succeeded (the automatic smoke).
func TestRunnerOnImportedAfterSuccess(t *testing.T) {
	r, _, _, _ := runnerFixture(t)
	var got string
	r.OnImported = func(_ context.Context, id string) { got = id }
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != testJob {
		t.Fatalf("OnImported %q", got)
	}
	r2, _, pv2, _ := runnerFixture(t)
	pv2.importFail = true
	called := false
	r2.OnImported = func(context.Context, string) { called = true }
	_, _ = r2.RunOnce(context.Background())
	if called {
		t.Fatal("OnImported after a failed import")
	}
}

// S3: the sweep and cleanup never take a manual clone of a test VM (another name, copied owner tag),
// nor a same-named VM configured by another smoke job, nor anything outside pc-templates.
func TestSmokeOwnershipNeedsExactNameAndTag(t *testing.T) {
	x, pv, rec, p := smokeSetup(t, "os")
	own := "proxcloud;pc-tu-smoke;" + SmokeOwnerTag(smokeJob)
	pv.vms[9301] = &smokeVM{node: "pve1", running: true, cfg: pve.VMConfig{"name": SmokeVMName(smokeJob), "pool": pve.TemplatePool, "tags": own}}                                   // ours
	pv.vms[9302] = &smokeVM{node: "pve1", running: true, cfg: pve.VMConfig{"name": "copy-of-smoke", "pool": pve.TemplatePool, "tags": own}}                                         // manual clone
	pv.vms[9303] = &smokeVM{node: "pve1", cfg: pve.VMConfig{"name": SmokeVMName(smokeJob), "pool": pve.TemplatePool, "tags": "proxcloud;pc-tu-smoke;pc-tu-smoke-0000aaaa0000aaaa"}} // other job's tag
	r := &SmokeReceipt{ID: smokeJob, Payload: p, Step: SmokeBooting, VMID: 9301}
	rec.m[smokeJob] = *r
	if warns := x.Cleanup(context.Background(), &testFence{epoch: 2, budget: -1}, smokeJob, p, r); len(warns) != 0 {
		t.Fatalf("warnings %v", warns)
	}
	if _, ok := pv.vms[9301]; ok {
		t.Fatal("own test VM survived")
	}
	for _, id := range []int{9302, 9303} {
		if _, ok := pv.vms[id]; !ok {
			t.Fatalf("VM %d (not this job's) was destroyed: %v", id, pv.calls)
		}
		for _, c := range pv.calls {
			if strings.HasSuffix(c, fmt.Sprintf(" %d", id)) {
				t.Fatalf("VM %d mutated: %v", id, pv.calls)
			}
		}
	}
	protectedIntact(t, pv)
}

// A leftover test VM that cannot be removed keeps the receipt uncleaned and the attempt retried.
func TestSmokeLeftoverKeepsReceiptUncleaned(t *testing.T) {
	x, pv, rec, p := smokeSetup(t, "os")
	pv.deleteFails = true
	err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 1, budget: -1})
	if err == nil || errors.As(err, new(*Permanent)) {
		t.Fatalf("want a retryable error, got %v", err)
	}
	r, _ := rec.LoadSmoke(context.Background(), smokeJob)
	if r.Cleaned || r.Result != SmokePass || r.Step == SmokeFinished {
		t.Fatalf("receipt %+v", r)
	}
	pv.deleteFails = false
	if err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 2, budget: -1}); err != nil {
		t.Fatal(err)
	}
	if r, _ = rec.LoadSmoke(context.Background(), smokeJob); !r.Cleaned || r.Result != SmokePass || len(testVMsLeft(pv)) != 0 {
		t.Fatalf("after retry %+v left %v", r, testVMsLeft(pv))
	}
}

// S6: the PASS records the template's config digest at clone time.
func TestSmokeRecordsTemplateConfigDigest(t *testing.T) {
	x, _, rec, p := smokeSetup(t, "os")
	if err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 1, budget: -1}); err != nil {
		t.Fatal(err)
	}
	if r, _ := rec.LoadSmoke(context.Background(), smokeJob); r.TemplateConfigDigest != "d" {
		t.Fatalf("template config digest %q", r.TemplateConfigDigest)
	}
}

// S5: IPv4 only, no zones, gateway inside and distinct; cicustom removed from the test VM.
func TestSmokeNetworkValidationS5(t *testing.T) {
	for name, n := range map[string]SmokeNetwork{
		"ipv6":           {Bridge: "vmbr0", IP: "2001:db8::5/64", Gateway: "2001:db8::1"},
		"v4-mapped":      {Bridge: "vmbr0", IP: "::ffff:192.0.2.50/120", Gateway: "192.0.2.1"},
		"network addr":   {Bridge: "vmbr0", IP: "192.0.2.0/24", Gateway: "192.0.2.1"},
		"gw is ip":       {Bridge: "vmbr0", IP: "192.0.2.50/24", Gateway: "192.0.2.50"},
		"gw zone":        {Bridge: "vmbr0", IP: "192.0.2.50/24", Gateway: "fe80::1%eth0"},
		"ns zone":        {Bridge: "vmbr0", Nameserver: "fe80::1%vmbr0"},
		"ns garbage":     {Bridge: "vmbr0", Nameserver: "dns.example"},
		"/32":            {Bridge: "vmbr0", IP: "192.0.2.50/32", Gateway: "192.0.2.1"},
		"vlan negative":  {Bridge: "vmbr0", VLAN: -1},
		"bridge options": {Bridge: "vmbr0,tag=5"},
	} {
		if n.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, n := range map[string]SmokeNetwork{
		"dhcp":      {Bridge: "vmbr0"},
		"static":    {Bridge: "vmbr0", VLAN: 29, IP: "192.0.2.50/24", Gateway: "192.0.2.1", Nameserver: "192.0.2.53"},
		"ns ipv6":   {Bridge: "vmbr0", Nameserver: "2001:db8::53"},
		"vnet name": {Bridge: "smokenet"},
	} {
		if err := n.Validate(); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	x, pv, _, p := smokeSetup(t, "os")
	pv.vms[9300].cfg["cicustom"] = "user=local:snippets/x.yaml"
	var seen pve.VMConfig
	pv.stopHook = func() {
		pv.mu.Lock()
		defer pv.mu.Unlock()
		for _, vm := range pv.vms {
			if strings.Contains(vm.cfg.Str("tags"), "pc-tu-smoke;") {
				seen = vm.cfg
			}
		}
	}
	if err := x.Run(context.Background(), smokeJob, p, &testFence{epoch: 1, budget: -1}); err != nil {
		t.Fatal(err)
	}
	if seen == nil || seen.Str("cicustom") != "" {
		t.Fatalf("cicustom kept on the test VM: %+v", seen)
	}
}

type fakeBridges struct {
	m   map[string]string
	err error
}

func (f fakeBridges) ManagementBridges(context.Context, ...string) (map[string]string, error) {
	return f.m, f.err
}

func TestCheckSmokeNetworkManagementBridge(t *testing.T) {
	mgmt := fakeBridges{m: map[string]string{"vmbr0": "pve1"}}
	var le *LifecycleError
	if err := CheckSmokeNetwork(context.Background(), mgmt, "pve1", SmokeNetwork{Bridge: "vmbr0"}); !errors.As(err, &le) || le.Code != "smoke_network_management_bridge" {
		t.Fatalf("untagged mgmt bridge: %v", err)
	}
	if err := CheckSmokeNetwork(context.Background(), mgmt, "pve1", SmokeNetwork{Bridge: "vmbr0", VLAN: 29}); err != nil {
		t.Fatalf("tagged on mgmt bridge: %v", err)
	}
	if err := CheckSmokeNetwork(context.Background(), mgmt, "pve1", SmokeNetwork{Bridge: "vmbr9"}); err != nil {
		t.Fatalf("dedicated bridge: %v", err)
	}
	if err := CheckSmokeNetwork(context.Background(), fakeBridges{err: errors.New("x")}, "pve1", SmokeNetwork{Bridge: "vmbr9"}); !errors.As(err, &le) || le.Code != "smoke_network_unverified" {
		t.Fatalf("unreadable: %v", err)
	}
}

// PVE adds a cloned VM to its pool only when the clone ends, so while its clone task runs the job's own test VM is
// invisible to the token (403, outside pc-templates). Cleanup must not take it for gone and mark the receipt cleaned:
// it waits for the clone task, and once the clone has joined the pool it removes the VM.
func TestSmokeCleanupWaitsForItsRunningClone(t *testing.T) {
	x, pv, rec, p := smokeSetup(t, "os")
	tags := TemplateTags(p.importPayload())
	pv.vms[9301] = &smokeVM{node: "pve1", cfg: pve.VMConfig{"name": SmokeVMName(smokeJob), "tags": tags, "lock": "clone"}}
	pv.cloneRunning = true
	r := &SmokeReceipt{ID: smokeJob, Payload: p, Step: SmokeCloning, VMID: 9301}
	rec.m[smokeJob] = *r
	if warns := x.Cleanup(context.Background(), &testFence{epoch: 3, budget: -1}, smokeJob, p, r); len(warns) == 0 {
		t.Fatal("a running clone of this job was taken for gone")
	}
	if _, ok := pv.vms[9301]; !ok {
		t.Fatal("the in-flight clone was touched")
	}
	// The clone ends and joins the pool: the next pass removes it.
	pv.cloneRunning = false
	pv.vms[9301].cfg["pool"] = pve.TemplatePool
	delete(pv.vms[9301].cfg, "lock")
	if warns := x.Cleanup(context.Background(), &testFence{epoch: 4, budget: -1}, smokeJob, p, r); len(warns) != 0 {
		t.Fatalf("warnings %v", warns)
	}
	if _, ok := pv.vms[9301]; ok {
		t.Fatal("the finished clone of this job survived")
	}
	// Without a running clone, an invisible VMID is gone: no warning, nothing touched.
	r2 := &SmokeReceipt{ID: smokeJob, Payload: p, Step: SmokeCloning, VMID: 9399}
	if warns := x.Cleanup(context.Background(), &testFence{epoch: 5, budget: -1}, smokeJob, p, r2); len(warns) != 0 {
		t.Fatalf("a gone VMID warned: %v", warns)
	}
	protectedIntact(t, pv)
}

// A VM in pc-templates that still answers 403, or a pool that cannot be read, is a real refusal: owned() keeps the
// error (fail closed) instead of calling the VM gone.
func TestSmokeOwnedKeepsRealRefusals(t *testing.T) {
	x, pv, _, p := smokeSetup(t, "os")
	pv.vms[9301] = &smokeVM{node: "pve1", cfg: pve.VMConfig{"name": SmokeVMName(smokeJob), "pool": "elsewhere"}}
	pv.poolMembersExtra = []pve.PoolMember{{Type: "qemu", VMID: 9301, Node: "pve1"}}
	if _, _, _, err := x.owned(context.Background(), smokeJob, p, 9301); !pve.IsForbidden(err) {
		t.Fatalf("a pool member answering 403 was not an error: %v", err)
	}
	pv.poolMembersExtra, pv.poolErr = nil, errors.New("pool read failed")
	if _, _, _, err := x.owned(context.Background(), smokeJob, p, 9301); err == nil {
		t.Fatal("a 403 with an unreadable pool was taken for gone")
	}
}

// The clone can end between owned()'s two reads (clone task, then pool): the VM has then joined the pool, so the first
// pass keeps the 403 and retries instead of calling it gone; the next pass removes it.
func TestSmokeCloneEndingBetweenTheChecksIsNotGone(t *testing.T) {
	x, pv, rec, p := smokeSetup(t, "os")
	tags := TemplateTags(p.importPayload())
	pv.vms[9301] = &smokeVM{node: "pve1", cfg: pve.VMConfig{"name": SmokeVMName(smokeJob), "tags": tags}}
	pv.cloneRunning, pv.cloneEndsNow = true, 9301
	r := &SmokeReceipt{ID: smokeJob, Payload: p, Step: SmokeCloning, VMID: 9301}
	rec.m[smokeJob] = *r
	if warns := x.Cleanup(context.Background(), &testFence{epoch: 3, budget: -1}, smokeJob, p, r); len(warns) == 0 {
		t.Fatal("a clone that ended between the checks was taken for gone")
	}
	if warns := x.Cleanup(context.Background(), &testFence{epoch: 4, budget: -1}, smokeJob, p, r); len(warns) != 0 {
		t.Fatalf("warnings %v", warns)
	}
	if _, ok := pv.vms[9301]; ok {
		t.Fatal("the clone of this job survived")
	}
}
