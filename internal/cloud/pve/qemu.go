package pve

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

func qemuPath(node string, vmid int) string {
	return "/nodes/" + node + "/qemu/" + strconv.Itoa(vmid)
}

// VMConfig is GET /nodes/{node}/qemu/{vmid}/config — kept as a raw map because the
// key space is open-ended (net0..N, scsi0..N, hostpci0..N, ipconfig0..N, …).
// Values are strings or numbers depending on the key; use the VMConfig helpers.
type VMConfig map[string]any

// Str returns a config value as a string regardless of the wire type.
func (c VMConfig) Str(key string) string {
	switch v := c[key].(type) {
	case string:
		return v
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// Int returns a config value as an int (0 when absent/non-numeric).
func (c VMConfig) Int(key string) int {
	switch v := c[key].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	default:
		return 0
	}
}

// GetVMConfig reads the current (non-pending) VM configuration.
func (c *Client) GetVMConfig(ctx context.Context, node string, vmid int) (VMConfig, error) {
	var cfg VMConfig
	if err := c.get(ctx, qemuPath(node, vmid)+"/config", nil, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SetVMConfig applies configuration changes asynchronously (POST /config → UPID).
// keys with empty values are dropped; to delete keys pass them via del.
func (c *Client) SetVMConfig(ctx context.Context, node string, vmid int, params map[string]string, del ...string) (UPID, error) {
	form := formVals(params).values()
	if len(del) > 0 {
		v := ""
		for i, d := range del {
			if i > 0 {
				v += ","
			}
			v += d
		}
		form.Set("delete", v)
	}
	return c.postTask(ctx, qemuPath(node, vmid)+"/config", form)
}

// CreateVM creates a VM from scratch (POST /nodes/{node}/qemu → UPID).
// params carries the full qemu config key set (vmid, name, cores, memory, net0, scsi0, …).
func (c *Client) CreateVM(ctx context.Context, node string, params map[string]string) (UPID, error) {
	return c.postTask(ctx, "/nodes/"+node+"/qemu", formVals(params).values())
}

// CloneOpts drives CloneVM.
type CloneOpts struct {
	NewID   int
	Name    string
	Pool    string // put the clone straight into the project pool — atomic vs a later pool update
	Target  string // target node (defaults to the template's node)
	Storage string // target storage for a full clone
	Full    bool   // full clone (templates on non-shared storage require it)
}

// CloneVM clones vmid (usually a template) into a new VM.
// ⚠ PVE serializes clones of one template with an internal lock — concurrent clones
// of the same template can 500 with "can't lock file"; callers retry with jitter.
func (c *Client) CloneVM(ctx context.Context, node string, vmid int, o CloneOpts) (UPID, error) {
	form := formVals{
		"newid":   strconv.Itoa(o.NewID),
		"name":    o.Name,
		"pool":    o.Pool,
		"target":  o.Target,
		"storage": o.Storage,
		"full":    pbool(o.Full),
	}.values()
	return c.postTask(ctx, qemuPath(node, vmid)+"/clone", form)
}

// DeleteVM destroys a VM. purge removes it from backup jobs + HA; destroyDisks
// removes owned-but-unreferenced disks too (both default on for platform-owned VMs).
func (c *Client) DeleteVM(ctx context.Context, node string, vmid int) (UPID, error) {
	form := formVals{
		"purge":                      "1",
		"destroy-unreferenced-disks": "1",
	}.values()
	return c.deleteTask(ctx, qemuPath(node, vmid), form)
}

// VMStatus is GET /nodes/{node}/qemu/{vmid}/status/current.
type VMStatus struct {
	Status    string  `json:"status"`    // "running" | "stopped"
	QMPStatus string  `json:"qmpstatus"` // "running" | "paused" | ...
	Name      string  `json:"name"`
	Lock      string  `json:"lock"`
	CPUs      float64 `json:"cpus"`
	MaxMem    int64   `json:"maxmem"`
	Mem       int64   `json:"mem"`
	Uptime    int64   `json:"uptime"`
	Agent     int     `json:"agent"`
	Template  int     `json:"template"`
}

// GetVMStatus reads the live VM state (node-scoped — the authoritative read-back
// after a task; /cluster/resources lags pmxcfs propagation).
func (c *Client) GetVMStatus(ctx context.Context, node string, vmid int) (*VMStatus, error) {
	var st VMStatus
	if err := c.get(ctx, qemuPath(node, vmid)+"/status/current", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// vmPower issues one of the status verbs. All return a UPID.
func (c *Client) vmPower(ctx context.Context, node string, vmid int, verb string, form url.Values) (UPID, error) {
	return c.postTask(ctx, qemuPath(node, vmid)+"/status/"+verb, form)
}

func (c *Client) StartVM(ctx context.Context, node string, vmid int) (UPID, error) {
	return c.vmPower(ctx, node, vmid, "start", nil)
}

// ShutdownVM is the graceful ACPI shutdown. timeoutSeconds>0 forces stop after the
// guest ignores ACPI that long.
func (c *Client) ShutdownVM(ctx context.Context, node string, vmid int, timeoutSeconds int) (UPID, error) {
	form := formVals{"timeout": itoa(timeoutSeconds), "forceStop": pbool(timeoutSeconds > 0)}.values()
	return c.vmPower(ctx, node, vmid, "shutdown", form)
}

// StopVM is the hard power-off.
func (c *Client) StopVM(ctx context.Context, node string, vmid int) (UPID, error) {
	return c.vmPower(ctx, node, vmid, "stop", nil)
}

// RebootVM is the graceful reboot; ResetVM the hard one.
func (c *Client) RebootVM(ctx context.Context, node string, vmid int) (UPID, error) {
	return c.vmPower(ctx, node, vmid, "reboot", nil)
}

func (c *Client) ResetVM(ctx context.Context, node string, vmid int) (UPID, error) {
	return c.vmPower(ctx, node, vmid, "reset", nil)
}

// ResizeDisk grows a disk (shrink is not supported by PVE). size uses PVE syntax:
// absolute ("32G") or relative ("+8G"). PUT /resize is asynchronous.
func (c *Client) ResizeDisk(ctx context.Context, node string, vmid int, disk, size string) (UPID, error) {
	form := formVals{"disk": disk, "size": size}.values()
	var upid UPID
	if err := c.put(ctx, qemuPath(node, vmid)+"/resize", form, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// MoveDiskOpts drives MoveDisk: exactly one of Storage (same VM, new storage) or
// TargetVMID+TargetDisk (reattach to another VM — PVE 8 "disk reassign").
type MoveDiskOpts struct {
	Disk       string // "scsi1", "unused0", …
	Storage    string
	TargetVMID int
	TargetDisk string
	Delete     bool // delete the source copy after a storage move
}

func (c *Client) MoveDisk(ctx context.Context, node string, vmid int, o MoveDiskOpts) (UPID, error) {
	form := formVals{
		"disk":        o.Disk,
		"storage":     o.Storage,
		"target-vmid": itoa(o.TargetVMID),
		"target-disk": o.TargetDisk,
		"delete":      pbool(o.Delete),
	}.values()
	return c.postTask(ctx, qemuPath(node, vmid)+"/move_disk", form)
}

// Snapshot is one row of GET /snapshot.
type Snapshot struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	SnapTime    int64  `json:"snaptime"`
	Parent      string `json:"parent"`
	VMState     int    `json:"vmstate"`
}

func (c *Client) ListSnapshots(ctx context.Context, node string, vmid int) ([]Snapshot, error) {
	var out []Snapshot
	if err := c.get(ctx, qemuPath(node, vmid)+"/snapshot", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateSnapshot snapshots the VM (disk-only; vmstate=false keeps it storage-cheap).
func (c *Client) CreateSnapshot(ctx context.Context, node string, vmid int, name, description string) (UPID, error) {
	form := formVals{"snapname": name, "description": description}.values()
	return c.postTask(ctx, qemuPath(node, vmid)+"/snapshot", form)
}

func (c *Client) DeleteSnapshot(ctx context.Context, node string, vmid int, name string) (UPID, error) {
	return c.deleteTask(ctx, qemuPath(node, vmid)+"/snapshot/"+url.PathEscape(name), nil)
}

func (c *Client) RollbackSnapshot(ctx context.Context, node string, vmid int, name string) (UPID, error) {
	return c.postTask(ctx, qemuPath(node, vmid)+"/snapshot/"+url.PathEscape(name)+"/rollback", nil)
}

// ToTemplate converts a stopped VM into a template.
func (c *Client) ToTemplate(ctx context.Context, node string, vmid int) (UPID, error) {
	return c.postTask(ctx, qemuPath(node, vmid)+"/template", nil)
}

// MigrateVM live/offline-migrates a VM to another node (online when running;
// with-local-disks covers non-shared storage).
func (c *Client) MigrateVM(ctx context.Context, node string, vmid int, target string, online bool) (UPID, error) {
	form := formVals{
		"target":           target,
		"online":           pbool(online),
		"with-local-disks": "1",
	}.values()
	return c.postTask(ctx, qemuPath(node, vmid)+"/migrate", form)
}

// AgentInterface is one NIC as reported by the qemu guest agent.
type AgentInterface struct {
	Name        string `json:"name"`
	HardwareMAC string `json:"hardware-address"`
	IPAddresses []struct {
		Type    string `json:"ip-address-type"` // "ipv4" | "ipv6"
		Address string `json:"ip-address"`
		Prefix  int    `json:"prefix"`
	} `json:"ip-addresses"`
}

// AgentNetworkInterfaces asks the guest agent for live NIC/IP data. Fails when the
// agent isn't installed/running in the guest — callers treat that as "no IPs yet".
func (c *Client) AgentNetworkInterfaces(ctx context.Context, node string, vmid int) ([]AgentInterface, error) {
	var out struct {
		Result []AgentInterface `json:"result"`
	}
	if err := c.get(ctx, qemuPath(node, vmid)+"/agent/network-get-interfaces", nil, &out); err != nil {
		return nil, err
	}
	return out.Result, nil
}

// SuspendVM pauses a running VM (billing suspension leg).
func (c *Client) SuspendVM(ctx context.Context, node string, vmid int) (UPID, error) {
	return c.vmPower(ctx, node, vmid, "suspend", nil)
}

// ResumeVM resumes a paused VM.
func (c *Client) ResumeVM(ctx context.Context, node string, vmid int) (UPID, error) {
	return c.vmPower(ctx, node, vmid, "resume", nil)
}
