package platformlife

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
// run (class seed, with the four things it writes on the seed); back, two minors and a changed published version
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
				!strings.Contains(c.Impact, "tenant cluster") || !up.Allowed || !up.MovesSeed() || len(site) != 4 ||
				!strings.Contains(site[0], k3sJobName(version)) || !strings.Contains(site[1], "pre-upgrade-v1.35.10-k3s1") || site[2] != k3sLeaderElectFile ||
				!strings.Contains(site[3], "k3s.prev") {
				t.Fatalf("%+v, site %v, upgrade %+v", c, site, up)
			}
			// The binary's row carries the way back: across a minor it is the snapshot restore, within one the binary alone.
			back := changes[slices.IndexFunc(changes, func(w PlanChange) bool { return w.Name == "seed-k3s-binary" })].Impact
			if minor := version == "v1.36.0+k3s1"; strings.Contains(back, "--cluster-reset-restore-path") != minor ||
				strings.Contains(back, "mv /usr/local/bin/k3s.prev /usr/local/bin/k3s && systemctl restart k3s") == minor {
				t.Fatalf("the way back of %s: %s", version, back)
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
	conf := slices.IndexFunc(lines, func(l string) bool { return strings.HasSuffix(l, " "+k3sLeaderElectFile) })
	if shared != 3 || lines[0] != "k3s etcd-snapshot save --name 'pre-upgrade-v1.35.9-k3s1'" || sum < 0 || conf < sum || mv < conf ||
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
		run.Stages[slices.Index(stageIDs(RunApply, ""), stageSeedK3s)] = Stage{ID: stageSeedK3s, State: StagePending}
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
		run.Stages[slices.Index(stageIDs(RunApply, ""), stageSeedK3s)].Deadline = &past
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

	// The Job ended long ago and the node still reports the old k3s (the previous binary was put back by hand): the
	// Job goes and the run ends, so that Retry makes a new one.
	p, job = k3sUpgrade(t)
	p.once()
	p.seed.mu.Lock()
	p.seed.objs[job]["status"] = map[string]any{"succeeded": 1, "completionTime": time.Now().Add(-2 * seedK3sStale).UTC().Format(time.RFC3339)}
	p.seed.mu.Unlock()
	if end, s := p.once(), p.stage(stageSeedK3s); end != "fail:"+CodeSeedK3sFailed || !strings.Contains(s.Message, "previous binary is in place") || p.seed.obj(job) != nil {
		t.Fatalf("a Job that ended on a seed still at the old k3s: end %s %+v", end, s)
	}
}

// A seed behind a k3s that both BOMs name is not moved: the plan listed no seed row and asked for no backup.
func TestSeedK3sNotInThePlanIsRefused(t *testing.T) {
	old := nextRelease(t, nil)
	cur, _ := Current()
	from := refOf(old)
	p := newPlatform(t, RunApply)
	p.mutate(func(run *Run, in *Installation, _ *Secrets) { run.BOM, in.Status.BOM = from, &from })
	p.upTo(stageSeedK3s)
	if end := p.until(30); end != "complete" {
		t.Fatalf("the installing run: %s", end)
	}
	p.start(t, cur.SHA256, "upgrade lab to 0.9.0")
	p.atK3s()
	p.seed.node("v1.35.9+k3s1", true)
	if end, s := p.once(), p.stage(stageSeedK3s); end != "fail:"+CodeSeedK3sRefused || !strings.Contains(s.Message, "does not replace k3s") ||
		p.seed.obj(k3sJobs+k3sJobName(cur.Seed.K3s.Version)) != nil {
		t.Fatalf("a seed behind an unchanged pin: end %s %+v", end, s)
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
	if err := startApply(&Run{}, in, cur.SHA256, "upgrade lab to 0.9.0", "", SeedBackup{}, RollRead{}); !errors.As(err, &missing) || in.Status.TargetBOM != nil ||
		err.Error() != "seed_backup_missing: the portal saw no backup of seed VM 120 younger than 24 hours, and on most stores it cannot see one (the seed-backup row says where). Check in Proxmox that there is an archive of this seed VM, made after the seed VM was created and younger than 24 hours (vzdump 120 makes one), then confirm it (seedBackupTaken)." {
		t.Fatalf("without a backup: %v", err)
	}
	seen, taken, retry := &Run{}, &Run{}, &Run{}
	if err := startApply(seen, in, cur.SHA256, "upgrade lab to 0.9.0", "", SeedBackup{Seen: true}, RollRead{}); err != nil || seen.SeedBackupTaken {
		t.Fatalf("a backup the portal saw: %+v %v", seen, err)
	}
	if err := startApply(retry, in, "", "", "", SeedBackup{}, RollRead{}); err != nil || retry.SeedBackupTaken {
		t.Fatalf("Retry: %+v %v", retry, err)
	}
	in.Status.TargetBOM = nil
	if err := startApply(taken, in, cur.SHA256, "upgrade lab to 0.9.0", "", SeedBackup{Taken: true}, RollRead{}); err != nil || !taken.SeedBackupTaken {
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

// fakeHost runs sh lines the product gives for the seed's host (the Job's script, the step by hand) with the real sh
// -eu, coreutils and sha256sum on a directory that stands for the seed's root. k3s, curl, systemctl and
// k3s-killall.sh are stubs that write their arguments to the returned log; curl writes "new k3s" where -o says, and a
// stub named in fail exits 1. It shows what the shell does with the lines (quoting, order, where a failure stops
// them), nothing of what k3s does with the files.
func fakeHost(t *testing.T, script string, fail ...string) (root, log string) {
	t.Helper()
	root = t.TempDir()
	return root, hostRun(t, root, script, fail...)
}

func hostRun(t *testing.T, root, script string, fail ...string) string {
	t.Helper()
	stubs := filepath.Join(root, "stubs")
	for _, d := range []string{stubs, filepath.Join(root, "usr/local/bin"), filepath.Join(root, "etc/rancher/k3s"), filepath.Join(root, k3sSnapshots)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logFile := filepath.Join(root, "log")
	for _, name := range []string{"k3s", "curl", "systemctl", "k3s-killall.sh"} {
		body := "#!/bin/sh\necho " + name + " \"$@\" >> " + logFile + "\n"
		switch {
		case slices.Contains(fail, name):
			body += "exit 1\n"
		case name == "curl":
			body += "while [ $# -gt 1 ]; do [ \"$1\" = -o ] && printf 'new k3s' > \"$2\"; shift; done\n"
		}
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"/usr/local/bin", "/etc/rancher", "/var/lib/rancher"} {
		script = strings.ReplaceAll(script, dir, root+dir)
	}
	cmd := exec.Command("sh", "-euc", script)
	cmd.Env = []string{"PATH=" + stubs + ":" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	b, _ := os.ReadFile(logFile)
	if err != nil {
		return string(b) + "exit: " + err.Error() + " " + string(out)
	}
	return string(b)
}

// The Job's script on a host: it writes the drop-in the first boot writes (the same bytes, which parse to the three
// keys k3s appends), a second run changes nothing, and each failure stops it where the stage's message says: a wrong
// sum before the drop-in and the binary, and a failure at the binary step with the drop-in whole and no restart.
func TestSeedK3sJobOnAHost(t *testing.T) {
	b := *embedded(t)
	b.Seed.K3s.Binary.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("new k3s")))
	script := func(b *BOM) string {
		c := dig(SeedK3sJob(b, "v1.35.10+k3s1"), "spec", "template", "spec", "containers").([]any)[0].(map[string]any)["command"].([]any)
		return c[len(c)-1].(string)
	}
	read := func(root, path string) string { d, _ := os.ReadFile(filepath.Join(root, path)); return string(d) }
	const bin = "/usr/local/bin/k3s"

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "usr/local/bin"), 0o755)
	os.WriteFile(filepath.Join(root, bin), []byte("old k3s"), 0o755)
	for range 2 {
		log := hostRun(t, root, script(&b))
		os.Remove(filepath.Join(root, "log"))
		var conf map[string][]string
		if err := yaml.Unmarshal([]byte(read(root, k3sLeaderElectFile)), &conf); err != nil {
			t.Fatal(err)
		}
		off := []string{"leader-elect=false"}
		if !strings.HasPrefix(log, "k3s etcd-snapshot save --name pre-upgrade-v1.35.10-k3s1\ncurl ") || !strings.HasSuffix(log, "\nsystemctl restart k3s\n") ||
			read(root, k3sLeaderElectFile) != k3sLeaderElect+"\n" || read(root, k3sLeaderElectFile+".new") != "" || len(conf) != 3 ||
			!slices.Equal(conf["kube-controller-manager-arg+"], off) || !slices.Equal(conf["kube-scheduler-arg+"], off) ||
			!slices.Equal(conf["kube-cloud-controller-manager-arg+"], off) || read(root, bin) != "new k3s" || read(root, bin+".prev") == "" {
			t.Fatalf("log:\n%s\ndrop-in %q, conf %v, binary %q", log, read(root, k3sLeaderElectFile), conf, read(root, bin))
		}
	}

	// A wrong sum: nothing of k3s's is written.
	wrong := b
	wrong.Seed.K3s.Binary.SHA256 = strings.Repeat("7", 64)
	root = t.TempDir()
	os.MkdirAll(filepath.Join(root, "usr/local/bin"), 0o755)
	os.WriteFile(filepath.Join(root, bin), []byte("old k3s"), 0o755)
	if log := hostRun(t, root, script(&wrong)); !strings.Contains(log, "exit: ") || strings.Contains(log, "systemctl") ||
		read(root, bin) != "old k3s" || read(root, bin+".prev") != "" || read(root, k3sLeaderElectFile) != "" {
		t.Fatalf("a wrong sum:\n%s\nbinary %q, drop-in %q", log, read(root, bin), read(root, k3sLeaderElectFile))
	}
	// A failed snapshot: the same, and no download.
	root, log := fakeHost(t, script(&b), "k3s")
	if !strings.Contains(log, "exit: ") || strings.Contains(log, "curl") || read(root, k3sLeaderElectFile) != "" {
		t.Fatalf("a failed snapshot:\n%s", log)
	}
	// The binary step fails (here: no running binary to keep): the drop-in is there, whole, and k3s is not restarted.
	// That is the one failure that leaves something behind; the running k3s does not read it.
	root, log = fakeHost(t, script(&b))
	if !strings.Contains(log, "exit: ") || strings.Contains(log, "systemctl") || read(root, k3sLeaderElectFile) != k3sLeaderElect+"\n" ||
		read(root, bin) != "" {
		t.Fatalf("a failed binary step:\n%s\ndrop-in %q", log, read(root, k3sLeaderElectFile))
	}
}

// The step by hand of seed_k3s_timeout. Within a minor it is the previous binary. Across one it is k3s's rollback for
// embedded etcd, run here by a real sh: with two snapshots of the Job's name the newest is restored, after
// k3s-killall.sh and the binary, before the start; with none the command stops before it changes anything.
func TestSeedK3sWayBack(t *testing.T) {
	at := func(v string) *BOM { b := *embedded(t); newerK3s(v)(&b); return &b }
	from := at("v1.35.10+k3s1")
	patch := seedK3sTimeout(from, at("v1.35.11+k3s1"))
	if !strings.Contains(patch, "`mv /usr/local/bin/k3s.prev /usr/local/bin/k3s && systemctl restart k3s`") || strings.Contains(patch, "cluster-reset") ||
		seedK3sTimeout(nil, from) != patch {
		t.Fatalf("a patch step: %s", patch)
	}
	msg := seedK3sTimeout(from, at("v1.36.6+k3s1"))
	if !strings.Contains(msg, "keep serving") || !strings.Contains(msg, k3sSnapshots+"/pre-upgrade-v1.35.10-k3s1-<node>-<time>") || strings.Contains(msg, "systemctl restart") {
		t.Fatalf("a minor step: %s", msg)
	}
	_, cmd, _ := strings.Cut(msg, "`")
	cmd, _, _ = strings.Cut(cmd, "`")

	root := t.TempDir()
	prev := filepath.Join(root, "usr/local/bin/k3s.prev")
	place := func() {
		os.MkdirAll(filepath.Dir(prev), 0o755)
		os.WriteFile(prev, []byte("old k3s"), 0o755)
		os.WriteFile(filepath.Join(root, "usr/local/bin/k3s"), []byte("new k3s"), 0o755)
	}
	place()
	if log := hostRun(t, root, cmd); !strings.Contains(log, "exit: ") || strings.Contains(log, "k3s-killall.sh") || strings.Contains(log, "k3s server") {
		t.Fatalf("without a snapshot:\n%s", log)
	}
	if d, _ := os.ReadFile(prev); string(d) != "old k3s" {
		t.Fatalf("without a snapshot the binary was moved")
	}
	os.Remove(filepath.Join(root, "log"))
	snaps := filepath.Join(root, k3sSnapshots)
	older, newest := filepath.Join(snaps, "pre-upgrade-v1.35.10-k3s1-seed-1700000000"), filepath.Join(snaps, "pre-upgrade-v1.35.10-k3s1-seed-1700000900")
	for _, f := range []string{older, newest, filepath.Join(snaps, "etcd-snapshot-seed-1700009999")} {
		os.WriteFile(f, nil, 0o600)
	}
	os.Chtimes(older, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	want := "k3s-killall.sh\nk3s server --cluster-reset --cluster-reset-restore-path=" + newest + "\nsystemctl start k3s\n"
	if log := hostRun(t, root, cmd); log != want {
		t.Fatalf("the way back ran:\n%s\nwant:\n%s", log, want)
	}
	if d, _ := os.ReadFile(filepath.Join(root, "usr/local/bin/k3s")); string(d) != "old k3s" {
		t.Fatalf("the binary after the way back: %q", d)
	}
}
