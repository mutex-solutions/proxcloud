package platformlife

// seedk3s.go — stage seed-k3s: an upgrade run whose BOM names a newer k3s replaces the binary on the running seed and
// restarts k3s, through a privileged Job on the seed itself (the portal has no shell there). The Job runs the first
// boot's own download and checksum lines (k3sFetch) on the host; the install script is not run again, as k3s's manual
// upgrade is the binary and a restart. Never run on a lab: docs/managed-services.md lists what a lab must show.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// k3sSnapshots is where k3s keeps the snapshots of its embedded etcd.
const k3sSnapshots = "/var/lib/rancher/k3s/server/db/snapshots"

// seedK3sTimeout ends the message of a seed-k3s stage that ran out of time.
const seedK3sTimeout = " The platform cluster and every tenant keep serving; nothing is reconciled (machines, the platform's own components, runs) " +
	"until the seed's API answers. If it does not come back and the binary was replaced (/usr/local/bin/k3s.prev exists), put the previous one back " +
	"over SSH with a key of spec.seed.sshKeys: `mv /usr/local/bin/k3s.prev /usr/local/bin/k3s && systemctl restart k3s` (this step was not tried on " +
	"a lab). Retry then runs the stage again."

func k3sJobName(version string) string {
	return "k3s-upgrade-" + strings.ReplaceAll(slug(version), ".", "-")
}

func k3sSnapshotName(from string) string { return "pre-upgrade-" + slug(from) }

// SeedK3sJob is the Job that moves the seed from k3s version from to the BOM's: in the host's namespaces (hostPID,
// privileged, nsenter into pid 1) it saves an etcd snapshot, downloads the binary beside the running one, checks its
// sha256, keeps the running binary as k3s.prev, moves the new one in and restarts k3s. sh -eu ends the script at the
// first failure, so a failed snapshot, download or checksum leaves k3s as it was. The image is the BOM's clusterctl
// image: alpine, whose busybox has nsenter with these flags, and already on the seed (the capi hook pulled it). The
// Job holds no secret: the URL and the sum are public.
func SeedK3sJob(b *BOM, from string) map[string]any {
	const bin = "/usr/local/bin/k3s"
	fetch := k3sFetch(b.Seed.K3s, bin+".new")
	script := strings.Join([]string{
		"k3s etcd-snapshot save --name " + q(k3sSnapshotName(from)),
		fetch[0], fetch[1], fetch[2],
		"cp -p " + bin + " " + bin + ".prev",
		"mv -f " + bin + ".new " + bin,
		"systemctl restart k3s",
	}, "\n")
	return with(object("batch/v1", "Job", SeedPortalNamespace, k3sJobName(b.Seed.K3s.Version)), map[string]any{
		"spec": map[string]any{
			"backoffLimit": 0,
			"template": map[string]any{"spec": map[string]any{
				"hostPID":       true,
				"restartPolicy": "Never",
				"tolerations":   []any{map[string]any{"operator": "Exists"}},
				"containers": []any{map[string]any{
					"name":    "k3s-upgrade",
					"image":   b.Images.Clusterctl,
					"command": []any{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "sh", "-euc", script},
					// The image runs as 65532; the last lines of a failed script become the container's message.
					"securityContext":          map[string]any{"privileged": true, "runAsUser": 0},
					"terminationMessagePolicy": "FallbackToLogsOnError",
				}},
			}},
		},
	})
}

// seedK3s is stage seed-k3s. Every look reads the seed's node: its kubelet version is the k3s that runs, and the stage
// records it (status.seed.k3sVersion). A run that stays on its BOM, and one whose seed already runs the BOM's k3s, is
// skipped. Otherwise the Job is made once and watched: failed ends the run with the script's last lines and the Job
// deleted, so Retry makes a new one; the stage is done when the node reports the BOM's version and is Ready. While
// k3s restarts the seed's API does not answer: that is waited for, not counted as an error.
func (x *acq) seedK3s(ctx context.Context) error {
	seed, err := x.seed()
	if err != nil {
		return err
	}
	away := func(err error) error {
		if refused(err) || startingUp(err) {
			return classify(CodeSeedAPIUnreachable, err)
		}
		return waiting(CodeSeedAPIUnreachable, "The seed's API does not answer, as while k3s restarts: %s", clip(err.Error()))
	}
	nodes, err := seed.ListNodes(ctx)
	if err != nil {
		return away(err)
	}
	if len(nodes) == 0 {
		return waiting(TimeoutCode(stageSeedK3s), "The seed's API lists no node yet.")
	}
	node := nodes[0] // a seed is one k3s server
	live, ready := str(node, "status", "nodeInfo", "kubeletVersion"), condition(node, "Ready") == "True"
	if live != "" && live != x.in.Status.Seed.K3sVersion {
		if err := x.record(ctx, func(_ *Run, in *Installation, _ *Secrets) { in.Status.Seed.K3sVersion = live }); err != nil {
			return err
		}
	}
	if x.from == nil {
		return errSkipped
	}
	target := x.bom.Seed.K3s.Version
	ns, name := SeedPortalNamespace, k3sJobName(target)
	job, err := seed.GetCR(ctx, "batch", "v1", "jobs", ns, name)
	if err != nil {
		return away(err)
	}
	drop := func() error {
		if err := x.check(ctx); err != nil {
			return err
		}
		// The Job's pod goes with it: the Job would otherwise leave it behind.
		pods, err := seed.ListPods(ctx, ns, "job-name="+name)
		if err != nil {
			return away(err)
		}
		for _, p := range pods {
			if err := seed.DeleteCore(ctx, "pods", ns, str(p, "metadata", "name")); err != nil {
				return away(err)
			}
		}
		if err := seed.DeleteCR(ctx, "batch", "v1", "jobs", ns, name); err != nil {
			return away(err)
		}
		return nil
	}
	count := func(field string) bool { n, _ := dig(job, "status", field).(float64); return n >= 1 }
	if live == target {
		switch {
		case job == nil:
			return errSkipped // the seed ran the BOM's k3s before this look: an earlier run's Job, or a hand
		case !ready:
			return waiting(TimeoutCode(stageSeedK3s), "The seed runs k3s %s; its node is not Ready yet.", target)
		case !count("succeeded") && !count("failed"):
			return waiting(TimeoutCode(stageSeedK3s), "The seed runs k3s %s; the Job %s/%s has not reported its end yet.", target, ns, name)
		}
		return drop()
	}
	if why := k3sStep(live, target); why != "" {
		return permanent(CodeSeedK3sRefused, "The seed's node reports k3s %s and the BOM names %s: %s. The record and the site disagree; nothing was changed.",
			live, target, why)
	}
	switch {
	case job == nil:
		if err := x.check(ctx); err != nil {
			return err
		}
		if err := seed.ApplyCR(ctx, "batch", "v1", "jobs", SeedK3sJob(x.bom, live)); err != nil {
			return classify(CodeSeedK3sFailed, err)
		}
		return waiting(TimeoutCode(stageSeedK3s), "The Job %s/%s was created: it saves an etcd snapshot, downloads k3s %s, checks it and restarts k3s.", ns, name, target)
	case count("failed"):
		tail := "the Job's pod left no message"
		if pods, err := seed.ListPods(ctx, ns, "job-name="+name); err == nil && len(pods) > 0 {
			if cs, _ := dig(pods[0], "status", "containerStatuses").([]any); len(cs) > 0 {
				if m, _ := dig(cs[0], "state", "terminated", "message").(string); m != "" {
					r := []rune(oneLine(m)) // the end says what failed
					tail = string(r[max(0, len(r)-400):])
				}
			}
		}
		if err := drop(); err != nil {
			return err
		}
		return permanent(CodeSeedK3sFailed, "The k3s upgrade Job failed before k3s was replaced: %s. The seed runs k3s %s as before. Retry runs it again.",
			tail, live)
	case count("succeeded"):
		return waiting(TimeoutCode(stageSeedK3s), "The Job %s/%s ended and k3s was restarted; the seed's node still reports k3s %s.", ns, name, live)
	}
	return waiting(TimeoutCode(stageSeedK3s), "The Job %s/%s runs; the seed's node reports k3s %s.", ns, name, live)
}

// SeedBackupAge is how old the newest backup of the seed VM may be for an upgrade that replaces the seed's k3s.
const SeedBackupAge = 24 * time.Hour

// SeedBackupCheck is the seed-backup row: ok when a store of the seed's node holds a vzdump archive of the seed VM
// younger than SeedBackupAge, with its time and store; a warning otherwise, never a failure (an apply does not need
// one; an upgrade that replaces the seed's k3s does, or the operator's word: SeedBackupMissing). It reads with the
// portal token, which may not see the store the operator backs up to: the row then says which stores it could not read.
func SeedBackupCheck(ctx context.Context, portal *pve.Client, in *Installation, now time.Time) Check {
	st := in.Status.Seed
	warn := func(format string, a ...any) Check {
		return Check{ID: "seed-backup", Status: CheckWarn, Message: fmt.Sprintf("No backup of seed VM %d younger than 24 hours is visible to the portal: ", st.VMID) +
			fmt.Sprintf(format, a...) + fmt.Sprintf(" The seed holds the Cluster API state of the platform cluster: back it up (vzdump %d) before an upgrade that replaces its k3s.", st.VMID)}
	}
	stores, err := portal.NodeStorages(ctx, st.Node)
	if err != nil {
		return warn("the stores of node %s could not be read (%s).", st.Node, clip(err.Error()))
	}
	var newest pve.StorageContent
	var on string
	var seen, unread []string
	for _, s := range stores {
		if !slices.Contains(strings.Split(s.Content, ","), "backup") {
			continue
		}
		rows, err := portal.StorageContents(ctx, st.Node, s.Storage, "backup", st.VMID)
		if err != nil {
			unread = append(unread, s.Storage)
			continue
		}
		seen = append(seen, s.Storage)
		for _, r := range rows {
			if r.VMID == st.VMID && r.CTime > newest.CTime {
				newest, on = r, s.Storage
			}
		}
	}
	taken := time.Unix(newest.CTime, 0).UTC()
	note := ""
	if len(unread) > 0 {
		note = " The portal token could not read the backups of " + strings.Join(unread, ", ") + "."
	}
	switch {
	case on != "" && now.Sub(taken) < SeedBackupAge:
		return Check{ID: "seed-backup", Status: CheckOK, Message: fmt.Sprintf("Seed VM %d was backed up %s on store %s (%s).", st.VMID, taken.Format("2006-01-02 15:04 UTC"), on, newest.VolID)}
	case on != "":
		return warn("the newest, on store %s, is of %s.%s", on, taken.Format("2006-01-02 15:04 UTC"), note)
	case len(seen) == 0 && len(unread) == 0:
		return warn("node %s shows the portal token no store that takes backups.", st.Node)
	}
	return warn("the stores read (%s) hold none.%s", strings.Join(seen, ", "), note)
}
