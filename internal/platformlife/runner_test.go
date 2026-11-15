package platformlife

import (
	"context"
	"encoding/json"
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

// only marks every stage of the apply up to stage 5 done but id, so a run ends at stage 6 once id is done.
func (e *env) only(id string) {
	e.upTo(id)
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		after := false
		for i := range run.Stages {
			switch s := &run.Stages[i]; {
			case s.ID == id:
				after = true
			case after && slices.Contains([]string{StageSeedAPI, StageSeedIdentity}, s.ID):
				s.State = StageDone
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
	if end := e.until(5); end != "fail:"+CodeStageUnavailable {
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
	if in.Status.Phase != PhaseFailed {
		t.Fatalf("phase %s after stage 6 is refused", in.Status.Phase)
	}
	run, _, _ := e.state.snapshot()
	if run.State != RunFailed || run.Code != CodeStageUnavailable || !strings.Contains(run.Message, "stages 1 to 5") {
		t.Fatalf("run %s %s %q", run.State, run.Code, run.Message)
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
		if end := e.until(8); end != "fail:"+CodeStageUnavailable {
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
	if end := e.until(5); end != "fail:"+CodeStageUnavailable {
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
		if end := e.until(5); end != "fail:"+CodeStageUnavailable {
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
	if got := TimeoutCode(StageSeedIdentity); got != "seed_identity_timeout" {
		t.Fatalf("timeout code %s", got)
	}
}

// A run whose BOM this build no longer embeds ends portal_changed.
func TestPortalChanged(t *testing.T) {
	e := newEnv(t, RunApply)
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.BOM.SHA256 = strings.Repeat("0", 64) })
	if end := e.once(); end != "fail:"+CodePortalChanged {
		t.Fatalf("end %s", end)
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
	if end := e.until(3); end != "fail:"+CodeStageUnavailable || !slices.Equal(seed.callsOf("boot"), []string{"GET /readyz"}) {
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
	if _, attached := e.pve.vms[120].cfg["ide2"]; attached {
		t.Fatal("the CD is still attached")
	}
	if end := e.until(3); end != "fail:"+CodeStageUnavailable {
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

// A service account that is not cluster-admin, or a refused bootstrap token, ends the stage seed_identity_lost.
func TestSeedIdentityLost(t *testing.T) {
	e, seed := fakeSeedEnv(t, StageSeedIdentity)
	seed.saAllowed = false
	if end := e.once(); end != "fail:"+CodeSeedIdentityLost {
		t.Fatalf("not cluster-admin: %s", end)
	}
	e, seed = fakeSeedEnv(t, StageSeedIdentity)
	seed.boot = func() string { return "zzzzzz.zzzzzzzzzzzzzzzz" }
	if end := e.once(); end != "fail:"+CodeSeedIdentityLost {
		t.Fatalf("expired bootstrap token: %s", end)
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

	e = imageEnv(t)
	e.once()
	past := time.Now().Add(-time.Second)
	e.mutate(func(run *Run, _ *Installation, _ *Secrets) { run.Stages[1].Deadline = &past })
	if end := e.once(); end != "fail:"+CodeImageImportStalled {
		t.Fatalf("stalled: %s", end)
	}
}

// Remove, seed part: refused while a workload cluster status exists; otherwise the seed (ours, protection off,
// stopped, deleted) and every pcplat CD go, platformSecret is wiped and the phase is removed. It resumes after a lost
// lease at any checkpoint.
func TestRemoveSeed(t *testing.T) {
	e := newEnv(t, RunRemove)
	e.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Status.Clusters = []ClusterStatus{{Name: ClusterName}} })
	if end := e.once(); end != "fail:"+CodeStageUnavailable || len(e.pve.writes) != 0 {
		t.Fatalf("with a cluster: %s %v", end, e.pve.writes)
	}
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
		if n == 0 {
			total = e.jobs.checkpoints
		}
	}
}

// The whole of what this build runs: stages 1 to 5 against fakes, the seed API answering with a certificate of the CA
// that went onto the CD, then stage 6 refused. No secret appears in the run, the installation, the job codes or the
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
	if end := e.until(20); end != "fail:"+CodeStageUnavailable {
		t.Fatalf("end %s: %s", end, e.logs)
	}
	run, in, sec := e.state.snapshot()
	for i, s := range run.Stages[:5] {
		if s.State != StageDone {
			t.Fatalf("stage %d %s: %+v", i+1, s.ID, s)
		}
	}
	if run.Stages[5].State != StageFailed || run.Stages[5].Code != CodeStageUnavailable || sec.SeedKubeconfig == nil ||
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
