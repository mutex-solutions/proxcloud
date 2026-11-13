package templateupdates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// ThirdPartyGitHub is the thirdPartyIntegration type that holds the PAT
// (secret.token, encrypted at rest, write-only).
const ThirdPartyGitHub = "GitHub"

// TokenSource returns the decrypted PAT of the GitHub integration ("" = not configured).
type TokenSource func(ctx context.Context) (token string, integrationID string, err error)

// ProviderView is what discovery needs from a cloud provider (no secrets).
type ProviderView struct {
	ID, Name string
	// Templates are the cluster's qemu template rows in pool pc-templates (nil when unreachable).
	Templates []pve.ClusterResource
	Error     string
	// Sandbox template images (sandbox providers only): template name → image ref.
	SandboxImages map[string]string
}

// Service is the admin-facing façade.
type Service struct {
	Token       TokenSource
	Options     Options // registry/API overrides (tests); zero in production
	Jobs        *durablejobs.Store
	DB          *pgdoc.DB
	Receipts    PGReceipts
	Smokes      PGSmokes
	VMEnabled   bool // PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED: the executor runs only when set
	mu          sync.Mutex
	cache       *discoveryCache
	newRegistry func(token string) (Registry, error)
}

type discoveryCache struct {
	at      time.Time
	vm      []VMLine
	sandbox []SandboxRow
	names   string
}

// ErrNotConfigured surfaces as "configure the GitHub integration" (HTTP 409).
var ErrNotConfigured = errors.New("configure the GitHub integration (a classic personal access token with read:packages) first")

// Client builds a GitHub client from the integration's current token.
func (s *Service) Client(ctx context.Context) (*Client, error) {
	tok, _, err := s.Token(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(tok) == "" {
		return nil, ErrNotConfigured
	}
	return NewClient(tok, s.Options)
}

func (s *Service) registry(ctx context.Context) (Registry, *Client, error) {
	if s.newRegistry != nil {
		tok, _, err := s.Token(ctx)
		if err != nil {
			return nil, nil, err
		}
		if tok == "" {
			return nil, nil, ErrNotConfigured
		}
		r, err := s.newRegistry(tok)
		return r, nil, err
	}
	c, err := s.Client(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c, c, nil
}

// Status is the GitHub integration state shown on the page.
type Status struct {
	Configured    bool       `json:"configured"`
	IntegrationID string     `json:"integrationId,omitempty"`
	Login         string     `json:"login,omitempty"`
	Scopes        []string   `json:"scopes,omitempty"`
	RateLimit     *RateLimit `json:"rateLimit,omitempty"`
	Error         string     `json:"error,omitempty"`
	VMEnabled     bool       `json:"vmUpdatesEnabled"`
	SandboxMirror bool       `json:"sandboxMirrorEnabled"`
}

// GetStatus validates the stored token live (one GET /user).
func (s *Service) GetStatus(ctx context.Context) Status {
	st := Status{VMEnabled: s.VMEnabled}
	tok, id, err := s.Token(ctx)
	st.IntegrationID = id
	if err != nil {
		st.Error = "could not read the GitHub integration"
		return st
	}
	if tok == "" {
		return st
	}
	st.Configured = true
	c, err := NewClient(tok, s.Options)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	u, err := c.Validate(ctx)
	if err != nil {
		st.Error = err.Error()
		st.RateLimit = c.LastRateLimit()
		return st
	}
	st.Login, st.Scopes, st.RateLimit = u.Login, u.Scopes, c.LastRateLimit()
	return st
}

// ValidateToken is the save-time check of a new PAT (never stored if it fails).
func ValidateToken(ctx context.Context, token string, o Options) (*User, error) {
	c, err := NewClient(token, o)
	if err != nil {
		return nil, err
	}
	return c.Validate(ctx)
}

// Discovery is the page payload.
type Discovery struct {
	VM        []VMLine         `json:"vm"`
	Sandbox   []SandboxRow     `json:"sandbox"`
	CheckedAt time.Time        `json:"checkedAt"`
	RateLimit *RateLimit       `json:"rateLimit,omitempty"`
	Providers []ProviderStatus `json:"providers"`
}

// ProviderStatus reports a provider whose installed state could not be read.
type ProviderStatus struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

// cacheTTL keeps repeated page loads from spending the rate limit.
const cacheTTL = 2 * time.Minute

// Discover lists the newest artifacts and merges the installed/pinned state of providers.
func (s *Service) Discover(ctx context.Context, providers []ProviderView, sandboxNames []string, refresh bool) (*Discovery, error) {
	sort.Strings(sandboxNames)
	key := strings.Join(sandboxNames, ",")
	var vm []VMLine
	var sb []SandboxRow
	var rl *RateLimit
	s.mu.Lock()
	c := s.cache
	s.mu.Unlock()
	if c != nil && !refresh && time.Since(c.at) < cacheTTL && c.names == key {
		vm, sb = cloneVM(c.vm), cloneSB(c.sandbox)
	} else {
		reg, client, err := s.registry(ctx)
		if err != nil {
			return nil, err
		}
		if vm, err = DiscoverVM(ctx, reg); err != nil {
			return nil, err
		}
		if sb, err = DiscoverSandbox(ctx, reg, sandboxNames); err != nil {
			return nil, err
		}
		if client != nil {
			rl = client.LastRateLimit()
		}
		s.mu.Lock()
		s.cache = &discoveryCache{at: time.Now(), vm: cloneVM(vm), sandbox: cloneSB(sb), names: key}
		s.mu.Unlock()
	}
	out := &Discovery{VM: vm, Sandbox: sb, CheckedAt: time.Now().UTC(), RateLimit: rl, Providers: []ProviderStatus{}}
	for _, p := range providers {
		out.Providers = append(out.Providers, ProviderStatus{ID: p.ID, Name: p.Name, Error: p.Error})
	}
	MergeInstalled(out, providers)
	if s.Receipts.DB != nil {
		blocked, err := s.Receipts.BlockedSet(ctx)
		if err != nil {
			return nil, err
		}
		MarkWithdrawn(out, blocked)
	}
	return out, nil
}

func cloneVM(in []VMLine) []VMLine {
	b, _ := json.Marshal(in)
	var out []VMLine
	_ = json.Unmarshal(b, &out)
	for i := range out {
		if in[i].Latest != nil {
			out[i].Latest.Layers = in[i].Latest.Layers
			out[i].Latest.Info = in[i].Latest.Info
		}
	}
	return out
}

func cloneSB(in []SandboxRow) []SandboxRow {
	b, _ := json.Marshal(in)
	var out []SandboxRow
	_ = json.Unmarshal(b, &out)
	return out
}

// MergeInstalled fills Installed/Pinned and UpdateAvailable.
//
// A VM line is up to date on a provider when it holds a template whose pc-src- tag equals the
// latest digest prefix, or (pipeline-built native templates, which carry no digest) a pc-v-
// version ≥ the artifact's build (minute precision). "Update available" = the latest verified
// artifact is newer than everything installed on at least one reachable provider.
func MergeInstalled(d *Discovery, providers []ProviderView) {
	for i := range d.VM {
		row := &d.VM[i]
		row.Installed = []InstalledVM{}
		for _, p := range providers {
			if p.Templates == nil {
				continue
			}
			current := false
			for _, t := range p.Templates {
				inst, ok := installedFor(row, t)
				if !ok {
					continue
				}
				inst.ServiceID, inst.ServiceName = p.ID, p.Name
				row.Installed = append(row.Installed, inst)
				if row.Latest != nil && (inst.SourceDigest != "" && strings.HasPrefix(strings.TrimPrefix(row.Latest.Digest, "sha256:"), inst.SourceDigest) ||
					inst.SourceDigest == "" && inst.Version != "" && inst.Version >= row.Latest.Info.BuildVersion[:12]) {
					current = true
				}
			}
			if row.Latest != nil && !current {
				row.UpdateAvailable = true
			}
		}
		sort.Slice(row.Installed, func(a, b int) bool {
			if row.Installed[a].ServiceID != row.Installed[b].ServiceID {
				return row.Installed[a].ServiceID < row.Installed[b].ServiceID
			}
			return row.Installed[a].VMID < row.Installed[b].VMID
		})
	}
	for i := range d.Sandbox {
		row := &d.Sandbox[i]
		row.Pinned = []PinnedSandbox{}
		for _, p := range providers {
			names := make([]string, 0, len(p.SandboxImages))
			for n := range p.SandboxImages {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, tpl := range names {
				img := p.SandboxImages[tpl]
				name, dig, ok := SandboxNameOfImage(img)
				if !ok || name != row.Name {
					continue
				}
				row.Pinned = append(row.Pinned, PinnedSandbox{ServiceID: p.ID, ServiceName: p.Name, Template: tpl, Image: img, Digest: dig})
				if row.Latest != nil && dig != row.Latest.Digest {
					row.UpdateAvailable = true
				}
			}
		}
	}
}

// installedFor reports whether a cluster template row belongs to the VM line.
func installedFor(row *VMLine, t pve.ClusterResource) (InstalledVM, bool) {
	if t.Type != "qemu" || t.Template != 1 || t.Pool != pve.TemplatePool {
		return InstalledVM{}, false
	}
	tags := strings.Split(t.Tags, ";")
	has := func(want string) bool {
		for _, x := range tags {
			if x == want {
				return true
			}
		}
		return false
	}
	val := func(prefix string) string {
		for _, x := range tags {
			if strings.HasPrefix(x, prefix) {
				return strings.TrimPrefix(x, prefix)
			}
		}
		return ""
	}
	inst := InstalledVM{Node: t.Node, VMID: t.VMID, SourceDigest: val("pc-src-"), Published: has(pve.TagTemplatePub) || has(TagK8sPublished), Withdrawn: has(pve.TagTemplateWithdrawn)}
	if row.Variant == "kubernetes" {
		// Pipeline-built (images/k8s-node-template.sh): pc-k8s;pc-k8s-<ver dashes>;pc-k8s-os-<os>.
		// Imported by this feature: pc-tu-k8s;pc-k8sver-<ver dashes> (+ name pc-ghcr-<os>-…).
		ver := strings.ReplaceAll(row.KubernetesVersion, ".", "-")
		if has("pc-k8s") && has("pc-k8s-"+ver) && has("pc-k8s-os-"+row.OS) {
			return inst, true
		}
		if has("pc-tu-k8s") && val("pc-k8sver-") == ver && strings.HasPrefix(t.Name, "pc-ghcr-"+row.OS+"-") {
			inst.Version = strings.TrimPrefix(t.Name, "pc-ghcr-"+row.OS+"-k8s-"+ver+"-")
			return inst, true
		}
		return InstalledVM{}, false
	}
	if has(TagImport) && strings.HasPrefix(t.Name, "pc-ghcr-"+row.OS+"-os-") {
		// Imported by this feature (staged and withdrawn ones carry no pc-img-): the build is in the name.
		inst.Version = val(pve.TagTemplateVersion)
		if b := strings.TrimPrefix(t.Name, "pc-ghcr-"+row.OS+"-os-"); inst.Version == "" && len(b) >= 12 {
			inst.Version = b[:12]
		}
		return inst, true
	}
	if val(pve.TagTemplateImage) != row.OS {
		return InstalledVM{}, false
	}
	inst.Version = val(pve.TagTemplateVersion)
	return inst, true
}

// ── enqueue ────────────────────────────────────────────────────────────────────────────────────

// VMRequest is the admin's update request: a digest (confirmed in the dialog) and placement.
type VMRequest struct {
	Package       string `json:"package"`
	Digest        string `json:"digest"`
	ServiceID     string `json:"serviceId"`
	Node          string `json:"node"`
	ImportStorage string `json:"importStorage"`
	DiskStorage   string `json:"diskStorage"`
	// Publish is accepted for backward-compatible parsing only: false (or absent) is ignored and
	// true is refused with ErrPublishAtImport (HTTP 400). Imports always land staged; publishing
	// is a separate action after a passing smoke test.
	Publish bool `json:"publish"`
	// ConfirmDigest must repeat Digest: the dialog shows the exact digest and the admin confirms it.
	ConfirmDigest string `json:"confirmDigest"`
}

// Placement is the provider check the admin handler performs before enqueue.
type Placement struct {
	ImportStorageOK, DiskStorageOK bool
	Error                          string
}

// ErrDuplicate is an identical update already queued or running (or done) on that node.
var ErrDuplicate = errors.New("this artifact is already being imported (or was imported) on that node")

// EnqueueVM re-verifies the artifact by digest (never trusts a client-supplied tag/size), then
// commits the receipt and the durable job in one transaction.
func (s *Service) EnqueueVM(ctx context.Context, req VMRequest, actor string) (*Receipt, error) {
	if !s.VMEnabled {
		return nil, errors.New("VM template updates are not enabled on this site (PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED)")
	}
	if req.Publish {
		return nil, ErrPublishAtImport
	}
	if req.ConfirmDigest != req.Digest || !digestRE.MatchString(req.Digest) {
		return nil, errors.New("confirm the exact digest to import")
	}
	if b, err := s.Receipts.Blocked(ctx, req.Digest); err != nil {
		return nil, err
	} else if b != nil {
		return nil, ErrBlocklisted
	}
	reg, _, err := s.registry(ctx)
	if err != nil {
		return nil, err
	}
	versions, err := reg.ListVersions(ctx, req.Package)
	if err != nil {
		return nil, err
	}
	var cand *Candidate
	for _, cs := range NewestPerLine(req.Package, versions, 0) {
		for i := range cs {
			if cs[i].Digest == req.Digest {
				c := cs[i]
				cand = &c
			}
		}
	}
	if cand == nil {
		return nil, errors.New("that digest is not a published version of the package with a contract tag")
	}
	v, err := VerifyVM(ctx, reg, *cand)
	if err != nil {
		return nil, fmt.Errorf("the artifact failed verification: %w", err)
	}
	p := VMPayload{Version: 1, Package: v.Package, Tag: v.Tag, Digest: v.Digest, DiskDigest: v.DiskDigest, DiskBytes: v.DiskBytes,
		VirtualSizeBytes: v.Manifest.Disk.VirtualSizeBytes, OS: v.Manifest.OS, Variant: v.Manifest.Variant, KubernetesVersion: v.Manifest.KubernetesVersion,
		BuildVersion: v.Manifest.BuildVersion, ServiceID: req.ServiceID, Node: req.Node, ImportStorage: req.ImportStorage, DiskStorage: req.DiskStorage}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(p)
	now := time.Now().UTC()
	rec := &Receipt{Kind: "vm", Payload: p, RequestedBy: actor, CreatedAt: now, UpdatedAt: now, Step: StepQueued}
	// Everything below runs in ONE transaction under an advisory lock on the payload identity, so
	// two concurrent enqueues of the same artifact/node cannot both pass the duplicate check.
	err = s.DB.WithTx(ctx, func(tc context.Context) error {
		if err := s.DB.LockXact(tc, "templateupdate-enqueue:"+p.Key()); err != nil {
			return err
		}
		prior, err := s.Receipts.ByKey(tc, p.ServiceID, p.Node, p.Digest)
		if err != nil {
			return err
		}
		for _, r := range prior {
			// A template already made (the final write landed) blocks a re-import whatever its job
			// state: a job that died after its final write must not yield a duplicate template.
			if r.Step == StepTemplated || r.Step == StepSucceeded {
				return ErrDuplicate
			}
			// Otherwise a receipt is finished-failed when its step says failed OR its durable job is
			// dead (e.g. it died on a lease expiry before writing the failure). Everything else blocks.
			if r.Step == StepFailed {
				continue
			}
			if state, err := s.JobState(tc, r.ID); err == nil && state == "dead" {
				continue
			}
			return ErrDuplicate
		}
		// A failed earlier attempt keeps its own job row; a retry gets a distinct key.
		key := p.Key()
		if len(prior) > 0 {
			key = fmt.Sprintf("%s-%d", key, len(prior))
		}
		id, err := s.Jobs.EnqueueTx(tc, durablejobs.TemplateUpdateVM, key, raw, 3)
		if err != nil {
			return err
		}
		// Never overwrite an existing receipt (an identical key enqueued concurrently returns the
		// same job id): that would reset a running job's progress to "queued".
		if existing, err := s.Receipts.Load(tc, id); err != nil {
			return err
		} else if existing != nil {
			return ErrDuplicate
		}
		rec.ID = id
		return s.Receipts.Save(tc, rec)
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// ReplayableReceipt decides whether a dead template-update job may be replayed: only one whose
// receipt is still mid-flight (it died before finishing) resumes usefully. A failed or finished job
// is a no-op on replay; the retry path is a new import from the Template updates page.
func ReplayableReceipt(r *Receipt) error {
	switch {
	case r == nil:
		return errors.New("the template update receipt is missing")
	case r.Step == StepFailed:
		return errors.New("this template update failed; start a new import from Template updates instead of replaying")
	case r.Step == StepTemplated || r.Step == StepSucceeded:
		return errors.New("this template update already produced its template")
	case r.Cleaned:
		return errors.New("this template update's leftovers were already cleaned up; start a new import")
	}
	return nil
}

// JobState reads the durable job state of a receipt ("" when the row is gone, e.g. pruned).
func (s *Service) JobState(ctx context.Context, id string) (string, error) {
	if s.Jobs == nil {
		return "", nil
	}
	j, err := s.Jobs.Get(ctx, id)
	if err != nil {
		return "", nil
	}
	return j.State, nil
}

// History returns the newest receipts.
func (s *Service) History(ctx context.Context) ([]Receipt, error) { return s.Receipts.Recent(ctx, 50) }
