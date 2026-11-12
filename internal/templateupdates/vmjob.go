package templateupdates

// vmjob.go — the VM template update job: a published, verified VM artifact is streamed from GHCR
// into the provider's import store and imported as a NEW template VM. Steps are fenced by the
// durable job lease (Fence.Checkpoint) and recorded in a receipt so a new acquisition resumes from
// the last committed step instead of repeating external effects.
//
// Invariants:
//   - never modify or delete an existing template: the only VM this job ever mutates or destroys
//     is one whose tags carry this job's ownership tag (pc-tu-<job>) and that is not published;
//   - the only import file it ever deletes is one whose name carries this job's prefix;
//   - bytes are verified three times: the tee SHA-256 + byte count while streaming (bounded by the
//     manifest's disk.bytes), PVE's own checksum (sha256 = manifest disk.sha256) on upload, and the
//     layer digest == sha256:<disk.sha256> checked at discovery and enqueue.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// VMPayload is the frozen, secret-free job payload. Identity (job key) = service/node/digest.
type VMPayload struct {
	Version           int    `json:"version"`
	Package           string `json:"package"`
	Tag               string `json:"tag"`
	Digest            string `json:"digest"` // OCI manifest digest
	DiskDigest        string `json:"diskDigest"`
	DiskBytes         int64  `json:"diskBytes"`
	VirtualSizeBytes  int64  `json:"virtualSizeBytes"`
	OS                string `json:"os"`
	Variant           string `json:"variant"`
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	BuildVersion      string `json:"buildVersion"`
	ServiceID         string `json:"serviceId"`
	Node              string `json:"node"`
	ImportStorage     string `json:"importStorage"`
	DiskStorage       string `json:"diskStorage"`
	Publish           bool   `json:"publish"`
}

var (
	nodeRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,62}$`)
	storageRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,99}$`)
	serviceRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
)

// Key is the durable job identity: the same artifact onto the same provider node is one job.
func (p VMPayload) Key() string { return p.ServiceID + "/" + p.Node + "/" + p.Digest }

// Validate checks every field against the contract; a decoded payload that fails is permanent.
func (p VMPayload) Validate() error {
	info, ok := ParseTag(p.Tag)
	pkg := "proxcloud-vm-" + info.OS
	if info.Variant == "kubernetes" {
		pkg += "-k8s"
	}
	switch {
	case p.Version != 1, !ok, p.Package != pkg, info.OS != p.OS, info.Variant != p.Variant, info.KubernetesVersion != p.KubernetesVersion, info.BuildVersion != p.BuildVersion:
		return errors.New("payload identity does not match its tag")
	case !digestRE.MatchString(p.Digest), !digestRE.MatchString(p.DiskDigest):
		return errors.New("payload digests malformed")
	case p.DiskBytes <= 0 || p.DiskBytes >= MaxDiskBytes, p.VirtualSizeBytes < MinVirtualSizeBytes || p.VirtualSizeBytes > MaxVirtualSizeBytes:
		return errors.New("payload sizes outside the contract")
	case !serviceRE.MatchString(p.ServiceID), !nodeRE.MatchString(p.Node), !storageRE.MatchString(p.ImportStorage), !storageRE.MatchString(p.DiskStorage):
		return errors.New("payload placement malformed")
	}
	return nil
}

// DecodeVMPayload strictly decodes a payload (unknown fields refused).
func DecodeVMPayload(raw []byte) (VMPayload, error) {
	var p VMPayload
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

// Step is the receipt's position in the state machine.
type Step string

const (
	StepQueued    Step = "queued"
	StepUploading Step = "uploading" // intent: upload file name recorded, bytes in flight
	StepUploaded  Step = "uploaded"  // PVE task OK: checksum verified, file in the import store
	StepCreating  Step = "creating"  // intent: VMID recorded before the create call
	StepImported  Step = "imported"  // import-from done, VM not yet a template
	StepTemplated Step = "templated" // converted, final name + tags set
	StepSucceeded Step = "succeeded"
	StepFailed    Step = "failed"
)

// Receipt is the durable per-job record (collection templateUpdate, id = job id). It holds no
// secret; Message is a fixed classification text, never a provider error string.
type Receipt struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"` // "vm"
	Payload     VMPayload `json:"payload"`
	RequestedBy string    `json:"requestedBy"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Step        Step      `json:"step"`
	Epoch       int64     `json:"epoch"` // job acquisition epoch that wrote this step
	UploadFile  string    `json:"uploadFile,omitempty"`
	VMID        int       `json:"vmid,omitempty"`
	TemplateTag string    `json:"templateTags,omitempty"`
	Code        string    `json:"code,omitempty"`
	Message     string    `json:"message,omitempty"`
	Warnings    []string  `json:"warnings,omitempty"`
}

// Fence commits receipt writes only while the job acquisition still owns its lease
// (durablejobs.Store.Checkpoint bound to one Job). ErrLeaseLost → stop all external work.
type Fence interface {
	Checkpoint(ctx context.Context, fn func(context.Context) error) error
	Epoch() int64
}

// Receipts persists receipts (inside the Fence transaction when called from Checkpoint).
type Receipts interface {
	Load(ctx context.Context, id string) (*Receipt, error)
	Save(ctx context.Context, r *Receipt) error
}

// PVE is the subset of *pve.Client the executor uses.
type PVE interface {
	UploadSpace(ctx context.Context) (pve.StorageNodeState, error)
	UploadImportFile(ctx context.Context, node, storage, filename string, src io.Reader, size int64, checksum, algorithm string) (pve.UPID, error)
	TaskStopped(ctx context.Context, upid pve.UPID, wait time.Duration) bool
	TaskStatus(ctx context.Context, upid pve.UPID) (*pve.TaskStatus, error)
	WaitTask(ctx context.Context, upid pve.UPID) error
	StorageContents(ctx context.Context, node, storage, content string, vmid int) ([]pve.StorageContent, error)
	DeleteStorageVolume(ctx context.Context, node, storage, vol string) error
	ClusterResources(ctx context.Context, kind string) ([]pve.ClusterResource, error)
	CreateVM(ctx context.Context, node string, params map[string]string) (pve.UPID, error)
	SetVMConfig(ctx context.Context, node string, vmid int, params map[string]string, del ...string) (pve.UPID, error)
	GetVMConfig(ctx context.Context, node string, vmid int) (pve.VMConfig, error)
	ToTemplate(ctx context.Context, node string, vmid int) (pve.UPID, error)
	DeleteVM(ctx context.Context, node string, vmid int) (pve.UPID, error)
}

// BlobSource opens a blob by digest (the GHCR client, built from the integration's token at run
// time — the token is never part of the payload or receipt).
type BlobSource interface {
	OpenBlob(ctx context.Context, pkg, digest string) (io.ReadCloser, error)
}

// Permanent marks a failure that retrying cannot fix (bad bytes, refused contract, missing rights).
type Permanent struct {
	Code, Message string
}

func (e *Permanent) Error() string { return e.Code + ": " + e.Message }

func permanent(code, msg string) error { return &Permanent{Code: code, Message: msg} }

// ErrStopped is returned when the fence was lost: the executor performed no further external step.
var ErrStopped = errors.New("template update: lease lost, stopped before the next external step")

// templateVMIDStart mirrors images/build.py next_vmid: platform templates live from 9200 up.
const templateVMIDStart = 9200

// OwnerTag is the ownership tag a job stamps on the only VM it may mutate or destroy.
func OwnerTag(jobID string) string {
	return "pc-tu-" + strings.ReplaceAll(jobID, "-", "")[:16]
}

// uploadName is the job's import file for one acquisition epoch: a fresh name per epoch, so a
// superseded runner still uploading can never collide with the current one.
func uploadName(jobID string, epoch int64, digest string) string {
	return fmt.Sprintf("%s-e%d-%s.qcow2", OwnerTag(jobID), epoch, strings.TrimPrefix(digest, "sha256:")[:12])
}

// TemplateName is the final template name. Never pc-<os>-…: images/build.py treats an unpublished
// pc-<os>-* VM as its own failed build and destroys it.
func TemplateName(p VMPayload) string {
	v := "os"
	if p.Variant == "kubernetes" {
		v = "k8s-" + strings.ReplaceAll(p.KubernetesVersion, ".", "-")
	}
	return fmt.Sprintf("pc-ghcr-%s-%s-%s", p.OS, v, p.BuildVersion)
}

// TemplateTags are the final tags. A native OS template joins the catalog (pc-public + pc-img-<os>
// + pc-v-<yyyymmddhhmm>) only when Publish was confirmed; otherwise it is staged for review. A
// Kubernetes node image is never a catalog entry: an operator references its VMID in the kamaji
// provider's version map.
func TemplateTags(p VMPayload, jobID string) string {
	tags := []string{pve.TagManaged, OwnerTag(jobID), "pc-src-" + strings.TrimPrefix(p.Digest, "sha256:")[:12]}
	if p.Variant == "kubernetes" {
		tags = append(tags, "pc-tu-k8s", "pc-k8sver-"+strings.ReplaceAll(p.KubernetesVersion, ".", "-"))
	} else {
		tags = append(tags, pve.TagTemplateImage+p.OS, pve.TagTemplateVersion+p.BuildVersion[:12])
		if p.Publish {
			tags = append(tags, pve.TagTemplatePub)
		} else {
			tags = append(tags, "pc-tu-staged")
		}
	}
	return strings.Join(tags, ";")
}

// Executor runs one acquisition of a VM template update job.
type Executor struct {
	PVE      PVE
	Blobs    BlobSource
	Receipts Receipts
	// HeadroomPct is the provider's storage headroom (a fraction of capacity kept free).
	HeadroomPct float64
	// TaskWait bounds a PVE upload/import task (default pve.ImportTimeout).
	TaskWait time.Duration
}

// checkpoint persists r through the fence. Any error (lease lost included) stops the executor.
func (x *Executor) checkpoint(ctx context.Context, f Fence, r *Receipt, step Step, mut func(*Receipt)) error {
	next := *r
	next.Step, next.Epoch, next.UpdatedAt = step, f.Epoch(), time.Now().UTC()
	if mut != nil {
		mut(&next)
	}
	if err := f.Checkpoint(ctx, func(tc context.Context) error { return x.Receipts.Save(tc, &next) }); err != nil {
		return fmt.Errorf("%w: %v", ErrStopped, err)
	}
	*r = next
	return nil
}

// guard proves the lease is still held (a no-op fenced commit) immediately before an external
// mutation. A superseded acquisition therefore performs no further external step.
func (x *Executor) guard(ctx context.Context, f Fence) error {
	if err := f.Checkpoint(ctx, nil); err != nil {
		return fmt.Errorf("%w: %v", ErrStopped, err)
	}
	return nil
}

// Run executes the job from its last receipt. It returns nil on success (receipt StepTemplated;
// the runner then marks it succeeded), *Permanent for a terminal failure, ErrStopped (wrapped) when
// the fence was lost, or another error to retry later.
func (x *Executor) Run(ctx context.Context, jobID string, p VMPayload, f Fence) error {
	if err := p.Validate(); err != nil {
		return permanent("invalid_payload", "the job payload failed validation")
	}
	r, err := x.Receipts.Load(ctx, jobID)
	if err != nil {
		return err
	}
	if r == nil || r.Payload != p {
		return permanent("receipt_mismatch", "the job receipt is missing or belongs to another payload")
	}
	if x.TaskWait <= 0 {
		x.TaskWait = pve.ImportTimeout
	}
	switch r.Step {
	case StepTemplated, StepSucceeded:
		return nil
	case StepFailed:
		return permanent(r.Code, r.Message)
	}
	// 1. Upload (fresh file name per epoch). A verified upload of an earlier epoch is reused only
	// while it is still in the import store.
	if r.Step == StepUploaded {
		ok, err := x.uploadPresent(ctx, jobID, p, r.UploadFile)
		if err != nil {
			return err
		}
		if !ok {
			r.Step = StepQueued
		}
	}
	if r.Step == StepQueued || r.Step == StepUploading {
		if err := x.upload(ctx, jobID, p, r, f); err != nil {
			return err
		}
	}
	// 2. Create + import (VMID recorded before the create call).
	if r.Step == StepUploaded || r.Step == StepCreating {
		if err := x.createAndImport(ctx, jobID, p, r, f); err != nil {
			return err
		}
	}
	// 3. Convert to a template, then name and tag it.
	if r.Step == StepImported {
		if err := x.finish(ctx, jobID, p, r, f); err != nil {
			return err
		}
	}
	// 4. Best-effort, fenced: drop this job's import files and any orphan VM a superseded
	// acquisition of this job left half-created (never the template itself).
	if x.guard(ctx, f) == nil {
		warns := x.sweepOrphans(ctx, f, jobID, p, r.VMID)
		warns = append(warns, x.dropOwnFiles(ctx, f, jobID, p, r, "")...)
		if len(warns) > 0 {
			_ = x.checkpoint(ctx, f, r, r.Step, func(n *Receipt) { n.Warnings = append(n.Warnings, warns...) })
		}
	}
	return nil
}

func (x *Executor) ownFile(jobID, volid string, p VMPayload) bool {
	return strings.HasPrefix(volid, p.ImportStorage+":import/"+OwnerTag(jobID)+"-")
}

// uploadPresent reports whether the receipt's upload is still in the import store. PVE only keeps
// an upload after its checksum task succeeded, and StepUploaded is committed only after that task
// reported OK, so presence + name is the resume proof (an intent-only StepUploading never is).
func (x *Executor) uploadPresent(ctx context.Context, jobID string, p VMPayload, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	items, err := x.PVE.StorageContents(ctx, p.Node, p.ImportStorage, "import", 0)
	if err != nil {
		return false, err
	}
	want := p.ImportStorage + ":import/" + name
	for _, it := range items {
		if it.VolID == want && x.ownFile(jobID, it.VolID, p) {
			return true, nil
		}
	}
	return false, nil
}

func (x *Executor) upload(ctx context.Context, jobID string, p VMPayload, r *Receipt, f Fence) error {
	// A StepUploading receipt is only an intent: its outcome is unknown, so the bytes are uploaded
	// again under this epoch's own name (the earlier file, if any, is swept as this job's own).
	space, err := x.PVE.UploadSpace(ctx)
	if err != nil {
		if pve.IsForbidden(err) {
			return permanent("pve_forbidden", "the provider token cannot read the API node's disk space (Sys.Audit on /nodes)")
		}
		return err
	}
	if !pve.StorageHasRoom(space, p.DiskBytes, x.HeadroomPct) {
		return errors.New("no room on the API node for the upload right now")
	}
	name := uploadName(jobID, f.Epoch(), p.DiskDigest)
	if err := x.checkpoint(ctx, f, r, StepUploading, func(n *Receipt) { n.UploadFile = name }); err != nil {
		return err
	}
	if err := x.guard(ctx, f); err != nil {
		return err
	}
	body, err := x.Blobs.OpenBlob(ctx, p.Package, p.DiskDigest)
	if err != nil {
		return err
	}
	defer body.Close()
	// Bounded by the manifest's disk.bytes; the final read fails on a length or hash mismatch,
	// which aborts the HTTP body so PVE never completes the file.
	vr := NewVerifyingReader(body, p.DiskDigest, p.DiskBytes)
	up, err := x.PVE.UploadImportFile(ctx, p.Node, p.ImportStorage, name, vr, p.DiskBytes, strings.TrimPrefix(p.DiskDigest, "sha256:"), "sha256")
	if errors.Is(err, ErrDigestMismatch) || errors.Is(err, ErrSizeMismatch) || errors.Is(vr.err, ErrDigestMismatch) || errors.Is(vr.err, ErrSizeMismatch) {
		return permanent("digest_mismatch", "the downloaded disk does not match the manifest's sha256/bytes")
	}
	if err != nil {
		if pve.IsForbidden(err) {
			return permanent("pve_forbidden", "the provider token cannot upload to the import store (Datastore.AllocateTemplate)")
		}
		return err
	}
	if vr.N() != p.DiskBytes || vr.Sum() != strings.TrimPrefix(p.DiskDigest, "sha256:") {
		return permanent("digest_mismatch", "the streamed disk does not match the manifest's sha256/bytes")
	}
	if !x.PVE.TaskStopped(ctx, up, x.TaskWait) {
		return errors.New("upload task did not finish in time")
	}
	st, err := x.PVE.TaskStatus(context.WithoutCancel(ctx), up)
	if err != nil {
		return err
	}
	if st.ExitStatus != "OK" {
		return permanent("pve_checksum_refused", "Proxmox refused the upload (checksum mismatch, invalid qcow2 or a backing file)")
	}
	return x.checkpoint(ctx, f, r, StepUploaded, nil)
}

// owned reports whether vmid on node is a VM this job created: its tags carry the job's ownership
// tag and it is not a published template.
func (x *Executor) owned(ctx context.Context, jobID string, p VMPayload, vmid int) (exists, own bool, cfg pve.VMConfig, err error) {
	cfg, err = x.PVE.GetVMConfig(ctx, p.Node, vmid)
	if pve.IsNotFound(err) {
		return false, false, nil, nil
	}
	if err != nil {
		return false, false, nil, err
	}
	tags := strings.Split(cfg.Str("tags"), ";")
	mine, published := false, false
	for _, t := range tags {
		mine = mine || t == OwnerTag(jobID)
		published = published || t == pve.TagTemplatePub
	}
	return true, mine && !published, cfg, nil
}

func (x *Executor) nextTemplateID(ctx context.Context, avoid int) (int, error) {
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

func (x *Executor) createAndImport(ctx context.Context, jobID string, p VMPayload, r *Receipt, f Fence) error {
	if r.Step == StepCreating && r.VMID > 0 {
		exists, own, cfg, err := x.owned(ctx, jobID, p, r.VMID)
		if err != nil {
			return err
		}
		switch {
		case exists && own && cfg.Str("scsi0") != "":
			return x.checkpoint(ctx, f, r, StepImported, nil)
		case exists && own:
			// A half-created VM of ours (import-from never landed): remove it and start over.
			if err := x.destroyOwned(ctx, f, jobID, p, r.VMID); err != nil {
				return err
			}
		case exists:
			// Someone else holds that VMID now: never touch it, pick another.
		}
	}
	upload := p.ImportStorage + ":import/" + r.UploadFile
	if !x.ownFile(jobID, upload, p) {
		return permanent("receipt_mismatch", "the receipt's upload is not this job's file")
	}
	if ok, err := x.uploadPresent(ctx, jobID, p, r.UploadFile); err != nil {
		return err
	} else if !ok {
		// The verified file is gone (removed by an operator): upload again on the next attempt.
		if err := x.checkpoint(ctx, f, r, StepQueued, func(n *Receipt) { n.UploadFile = "" }); err != nil {
			return err
		}
		return errors.New("the verified upload is no longer in the import store; it will be uploaded again")
	}
	var vmid int
	for attempt := 0; ; attempt++ {
		id, err := x.nextTemplateID(ctx, r.VMID)
		if err != nil {
			return err
		}
		if err := x.checkpoint(ctx, f, r, StepCreating, func(n *Receipt) { n.VMID = id }); err != nil {
			return err
		}
		if err := x.guard(ctx, f); err != nil {
			return err
		}
		up, err := x.PVE.CreateVM(ctx, p.Node, map[string]string{
			"vmid": strconv.Itoa(id), "pool": pve.TemplatePool, "name": "pc-tu-build-" + OwnerTag(jobID)[6:], "tags": pve.TagManaged + ";" + OwnerTag(jobID),
		})
		if err == nil {
			err = x.PVE.WaitTask(ctx, up)
		}
		if err != nil && attempt < 2 && strings.Contains(err.Error(), "already exists") {
			continue
		}
		if err != nil {
			if pve.IsForbidden(err) {
				return permanent("pve_forbidden", "the provider token cannot create VMs in pool pc-templates")
			}
			return err
		}
		vmid = id
		break
	}
	exists, own, _, err := x.owned(ctx, jobID, p, vmid)
	if err != nil {
		return err
	}
	if !exists || !own {
		return permanent("ownership_lost", "the created VM does not carry this job's ownership tag")
	}
	// The pipeline template shape (images/build.py, pve.ImportImage), without a NIC.
	if err := x.guard(ctx, f); err != nil {
		return err
	}
	up, err := x.PVE.SetVMConfig(ctx, p.Node, vmid, map[string]string{
		"ostype": "l26", "cores": "2", "memory": "2048", "cpu": "host", "agent": "1",
		"scsihw": "virtio-scsi-single", "serial0": "socket", "vga": "serial0", "boot": "order=scsi0",
		"scsi0": fmt.Sprintf("%s:0,discard=on,import-from=%s", p.DiskStorage, upload),
		"ide2":  p.DiskStorage + ":cloudinit",
	})
	if err == nil {
		err = x.PVE.WaitTask(ctx, up)
	}
	if err != nil {
		return permanent("pve_import_refused", "Proxmox could not import the disk (invalid qcow2, or the disk storage refused it)")
	}
	return x.checkpoint(ctx, f, r, StepImported, nil)
}

func (x *Executor) finish(ctx context.Context, jobID string, p VMPayload, r *Receipt, f Fence) error {
	exists, own, cfg, err := x.owned(ctx, jobID, p, r.VMID)
	if err != nil {
		return err
	}
	if !exists || !own {
		return permanent("ownership_lost", "the imported VM is gone or no longer this job's")
	}
	if !isTemplate(cfg) {
		if err := x.guard(ctx, f); err != nil {
			return err
		}
		up, err := x.PVE.ToTemplate(ctx, p.Node, r.VMID)
		if err == nil {
			err = x.PVE.WaitTask(ctx, up)
		}
		if err != nil {
			return err
		}
	}
	tags := TemplateTags(p, jobID)
	if err := x.checkpoint(ctx, f, r, StepImported, func(n *Receipt) { n.TemplateTag = tags }); err != nil {
		return err
	}
	if err := x.guard(ctx, f); err != nil {
		return err
	}
	up, err := x.PVE.SetVMConfig(ctx, p.Node, r.VMID, map[string]string{
		"name": TemplateName(p), "tags": tags,
		"description": fmt.Sprintf("ProxCloud template imported from ghcr.io/%s/%s@%s (tag %s, disk sha256 %s)", Owner, p.Package, p.Digest, p.Tag, strings.TrimPrefix(p.DiskDigest, "sha256:")),
	})
	if err == nil {
		err = x.PVE.WaitTask(ctx, up)
	}
	if err != nil {
		return err
	}
	return x.checkpoint(ctx, f, r, StepTemplated, nil)
}

func isTemplate(cfg pve.VMConfig) bool {
	s := cfg.Str("template")
	return s == "1" || s == "true"
}

// destroyOwned deletes vmid only after re-reading it carries this job's ownership tag, and only
// right after a successful fence check.
func (x *Executor) destroyOwned(ctx context.Context, f Fence, jobID string, p VMPayload, vmid int) error {
	exists, own, _, err := x.owned(ctx, jobID, p, vmid)
	if err != nil || !exists {
		return err
	}
	if !own {
		return nil
	}
	if err := x.guard(ctx, f); err != nil {
		return err
	}
	up, err := x.PVE.DeleteVM(ctx, p.Node, vmid)
	if err == nil {
		err = x.PVE.WaitTask(ctx, up)
	}
	return err
}

// dropOwnFiles deletes this job's import files (all epochs) except keep. Best-effort: a missing
// Datastore.Allocate right leaves the file and a receipt warning.
func (x *Executor) dropOwnFiles(ctx context.Context, f Fence, jobID string, p VMPayload, r *Receipt, keep string) []string {
	items, err := x.PVE.StorageContents(ctx, p.Node, p.ImportStorage, "import", 0)
	if err != nil {
		return []string{"could not list the import store to remove this job's upload"}
	}
	var warns []string
	for _, it := range items {
		if !x.ownFile(jobID, it.VolID, p) || it.VolID == keep {
			continue
		}
		vol := strings.TrimPrefix(it.VolID, p.ImportStorage+":")
		if x.guard(ctx, f) != nil {
			return append(warns, "stopped: the job's lease was lost")
		}
		if err := x.PVE.DeleteStorageVolume(ctx, p.Node, p.ImportStorage, vol); err != nil {
			warns = append(warns, "the uploaded file "+it.VolID+" stays on the import store (needs Datastore.Allocate there)")
		}
	}
	return warns
}

// sweepOrphans deletes VMs on the job's node that carry this job's ownership tag, are not
// templates and are not keep (a create a superseded acquisition finished after losing its lease).
func (x *Executor) sweepOrphans(ctx context.Context, f Fence, jobID string, p VMPayload, keep int) []string {
	rows, err := x.PVE.ClusterResources(ctx, "vm")
	if err != nil {
		return []string{"could not list VMs to check for this job's orphans"}
	}
	var warns []string
	for _, row := range rows {
		if row.Type != "qemu" || row.Node != p.Node || row.VMID == keep || row.Template == 1 || !hasTagValue(row.Tags, OwnerTag(jobID)) {
			continue
		}
		if err := x.destroyOwned(ctx, f, jobID, p, row.VMID); errors.Is(err, ErrStopped) {
			return append(warns, "stopped: the job's lease was lost")
		} else if err != nil {
			warns = append(warns, fmt.Sprintf("orphan VM %d of this job could not be removed", row.VMID))
		}
	}
	return warns
}

func hasTagValue(tags, want string) bool {
	for _, t := range strings.Split(tags, ";") {
		if t == want {
			return true
		}
	}
	return false
}

// Cleanup runs after a terminal failure: it removes only resources this job created — a VM still
// carrying its ownership tag (never a published template) and its own import files.
// Each delete is preceded by a successful fence check: a lost lease stops cleanup immediately.
func (x *Executor) Cleanup(ctx context.Context, f Fence, jobID string, p VMPayload, r *Receipt) []string {
	var warns []string
	if r != nil && r.VMID > 0 {
		if err := x.destroyOwned(ctx, f, jobID, p, r.VMID); errors.Is(err, ErrStopped) {
			return append(warns, "stopped: the job's lease was lost")
		} else if err != nil {
			warns = append(warns, fmt.Sprintf("VM %d of this job could not be removed", r.VMID))
		}
	}
	warns = append(warns, x.sweepOrphans(ctx, f, jobID, p, 0)...)
	return append(warns, x.dropOwnFiles(ctx, f, jobID, p, r, "")...)
}

// NewJobID is a fresh job id (exported for the admin enqueue path).
func NewJobID() string { return uuid.NewString() }
