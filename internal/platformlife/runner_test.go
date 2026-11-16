package platformlife

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
)

const inID = "pi-0a1b2c3d"

// only marks every stage of the run done but id, so the run succeeds once id is done.
func (e *env) only(id string) {
	e.upTo(id)
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		for i := range run.Stages {
			if run.Stages[i].ID != id {
				run.Stages[i].State = StageDone
			}
		}
	})
}

func TestDecodePayloadRefusesUnknownFields(t *testing.T) {
	if _, err := DecodePayload([]byte(`{"version":1,"runId":"pr-1","installationId":"pi-1"}`)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"version":1,"runId":"pr-1","installationId":"pi-1","kind":"apply"}`, `{"version":2,"runId":"pr-1","installationId":"pi-1"}`,
		`{"version":1,"runId":"pr-1"}`, `not json`} {
		if _, err := DecodePayload([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	e := newEnv(t, RunApply)
	e.job.Payload = []byte(`{"version":1,"runId":"pr-1","installationId":"` + inID + `","extra":true}`)
	if end := e.once(); end != "fail:invalid_payload" {
		t.Fatalf("a job with an unknown payload field: %s", end)
	}
	if len(e.pve.writes) != 0 {
		t.Fatalf("PVE written: %v", e.pve.writes)
	}
}

func TestPlatformRunIsAnExternalKindWithoutCleanup(t *testing.T) {
	if !durablejobs.External(durablejobs.PlatformRun) || slices.Contains(durablejobs.ClaimDeadKinds, durablejobs.PlatformRun) {
		t.Fatal("platform.run/v1 must be external and not claimed dead")
	}
}

// Stage 3 from nothing: one full clone of the seed template into proxcloud-seed named for generation 1; the template's
// tags replaced by the installation's, net0 on the platform bridge with the firewall and its MAC kept, protection on,
// the disk grown, the CD uploaded and put in the cloud-init slot, the VM firewall written, the seed started.
func TestSeedVMBuildsTheSeed(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	if end := e.until(5); end != "complete" {
		t.Fatalf("end %s; logs %s", end, e.logs)
	}
	if s := e.stage(StageSeedVM); s.State != StageDone || s.Receipt == nil || s.Receipt.VMID != 120 || s.Receipt.Generation != 1 ||
		!strings.HasPrefix(s.Receipt.CloneUPID, "UPID:pve1:") || s.Receipt.CDVolume != "local:iso/pcplat-"+inID+"-g1.iso" {
		t.Fatalf("stage: %+v %+v", s, s.Receipt)
	}
	if got := e.pve.pooled(); !slices.Equal(got, []int{120}) || e.pve.writeCount("POST /nodes/pve1/qemu/9100/clone") != 1 {
		t.Fatalf("pool %v, writes %v", got, e.pve.writes)
	}
	vm := e.pve.vms[120]
	cfg := func(k string) string { s, _ := vm.cfg[k].(string); return s }
	if cfg("name") != "pc-seed-"+inID+"-g1" || cfg("tags") != "proxcloud;pc-platform;pc-platform-"+inID || cfg("protection") != "1" ||
		cfg("agent") != "1" || cfg("onboot") != "1" || cfg("cores") != "2" || cfg("memory") != "4096" ||
		cfg("net0") != "virtio=BC:24:11:00:00:01,bridge=vmbr1,firewall=1" || !strings.HasSuffix(cfg("scsi0"), "size=30G") ||
		cfg("ide2") != "local:iso/pcplat-"+inID+"-g1.iso,media=cdrom" || !vm.running {
		t.Fatalf("VM config %v running %v", vm.cfg, vm.running)
	}
	if !slices.Equal(e.pve.iso, []string{"pcplat-" + inID + "-g1.iso"}) || len(e.pve.uploads) != 1 {
		t.Fatalf("ISO store %v", e.pve.iso)
	}
	// The template has no NIC: the CD's network config names the MAC address Proxmox gave net0.
	if !bytes.Contains(e.pve.uploads[0], []byte(`"mac_address":"bc:24:11:00:00:01"`)) {
		t.Fatal("the CD does not name the MAC address of the VM's net0")
	}
	var rules []string
	for _, r := range e.pve.rules {
		rules = append(rules, r.Type+" "+r.Action+" "+r.Proto+" "+r.Source+" "+r.DPort)
	}
	if !slices.Equal(rules, []string{"in ACCEPT tcp 192.168.33.0/24 6443", "in ACCEPT tcp 192.168.33.0/24 22"}) {
		t.Fatalf("firewall rules %v", rules)
	}
	_, in, sec := e.state.snapshot()
	if st := in.Status.Seed; st == nil || st.VMID != 120 || st.Generation != 1 || st.APIServer != "https://10.200.200.10:6443" ||
		!strings.Contains(st.CACert, "BEGIN CERTIFICATE") || strings.Contains(st.CACert, "PRIVATE") || st.CDVolume == "" ||
		sec.K3sToken == "" || !bootstrapTokenRE.MatchString(sec.BootstrapToken) {
		t.Fatalf("seed status %+v", in.Status.Seed)
	}
	if run, _, _ := e.state.snapshot(); in.Status.Phase != PhaseReady || run.State != RunSucceeded {
		t.Fatalf("phase %s, run %s %s %q", in.Status.Phase, run.State, run.Code, run.Message)
	}
}

// A lost lease at any checkpoint stops every Proxmox call at once; the next acquisition resumes, and the end is one
// seed VM with one CD of its generation, never a second clone left behind.
func TestSeedVMLeaseLossAfterEachIntent(t *testing.T) {
	clean := newEnv(t, RunApply)
	clean.only(StageSeedVM)
	clean.until(5)
	total := clean.jobs.checkpoints
	for n := 1; n <= total; n++ {
		e := newEnv(t, RunApply)
		e.only(StageSeedVM)
		e.jobs.loseAt = n
		var atLoss int
		e.jobs.onLose = func() { atLoss = len(e.pve.writes) }
		if end := e.once(); end != "" {
			t.Fatalf("lost at %d: the job was given back (%s)", n, end)
		}
		if len(e.pve.writes) != atLoss {
			t.Fatalf("lost at checkpoint %d: Proxmox written after the loss: %v", n, e.pve.writes[atLoss:])
		}
		e.jobs = &fakeJobs{}
		e.r.Jobs = e.jobs
		if end := e.until(8); end != "complete" {
			t.Fatalf("lost at %d, resumed: %s", n, end)
		}
		_, in, _ := e.state.snapshot()
		st := in.Status.Seed
		if pool := e.pve.pooled(); !slices.Equal(pool, []int{st.VMID}) || len(e.pve.vms) != 1 {
			t.Fatalf("lost at %d: pool %v, VMs %d, seed %+v", n, pool, len(e.pve.vms), st)
		}
		if name := e.pve.vms[st.VMID].cfg["name"]; name != SeedVMName(inID, st.Generation) || !e.pve.vms[st.VMID].running ||
			!slices.Equal(e.pve.iso, []string{SeedCDName(inID, st.Generation)}) {
			t.Fatalf("lost at %d: VM %v, ISO %v, generation %d", n, name, e.pve.iso, st.Generation)
		}
		// Wherever the run stopped, the CD in the store names the MAC address the VM has.
		net0 := fmt.Sprint(e.pve.vms[st.VMID].cfg["net0"])
		mac := strings.ToLower(net0[len("virtio="):][:17])
		if !bytes.Contains(e.pve.uploads[len(e.pve.uploads)-1], []byte(`"mac_address":"`+mac+`"`)) || e.pve.vms[st.VMID].cfg["protection"] != "1" {
			t.Fatalf("lost at %d: the CD does not name %s of net0 %q, or the VM is not protected", n, mac, net0)
		}
	}
}

// A CD that cannot be rendered fails the stage before anything is stored: no CA, no token, no CD volume, no upload.
// The failed run is logged as an error with its stage and code.
func TestSeedVMRenderFailureStoresNothing(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.pve.noMAC = true
	if end := e.until(5); end != "fail:"+CodeSeedConfigRefused {
		t.Fatalf("end %s", end)
	}
	_, in, sec := e.state.snapshot()
	if st := in.Status.Seed; st.CACert != "" || st.CDVolume != "" || sec.K3sToken != "" || sec.BootstrapToken != "" || len(e.pve.uploads) != 0 {
		t.Fatalf("stored before the render: seed %+v, uploads %d", st, len(e.pve.uploads))
	}
	if log := e.logs.String(); !strings.Contains(log, `level=ERROR msg="platform run failed" run=pr-1 stage=seed-vm code=seed_config_refused`) {
		t.Fatalf("the failed run is not logged as an error: %s", log)
	}
}

// bootDisk reads the sizes Proxmox writes: a whole number of T, G, M or K, or bytes.
func TestBootDiskSize(t *testing.T) {
	for val, want := range map[string]int{"local-zfs:vm-102-disk-0,discard=on,size=20G": 20, "s:d,size=3584M": 3, "s:d,size=1T": 1024,
		"s:d,size=2361393152": 2, "s:d": 0} {
		if disk, got := bootDisk(pve.VMConfig{"ide2": "s:vm-1-cloudinit,media=cdrom", "scsi0": val}); disk != "scsi0" || got != want {
			t.Errorf("%s: %s %d, want scsi0 %d", val, disk, got, want)
		}
	}
}

// The VMID of the receipt is reused when the VM there is ours: no new clone, no new VMID.
func TestSeedVMReusesItsVMID(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.pve.vms[120] = &fakeVM{pooled: true, cfg: map[string]any{"name": SeedVMName(inID, 1), "tags": "pc-template", "net0": "virtio=AA:00:00:00:00:01,bridge=vmbr0",
		"scsi0": "local-lvm:vm-120-disk-0,size=6G", "ide2": "local-lvm:vm-120-cloudinit,media=cdrom"}}
	e.mutate(func(run *Run, in *Installation, _ *Secrets) {
		run.Stages[2].Receipt = &StageReceipt{VMID: 120, Name: SeedVMName(inID, 1), Generation: 1}
		in.Status.Seed = &SeedStatus{VMID: 120, Node: "pve1", Generation: 1}
	})
	e.until(5)
	if e.pve.writeCount("POST /nodes/pve1/qemu/9100/clone") != 0 || !slices.Equal(e.pve.pooled(), []int{120}) || !e.pve.vms[120].running {
		t.Fatalf("writes %v", e.pve.writes)
	}
}

// A VM at the receipt's VMID that fails the rule (another name, in or out of the pool) is foreign: a new intent and a
// new VMID, and nothing is written to it.
func TestSeedVMLeavesAForeignVMID(t *testing.T) {
	for name, vm := range map[string]*fakeVM{
		"other name in the pool": {pooled: true, cfg: map[string]any{"name": "pc-seed-pi-ffffffff-g1"}},
		"outside the pool":       {cfg: map[string]any{"name": SeedVMName(inID, 1)}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, RunApply)
			e.only(StageSeedVM)
			e.pve.vms[120] = vm
			e.mutate(func(run *Run, in *Installation, _ *Secrets) {
				run.Stages[2].Receipt = &StageReceipt{VMID: 120, Name: SeedVMName(inID, 1), Generation: 1}
				in.Status.Seed = &SeedStatus{VMID: 120, Node: "pve1", Generation: 1}
			})
			e.until(5)
			for _, w := range e.pve.writes {
				if strings.Contains(w, "/qemu/120") {
					t.Fatalf("foreign VM written: %s", w)
				}
			}
			if s := e.stage(StageSeedVM); s.Receipt.VMID != 121 || !e.pve.vms[121].running {
				t.Fatalf("receipt %+v", s.Receipt)
			}
		})
	}
}

// A held VMID outside the pool while a clone of the seed template runs is a clone that has not joined the pool yet:
// the stage waits instead of taking a new VMID.
func TestSeedVMWaitsForACloneInFlight(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.pve.vms[120] = &fakeVM{cfg: map[string]any{"name": SeedVMName(inID, 1)}}
	e.pve.cloneBusy = true
	e.mutate(func(run *Run, in *Installation, _ *Secrets) {
		run.Stages[2].Receipt = &StageReceipt{VMID: 120, Name: SeedVMName(inID, 1), Generation: 1}
		in.Status.Seed = &SeedStatus{VMID: 120, Node: "pve1", Generation: 1}
	})
	if end := e.once(); end != "defer:waiting_seed_vm" || e.stage(StageSeedVM).Receipt.VMID != 120 {
		t.Fatalf("end %s, receipt %+v", end, e.stage(StageSeedVM).Receipt)
	}
}

// A clone that ends, and joins the pool, between the pool read and the task read is ours, not a foreign VM: no second
// clone, no new VMID.
func TestSeedVMCloneJoinsThePoolBetweenTheReads(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.pve.vms[120] = &fakeVM{pooled: true, cfg: map[string]any{"name": SeedVMName(inID, 1), "tags": "pc-template", "net0": "virtio=AA:00:00:00:00:01,bridge=vmbr0",
		"scsi0": "local-lvm:vm-120-disk-0,size=6G", "ide2": "local-lvm:vm-120-cloudinit,media=cdrom"}}
	e.pve.poolLag = 1
	e.mutate(func(run *Run, in *Installation, _ *Secrets) {
		run.Stages[2].Receipt = &StageReceipt{VMID: 120, Name: SeedVMName(inID, 1), Generation: 1}
		in.Status.Seed = &SeedStatus{VMID: 120, Node: "pve1", Generation: 1}
	})
	e.until(5)
	if s := e.stage(StageSeedVM); e.pve.writeCount("POST /nodes/pve1/qemu/9100/clone") != 0 || s.Receipt.VMID != 120 || len(e.pve.vms) != 1 || !e.pve.vms[120].running {
		t.Fatalf("receipt %+v, writes %v", s.Receipt, e.pve.writes)
	}
}

// A generation whose CA exists never gets a second clone: its CD names the first clone's MAC, and a clone is configured
// where its CD is made. A VMID turned foreign or a VM gone after the upload starts the next generation with one clone,
// configured and behind its firewall; so does a VM deleted for a lost CD when the run stopped before the new intent.
func TestSeedVMNeverReclonesAGenerationWithItsCA(t *testing.T) {
	for name, tc := range map[string]struct {
		vm   *fakeVM
		cd   bool
		vmid int
	}{
		"foreign with the CD": {&fakeVM{pooled: true, running: true, cfg: map[string]any{"name": "renamed-by-an-operator"}}, true, 121},
		"absent with the CD":  {nil, true, 120},
		"absent, CD lost":     {nil, false, 120},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, RunApply)
			e.only(StageSeedVM)
			if tc.vm != nil {
				e.pve.vms[120] = tc.vm
			}
			if tc.cd {
				e.pve.iso = []string{SeedCDName(inID, 1)}
			}
			e.mutate(func(run *Run, in *Installation, sec *Secrets) {
				run.Stages[2].Receipt = &StageReceipt{VMID: 120, Name: SeedVMName(inID, 1), Generation: 1}
				in.Status.Seed = &SeedStatus{VMID: 120, Node: "pve1", Generation: 1, CACert: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n",
					CDVolume: "local:iso/" + SeedCDName(inID, 1)}
				sec.K3sToken, sec.BootstrapToken = runK3s, runBoot
			})
			if end := e.until(5); end != "complete" {
				t.Fatalf("end %s", end)
			}
			_, in, _ := e.state.snapshot()
			st := in.Status.Seed
			if st.Generation != 2 || st.VMID != tc.vmid || e.pve.writeCount("POST /nodes/pve1/qemu/9100/clone") != 1 || len(e.pve.uploads) != 1 ||
				!slices.Equal(e.pve.iso, []string{SeedCDName(inID, 2)}) || st.CDVolume != "local:iso/"+SeedCDName(inID, 2) {
				t.Fatalf("seed %+v, ISO %v, writes %v", st, e.pve.iso, e.pve.writes)
			}
			cfg := e.pve.vms[st.VMID].cfg
			if cfg["name"] != SeedVMName(inID, 2) || cfg["net0"] != "virtio=BC:24:11:00:00:01,bridge=vmbr1,firewall=1" || cfg["protection"] != "1" ||
				cfg["tags"] != "proxcloud;pc-platform;pc-platform-"+inID || !strings.HasSuffix(fmt.Sprint(cfg["scsi0"]), "size=30G") {
				t.Fatalf("the clone was not configured: %v", cfg)
			}
			for _, w := range e.pve.writes {
				if tc.vm != nil && strings.Contains(w, "/qemu/120") {
					t.Fatalf("foreign VM written: %s", w)
				}
			}
		})
	}
}

// Generation rules: secrets committed but no CD on the store means the CA key is gone with its process, so the VM is
// deleted (protection off first) and generation 2 is built on a new VMID.
func TestSeedVMNewGenerationWhenTheCDIsLost(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.pve.vms[120] = &fakeVM{pooled: true, cfg: map[string]any{"name": SeedVMName(inID, 1), "protection": "1", "net0": "virtio=AA:00:00:00:00:01,bridge=vmbr1"}}
	e.mutate(func(run *Run, in *Installation, sec *Secrets) {
		run.Stages[2].Receipt = &StageReceipt{VMID: 120, Name: SeedVMName(inID, 1), Generation: 1}
		in.Status.Seed = &SeedStatus{VMID: 120, Node: "pve1", Generation: 1, CACert: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n"}
		sec.K3sToken, sec.BootstrapToken = runK3s, runBoot
	})
	if end := e.until(5); end != "complete" {
		t.Fatalf("end %s", end)
	}
	_, in, _ := e.state.snapshot()
	if st := in.Status.Seed; st.Generation != 2 || len(e.pve.vms) != 1 || e.pve.vms[st.VMID].cfg["name"] != SeedVMName(inID, 2) ||
		!slices.Equal(e.pve.iso, []string{SeedCDName(inID, 2)}) || e.pve.writeCount("DELETE /nodes/pve1/qemu/120") != 1 {
		t.Fatalf("seed %+v, VMs %v, ISO %v", in.Status.Seed, e.pve.pooled(), e.pve.iso)
	}
	if i, j := slices.Index(e.pve.writes, "POST /nodes/pve1/qemu/120/config"), slices.Index(e.pve.writes, "DELETE /nodes/pve1/qemu/120"); i < 0 || j < i {
		t.Fatalf("protection not taken off before the delete: %v", e.pve.writes)
	}
}

// Retry: a seed an earlier run built that never became ready (no service-account token stored) is deleted and the next
// generation cloned; one that became ready is never deleted, and if it is gone the run fails seed_lost.
func TestSeedRecreatedOnlyBeforeReady(t *testing.T) {
	t.Run("not ready", func(t *testing.T) {
		e := newEnv(t, RunApply)
		e.only(StageSeedVM)
		e.seedAt(120, 1, nil, true)
		if end := e.until(5); end != "complete" {
			t.Fatalf("end %s", end)
		}
		_, in, _ := e.state.snapshot()
		if st := in.Status.Seed; st.Generation != 2 || len(e.pve.vms) != 1 || e.pve.vms[st.VMID].cfg["name"] != SeedVMName(inID, 2) ||
			!slices.Equal(e.pve.iso, []string{SeedCDName(inID, 2)}) || e.pve.writeCount("POST /nodes/pve1/qemu/120/status/stop") != 1 ||
			e.pve.writeCount("DELETE /nodes/pve1/qemu/120") != 1 {
			t.Fatalf("seed %+v, VMs %v, ISO %v, writes %v", in.Status.Seed, e.pve.pooled(), e.pve.iso, e.pve.writes)
		}
	})
	t.Run("ready", func(t *testing.T) {
		e := newEnv(t, RunApply)
		e.only(StageSeedVM)
		e.seedAt(120, 1, nil, false)
		e.mutate(func(_ *Run, _ *Installation, sec *Secrets) {
			sec.SeedKubeconfig = &SeedKubeconfig{Server: "https://x", CA: "c", Token: runSA}
		})
		e.until(5)
		if e.pve.writeCount("DELETE") != 0 || e.pve.writeCount("POST /nodes/pve1/qemu/9100/clone") != 0 || !e.pve.vms[120].running || len(e.pve.rules) != 2 {
			t.Fatalf("a ready seed was touched: %v", e.pve.writes)
		}
		delete(e.pve.vms, 120)
		e.mutate(func(run *Run, _ *Installation, _ *Secrets) {
			run.State, run.Stages[2] = RunRunning, Stage{ID: StageSeedVM, State: StagePending}
		})
		if end := e.once(); end != "fail:"+CodeSeedLost || e.pve.writeCount("POST /nodes/pve1/qemu/9100/clone") != 0 {
			t.Fatalf("ready seed gone: %s, writes %v", end, e.pve.writes)
		}
	})
}

// Waits and transient errors give the job back with Defer (no attempt spent); transient ones count in the stage's
// retries, and the 41st fails the run.
func TestDeferKeepsAttempts(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.pve.failOnce["POST /nodes/pve1/qemu/9100/clone"] = `{"errors":{"lock":"can't lock file '/var/lock/qemu-server/lock-9100.conf' - got timeout"}}`
	if end := e.once(); end != "defer:retry_seed_vm" {
		t.Fatalf("a clone lock: %s", end)
	}
	if s := e.stage(StageSeedVM); s.Retries != 1 || s.State != StageWaiting || s.Code != CodeSeedCloneFailed {
		t.Fatalf("stage %+v", s)
	}
	if run, _, _ := e.state.snapshot(); run.State != RunWaiting {
		t.Fatalf("run %s", run.State)
	}
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.Stages[2].Retries = stageRetries })
	e.pve.failOnce["POST /nodes/pve1/qemu/9100/clone"] = `{"errors":{"lock":"can't lock file"}}`
	if end := e.once(); end != "fail:"+CodeSeedCloneFailed {
		t.Fatalf("budget spent: %s", end)
	}
	if strings.Contains(strings.Join(e.jobs.ends, ","), "retry:") {
		t.Fatalf("an attempt was spent: %v", e.jobs.ends)
	}
}

// A clone task that failed ends the stage with seed_clone_failed and the last lines of its log.
func TestSeedCloneFailedCarriesTheTaskLog(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.pve.vms[120] = &fakeVM{pooled: true, cfg: map[string]any{"name": SeedVMName(inID, 1)}}
	up := e.pve.upid("qmclone", 9100)
	e.pve.tasks[up] = "clone failed: no space left"
	e.mutate(func(run *Run, in *Installation, _ *Secrets) {
		run.Stages[2].Receipt = &StageReceipt{VMID: 120, Name: SeedVMName(inID, 1), Generation: 1, CloneUPID: up}
		in.Status.Seed = &SeedStatus{VMID: 120, Node: "pve1", Generation: 1}
	})
	if end := e.once(); end != "fail:"+CodeSeedCloneFailed {
		t.Fatalf("end %s", end)
	}
	if s := e.stage(StageSeedVM); s.State != StageFailed || !strings.Contains(s.Message, "no space left") {
		t.Fatalf("stage %+v", s)
	}
}

// Cancel is honoured at wake-up: the run ends cancelled and the installation failed, with no Proxmox call.
func TestCancelAtWakeUp(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.CancelRequested = true })
	if end := e.once(); end != "complete" || len(e.pve.writes) != 0 {
		t.Fatalf("end %s, writes %v", end, e.pve.writes)
	}
	run, in, _ := e.state.snapshot()
	if run.State != RunCancelled || in.Status.Phase != PhaseFailed || run.FinishedAt == nil {
		t.Fatalf("run %s, phase %s", run.State, in.Status.Phase)
	}
}

// Cancel is honoured between two external calls of a stage: the result of the call in flight is still recorded, no
// other Proxmox call follows, and the run ends cancelled with the installation failed.
func TestCancelBetweenExternalCalls(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.pve.onWrite = func(w string) {
		if w == "POST /nodes/pve1/qemu/9100/clone" {
			e.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.CancelRequested = true })
		}
	}
	if end := e.once(); end != "complete" || !slices.Equal(e.pve.writes, []string{"POST /nodes/pve1/qemu/9100/clone"}) {
		t.Fatalf("end %s, writes %v", end, e.pve.writes)
	}
	run, in, _ := e.state.snapshot()
	if s := e.stage(StageSeedVM); run.State != RunCancelled || in.Status.Phase != PhaseFailed || !strings.HasPrefix(s.Receipt.CloneUPID, "UPID:pve1:") {
		t.Fatalf("run %s, phase %s, receipt %+v", run.State, in.Status.Phase, s.Receipt)
	}
}

// A lease no renewal reaches in time is lost, whatever the renewal failed with: the work stops and the job is not
// given back by this acquisition.
func TestLeaseThatCannotBeRenewedStopsTheRun(t *testing.T) {
	e := newEnv(t, RunApply)
	e.r.Lease = 60 * time.Millisecond
	e.jobs.renewErr = errors.New("database unreachable")
	e.r.Preflight = func(ctx context.Context, _ *Installation) ([]Check, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
			return []Check{{ID: "flags", Status: CheckFail}}, nil
		}
	}
	start := time.Now()
	if end := e.once(); end != "" || time.Since(start) > 2*time.Second {
		t.Fatalf("end %q after %s", end, time.Since(start))
	}
}

// A passed absolute deadline fails the stage with the code of its last observation (or <stage>_timeout).
func TestDeadline(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedAPI)
	past, started := time.Now().Add(-time.Minute), time.Now().Add(-21*time.Minute)
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		run.Stages[3] = Stage{ID: StageSeedAPI, State: StageWaiting, StartedAt: &started, Deadline: &past, Code: CodeSeedNotBooted}
	})
	if end := e.once(); end != "fail:"+CodeSeedNotBooted {
		t.Fatalf("end %s", end)
	}
	if s := e.stage(StageSeedAPI); s.State != StageFailed || len(e.pve.writes) != 0 {
		t.Fatalf("stage %+v, writes %v", s, e.pve.writes)
	}
	e = newEnv(t, RunApply)
	e.only(StageSeedAPI)
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		run.Stages[3] = Stage{ID: StageSeedAPI, State: StageRunning, StartedAt: &started, Deadline: &past}
	})
	if end := e.once(); end != "fail:seed_api_timeout" || e.stage(StageSeedAPI).Code != "seed_api_timeout" {
		t.Fatalf("no code yet: %s %+v", end, e.stage(StageSeedAPI))
	}
}

// A run whose BOM this build no longer embeds ends portal_changed. The stage it stood at ends with it: it does not
// keep reading waiting with the code it had, and the log names that stage, not the first.
func TestPortalChanged(t *testing.T) {
	e := newEnv(t, RunApply)
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.BOM.SHA256 = strings.Repeat("0", 64) })
	if end := e.once(); end != "fail:"+CodePortalChanged {
		t.Fatalf("end %s", end)
	}
	if s := e.stage(StageValidate); s.State != StagePending || strings.Contains(e.logs.String(), "stage=validate") {
		t.Fatalf("a run that had not started a stage: %+v\n%s", s, e.logs)
	}
	e = newEnv(t, RunApply)
	e.upTo(stageCluster)
	started := time.Now().Add(-time.Minute)
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		run.BOM.SHA256, run.State = strings.Repeat("0", 64), RunWaiting
		run.Stages[6] = Stage{ID: stageCluster, State: StageWaiting, StartedAt: &started, Code: CodeControlPlaneTimeout, Message: "0 of 4 machines are Running"}
	})
	if end := e.once(); end != "fail:"+CodePortalChanged {
		t.Fatalf("end %s", end)
	}
	if s := e.stage(stageCluster); s.State != StageFailed || s.Code != CodePortalChanged || !strings.Contains(s.Message, "upgraded") || s.FinishedAt == nil ||
		!strings.Contains(e.logs.String(), "stage=cluster") || e.stage(StageValidate).State != StageDone {
		t.Fatalf("the stage the run stood at: %+v\n%s", s, e.logs)
	}
}

// Stage 1 fails preflight_failed with the failed rows as items, or isolation_not_enforced when isolation is the only one.
func TestValidateStage(t *testing.T) {
	for name, tc := range map[string]struct {
		checks []Check
		end    string
	}{
		"isolation only": {[]Check{{ID: "flags", Status: CheckOK}, {ID: "isolation", Status: CheckFail, Message: "add 10.200.200.10/32"}}, "fail:" + CodeIsolationNotEnforced},
		"others":         {[]Check{{ID: "isolation", Status: CheckFail}, {ID: "privileges:capi", Status: CheckFail}}, "fail:" + CodePreflightFailed},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, RunApply)
			e.r.Preflight = func(context.Context, *Installation) ([]Check, error) { return tc.checks, nil }
			if end := e.once(); end != tc.end {
				t.Fatalf("end %s", end)
			}
			if s := e.stage(StageValidate); s.State != StageFailed || len(s.Items) != len(tc.checks) {
				t.Fatalf("stage %+v", s)
			}
		})
	}
}

// A Proxmox error that echoes a secret reaches the stage message redacted.
func TestStageMessagesAreRedacted(t *testing.T) {
	e := newEnv(t, RunApply)
	e.only(StageSeedVM)
	e.pve.failOnce["POST /nodes/pve1/qemu/9100/clone"] = `{"errors":{"auth":"PVEAPIToken=capmox@pve!capi=` + runCAPI + ` refused"}}`
	if end := e.once(); end != "defer:retry_seed_vm" {
		t.Fatalf("end %s", end)
	}
	if s := e.stage(StageSeedVM); !strings.Contains(s.Message, "[redacted]") || strings.Contains(s.Message, runCAPI) {
		t.Fatalf("message %q", s.Message)
	}
}

// fakeSeedEnv is a run at stage seed-api or seed-identity, with the seed at VMID 120 answering on a fake API.
func fakeSeedEnv(t *testing.T, stage string) (*env, *fakeSeed) {
	e := newEnv(t, RunApply)
	e.only(stage)
	seed := newFakeSeed(t, staticCA(t), func() string { return runBoot })
	e.seedAt(120, 1, seed, true)
	return e, seed
}

// Stage 4 pins TLS to the seed's CA: another server on the address fails seed_tls_mismatch, before any token is sent.
// While the API does not answer, the guest agent tells a seed that has not booted from one the portal cannot reach.
func TestSeedAPI(t *testing.T) {
	e, seed := fakeSeedEnv(t, StageSeedAPI)
	if end := e.until(3); end != "complete" || !slices.Equal(seed.callsOf("boot"), []string{"GET /readyz"}) {
		t.Fatalf("end %s, calls %v", end, seed.calls)
	}

	e, seed = fakeSeedEnv(t, StageSeedAPI)
	other := newFakeSeed(t, staticCA(t), func() string { return runBoot })
	e.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Status.Seed.APIServer = other.url })
	if end := e.once(); end != "fail:"+CodeSeedTLSMismatch || len(other.calls) != 0 {
		t.Fatalf("another server: %s, it saw %v", end, other.calls)
	}

	e, seed = fakeSeedEnv(t, StageSeedAPI)
	seed.ready = false
	if end := e.once(); end != "defer:waiting_seed_api" || e.stage(StageSeedAPI).Code != CodeSeedNotBooted {
		t.Fatalf("no agent: %s %+v", end, e.stage(StageSeedAPI))
	}
	e.pve.agent = true
	if end := e.once(); end != "defer:waiting_seed_api" || e.stage(StageSeedAPI).Code != CodeSeedAPIUnreachable {
		t.Fatalf("agent up: %s %+v", end, e.stage(StageSeedAPI))
	}
}

// Stage 5 stores the service account's kubeconfig (read with the bootstrap token, checked cluster-admin), deletes the
// bootstrap objects with it, detaches and deletes the CD, and is done only when the store no longer lists it; then the
// bootstrap token is wiped.
func TestSeedIdentityDoneOnlyWhenTheCDIsGone(t *testing.T) {
	e, seed := fakeSeedEnv(t, StageSeedIdentity)
	e.pve.keepISO = 1
	if end := e.once(); end != "defer:waiting_seed_identity" {
		t.Fatalf("CD still listed: %s", end)
	}
	_, in, sec := e.state.snapshot()
	if s := e.stage(StageSeedIdentity); s.Code != CodeSeedCDNotDeleted || sec.BootstrapToken != runBoot || sec.SeedKubeconfig == nil ||
		sec.SeedKubeconfig.Token != runSA || in.Status.Seed.ReadyAt == nil || in.Status.Seed.TokenSecret != SeedPortalTokenSecret {
		t.Fatalf("stage %+v, secrets %+v", s, sec)
	}
	// The seed is protected and running: its CD is ejected (Proxmox removes no drive from a protected VM).
	if cfg := e.pve.vms[120].cfg; cfg["ide2"] != "none,media=cdrom" || cfg["protection"] != "1" || e.pve.writeCount("POST /nodes/pve1/qemu/120/config") != 1 {
		t.Fatalf("the CD is still attached: %v, writes %v", cfg, e.pve.writes)
	}
	if end := e.until(3); end != "complete" {
		t.Fatalf("end %s", end)
	}
	_, in, sec = e.state.snapshot()
	if len(e.pve.iso) != 0 || sec.BootstrapToken != "" || in.Status.Seed.CDVolume != "" || e.stage(StageSeedIdentity).State != StageDone {
		t.Fatalf("ISO %v, bootstrap %q, cd %q", e.pve.iso, sec.BootstrapToken, in.Status.Seed.CDVolume)
	}
	if boot := seed.callsOf("boot"); !slices.Equal(boot, []string{"GET /api/v1/namespaces/proxcloud-system/secrets/proxcloud-portal-token-g1"}) {
		t.Fatalf("the bootstrap token was used for %v", boot)
	}
	for _, want := range []string{"DELETE /api/v1/namespaces/kube-system/secrets/bootstrap-token-abcdef",
		"DELETE /apis/rbac.authorization.k8s.io/v1/namespaces/proxcloud-system/roles/proxcloud-bootstrap",
		"DELETE /apis/rbac.authorization.k8s.io/v1/namespaces/proxcloud-system/rolebindings/proxcloud-bootstrap"} {
		if !slices.Contains(seed.callsOf("sa"), want) {
			t.Fatalf("missing %s in %v", want, seed.callsOf("sa"))
		}
	}
}

// The CD is deleted where the seed status says it went, not only on the spec's ISO store, which may have changed since
// the upload: stage 5 and Remove are done only when that store no longer lists it.
func TestTheCDIsDeletedWhereItWasUploaded(t *testing.T) {
	e, _ := fakeSeedEnv(t, StageSeedIdentity)
	e.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Storage.ISO = "nas-iso" })
	if end := e.until(3); end != "complete" || e.stage(StageSeedIdentity).State != StageDone {
		t.Fatalf("end %s, stage %+v", end, e.stage(StageSeedIdentity))
	}
	if _, in, _ := e.state.snapshot(); len(e.pve.iso) != 0 || in.Status.Seed.CDVolume != "" {
		t.Fatalf("stage 5 left the CD: ISO %v, cdVolume %q", e.pve.iso, in.Status.Seed.CDVolume)
	}

	e = newEnv(t, RunRemove)
	e.seedAt(120, 1, nil, true)
	e.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Storage.ISO = "nas-iso" })
	if end := e.until(5); end != "complete" || len(e.pve.iso) != 0 {
		t.Fatalf("Remove left the CD: %s, ISO %v", end, e.pve.iso)
	}
}

// The seed applies the bootstrap token before the Role and RoleBinding that let it read the token Secret: a 403 on that
// read is waited for, not taken for a lost identity.
func TestSeedIdentityWaitsForTheBootstrapRBAC(t *testing.T) {
	e, seed := fakeSeedEnv(t, StageSeedIdentity)
	seed.forbid = 1
	if end := e.once(); end != "defer:waiting_seed_identity" || e.stage(StageSeedIdentity).Code != CodeSeedIdentityLost {
		t.Fatalf("403 on the first read: %s %+v", end, e.stage(StageSeedIdentity))
	}
	if end := e.until(3); end != "complete" || e.stage(StageSeedIdentity).State != StageDone {
		t.Fatalf("end %s, stage %+v", end, e.stage(StageSeedIdentity))
	}
}

// A service account that is not cluster-admin, or a refused bootstrap token, ends the stage seed_identity_lost. Not at
// the first look: a seed that has just started knows no bootstrap token and denies every access review until its
// caches are filled, so both are tried again, and the stage's time limit ends them with that code.
func TestSeedIdentityLost(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	expire := func(e *env) {
		e.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.Stages[4].Deadline = &past })
	}
	e, seed := fakeSeedEnv(t, StageSeedIdentity)
	seed.saAllowed = false
	if end := e.once(); end != "defer:waiting_seed_identity" || e.stage(StageSeedIdentity).Code != CodeSeedIdentityLost {
		t.Fatalf("not cluster-admin: %s %+v", end, e.stage(StageSeedIdentity))
	}
	if _, _, sec := e.state.snapshot(); sec.SeedKubeconfig != nil {
		t.Fatal("a credential that is not cluster-admin was stored")
	}
	expire(e)
	if end := e.once(); end != "fail:"+CodeSeedIdentityLost || !strings.Contains(e.stage(StageSeedIdentity).Message, "cluster-admin") {
		t.Fatalf("not cluster-admin at the time limit: %s %+v", end, e.stage(StageSeedIdentity))
	}
	// A seed that answers the review only after its start: the stage goes on.
	e, seed = fakeSeedEnv(t, StageSeedIdentity)
	seed.saAllowed = false
	e.once()
	seed.mu.Lock()
	seed.saAllowed = true
	seed.mu.Unlock()
	if end := e.until(3); end != "complete" {
		t.Fatalf("cluster-admin at the second look: %s %+v", end, e.stage(StageSeedIdentity))
	}

	e, seed = fakeSeedEnv(t, StageSeedIdentity)
	seed.boot = func() string { return "zzzzzz.zzzzzzzzzzzzzzzz" }
	if end, s := e.once(), e.stage(StageSeedIdentity); end != "defer:retry_seed_identity" || s.Code != CodeSeedIdentityLost || s.Retries != 1 {
		t.Fatalf("a refused bootstrap token: %s %+v", end, s)
	}
	expire(e)
	if end := e.once(); end != "fail:"+CodeSeedIdentityLost || !strings.Contains(e.stage(StageSeedIdentity).Message, "bootstrap token") {
		t.Fatalf("expired bootstrap token: %s %+v", end, e.stage(StageSeedIdentity))
	}
	// The same 401 from a seed whose token is good once it has read its Secrets.
	e, seed = fakeSeedEnv(t, StageSeedIdentity)
	seed.boot = func() string { return "zzzzzz.zzzzzzzzzzzzzzzz" }
	e.once()
	seed.mu.Lock()
	seed.boot = func() string { return runBoot }
	seed.mu.Unlock()
	if end := e.until(3); end != "complete" {
		t.Fatalf("a bootstrap token the seed knows at the second look: %s %+v", end, e.stage(StageSeedIdentity))
	}
}

func imageEnv(t *testing.T) *env {
	e := newEnv(t, RunApply)
	e.upTo(StageImages)
	return e
}

// Stage 2 reuses a finished import whose template is still a template in pc-templates on that node, enqueues the rest
// with the BOM digest confirmed and the installation's stores (one audit entry each), and waits for them.
func TestImages(t *testing.T) {
	e := imageEnv(t)
	cur, _ := Current()
	seedDigest, nodeDigest := cur.Seed.Image.Digest, cur.Kubernetes.NodeImage.Digest
	e.imports.recs = []templateupdates.Receipt{{ID: "tu-old", Step: templateupdates.StepSucceeded, VMID: 9100, CreatedAt: time.Now().Add(-time.Hour),
		Payload: templateupdates.VMPayload{ServiceID: "svc-pve", Node: "pve1", Digest: seedDigest}}}
	e.pve.templates = []pve.ClusterResource{{Type: "qemu", VMID: 9100, Node: "pve1", Template: 1, Pool: pve.TemplatePool, Name: "pc-ghcr-seed"}}
	var audited []string
	e.r.OnImport = func(rec *templateupdates.Receipt, actor string) {
		audited = append(audited, rec.Payload.Node+" "+actor)
	}
	if end := e.once(); end != "defer:waiting_images" {
		t.Fatalf("end %s", end)
	}
	if len(e.imports.enqueued) != 3 || len(audited) != 3 || audited[0] != "pve1 platform-run:pr-1" {
		t.Fatalf("enqueued %+v, audited %v", e.imports.enqueued, audited)
	}
	for _, req := range e.imports.enqueued {
		if req.Digest != nodeDigest || req.ConfirmDigest != nodeDigest || req.ImportStorage != "nas-templates" || req.DiskStorage != "local-lvm" || req.ServiceID != "svc-pve" {
			t.Fatalf("request %+v", req)
		}
	}
	s := e.stage(StageImages)
	if s.Receipt.Templates["seed:pve1"] != 9100 || len(s.Receipt.ImportReceipts) != 3 || s.Code != CodeImageImportStalled {
		t.Fatalf("stage %+v %+v", s, s.Receipt)
	}
	// The imports finish: their templates appear and the stage is done.
	for i := range e.imports.recs {
		r := &e.imports.recs[i]
		if r.ID != "tu-old" {
			r.Step, r.VMID = templateupdates.StepTemplated, 9200+i
			e.pve.templates = append(e.pve.templates, pve.ClusterResource{Type: "qemu", VMID: r.VMID, Node: r.Payload.Node, Template: 1, Pool: pve.TemplatePool, Name: "pc-ghcr-node"})
		}
	}
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		for i := range run.Stages {
			if run.Stages[i].ID == StageSeedVM {
				run.Stages[i].State = StageDone // stop after stage 2
			}
		}
	})
	e.once()
	if s := e.stage(StageImages); s.State != StageDone || len(s.Receipt.Templates) != 4 {
		t.Fatalf("stage %+v %+v", s, s.Receipt)
	}
}

// A deleted template frees its receipt (MarkTemplateGone) and the image is imported again; an import this run asked
// for that failed ends image_import_failed; a blocklisted digest image_blocked; no import moving for an hour
// image_import_stalled.
func TestImageFailures(t *testing.T) {
	cur, _ := Current()
	seedDigest := cur.Seed.Image.Digest
	e := imageEnv(t)
	e.imports.recs = []templateupdates.Receipt{{ID: "tu-old", Step: templateupdates.StepSucceeded, VMID: 9100,
		Payload: templateupdates.VMPayload{ServiceID: "svc-pve", Node: "pve1", Digest: seedDigest}}}
	e.once()
	if !e.imports.recs[0].TemplateGone || len(e.imports.enqueued) != 4 {
		t.Fatalf("gone template: %+v, enqueued %d", e.imports.recs[0], len(e.imports.enqueued))
	}
	e.imports.recs[1].Step, e.imports.recs[1].Code, e.imports.recs[1].Message = templateupdates.StepFailed, "upload_failed", "checksum"
	if end := e.once(); end != "fail:"+CodeImageImportFailed {
		t.Fatalf("our import failed: %s", end)
	}

	e = imageEnv(t)
	e.imports.enqueueErr = templateupdates.ErrBlocklisted
	if end := e.once(); end != "fail:"+CodeImageBlocked {
		t.Fatalf("blocklisted: %s", end)
	}

	// Only a refusal is permanent: a request or an artifact that fails validation, or no proof that a template is gone.
	// A read that failed (Proxmox, the registry, the database) is retried within the stage's budget.
	for name, tc := range map[string]struct {
		mark, enqueue error
		end           string
	}{
		"mark: read failed":    {context.DeadlineExceeded, nil, "defer:retry_images"},
		"mark: refused":        {&templateupdates.LifecycleError{Status: 409, Code: "vm_updates_disabled", Message: "not enabled"}, nil, "fail:" + CodeImageImportFailed},
		"enqueue: refused":     {nil, fmt.Errorf("the artifact failed verification: %w", templateupdates.ErrRefused), "fail:" + CodeImageImportFailed},
		"enqueue: flag off":    {nil, &templateupdates.LifecycleError{Status: 409, Code: "vm_updates_disabled", Message: "not enabled"}, "fail:" + CodeImageImportFailed},
		"enqueue: read failed": {nil, errors.New("ghcr: 503"), "defer:retry_images"},
	} {
		e = imageEnv(t)
		e.imports.recs = []templateupdates.Receipt{{ID: "tu-old", Step: templateupdates.StepSucceeded, VMID: 9100,
			Payload: templateupdates.VMPayload{ServiceID: "svc-pve", Node: "pve1", Digest: seedDigest}}}
		if tc.mark == nil {
			e.imports.recs = nil
		}
		e.imports.markErr, e.imports.enqueueErr = tc.mark, tc.enqueue
		if end := e.once(); end != tc.end {
			t.Fatalf("%s: %s", name, end)
		}
	}

	e = imageEnv(t)
	e.once()
	past := time.Now().Add(-time.Second)
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.Stages[1].Deadline = &past })
	if end := e.once(); end != "fail:"+CodeImageImportStalled {
		t.Fatalf("stalled: %s", end)
	}
}

// An import that has just finished: the VM list does not show its template yet (it was read before the receipt, and
// Proxmox lists the flag and the pool a moment later), and Template updates refuses to call the template gone. The
// stage looks again and is done; nothing is imported twice. A template that never shows ends at the stall limit.
func TestImagesWaitsForATemplateThatIsNotListedYet(t *testing.T) {
	cur, _ := Current()
	e := imageEnv(t)
	e.imports.recs = []templateupdates.Receipt{{ID: "tu-seed", Step: templateupdates.StepTemplated, VMID: 9200, UpdatedAt: time.Now(),
		Payload: templateupdates.VMPayload{ServiceID: "svc-pve", Node: "pve1", Digest: cur.Seed.Image.Digest}}}
	for i, n := range []string{"pve1", "pve2", "pve3"} {
		e.imports.recs = append(e.imports.recs, templateupdates.Receipt{ID: "tu-" + n, Step: templateupdates.StepSucceeded, VMID: 9201 + i,
			Payload: templateupdates.VMPayload{ServiceID: "svc-pve", Node: n, Digest: cur.Kubernetes.NodeImage.Digest}})
		e.pve.templates = append(e.pve.templates, pve.ClusterResource{Type: "qemu", VMID: 9201 + i, Node: n, Template: 1, Pool: pve.TemplatePool, Name: "node"})
	}
	e.imports.markErr = &templateupdates.LifecycleError{Status: 409, Code: "template_present", Message: "VM 9200 on pve1 may still be this import's template"}
	if end := e.once(); end != "defer:waiting_images" {
		t.Fatalf("template not listed yet: %s", end)
	}
	if s := e.stage(StageImages); s.Code != CodeImageImportStalled || len(s.Receipt.Templates) != 3 || s.Items[0].State != "checking" ||
		!strings.Contains(s.Items[0].Message, "VM 9200 on pve1") {
		t.Fatalf("stage %+v %+v", s, s.Receipt)
	}
	e.pve.templates = append(e.pve.templates, pve.ClusterResource{Type: "qemu", VMID: 9200, Node: "pve1", Template: 1, Pool: pve.TemplatePool, Name: "seed"})
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.Stages[2].State = StageDone }) // stop after stage 2
	e.once()
	if s := e.stage(StageImages); s.State != StageDone || s.Receipt.Templates["seed:pve1"] != 9200 || len(e.imports.enqueued) != 0 || e.imports.recs[0].TemplateGone {
		t.Fatalf("stage %+v %+v, enqueued %d", s, s.Receipt, len(e.imports.enqueued))
	}

	// An import that wrote its receipt long ago is not in that window (its template was moved out of pc-templates): the
	// refusal fails the stage at once, with the text that names the VM, and nothing is imported.
	e = imageEnv(t)
	e.imports.recs = []templateupdates.Receipt{{ID: "tu-seed", Step: templateupdates.StepTemplated, VMID: 9200, UpdatedAt: time.Now().Add(-time.Hour),
		Payload: templateupdates.VMPayload{ServiceID: "svc-pve", Node: "pve1", Digest: cur.Seed.Image.Digest}}}
	e.imports.markErr = &templateupdates.LifecycleError{Status: 409, Code: "template_present", Message: "VM 9200 exists outside what this provider's token can see"}
	if end := e.once(); end != "fail:"+CodeImageImportFailed || len(e.imports.enqueued) != 0 {
		t.Fatalf("a template that was moved: %s, enqueued %d", end, len(e.imports.enqueued))
	}
	if s := e.stage(StageImages); !strings.Contains(s.Message, "VM 9200 exists outside") {
		t.Fatalf("stage %+v", s)
	}
}

// An eject that Proxmox takes (task OK) but leaves pending on the running seed is no eject: the guest still reads the
// CD, so its file is kept, the bootstrap token and the CD's name stay recorded, and the stage waits.
func TestSeedIdentityWaitsWhileTheEjectIsPending(t *testing.T) {
	e, _ := fakeSeedEnv(t, StageSeedIdentity)
	e.pve.pendCD = true
	if end := e.once(); end != "defer:waiting_seed_identity" {
		t.Fatalf("eject pending: %s", end)
	}
	_, in, sec := e.state.snapshot()
	if s := e.stage(StageSeedIdentity); s.Code != CodeSeedCDNotDeleted || !strings.Contains(s.Message, "pending") || len(e.pve.iso) != 1 ||
		sec.BootstrapToken != runBoot || in.Status.Seed.CDVolume == "" {
		t.Fatalf("stage %+v, ISO %v, cd %q", s, e.pve.iso, in.Status.Seed.CDVolume)
	}
	e.pve.mu.Lock() // the seed's next start applies it
	vm := e.pve.vms[120]
	maps.Copy(vm.cfg, vm.pending)
	vm.pending, e.pve.pendCD = nil, false
	e.pve.mu.Unlock()
	if end := e.until(3); end != "complete" || len(e.pve.iso) != 0 {
		t.Fatalf("end %s, ISO %v", end, e.pve.iso)
	}
}

// Remove, seed part: refused while a workload cluster status exists; otherwise the seed (ours, protection off,
// stopped, deleted) and every pcplat CD go, platformSecret is wiped and the phase is removed. It resumes after a lost
// lease at any checkpoint.
func TestRemoveSeed(t *testing.T) {
	total := -1
	for n := 0; n <= total || total < 0; n++ {
		e := newEnv(t, RunRemove)
		e.seedAt(120, 2, nil, true)
		e.pve.vms[120].cfg["protection"] = "1"
		e.pve.iso = append(e.pve.iso, SeedCDName(inID, 1), "pcseed-tenant.iso")
		e.pve.vms[300] = &fakeVM{cfg: map[string]any{"name": "tenant"}}
		if n > 0 {
			e.jobs.loseAt = n
			var atLoss int
			e.jobs.onLose = func() { atLoss = len(e.pve.writes) }
			e.once()
			if len(e.pve.writes) != atLoss {
				t.Fatalf("lost at %d: Proxmox written after the loss: %v", n, e.pve.writes[atLoss:])
			}
			e.jobs = &fakeJobs{}
			e.r.Jobs = e.jobs
		}
		if end := e.until(5); end != "complete" {
			t.Fatalf("lost at %d: %s", n, end)
		}
		run, in, sec := e.state.snapshot()
		if e.pve.vms[120] != nil || e.pve.vms[300] == nil || !slices.Equal(e.pve.iso, []string{"pcseed-tenant.iso"}) ||
			run.State != RunSucceeded || in.Status.Phase != PhaseRemoved || in.Status.Seed != nil || sec.CAPI != nil || sec.BootstrapToken != "" {
			t.Fatalf("lost at %d: VMs %v, ISO %v, run %s, phase %s, secrets %+v", n, e.pve.pooled(), e.pve.iso, run.State, in.Status.Phase, sec)
		}
		// The finished run says what it deleted.
		if vm, cds, secrets := item(e.stage(StageRemoveSeed), "vm-120"), item(e.stage(StageRemoveSeed), "cds"), item(e.stage(StageRemoveSecrets), "secrets"); vm == nil ||
			vm.State != "gone" || !strings.Contains(vm.Message, SeedVMName(inID, 2)) || cds == nil || secrets == nil || secrets.State != "deleted" {
			t.Fatalf("lost at %d: items %+v %+v", n, e.stage(StageRemoveSeed).Items, e.stage(StageRemoveSecrets).Items)
		}
		if n == 0 {
			total = e.jobs.checkpoints
		}
	}
}

// Stages 1 to 5 against fakes, the seed API answering with a certificate of the CA that went onto the CD, up to stage 6
// waiting for the seed's ArgoCD. No secret appears in the run, the installation, the job codes or the
// logs, even when Proxmox echoes one in an error.
func TestApplyRunsStagesOneToFive(t *testing.T) {
	e := newEnv(t, RunApply)
	cur, _ := Current()
	e.imports.recs = []templateupdates.Receipt{{ID: "tu-seed", Step: templateupdates.StepSucceeded, VMID: 9100,
		Payload: templateupdates.VMPayload{ServiceID: "svc-pve", Node: "pve1", Digest: cur.Seed.Image.Digest}}}
	e.pve.templates = []pve.ClusterResource{{Type: "qemu", VMID: 9100, Node: "pve1", Template: 1, Pool: pve.TemplatePool, Name: "seed"}}
	for i, n := range []string{"pve1", "pve2", "pve3"} {
		e.imports.recs = append(e.imports.recs, templateupdates.Receipt{ID: "tu-" + n, Step: templateupdates.StepSucceeded, VMID: 9101 + i,
			Payload: templateupdates.VMPayload{ServiceID: "svc-pve", Node: n, Digest: cur.Kubernetes.NodeImage.Digest}})
		e.pve.templates = append(e.pve.templates, pve.ClusterResource{Type: "qemu", VMID: 9101 + i, Node: n, Template: 1, Pool: pve.TemplatePool, Name: "node"})
	}
	e.pve.agent = true
	e.pve.failOnce["PUT /nodes/pve1/qemu/120/firewall/options"] = `{"errors":{"token":"refused PVEAPIToken=capmox@pve!capi=` + runCAPI + `"}}`
	fromCD := func(kind string) string {
		e.pve.mu.Lock()
		defer e.pve.mu.Unlock()
		if len(e.pve.uploads) == 0 {
			return ""
		}
		re := regexp.MustCompile(`-----BEGIN ` + kind + `-----(\\n[A-Za-z0-9+/=]+)+\\n-----END ` + kind + `-----`)
		return strings.ReplaceAll(string(re.Find(e.pve.uploads[len(e.pve.uploads)-1])), `\n`, "\n") + "\n"
	}
	var boot string
	seed := newFakeSeed(t, func() (string, string) { return fromCD("CERTIFICATE"), fromCD("EC PRIVATE KEY") }, func() string {
		_, _, sec := e.state.snapshot()
		if sec.BootstrapToken != "" {
			boot = sec.BootstrapToken
		}
		return sec.BootstrapToken
	})
	e.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Spec.Seed.Address = "127.0.0.1" })
	e.r.SeedPort = seed.url[strings.LastIndex(seed.url, ":")+1:]
	end := ""
	for range 20 { // until stage 6 waits for the seed's ArgoCD, which this fake seed never starts
		if end = e.once(); end == "defer:waiting_seed_platform" {
			break
		}
	}
	if end != "defer:waiting_seed_platform" {
		t.Fatalf("end %s: %s", end, e.logs)
	}
	run, in, sec := e.state.snapshot()
	for i, s := range run.Stages[:5] {
		if s.State != StageDone {
			t.Fatalf("stage %d %s: %+v", i+1, s.ID, s)
		}
	}
	if run.Stages[5].State != StageWaiting || run.Stages[5].Code != CodeComponentUnhealthy || sec.SeedKubeconfig == nil ||
		sec.BootstrapToken != "" || len(e.pve.iso) != 0 || in.Status.Seed.ReadyAt == nil || e.stage(StageSeedVM).Retries != 1 {
		t.Fatalf("run %+v, seed %+v", run.Stages[5], in.Status.Seed)
	}
	caKey := strings.Split(strings.TrimSpace(fromCD("EC PRIVATE KEY")), "\n")
	if len(caKey) < 3 || boot == "" || sec.K3sToken == "" {
		t.Fatalf("no CA key on the CD (%d lines) or no tokens", len(caKey))
	}
	dump, _ := json.Marshal(map[string]any{"run": run, "installation": in, "ends": e.jobs.ends, "logs": e.logs.String()})
	for name, secret := range map[string]string{"capi": runCAPI, "csi": runCSI, "sa": runSA, "k3s": sec.K3sToken, "bootstrap": boot,
		"bootstrap secret": boot[7:], "ca key": caKey[1]} {
		if strings.Contains(string(dump), secret) {
			t.Fatalf("%s leaked: %s", name, dump)
		}
	}
}
