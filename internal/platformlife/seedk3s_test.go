package platformlife

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// newerK3s makes the target BOM's k3s another version, with the downloads of that version.
func newerK3s(version string) func(*BOM) {
	return func(b *BOM) {
		tag := strings.ReplaceAll(version, "+", "%2B")
		b.Seed.K3s = K3s{Version: version,
			Binary:        Download{URL: "https://github.com/k3s-io/k3s/releases/download/" + tag + "/k3s", SHA256: strings.Repeat("7", 64)},
			InstallScript: Download{URL: "https://raw.githubusercontent.com/k3s-io/k3s/" + tag + "/install.sh", SHA256: strings.Repeat("8", 64)}}
	}
}

// The step rule of the seed's k3s, from 0.8.1's v1.35.10+k3s1: a patch, a k3s build and one minor up are moved by the
// run (class seed, with the three things it writes on the seed); back, two minors and a changed published version
// refuse the upgrade, each with its own sentence.
func TestK3sStepRule(t *testing.T) {
	for version, refusal := range map[string]string{
		"v1.35.11+k3s1": "",
		"v1.35.10+k3s2": "",
		"v1.36.0+k3s1":  "",
		"v1.35.9+k3s3":  "k3s never goes back",
		"v1.34.12+k3s1": "k3s never goes back",
		"v1.37.0+k3s1":  "one minor at a time: go through 1.36 first",
		"v1.35.10+k3s1": "a published k3s version does not change", // the same version, other sums
	} {
		t.Run(version, func(t *testing.T) {
			old := nextRelease(t, newerK3s(version))
			cur, _ := Current()
			if err := cur.Validate(); err != nil {
				t.Fatalf("the test BOM: %v", err)
			}
			changes := Classify(old.BOM, cur.BOM)
			i := slices.IndexFunc(changes, func(c PlanChange) bool { return c.Name == "seed.k3s" })
			ref := refOf(old)
			_, up, err := move(installed(&ref), cur.SHA256)
			if i < 0 || err != nil {
				t.Fatalf("changes %+v: %v", changes, err)
			}
			c := changes[i]
			if refusal != "" {
				if c.Class != ClassRefused || !strings.HasSuffix(c.Blocking, refusal) || up.Allowed || up.MovesSeed() || len(up.Blocking) != 1 {
					t.Fatalf("%+v, upgrade %+v", c, up)
				}
				return
			}
			var site []string
			for _, w := range changes {
				if w.Class == ClassSite && w.Stage == stageSeedK3s && w.Impact != "" {
					site = append(site, w.To)
				}
			}
			if c.Class != ClassSeed || c.Stage != stageSeedK3s || c.From != "v1.35.10+k3s1" || c.To != version || c.Blocking != "" ||
				!strings.Contains(c.Impact, "tenant cluster") || !up.Allowed || !up.MovesSeed() || len(site) != 3 ||
				!strings.Contains(site[0], k3sJobName(version)) || !strings.Contains(site[1], "pre-upgrade-v1.35.10-k3s1") || !strings.Contains(site[2], "k3s.prev") {
				t.Fatalf("%+v, site %v, upgrade %+v", c, site, up)
			}
		})
	}
}

// The Job runs the first boot's own download, checksum and mode lines, on another path: neither can change alone.
// Its script stops at the first failure and replaces the binary only after the checksum.
func TestSeedK3sJobSharesTheFirstBootLines(t *testing.T) {
	b := embedded(t)
	ud, err := SeedUserData(b, renderInput(), plantBoot)
	if err != nil {
		t.Fatal(err)
	}
	var cloud struct {
		Runcmd []string `json:"runcmd"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(ud, "#cloud-config\n")), &cloud); err != nil {
		t.Fatal(err)
	}
	job := SeedK3sJob(b, "v1.35.9+k3s1")
	c := dig(job, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
	cmd := c["command"].([]any)
	lines := strings.Split(cmd[len(cmd)-1].(string), "\n")
	if got := cmd[:len(cmd)-1]; !slices.Equal(got, []any{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "sh", "-euc"}) {
		t.Fatalf("command %v", got)
	}
	shared := 0
	for _, l := range lines {
		if slices.Contains(cloud.Runcmd, strings.ReplaceAll(l, "/usr/local/bin/k3s.new", "/usr/local/bin/k3s")) && strings.Contains(l, "k3s.new") {
			shared++
		}
	}
	sum := slices.IndexFunc(lines, func(l string) bool {
		return strings.Contains(l, b.Seed.K3s.Binary.SHA256) && strings.HasSuffix(l, "| sha256sum -c -")
	})
	mv := slices.Index(lines, "mv -f /usr/local/bin/k3s.new /usr/local/bin/k3s")
	if shared != 3 || lines[0] != "k3s etcd-snapshot save --name 'pre-upgrade-v1.35.9-k3s1'" || sum < 0 || mv < sum ||
		lines[mv-1] != "cp -p /usr/local/bin/k3s /usr/local/bin/k3s.prev" || lines[len(lines)-1] != "systemctl restart k3s" {
		t.Fatalf("%d shared lines of:\n%s", shared, strings.Join(lines, "\n"))
	}
	if c["image"] != b.Images.Clusterctl || dig(job, "spec", "template", "spec", "hostPID") != true || dig(c, "securityContext", "privileged") != true ||
		dig(job, "spec", "backoffLimit") != 0 || dig(job, "spec", "template", "spec", "restartPolicy") != "Never" {
		t.Fatalf("job %v", job)
	}
}

const k3sJobs = "/apis/batch/v1/namespaces/proxcloud-system/jobs/"

// k3sUpgrade is an installed 0.8.1 platform whose seed runs 0.8.1's k3s, with the first run of an upgrade to a BOM
// that names k3s v1.35.11+k3s1 standing at stage seed-k3s.
func k3sUpgrade(t *testing.T) (p *platform, job string) {
	t.Helper()
	old := nextRelease(t, newerK3s("v1.35.11+k3s1"))
	cur, _ := Current()
	from := refOf(old)
	p = newPlatform(t, RunApply)
	p.seed.node(old.Seed.K3s.Version, true)
	p.mutate(func(run *Run, in *Installation, _ *Secrets) { run.BOM, in.Status.BOM = from, &from })
	p.upTo(stageSeedK3s)
	if end := p.until(30); end != "complete" || p.stage(stageSeedK3s).State != StageSkipped {
		t.Fatalf("the installing run: %s, %+v", end, p.stage(stageSeedK3s))
	}
	p.start(t, cur.SHA256, "upgrade lab to 0.9.0")
	p.atK3s()
	return p, k3sJobs + k3sJobName(cur.Seed.K3s.Version)
}

// atK3s makes the run stand at stage seed-k3s (start marks every stage before the seed's platform done).
func (p *platform) atK3s() {
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		run.Stages[slices.Index(stageIDs(RunApply), stageSeedK3s)] = Stage{ID: stageSeedK3s, State: StagePending}
	})
}

// ended gives the Job its end, as the kubelet reports it once k3s is back.
func (p *platform) ended(job, field string) {
	p.seed.mu.Lock()
	defer p.seed.mu.Unlock()
	p.seed.objs[job]["status"] = map[string]any{field: 1}
}

func TestSeedK3sUpgrade(t *testing.T) {
	p, job := k3sUpgrade(t)
	cur, _ := Current()
	if run, in, _ := p.state.snapshot(); !run.SeedBackupTaken || in.Status.Seed.K3sVersion != "v1.35.10+k3s1" {
		t.Fatalf("the upgrade's run: backup taken %t, recorded k3s %q", run.SeedBackupTaken, in.Status.Seed.K3sVersion)
	}
	// The first look makes the Job, with the BOM's image and the target's sum; the next ones wait for it.
	for range 2 {
		if end, s := p.once(), p.stage(stageSeedK3s); end != "defer:waiting_seed_k3s" || s.Code != TimeoutCode(stageSeedK3s) {
			t.Fatalf("end %s %+v", end, s)
		}
	}
	made := p.seed.obj(job)
	c, _ := dig(made, "spec", "template", "spec", "containers").([]any)
	if len(c) != 1 || str(c[0].(map[string]any), "image") != cur.Images.Clusterctl || !strings.Contains(jsonOf(made), strings.Repeat("7", 64)) ||
		!strings.Contains(jsonOf(made), "pre-upgrade-v1.35.10-k3s1") {
		t.Fatalf("the Job: %v", made)
	}
	// k3s restarts: the seed's API is away, which is waited for and spends no retry.
	p.seed.mu.Lock()
	p.seed.deny = []denial{{503, "apiserver not ready"}}
	p.seed.mu.Unlock()
	if end, s := p.once(), p.stage(stageSeedK3s); end != "defer:waiting_seed_k3s" || s.Code != CodeSeedAPIUnreachable || s.Retries != 0 {
		t.Fatalf("while k3s restarts: end %s %+v", end, s)
	}
	// The node at the target but the Job's end not written yet, then both: the record moves, the Job goes, stage 6 follows.
	p.seed.node("v1.35.11+k3s1", true)
	if end := p.once(); end != "defer:waiting_seed_k3s" {
		t.Fatalf("before the Job's end is known: %s", end)
	}
	p.ended(job, "succeeded")
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageSeedK3s))
	}
	_, in, _ := p.state.snapshot()
	if in.Status.Seed.K3sVersion != "v1.35.11+k3s1" || p.seed.obj(job) != nil || p.stage(stageSeedK3s).State != StageDone ||
		p.stage(stageSeedPlatform).State != StageDone || in.Status.BOM.SHA256 != cur.SHA256 {
		t.Fatalf("after the upgrade: seed %+v, job %v, stage %+v", in.Status.Seed, p.seed.obj(job), p.stage(stageSeedK3s))
	}
	p.leaks(t)
}

// A Job that failed ends the run with the script's last words and is deleted, so Retry makes a new one. A seed whose
// node reports a k3s the BOM's is no step from is refused before anything is made.
func TestSeedK3sUpgradeFails(t *testing.T) {
	p, job := k3sUpgrade(t)
	p.once()
	p.ended(job, "failed")
	pod := "/api/v1/namespaces/proxcloud-system/pods/k3s-upgrade-x"
	p.seed.put(pod, map[string]any{"metadata": map[string]any{"name": "k3s-upgrade-x", "labels": map[string]any{"job-name": job[len(k3sJobs):]}},
		"status": map[string]any{"containerStatuses": []any{map[string]any{"state": map[string]any{"terminated": map[string]any{
			"message": "/usr/local/bin/k3s.new: FAILED\nsha256sum: WARNING: 1 computed checksum did NOT match\n"}}}}}})
	end, s := p.once(), p.stage(stageSeedK3s)
	if end != "fail:"+CodeSeedK3sFailed || s.State != StageFailed || !strings.Contains(s.Message, "did NOT match. The seed runs k3s v1.35.10+k3s1 as before. Retry runs it again.") ||
		p.seed.obj(job) != nil || p.seed.obj(pod) != nil {
		t.Fatalf("end %s %+v, job %v", end, s, p.seed.obj(job))
	}
	p.start(t, "", "") // Retry
	p.atK3s()
	if end := p.once(); end != "defer:waiting_seed_k3s" || p.seed.obj(job) == nil {
		t.Fatalf("Retry: end %s, job %v", end, p.seed.obj(job))
	}
	// The seed's API stays away past the stage's deadline: one code, and the step by hand.
	p.seed.mu.Lock()
	p.seed.deny = []denial{{503, "apiserver not ready"}}
	p.seed.mu.Unlock()
	p.once()
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		past := time.Now().Add(-time.Minute)
		run.Stages[slices.Index(stageIDs(RunApply), stageSeedK3s)].Deadline = &past
	})
	if end, s := p.once(), p.stage(stageSeedK3s); end != "fail:seed_k3s_timeout" || !strings.Contains(s.Message, "does not answer") ||
		!strings.Contains(s.Message, "keep serving") || !strings.Contains(s.Message, "mv /usr/local/bin/k3s.prev /usr/local/bin/k3s && systemctl restart k3s") {
		t.Fatalf("past the deadline: end %s %+v", end, s)
	}

	p, job = k3sUpgrade(t)
	p.seed.node("v1.33.4+k3s1", true)
	if end, s := p.once(), p.stage(stageSeedK3s); end != "fail:"+CodeSeedK3sRefused || !strings.Contains(s.Message, "v1.33.4+k3s1") ||
		!strings.Contains(s.Message, "v1.35.11+k3s1") || p.seed.obj(job) != nil {
		t.Fatalf("a seed two minors behind: end %s %+v", end, s)
	}
}

// An upgrade that replaces the seed's k3s starts only with a backup of the seed: seen by the portal, or taken by the
// operator's word, which the run records. A Retry asks for none.
func TestStartApplyNeedsASeedBackup(t *testing.T) {
	old := nextRelease(t, newerK3s("v1.35.11+k3s1"))
	cur, _ := Current()
	ref := refOf(old)
	in := installed(&ref)
	in.Status.Seed = &SeedStatus{VMID: 120, Node: "pve1"}
	var missing *SeedBackupMissing
	if err := startApply(&Run{}, in, cur.SHA256, "upgrade lab to 0.9.0", SeedBackup{}); !errors.As(err, &missing) || in.Status.TargetBOM != nil ||
		err.Error() != "seed_backup_missing: no backup of seed VM 120 younger than 24 hours is visible to the portal. Take one (vzdump 120), or confirm that you took one the portal cannot see." {
		t.Fatalf("without a backup: %v", err)
	}
	seen, taken, retry := &Run{}, &Run{}, &Run{}
	if err := startApply(seen, in, cur.SHA256, "upgrade lab to 0.9.0", SeedBackup{Seen: true}); err != nil || seen.SeedBackupTaken {
		t.Fatalf("a backup the portal saw: %+v %v", seen, err)
	}
	if err := startApply(retry, in, "", "", SeedBackup{}); err != nil || retry.SeedBackupTaken {
		t.Fatalf("Retry: %+v %v", retry, err)
	}
	in.Status.TargetBOM = nil
	if err := startApply(taken, in, cur.SHA256, "upgrade lab to 0.9.0", SeedBackup{Taken: true}); err != nil || !taken.SeedBackupTaken {
		t.Fatalf("a backup the operator took: %+v %v", taken, err)
	}
	// The plan says the same before the run, and nothing once the upgrade has started.
	in.Status.TargetBOM = nil
	p, _ := PlanFor(in, "")
	reads := 0
	p.NeedSeedBackup(in, func() Check { reads++; return Check{ID: "seed-backup", Status: CheckWarn} })
	if u := p.Upgrade; reads != 1 || !u.Allowed || u.SeedBackup == nil || len(u.Blocking) != 1 || u.Blocking[0] != missing.Error() {
		t.Fatalf("the plan: %+v", p.Upgrade)
	}
	in.Status.TargetBOM = &BOMRef{Release: cur.Release, SHA256: cur.SHA256}
	p, _ = PlanFor(in, "")
	p.NeedSeedBackup(in, func() Check { reads++; return Check{} })
	if reads != 1 || len(p.Upgrade.Blocking) != 0 {
		t.Fatalf("the plan of a started upgrade read Proxmox or blocks: %+v", p.Upgrade)
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }
