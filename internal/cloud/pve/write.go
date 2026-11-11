package pve

// write.go — VM lifecycle on one PVE cluster: clone-from-template create, delete,
// power verbs, resize, rename. The platform handler (cloud_writes_pve.go) is the
// HTTP face; everything cloud-touching lives here so it is testable against the
// httptest fake. Every mutation waits its UPID and reads back node-scoped state
// (cluster/resources lags pmxcfs — the read-back-after-wait discipline).

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// ServerSpec is the flavor demand of a create/resize (a neutral twin of the
// platform's pveflavor.Flavor — pve must not import platform packages).
type ServerSpec struct {
	Slug           string
	VCPUs          int
	RAMMb          int
	DiskGb         int
	StorageProfile string
	GPUModel       string
	GPUCount       int
}

// CreateServerReq drives CreateServer.
type CreateServerReq struct {
	ProjectID     string
	Region        string
	Name          string
	Spec          ServerSpec
	TemplateVMID  int
	TemplateNode  string
	ImageSlug     string
	TargetNode    string   // scheduler pick; "" = template's node
	Storage       string   // full-clone target storage; "" = template's
	Bridge        string   // net0 override ("" = keep the template's NIC as cloned)
	SSHKeys       string   // authorized_keys content (cloud-init)
	Password      string   // cloud-init user password
	RootDiskGiB   int      // ≥ template disk; 0 = flavor DiskGb
	GPUMappingIDs []string // reserved cluster PCI mappings for Spec.GPUModel (nil = no GPU)
	// SecurityGroups are firewall group ids the VM references (ServerSecurityGroups
	// resolves them; the platform always passes at least the project default).
	SecurityGroups []string
	// AssignIP picks net0's fixed address once the vmid is known, on a vnet whose zone has no
	// DHCP (nil = the zone's DHCP serves it). The caller owns the allocation and releases it
	// when CreateServer fails.
	AssignIP func(ctx context.Context, vmid int) (ip string, prefix int, gateway string, err error)
	// Seed writes the server's NoCloud seed before its first boot (nil = PVE's own drive).
	Seed func(ctx context.Context, node string, vmid int) error
	// SpreadGroup rides the VM's tags: the scheduler keeps a group's members apart.
	SpreadGroup string
	// LoadBalancerID marks the VM as a load balancer's appliance ("" for a tenant's server).
	LoadBalancerID string
	// createdUnix is the identity stamp, set by CreateServer.
	createdUnix int64
}

// vmTags renders the identity tags for a create/read-back.
func (r CreateServerReq) vmTags() string {
	tags := []string{TagManaged, FlavorTag(r.Spec.Slug), ImageTag(r.ImageSlug), ProfileTag(r.Spec.StorageProfile), CreatedTag(r.createdUnix)}
	if r.Spec.GPUModel != "" && r.Spec.GPUCount > 0 {
		tags = append(tags, GPUTag(r.Spec.GPUModel, r.Spec.GPUCount))
	}
	if r.SpreadGroup != "" {
		tags = append(tags, SpreadTag(r.SpreadGroup))
	}
	if r.LoadBalancerID != "" {
		tags = append(tags, LBTag(r.LoadBalancerID))
	}
	return JoinTags(tags...)
}

// createTimeout bounds one create end-to-end (full clones copy the whole disk).
const createTimeout = 15 * time.Minute

// CreateServer clones the template, applies the flavor + cloud-init config,
// resizes the root disk and starts the VM. Returns the cache row (the sync
// refreshes it within a cycle).
func (s *Service) CreateServer(ctx context.Context, req CreateServerReq) (*cloud.CloudResource, error) {
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	pool := s.PoolFor(req.ProjectID)

	// vmid: NextID is racy by design — retry a fresh id on "already exists".
	var vmid int
	var upid UPID
	for attempt := 0; ; attempt++ {
		id, err := s.C.NextID(ctx)
		if err != nil {
			return nil, fmt.Errorf("pve create: next vmid: %w", err)
		}
		vmid = id
		upid, err = s.C.CloneVM(ctx, req.TemplateNode, req.TemplateVMID, CloneOpts{
			NewID: vmid, Name: req.Name, Pool: pool,
			Target: req.TargetNode, Storage: req.Storage, Full: true,
		})
		if err == nil {
			break
		}
		// vmid collision or the template's serialized clone lock — both retryable.
		msg := strings.ToLower(err.Error())
		if attempt < 3 && (strings.Contains(msg, "already exist") || strings.Contains(msg, "can't lock")) {
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		return nil, fmt.Errorf("pve create: clone: %w", err)
	}
	if err := s.C.WaitTask(ctx, upid); err != nil {
		return nil, fmt.Errorf("pve create: clone: %w", err)
	}
	node := req.TargetNode
	if node == "" {
		node = req.TemplateNode
	}

	// Every NIC the platform leaves on the VM carries firewall=1 — the cloned
	// template's NICs included — or no group / ipfilter applies to it.
	cloned, err := s.C.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return nil, s.undoCreate(ctx, node, vmid, fmt.Errorf("pve create: read config: %w", err))
	}
	req.createdUnix = nowUTC().Unix()
	params := nicFirewallParams(cloned)
	drop := extraNICKeys(cloned)
	for _, k := range drop {
		delete(params, k)
	}
	ipconfig0, fixedIP := "ip=dhcp", ""
	if req.AssignIP != nil {
		ip, prefix, gw, aerr := req.AssignIP(ctx, vmid)
		if aerr != nil {
			return nil, s.undoCreate(ctx, node, vmid, aerr)
		}
		fixedIP = ip
		ipconfig0 = fmt.Sprintf("ip=%s/%d,gw=%s", ip, prefix, gw)
	}
	// Flavor + cloud-init + identity tags. sshkeys is the PVE quirk: the value must
	// be URI-encoded ONCE MORE inside the form encoding.
	for k, v := range map[string]string{
		"cores":     strconv.Itoa(req.Spec.VCPUs),
		"memory":    strconv.Itoa(req.Spec.RAMMb),
		"agent":     "1", // guest agent gives the sync its IP addresses
		"tags":      req.vmTags(),
		"ipconfig0": ipconfig0,
	} {
		params[k] = v
	}
	if len(req.GPUMappingIDs) > 0 {
		for k, v := range GPUCreateParams(req.GPUMappingIDs) {
			params[k] = v
		}
	}
	if req.SSHKeys != "" {
		// PVE uri-unescapes the value: "+" stays literal, so spaces must be %20.
		params["sshkeys"] = strings.ReplaceAll(url.QueryEscape(strings.TrimSpace(req.SSHKeys)), "+", "%20")
	}
	if req.Password != "" {
		params["cipassword"] = req.Password
	}
	if req.Bridge != "" {
		params["net0"] = "virtio,bridge=" + req.Bridge + ",firewall=1"
	}
	cfgUPID, err := s.C.SetVMConfig(ctx, node, vmid, params, drop...)
	if err == nil {
		err = s.C.WaitTask(ctx, cfgUPID)
	}
	if err != nil {
		// The clone exists but is misconfigured — destroy it rather than leak a
		// half-built VM into the pool (billing would meter it).
		return nil, s.undoCreate(ctx, node, vmid, fmt.Errorf("pve create: configure: %w", err))
	}

	// Root disk grow (PVE cannot shrink; template smaller than flavor is the norm).
	rootGiB := req.RootDiskGiB
	if rootGiB == 0 {
		rootGiB = req.Spec.DiskGb
	}
	if rootGiB > 0 {
		if slot := s.bootDiskSlot(ctx, node, vmid); slot != "" {
			if up, rerr := s.C.ResizeDisk(ctx, node, vmid, slot, fmt.Sprintf("%dG", rootGiB)); rerr == nil {
				_ = s.C.WaitTask(ctx, up)
			}
			// resize failure is non-fatal: the VM boots with the template disk; the
			// sync reports the honest size and the user can retry via volume extend.
		}
	}

	// Firewall before the first boot: a VM never runs unfiltered. Failure destroys
	// it rather than start it open. A clone inherits the source's firewall config, so a
	// server made from a project image starts from its groups: only the chosen ones stay.
	err = s.clearVMRules(ctx, node, vmid)
	for _, g := range req.SecurityGroups {
		if err != nil {
			break
		}
		err = s.C.AddVMGroupRule(ctx, node, vmid, g)
	}
	if err == nil {
		err = s.SecureVM(ctx, node, vmid)
	}
	if err == nil && fixedIP != "" {
		err = s.SetIPFilter(ctx, node, vmid, "net0", []string{fixedIP + "/32"})
	}
	if err != nil {
		return nil, s.undoCreate(ctx, node, vmid, fmt.Errorf("pve create: firewall: %w", err))
	}
	if req.Seed != nil {
		if err := req.Seed(ctx, node, vmid); err != nil {
			return nil, s.undoCreate(ctx, node, vmid, fmt.Errorf("pve create: seed: %w", err))
		}
	}

	if up, serr := s.C.StartVM(ctx, node, vmid); serr == nil {
		_ = s.C.WaitTask(ctx, up)
	}

	cr, err := s.readBackServer(ctx, node, vmid, req)
	if err != nil {
		// A VM with no row that keeps running would hold addresses the caller is about to give back.
		return nil, s.undoCreate(ctx, node, vmid, err)
	}
	return cr, nil
}

// VMLeftError: a create failed and the VM it made is not known to be gone. Its addresses stay held, since the VM may
// still be using them: the sync adopts it, and its delete or the orphan sweep frees them.
type VMLeftError struct {
	VMID int
	Err  error
}

func (e *VMLeftError) Error() string {
	return fmt.Sprintf("pve create: vm %d not confirmed destroyed: %v", e.VMID, e.Err)
}
func (e *VMLeftError) Unwrap() error { return e.Err }

// undoCreate destroys what a failed create made and answers cause, or a VMLeftError unless PVE confirms the VM is gone:
// its destroy task ended well, or the cluster no longer lists it. Asking the VM itself cannot tell: a token whose rights
// come from the pool gets 403 for a VM that left it. It has its own clock: the create's may be what ran out.
func (s *Service) undoCreate(ctx context.Context, node string, vmid int, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()
	if up, err := s.C.StopVM(ctx, node, vmid); err == nil {
		_ = s.C.WaitTask(ctx, up)
	}
	up, err := s.C.DeleteVM(ctx, node, vmid)
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err == nil {
		return cause
	}
	rows, lerr := s.C.ClusterResources(ctx, "vm")
	if lerr != nil {
		return &VMLeftError{VMID: vmid, Err: cause}
	}
	for _, r := range rows {
		if r.Type == "qemu" && r.VMID == vmid {
			return &VMLeftError{VMID: vmid, Err: cause}
		}
	}
	return cause
}

// destroyBestEffort reaps a half-created VM (stop → delete, errors swallowed).
func (s *Service) destroyBestEffort(ctx context.Context, node string, vmid int) {
	if up, err := s.C.StopVM(ctx, node, vmid); err == nil {
		_ = s.C.WaitTask(ctx, up)
	}
	if up, err := s.C.DeleteVM(ctx, node, vmid); err == nil {
		_ = s.C.WaitTask(ctx, up)
	}
}

// bootDiskSlot finds the VM's boot disk config key (scsi0/virtio0 by convention).
func (s *Service) bootDiskSlot(ctx context.Context, node string, vmid int) string {
	cfg, err := s.C.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return ""
	}
	for _, slot := range []string{"scsi0", "virtio0", "sata0", "ide0"} {
		if cfg.Str(slot) != "" {
			return slot
		}
	}
	return ""
}

// readBackServer builds the cache row from node-scoped live state.
func (s *Service) readBackServer(ctx context.Context, node string, vmid int, req CreateServerReq) (*cloud.CloudResource, error) {
	st, err := s.C.GetVMStatus(ctx, node, vmid)
	if err != nil {
		return nil, fmt.Errorf("pve create: read back vm %d: %w", vmid, err)
	}
	r := ClusterResource{
		VMID: vmid, Name: st.Name, Node: node, Status: st.Status, Lock: st.Lock,
		Tags:   req.vmTags(),
		MaxCPU: float64(req.Spec.VCPUs), MaxMem: int64(req.Spec.RAMMb) << 20,
		MaxDisk: int64(req.Spec.DiskGb) << 30,
	}
	if st.MaxDisk > 0 { // a project image can be larger than the flavor's disk
		r.MaxDisk = st.MaxDisk
	}
	if r.Name == "" {
		r.Name = req.Name
	}
	now := nowUTC()
	return &cloud.CloudResource{
		ExternalID: strconv.Itoa(vmid),
		ProjectID:  req.ProjectID,
		Region:     req.Region,
		Type:       cloud.TypeServer,
		Data:       ServerData(r, s.ServerAddresses(ctx, r), nil),
		CreatedAt:  &now, UpdatedAt: &now,
		Info: &cloud.Info{CreatedAt: &now, UpdatedAt: &now},
	}, nil
}

// opTimeout bounds one power/config operation.
const opTimeout = 5 * time.Minute

// PoweredOn reports a VM holding a QEMU process, which has to be stopped before it is off or can be destroyed. A suspended VM
// reads "paused", not "running": a project's VMs are paused while it is DISABLED, and its members can still stop and delete them.
func (r *ClusterResource) PoweredOn() bool { return r.Status == "running" || r.Status == "paused" }

// ErrNotInProject is ResolveVM refusing a vmid that names no VM of the project's pool. Tenant input reaches it (an attach
// target, a load balancer member), so callers answer it as a missing server, not as a failing backend.
var ErrNotInProject = errors.New("pve: server not in this project")

// ResolveVM finds a pool VM by vmid string, verifying pool membership — the
// per-op ownership re-check (platform-is-the-PDP): a cached row names the vmid,
// but the pool is re-verified against the live cluster before any mutation.
func (s *Service) ResolveVM(ctx context.Context, projectID, externalID string) (*ClusterResource, error) {
	vmid, err := strconv.Atoi(externalID)
	if err != nil {
		return nil, fmt.Errorf("%w: bad vmid %q", ErrNotInProject, externalID)
	}
	rows, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	pool := s.PoolFor(projectID)
	for i := range rows {
		if rows[i].Type == "qemu" && rows[i].VMID == vmid {
			if rows[i].Pool != pool {
				return nil, fmt.Errorf("%w: vm %d is not in project pool %s", ErrNotInProject, vmid, pool)
			}
			// cluster/resources lags a just-finished task by seconds: pool and node come
			// from it, power state and config from the node itself (when it answers).
			r := &rows[i]
			if r.NodeDown {
				return r, nil
			}
			st, err := s.C.GetVMStatus(ctx, r.Node, vmid)
			if err != nil {
				r.StatusErr = err
				return r, nil
			}
			r.Status, r.Lock, r.Name = st.Status, st.Lock, st.Name
			r.Tags, r.MaxCPU, r.MaxMem, r.MaxDisk = st.Tags, st.CPUs, st.MaxMem, st.MaxDisk
			if st.QMPStatus == "paused" { // a suspended billing profile or disabled project
				r.Status = "paused"
			}
			return r, nil
		}
	}
	return nil, nil // vanished — caller archives the row
}

// ServerAddresses is the VM's addresses in the API shape: what the guest agent reports, or
// failing that (stopped, still booting, no agent) the static ipconfigN of its config. An
// address on ipconfig1+ is a public-NIC address, the rest are private.
func (s *Service) ServerAddresses(ctx context.Context, r ClusterResource) map[string]any {
	cfg, err := s.C.GetVMConfig(ctx, r.Node, r.VMID)
	if err != nil {
		return map[string]any{}
	}
	public := map[string]bool{}
	var configured []any
	for _, key := range nicKeys(cfg) {
		addr := ipconfigAddr(cfg.Str("ipconfig" + key[3:]))
		if addr == "" {
			continue
		}
		configured = append(configured, addr)
		public[addr] = key != "net0"
	}
	var addrs []any
	if r.Status == "running" {
		if ifaces, err := s.C.AgentNetworkInterfaces(ctx, r.Node, r.VMID); err == nil {
			if private, ok := AgentAddresses(ifaces)["private"].([]any); ok {
				addrs = private
			}
		}
	}
	if len(addrs) == 0 {
		slices.SortFunc(configured, func(a, b any) int { return cmp.Compare(a.(string), b.(string)) })
		for _, a := range configured {
			addrs = append(addrs, map[string]any{"addr": a, "version": 4})
		}
	}
	out := map[string]any{}
	seen := map[string]bool{}
	for _, a := range addrs {
		addr := a.(map[string]any)["addr"].(string)
		seen[addr] = true
		group := "private"
		if public[addr] {
			group = "public"
		}
		list, _ := out[group].([]any)
		out[group] = append(list, a)
	}
	// A public address the guest has not brought up yet (it needs a reboot) is still its address.
	for _, a := range configured {
		if addr := a.(string); public[addr] && !seen[addr] {
			list, _ := out["public"].([]any)
			out["public"] = append(list, map[string]any{"addr": addr, "version": 4})
		}
	}
	return out
}

// ipconfigAddr extracts the static address of an ipconfigN value ("" for dhcp or none).
func ipconfigAddr(val string) string {
	for _, p := range strings.Split(val, ",") {
		if v, ok := strings.CutPrefix(p, "ip="); ok && v != "dhcp" && v != "manual" {
			addr, _, _ := strings.Cut(v, "/")
			return addr
		}
	}
	return ""
}

// DeleteServer stops (hard) and destroys a VM.
func (s *Service) DeleteServer(ctx context.Context, projectID, externalID string) error {
	if s.HolderOperations != nil {
		ctx, release, err := s.beginHolders(ctx, projectID)
		if err != nil {
			return err
		}
		defer release()
		vm, err := s.ResolveVM(ctx, projectID, externalID)
		if err != nil || vm == nil {
			return err
		}
		return s.deleteFencedServer(ctx, projectID, vm, false)
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if vm == nil {
		return nil // already gone
	}
	// HA would restart what we stop below.
	if err := s.disableProtectionOn(ctx, vm); err != nil {
		return err
	}
	s.waitReplicaRemoved(ctx, vm.VMID)
	if vm.PoweredOn() {
		if up, err := s.C.StopVM(ctx, vm.Node, vm.VMID); err == nil {
			_ = s.C.WaitTask(ctx, up)
		}
	}
	up, err := s.C.DeleteVM(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

// PowerAction runs one FE power verb. Returns the post-action status string.
func (s *Service) PowerAction(ctx context.Context, projectID, externalID, verb string) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if vm == nil {
		return fmt.Errorf("pve: vm %s not found", externalID)
	}
	if vm.NodeDown { // what follows reads a stop of a VM that is not running as already done
		return conflictf("The host of this server is down. Try again when it is back")
	}
	running := vm.Status == "running"
	var up UPID
	switch verb {
	case "START":
		if running {
			return nil
		}
		if vm.Status == "paused" { // a resume that did not reach it: a project that can start things is not held
			up, err = s.C.ResumeVM(ctx, vm.Node, vm.VMID)
			break
		}
		up, err = s.C.StartVM(ctx, vm.Node, vm.VMID)
	case "STOP", "SHUTOFF", "SHUTDOWN":
		if !vm.PoweredOn() {
			return nil
		}
		up, err = s.C.ShutdownVM(ctx, vm.Node, vm.VMID, 60) // forceStop: PVE stops a paused VM at once
	case "REBOOT_SOFT", "SOFTREBOOT", "REBOOT":
		if !running {
			return conflictf("Server %s is not running", vm.Name)
		}
		up, err = s.C.RebootVM(ctx, vm.Node, vm.VMID)
	case "REBOOT_HARD", "HARDREBOOT":
		if !running {
			return conflictf("Server %s is not running", vm.Name)
		}
		up, err = s.C.ResetVM(ctx, vm.Node, vm.VMID)
	default:
		return fmt.Errorf("pve: unsupported power verb %q", verb)
	}
	if err != nil {
		return err
	}
	err = s.C.WaitTask(ctx, up)
	if err != nil && strings.Contains(err.Error(), "got timeout") {
		return conflictf("Server %s did not respond in time. If it is still booting, try again when it is up", vm.Name)
	}
	if err == nil && HasTag(vm.Tags, tagProtected) {
		s.waitHAPower(ctx, vm, verb)
	}
	return err
}

// ResizeServer applies a new flavor: graceful stop when running, set cores/memory + flavor tag, grow the root disk when the
// new flavor's is larger, start again if it was running.
func (s *Service) ResizeServer(ctx context.Context, projectID, externalID string, spec ServerSpec, headroomPct float64) error {
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	if s.HolderOperations != nil {
		var release func()
		var err error
		ctx, release, err = s.beginHolders(ctx, projectID)
		if err != nil {
			return err
		}
		defer release()
	}
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if vm == nil {
		return fmt.Errorf("pve: vm %s not found", externalID)
	}
	// Grow first: an unverified/full storage must not stop the VM or change its
	// flavor. ExtendVolume joins this holder session and preserves R1 intent and
	// effect checks; failures (including uncertain backend effects) propagate.
	if spec.DiskGb > 0 {
		target, err := volumeTargetBytes(spec.DiskGb)
		if err != nil {
			return err
		}
		cfg, err := s.volumeGrowthConfig(ctx, vm)
		if err != nil {
			return err
		}
		var root VMDisk
		for _, slot := range []string{"scsi0", "virtio0", "sata0", "ide0"} {
			for _, d := range ParseDisks(cfg) {
				if d.Slot == slot {
					root = d
					break
				}
			}
			if root.Slot != "" {
				break
			}
		}
		current, known := diskSizeBytes(cfg.Str(root.Slot))
		if !known {
			return conflictf("The live boot disk size is unknown or invalid")
		}
		// A smaller flavor keeps the existing disk; never ask PVE to shrink.
		if target > current {
			if err := s.ExtendVolume(ctx, projectID, vm.VMID, root.VolID, spec.DiskGb, headroomPct); err != nil {
				return err
			}
		}
	}
	wasRunning := vm.Status == "running"
	if vm.PoweredOn() {
		if up, err := s.C.ShutdownVM(ctx, vm.Node, vm.VMID, 120); err == nil {
			if werr := s.C.WaitTask(ctx, up); werr != nil {
				return fmt.Errorf("pve resize: shutdown: %w", werr)
			}
			if werr := s.waitStopped(ctx, vm); werr != nil {
				return werr
			}
		} else {
			return fmt.Errorf("pve resize: shutdown: %w", err)
		}
	}
	// Swap the flavor tag in place; other tags survive.
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return err
	}
	var tags []string
	for _, t := range strings.Split(cfg.Str("tags"), ";") {
		if t != "" && !strings.HasPrefix(t, tagFlavorPref) {
			tags = append(tags, t)
		}
	}
	tags = append(tags, FlavorTag(spec.Slug))
	up, err := s.C.SetVMConfig(ctx, vm.Node, vm.VMID, map[string]string{
		"cores":  strconv.Itoa(spec.VCPUs),
		"memory": strconv.Itoa(spec.RAMMb),
		"tags":   JoinTags(tags...),
	})
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		return fmt.Errorf("pve resize: configure: %w", err)
	}
	if wasRunning {
		up, err := s.C.StartVM(ctx, vm.Node, vm.VMID)
		if err == nil {
			err = s.C.WaitTask(ctx, up)
		}
		if err != nil {
			return fmt.Errorf("pve resize: start: %w", err)
		}
		if !s.waitUnderHA(ctx, vm, "START") {
			return conflictf("Server %s was resized but has not come back yet. Start it if it stays off", vm.Name)
		}
	}
	return nil
}

// RenameServer sets the VM name (display-only; vmid is the identity).
func (s *Service) RenameServer(ctx context.Context, projectID, externalID, name string) error {
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if vm == nil {
		return fmt.Errorf("pve: vm %s not found", externalID)
	}
	up, err := s.C.SetVMConfig(ctx, vm.Node, vm.VMID, map[string]string{"name": name})
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

// Console mints a one-shot VNC (or serial when serial=true) websocket target.
// The returned URL is the PVE backend socket; the platform console proxy dials it
// with the API-token header. password is the VNC ticket — noVNC must present it
// as the RFB credential. A serial session (termproxy) opens by sending "<user>:<ticket>\n".
func (s *Service) Console(ctx context.Context, projectID, externalID string, serial bool) (backendURL, user, password string, err error) {
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil {
		return "", "", "", err
	}
	if vm == nil {
		return "", "", "", fmt.Errorf("pve: vm %s not found", externalID)
	}
	var t *VNCProxyTicket
	if serial {
		t, err = s.C.TermProxy(ctx, vm.Node, vm.VMID)
	} else {
		t, err = s.C.VNCProxy(ctx, vm.Node, vm.VMID)
	}
	if err != nil {
		return "", "", "", err
	}
	q := url.Values{}
	q.Set("port", t.PortString())
	q.Set("vncticket", t.Ticket)
	u := s.C.BaseURL() + qemuPath(vm.Node, vm.VMID) + "/vncwebsocket?" + q.Encode()
	return u, t.User, t.Ticket, nil
}

// imageBuildPrefix names a clone that is not a server: one CreateImage has not converted yet, or the
// disk donor of a rebuild. No sync takes it for a server.
const imageBuildPrefix = "pc-img-build-"

// CreateImage snapshots a pool VM into a project-owned template (full clone →
// convert). Returns the template's vmid; the IMAGE sync row follows.
func (s *Service) CreateImage(ctx context.Context, projectID, serverExtID, name string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	vm, err := s.ResolveVM(ctx, projectID, serverExtID)
	if err != nil {
		return 0, err
	}
	if vm == nil {
		return 0, fmt.Errorf("pve: server %s not found", serverExtID)
	}
	if err := refuseAppliance(vm); err != nil {
		return 0, err
	}
	newID, err := s.C.NextID(ctx)
	if err != nil {
		return 0, err
	}
	upid, err := s.C.CloneVM(ctx, vm.Node, vm.VMID, CloneOpts{
		NewID: newID, Name: imageBuildPrefix + name, Pool: s.PoolFor(projectID), Full: true,
	})
	if err != nil {
		return 0, err
	}
	if err := s.C.WaitTask(ctx, upid); err != nil {
		return 0, err
	}
	// The clone inherited the source VM's identity stamp. Dropping it keeps it from spreading to
	// every VM later cloned out of this template.
	cfg, err := s.C.GetVMConfig(ctx, vm.Node, newID)
	if err == nil {
		var tags []string
		for _, t := range strings.Split(cfg.Str("tags"), ";") {
			if t != "" && !strings.HasPrefix(t, tagCreatedPref) {
				tags = append(tags, t)
			}
		}
		var up UPID
		if up, err = s.C.SetVMConfig(ctx, vm.Node, newID, map[string]string{"tags": JoinTags(tags...)}); err == nil {
			err = s.C.WaitTask(ctx, up)
		}
	}
	// A project image is seeded per server like any other: it must not keep this server's CD.
	if err == nil {
		err = s.dropSeedFromClone(ctx, vm.Node, newID)
	}
	if err != nil {
		s.destroyBestEffort(ctx, vm.Node, newID)
		return 0, err
	}
	tupid, err := s.C.ToTemplate(ctx, vm.Node, newID)
	if err != nil {
		s.destroyBestEffort(ctx, vm.Node, newID)
		return 0, err
	}
	if err := s.C.WaitTask(ctx, tupid); err != nil {
		s.destroyBestEffort(ctx, vm.Node, newID)
		return 0, err
	}
	up, err := s.C.SetVMConfig(ctx, vm.Node, newID, map[string]string{"name": name})
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		s.destroyBestEffort(ctx, vm.Node, newID)
		return 0, err
	}
	return newID, nil
}

// dropSeedFromClone clears what a clone inherited of its source's seed: the smbios serial (the seed
// URL with the source's token) and the seed CD, which a PVE cloud-init drive replaces.
func (s *Service) dropSeedFromClone(ctx context.Context, node string, vmid int) error {
	cfg, err := s.C.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return err
	}
	slot, vol := cloudInitSlot(cfg), seedVolOf(cfg)
	params := map[string]string{}
	if v := cfg.Str("smbios1"); strings.Contains(v, "serial=") {
		params["smbios1"] = smbiosWithoutSerial(v)
	}
	var del []string
	if vol != "" {
		del = append(del, slot)
	}
	if len(params) > 0 || len(del) > 0 {
		up, err := s.C.SetVMConfig(ctx, node, vmid, params, del...)
		if err == nil {
			err = s.C.WaitTask(ctx, up)
		}
		if err != nil {
			return err
		}
	}
	if slot != "" && vol == "" {
		return nil // still has PVE's drive
	}
	if slot == "" {
		slot = "ide2"
	}
	storage, _, ok := strings.Cut(cfg.Str(s.bootDiskSlot(ctx, node, vmid)), ":")
	if !ok {
		return nil
	}
	up, err := s.C.SetVMConfig(ctx, node, vmid, map[string]string{slot: storage + ":cloudinit"})
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	return err
}

// SetPassword changes a user's password in a running guest through its agent. The guest's own
// users are the guest's business: an unknown user comes back as the agent's error.
func (s *Service) SetPassword(ctx context.Context, projectID, externalID, username, password string) error {
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if vm == nil {
		return fmt.Errorf("pve: vm %s not found", externalID)
	}
	if vm.Status != "running" {
		return conflictf("Start server %s to set a password: the guest agent does it", vm.Name)
	}
	err = s.C.AgentSetUserPassword(ctx, vm.Node, vm.VMID, username, password)
	if IsForbidden(err) {
		return conflictf("The provider's API token cannot set guest passwords (it needs VM.GuestAgent.Unrestricted)")
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.Status >= 500 {
		// A 500 is the agent answering (or not): no agent yet, or no such user.
		return conflictf("The guest agent could not set the password for user %q: %s", username, ae.Message)
	}
	return err
}

// DefaultUserOf is the login user of a server's image: cloud images name it after the distro.
func DefaultUserOf(imageSlug string) string {
	first, _, _ := strings.Cut(imageSlug, "-")
	switch first {
	case "debian", "ubuntu", "fedora", "centos", "rocky", "almalinux", "alpine", "arch":
		return first
	case "rhel":
		return "cloud-user"
	}
	return "root"
}

// RebuildReq drives RebuildServer.
type RebuildReq struct {
	ProjectID, ServerID string
	TemplateVMID        int
	TemplateNode        string
	ImageSlug           string
	// Reseed runs once the new disk is in, before the first boot (nil = PVE's own drive).
	Reseed func(ctx context.Context, node string, vmid int) error
}

// RebuildServer replaces a server's boot disk with a fresh copy of an image; its identity (vmid, NICs,
// addresses, firewall, data disks) stays. The sequence never leaves the VM without a disk: the old one
// is parked as unusedN while the new one moves in and only removed once it did, and any failure until
// then puts it back. PVE moves a disk between VMs only on one node and storage, so the copy lands on
// the storage the old disk is on. A boot disk with snapshots cannot be moved away from: 409.
func (s *Service) RebuildServer(ctx context.Context, req RebuildReq) error {
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	vm, err := s.ResolveVM(ctx, req.ProjectID, req.ServerID)
	if err != nil {
		return err
	}
	if vm == nil {
		return fmt.Errorf("pve: vm %s not found", req.ServerID)
	}
	node := vm.Node
	snaps, err := s.C.ListSnapshots(ctx, node, vm.VMID)
	if err != nil {
		return err
	}
	for _, sn := range snaps {
		if sn.Name != "current" {
			return conflictf("Delete the snapshots of server %s before rebuilding it", vm.Name)
		}
	}
	cfg, err := s.C.GetVMConfig(ctx, node, vm.VMID)
	if err != nil {
		return err
	}
	slot := s.bootDiskSlot(ctx, node, vm.VMID)
	if slot == "" {
		return conflictf("Server %s has no boot disk", vm.Name)
	}
	oldVal := cfg.Str(slot)
	oldVol, _, _ := strings.Cut(oldVal, ",")
	storage, _, _ := strings.Cut(oldVol, ":")
	oldGiB := 0
	for _, d := range ParseDisks(cfg) {
		if d.Slot == slot {
			oldGiB = d.SizeGiB
		}
	}

	// The fresh disk first: nothing has happened to the server if this fails.
	tmpID, err := s.cloneRetry(ctx, req.TemplateNode, req.TemplateVMID, CloneOpts{
		Name: imageBuildPrefix + "rebuild-" + req.ServerID, Pool: s.PoolFor(req.ProjectID), Target: nodeIfOther(node, req.TemplateNode), Storage: storage, Full: true,
	})
	if err != nil {
		return fmt.Errorf("pve rebuild: clone: %w", err)
	}
	defer s.destroyBestEffort(context.WithoutCancel(ctx), node, tmpID)
	tmpSlot := s.bootDiskSlot(ctx, node, tmpID)
	if tmpSlot == "" {
		return conflictf("Image %s has no boot disk", req.ImageSlug)
	}

	if vm.PoweredOn() {
		up, err := s.C.StopVM(ctx, node, vm.VMID)
		if err == nil {
			err = s.C.WaitTask(ctx, up)
		}
		if err != nil {
			return fmt.Errorf("pve rebuild: stop: %w", err)
		}
		if err := s.waitStopped(ctx, vm); err != nil {
			return err
		}
	}
	// Removing a disk from its slot parks it as unusedN.
	up, err := s.C.SetVMConfig(ctx, node, vm.VMID, nil, slot)
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		return fmt.Errorf("pve rebuild: detach: %w", err)
	}
	restore := func() {
		rctx := context.WithoutCancel(ctx)
		if up, err := s.C.SetVMConfig(rctx, node, vm.VMID, map[string]string{slot: oldVal}); err == nil {
			_ = s.C.WaitTask(rctx, up)
		}
		if vm.Status == "running" {
			if up, err := s.C.StartVM(rctx, node, vm.VMID); err == nil {
				_ = s.C.WaitTask(rctx, up)
			}
		}
	}
	up, err = s.C.MoveDisk(ctx, node, tmpID, MoveDiskOpts{Disk: tmpSlot, TargetVMID: vm.VMID, TargetDisk: slot})
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		restore()
		return fmt.Errorf("pve rebuild: move disk: %w", err)
	}

	cfg, err = s.C.GetVMConfig(ctx, node, vm.VMID)
	if err != nil {
		return err
	}
	var del []string
	for _, d := range ParseDisks(cfg) {
		if strings.HasPrefix(d.Slot, "unused") && d.VolID == oldVol {
			del = append(del, d.Slot)
		}
	}
	var tags []string
	for _, t := range strings.Split(cfg.Str("tags"), ";") {
		if t != "" && !strings.HasPrefix(t, tagImagePref) {
			tags = append(tags, t)
		}
	}
	tags = append(tags, ImageTag(req.ImageSlug))
	up, err = s.C.SetVMConfig(ctx, node, vm.VMID, map[string]string{"tags": JoinTags(tags...), "boot": "order=" + slot}, del...)
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		return fmt.Errorf("pve rebuild: finish: %w", err)
	}
	if oldGiB > 0 {
		if up, rerr := s.C.ResizeDisk(ctx, node, vm.VMID, slot, fmt.Sprintf("%dG", oldGiB)); rerr == nil {
			_ = s.C.WaitTask(ctx, up)
		}
	}
	if req.Reseed != nil {
		if err := req.Reseed(ctx, node, vm.VMID); err != nil {
			return fmt.Errorf("pve rebuild: seed: %w", err)
		}
	}
	up, err = s.C.StartVM(ctx, node, vm.VMID)
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		return fmt.Errorf("pve rebuild: start: %w", err)
	}
	s.waitUnderHA(ctx, vm, "START")
	return nil
}

func nodeIfOther(node, templateNode string) string {
	if node == templateNode {
		return ""
	}
	return node
}

// cloneRetry clones under a fresh vmid, retrying the races NextID and the template's clone lock allow.
func (s *Service) cloneRetry(ctx context.Context, node string, vmid int, o CloneOpts) (int, error) {
	for attempt := 0; ; attempt++ {
		id, err := s.C.NextID(ctx)
		if err != nil {
			return 0, err
		}
		o.NewID = id
		up, err := s.C.CloneVM(ctx, node, vmid, o)
		if err == nil {
			if err := s.C.WaitTask(ctx, up); err != nil {
				return 0, err
			}
			return id, nil
		}
		msg := strings.ToLower(err.Error())
		if attempt < 3 && (strings.Contains(msg, "already exist") || strings.Contains(msg, "can't lock")) {
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		return 0, err
	}
}
