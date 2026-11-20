package platformlife

// seedk3s.go — stage seed-k3s: an upgrade run whose BOM names a newer k3s replaces the binary on the running seed and
// restarts k3s, through a privileged Job on the seed itself (the portal has no shell there). The Job runs the first
// boot's own download and checksum lines (k3sFetch) on the host; the install script is not run again, as k3s's manual
// upgrade is the binary and a restart. Never run on a lab: docs/managed-services.md lists what a lab must show.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
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

// seedK3sStale is how long after the Job's end the seed's node may still report the old k3s before the stage takes
// the upgrade as undone.
const seedK3sStale = 5 * time.Minute

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
	// The plan lists the seed row, and the start asks for the backup, only when the two BOMs name another k3s. A seed
	// behind a BOM that does not move k3s was changed by hand: the run does not move it.
	if x.from.Seed.K3s.Version == target {
		return permanent(CodeSeedK3sRefused, "The seed's node reports k3s %s and both BOMs name %s: this upgrade does not replace k3s. The record and the site disagree; nothing was changed.",
			live, target)
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
		// The kubelet reports its version within seconds of its start. A Job that ended long ago on a seed that still
		// reports the old k3s means the previous binary was put back (the hand step): the Job goes, so that Retry
		// makes a new one instead of waiting on this one for ever.
		if done, err := time.Parse(time.RFC3339, str(job, "status", "completionTime")); err == nil && time.Since(done) > seedK3sStale {
			if err := drop(); err != nil {
				return err
			}
			return permanent(CodeSeedK3sFailed, "The k3s upgrade Job ended %s ago and the seed's node still reports k3s %s: the previous binary is in place. The Job was deleted. Retry runs it again.",
				time.Since(done).Round(time.Minute), live)
		}
		return waiting(TimeoutCode(stageSeedK3s), "The Job %s/%s ended and k3s was restarted; the seed's node still reports k3s %s.", ns, name, live)
	}
	return waiting(TimeoutCode(stageSeedK3s), "The Job %s/%s runs; the seed's node reports k3s %s.", ns, name, live)
}

// SeedBackupAge is how old the newest backup of the seed VM may be for an upgrade that replaces the seed's k3s.
const SeedBackupAge = 24 * time.Hour

// SeedBackupCheck is the seed-backup row: ok with the archive's time and store when a store of the seed's node shows
// a vzdump archive of the seed VM younger than SeedBackupAge; never a failure (an apply does not need a backup; an
// upgrade that replaces the seed's k3s does, or the operator's word: SeedBackupMissing).
//
// Proxmox lists a backup only to a token that may read it (PVE::Storage::check_volume_access: Datastore.Allocate on
// the store, or Datastore.AllocateSpace on the store with VM.Backup on the VM), and answers 200 with the rest of the
// list otherwise. The portal token is fenced off the seed VM (NoAccess on its pool) and holds Datastore.Allocate on
// the import store alone; capi (pve.CAPITokenID, nil when not stored) holds no VM.Backup and Datastore.Allocate on the
// ISO store alone. So both read, each store counts as read only when a token holds what its listing needs, and for
// the others the row says that the portal cannot see the seed's backups there: never that none exist. needed says a
// pending change needs the backup (the plan and the start of an upgrade that replaces the seed's k3s): the row is then
// a warning unless an archive was seen, and callers take ok for "seen". Without it (the Check) a store the portal
// cannot see into is no warning: the row is ok and says what the portal cannot know.
//
// An archive counts only when it is of this seed VM, not of an earlier VM that had the same VMID (Proxmox gives a freed
// VMID to the next VM, and a store keeps the old archives): it must have been made after the seed became ready (the
// record's readyAt; without one, the creation time in the VM's config), and its notes, when it has any, must name the
// seed VM (vzdump's default notes are the guest's name).
func SeedBackupCheck(ctx context.Context, portal, capi *pve.Client, in *Installation, now time.Time, needed bool) Check {
	st := in.Status.Seed
	name := SeedVMName(in.ID, st.Generation)
	advice := fmt.Sprintf(" The seed holds the Cluster API state of the platform cluster: back it up (vzdump %d) before an upgrade that replaces its k3s.", st.VMID)
	warn := func(format string, a ...any) Check {
		return Check{ID: "seed-backup", Status: CheckWarn, Message: fmt.Sprintf("No backup of seed VM %d younger than 24 hours is visible to the portal: ", st.VMID) +
			fmt.Sprintf(format, a...) + advice}
	}
	var since int64 // archives made before this are of another VM
	if st.ReadyAt != nil {
		since = st.ReadyAt.Unix()
	}
	var newest pve.StorageContent
	var on string
	var listErr error
	sure := map[string]bool{} // backup store of the node -> a token holds what listing the seed's archives needs
	foreign := 0
	for _, c := range []*pve.Client{capi, portal} {
		if c == nil {
			continue
		}
		if since == 0 { // the portal token may not read the seed VM's config
			if cfg, err := c.GetVMConfig(ctx, st.Node, st.VMID); err == nil {
				if _, ct, ok := strings.Cut(cfg.Str("meta"), "ctime="); ok {
					ct, _, _ = strings.Cut(ct, ",")
					since, _ = strconv.ParseInt(ct, 10, 64)
				}
			}
		}
		stores, err := c.NodeStorages(ctx, st.Node)
		if err != nil {
			listErr = err
			continue
		}
		vm, _ := c.Permissions(ctx, fmt.Sprintf("/vms/%d", st.VMID))
		for _, s := range stores {
			if !slices.Contains(strings.Split(s.Content, ","), "backup") {
				continue
			}
			rows, err := c.StorageContents(ctx, st.Node, s.Storage, "backup", st.VMID)
			p, _ := c.Permissions(ctx, "/storage/"+s.Storage)
			sure[s.Storage] = sure[s.Storage] || (err == nil && (p["Datastore.Allocate"] || (p["Datastore.AllocateSpace"] && vm["VM.Backup"])))
			if err != nil {
				continue
			}
			for _, r := range rows {
				switch {
				case r.VMID != st.VMID:
				case r.CTime < since || (strings.TrimSpace(r.Notes) != "" && !strings.Contains(r.Notes, name)):
					foreign++
				case r.CTime > newest.CTime:
					newest, on = r, s.Storage
				}
			}
		}
	}
	var read, blind []string
	for _, s := range slices.Sorted(maps.Keys(sure)) {
		if sure[s] {
			read = append(read, s)
		} else {
			blind = append(blind, s)
		}
	}
	taken := time.Unix(newest.CTime, 0).UTC()
	note := ""
	if on != "" {
		note = fmt.Sprintf(" The newest it sees, on store %s, is of %s.", on, taken.Format("2006-01-02 15:04 UTC"))
	}
	if foreign > 0 {
		note += fmt.Sprintf(" %d archives of VMID %d were not counted: they were made before this seed VM became ready, or their notes name another VM than %s "+
			"(an earlier VM had the same VMID).", foreign, st.VMID, name)
	}
	switch {
	case on != "" && now.Sub(taken) < SeedBackupAge:
		return Check{ID: "seed-backup", Status: CheckOK, Message: fmt.Sprintf("Seed VM %d was backed up %s on store %s (%s).", st.VMID, taken.Format("2006-01-02 15:04 UTC"), on, newest.VolID)}
	case len(sure) == 0 && listErr != nil:
		return warn("the stores of node %s could not be read (%s).", st.Node, clip(listErr.Error()))
	case len(sure) == 0:
		return warn("node %s shows the portal no store that takes backups.", st.Node)
	case len(blind) > 0:
		c := Check{ID: "seed-backup", Status: CheckOK, Message: fmt.Sprintf("The portal cannot see backups of seed VM %d on %s of node %s, so whether one exists there is not known to it. "+
			"Proxmox lists a backup only to a token that may back up its VM (VM.Backup) or that manages the store (Datastore.Allocate), and by design the portal's tokens hold neither for the seed VM there: "+
			"this is no sign that a backup is missing.", st.VMID, storesText(blind), st.Node)}
		if len(read) > 0 && on == "" {
			c.Message += fmt.Sprintf(" It can see them on %s and sees none there.", storesText(read))
		}
		c.Message += note + " " + SeedBackupToCheck(in)
		if needed {
			c.Status = CheckWarn
		}
		return c
	case on != "":
		return warn("every store that takes backups was read (%s).%s", strings.Join(read, ", "), note)
	}
	return warn("the stores read (%s) hold none.%s", strings.Join(read, ", "), note)
}

func storesText(s []string) string {
	if len(s) == 1 {
		return "store " + s[0]
	}
	return "stores " + strings.Join(s, ", ")
}

// SeedBackupToCheck says what the operator looks for in Proxmox before answering for a backup of the seed VM
// (seedBackupTaken): the portal cannot see the seed's archives on most stores.
func SeedBackupToCheck(in *Installation) string {
	st := in.Status.Seed
	made := "after this seed VM was created"
	if st.ReadyAt != nil {
		made += " (" + st.ReadyAt.UTC().Format("2006-01-02 15:04 UTC") + ")"
	}
	return fmt.Sprintf("Before an upgrade that replaces the seed's k3s, check in Proxmox that there is an archive of this seed VM (%s, VMID %d), made %s and younger than 24 hours "+
		"(vzdump %d makes one), then confirm it when you start the upgrade.", SeedVMName(in.ID, st.Generation), st.VMID, made, st.VMID)
}
