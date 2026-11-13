package templateupdates

// smoke.go — the smoke test of an imported template (docs/template-updates.md, "Lifecycle"): a
// durable, fenced job of kind templateupdate.smoke/v1 on the template update runner.
//
// It clones the staged template into a short-lived test VM in pool pc-templates (never a tenant
// pool), gives it fresh cloud-init (an ephemeral SSH key generated in memory, only its SHA-256
// fingerprint recorded; no password) on the provider's smoke network, grows its root disk, starts it,
// waits for the guest agent (ping + guest-get-osinfo), checks through the agent that cloud-init
// finished (`cloud-init status --format json`) and the root filesystem grew, and for a Kubernetes
// node image that kubelet and containerd are present (no join). Then it stops and destroys the test
// VM. Nothing runs on the API host: every check goes through the Proxmox API.
//
// Invariants (the same as the import job's):
//   - every PVE mutation (clone, config, resize, start, guest exec, stop, destroy) is immediately
//     preceded by a successful fence check: a lost lease stops the job before its next effect;
//   - the only VM it ever mutates or destroys is its own test VM: a member of pool pc-templates on the
//     job's node, NOT a template, carrying this job's owner tag or (in the moment between the clone
//     and its first config write, while it still carries the template's copied tags) this job's
//     name. The template itself is only read;
//   - cleanup is always attempted: on a check failure, on any error of the attempt, after a terminal
//     failure and by the dead-job pass.
//
// A result is PASS or FAIL with a fixed reason code. A FAIL is a finished job (the smoke ran), not
// a failed one; infrastructure errors are retried and, once attempts run out, recorded as FAIL.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Smoke results.
const (
	SmokePass = "PASS"
	SmokeFail = "FAIL"
)

// Smoke failure codes (fixed). The message is composed by this package from fixed text plus bounded,
// clipped values it observed (OS id/version, cloud-init status, sizes, kubelet version) — never a
// Proxmox error string or free guest output.
const (
	SmokeTemplateMismatch  = "template_mismatch"
	SmokeDigestWithdrawn   = "digest_withdrawn"
	SmokeCloneFailed       = "clone_failed"
	SmokeConfigureFailed   = "configure_failed"
	SmokeResizeFailed      = "resize_failed"
	SmokeStartFailed       = "start_failed"
	SmokeAgentTimeout      = "agent_timeout"
	SmokeOSInfoFailed      = "osinfo_failed"
	SmokeOSInfoMismatch    = "osinfo_mismatch"
	SmokeExecForbidden     = "guest_exec_forbidden"
	SmokeCloudInitTimeout  = "cloudinit_timeout"
	SmokeCloudInitFailed   = "cloudinit_failed"
	SmokeCloudInitBadOut   = "cloudinit_unreadable"
	SmokeDiskNotGrown      = "disk_not_grown"
	SmokeDiskUnverifiable  = "disk_unverifiable"
	SmokeK8sMissing        = "k8s_components_missing"
	SmokeK8sVersion        = "k8s_version_mismatch"
	SmokePVEForbidden      = "pve_forbidden"
	SmokeAttemptsExhausted = "attempts_exhausted"
	SmokeJobDead           = "job_dead"
	SmokeInvalidPayload    = "invalid_payload"
	SmokeProviderMissing   = "provider_missing"
)

// SmokeNetwork is the provider's smoke-test network (config.templateSmoke). IP "" = DHCP.
type SmokeNetwork struct {
	Bridge     string `json:"bridge"`
	VLAN       int    `json:"vlan,omitempty"`
	IP         string `json:"ip,omitempty"` // CIDR, e.g. 192.0.2.50/24
	Gateway    string `json:"gateway,omitempty"`
	Nameserver string `json:"nameserver,omitempty"`
}

var bridgeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,31}$`)

// ErrSmokeNetworkUnset: the provider has no smoke network, so no smoke test can run.
var ErrSmokeNetworkUnset = &LifecycleError{Status: http.StatusConflict, Code: "smoke_network_unset",
	Message: "The provider has no smoke-test network: set config.templateSmoke (bridge, optional vlan, ip/gateway or DHCP) on the Proxmox provider first"}

// Validate checks the network; an unset bridge is ErrSmokeNetworkUnset. IPv4 only (cloud-init gets
// ipconfig0 ip=/gw=; an IPv6-only smoke network is not supported); addresses without zones.
func (n SmokeNetwork) Validate() error {
	if strings.TrimSpace(n.Bridge) == "" {
		return ErrSmokeNetworkUnset
	}
	bad := func(msg string) error {
		return &LifecycleError{Status: http.StatusConflict, Code: "smoke_network_invalid", Message: "config.templateSmoke: " + msg}
	}
	if !bridgeRE.MatchString(n.Bridge) {
		return bad("bridge must be a bridge or vnet name")
	}
	if n.VLAN < 0 || n.VLAN > 4094 {
		return bad("vlan must be 0 (untagged) to 4094")
	}
	if n.IP != "" {
		pfx, err := netip.ParsePrefix(n.IP)
		if err != nil || !pfx.Addr().Is4() || pfx.Addr() != pfx.Addr().Unmap() || pfx.Bits() < 8 || pfx.Bits() > 30 || pfx.Addr() == pfx.Masked().Addr() {
			return bad("ip must be an IPv4 host address in CIDR form, /8 to /30, e.g. 192.0.2.50/24 (empty = DHCP; IPv6 is not supported)")
		}
		gw, err := netip.ParseAddr(n.Gateway)
		if err != nil || !gw.Is4() || gw.Zone() != "" || !pfx.Masked().Contains(gw) || gw == pfx.Addr() {
			return bad("gateway must be another IPv4 address inside ip's network")
		}
	} else if n.Gateway != "" {
		return bad("gateway needs ip (DHCP supplies its own)")
	}
	if n.Nameserver != "" {
		ns, err := netip.ParseAddr(n.Nameserver)
		if err != nil || ns.Zone() != "" || !ns.Is4() && !ns.Is6() {
			return bad("nameserver must be a plain IP address (no zone)")
		}
	}
	return nil
}

// BridgeReader reads the bridges that carry a node's own address or default gateway
// (*pve.Client.ManagementBridges).
type BridgeReader interface {
	ManagementBridges(ctx context.Context, nodes ...string) (map[string]string, error)
}

// CheckSmokeNetwork validates n for a smoke test on node and refuses an UNTAGGED smoke network on a
// bridge that carries the node's management address or default gateway (as public networks are
// refused, pveadmin.go): the test VM would sit on the hypervisor's L2. A VLAN-tagged network on such
// a bridge is its own L2 segment and is allowed (the image pipeline builds the same way).
func CheckSmokeNetwork(ctx context.Context, c BridgeReader, node string, n SmokeNetwork) error {
	if err := n.Validate(); err != nil {
		return err
	}
	if n.VLAN > 0 {
		return nil
	}
	mgmt, err := c.ManagementBridges(ctx, node)
	if err != nil {
		return &LifecycleError{Status: http.StatusBadGateway, Code: "smoke_network_unverified",
			Message: "The node's networks could not be read to check the smoke bridge; nothing was started — try again"}
	}
	if on, ok := mgmt[n.Bridge]; ok {
		return &LifecycleError{Status: http.StatusConflict, Code: "smoke_network_management_bridge",
			Message: fmt.Sprintf("config.templateSmoke: bridge %s carries node %s's management address; use a dedicated bridge or a VLAN tag (vlan) for the smoke network", n.Bridge, on)}
	}
	return nil
}

func (n SmokeNetwork) net0() string {
	s := "virtio,bridge=" + n.Bridge
	if n.VLAN > 0 {
		s += ",tag=" + strconv.Itoa(n.VLAN)
	}
	return s
}

func (n SmokeNetwork) ipconfig0() string {
	if n.IP == "" {
		return "ip=dhcp"
	}
	return "ip=" + n.IP + ",gw=" + n.Gateway
}

// SmokePayload is the frozen, secret-free smoke job payload: exactly one template VMID + digest.
type SmokePayload struct {
	Version           int          `json:"version"`
	ImportID          string       `json:"importId"`
	Package           string       `json:"package"`
	Tag               string       `json:"tag"`
	Digest            string       `json:"digest"`
	OS                string       `json:"os"`
	Variant           string       `json:"variant"`
	KubernetesVersion string       `json:"kubernetesVersion,omitempty"`
	BuildVersion      string       `json:"buildVersion"`
	ServiceID         string       `json:"serviceId"`
	Node              string       `json:"node"`
	TemplateVMID      int          `json:"templateVmid"`
	DiskStorage       string       `json:"diskStorage"`
	Network           SmokeNetwork `json:"network"`
}

// importPayload rebuilds the identity fields of the import a smoke belongs to (TemplateName etc.).
func (p SmokePayload) importPayload() VMPayload {
	return VMPayload{Version: 1, Package: p.Package, Tag: p.Tag, Digest: p.Digest, OS: p.OS, Variant: p.Variant,
		KubernetesVersion: p.KubernetesVersion, BuildVersion: p.BuildVersion, ServiceID: p.ServiceID, Node: p.Node, DiskStorage: p.DiskStorage}
}

// Validate checks every field; a decoded payload that fails is permanent.
func (p SmokePayload) Validate() error {
	info, ok := ParseTag(p.Tag)
	pkg := "proxcloud-vm-" + info.OS
	if info.Variant == "kubernetes" {
		pkg += "-k8s"
	}
	switch {
	case p.Version != 1, !ok, p.Package != pkg, info.OS != p.OS, info.Variant != p.Variant, info.KubernetesVersion != p.KubernetesVersion, info.BuildVersion != p.BuildVersion:
		return errors.New("smoke payload identity does not match its tag")
	case !digestRE.MatchString(p.Digest):
		return errors.New("smoke payload digest malformed")
	case p.ImportID == "" || len(p.ImportID) > 64:
		return errors.New("smoke payload import id malformed")
	case !serviceRE.MatchString(p.ServiceID), !nodeRE.MatchString(p.Node), !storageRE.MatchString(p.DiskStorage), p.TemplateVMID < 100 || p.TemplateVMID > 999999999:
		return errors.New("smoke payload placement malformed")
	}
	return p.Network.Validate()
}

// DecodeSmokePayload strictly decodes a smoke payload (unknown fields refused).
func DecodeSmokePayload(raw []byte) (SmokePayload, error) {
	var p SmokePayload
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return p, errors.New("trailing payload")
	}
	return p, p.Validate()
}

// Key is the smoke job identity (one per template + attempt number n).
func (p SmokePayload) Key(n int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", p.ServiceID, p.Node, p.TemplateVMID, p.Digest)))
	k := "tu-smoke-" + hex.EncodeToString(sum[:])
	if n > 0 {
		k += "-" + strconv.Itoa(n)
	}
	return k
}

// LockKey serializes smoke tests per provider (a static smoke IP can be used by one VM at a time).
func (p SmokePayload) LockKey() string { return "templateupdate-smoke:" + p.ServiceID }

// SmokeStep is the smoke receipt's position.
type SmokeStep string

const (
	SmokeQueued   SmokeStep = "queued"
	SmokeCloning  SmokeStep = "cloning"  // intent: test VMID recorded before the clone call
	SmokeBooting  SmokeStep = "booting"  // cloned, configured, started; checks running
	SmokeCleaning SmokeStep = "cleaning" // result recorded; the test VM is being removed
	SmokeFinished SmokeStep = "finished" // result recorded and the test VM is gone
)

// SmokeChecks are the observed values (fixed fields, bounded strings).
type SmokeChecks struct {
	OSID              string `json:"osId,omitempty"`
	OSVersionID       string `json:"osVersionId,omitempty"`
	OSPrettyName      string `json:"osPrettyName,omitempty"`
	CloudInitStatus   string `json:"cloudInitStatus,omitempty"`
	TemplateDiskBytes int64  `json:"templateDiskBytes,omitempty"`
	RootFSBytes       int64  `json:"rootFsBytes,omitempty"`
	Kubelet           string `json:"kubelet,omitempty"`
}

// SmokeReceipt is the durable record of one smoke job (collection templateUpdateSmoke, id = job id).
type SmokeReceipt struct {
	ID          string       `json:"id"`
	ImportID    string       `json:"importId"`
	Payload     SmokePayload `json:"payload"`
	RequestedBy string       `json:"requestedBy"`
	Auto        bool         `json:"auto,omitempty"`
	CreatedAt   time.Time    `json:"createdAt"`
	UpdatedAt   time.Time    `json:"updatedAt"`
	Step        SmokeStep    `json:"step"`
	Epoch       int64        `json:"epoch"`
	VMID        int          `json:"vmid,omitempty"` // the test VM
	// KeyFingerprint is the SHA-256 fingerprint of the ephemeral SSH key the test VM got (the key
	// itself lived only in memory).
	KeyFingerprint string `json:"keyFingerprint,omitempty"`
	// TemplateConfigDigest is the template's PVE config digest when the clone was made: a PASS
	// vouches for exactly that configuration, and Publish requires the template to still have it.
	TemplateConfigDigest string      `json:"templateConfigDigest,omitempty"`
	StartedAt            *time.Time  `json:"startedAt,omitempty"`
	Result               string      `json:"result,omitempty"` // PASS | FAIL | "" (not decided)
	Code                 string      `json:"code,omitempty"`
	Message              string      `json:"message,omitempty"`
	FinishedAt           *time.Time  `json:"finishedAt,omitempty"` // when the result was recorded
	CleanedAt            *time.Time  `json:"cleanedAt,omitempty"`  // when the test VM was confirmed gone
	Checks               SmokeChecks `json:"checks"`
	Warnings             []string    `json:"warnings,omitempty"`
	Cleaned              bool        `json:"cleaned,omitempty"`
}

// SmokeSummary is the latest smoke of an import, kept on the import receipt for the page.
type SmokeSummary struct {
	JobID      string     `json:"jobId"`
	Step       SmokeStep  `json:"step"`
	Result     string     `json:"result,omitempty"`
	Code       string     `json:"code,omitempty"`
	Message    string     `json:"message,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

func (r *SmokeReceipt) summary() SmokeSummary {
	return SmokeSummary{JobID: r.ID, Step: r.Step, Result: r.Result, Code: r.Code, Message: r.Message, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
}

// SmokeStore persists smoke receipts (inside the Fence transaction when called from Checkpoint).
type SmokeStore interface {
	LoadSmoke(ctx context.Context, id string) (*SmokeReceipt, error)
	// SaveSmoke saves r and mirrors its summary onto the import receipt.
	SaveSmoke(ctx context.Context, r *SmokeReceipt) error
}

// SmokeOwnerTag is the tag that makes the test VM this job's to mutate and destroy.
func SmokeOwnerTag(jobID string) string {
	return "pc-tu-smoke-" + strings.ReplaceAll(jobID, "-", "")[:16]
}

// SmokeVMName is the test VM's name, set atomically by the clone.
func SmokeVMName(jobID string) string { return SmokeOwnerTag(jobID) }

// SmokePVE is the subset of *pve.Client the smoke executor uses.
type SmokePVE interface {
	ClusterResources(ctx context.Context, kind string) ([]pve.ClusterResource, error)
	GetPool(ctx context.Context, poolID string) (*pve.PoolInfo, error)
	GetVMConfig(ctx context.Context, node string, vmid int) (pve.VMConfig, error)
	CloneVM(ctx context.Context, node string, vmid int, o pve.CloneOpts) (pve.UPID, error)
	SetVMConfig(ctx context.Context, node string, vmid int, params map[string]string, del ...string) (pve.UPID, error)
	ResizeDisk(ctx context.Context, node string, vmid int, disk, size string) (pve.UPID, error)
	StartVM(ctx context.Context, node string, vmid int) (pve.UPID, error)
	StopVM(ctx context.Context, node string, vmid int) (pve.UPID, error)
	DeleteVM(ctx context.Context, node string, vmid int) (pve.UPID, error)
	GetVMStatus(ctx context.Context, node string, vmid int) (*pve.VMStatus, error)
	WaitTask(ctx context.Context, upid pve.UPID) error
	CloneRunning(ctx context.Context, node string, srcVMID int) (bool, error)
	AgentPing(ctx context.Context, node string, vmid int) error
	AgentOSInfo(ctx context.Context, node string, vmid int) (*pve.AgentOSInfo, error)
	AgentFSInfo(ctx context.Context, node string, vmid int) ([]pve.AgentFS, error)
	GuestRun(ctx context.Context, node string, vmid int, timeout time.Duration, args ...string) (exit int, stdout, stderr string, err error)
}

// SmokeExecutor runs one acquisition of a smoke job.
type SmokeExecutor struct {
	PVE      SmokePVE
	Receipts SmokeStore
	// Blocked reports whether the digest was withdrawn (nil = never).
	Blocked func(ctx context.Context, digest string) (bool, error)
	// Bounds (defaults: clone task 30m, boot 5m, cloud-init 10m, guest command 60s, poll 5s).
	TaskWait, BootTimeout, CloudInitTimeout, ExecTimeout, Poll time.Duration
	// GrowGiB is how much the test VM's root disk is grown (default 8 GiB).
	GrowGiB int
	now     func() time.Time
}

func (x *SmokeExecutor) defaults() {
	if x.TaskWait <= 0 {
		x.TaskWait = 30 * time.Minute
	}
	if x.BootTimeout <= 0 {
		x.BootTimeout = 5 * time.Minute
	}
	if x.CloudInitTimeout <= 0 {
		x.CloudInitTimeout = 10 * time.Minute
	}
	if x.ExecTimeout <= 0 {
		x.ExecTimeout = time.Minute
	}
	if x.Poll <= 0 {
		x.Poll = 5 * time.Second
	}
	if x.GrowGiB <= 0 {
		x.GrowGiB = 8
	}
	if x.now == nil {
		x.now = func() time.Time { return time.Now().UTC() }
	}
}

// refused reports a PVE refusal of the request (400) or a task that finished with a non-OK status:
// the template/settings are at fault, not the transport.
func refused(err error) bool {
	var ae *pve.APIError
	return errors.As(err, &ae) && ae.Status == http.StatusBadRequest || strings.HasPrefix(err.Error(), "pve: task ")
}

// smokeFail is a check result: the smoke ran and the template failed it.
type smokeFail struct{ code, msg string }

func (e *smokeFail) Error() string { return e.code + ": " + e.msg }

func failed(code, msg string) error { return &smokeFail{code: code, msg: msg} }

func (x *SmokeExecutor) checkpoint(ctx context.Context, f Fence, r *SmokeReceipt, step SmokeStep, mut func(*SmokeReceipt)) error {
	next := *r
	next.Step, next.Epoch, next.UpdatedAt = step, f.Epoch(), x.now()
	if mut != nil {
		mut(&next)
	}
	if err := f.Checkpoint(ctx, func(tc context.Context) error { return x.Receipts.SaveSmoke(tc, &next) }); err != nil {
		return fmt.Errorf("%w: %v", ErrStopped, err)
	}
	*r = next
	return nil
}

func (x *SmokeExecutor) guard(ctx context.Context, f Fence) error {
	if err := f.Checkpoint(ctx, nil); err != nil {
		return fmt.Errorf("%w: %v", ErrStopped, err)
	}
	return nil
}

// Run executes the smoke job from its receipt. nil = the result is recorded and the test VM is
// gone; *Permanent = terminal (the runner records it as FAIL and cleans up); ErrStopped = lease lost;
// other errors are retried (this attempt's test VM is removed before returning).
func (x *SmokeExecutor) Run(ctx context.Context, jobID string, p SmokePayload, f Fence) (err error) {
	x.defaults()
	if err := p.Validate(); err != nil {
		return permanent(SmokeInvalidPayload, "the smoke job payload failed validation")
	}
	r, err := x.Receipts.LoadSmoke(ctx, jobID)
	if err != nil {
		return err
	}
	if r == nil || r.Payload != p {
		return permanent("receipt_mismatch", "the smoke receipt is missing or belongs to another payload")
	}
	if r.Step == SmokeFinished {
		return nil
	}
	if r.Result != "" {
		// The result is recorded; only the test VM's removal is left.
		return x.finishCleanup(ctx, f, jobID, p, r)
	}
	// A new acquisition never trusts an earlier attempt's test VM (its SSH key is gone with that
	// process, and its state is unknown): remove it and start over.
	if r.VMID > 0 {
		if err := x.destroyOwned(ctx, f, jobID, p, r.VMID); err != nil {
			return err
		}
		if err := x.checkpoint(ctx, f, r, SmokeQueued, func(n *SmokeReceipt) { n.VMID = 0 }); err != nil {
			return err
		}
	}
	defer func() {
		// Cleanup is always attempted: on a retryable error, the attempt's test VM goes now rather
		// than at the next attempt. A lost lease skips it (another acquisition owns the job).
		if err == nil || errors.Is(err, ErrStopped) || r.VMID == 0 || r.Result != "" {
			return // a recorded result's cleanup resumes at the next attempt (finishCleanup)
		}
		var perm *Permanent
		if errors.As(err, &perm) {
			return // the runner's terminal path records the result, then cleans up
		}
		if cerr := x.destroyOwned(context.WithoutCancel(ctx), f, jobID, p, r.VMID); cerr == nil {
			_ = x.checkpoint(context.WithoutCancel(ctx), f, r, SmokeQueued, func(n *SmokeReceipt) { n.VMID = 0 })
		}
	}()
	checks, ferr := x.test(ctx, f, jobID, p, r)
	if ferr == nil {
		return x.record(ctx, f, jobID, p, r, SmokePass, "", "The template booted, cloud-init finished and the checks passed", checks)
	}
	var sf *smokeFail
	if errors.As(ferr, &sf) {
		return x.record(ctx, f, jobID, p, r, SmokeFail, sf.code, sf.msg, checks)
	}
	return ferr
}

// record commits the result (fenced), then removes the test VM.
func (x *SmokeExecutor) record(ctx context.Context, f Fence, jobID string, p SmokePayload, r *SmokeReceipt, result, code, msg string, checks SmokeChecks) error {
	now := x.now()
	if err := x.checkpoint(ctx, f, r, SmokeCleaning, func(n *SmokeReceipt) {
		n.Result, n.Code, n.Message, n.FinishedAt, n.Checks = result, code, msg, &now, checks
	}); err != nil {
		return err
	}
	return x.finishCleanup(ctx, f, jobID, p, r)
}

func (x *SmokeExecutor) finishCleanup(ctx context.Context, f Fence, jobID string, p SmokePayload, r *SmokeReceipt) error {
	if r.VMID > 0 {
		if err := x.destroyOwned(ctx, f, jobID, p, r.VMID); err != nil {
			return err
		}
	}
	if warns := x.sweep(ctx, f, jobID, p, r.VMID); stopped(warns) {
		return ErrStopped
	} else if len(warns) > 0 {
		// A leftover test VM could not be removed: the result stands, the receipt stays uncleaned and
		// the attempt is retried (then the dead-job pass) until it is gone.
		if err := x.checkpoint(ctx, f, r, SmokeCleaning, func(n *SmokeReceipt) { n.Warnings = mergeWarnings(n.Warnings, warns) }); err != nil {
			return err
		}
		return errors.New("a leftover test VM of this smoke job could not be removed yet")
	}
	now := x.now()
	return x.checkpoint(ctx, f, r, SmokeFinished, func(n *SmokeReceipt) { n.CleanedAt, n.Cleaned = &now, true })
}

// expectedOS maps an artifact OS slug to the guest's os-release ID and VERSION_ID.
func expectedOS(slug string) (id, version string) {
	switch {
	case strings.HasPrefix(slug, "debian-"):
		return "debian", strings.TrimPrefix(slug, "debian-")
	case strings.HasPrefix(slug, "ubuntu-") && len(slug) == len("ubuntu-2404"):
		v := strings.TrimPrefix(slug, "ubuntu-")
		return "ubuntu", v[:2] + "." + v[2:]
	}
	return "", ""
}

// test runs the clone → boot → checks sequence. A *smokeFail is a check result; any other error is
// infrastructure (retried).
func (x *SmokeExecutor) test(ctx context.Context, f Fence, jobID string, p SmokePayload, r *SmokeReceipt) (SmokeChecks, error) {
	var checks SmokeChecks
	if x.Blocked != nil {
		if b, err := x.Blocked(ctx, p.Digest); err != nil {
			return checks, err
		} else if b {
			return checks, failed(SmokeDigestWithdrawn, "the artifact was withdrawn; it is not smoke-tested any more")
		}
	}
	// The template must still be exactly this import (read only, never changed).
	tcfg, err := importTemplateBy(ctx, x.PVE, TemplateName(p.importPayload()), p.Digest, p.Node, p.TemplateVMID)
	var le *LifecycleError
	if errors.As(err, &le) {
		return checks, failed(SmokeTemplateMismatch, le.Message)
	}
	if err != nil {
		return checks, err
	}
	if tcfg.Str("digest") == "" {
		return checks, failed(SmokeTemplateMismatch, "the template's configuration digest could not be read")
	}
	base, ok := pve.DiskSizeBytes(tcfg.Str("scsi0"))
	if !ok {
		return checks, failed(SmokeTemplateMismatch, "the template has no readable scsi0 size")
	}
	checks.TemplateDiskBytes = base

	// 1. Clone (VMID recorded before the call), into pool pc-templates, full, on the template's node.
	pub, fp, err := ephemeralKey()
	if err != nil {
		return checks, err
	}
	started := x.now()
	for attempt := 0; ; attempt++ {
		id, err := x.nextID(ctx, p.TemplateVMID)
		if err != nil {
			return checks, err
		}
		if err := x.checkpoint(ctx, f, r, SmokeCloning, func(n *SmokeReceipt) {
			n.VMID, n.KeyFingerprint, n.StartedAt, n.TemplateConfigDigest = id, fp, &started, tcfg.Str("digest")
		}); err != nil {
			return checks, err
		}
		if err := x.guard(ctx, f); err != nil {
			return checks, err
		}
		up, err := x.PVE.CloneVM(ctx, p.Node, p.TemplateVMID, pve.CloneOpts{NewID: id, Name: SmokeVMName(jobID), Pool: pve.TemplatePool, Storage: p.DiskStorage, Full: true})
		if err == nil {
			err = x.wait(ctx, up)
		}
		if err != nil && attempt < 2 && (strings.Contains(err.Error(), "already exist") || strings.Contains(err.Error(), "can't lock")) {
			continue
		}
		if err != nil {
			if pve.IsForbidden(err) {
				return checks, permanent(SmokePVEForbidden, "the provider token cannot clone into pool pc-templates (VM.Clone, VM.Allocate)")
			}
			if refused(err) {
				return checks, failed(SmokeCloneFailed, "Proxmox refused or failed to clone the template")
			}
			return checks, err
		}
		break
	}
	exists, own, _, err := x.owned(ctx, jobID, p, r.VMID)
	if err != nil {
		return checks, err
	}
	if !exists || !own {
		return checks, permanent("ownership_lost", "the test VM is not this job's clone")
	}
	// 2. Fresh cloud-init: ephemeral key, no password, the smoke network; the template's copied
	// tags are replaced by the owner tag at once.
	params := map[string]string{
		"tags":        pve.TagManaged + ";pc-tu-smoke;" + SmokeOwnerTag(jobID),
		"description": fmt.Sprintf("ProxCloud smoke test of template %d (%s), job %s. Removed when the test ends.", p.TemplateVMID, p.Tag, jobID),
		"agent":       "1",
		"net0":        p.Network.net0(),
		"ipconfig0":   p.Network.ipconfig0(),
		"sshkeys":     strings.ReplaceAll(url.QueryEscape(strings.TrimSpace(pub)), "+", "%20"),
		"onboot":      "0",
	}
	if p.Network.Nameserver != "" {
		params["nameserver"] = p.Network.Nameserver
	}
	if err := x.guard(ctx, f); err != nil {
		return checks, err
	}
	// No password, and no custom cloud-init snippets the template may reference: the test VM gets
	// exactly the generated cloud-init.
	up, err := x.PVE.SetVMConfig(ctx, p.Node, r.VMID, params, "cipassword", "cicustom")
	if err == nil {
		err = x.wait(ctx, up)
	}
	if err != nil {
		if refused(err) {
			return checks, failed(SmokeConfigureFailed, "Proxmox refused the smoke VM's cloud-init/network settings (check config.templateSmoke)")
		}
		return checks, err
	}
	// 3. Grow the root disk: cloud-init's growpart must extend the root filesystem.
	if err := x.guard(ctx, f); err != nil {
		return checks, err
	}
	up, err = x.PVE.ResizeDisk(ctx, p.Node, r.VMID, "scsi0", fmt.Sprintf("+%dG", x.GrowGiB))
	if err == nil {
		err = x.wait(ctx, up)
	}
	if err != nil {
		if refused(err) {
			return checks, failed(SmokeResizeFailed, "Proxmox refused to grow the smoke VM's disk")
		}
		return checks, err
	}
	// 4. Start.
	if err := x.checkpoint(ctx, f, r, SmokeBooting, nil); err != nil {
		return checks, err
	}
	if err := x.guard(ctx, f); err != nil {
		return checks, err
	}
	up, err = x.PVE.StartVM(ctx, p.Node, r.VMID)
	if err == nil {
		err = x.wait(ctx, up)
	}
	if err != nil {
		if refused(err) {
			return checks, failed(SmokeStartFailed, "the smoke VM did not start")
		}
		return checks, err
	}
	// 5. Guest agent: ping, then guest-get-osinfo, within the boot timeout.
	bootBy := x.now().Add(x.BootTimeout)
	for {
		if err := x.PVE.AgentPing(ctx, p.Node, r.VMID); err == nil {
			break
		}
		if err := x.sleep(ctx, bootBy); err != nil {
			if ctx.Err() != nil {
				return checks, ctx.Err()
			}
			return checks, failed(SmokeAgentTimeout, fmt.Sprintf("the guest agent did not answer within %s of the start", x.BootTimeout))
		}
	}
	var osi *pve.AgentOSInfo
	for {
		if osi, err = x.PVE.AgentOSInfo(ctx, p.Node, r.VMID); err == nil && osi != nil {
			break
		}
		if err := x.sleep(ctx, bootBy); err != nil {
			if ctx.Err() != nil {
				return checks, ctx.Err()
			}
			return checks, failed(SmokeOSInfoFailed, "the guest agent answered ping but not guest-get-osinfo")
		}
	}
	checks.OSID, checks.OSVersionID, checks.OSPrettyName = clip(osi.ID, 32), clip(osi.VersionID, 32), clip(osi.PrettyName, 96)
	if id, ver := expectedOS(p.OS); id == "" || osi.ID != id || osi.VersionID != ver {
		return checks, failed(SmokeOSInfoMismatch, fmt.Sprintf("the guest reports %q %q, the artifact is %s", checks.OSID, checks.OSVersionID, p.OS))
	}
	// 6. cloud-init finished (polled through guest-exec; bounded output).
	ciBy := x.now().Add(x.CloudInitTimeout)
cloudInit:
	for {
		if err := x.guard(ctx, f); err != nil {
			return checks, err
		}
		exit, out, _, err := x.PVE.GuestRun(ctx, p.Node, r.VMID, x.ExecTimeout, "/usr/bin/cloud-init", "status", "--format", "json")
		if pve.IsForbidden(err) {
			return checks, permanent(SmokeExecForbidden, "the provider token may not run guest agent commands (VM.GuestAgent.Unrestricted on /pool/pc-templates)")
		}
		if err == nil {
			st, perr := parseCloudInitStatus(out)
			if perr != nil {
				// cloud-init before --format (e.g. 22.x): the plain `cloud-init status` line.
				if err := x.guard(ctx, f); err != nil {
					return checks, err
				}
				_, plain, _, perr2 := x.PVE.GuestRun(ctx, p.Node, r.VMID, x.ExecTimeout, "/usr/bin/cloud-init", "status")
				if perr2 == nil {
					st, perr = parsePlainCloudInitStatus(plain)
				}
			}
			if perr != nil {
				return checks, failed(SmokeCloudInitBadOut, "`cloud-init status` answered something unreadable")
			}
			checks.CloudInitStatus = clip(st, 32)
			switch st {
			case "done":
				break cloudInit
			case "degraded done":
				r.Warnings = append(r.Warnings, "cloud-init finished degraded (recoverable errors); see the guest's cloud-init status --long")
				break cloudInit
			case "error", "disabled", "degraded error":
				return checks, failed(SmokeCloudInitFailed, fmt.Sprintf("cloud-init ended %q (exit %d)", checks.CloudInitStatus, exit))
			}
		}
		if err := x.sleep(ctx, ciBy); err != nil {
			if ctx.Err() != nil {
				return checks, ctx.Err()
			}
			return checks, failed(SmokeCloudInitTimeout, fmt.Sprintf("cloud-init did not finish within %s (last status %q)", x.CloudInitTimeout, checks.CloudInitStatus))
		}
	}
	// 7. The root filesystem grew past the template's whole disk (only growpart can do that).
	root, err := x.rootFSBytes(ctx, f, p, r.VMID)
	if err != nil {
		return checks, err
	}
	checks.RootFSBytes = root
	if root <= 0 {
		return checks, failed(SmokeDiskUnverifiable, "the guest agent did not report the root filesystem size")
	}
	if root <= base {
		return checks, failed(SmokeDiskNotGrown, fmt.Sprintf("the root filesystem (%d bytes) did not grow past the template disk (%d bytes) after a %d GiB grow", root, base, x.GrowGiB))
	}
	// 8. Kubernetes node image: kubelet (of the right version) and containerd are present.
	if p.Variant == "kubernetes" {
		if err := x.guard(ctx, f); err != nil {
			return checks, err
		}
		exit, out, _, err := x.PVE.GuestRun(ctx, p.Node, r.VMID, x.ExecTimeout, "/bin/sh", "-c",
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin; command -v containerd >/dev/null || exit 3; command -v kubelet >/dev/null || exit 4; kubelet --version")
		if pve.IsForbidden(err) {
			return checks, permanent(SmokeExecForbidden, "the provider token may not run guest agent commands (VM.GuestAgent.Unrestricted on /pool/pc-templates)")
		}
		if err != nil {
			return checks, err
		}
		if exit != 0 {
			return checks, failed(SmokeK8sMissing, fmt.Sprintf("containerd or kubelet is missing in the node image (exit %d)", exit))
		}
		checks.Kubelet = clip(strings.TrimSpace(firstLine(out)), 64)
		if checks.Kubelet != "Kubernetes v"+p.KubernetesVersion {
			return checks, failed(SmokeK8sVersion, fmt.Sprintf("kubelet reports %q, the artifact is Kubernetes %s", checks.Kubelet, p.KubernetesVersion))
		}
	}
	return checks, nil
}

// rootFSBytes reads the "/" filesystem size from guest-get-fsinfo, falling back to df through the
// agent when the agent does not report sizes.
func (x *SmokeExecutor) rootFSBytes(ctx context.Context, f Fence, p SmokePayload, vmid int) (int64, error) {
	fs, err := x.PVE.AgentFSInfo(ctx, p.Node, vmid)
	if err == nil {
		for _, m := range fs {
			if m.Mountpoint == "/" && m.TotalBytes != nil {
				return *m.TotalBytes, nil
			}
		}
	}
	if err := x.guard(ctx, f); err != nil {
		return 0, err
	}
	exit, out, _, err := x.PVE.GuestRun(ctx, p.Node, vmid, x.ExecTimeout, "/usr/bin/df", "-B1", "--output=size", "/")
	if pve.IsForbidden(err) {
		return 0, permanent(SmokeExecForbidden, "the provider token may not run guest agent commands (VM.GuestAgent.Unrestricted on /pool/pc-templates)")
	}
	if err != nil || exit != 0 || len(out) > 4096 {
		return 0, nil
	}
	lines := strings.Fields(out)
	if len(lines) < 2 {
		return 0, nil
	}
	n, _ := strconv.ParseInt(lines[len(lines)-1], 10, 64)
	return n, nil
}

// mergeWarnings appends warnings not already present (a retried cleanup must not repeat them).
func mergeWarnings(have, add []string) []string {
	for _, w := range add {
		if !contains(have, w) {
			have = append(have, w)
		}
	}
	return have
}

// parseCloudInitStatus reads `cloud-init status --format json` ({"status": "..."}); output is bounded.
func parseCloudInitStatus(out string) (string, error) {
	if len(out) > 64<<10 {
		return "", errors.New("cloud-init status output too large")
	}
	var v struct {
		Status         string `json:"status"`
		ExtendedStatus string `json:"extended_status"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Status == "" {
		return "", errors.New("unreadable cloud-init status")
	}
	if v.ExtendedStatus != "" && strings.HasPrefix(v.ExtendedStatus, "degraded") {
		return v.ExtendedStatus, nil
	}
	return v.Status, nil
}

// parsePlainCloudInitStatus reads the plain `cloud-init status` output ("status: done").
func parsePlainCloudInitStatus(out string) (string, error) {
	if len(out) > 64<<10 {
		return "", errors.New("cloud-init status output too large")
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "status:"); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v, nil
			}
		}
	}
	return "", errors.New("unreadable cloud-init status")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// sleep waits one poll interval; it fails when the deadline passed or ctx ended.
func (x *SmokeExecutor) sleep(ctx context.Context, deadline time.Time) error {
	if !x.now().Before(deadline) {
		return errors.New("deadline")
	}
	t := time.NewTimer(x.Poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
	}
	if !x.now().Before(deadline) {
		return errors.New("deadline")
	}
	return nil
}

// wait bounds a PVE task by TaskWait.
func (x *SmokeExecutor) wait(ctx context.Context, up pve.UPID) error {
	wctx, cancel := context.WithTimeout(ctx, x.TaskWait)
	defer cancel()
	return x.PVE.WaitTask(wctx, up)
}

func (x *SmokeExecutor) nextID(ctx context.Context, avoid int) (int, error) {
	rows, err := x.PVE.ClusterResources(ctx, "vm")
	if err != nil {
		return 0, err
	}
	used := map[int]bool{avoid: true}
	for _, r := range rows {
		used[r.VMID] = true
	}
	id := templateVMIDStart
	for used[id] {
		id++
	}
	return id, nil
}

// ephemeralKey makes an ed25519 key pair in memory; only the public key (for cloud-init) and its
// fingerprint leave this function. The private key is never stored or logged.
func ephemeralKey() (pub, fingerprint string, err error) {
	pk, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	sp, err := ssh.NewPublicKey(pk)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " proxcloud-smoke", ssh.FingerprintSHA256(sp), nil
}

// owned reports whether vmid on the job's node is this job's test VM: a pool pc-templates member on
// that node (read from /pools), not a template, named exactly SmokeVMName(jobID), and either carrying
// the job's owner tag or — only in the window between the clone and its first config write, while
// it still carries the template's copied tags — no pc-tu-smoke tag at all. A manual clone of a test
// VM (another name) or a test VM of another job (another owner tag) is never this job's.
func (x *SmokeExecutor) owned(ctx context.Context, jobID string, p SmokePayload, vmid int) (exists, own bool, cfg pve.VMConfig, err error) {
	cfg, err = x.PVE.GetVMConfig(ctx, p.Node, vmid)
	if pve.IsNotFound(err) {
		return false, false, nil, nil
	}
	if pve.IsForbidden(err) {
		// The token reaches a VM only through its pool, so PVE answers 403, not 404, for a VMID that no longer exists
		// (verified on PVE 9.2.21: the destroyed test VM). Out of pc-templates it is not ours: out of view. Except a
		// clone of ours still running: PVE adds a clone to its pool only when the clone ends, logged under the source.
		// Clone first, pool second: a clone that ends in between is then already a member, and the 403 stands.
		if running, rerr := x.PVE.CloneRunning(ctx, p.Node, p.TemplateVMID); rerr != nil || running {
			return false, false, nil, errors.New("the test VM may still be cloning (a clone of the template is running, or the task list is unreadable); retrying later")
		}
		if inPool, perr := x.inTemplatePool(ctx, p.Node, vmid); perr != nil || inPool {
			return false, false, nil, err
		}
		return false, false, nil, nil
	}
	if err != nil {
		return false, false, nil, err
	}
	inPool, err := x.inTemplatePool(ctx, p.Node, vmid)
	if err != nil {
		return true, false, cfg, err
	}
	return true, inPool && !isTemplate(cfg) && smokeMine(cfg, jobID), cfg, nil
}

// inTemplatePool reads pool pc-templates' members directly.
func (x *SmokeExecutor) inTemplatePool(ctx context.Context, node string, vmid int) (bool, error) {
	pool, err := x.PVE.GetPool(ctx, pve.TemplatePool)
	if err != nil || pool == nil {
		return false, err
	}
	for _, m := range pool.Members {
		if m.Type == "qemu" && m.VMID == vmid && m.Node == node {
			return true, nil
		}
	}
	return false, nil
}

// smokeMine is the name + tag half of the ownership proof.
func smokeMine(cfg pve.VMConfig, jobID string) bool {
	if cfg.Str("name") != SmokeVMName(jobID) {
		return false
	}
	tags := cfg.Str("tags")
	if hasTagValue(tags, SmokeOwnerTag(jobID)) {
		return true
	}
	for _, t := range splitTags(tags) {
		if strings.HasPrefix(t, "pc-tu-smoke") {
			return false // configured by (or copied from) another smoke job
		}
	}
	return true // the clone→config window: still the template's copied tags
}

// destroyOwned stops and destroys vmid only after re-reading that it is this job's test VM, each
// mutation right after a successful fence check. A VM that is not ours is never touched.
func (x *SmokeExecutor) destroyOwned(ctx context.Context, f Fence, jobID string, p SmokePayload, vmid int) error {
	exists, own, cfg, err := x.owned(ctx, jobID, p, vmid)
	if err != nil || !exists || !own {
		return err
	}
	if cfg.Str("lock") != "" {
		return errors.New("the test VM is locked (a clone or other task is still running); retrying later")
	}
	if st, err := x.PVE.GetVMStatus(ctx, p.Node, vmid); err != nil {
		return err
	} else if st.Status != "stopped" {
		if err := x.guard(ctx, f); err != nil {
			return err
		}
		up, err := x.PVE.StopVM(ctx, p.Node, vmid)
		if err == nil {
			err = x.wait(ctx, up)
		}
		if err != nil {
			return err
		}
	}
	// Re-read: still ours, still not a template.
	if exists, own, _, err := x.owned(ctx, jobID, p, vmid); err != nil || !exists || !own {
		return err
	}
	if err := x.guard(ctx, f); err != nil {
		return err
	}
	up, err := x.PVE.DeleteVM(ctx, p.Node, vmid)
	if err == nil {
		err = x.wait(ctx, up)
	}
	if err != nil && !pve.IsNotFound(err) {
		return err
	}
	if exists, _, _, err := x.owned(ctx, jobID, p, vmid); err != nil {
		return err
	} else if exists {
		return errors.New("the test VM is still there after its destroy task")
	}
	return nil
}

// sweep removes any other test VM of this job (a clone a superseded acquisition finished after
// losing its lease): candidates are only pool pc-templates members on the node; destroyOwned
// re-proves each.
func (x *SmokeExecutor) sweep(ctx context.Context, f Fence, jobID string, p SmokePayload, keep int) []string {
	pool, err := x.PVE.GetPool(ctx, pve.TemplatePool)
	if err != nil || pool == nil {
		return []string{"could not read pool pc-templates to check for this smoke job's leftovers"}
	}
	var warns []string
	for _, m := range pool.Members {
		if m.Type != "qemu" || m.Node != p.Node || m.VMID == keep || m.VMID == p.TemplateVMID {
			continue
		}
		cfg, err := x.PVE.GetVMConfig(ctx, p.Node, m.VMID)
		if err != nil || isTemplate(cfg) || !smokeMine(cfg, jobID) {
			continue
		}
		if err := x.destroyOwned(ctx, f, jobID, p, m.VMID); errors.Is(err, ErrStopped) {
			return append(warns, stoppedWarn)
		} else if err != nil {
			warns = append(warns, fmt.Sprintf("leftover test VM %d of this smoke job could not be removed", m.VMID))
		}
	}
	return warns
}

// Cleanup removes this job's test VM(s) after a terminal failure or for a dead job. It never touches
// the template or any VM that is not this job's.
func (x *SmokeExecutor) Cleanup(ctx context.Context, f Fence, jobID string, p SmokePayload, r *SmokeReceipt) []string {
	x.defaults()
	var warns []string
	if r != nil && r.VMID > 0 {
		if err := x.destroyOwned(ctx, f, jobID, p, r.VMID); errors.Is(err, ErrStopped) {
			return append(warns, stoppedWarn)
		} else if err != nil {
			warns = append(warns, fmt.Sprintf("test VM %d of this smoke job could not be removed; remove it by hand", r.VMID))
		}
	}
	keep := 0
	if r != nil {
		keep = r.VMID
	}
	return append(warns, x.sweep(ctx, f, jobID, p, keep)...)
}

// ── receipts on pgdoc ─────────────────────────────────────────────────────────────────────────

// SmokeCollection holds one smoke receipt per smoke job (id = durable job id).
const SmokeCollection = "templateUpdateSmoke"

// PGSmokes stores smoke receipts in pgdoc (inside the fence transaction when called from
// Checkpoint) and mirrors the summary onto the import receipt.
type PGSmokes struct{ DB *pgdoc.DB }

// Ensure creates the smoke and blocklist tables (enqueue and the dead-job query read them in a
// transaction, where pgdoc cannot auto-create a table).
func (s PGSmokes) Ensure(ctx context.Context) error {
	if err := s.DB.C(SmokeCollection).Ensure(ctx); err != nil {
		return err
	}
	return s.DB.C(BlocklistCollection).Ensure(ctx)
}

func (s PGSmokes) LoadSmoke(ctx context.Context, id string) (*SmokeReceipt, error) {
	var r SmokeReceipt
	found, err := s.DB.C(SmokeCollection).Get(ctx, id, &r)
	if err != nil || !found {
		return nil, err
	}
	return &r, nil
}

func (s PGSmokes) SaveSmoke(ctx context.Context, r *SmokeReceipt) error {
	if err := s.DB.C(SmokeCollection).Upsert(ctx, r.ID, r); err != nil {
		return err
	}
	if r.ImportID == "" {
		return nil
	}
	// Only the newest smoke of the import is mirrored onto it.
	latest, err := s.latestFor(ctx, r.Payload)
	if err != nil {
		return err
	}
	if latest != nil && latest.ID != r.ID {
		return nil
	}
	_, err = s.DB.C(ReceiptCollection).SetByID(ctx, r.ImportID, pgdoc.M{"smoke": r.summary()}, nil)
	return err
}

func (s PGSmokes) latestFor(ctx context.Context, p SmokePayload) (*SmokeReceipt, error) {
	return s.Latest(ctx, p.ServiceID, p.Node, p.TemplateVMID, p.Digest)
}

// Latest is the newest smoke of exactly that template VMID + digest on that provider node.
func (s PGSmokes) Latest(ctx context.Context, serviceID, node string, vmid int, digest string) (*SmokeReceipt, error) {
	var r SmokeReceipt
	found, err := s.DB.C(SmokeCollection).FindOne(ctx, pgdoc.M{"payload.serviceId": serviceID, "payload.node": node, "payload.templateVmid": vmid, "payload.digest": digest},
		&r, pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime), pgdoc.Desc("_id")))
	if err != nil || !found {
		return nil, err
	}
	return &r, nil
}

// ForTemplate lists every smoke of a template, newest first (count = the next key suffix).
func (s PGSmokes) ForTemplate(ctx context.Context, serviceID, node string, vmid int, digest string) ([]SmokeReceipt, error) {
	out := []SmokeReceipt{}
	err := s.DB.C(SmokeCollection).Find(ctx, pgdoc.M{"payload.serviceId": serviceID, "payload.node": node, "payload.templateVmid": vmid, "payload.digest": digest},
		&out, pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime), pgdoc.Desc("_id")))
	return out, err
}

// Recent lists the newest smoke receipts.
func (s PGSmokes) Recent(ctx context.Context, limit int64) ([]SmokeReceipt, error) {
	out := []SmokeReceipt{}
	err := s.DB.C(SmokeCollection).Find(ctx, nil, &out, pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime)), pgdoc.Limit(limit))
	return out, err
}

// DeadUncleaned lists DEAD smoke jobs whose receipt is not cleaned (the runner's dead-job pass).
func (s PGSmokes) DeadUncleaned(ctx context.Context) ([]string, error) {
	if err := s.DB.C(SmokeCollection).Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool.Query(ctx, `SELECT j.id::text FROM durable_job j JOIN "templateUpdateSmoke" t ON t.id=j.id::text
	 WHERE j.kind=$1 AND j.state='dead' AND j.last_error<>'cleaned_up' AND coalesce((t.doc->>'cleaned')::boolean,false)=false
	 AND (j.last_error<>'cleanup_pending' OR j.finished_at IS NULL OR j.finished_at < clock_timestamp()-$2*interval '1 microsecond')
	 ORDER BY j.finished_at NULLS FIRST LIMIT 20`, durablejobs.TemplateUpdateSmoke, cleanupRetryAfter.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
