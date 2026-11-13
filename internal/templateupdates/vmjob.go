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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
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
	// Publish is ignored: imports always land staged (kept so older payloads still decode).
	Publish bool `json:"publish"`
}

var (
	nodeRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,62}$`)
	storageRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,99}$`)
	serviceRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
)

// Key is the durable job identity: the same artifact onto the same provider node is one job. It is
// a SHA-256 of service/node/digest, so it never exceeds the 256-byte durable job key limit whatever
// the service id length.
func (p VMPayload) Key() string {
	sum := sha256.Sum256([]byte(p.ServiceID + "\x00" + p.Node + "\x00" + p.Digest))
	return "tu-vm-" + hex.EncodeToString(sum[:])
}

// UploadLockKey serializes template-update uploads per PROVIDER (a pg advisory lock in the runner).
// The upload spools on the API node pveproxy answers on (one per provider at a time), whatever the
// target node or import storage, so a finer key would not protect that disk.
func (p VMPayload) UploadLockKey() string {
	return "templateupdate-upload:" + p.ServiceID
}

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
	// Cleaned marks a dead job whose leftovers the runner already removed.
	Cleaned bool `json:"cleaned,omitempty"`
	// Smoke is the latest smoke test of the template this import produced (mirrored by PGSmokes).
	Smoke *SmokeSummary `json:"smoke,omitempty"`
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
	GetPool(ctx context.Context, poolID string) (*pve.PoolInfo, error)
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

// TemplateTags are the final tags of an import. Every import lands STAGED (docs/template-updates.md,
// lifecycle): the import marker pc-tu-import, the source digest and pc-tu-staged — and NO catalog
// tag (no pc-public, pc-img-, pc-v-), so neither the tenant catalog, images/build.py nor an
// installer template selector (which match pc-img-<os> + pc-v-) can ever pick a staged import. A
// Kubernetes node image also carries pc-tu-k8s + pc-k8sver-<x-y-z> (its identity, not a catalog
// tag). Publish (lifecycle.go) adds the catalog tags after a passing smoke test.
//
// The final tags deliberately DROP the job's ownership tag: PVE clones copy tags (CAPMOX keeps
// them), so a clone of the finished template must never look like something this job may destroy.
// Destructive ownership proof ends when the template is finished.
//
// VMPayload.Publish is kept only so payloads written before the lifecycle change still decode; it
// is ignored (such a job also lands staged).
func TemplateTags(p VMPayload) string {
	tags := []string{pve.TagManaged, TagSource + strings.TrimPrefix(p.Digest, "sha256:")[:12], TagImport}
	if p.Variant == "kubernetes" {
		tags = append(tags, TagK8sImport, TagK8sVersion+strings.ReplaceAll(p.KubernetesVersion, ".", "-"))
	}
	return strings.Join(append(tags, TagStaged), ";")
}

// legacyTemplateTags are the final tags an import written before the lifecycle change set (no
// import marker, publish-at-import). finished() accepts them so a job that landed its final write
// on the old version and resumes on the new one completes instead of failing.
func legacyTemplateTags(p VMPayload) string {
	tags := []string{pve.TagManaged, TagSource + strings.TrimPrefix(p.Digest, "sha256:")[:12]}
	if p.Variant == "kubernetes" {
		tags = append(tags, TagK8sImport, TagK8sVersion+strings.ReplaceAll(p.KubernetesVersion, ".", "-"))
	} else {
		tags = append(tags, pve.TagTemplateImage+p.OS, pve.TagTemplateVersion+p.BuildVersion[:12])
		if p.Publish {
			tags = append(tags, pve.TagTemplatePub)
		} else {
			tags = append(tags, TagStaged)
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
	// UploadLock serializes uploads per provider with customer image imports (nil = none).
	UploadLock func(ctx context.Context) (func(), error)
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
	if x.UploadLock != nil {
		release, err := x.UploadLock(ctx)
		if err != nil {
			return err // busy: retried later
		}
		defer release()
	}
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

// owned reports whether vmid on node is a VM this job created and may still mutate or destroy:
//   - it is a member of pool pc-templates on this node (read from /pools/pc-templates, not the
//     possibly-lagging cluster resources). A clone in a tenant/CAPMOX pool is never ours, even if it
//     copied our tags;
//   - its tags carry the job's ownership tag (only an UNFINISHED build has it: TemplateTags drops it);
//   - it is not a published template.
func (x *Executor) owned(ctx context.Context, jobID string, p VMPayload, vmid int) (exists, own bool, cfg pve.VMConfig, err error) {
	cfg, err = x.PVE.GetVMConfig(ctx, p.Node, vmid)
	if pve.IsNotFound(err) {
		return false, false, nil, nil
	}
	if err != nil {
		return false, false, nil, err
	}
	inPool, err := x.inTemplatePool(ctx, p.Node, vmid)
	if err != nil {
		return true, false, cfg, err
	}
	tags := strings.Split(cfg.Str("tags"), ";")
	mine, published := false, false
	for _, t := range tags {
		mine = mine || t == OwnerTag(jobID)
		published = published || t == pve.TagTemplatePub
	}
	return true, inPool && mine && !published, cfg, nil
}

// inTemplatePool reads pool pc-templates' members directly.
func (x *Executor) inTemplatePool(ctx context.Context, node string, vmid int) (bool, error) {
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

// finished reports a VM already carrying exactly this job's final name and tags (a final config
// write that landed although its task wait failed). It is not "owned" any more by design.
func finished(cfg pve.VMConfig, p VMPayload) bool {
	return isTemplate(cfg) && cfg.Str("name") == TemplateName(p) &&
		(sameTags(cfg.Str("tags"), TemplateTags(p)) || sameTags(cfg.Str("tags"), legacyTemplateTags(p)))
}

func sameTags(a, b string) bool {
	set := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, t := range strings.Split(s, ";") {
			if t = strings.TrimSpace(t); t != "" {
				m[t] = true
			}
		}
		return m
	}
	x, y := set(a), set(b)
	if len(x) != len(y) {
		return false
	}
	for t := range x {
		if !y[t] {
			return false
		}
	}
	return true
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
		case exists && finished(cfg, p):
			return x.checkpoint(ctx, f, r, StepTemplated, nil)
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
	if err != nil {
		var ae *pve.APIError
		if errors.As(err, &ae) && ae.Status == 400 {
			return permanent("pve_import_refused", "Proxmox refused the import parameters (disk storage or import file)")
		}
		return err // transport/5xx: the VM is ours and empty or importing; the next attempt re-reads it
	}
	// Only a task that positively FINISHED with a non-OK exit status is a refusal of the bytes.
	if !x.PVE.TaskStopped(ctx, up, x.TaskWait) {
		return errors.New("the import task did not finish in time")
	}
	st, err := x.PVE.TaskStatus(context.WithoutCancel(ctx), up)
	if err != nil {
		return err
	}
	if st.ExitStatus != "OK" {
		return permanent("pve_import_refused", "Proxmox could not import the disk (invalid qcow2, or the disk storage refused it)")
	}
	return x.checkpoint(ctx, f, r, StepImported, nil)
}

func (x *Executor) finish(ctx context.Context, jobID string, p VMPayload, r *Receipt, f Fence) error {
	exists, own, cfg, err := x.owned(ctx, jobID, p, r.VMID)
	if err != nil {
		return err
	}
	if exists && finished(cfg, p) {
		// The final write landed in an earlier attempt (its task wait failed): done.
		return x.checkpoint(ctx, f, r, StepTemplated, nil)
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
	tags := TemplateTags(p)
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
		// Maybe it landed anyway: a retry sees finished() and completes.
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
			return append(warns, stoppedWarn)
		}
		if err := x.PVE.DeleteStorageVolume(ctx, p.Node, p.ImportStorage, vol); err != nil {
			warns = append(warns, "the uploaded file "+it.VolID+" stays on the import store (needs Datastore.Allocate there)")
		}
	}
	return warns
}

// sweepOrphans deletes VMs this job left half-created (a create a superseded acquisition finished
// after losing its lease): candidates are ONLY members of pool pc-templates on the job's node, other
// than keep, that are not templates; destroyOwned then re-reads each and requires the ownership tag,
// pool membership and no pc-public. A VM outside pc-templates (a tenant or CAPMOX clone that copied
// tags) is never even considered.
func (x *Executor) sweepOrphans(ctx context.Context, f Fence, jobID string, p VMPayload, keep int) []string {
	pool, err := x.PVE.GetPool(ctx, pve.TemplatePool)
	if err != nil || pool == nil {
		return []string{"could not read pool pc-templates to check for this job's orphans"}
	}
	var warns []string
	for _, m := range pool.Members {
		if m.Type != "qemu" || m.Node != p.Node || m.VMID == keep {
			continue
		}
		cfg, err := x.PVE.GetVMConfig(ctx, p.Node, m.VMID)
		if err != nil || isTemplate(cfg) || !hasTagValue(cfg.Str("tags"), OwnerTag(jobID)) {
			continue
		}
		row := m
		if err := x.destroyOwned(ctx, f, jobID, p, row.VMID); errors.Is(err, ErrStopped) {
			return append(warns, stoppedWarn)
		} else if err != nil {
			warns = append(warns, fmt.Sprintf("orphan VM %d of this job could not be removed", row.VMID))
		}
	}
	return warns
}

// stoppedWarn is the Cleanup warning that means the fence was lost mid-cleanup.
const stoppedWarn = "stopped: the job's lease was lost"

func stopped(warns []string) bool { return slices.Contains(warns, stoppedWarn) }

// pendingWarn is the sandbox Cleanup warning that means a store write failed (not the lease): the cleanup is not
// done and the dead-job pass retries it on its cleanup_pending throttle instead of at once on every lease.
const pendingWarn = cleanupWaiting + " the pin record could not be settled (a store error); retried every 10 minutes (if it persists, check the database, or delete the provider)"

// cleanupWaiting starts every receipt warning that says a sandbox cleanup is still to come; the dead-job pass drops
// them once the cleanup is done.
const cleanupWaiting = "cleanup waiting:"

func pending(warns []string) bool { return slices.Contains(warns, pendingWarn) }

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
	if r != nil && (r.Step == StepTemplated || r.Step == StepSucceeded) {
		// Finished: the template is the result, not a leftover. Only import files are dropped.
		return x.dropOwnFiles(ctx, f, jobID, p, r, "")
	}
	var warns []string
	if r != nil && r.VMID > 0 {
		if err := x.destroyOwned(ctx, f, jobID, p, r.VMID); errors.Is(err, ErrStopped) {
			return append(warns, stoppedWarn)
		} else if err != nil {
			warns = append(warns, fmt.Sprintf("VM %d of this job could not be removed", r.VMID))
		}
	}
	warns = append(warns, x.sweepOrphans(ctx, f, jobID, p, 0)...)
	return append(warns, x.dropOwnFiles(ctx, f, jobID, p, r, "")...)
}

// NewJobID is a fresh job id (exported for the admin enqueue path).
func NewJobID() string { return uuid.NewString() }
