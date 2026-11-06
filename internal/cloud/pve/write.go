package pve

// write.go — VM lifecycle on one PVE cluster: clone-from-template create, delete,
// power verbs, resize, rename. The platform handler (cloud_writes_pve.go) is the
// HTTP face; everything cloud-touching lives here so it is testable against the
// httptest fake. Every mutation waits its UPID and reads back node-scoped state
// (cluster/resources lags pmxcfs — the read-back-after-wait discipline).

import (
	"cmp"
	"context"
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
}

// vmTags renders the identity tags for a create/read-back.
func (r CreateServerReq) vmTags() string {
	tags := []string{TagManaged, FlavorTag(r.Spec.Slug), ImageTag(r.ImageSlug), ProfileTag(r.Spec.StorageProfile)}
	if r.Spec.GPUModel != "" && r.Spec.GPUCount > 0 {
		tags = append(tags, GPUTag(r.Spec.GPUModel, r.Spec.GPUCount))
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
		s.destroyBestEffort(ctx, node, vmid)
		return nil, fmt.Errorf("pve create: read config: %w", err)
	}
	params := nicFirewallParams(cloned)
	drop := extraNICKeys(cloned)
	for _, k := range drop {
		delete(params, k)
	}
	ipconfig0, fixedIP := "ip=dhcp", ""
	if req.AssignIP != nil {
		ip, prefix, gw, aerr := req.AssignIP(ctx, vmid)
		if aerr != nil {
			s.destroyBestEffort(ctx, node, vmid)
			return nil, aerr
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
		s.destroyBestEffort(ctx, node, vmid)
		return nil, fmt.Errorf("pve create: configure: %w", err)
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
		s.destroyBestEffort(ctx, node, vmid)
		return nil, fmt.Errorf("pve create: firewall: %w", err)
	}

	if up, serr := s.C.StartVM(ctx, node, vmid); serr == nil {
		_ = s.C.WaitTask(ctx, up)
	}

	return s.readBackServer(ctx, node, vmid, req)
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

// ResolveVM finds a pool VM by vmid string, verifying pool membership — the
// per-op ownership re-check (platform-is-the-PDP): a cached row names the vmid,
// but the pool is re-verified against the live cluster before any mutation.
func (s *Service) ResolveVM(ctx context.Context, projectID, externalID string) (*ClusterResource, error) {
	vmid, err := strconv.Atoi(externalID)
	if err != nil {
		return nil, fmt.Errorf("pve: bad vmid %q", externalID)
	}
	rows, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	pool := s.PoolFor(projectID)
	for i := range rows {
		if rows[i].Type == "qemu" && rows[i].VMID == vmid {
			if rows[i].Pool != pool {
				return nil, fmt.Errorf("pve: vm %d is not in project pool %s", vmid, pool)
			}
			// cluster/resources lags a just-finished task by seconds: pool and node come
			// from it, power state and config from the node itself (when it answers).
			r := &rows[i]
			if st, err := s.C.GetVMStatus(ctx, r.Node, vmid); err == nil {
				r.Status, r.Lock, r.Name = st.Status, st.Lock, st.Name
				r.Tags, r.MaxCPU, r.MaxMem, r.MaxDisk = st.Tags, st.CPUs, st.MaxMem, st.MaxDisk
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
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if vm == nil {
		return nil // already gone
	}
	if vm.Status == "running" {
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
	running := vm.Status == "running"
	var up UPID
	switch verb {
	case "START":
		if running {
			return nil
		}
		up, err = s.C.StartVM(ctx, vm.Node, vm.VMID)
	case "STOP", "SHUTOFF", "SHUTDOWN":
		if !running {
			return nil
		}
		up, err = s.C.ShutdownVM(ctx, vm.Node, vm.VMID, 60)
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
	return err
}

// ResizeServer applies a new flavor: graceful stop when running, set cores/memory + flavor tag, grow the root disk when the
// new flavor's is larger, start again if it was running.
func (s *Service) ResizeServer(ctx context.Context, projectID, externalID string, spec ServerSpec) error {
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	vm, err := s.ResolveVM(ctx, projectID, externalID)
	if err != nil {
		return err
	}
	if vm == nil {
		return fmt.Errorf("pve: vm %s not found", externalID)
	}
	wasRunning := vm.Status == "running"
	if wasRunning {
		if up, err := s.C.ShutdownVM(ctx, vm.Node, vm.VMID, 120); err == nil {
			if werr := s.C.WaitTask(ctx, up); werr != nil {
				return fmt.Errorf("pve resize: shutdown: %w", werr)
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
	if spec.DiskGb > 0 {
		if slot := s.bootDiskSlot(ctx, vm.Node, vm.VMID); slot != "" {
			// Grow-only: PVE rejects a shrink; a smaller flavor keeps the disk (honest).
			if up, rerr := s.C.ResizeDisk(ctx, vm.Node, vm.VMID, slot, fmt.Sprintf("%dG", spec.DiskGb)); rerr == nil {
				_ = s.C.WaitTask(ctx, up)
			}
		}
	}
	if wasRunning {
		if up, serr := s.C.StartVM(ctx, vm.Node, vm.VMID); serr == nil {
			_ = s.C.WaitTask(ctx, up)
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
	newID, err := s.C.NextID(ctx)
	if err != nil {
		return 0, err
	}
	upid, err := s.C.CloneVM(ctx, vm.Node, vm.VMID, CloneOpts{
		NewID: newID, Name: name, Pool: s.PoolFor(projectID), Full: true,
	})
	if err != nil {
		return 0, err
	}
	if err := s.C.WaitTask(ctx, upid); err != nil {
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
	return newID, nil
}
