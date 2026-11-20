package templateupdates

// lifecycle.go — what happens to an imported template after the import job (docs/template-updates.md,
// "Lifecycle"):
//
//	Staged (hidden) → automatic smoke test → admin Publish (only after a PASS) → the newest published
//	version is the default for new VMs; older ones stay → Withdraw removes a bad version from the
//	catalog (the previous published one takes over) and blocklists its digest for good.
//
// State lives where it is authoritative: the catalog flags are PVE tags on the template (the same
// tags pve.DiscoverTemplates reads), smoke results are durable receipts, and withdrawn digests are a
// blocklist document. Running VMs are full clones and are never affected.
//
// Publish and Withdraw are synchronous admin actions (one tag write each), serialized per artifact
// digest with a PostgreSQL session advisory lock. They only ever mutate a template that proves to be
// this feature's import of that exact artifact: a member of pool pc-templates on the receipt's node,
// a template, named TemplateName(payload) and tagged pc-src-<digest prefix>. The write carries the
// config digest PVE returned, so a concurrent change of the template's config makes it fail instead of
// overwriting.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Template tags of this feature (pve.TagTemplatePub / TagTemplateImage / TagTemplateVersion /
// TagTemplateWithdrawn are the catalog's own).
const (
	// TagSource carries the first 12 hex of the artifact digest.
	TagSource = "pc-src-"
	// TagImport marks a template this feature imported. images/build.py never prunes such a template.
	TagImport = "pc-tu-import"
	// TagStaged marks an imported template that is not (yet) published.
	TagStaged = "pc-tu-staged"
	// TagK8sImport / TagK8sVersion identify an imported Kubernetes node image and its version.
	TagK8sImport  = "pc-tu-k8s"
	TagK8sVersion = "pc-k8sver-"
	// TagK8sPublished is "published" for a Kubernetes node image. It is NOT pc-public: a node image is
	// never a tenant OS catalog entry (the kamaji provider's version map names VMIDs explicitly).
	TagK8sPublished = "pc-tu-k8s-public"
)

// BlocklistCollection holds one document per withdrawn artifact digest (id = digest).
const BlocklistCollection = "templateUpdateBlocklist"

// BlockedDigest is a withdrawn artifact: it can never be imported or published again.
type BlockedDigest struct {
	Digest    string    `json:"id"`
	Package   string    `json:"package"`
	Tag       string    `json:"tag"`
	Reason    string    `json:"reason"`
	Actor     string    `json:"actor"`
	At        time.Time `json:"at"`
	ServiceID string    `json:"serviceId,omitempty"`
	Node      string    `json:"node,omitempty"`
	VMID      int       `json:"vmid,omitempty"`
	ImportID  string    `json:"importId,omitempty"`
}

// Errors of the lifecycle actions; LifecycleError carries the HTTP status and a fixed code.
var (
	// ErrPublishAtImport refuses the removed publish-at-import option (HTTP 400).
	ErrPublishAtImport = &LifecycleError{Status: http.StatusBadRequest, Code: "publish_at_import_removed",
		Message: "Publishing at import was removed: every import lands staged, is smoke-tested, then published with Publish"}
	// ErrBlocklisted refuses a withdrawn digest (import, smoke and publish).
	ErrBlocklisted = &LifecycleError{Status: http.StatusConflict, Code: "digest_withdrawn",
		Message: "This artifact digest was withdrawn and can never be imported or published again"}
	errVMDisabled = &LifecycleError{Status: http.StatusConflict, Code: "vm_updates_disabled",
		Message: "VM template updates are not enabled on this site (PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED)"}
)

// LifecycleError is a refused lifecycle action.
type LifecycleError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *LifecycleError) Error() string { return e.Message }

func lcErr(status int, code, msg string) *LifecycleError {
	return &LifecycleError{Status: status, Code: code, Message: msg}
}

// ── tags ──────────────────────────────────────────────────────────────────────────────────────

func splitTags(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ";") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// editTags removes and adds tags, keeping the order of the rest and never duplicating one.
func editTags(tags string, remove, add []string) string {
	drop := map[string]bool{}
	for _, t := range remove {
		drop[t] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range append(splitTags(tags), add...) {
		if drop[t] && !contains(add, t) || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return strings.Join(out, ";")
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func digestPrefix(d string) string { return strings.TrimPrefix(d, "sha256:")[:12] }

// slugOf is the catalog line a payload belongs to: the OS slug (pc-img-<os>, what
// pve.DiscoverTemplates groups by) or "<os>-k8s-<x.y.z>" for a Kubernetes node image.
func slugOf(p VMPayload) string {
	if p.Variant == "kubernetes" {
		return p.OS + "-k8s-" + p.KubernetesVersion
	}
	return p.OS
}

// publishedTags is the tag set of a published import. A native OS template's tags start like a
// pipeline template's (proxcloud;pc-public;pc-img-<os>;pc-v-<version>) so selectors that sort by the
// tag string (installer stages 45/56) still order pipeline and imported versions by version.
func publishedTags(tags string, p VMPayload) string {
	var lead, drop []string
	if p.Variant == "kubernetes" {
		lead = []string{pve.TagManaged, TagK8sPublished, TagK8sImport, TagK8sVersion + strings.ReplaceAll(p.KubernetesVersion, ".", "-"), pve.TagTemplateVersion + p.BuildVersion[:12]}
		// A node image is never a tenant catalog entry, whatever tags it was given by hand.
		for _, t := range splitTags(tags) {
			if t == pve.TagTemplatePub || strings.HasPrefix(t, pve.TagTemplateImage) {
				drop = append(drop, t)
			}
		}
	} else {
		lead = []string{pve.TagManaged, pve.TagTemplatePub, pve.TagTemplateImage + p.OS, pve.TagTemplateVersion + p.BuildVersion[:12]}
	}
	rest := editTags(tags, append(append([]string{TagStaged}, drop...), lead...), nil)
	return editTags(strings.Join(append(append([]string{}, lead...), splitTags(rest)...), ";"), nil, []string{TagSource + digestPrefix(p.Digest), TagImport})
}

// withdrawnTags is the tag set of a withdrawn import: out of every catalog and every pc-img-<os>
// selector (installer stages pick templates by pc-img-<os> + pc-v- without checking pc-public), the
// template itself is kept for forensics.
func withdrawnTags(tags string) string {
	var drop []string
	for _, t := range splitTags(tags) {
		if t == pve.TagTemplatePub || t == TagK8sPublished || t == TagStaged || strings.HasPrefix(t, pve.TagTemplateImage) {
			drop = append(drop, t)
		}
	}
	// The import marker stays (added for imports made before it existed): images/build.py never
	// prunes a template carrying it, so the withdrawn template is kept for forensics.
	return editTags(tags, drop, []string{pve.TagTemplateWithdrawn, TagImport})
}

// isPublished reports whether a template's tags put it in its catalog.
func isPublished(tags string, variant string) bool {
	if hasTagValue(tags, pve.TagTemplateWithdrawn) {
		return false
	}
	if variant == "kubernetes" {
		return hasTagValue(tags, TagK8sPublished)
	}
	return hasTagValue(tags, pve.TagTemplatePub)
}

// servesLine reports whether a cluster row is a published, usable template of p's catalog line
// (whoever built it): what remains for new VMs when p's copies leave the catalog.
func servesLine(r pve.ClusterResource, p VMPayload) bool {
	if r.Type != "qemu" || r.Template != 1 || r.NodeDown || r.Pool != pve.TemplatePool || hasTagValue(r.Tags, pve.TagTemplateWithdrawn) {
		return false
	}
	if p.Variant == "kubernetes" {
		ver := strings.ReplaceAll(p.KubernetesVersion, ".", "-")
		imported := hasTagValue(r.Tags, TagK8sPublished) && pve.TagValue(r.Tags, TagK8sVersion) == ver && strings.HasPrefix(r.Name, "pc-ghcr-"+p.OS+"-")
		pipeline := hasTagValue(r.Tags, "pc-k8s") && hasTagValue(r.Tags, "pc-k8s-"+ver) && hasTagValue(r.Tags, "pc-k8s-os-"+p.OS)
		return imported || pipeline
	}
	return hasTagValue(r.Tags, pve.TagTemplatePub) && pve.TagValue(r.Tags, pve.TagTemplateImage) == p.OS
}

// servesAnyK8s reports whether a cluster row is a published Kubernetes node image of that OS, of any
// Kubernetes version (imported or built by the pipeline).
func servesAnyK8s(r pve.ClusterResource, os string) bool {
	if r.Type != "qemu" || r.Template != 1 || r.NodeDown || r.Pool != pve.TemplatePool || hasTagValue(r.Tags, pve.TagTemplateWithdrawn) {
		return false
	}
	return hasTagValue(r.Tags, TagK8sPublished) && strings.HasPrefix(r.Name, "pc-ghcr-"+os+"-") ||
		hasTagValue(r.Tags, "pc-k8s") && hasTagValue(r.Tags, "pc-k8s-os-"+os)
}

// copyOf reports whether a cluster row is a finished import of p's artifact (any node).
func copyOf(r pve.ClusterResource, p VMPayload) bool {
	return r.Type == "qemu" && r.Template == 1 && r.Pool == pve.TemplatePool && r.Name == TemplateName(p) &&
		hasTagValue(r.Tags, TagSource+digestPrefix(p.Digest))
}

// Holds reports whether a cluster row is the template this receipt's import finished and still names: the import
// reached its template, the template was not deleted since, and the row at the receipt's node and VM id is a copy of
// its artifact (copyOf). A VM id alone proves nothing: an import that failed after it took an id, or a template
// deleted by hand, leaves a receipt whose id Proxmox gives to the next VM, which may be another portal's.
func (rec Receipt) Holds(row pve.ClusterResource) bool {
	p := rec.Payload
	return !rec.TemplateGone && (rec.Step == StepSucceeded || rec.Step == StepTemplated) &&
		rec.VMID != 0 && rec.VMID == row.VMID && p.Node == row.Node &&
		len(strings.TrimPrefix(p.Digest, "sha256:")) >= 12 && copyOf(row, p)
}

// ── PVE access ────────────────────────────────────────────────────────────────────────────────

// LifecyclePVE is the subset of *pve.Client Publish/Withdraw/state use.
type LifecyclePVE interface {
	ClusterResources(ctx context.Context, kind string) ([]pve.ClusterResource, error)
	VMIDFree(ctx context.Context, vmid int) (bool, error)
	GetPool(ctx context.Context, poolID string) (*pve.PoolInfo, error)
	GetVMConfig(ctx context.Context, node string, vmid int) (pve.VMConfig, error)
	SetVMConfig(ctx context.Context, node string, vmid int, params map[string]string, del ...string) (pve.UPID, error)
	WaitTask(ctx context.Context, upid pve.UPID) error
}

// importTemplate re-reads vmid on node and proves it is p's finished import: a template in pool
// pc-templates (read from /pools), named TemplateName(p), tagged with p's source digest. A template
// imported before the lifecycle change (no pc-tu-import marker) qualifies too.
func importTemplate(ctx context.Context, pv LifecyclePVE, p VMPayload, node string, vmid int) (pve.VMConfig, error) {
	return importTemplateBy(ctx, pv, TemplateName(p), p.Digest, node, vmid)
}

// templateReader is what importTemplateBy reads (LifecyclePVE and SmokePVE both have it).
type templateReader interface {
	GetPool(ctx context.Context, poolID string) (*pve.PoolInfo, error)
	GetVMConfig(ctx context.Context, node string, vmid int) (pve.VMConfig, error)
}

func importTemplateBy(ctx context.Context, pv templateReader, name, digest, node string, vmid int) (pve.VMConfig, error) {
	pool, err := pv.GetPool(ctx, pve.TemplatePool)
	if err != nil {
		return nil, err
	}
	member := false
	if pool != nil {
		for _, m := range pool.Members {
			member = member || m.Type == "qemu" && m.VMID == vmid && m.Node == node
		}
	}
	if !member {
		return nil, lcErr(http.StatusConflict, "template_missing", fmt.Sprintf("VM %d on %s is not a template in pool pc-templates any more", vmid, node))
	}
	cfg, err := pv.GetVMConfig(ctx, node, vmid)
	if pve.IsNotFound(err) {
		return nil, lcErr(http.StatusConflict, "template_missing", fmt.Sprintf("VM %d on %s no longer exists", vmid, node))
	}
	if err != nil {
		return nil, err
	}
	if !isTemplate(cfg) || cfg.Str("name") != name || !hasTagValue(cfg.Str("tags"), TagSource+digestPrefix(digest)) {
		return nil, lcErr(http.StatusConflict, "template_mismatch", fmt.Sprintf("VM %d on %s is not this import's template (name, template flag or source tag differ)", vmid, node))
	}
	return cfg, nil
}

// SyncImportedTemplates is the provider sync's pass over the templates of the provider that one of recs holds
// (Receipt.Holds: an import of this portal that finished and still names its template) and that are not withdrawn.
// It writes what an import of today writes (Executor.importDisk) on a template imported before, so that the machines
// cloned from it from now on have it too:
//   - vga std where the display is a serial port (every import before v0.10.0), on OS and Kubernetes templates: the
//     Proxmox console then opens on the graphical display, and serial0 stays for the serial terminal;
//   - onboot on a Kubernetes node template that lacks it (an import before v0.9.0), so that its clones start with
//     their Proxmox node.
//
// A withdrawn template (pc-tu-withdrawn) is kept for forensics and is neither read nor written: no new node group or
// server gets it, and a VM cloned from it before is covered by the sync's passes over the VMs
// (pve.Service.StartWorkersAtBoot; pve.Service.ShowGraphicalDisplay for a stopped server) and by the portal's start
// (pve.Client.StartVM). A template is never running, so vga lands at once and no change stays pending. A template no receipt holds (built by the image
// pipeline, the site's own, another portal's) is neither read nor written. The write follows importTemplate's proof
// and carries the digest it read, as a tag write does, and takes VM.Config.Options (onboot) and VM.Config.HWType
// (vga) on /pool/pc-templates, which the import already uses.
func SyncImportedTemplates(ctx context.Context, pv LifecyclePVE, recs []Receipt, serviceID string) error {
	rows, err := pv.ClusterResources(ctx, "vm")
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		i := slices.IndexFunc(recs, func(r Receipt) bool { return r.Payload.ServiceID == serviceID && r.Holds(row) })
		if i < 0 || row.NodeDown || hasTagValue(row.Tags, pve.TagTemplateWithdrawn) {
			continue
		}
		cfg, err := importTemplate(ctx, pv, recs[i].Payload, row.Node, row.VMID)
		params := map[string]string{}
		if err == nil {
			if recs[i].Payload.Variant == "kubernetes" && cfg.Int("onboot") != 1 {
				params["onboot"] = "1"
			}
			if pve.SerialDisplay(cfg) {
				params["vga"] = "std"
			}
		}
		if len(params) > 0 {
			if d := cfg.Str("digest"); d != "" {
				params["digest"] = d
			}
			var up pve.UPID
			if up, err = pv.SetVMConfig(ctx, row.Node, row.VMID, params); err == nil {
				err = pv.WaitTask(ctx, up)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("template %d on %s: start at boot and display: %w", row.VMID, row.Node, err))
		}
	}
	return errors.Join(errs...)
}

// setTags writes tags with the config digest just read (a concurrent config change makes PVE
// refuse) and reads them back.
func setTags(ctx context.Context, pv LifecyclePVE, node string, vmid int, cfg pve.VMConfig, tags string) error {
	params := map[string]string{"tags": tags}
	if d := cfg.Str("digest"); d != "" {
		params["digest"] = d
	}
	up, err := pv.SetVMConfig(ctx, node, vmid, params)
	if err == nil {
		err = pv.WaitTask(ctx, up)
	}
	if err != nil {
		var ae *pve.APIError
		if errors.As(err, &ae) && ae.Status == http.StatusForbidden {
			return lcErr(http.StatusBadGateway, "pve_forbidden", "The provider token cannot change the template's tags (VM.Config.Options on /pool/pc-templates)")
		}
		return lcErr(http.StatusBadGateway, "pve_tag_write_failed", fmt.Sprintf("Proxmox did not take the tag change on VM %d (changed concurrently, or unreachable); nothing else was changed — try again", vmid))
	}
	after, err := pv.GetVMConfig(ctx, node, vmid)
	if err != nil || !sameTags(after.Str("tags"), tags) {
		return lcErr(http.StatusBadGateway, "pve_tag_readback", fmt.Sprintf("The tag change on VM %d could not be read back; check the template", vmid))
	}
	return nil
}

// ── blocklist ─────────────────────────────────────────────────────────────────────────────────

// Blocked returns the blocklist entry of a digest (nil = not withdrawn).
func (s PGReceipts) Blocked(ctx context.Context, digest string) (*BlockedDigest, error) {
	if s.DB == nil {
		return nil, errors.New("template update store is not configured")
	}
	var b BlockedDigest
	found, err := s.DB.C(BlocklistCollection).Get(ctx, digest, &b)
	if err != nil || !found {
		return nil, err
	}
	return &b, nil
}

// BlockedSet lists every withdrawn digest.
func (s PGReceipts) BlockedSet(ctx context.Context) (map[string]BlockedDigest, error) {
	var all []BlockedDigest
	if err := s.DB.C(BlocklistCollection).Find(ctx, nil, &all); err != nil {
		return nil, err
	}
	out := make(map[string]BlockedDigest, len(all))
	for _, b := range all {
		out[b.Digest] = b
	}
	return out, nil
}

// Block records a withdrawn digest; the first withdrawal (reason, actor, time) is kept.
func (s PGReceipts) Block(ctx context.Context, b BlockedDigest) (*BlockedDigest, error) {
	var out *BlockedDigest
	err := s.DB.WithTx(ctx, func(tc context.Context) error {
		if err := s.DB.LockXact(tc, "templateupdate-blocklist:"+b.Digest); err != nil {
			return err
		}
		existing, err := s.Blocked(tc, b.Digest)
		if err != nil {
			return err
		}
		if existing != nil {
			out = existing
			return nil
		}
		if _, err := s.DB.C(BlocklistCollection).InsertOne(tc, &b); err != nil {
			return err
		}
		out = &b
		return nil
	})
	return out, err
}

// Blocklist lists the withdrawn digests, newest first.
func (s *Service) Blocklist(ctx context.Context) ([]BlockedDigest, error) {
	out := []BlockedDigest{}
	err := s.DB.C(BlocklistCollection).Find(ctx, nil, &out, pgdoc.Sort(pgdoc.DescK("at", pgdoc.KTime)))
	return out, err
}

// MarkWithdrawn flags discovery rows whose newest artifact was withdrawn.
func MarkWithdrawn(d *Discovery, blocked map[string]BlockedDigest) {
	for i := range d.VM {
		if l := d.VM[i].Latest; l != nil {
			if b, ok := blocked[l.Digest]; ok {
				b := b
				d.VM[i].LatestWithdrawn = &b
				d.VM[i].UpdateAvailable = false
			}
		}
	}
}

// ── publish / withdraw ────────────────────────────────────────────────────────────────────────

// LifecycleResult is the outcome of Publish/Withdraw (also the audit "after" snapshot).
type LifecycleResult struct {
	ID        string `json:"id"` // import receipt id
	Action    string `json:"action"`
	Digest    string `json:"digest"`
	Slug      string `json:"slug"`
	ServiceID string `json:"serviceId"`
	Changed   []int  `json:"changed"`             // VMIDs whose tags changed
	Unchanged bool   `json:"unchanged,omitempty"` // idempotent repeat: nothing to do
	Smoke     string `json:"smokeJob,omitempty"`  // the PASS smoke a publish relied on
	Remaining []int  `json:"remaining,omitempty"` // withdraw: published VMIDs left on the line (all providers)
	// Providers is the withdraw outcome per Proxmox provider holding a copy of the artifact.
	Providers []ProviderOutcome `json:"providers,omitempty"`
	// Complete is false when the digest was blocklisted but some copy is not withdrawn yet (see
	// Providers/Warnings): the page shows "Withdraw incomplete" and repeating the withdraw finishes it.
	Complete bool     `json:"complete"`
	Warnings []string `json:"warnings,omitempty"`
}

// ProviderOutcome is one provider's part of a withdraw.
type ProviderOutcome struct {
	ServiceID string `json:"serviceId"`
	// Status: withdrawn (every copy there carries pc-tu-withdrawn), incomplete (a copy's tag change did
	// not land or its node is down), unreachable (the cluster could not be read: nothing changed
	// there), provider_missing (the provider no longer exists: nothing can be changed there).
	Status          string   `json:"status"`
	Changed         []int    `json:"changed"`
	Remaining       []int    `json:"remaining"` // published templates of the line left on this provider
	NodesLosingLast []string `json:"nodesLosingLast,omitempty"`
	// VersionsMapLine (Kubernetes node image) is the config.cluster.versions entry without the
	// withdrawn copies: kamaji names template VMIDs explicitly and keeps cloning a withdrawn one.
	VersionsMapLine string   `json:"versionsMapLine,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
}

// PVEResolver gives the Proxmox access of a provider (a *LifecycleError provider_missing when it is
// gone for good).
type PVEResolver func(ctx context.Context, serviceID string) (LifecyclePVE, error)

// withdrawPlan is what withdrawing p means on one provider's cluster.
type withdrawPlan struct {
	copies          []pve.ClusterResource // finished imports of p's artifact (any node)
	publishedCopies int
	remaining       []int    // published templates of the line that are not copies
	nodesLosingLast []string // nodes whose only published version of the line is a copy (provider keeps others)
	lastOnProvider  bool     // no published version of the line would be left on the provider
}

func planWithdraw(rows []pve.ClusterResource, p VMPayload) withdrawPlan {
	var pl withdrawPlan
	isCopy, pubNodes, otherNodes := map[int]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		if copyOf(r, p) {
			pl.copies = append(pl.copies, r)
			isCopy[r.VMID] = true
			if isPublished(r.Tags, p.Variant) {
				pl.publishedCopies++
				pubNodes[r.Node] = true
			}
		}
	}
	pl.remaining = []int{}
	for _, r := range rows {
		if !isCopy[r.VMID] && servesLine(r, p) {
			pl.remaining = append(pl.remaining, r.VMID)
			otherNodes[r.Node] = true
		}
	}
	sort.Ints(pl.remaining)
	sort.Slice(pl.copies, func(i, j int) bool { return pl.copies[i].VMID < pl.copies[j].VMID })
	pl.lastOnProvider = pl.publishedCopies > 0 && len(pl.remaining) == 0
	if !pl.lastOnProvider {
		for n := range pubNodes {
			if !otherNodes[n] {
				pl.nodesLosingLast = append(pl.nodesLosingLast, n)
			}
		}
		sort.Strings(pl.nodesLosingLast)
	}
	return pl
}

// withoutCopies is rows minus the copies of p (the cluster as it is once p is withdrawn).
func withoutCopies(rows []pve.ClusterResource, p VMPayload) []pve.ClusterResource {
	out := make([]pve.ClusterResource, 0, len(rows))
	for _, r := range rows {
		if !copyOf(r, p) {
			out = append(out, r)
		}
	}
	return out
}

// importReceipt loads a finished import (the template a lifecycle action targets).
func (s *Service) importReceipt(ctx context.Context, id string) (*Receipt, error) {
	r, err := s.Receipts.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, lcErr(http.StatusNotFound, "not_found", "No template import with that id")
	}
	if (r.Step != StepSucceeded && r.Step != StepTemplated) || r.VMID <= 0 {
		return nil, lcErr(http.StatusConflict, "import_unfinished", "That import has not produced its template")
	}
	return r, nil
}

// MarkTemplateGone records that the template a finished import produced was deleted, so EnqueueVM
// treats the receipt like a failed one and the artifact can be imported on that node again. It reads
// the receipt's own provider (pvFor) and marks only on proof: nextid says no guest anywhere holds the
// VMID, or the guest that does is visibly another VM (templateGone). A read error, or a VM that may
// still be the template, changes nothing. A receipt already marked answers nil.
func (s *Service) MarkTemplateGone(ctx context.Context, pvFor PVEResolver, id string) error {
	if !s.VMEnabled {
		return errVMDisabled
	}
	r, err := s.Receipts.Load(ctx, id)
	if err != nil {
		return err
	}
	switch {
	case r == nil:
		return lcErr(http.StatusNotFound, "not_found", "No template import with that id")
	case r.TemplateGone:
		return nil
	case (r.Step != StepSucceeded && r.Step != StepTemplated) || r.VMID <= 0:
		return lcErr(http.StatusConflict, "import_unfinished", "That import has not produced its template")
	}
	pv, err := pvFor(ctx, r.Payload.ServiceID)
	if err != nil {
		return err
	}
	free, err := pv.VMIDFree(ctx, r.VMID)
	if err != nil {
		return err
	}
	if !free {
		rows, err := pv.ClusterResources(ctx, "vm")
		if err != nil {
			return err
		}
		others, err := s.Receipts.FinishedByDigest(ctx, r.Payload.Digest)
		if err != nil {
			return err
		}
		if err := templateGone(rows, *r, others); err != nil {
			return err
		}
	}
	_, err = s.DB.C(ReceiptCollection).SetByID(ctx, id, pgdoc.M{"templateGone": true, "updatedAt": time.Now().UTC()}, nil)
	return err
}

// templateGone is MarkTemplateGone's proof once nextid said some guest holds rec.VMID (unique
// cluster-wide): nil only when this token sees that guest and it is plainly another VM (its node is up,
// it has a name, and it has neither the import's template name nor its source tag: the VMID was
// reused), or it is a sibling's template (siblingHolds). The rows list only guests the token has
// VM.Audit on (through its pools), so a held VMID missing from them is a guest out of its view (a
// template moved out of pc-templates, or rights lost), and counts as present, as does a renamed or
// migrated template.
func templateGone(rows []pve.ClusterResource, rec Receipt, others []Receipt) error {
	vmid, p := rec.VMID, rec.Payload
	for _, r := range rows {
		if r.VMID != vmid {
			continue
		}
		switch {
		case r.NodeDown || r.Name == "": // the row says only that a VM exists
		case r.Name != TemplateName(p) && !hasTagValue(r.Tags, TagSource+digestPrefix(p.Digest)):
			return nil
		case siblingHolds(r, rec, others):
			return nil
		}
		return lcErr(http.StatusConflict, "template_present", fmt.Sprintf("VM %d on %s may still be this import's template; nothing was changed", vmid, r.Node))
	}
	return lcErr(http.StatusConflict, "template_present", fmt.Sprintf("VM %d exists outside what this provider's token can see (moved out of pc-templates?); nothing was changed", vmid))
}

// siblingHolds reports whether row is another import's template of the same artifact. Stage 2 imports
// one artifact on several nodes and every import takes the lowest free VMID from 9200, so a later import
// can take the VMID of this receipt's deleted template. A finished, unmarked receipt of the same
// provider recorded at exactly row's node and VMID, enqueued after rec's last write (when rec's
// template still held that VMID, or was already gone), made a VM there: rec's VM had left it by then.
func siblingHolds(row pve.ClusterResource, rec Receipt, others []Receipt) bool {
	for _, o := range others {
		if o.ID != rec.ID && !o.TemplateGone && o.Payload.ServiceID == rec.Payload.ServiceID &&
			o.Payload.Node == row.Node && o.VMID == row.VMID && o.CreatedAt.After(rec.UpdatedAt) {
			return true
		}
	}
	return false
}

// DeletePVE is what Delete uses of *pve.Client beyond the lifecycle reads.
type DeletePVE interface {
	LifecyclePVE
	StorageContents(ctx context.Context, node, storage, content string, vmid int) ([]pve.StorageContent, error)
	DeleteVMConfigDisks(ctx context.Context, node string, vmid int) (pve.UPID, error)
}

// TemplateUsers names what still clones from the template vmid on node of a Proxmox provider: the node groups of
// Kubernetes clusters and the version maps of Kubernetes providers (the admin handler reads them).
type TemplateUsers func(ctx context.Context, serviceID, node string, vmid int) ([]string, error)

// linkedClones lists the VM ids whose disks are linked clones of the template's base disks: the storage content rows
// of the storages the template's config names whose volume id or parent names base-<vmid>-, which is what Proxmox's
// own check before a destroy reads. A site that makes full clones only has none.
func linkedClones(ctx context.Context, pv DeletePVE, node string, vmid int, cfg pve.VMConfig) ([]int, error) {
	base := fmt.Sprintf("base-%d-", vmid)
	storages := map[string]bool{}
	for k := range cfg {
		vol, _, _ := strings.Cut(cfg.Str(k), ",")
		if storage, name, ok := strings.Cut(vol, ":"); ok && strings.Contains(name, base) {
			storages[storage] = true
		}
	}
	seen := map[int]bool{}
	for storage := range storages {
		items, err := pv.StorageContents(ctx, node, storage, "images", 0)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.VMID != vmid && (strings.Contains(it.VolID, base) || strings.Contains(it.Parent, base)) {
				seen[it.VMID] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// Delete destroys a withdrawn template of this portal's own import and frees its receipt, so that the artifact's VM
// id and node are free again. Every condition is read from Proxmox and the store right before the destroy, and each
// refusal has its own code:
//
//   - not_withdrawn: the digest is not on the blocklist, or the template lacks pc-tu-withdrawn or pc-tu-import;
//   - template_published: it still carries pc-public or pc-tu-k8s-public (a withdraw that is not finished);
//   - template_mismatch / template_present: the VM at the receipt's id is not this import's template (importTemplate,
//     the proof publish and withdraw use: a template in pool pc-templates on the receipt's node, with the import's
//     name and source tag), or it sits outside what the token sees;
//   - job_running / smoke_running: the import's job or a smoke test of this template has not ended;
//   - linked_clones: a linked clone still uses a base disk (linkedClones);
//   - template_in_use: a cluster's node group or a provider's version map still names it (users).
//
// The destroy is DeleteVMConfigDisks: the disks the config names, never "destroy unreferenced disks". The receipt is
// then freed by MarkTemplateGone, on its proof that no guest holds the id. A template that is already gone (deleted
// by hand, or by a Delete whose receipt write failed) is freed the same way and answers Unchanged.
func (s *Service) Delete(ctx context.Context, pv DeletePVE, users TemplateUsers, id string) (*LifecycleResult, error) {
	if !s.VMEnabled {
		return nil, errVMDisabled
	}
	rec, err := s.importReceipt(ctx, id)
	if err != nil {
		return nil, err
	}
	p := rec.Payload
	out := &LifecycleResult{ID: id, Action: "delete", Digest: p.Digest, Slug: slugOf(p), ServiceID: p.ServiceID, Changed: []int{}, Complete: true}
	if rec.TemplateGone {
		out.Unchanged = true
		return out, nil
	}
	release, err := s.lockDigest(ctx, p.Digest)
	if err != nil {
		return nil, err
	}
	defer release()
	if b, err := s.Receipts.Blocked(ctx, p.Digest); err != nil {
		return nil, err
	} else if b == nil {
		return nil, lcErr(http.StatusConflict, "not_withdrawn", "Only a withdrawn template can be deleted: withdraw it first")
	}
	if state, _ := s.JobState(ctx, id); state == "pending" || state == "running" {
		return nil, lcErr(http.StatusConflict, "job_running", "The import job of this template has not ended; try again when it has")
	}
	smoke, err := s.Smokes.Latest(ctx, p.ServiceID, p.Node, rec.VMID, p.Digest)
	if err != nil {
		return nil, err
	}
	if smoke != nil && smoke.Result == "" {
		if state, _ := s.JobState(ctx, smoke.ID); state != "dead" {
			return nil, lcErr(http.StatusConflict, "smoke_running", "A smoke test of this template is still running; try again when it has ended")
		}
	}
	gone := func() error {
		return s.MarkTemplateGone(ctx, func(context.Context, string) (LifecyclePVE, error) { return pv, nil }, id)
	}
	cfg, err := importTemplate(ctx, pv, p, p.Node, rec.VMID)
	var le *LifecycleError
	if errors.As(err, &le) && (le.Code == "template_missing" || le.Code == "template_mismatch") {
		// Nothing of this import is at that id any more: nothing to destroy, the receipt is freed on proof. A VM that
		// may still be the template (renamed, moved out of the pool) is refused: template_mismatch as it was read,
		// template_present for one out of view.
		if gerr := gone(); gerr != nil {
			if le.Code == "template_mismatch" {
				return nil, err
			}
			return nil, gerr
		}
		out.Unchanged = true
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	tags := cfg.Str("tags")
	switch {
	case !hasTagValue(tags, pve.TagTemplateWithdrawn) || !hasTagValue(tags, TagImport):
		return nil, lcErr(http.StatusConflict, "not_withdrawn", fmt.Sprintf("Template %d on %s is not tagged withdrawn (%s and %s): finish the withdraw first", rec.VMID, p.Node, pve.TagTemplateWithdrawn, TagImport))
	case hasTagValue(tags, pve.TagTemplatePub) || hasTagValue(tags, TagK8sPublished):
		return nil, lcErr(http.StatusConflict, "template_published", fmt.Sprintf("Template %d on %s still carries a published tag: finish the withdraw first", rec.VMID, p.Node))
	}
	clones, err := linkedClones(ctx, pv, p.Node, rec.VMID, cfg)
	if err != nil {
		return nil, err
	}
	if len(clones) > 0 {
		e := lcErr(http.StatusConflict, "linked_clones", fmt.Sprintf("Template %d on %s is the base disk of linked clones (VM %s): delete or fully clone them first", rec.VMID, p.Node, joinInts(clones)))
		e.Details = map[string]any{"vmids": clones}
		return nil, e
	}
	names, err := users(ctx, p.ServiceID, p.Node, rec.VMID)
	if err != nil {
		return nil, err
	}
	if len(names) > 0 {
		e := lcErr(http.StatusConflict, "template_in_use", fmt.Sprintf("Template %d on %s is still named by: %s. New workers would be cloned from it: move them to another image first", rec.VMID, p.Node, strings.Join(names, "; ")))
		e.Details = map[string]any{"users": names}
		return nil, e
	}
	up, err := pv.DeleteVMConfigDisks(ctx, p.Node, rec.VMID)
	if err == nil {
		err = pv.WaitTask(ctx, up)
	}
	if err != nil && !pve.IsNotFound(err) {
		return nil, err
	}
	out.Changed = append(out.Changed, rec.VMID)
	if err := gone(); err != nil {
		return nil, err
	}
	return out, nil
}

func joinInts(ids []int) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.Itoa(id)
	}
	return strings.Join(out, ", ")
}

// lockDigest serializes lifecycle actions on one artifact across API replicas.
func (s *Service) lockDigest(ctx context.Context, digest string) (func(), error) {
	release, ok, err := s.DB.TryAdvisoryLock(ctx, "templateupdate-lifecycle:"+digest)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, lcErr(http.StatusConflict, "busy", "Another publish or withdraw of this artifact is running; try again")
	}
	return release, nil
}

// Publish puts an imported template into its catalog: only when the latest smoke test of exactly
// that template VMID + digest passed and the digest is not withdrawn. Idempotent: an already
// published template answers Unchanged.
func (s *Service) Publish(ctx context.Context, pv LifecyclePVE, id, confirmDigest, actor string) (*LifecycleResult, error) {
	if !s.VMEnabled {
		return nil, errVMDisabled
	}
	rec, err := s.importReceipt(ctx, id)
	if err != nil {
		return nil, err
	}
	p := rec.Payload
	if confirmDigest != p.Digest {
		return nil, lcErr(http.StatusBadRequest, "confirm_digest", "Confirm the exact digest to publish")
	}
	release, err := s.lockDigest(ctx, p.Digest)
	if err != nil {
		return nil, err
	}
	defer release()
	if b, err := s.Receipts.Blocked(ctx, p.Digest); err != nil {
		return nil, err
	} else if b != nil {
		return nil, ErrBlocklisted
	}
	smoke, err := s.Smokes.Latest(ctx, p.ServiceID, p.Node, rec.VMID, p.Digest)
	if err != nil {
		return nil, err
	}
	switch {
	case smoke == nil:
		return nil, lcErr(http.StatusConflict, "smoke_missing", "Run the smoke test first: only a template whose latest smoke test passed can be published")
	case smoke.Result == "":
		return nil, lcErr(http.StatusConflict, "smoke_running", "A smoke test of this template is still running")
	case smoke.Result != SmokePass:
		return nil, lcErr(http.StatusConflict, "smoke_not_passed", "The latest smoke test of this template did not pass ("+smoke.Code+"); fix the cause and run it again")
	}
	cfg, err := importTemplate(ctx, pv, p, p.Node, rec.VMID)
	if err != nil {
		return nil, err
	}
	out := &LifecycleResult{ID: id, Action: "publish", Digest: p.Digest, Slug: slugOf(p), ServiceID: p.ServiceID, Changed: []int{}, Smoke: smoke.ID, Complete: true}
	if hasTagValue(cfg.Str("tags"), pve.TagTemplateWithdrawn) {
		return nil, ErrBlocklisted
	}
	want := publishedTags(cfg.Str("tags"), p)
	if sameTags(cfg.Str("tags"), want) {
		out.Unchanged = true
		return out, nil
	}
	// The PASS vouches for the template as it was when the smoke test started: any config change
	// since (disk, cloud-init, hardware, tags) invalidates it.
	if d := cfg.Str("digest"); d == "" || d != smoke.TemplateConfigDigest {
		return nil, lcErr(http.StatusConflict, "template_changed_since_smoke", "The template's configuration changed after its smoke test; run the smoke test again before publishing")
	}
	if err := setTags(ctx, pv, p.Node, rec.VMID, cfg, want); err != nil {
		return nil, err
	}
	out.Changed = append(out.Changed, rec.VMID)
	return out, nil
}

// WithdrawRequest is the admin's withdraw: a reason is required; withdrawing the last published
// version of a line on a provider also needs Force and ConfirmLast = the line's slug; leaving a node
// without a published version of the line needs ConfirmNodeLoss.
type WithdrawRequest struct {
	Reason          string `json:"reason"`
	ConfirmDigest   string `json:"confirmDigest"`
	Force           bool   `json:"force"`
	ConfirmLast     string `json:"confirmLast"`
	ConfirmNodeLoss bool   `json:"confirmNodeLoss"`
}

// FinishedByDigest lists every import of a digest that produced its template (all providers).
func (s PGReceipts) FinishedByDigest(ctx context.Context, digest string) ([]Receipt, error) {
	var all []Receipt
	if err := s.DB.C(ReceiptCollection).Find(ctx, pgdoc.M{"payload.digest": digest}, &all); err != nil {
		return nil, err
	}
	out := []Receipt{}
	for _, r := range all {
		if (r.Step == StepSucceeded || r.Step == StepTemplated) && r.VMID > 0 {
			out = append(out, r)
		}
	}
	return out, nil
}

// Withdraw blocklists the template's artifact digest and takes EVERY copy of it out of its catalog
// on EVERY Proxmox provider that holds a finished import of it (the templates are kept for
// forensics), so a blocklisted digest never stays published anywhere. The catalog then falls back to
// the previous published version (pve.DiscoverTemplates: newest pc-v- wins).
//
// Guards, evaluated per provider before anything is changed: a provider left without any published
// version of the line needs Force + ConfirmLast (the slug); a node left without one needs
// ConfirmNodeLoss. A provider whose cluster cannot be read changes nothing and is reported
// (Complete=false): the page shows "Withdraw incomplete" and the same action finishes it.
//
// A Kubernetes node image: the result warns only about withdrawn copies that users says something still names by
// VMID (a provider's version map, a cluster's node group, a platform), since those keep cloning them.
func (s *Service) Withdraw(ctx context.Context, pvFor PVEResolver, users TemplateUsers, id string, req WithdrawRequest, actor string) (*LifecycleResult, error) {
	if !s.VMEnabled {
		return nil, errVMDisabled
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if len(req.Reason) < 8 || len(req.Reason) > 500 {
		return nil, lcErr(http.StatusBadRequest, "reason_required", "Give a reason of 8 to 500 characters")
	}
	rec, err := s.importReceipt(ctx, id)
	if err != nil {
		return nil, err
	}
	p := rec.Payload
	if req.ConfirmDigest != p.Digest {
		return nil, lcErr(http.StatusBadRequest, "confirm_digest", "Confirm the exact digest to withdraw")
	}
	release, err := s.lockDigest(ctx, p.Digest)
	if err != nil {
		return nil, err
	}
	defer release()
	recs, err := s.Receipts.FinishedByDigest(ctx, p.Digest)
	if err != nil {
		return nil, err
	}
	// Providers in a stable order: the import's own first, then the others by id.
	bySvc := map[string][]Receipt{p.ServiceID: {*rec}}
	svcs := []string{}
	for _, r := range recs {
		if r.ID == rec.ID {
			continue
		}
		if _, ok := bySvc[r.Payload.ServiceID]; !ok {
			svcs = append(svcs, r.Payload.ServiceID)
		}
		bySvc[r.Payload.ServiceID] = append(bySvc[r.Payload.ServiceID], r)
	}
	sort.Strings(svcs)
	svcs = append([]string{p.ServiceID}, svcs...)
	slug := slugOf(p)
	type provider struct {
		out  *ProviderOutcome
		pv   LifecyclePVE
		rows []pve.ClusterResource
		plan withdrawPlan
	}
	provs := make([]*provider, 0, len(svcs))
	var lastOn, nodeLoss []string
	for _, svc := range svcs {
		pr := &provider{out: &ProviderOutcome{ServiceID: svc, Changed: []int{}, Remaining: []int{}}}
		provs = append(provs, pr)
		pv, err := pvFor(ctx, svc)
		var le *LifecycleError
		if errors.As(err, &le) && le.Code == "provider_missing" {
			pr.out.Status = "provider_missing"
			pr.out.Warnings = append(pr.out.Warnings, "the provider no longer exists: any copy left in its cluster cannot be changed from here")
			continue
		}
		if err != nil {
			pr.out.Status = "unreachable"
			pr.out.Warnings = append(pr.out.Warnings, "the provider is not reachable: nothing was changed there yet; withdraw again once it is")
			continue
		}
		rows, err := pv.ClusterResources(ctx, "vm")
		if err != nil {
			pr.out.Status = "unreachable"
			pr.out.Warnings = append(pr.out.Warnings, "the cluster could not be read: nothing was changed there yet; withdraw again once it can")
			continue
		}
		pr.pv, pr.rows, pr.plan = pv, rows, planWithdraw(rows, p)
		pr.out.Remaining, pr.out.NodesLosingLast = pr.plan.remaining, pr.plan.nodesLosingLast
		if pr.plan.lastOnProvider {
			lastOn = append(lastOn, svc)
		}
		for _, n := range pr.plan.nodesLosingLast {
			nodeLoss = append(nodeLoss, svc+"/"+n)
		}
	}
	if len(lastOn) > 0 && !(req.Force && req.ConfirmLast == slug) {
		return nil, &LifecycleError{Status: http.StatusConflict, Code: "last_published_version",
			Message: fmt.Sprintf("This is the only published version of %s on provider(s) %s: after withdrawing it no new VM of it can be created there until another version is published. To do it anyway, repeat with force and confirm %q", slug, strings.Join(lastOn, ", "), slug),
			Details: map[string]any{"slug": slug, "providers": lastOn}}
	}
	if len(nodeLoss) > 0 && !req.ConfirmNodeLoss {
		return nil, &LifecycleError{Status: http.StatusConflict, Code: "nodes_losing_last_copy",
			Message: fmt.Sprintf("Node(s) %s keep no published version of %s after this withdraw: new VMs placed there cannot be created from it until another version is published there. Confirm to continue", strings.Join(nodeLoss, ", "), slug),
			Details: map[string]any{"slug": slug, "nodes": nodeLoss}}
	}
	if _, err := s.Receipts.Block(ctx, BlockedDigest{Digest: p.Digest, Package: p.Package, Tag: p.Tag, Reason: req.Reason, Actor: actor,
		At: time.Now().UTC(), ServiceID: p.ServiceID, Node: p.Node, VMID: rec.VMID, ImportID: rec.ID}); err != nil {
		return nil, err
	}
	out := &LifecycleResult{ID: id, Action: "withdraw", Digest: p.Digest, Slug: slug, ServiceID: p.ServiceID, Changed: []int{}, Remaining: []int{}, Complete: true}
	for _, pr := range provs {
		if pr.pv == nil {
			if pr.out.Status != "provider_missing" {
				out.Complete = false
			}
			out.Providers = append(out.Providers, *pr.out)
			continue
		}
		pr.out.Status = "withdrawn"
		// A receipt's template on a node that is down cannot be read or changed now: it stays as it
		// was (maybe published) until the node is back and the withdraw is repeated.
		for _, r := range bySvc[pr.out.ServiceID] {
			for _, row := range pr.rows {
				if row.VMID == r.VMID && row.NodeDown {
					pr.out.Status = "incomplete"
					pr.out.Warnings = append(pr.out.Warnings, fmt.Sprintf("VM %d: node %s is down; withdraw again once it is back", r.VMID, row.Node))
				}
			}
		}
		for _, c := range pr.plan.copies {
			cfg, err := importTemplate(ctx, pr.pv, p, c.Node, c.VMID)
			if err != nil {
				pr.out.Status = "incomplete"
				pr.out.Warnings = append(pr.out.Warnings, fmt.Sprintf("VM %d on %s was not changed: %v", c.VMID, c.Node, err))
				continue
			}
			want := withdrawnTags(cfg.Str("tags"))
			if sameTags(cfg.Str("tags"), want) {
				continue
			}
			if err := setTags(ctx, pr.pv, c.Node, c.VMID, cfg, want); err != nil {
				pr.out.Status = "incomplete"
				pr.out.Warnings = append(pr.out.Warnings, fmt.Sprintf("VM %d on %s: %v", c.VMID, c.Node, err))
				continue
			}
			pr.out.Changed = append(pr.out.Changed, c.VMID)
		}
		if p.Variant == "kubernetes" {
			after := withoutCopies(pr.rows, p)
			v := versionsMapValue(after, p)
			pr.out.VersionsMapLine = fmt.Sprintf("%q: %q", p.KubernetesVersion, v)
			// A withdraw does not stop what names a withdrawn copy by its VMID from cloning it. The warning is for the
			// copies something still names (users), and says who: not for a provider whose map already names the others.
			ids, unread := []string{}, users == nil
			for _, c := range pr.plan.copies {
				if unread {
					ids = append(ids, fmt.Sprintf("%s=%d", c.Node, c.VMID))
					continue
				}
				who, err := users(ctx, pr.out.ServiceID, c.Node, c.VMID)
				if err != nil {
					unread, ids = true, []string{fmt.Sprintf("%s=%d", c.Node, c.VMID)}
				} else if len(who) > 0 {
					ids = append(ids, fmt.Sprintf("%s=%d (%s)", c.Node, c.VMID, strings.Join(who, "; ")))
				}
			}
			msg := fmt.Sprintf("Kubernetes %s: withdrawing does not stop new workers from being cloned from %s, which is still named by its VMID; the config.cluster.versions entry of a Kubernetes provider without the withdrawn copies is %s",
				p.KubernetesVersion, strings.Join(ids, ", "), pr.out.VersionsMapLine)
			if v == "" {
				msg = fmt.Sprintf("Kubernetes %s: no other published node image of this version is left on this provider, and %s is still named by its VMID: remove that version from the Kubernetes provider or publish another image before new workers are created",
					p.KubernetesVersion, strings.Join(ids, ", "))
			}
			if unread { // not known: the warning must not say that a copy is still named
				msg = fmt.Sprintf("Kubernetes %s: what still names the withdrawn copies (%s) by their VMID could not be read, and withdrawing does not stop new workers from being cloned from a copy that is still named; the config.cluster.versions entry of a Kubernetes provider without the withdrawn copies is %s",
					p.KubernetesVersion, strings.Join(ids, ", "), pr.out.VersionsMapLine)
				if v == "" {
					msg = fmt.Sprintf("Kubernetes %s: what still names the withdrawn copies (%s) by their VMID could not be read, and no other published node image of this version is left on this provider: if a Kubernetes provider still names the version, remove it there or publish another image before new workers are created",
						p.KubernetesVersion, strings.Join(ids, ", "))
				}
			}
			if len(ids) > 0 {
				pr.out.Warnings = append(pr.out.Warnings, msg)
			}
		}
		if pr.out.Status != "withdrawn" {
			out.Complete = false
		}
		out.Changed = append(out.Changed, pr.out.Changed...)
		out.Remaining = append(out.Remaining, pr.out.Remaining...)
		out.Providers = append(out.Providers, *pr.out)
	}
	for _, po := range out.Providers {
		for _, w := range po.Warnings {
			out.Warnings = append(out.Warnings, po.ServiceID+": "+w)
		}
	}
	out.Unchanged = len(out.Changed) == 0 && out.Complete
	return out, nil
}
