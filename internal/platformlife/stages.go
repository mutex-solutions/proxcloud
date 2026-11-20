package platformlife

// stages.go — stages 1 to 5 of an apply (design §4.2) and the seed part of a remove (§4.6); cluster.go and remove.go
// hold the rest. Each observes first and
// then acts, so a stage resumed after a crash, a lost lease or a Defer continues from what exists.

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
)

const (
	seedRuleComment = "proxcloud-platform seed"
	imagesStall     = 60 * time.Minute // stage 2 fails when no import moved for this long
	templateSettle  = 5 * time.Minute  // how long after an import's last write its template may still be missing from the VM list
)

// validate is stage 1: the preflight on the frozen spec, with the live isolation check.
func (x *acq) validate(ctx context.Context) error {
	checks, err := x.r.Preflight(ctx, x.in)
	if err != nil {
		return classify(CodePreflightFailed, err)
	}
	var items []StageItem
	var bad []Check
	for _, c := range checks {
		items = append(items, StageItem{ID: c.ID, State: c.Status, Message: clip(c.Message)})
		if c.Status == CheckFail {
			bad = append(bad, c)
		}
	}
	if err := x.saveStage(ctx, func(s *Stage) { s.Items = items }); err != nil {
		return err
	}
	switch {
	case len(bad) == 0:
		return x.rollChecks(ctx)
	case len(bad) == 1 && bad[0].ID == "isolation":
		return permanent(CodeIsolationNotEnforced, "%s", clip(bad[0].Message))
	}
	var ids []string
	for _, c := range bad {
		ids = append(ids, c.ID)
	}
	return permanent(CodePreflightFailed, "Failed checks: %s.", strings.Join(ids, ", "))
}

// images is stage 2: the seed image on the seed node and the node image on every node of the cluster, imported through
// Template updates unless a finished import still has its template there.
func (x *acq) images(ctx context.Context) error {
	sp := x.in.Spec
	type want struct {
		id, node string
		img      VMImage
	}
	var wants []want
	if sp.Seed.Mode == SeedVM {
		wants = append(wants, want{"seed:" + sp.Seed.Node, sp.Seed.Node, x.bom.Seed.Image})
	}
	if sp.NodeImage.Mode == ImageBOM {
		var nodes []string
		for _, c := range sp.Clusters {
			nodes = append(nodes, c.ControlPlane.Node)
			for _, g := range c.NodeGroups {
				nodes = append(nodes, g.Nodes...)
			}
		}
		for _, n := range slices.Compact(slices.Sorted(slices.Values(nodes))) {
			wants = append(wants, want{"node:" + n, n, x.bom.Kubernetes.NodeImage})
		}
	}
	if len(wants) == 0 {
		return errSkipped
	}
	caps, err := x.portal.DetectStorage(ctx)
	if err != nil {
		return classify(CodeImageImportFailed, err)
	}
	rows, err := x.portal.ClusterResources(ctx, "vm")
	if err != nil {
		return classify(CodeImageImportFailed, err)
	}
	opts := templateupdates.PlacementOptions(caps, sp.Storage.Import)
	actor := templateupdates.PlatformRunActor + x.run.ID
	items, templates, ours := []StageItem{}, map[string]int{}, []string{}
	for _, w := range wants {
		if !templateupdates.PlacementAllowed(opts, w.node, sp.Storage.Import, sp.Storage.Boot) {
			return permanent(CodeImageImportFailed, "Node %s needs %s active with import content and %s with images content.", w.node, sp.Storage.Import, sp.Storage.Boot)
		}
		note := ""
		state, vmid, err := x.image(ctx, w.node, w.img, rows, actor, &ours, &note)
		if err != nil {
			return err
		}
		items = append(items, StageItem{ID: w.id, State: state, Message: note})
		if vmid > 0 {
			templates[w.id] = vmid
		}
	}
	moved := !slices.Equal(items, x.run.Stages[x.si].Items)
	if err := x.saveStage(ctx, func(s *Stage) {
		if s.Receipt == nil {
			s.Receipt = &StageReceipt{}
		}
		s.Receipt.Templates, s.Receipt.ImportReceipts = templates, slices.Compact(slices.Sorted(slices.Values(ours)))
		s.Items, s.Progress = items, fmt.Sprintf("%d of %d images are templates", len(templates), len(wants))
		if moved { // the stall timer starts again
			now := time.Now().UTC()
			d := now.Add(imagesStall)
			s.LastProgressAt, s.Deadline = &now, &d
		}
	}); err != nil {
		return err
	}
	if len(templates) == len(wants) {
		return nil
	}
	return waiting(CodeImageImportStalled, "%d of %d images are templates; the stage fails when no import moves for an hour.", len(templates), len(wants))
}

// image is one image on one node: done with its template VMID, or the step of the import in flight (note says why an
// item is "checking").
func (x *acq) image(ctx context.Context, node string, img VMImage, rows []pve.ClusterResource, actor string, ours *[]string, note *string) (string, int, error) {
	recs, err := x.r.Imports.ByKey(ctx, x.in.ServiceID, node, img.Digest)
	if err != nil {
		return "", 0, classify(CodeImageImportFailed, err)
	}
	slices.SortFunc(recs, func(a, b templateupdates.Receipt) int { return b.CreatedAt.Compare(a.CreatedAt) })
	pvFor := func(context.Context, string) (templateupdates.LifecyclePVE, error) { return x.portal, nil }
	for _, rec := range recs {
		if rec.RequestedBy == actor {
			*ours = append(*ours, rec.ID)
		}
		switch {
		case rec.TemplateGone:
		case rec.Step == templateupdates.StepTemplated || rec.Step == templateupdates.StepSucceeded:
			if slices.ContainsFunc(rows, func(r pve.ClusterResource) bool {
				return r.VMID == rec.VMID && r.Node == node && r.Template == 1 && r.Pool == pve.TemplatePool
			}) {
				return "done", rec.VMID, nil
			}
			// Its template was deleted: free the receipt (only on proof), then import again.
			// Only its refusal (no proof) is permanent; a read that failed is tried again.
			err := x.r.Imports.MarkTemplateGone(ctx, pvFor, rec.ID)
			if le := (*templateupdates.LifecycleError)(nil); errors.As(err, &le) && le.Code == "template_present" && time.Since(rec.UpdatedAt) < templateSettle {
				// A VM holds the VMID and may be the template: rows is older than the receipt, and Proxmox lists a new
				// template's flag and pool a moment after its task ends. The next look settles it. Only an import that
				// has just written its receipt can be in that window: later the refusal is final (the template was
				// moved, or is out of the token's view) and fails with its text.
				*note = clip(le.Message)
				return "checking", 0, nil
			} else if errors.As(err, &le) {
				return "", 0, permanent(CodeImageImportFailed, "VM %d on %s is no template in %s, and it could not be shown to be gone: %s",
					rec.VMID, node, pve.TemplatePool, clip(err.Error()))
			} else if err != nil {
				return "", 0, classify(CodeImageImportFailed, err)
			}
		case rec.Step == templateupdates.StepFailed || x.jobDead(ctx, rec.ID):
			if rec.RequestedBy == actor {
				return "", 0, permanent(CodeImageImportFailed, "The import of %s on %s failed: %s %s", img.Tag, node, rec.Code, clip(rec.Message))
			}
		default:
			// An import that is tried again shows it: the step alone stands still for as long as the tries take.
			if n, code := x.r.Imports.JobTries(ctx, rec.ID); n > 1 {
				*note = fmt.Sprintf("Try %d of the import; the one before ended with %s. The API log has what Proxmox answered (\"template update attempt failed\").",
					n, cmp.Or(code, "no recorded code"))
			}
			return string(rec.Step), 0, nil
		}
	}
	if err := x.check(ctx); err != nil {
		return "", 0, err
	}
	rec, err := x.r.Imports.EnqueueVM(ctx, templateupdates.VMRequest{Package: img.Package, Digest: img.Digest, ConfirmDigest: img.Digest,
		ServiceID: x.in.ServiceID, Node: node, ImportStorage: x.in.Spec.Storage.Import, DiskStorage: x.in.Spec.Storage.Boot}, actor)
	var refusal *templateupdates.LifecycleError
	switch {
	case errors.Is(err, templateupdates.ErrBlocklisted):
		return "", 0, permanent(CodeImageBlocked, "%s %s is on the template update blocklist: %s.", img.Package, img.Digest, blockedAdvice(x.in))
	case errors.Is(err, templateupdates.ErrDuplicate):
		return string(templateupdates.StepQueued), 0, nil
	case errors.As(err, &refusal) || errors.Is(err, templateupdates.ErrRefused):
		return "", 0, permanent(CodeImageImportFailed, "The import of %s on %s was refused: %s", img.Tag, node, clip(err.Error()))
	case err != nil: // the registry, the network or the database
		return "", 0, transient(CodeImageImportFailed, err)
	}
	*ours = append(*ours, rec.ID)
	if x.r.OnImport != nil {
		x.r.OnImport(rec, actor)
	}
	return string(rec.Step), 0, nil
}

func (x *acq) jobDead(ctx context.Context, id string) bool {
	state, err := x.r.Imports.JobState(ctx, id)
	return err == nil && state == "dead"
}

// vmState is what observe saw at a VMID. Ours means in pool proxcloud-seed with exactly the generation's name; tags
// are not part of it, because a clone carries its template's.
type vmState struct {
	absent, foreign, locked, running bool
	node                             string
	cfg                              pve.VMConfig
}

// poolNode is the node of vmid when it is a member of proxcloud-seed ("" when it is not).
func (x *acq) poolNode(ctx context.Context, vmid int) (string, error) {
	pool, err := x.capi.GetPool(ctx, pve.SeedPool)
	if err != nil {
		return "", classify(CodeSeedCloneFailed, err)
	}
	for i := 0; pool != nil && i < len(pool.Members); i++ {
		if pool.Members[i].VMID == vmid {
			return pool.Members[i].Node, nil
		}
	}
	return "", nil
}

func (x *acq) observe(ctx context.Context, vmid, gen int) (vmState, error) {
	node, err := x.poolNode(ctx, vmid)
	if err != nil {
		return vmState{}, err
	}
	if node == "" {
		free, err := x.capi.VMIDFree(ctx, vmid)
		if err != nil || free {
			return vmState{absent: free}, classify(CodeSeedCloneFailed, err)
		}
		// PVE adds a clone to its pool only when the clone ends: while one of the seed template runs on the seed's node
		// (any clone there when the template is not known), the held VMID may still become ours.
		busy, err := x.capi.TaskRunning(ctx, x.in.Spec.Seed.Node, x.seedTemplate(), "qmclone")
		if err != nil {
			return vmState{}, classify(CodeSeedCloneFailed, err)
		}
		if busy {
			return vmState{locked: true}, nil
		}
		// Our clone may have ended, and joined the pool, between the pool read and the task read: only a pool read
		// taken after the task read makes the VMID foreign.
		if node, err = x.poolNode(ctx, vmid); err != nil || node == "" {
			return vmState{foreign: err == nil}, err
		}
	}
	cfg, err := x.capi.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return vmState{}, classify(CodeSeedCloneFailed, err)
	}
	if cfg.Str("name") != SeedVMName(x.in.ID, gen) {
		return vmState{foreign: true}, nil
	}
	st, err := x.capi.GetVMStatus(ctx, node, vmid)
	if err != nil {
		return vmState{}, classify(CodeSeedCloneFailed, err)
	}
	return vmState{node: node, cfg: cfg, locked: cfg.Str("lock") != "", running: st.Status == "running"}, nil
}

// seedVM is stage 3: clone, configure and boot the seed with its CD, behind its VM firewall.
func (x *acq) seedVM(ctx context.Context) error {
	if x.in.Spec.Seed.Mode != SeedVM {
		return permanent(CodeStageUnavailable, "A seed of mode existing is adopted in a later release; this build builds seed VMs only.")
	}
	if err := x.needCAPI(); err != nil {
		return err
	}
	// At every apply, and below before the seed is cloned: the rule that lets the platform network reach the Proxmox API.
	if x.sec.SeedKubeconfig != nil {
		if err := x.ensureAPIAccess(ctx); err != nil {
			return err
		}
		return x.readySeed(ctx)
	}
	// A routed platform network is made before the first VM is put on it. An installed platform never comes here.
	if err := x.ensureNetwork(ctx); err != nil {
		return err
	}
	if err := x.ensureAPIAccess(ctx); err != nil {
		return err
	}
	if x.receipt().VMID == 0 {
		gen := 1
		if st := x.in.Status.Seed; st != nil && st.Generation > 0 {
			// A Retry: the seed an earlier run built never became ready, so the next generation replaces it.
			gen = st.Generation + 1
			if err := x.deleteSeed(ctx, st.VMID, st.Generation); err != nil {
				return err
			}
		}
		if err := x.intent(ctx, gen); err != nil {
			return err
		}
	}
	for turn := 0; ; turn++ {
		rc := x.receipt()
		if turn == 4 { // a VM that does not settle is observed again later, not cloned in a tight loop
			return waiting(CodeSeedCloneFailed, "VM %d did not settle after its clone; it is observed again.", rc.VMID)
		}
		if rc.CloneUPID != "" {
			if err := x.cloneTask(ctx, pve.UPID(rc.CloneUPID)); err != nil {
				return err
			}
		}
		vm, err := x.observe(ctx, rc.VMID, rc.Generation)
		switch {
		case err != nil:
			return err
		case vm.foreign || vm.absent:
			// A CD fits one clone only (its network config names that clone's MAC, and a clone is configured where its
			// CD is made), so a generation that has its CA never gets another clone: the next generation does.
			st := x.in.Status.Seed
			switch {
			case st != nil && st.Generation == rc.Generation && st.CACert != "":
				err = x.intent(ctx, rc.Generation+1)
			case vm.foreign: // someone else's VM holds the VMID: a new VMID; nothing is deleted
				err = x.intent(ctx, rc.Generation)
			default:
				err = x.clone(ctx, rc)
			}
			if err != nil {
				return err
			}
			continue
		case vm.locked:
			return waiting(CodeSeedCloneFailed, "VM %d is locked (%s): waiting for its Proxmox task.", rc.VMID, vm.cfg.Str("lock"))
		}
		cd := SeedCDName(x.in.ID, rc.Generation)
		if !vm.running && !mentions(vm.cfg, cd) {
			vols, err := x.cds(ctx, vm.node)
			if err != nil {
				return classify(CodeSeedConfigRefused, err)
			}
			vol := x.in.Spec.Storage.ISO + ":iso/" + cd
			if i := slices.IndexFunc(vols, func(v string) bool { return path.Base(v) == cd }); i >= 0 {
				vol = vols[i] // where it was uploaded, should the spec's ISO store have changed since
			} else {
				if st := x.in.Status.Seed; st != nil && st.Generation == rc.Generation && st.CACert != "" {
					// The CA key lived in the process that made it and the CD never reached the store: next generation.
					if err := x.deleteSeed(ctx, rc.VMID, rc.Generation); err != nil {
						return err
					}
					if err := x.intent(ctx, rc.Generation+1); err != nil {
						return err
					}
					continue
				}
				if err := x.uploadCD(ctx, rc, vm); err != nil {
					return err
				}
			}
			if err := x.attach(ctx, vm, rc.VMID, vol); err != nil {
				return err
			}
			if x.receipt().CDVolume != vol { // uploaded by an acquisition that lost its lease before recording it
				if err := x.recordReceipt(ctx, func(r *StageReceipt, in *Installation, _ *Secrets) { r.CDVolume, in.Status.Seed.CDVolume = vol, vol }); err != nil {
					return err
				}
			}
		}
		return x.boot(ctx, vm, rc.VMID, rc.Generation)
	}
}

func (x *acq) needCAPI() error {
	if x.capi == nil {
		return permanent(CodeCredentialsMissing, "The token %s is not stored: paste it (PUT …/credentials), then Retry.", pve.CAPITokenID)
	}
	return nil
}

// intent records the VM a generation will be: a fresh VMID, the generation's name, and the seed status pointing at it.
func (x *acq) intent(ctx context.Context, gen int) error {
	if err := x.check(ctx); err != nil {
		return err
	}
	vmid, err := x.capi.NextID(ctx)
	if err != nil {
		return classify(CodeSeedCloneFailed, err)
	}
	sp := x.in.Spec.Seed
	if old := x.in.Status.Seed; old != nil && old.CDVolume != "" {
		// The status is the only record of the store a CD went to: the CD goes before the status stops naming it.
		if err := x.purgeCDs(ctx, old.Node); err != nil {
			return err
		}
	}
	return x.saveReceipt(ctx, func(r *StageReceipt, in *Installation, _ *Secrets) {
		*r = StageReceipt{VMID: vmid, Name: SeedVMName(in.ID, gen), Generation: gen}
		in.Status.Seed = &SeedStatus{VMID: vmid, Node: sp.Node, Generation: gen, APIServer: x.r.seedServer(sp.Address)}
	})
}

func (x *acq) cloneTask(ctx context.Context, up pve.UPID) error {
	st, err := x.capi.TaskStatus(ctx, up)
	switch {
	case err != nil:
		return classify(CodeSeedCloneFailed, err)
	case st.Status != "stopped":
		return waiting(CodeSeedCloneFailed, "The clone of the seed is running (task %s).", up)
	case st.ExitStatus != "OK":
		return permanent(CodeSeedCloneFailed, "The clone failed: %s", x.tail(ctx, up, st.ExitStatus))
	}
	return nil
}

// seedTemplate is the seed image's template on the seed node, as stage 2 recorded it (0 = not known).
func (x *acq) seedTemplate() int {
	for _, s := range x.run.Stages {
		if s.ID == StageImages && s.Receipt != nil {
			return s.Receipt.Templates["seed:"+x.in.Spec.Seed.Node]
		}
	}
	return 0
}

func (x *acq) clone(ctx context.Context, rc StageReceipt) error {
	tmpl := x.seedTemplate()
	if tmpl == 0 {
		return permanent(CodeSeedCloneFailed, "Stage %s recorded no seed template on %s.", StageImages, x.in.Spec.Seed.Node)
	}
	if err := x.check(ctx); err != nil {
		return err
	}
	up, err := x.capi.CloneVM(ctx, x.in.Spec.Seed.Node, tmpl, pve.CloneOpts{NewID: rc.VMID, Name: rc.Name, Pool: pve.SeedPool,
		Storage: x.in.Spec.Storage.Boot, Full: true})
	if err != nil {
		return classify(CodeSeedCloneFailed, err)
	}
	if err := x.recordReceipt(ctx, func(r *StageReceipt, _ *Installation, _ *Secrets) { r.CloneUPID = string(up) }); err != nil {
		return err
	}
	return x.task(ctx, up, CodeSeedCloneFailed)
}

// uploadCD configures the clone, renders its CD, stores the generation's secrets (committed before the upload, with the
// volume the CD goes to: its CA is what stage 4 pins, its tokens what stages 4 and 5 send; the CA key stays in memory),
// and uploads the CD. What cannot be rendered or built fails before anything is stored.
func (x *acq) uploadCD(ctx context.Context, rc StageReceipt, vm vmState) error {
	sp := x.in.Spec
	if err := x.pveTask(ctx, CodeSeedConfigRefused, func() (pve.UPID, error) {
		return x.capi.SetVMConfig(ctx, vm.node, rc.VMID, map[string]string{"cores": strconv.Itoa(sp.Seed.Cores),
			"memory": strconv.Itoa(sp.Seed.MemoryMiB), "agent": "1", "onboot": "1",
			"tags": "proxcloud;pc-platform;pc-platform-" + x.in.ID, "net0": withNet0(vm.cfg.Str("net0"), sp.Network.Bridge, sp.Network.VLAN, sp.Network.MTU)})
	}); err != nil {
		return err
	}
	// A template made by Template updates has no NIC, so Proxmox gives net0 its MAC address in the call above: the CD
	// names the one read back. A later acquisition reads it in observe and withNet0 sends it again, so it never changes.
	set, err := x.capi.GetVMConfig(ctx, vm.node, rc.VMID)
	if err != nil {
		return classify(CodeSeedConfigRefused, err)
	}
	if disk, size := bootDisk(vm.cfg); disk != "" && size < sp.Seed.DiskGiB {
		if err := x.pveTask(ctx, CodeSeedConfigRefused, func() (pve.UPID, error) {
			return x.capi.ResizeDisk(ctx, vm.node, rc.VMID, disk, strconv.Itoa(sp.Seed.DiskGiB)+"G")
		}); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	secrets, err := NewSeedSecrets(x.in.ID, rc.Generation, now)
	if err != nil {
		return err
	}
	prefix, _ := netip.ParsePrefix(sp.Network.Prefix)
	files, err := SeedFiles(x.bom, RenderInput{ID: x.in.ID, SeedAddress: sp.Seed.Address, SSHKeys: sp.Seed.SSHKeys,
		Network: RenderNetwork{Bridge: sp.Network.Bridge, VLAN: sp.Network.VLAN, Gateway: sp.Network.Gateway, Prefix: prefix.Bits(), DNS: sp.Network.DNS, MTU: sp.Network.MTU}},
		SeedBoot{Generation: rc.Generation, Net0: set.Str("net0"), Secrets: secrets, Now: now})
	if err != nil {
		return permanent(CodeSeedConfigRefused, "The seed's CD could not be rendered: %s", clip(err.Error()))
	}
	iso, err := pve.BuildSeedISO(files.Files())
	if err != nil {
		return permanent(CodeSeedConfigRefused, "The seed's CD could not be built: %s", clip(err.Error()))
	}
	name := SeedCDName(x.in.ID, rc.Generation)
	vol := sp.Storage.ISO + ":iso/" + name
	if err := x.save(ctx, func(_ *Run, in *Installation, sec *Secrets) {
		in.Status.Seed.CACert, in.Status.Seed.CDVolume = secrets.CACert, vol
		sec.K3sToken, sec.BootstrapToken = secrets.K3sToken, secrets.BootstrapToken
	}); err != nil {
		return err
	}
	if err := x.pveTask(ctx, CodeSeedConfigRefused, func() (pve.UPID, error) {
		return x.capi.UploadISO(ctx, vm.node, sp.Storage.ISO, name, iso)
	}); err != nil {
		return err
	}
	return x.recordReceipt(ctx, func(r *StageReceipt, in *Installation, _ *Secrets) { r.CDVolume, in.Status.Seed.CDVolume = vol, vol })
}

// attach puts the CD in the clone's cloud-init slot (its PVE drive is dropped first: two cidata CDs would make the pick
// arbitrary) or ide2, and protects the VM in the same call: Proxmox removes no drive from a protected VM, so the
// protection comes after the drop, and a VM that has its CD is protected.
var cdSlots = []string{"ide0", "ide1", "ide2", "ide3", "scsi1", "sata0"}

func (x *acq) attach(ctx context.Context, vm vmState, vmid int, vol string) error {
	slot := "ide2"
	for _, s := range cdSlots {
		if v := vm.cfg.Str(s); strings.Contains(v, "cloudinit") || strings.Contains(v, "iso/pcplat-") {
			slot = s
			break
		}
	}
	if strings.Contains(vm.cfg.Str(slot), "cloudinit") {
		if err := x.pveTask(ctx, CodeSeedConfigRefused, func() (pve.UPID, error) {
			return x.capi.SetVMConfig(ctx, vm.node, vmid, nil, slot)
		}); err != nil {
			return err
		}
	}
	return x.pveTask(ctx, CodeSeedConfigRefused, func() (pve.UPID, error) {
		return x.capi.SetVMConfig(ctx, vm.node, vmid, map[string]string{slot: vol + ",media=cdrom", "protection": "1"})
	})
}

// boot deletes the CDs of other generations, writes the VM firewall and starts the seed.
func (x *acq) boot(ctx context.Context, vm vmState, vmid, gen int) error {
	if err := x.dropCDs(ctx, vm.node, SeedCDName(x.in.ID, gen)); err != nil {
		return err
	}
	if err := x.firewall(ctx, vm.node, vmid); err != nil {
		return err
	}
	if vm.running {
		return nil
	}
	return x.pveTask(ctx, CodeSeedConfigRefused, func() (pve.UPID, error) { return x.capi.StartVM(ctx, vm.node, vmid) })
}

// readySeed is stage 3 once the seed's identity is stored: the seed is never deleted or rebuilt automatically, only
// its firewall re-applied and the VM started.
func (x *acq) readySeed(ctx context.Context) error {
	st := x.in.Status.Seed
	if st == nil {
		return permanent(CodeSeedLost, "The installation holds a seed credential but no seed.")
	}
	vm, err := x.observe(ctx, st.VMID, st.Generation)
	if err != nil {
		return err
	}
	if vm.absent || vm.foreign {
		return permanent(CodeSeedLost, "The seed VM %d is gone or no longer named %s; a seed that became ready is not rebuilt automatically.",
			st.VMID, SeedVMName(x.in.ID, st.Generation))
	}
	return x.boot(ctx, vm, st.VMID, st.Generation)
}

// firewall enables the seed's VM firewall with policy DROP and ACCEPT rules for 6443 (and 22 with SSH keys) from each
// API source; rules of ours that are no longer wanted are deleted.
func (x *acq) firewall(ctx context.Context, node string, vmid int) error {
	if err := x.check(ctx); err != nil {
		return err
	}
	// Written only when it differs: a PUT of the same options rewrites the VM's firewall file, which a run whose plan
	// lists nothing for the site must not do. An options read that fails is no reason to skip the write.
	if opts, err := x.capi.GetVMFirewallOptions(ctx, node, vmid); err != nil || fmt.Sprint(opts["enable"]) != "1" || fmt.Sprint(opts["policy_in"]) != "DROP" {
		if err := x.capi.SetVMFirewallOptions(ctx, node, vmid, map[string]string{"enable": "1", "policy_in": "DROP"}); err != nil {
			return classify(CodeSeedConfigRefused, err)
		}
	}
	ports := []string{"6443"}
	if len(x.in.Spec.Seed.SSHKeys) > 0 {
		ports = append(ports, "22")
	}
	var want []pve.FirewallRule
	for _, src := range x.in.Spec.Seed.APISources {
		for _, p := range ports {
			want = append(want, pve.FirewallRule{Type: "in", Action: "ACCEPT", Proto: "tcp", Source: src, DPort: p, Comment: seedRuleComment})
		}
	}
	same := func(a pve.FirewallRule) func(pve.FirewallRule) bool {
		return func(b pve.FirewallRule) bool {
			return a.Type == b.Type && a.Action == b.Action && a.Proto == b.Proto && a.Source == b.Source && a.DPort == b.DPort
		}
	}
	for {
		rules, err := x.capi.ListVMFirewallRules(ctx, node, vmid)
		if err != nil {
			return classify(CodeSeedConfigRefused, err)
		}
		stale := -1
		for i, r := range rules {
			if r.Comment == seedRuleComment && !slices.ContainsFunc(want, same(r)) && (stale < 0 || r.Pos > rules[stale].Pos) {
				stale = i
			}
		}
		if stale < 0 {
			for _, w := range want {
				if !slices.ContainsFunc(rules, same(w)) {
					if err := x.capi.AddVMFirewallRule(ctx, node, vmid, w); err != nil {
						return classify(CodeSeedConfigRefused, err)
					}
				}
			}
			return nil
		}
		if err := x.capi.DeleteVMFirewallRule(ctx, node, vmid, rules[stale].Pos, rules[stale].Digest); err != nil {
			return classify(CodeSeedConfigRefused, err)
		}
	}
}

// seedClient is a client of the seed API with TLS pinned to the seed's CA: the token goes out only on a connection
// whose certificate that CA signed.
func seedClient(st *SeedStatus, token string) (*kamajik8s.Client, error) {
	kc, err := kamajik8s.BuildKubeconfig("pc-seed", st.APIServer, []byte(st.CACert), token)
	if err != nil {
		return nil, permanent(CodeSeedIdentityLost, "The seed's CA or address is not usable: %s", clip(err.Error()))
	}
	return kamajik8s.New(kc)
}

// seedAPI is stage 4: wait until the seed's API answers /readyz over the pinned TLS.
func (x *acq) seedAPI(ctx context.Context) error {
	st, token := x.in.Status.Seed, x.sec.BootstrapToken
	if k := x.sec.SeedKubeconfig; k != nil {
		token = k.Token
	}
	if st == nil || st.CACert == "" || token == "" {
		return permanent(CodeSeedIdentityLost, "The run holds no credential for the seed: Retry rebuilds a seed that never became ready.")
	}
	if err := x.needCAPI(); err != nil {
		return err
	}
	c, err := seedClient(st, token)
	if err != nil {
		return err
	}
	err = c.Readyz(ctx)
	if err == nil {
		return nil
	}
	if tlsMismatch(err) {
		return permanent(CodeSeedTLSMismatch, "The server at %s does not present a certificate of the seed's CA: another server answers on that address.", st.APIServer)
	}
	if err := x.check(ctx); err != nil {
		return err
	}
	if perr := x.capi.AgentPing(ctx, st.Node, st.VMID); perr != nil {
		return waiting(CodeSeedNotBooted, "The seed's guest agent does not answer yet (VM %d).", st.VMID)
	}
	return waiting(CodeSeedAPIUnreachable, "The seed is booted but %s/readyz is not ready yet: %s", st.APIServer, clip(err.Error()))
}

func tlsMismatch(err error) bool {
	var cv *tls.CertificateVerificationError
	return errors.As(err, &cv)
}

// refused reports a Kubernetes refusal (4xx but 409 and 429).
func refused(err error) bool {
	var ka *kamajik8s.APIError
	return errors.As(err, &ka) && ka.Status >= 400 && ka.Status < 500 && ka.Status != http.StatusConflict && ka.Status != http.StatusTooManyRequests
}

// startingUp reports a Kubernetes 401 or 403: what a cluster answers to a credential it refuses, and also what its API
// server answers to a good one for some seconds after it starts, while its caches fill (seen on a lab after the
// seed's k3s restarted under disk load). At v1.35:
//   - a 401, the bare "Unauthorized" of every failed authentication (apiserver/pkg/endpoints/filters/
//     authentication.go), also for a service account token whose Secret the server cannot read yet;
//   - a 403 of the RBAC authorizer, with "RBAC: [clusterrole … not found]" when it has read the subject's bindings
//     and not the roles (plugin/pkg/auth/authorizer/rbac/rbac.go), and with no reason when it has read neither;
//   - a 403 "not yet ready to handle request" of an admission plugin whose informer has not synced in 10 s
//     (apiserver/pkg/admission/plugin/namespace/lifecycle: every write of a namespaced object).
//
// No wording tells the start from a revoked token, a deleted role or binding, a quota or a namespace that is being
// deleted, and /readyz does not either: it answers the same 401 to a token the server does not know (k3s allows no
// anonymous request). So the stage counts the looks on end that end so (Stage.Refused): the first refusedLooks-1 are
// passing errors, the next ends the run with the stage's code and the server's text. With the delays of a retry
// that is 3 min 45 s or more, where a start takes seconds.
func startingUp(err error) bool {
	var ka *kamajik8s.APIError
	return errors.As(err, &ka) && (ka.Status == http.StatusUnauthorized || ka.Status == http.StatusForbidden)
}

const (
	refusedLooks   = 5
	startingUpNote = "The cluster's API refuses the run's credential, as an API server does for some seconds after it starts. "
)

// seedIdentity is stage 5: read the portal's service account token with the bootstrap token, store it, delete the
// bootstrap objects with it, then eject and delete the CD. Done only when the running seed's config no longer names
// the CD and the ISO store no longer lists it.
func (x *acq) seedIdentity(ctx context.Context) error {
	st := x.in.Status.Seed
	if st == nil || st.VMID == 0 || (x.sec.SeedKubeconfig == nil && len(x.sec.BootstrapToken) < 6) {
		return permanent(CodeSeedIdentityLost, "The run holds no credential for the seed: Retry rebuilds a seed that never became ready.")
	}
	if err := x.needCAPI(); err != nil {
		return err
	}
	if x.sec.SeedKubeconfig == nil {
		boot, err := seedClient(st, x.sec.BootstrapToken)
		if err != nil {
			return err
		}
		data, err := boot.GetSecretData(ctx, SeedPortalNamespace, SeedPortalTokenSecret)
		var ka *kamajik8s.APIError
		if errors.As(err, &ka) && ka.Status == http.StatusUnauthorized {
			// Not final at once: a seed that has just started knows no bootstrap token until it has read its Secrets
			// (plugin/pkg/auth/authenticator/token/bootstrap reads them from a cache only).
			return &stageErr{kindRefused, CodeSeedIdentityLost, fmt.Sprintf("The seed refuses the bootstrap token, which expires %s after the seed was built: "+
				"Retry rebuilds the seed.", SeedBootstrapTTL)}
		}
		if errors.As(err, &ka) && ka.Status == http.StatusForbidden {
			// The seed applies the token before the Role and RoleBinding that let it read the Secret.
			return waiting(CodeSeedIdentityLost, "The seed does not let the bootstrap token read %s/%s yet.", SeedPortalNamespace, SeedPortalTokenSecret)
		}
		if err != nil {
			return classify(CodeSeedIdentityLost, err)
		}
		token := string(data["token"])
		if token == "" {
			return waiting(CodeSeedIdentityLost, "The seed has not filled %s/%s with a token yet.", SeedPortalNamespace, SeedPortalTokenSecret)
		}
		sa, err := seedClient(st, token)
		if err != nil {
			return err
		}
		admin, err := sa.CanI(ctx, kamajik8s.AccessCheck{Verb: "*", Group: "*", Resource: "*"})
		if err != nil {
			return classify(CodeSeedIdentityLost, err)
		}
		if !admin {
			// Waited for: a seed that has just started denies every review until its RBAC caches are filled.
			return waiting(CodeSeedIdentityLost, "The seed does not report its service account %s as cluster-admin (yet).", SeedPortalAccount)
		}
		if err := x.record(ctx, func(_ *Run, in *Installation, sec *Secrets) {
			now := time.Now().UTC()
			sec.SeedKubeconfig = &SeedKubeconfig{Server: st.APIServer, CA: st.CACert, Token: token}
			in.Status.Seed.TokenSecret, in.Status.Seed.ReadyAt = SeedPortalTokenSecret, &now
		}); err != nil {
			return err
		}
	}
	if boot := x.sec.BootstrapToken; len(boot) >= 6 {
		sa, err := seedClient(st, x.sec.SeedKubeconfig.Token)
		if err != nil {
			return err
		}
		const rbac = "rbac.authorization.k8s.io"
		for _, del := range []func() error{
			func() error { return sa.DeleteSecret(ctx, "kube-system", "bootstrap-token-"+boot[:6]) },
			func() error {
				return sa.DeleteCR(ctx, rbac, "v1", "rolebindings", SeedPortalNamespace, SeedBootstrapRBAC)
			},
			func() error { return sa.DeleteCR(ctx, rbac, "v1", "roles", SeedPortalNamespace, SeedBootstrapRBAC) },
		} {
			if err := del(); err != nil {
				return classify(CodeSeedIdentityLost, err)
			}
		}
	}
	for ejected := false; ; ejected = true {
		// The values in effect, not the pending ones: a task that ends OK with the eject left pending would leave the
		// CD (CA key, tokens) readable in the guest while its file is deleted and the stage calls it gone.
		cfg, err := x.capi.GetVMConfigCurrent(ctx, st.Node, st.VMID)
		if err != nil {
			return transient(CodeSeedCDNotDeleted, err)
		}
		held := slices.DeleteFunc(slices.Clone(cdSlots), func(slot string) bool { return !strings.Contains(cfg.Str(slot), "iso/pcplat-"+x.in.ID+"-") })
		if len(held) == 0 {
			break
		}
		if ejected {
			return waiting(CodeSeedCDNotDeleted, "Proxmox took the eject of %s on VM %d but the running seed still has the CD: the change is pending (qm pending %d).",
				strings.Join(held, ", "), st.VMID, st.VMID)
		}
		for _, slot := range held {
			// Ejected, not removed: the seed is protected and running, and Proxmox removes no drive from a protected VM
			// (and an IDE drive of a running one only at its next start), while it changes a CD at once.
			if err := x.pveTask(ctx, CodeSeedCDNotDeleted, func() (pve.UPID, error) {
				return x.capi.SetVMConfig(ctx, st.Node, st.VMID, map[string]string{slot: "none,media=cdrom"})
			}); err != nil {
				return asTransient(err)
			}
		}
	}
	if err := x.purgeCDs(ctx, st.Node); err != nil {
		return err
	}
	return x.record(ctx, func(_ *Run, in *Installation, sec *Secrets) { sec.BootstrapToken, in.Status.Seed.CDVolume = "", "" })
}

// asTransient makes a permanent stage error transient: a refusal the stage budget bounds (seed_cd_not_deleted).
func asTransient(err error) error {
	var se *stageErr
	if errors.As(err, &se) && se.kind == kindFail {
		return &stageErr{kindRetry, se.code, se.msg}
	}
	return err
}

// removeSeed deletes the seed VM and every pcplat-<id>-* CD.
func (x *acq) removeSeed(ctx context.Context) error {
	st := x.in.Status.Seed
	if x.capi == nil {
		if st != nil {
			return permanent(CodeCredentialsMissing, "The token %s is not stored, so the seed VM %d cannot be deleted: paste it, then Remove again.", pve.CAPITokenID, st.VMID)
		}
		// No seed was ever recorded: only a routed network may be left, and the portal token removes it.
		var items []StageItem
		for _, remove := range []func(context.Context) (*StageItem, error){x.removeAPIAccess, x.removeNetwork} {
			it, err := remove(ctx)
			if err != nil {
				return err
			}
			if it != nil {
				items = append(items, *it)
			}
		}
		if len(items) == 0 {
			return errSkipped
		}
		return x.note(ctx, items...)
	}
	node := x.in.Spec.Seed.Node
	if st != nil {
		if err := x.deleteSeed(ctx, st.VMID, st.Generation); err != nil {
			return err
		}
		node = st.Node
	}
	if err := x.purgeCDs(ctx, node); err != nil {
		return err
	}
	// What the finished run shows: the seed VM as it is now, and that no CD is left.
	items := []StageItem{{ID: "cds", State: "gone", Message: fmt.Sprintf("No CD pcplat-%s-* is on the ISO store of %s.", x.in.ID, node)}}
	if st != nil && st.VMID != 0 {
		vm, err := x.observe(ctx, st.VMID, st.Generation)
		if err != nil {
			return err
		}
		// "gone", not "deleted": an earlier Remove may have deleted it, and this one only found it absent.
		seed := StageItem{ID: fmt.Sprintf("vm-%d", st.VMID), State: "gone", Message: fmt.Sprintf("Proxmox no longer has the seed VM %d (%s) of %s: it was deleted with its disk and its firewall rules.",
			st.VMID, SeedVMName(x.in.ID, st.Generation), st.Node)}
		if !vm.absent {
			seed.State, seed.Message = "left", fmt.Sprintf("The VM at %d is not this installation's seed (%s): not deleted.", st.VMID, SeedVMName(x.in.ID, st.Generation))
		}
		items = append(items, seed)
	}
	// After the last VM that used them: the platform machines went in the stage before.
	for _, remove := range []func(context.Context) (*StageItem, error){x.removeAPIAccess, x.removeNetwork} {
		it, err := remove(ctx)
		if err != nil {
			return err
		}
		if it != nil {
			items = append(items, *it)
		}
	}
	return x.note(ctx, items...)
}

// removeSecrets wipes platformSecret and what the status says was built.
func (x *acq) removeSecrets(ctx context.Context) error {
	return x.save(ctx, func(run *Run, in *Installation, sec *Secrets) {
		*sec = Secrets{ID: sec.ID}
		in.Status = Status{Phase: in.Status.Phase, Providers: in.Status.Providers} // the rows stay, DISABLED
		run.Stages[x.si].Items = []StageItem{{ID: "secrets", State: "deleted", Message: "The stored secrets of both tokens, the seed's credential and tokens, the recorded status and the last tenant path test. " +
			"The tokens themselves still exist in Proxmox, and the provider rows stay, disabled."}}
	})
}

// deleteSeed deletes the seed VM at vmid if it is ours (vmid, pool, exact name of gen) and no tag names another
// installation: protection off, stop, delete. Nothing of ours there is success.
func (x *acq) deleteSeed(ctx context.Context, vmid, gen int) error {
	if vmid == 0 {
		return nil
	}
	vm, err := x.observe(ctx, vmid, gen)
	if err != nil || vm.absent || vm.foreign {
		return err
	}
	for _, t := range vmTags(vm.cfg) {
		if strings.HasPrefix(t, "pc-platform-") && t != "pc-platform-"+x.in.ID {
			return permanent(CodeSeedConfigRefused, "VM %d carries this installation's seed name but the tag %s: it is not deleted.", vmid, t)
		}
	}
	if vm.locked {
		return waiting(CodeSeedConfigRefused, "VM %d is locked (%s): waiting before it is deleted.", vmid, vm.cfg.Str("lock"))
	}
	steps := []func() (pve.UPID, error){
		func() (pve.UPID, error) {
			return x.capi.SetVMConfig(ctx, vm.node, vmid, map[string]string{"protection": "0"})
		},
		func() (pve.UPID, error) { return x.capi.StopVM(ctx, vm.node, vmid) },
		func() (pve.UPID, error) { return x.capi.DeleteVM(ctx, vm.node, vmid) },
	}
	if !vm.running {
		steps = slices.Delete(steps, 1, 2)
	}
	for _, step := range steps {
		if err := x.pveTask(ctx, CodeSeedConfigRefused, step); err != nil {
			return err
		}
	}
	return nil
}

func vmTags(cfg pve.VMConfig) []string {
	return strings.FieldsFunc(cfg.Str("tags"), func(r rune) bool { return r == ';' || r == ',' || r == ' ' })
}

// cds lists this installation's CDs (volids) on the spec's ISO store and on the store the seed status says its CD
// went to: the spec's may have changed since the upload.
func (x *acq) cds(ctx context.Context, node string) ([]string, error) {
	stores := []string{x.in.Spec.Storage.ISO}
	if st := x.in.Status.Seed; st != nil {
		if s, _, ok := strings.Cut(st.CDVolume, ":"); ok && s != stores[0] {
			stores = append(stores, s)
		}
	}
	var out []string
	for _, iso := range stores {
		all, err := x.capi.StorageContents(ctx, node, iso, "iso", 0)
		if err != nil {
			return nil, err
		}
		for _, c := range all {
			if strings.HasPrefix(c.VolID, iso+":iso/pcplat-"+x.in.ID+"-") {
				out = append(out, c.VolID)
			}
		}
	}
	return out, nil
}

// purgeCDs deletes every CD of this installation and proves it: no store that may hold one lists one any more.
func (x *acq) purgeCDs(ctx context.Context, node string) error {
	if err := x.dropCDs(ctx, node, ""); err != nil {
		return err
	}
	left, err := x.cds(ctx, node)
	if err != nil {
		return transient(CodeSeedCDNotDeleted, err)
	}
	if len(left) > 0 {
		return waiting(CodeSeedCDNotDeleted, "Proxmox still lists %s.", strings.Join(left, ", "))
	}
	return nil
}

// dropCDs deletes this installation's CDs but keep (a file name; "" deletes all), by name.
func (x *acq) dropCDs(ctx context.Context, node, keep string) error {
	vols, err := x.cds(ctx, node)
	if err != nil {
		return transient(CodeSeedCDNotDeleted, err)
	}
	for _, v := range vols {
		if path.Base(v) == keep {
			continue
		}
		if err := x.check(ctx); err != nil {
			return err
		}
		iso, rel, _ := strings.Cut(v, ":")
		if err := x.capi.DeleteStorageVolume(ctx, node, iso, rel); err != nil {
			return transient(CodeSeedCDNotDeleted, err)
		}
	}
	return nil
}

// pveTask checks the lease and the cancel, makes one Proxmox call and follows its task for a while: a refused or failed
// task is code, one still running is waited for (the stage re-observes).
func (x *acq) pveTask(ctx context.Context, code string, call func() (pve.UPID, error)) error {
	if err := x.check(ctx); err != nil {
		return err
	}
	up, err := call()
	if err != nil {
		return classify(code, err)
	}
	return x.task(ctx, up, code)
}

func (x *acq) task(ctx context.Context, up pve.UPID, code string) error {
	if up == "" {
		return nil
	}
	res := x.capi.WaitTaskBounded(ctx, up, taskBudget)
	switch {
	case res.Outcome == pve.TaskSucceeded:
		return nil
	case res.Outcome == pve.TaskFailed:
		return permanent(code, "Proxmox task %s failed: %s", up, x.tail(ctx, up, res.ExitStatus))
	case x.lost() || ctx.Err() != nil:
		return errStop
	}
	return waiting(code, "Proxmox task %s is still running.", up)
}

// tail is a failed task's exit status with the last lines of its log.
func (x *acq) tail(ctx context.Context, up pve.UPID, exit string) string {
	lines, err := x.capi.TaskLog(ctx, up, 500)
	if err != nil || len(lines) == 0 {
		return clip(exit)
	}
	return clip(exit + " | " + strings.Join(lines[max(len(lines)-3, 0):], " | "))
}

// mentions reports whether any config value names file (a CD's file name).
func mentions(cfg pve.VMConfig, file string) bool {
	for _, v := range cfg {
		if s, ok := v.(string); ok && strings.Contains(s, "/"+file) {
			return true
		}
	}
	return false
}

// withNet0 is the clone's net0 with its model and MAC kept, on bridge with the VM firewall on, tagged with vlan when
// that is set (Proxmox then checks SDN.Use on /sdn/zones/localnetwork/<bridge>/<vlan>: pve.NICPath).
func withNet0(old, bridge string, vlan, mtu int) string {
	var keep []string
	for _, p := range strings.Split(old, ",") {
		if k, _, _ := strings.Cut(p, "="); p != "" && k != "bridge" && k != "firewall" && k != "mtu" && k != "tag" {
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		keep = []string{"virtio"}
	}
	keep = append(keep, "bridge="+bridge, "firewall=1")
	if vlan > 0 {
		keep = append(keep, "tag="+strconv.Itoa(vlan))
	}
	if mtu > 0 {
		keep = append(keep, "mtu="+strconv.Itoa(mtu))
	}
	return strings.Join(keep, ",")
}

// bootDisk is the clone's first disk and its size in GiB.
func bootDisk(cfg pve.VMConfig) (string, int) {
	for _, k := range []string{"scsi0", "virtio0", "sata0"} {
		v := cfg.Str(k)
		if v == "" || strings.Contains(v, "media=cdrom") {
			continue
		}
		for _, p := range strings.Split(v, ",") {
			if s, ok := strings.CutPrefix(p, "size="); ok && s != "" {
				n, _ := strconv.ParseFloat(strings.TrimRight(s, "KMGT"), 64)
				switch s[len(s)-1] {
				case 'T':
					n *= 1024
				case 'G':
				case 'M':
					n /= 1024
				case 'K':
					n /= 1 << 20
				default: // Proxmox writes a size that is no whole KiB in bytes
					n /= 1 << 30
				}
				return k, int(n)
			}
		}
		return k, 0
	}
	return "", 0
}
