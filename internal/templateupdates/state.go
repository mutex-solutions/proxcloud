package templateupdates

// state.go — enqueueing smoke tests and the per-template lifecycle state the admin page shows.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
)

// SmokeMaxAttempts bounds a smoke job's retries (provider errors); a check failure ends it at once.
const SmokeMaxAttempts = 3

// EnqueueSmoke queues a smoke test of the template import id produced. net is the provider's
// smoke network (config.templateSmoke). A smoke already queued or running for that template is
// returned instead of a second one (idempotent).
func (s *Service) EnqueueSmoke(ctx context.Context, id string, net SmokeNetwork, actor string, auto bool) (*SmokeReceipt, error) {
	if !s.VMEnabled {
		return nil, errVMDisabled
	}
	if err := net.Validate(); err != nil {
		return nil, err
	}
	rec, err := s.importReceipt(ctx, id)
	if err != nil {
		return nil, err
	}
	ip := rec.Payload
	if b, err := s.Receipts.Blocked(ctx, ip.Digest); err != nil {
		return nil, err
	} else if b != nil {
		return nil, ErrBlocklisted
	}
	p := SmokePayload{Version: 1, ImportID: rec.ID, Package: ip.Package, Tag: ip.Tag, Digest: ip.Digest, OS: ip.OS, Variant: ip.Variant,
		KubernetesVersion: ip.KubernetesVersion, BuildVersion: ip.BuildVersion, ServiceID: ip.ServiceID, Node: ip.Node,
		TemplateVMID: rec.VMID, DiskStorage: ip.DiskStorage, Network: net}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(p)
	now := time.Now().UTC()
	out := &SmokeReceipt{ImportID: rec.ID, Payload: p, RequestedBy: actor, Auto: auto, CreatedAt: now, UpdatedAt: now, Step: SmokeQueued}
	err = s.DB.WithTx(ctx, func(tc context.Context) error {
		if err := s.DB.LockXact(tc, "templateupdate-smoke-enqueue:"+p.Key(0)); err != nil {
			return err
		}
		prior, err := s.Smokes.ForTemplate(tc, p.ServiceID, p.Node, p.TemplateVMID, p.Digest)
		if err != nil {
			return err
		}
		for i := range prior {
			if prior[i].Step != SmokeFinished && !prior[i].Cleaned {
				if st, _ := s.JobState(tc, prior[i].ID); st != "dead" {
					out = &prior[i]
					return errSmokeActive
				}
			}
		}
		jid, err := s.Jobs.EnqueueTx(tc, durablejobs.TemplateUpdateSmoke, p.Key(len(prior)), raw, SmokeMaxAttempts)
		if err != nil {
			return err
		}
		if existing, err := s.Smokes.LoadSmoke(tc, jid); err != nil {
			return err
		} else if existing != nil {
			out = existing
			return errSmokeActive
		}
		out.ID = jid
		return s.Smokes.SaveSmoke(tc, out)
	})
	if errors.Is(err, errSmokeActive) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

var errSmokeActive = errors.New("a smoke test of this template is already queued or running")

// AutoSmoke queues the automatic smoke test after an import finished (the runner's OnImported).
// netFor reads and checks (CheckSmokeNetwork) the provider's smoke network for the import's node; an
// unset or refused one is recorded on the import instead.
func (s *Service) AutoSmoke(ctx context.Context, id string, netFor func(ctx context.Context, serviceID, node string) (SmokeNetwork, error)) error {
	rec, err := s.Receipts.Load(ctx, id)
	if err != nil || rec == nil {
		return err
	}
	net, err := netFor(ctx, rec.Payload.ServiceID, rec.Payload.Node)
	if err == nil {
		_, err = s.EnqueueSmoke(ctx, id, net, "system:auto-smoke", true)
	}
	if err != nil {
		code, msg := "smoke_not_started", "The automatic smoke test could not be queued; run it from Template updates"
		var le *LifecycleError
		if errors.As(err, &le) {
			code, msg = le.Code, le.Message
		}
		_, serr := s.DB.C(ReceiptCollection).SetByID(ctx, id, map[string]any{"smoke": SmokeSummary{Code: code, Message: msg}}, nil)
		if serr != nil {
			return serr
		}
		return err
	}
	return nil
}

// ── state for the page ────────────────────────────────────────────────────────────────────────

// Lifecycle states of an imported template.
const (
	StateImporting        = "importing"
	StateImportFailed     = "import_failed"
	StateStaged           = "staged"
	StateSmokeRunning     = "smoke_running"
	StateSmokeFailed      = "smoke_failed"
	StateReady            = "ready_to_publish"
	StatePublishedDefault = "published_default"
	StatePublishedOlder   = "published_older"
	StateWithdrawn        = "withdrawn"
	// StateWithdrawIncomplete: the digest is blocklisted but this copy is not tagged withdrawn yet
	// (its provider was unreachable or the tag write failed): repeating Withdraw finishes it.
	StateWithdrawIncomplete = "withdraw_incomplete"
	StateTemplateMissing    = "template_missing"
	StateProviderUnreadable = "unknown"
)

// TemplateState is one imported template on the page.
type TemplateState struct {
	ID          string          `json:"id"` // import receipt id
	Package     string          `json:"package"`
	Tag         string          `json:"tag"`
	Digest      string          `json:"digest"`
	OS          string          `json:"os"`
	Variant     string          `json:"variant"`
	K8sVersion  string          `json:"kubernetesVersion,omitempty"`
	Build       string          `json:"buildVersion"`
	Slug        string          `json:"slug"`
	ServiceID   string          `json:"serviceId"`
	Node        string          `json:"node"`
	VMID        int             `json:"vmid,omitempty"`
	State       string          `json:"state"`
	Smoke       *SmokeSummary   `json:"smoke,omitempty"`
	Withdrawal  *BlockedDigest  `json:"withdrawal,omitempty"`
	ImportStep  Step            `json:"importStep"`
	ImportedAt  time.Time       `json:"importedAt"`
	RequestedBy string          `json:"requestedBy"`
	Tags        string          `json:"tags,omitempty"`
	LineCount   int             `json:"publishedOnLine"` // published versions of the line on the provider
	VersionsMap string          `json:"versionsMapLine,omitempty"`
	Actions     TemplateActions `json:"actions"`
	Warnings    []string        `json:"warnings,omitempty"`
}

// TemplateActions says which buttons apply (the server re-checks every one).
type TemplateActions struct {
	Smoke    bool `json:"smoke"`
	Publish  bool `json:"publish"`
	Withdraw bool `json:"withdraw"`
	// LastPublished: withdrawing leaves some provider without a published version of the line
	// (force + the slug needed); LastOnProviders names them.
	LastPublished   bool     `json:"lastPublished"`
	LastOnProviders []string `json:"lastOnProviders,omitempty"`
	// NodesLosingLast ("<service>/<node>"): nodes left without a published version of the line
	// (confirmNodeLoss needed).
	NodesLosingLast []string `json:"nodesLosingLast,omitempty"`
}

// ProviderRows is what States needs per provider: its cluster's qemu rows (nil when unreadable).
type ProviderRows struct {
	Rows  []pve.ClusterResource
	Error string
}

// States lists the newest imports with their lifecycle state. rows maps serviceId → that
// provider's cluster qemu rows.
func (s *Service) States(ctx context.Context, rows map[string]ProviderRows) ([]TemplateState, error) {
	recs, err := s.Receipts.Recent(ctx, 200)
	if err != nil {
		return nil, err
	}
	blocked, err := s.Receipts.BlockedSet(ctx)
	if err != nil {
		return nil, err
	}
	out := []TemplateState{}
	for _, rec := range recs {
		p := rec.Payload
		st := TemplateState{ID: rec.ID, Package: p.Package, Tag: p.Tag, Digest: p.Digest, OS: p.OS, Variant: p.Variant, K8sVersion: p.KubernetesVersion,
			Build: p.BuildVersion, Slug: slugOf(p), ServiceID: p.ServiceID, Node: p.Node, VMID: rec.VMID, Smoke: rec.Smoke,
			ImportStep: rec.Step, ImportedAt: rec.CreatedAt, RequestedBy: rec.RequestedBy}
		if b, ok := blocked[p.Digest]; ok {
			b := b
			st.Withdrawal = &b
		}
		switch {
		case rec.Step == StepFailed:
			st.State = StateImportFailed
			out = append(out, st)
			continue
		case rec.Step != StepSucceeded && rec.Step != StepTemplated:
			if state, _ := s.JobState(ctx, rec.ID); state == "dead" {
				st.State = StateImportFailed
			} else {
				st.State = StateImporting
			}
			out = append(out, st)
			continue
		}
		pr, ok := rows[p.ServiceID]
		if !ok || pr.Rows == nil {
			st.State = StateProviderUnreadable
			st.Warnings = append(st.Warnings, "the provider's cluster could not be read")
			// A blocklisted digest whose copy here could not be checked: Withdraw may finish it.
			st.Actions.Withdraw = st.Withdrawal != nil && s.VMEnabled
			out = append(out, st)
			continue
		}
		var row *pve.ClusterResource
		for i := range pr.Rows {
			r := &pr.Rows[i]
			if r.VMID == rec.VMID && r.Node == p.Node && (copyOf(*r, p) || r.NodeDown) {
				row = r
			}
		}
		if row == nil {
			st.State = StateTemplateMissing
			out = append(out, st)
			continue
		}
		if row.NodeDown {
			st.State = StateProviderUnreadable
			st.Warnings = append(st.Warnings, "the template's node is down")
			st.Actions.Withdraw = st.Withdrawal != nil && s.VMEnabled
			out = append(out, st)
			continue
		}
		st.Tags = row.Tags
		blocked := st.Withdrawal != nil
		tagged := hasTagValue(row.Tags, pve.TagTemplateWithdrawn)
		published := isPublished(row.Tags, p.Variant)
		// Published versions of the line on the provider and which one is the default (newest
		// pc-v- per node, the DiscoverTemplates rule; ties to the higher VMID).
		def := lineDefault(pr.Rows, p, p.Node)
		for _, r := range pr.Rows {
			if servesLine(r, p) {
				st.LineCount++
			}
		}
		smokeRunning := rec.Smoke != nil && rec.Smoke.JobID != "" && rec.Smoke.Result == "" && rec.Smoke.Step != SmokeFinished
		if smokeRunning {
			// A smoke job that died before recording a result is not running (the dead-job pass
			// records FAIL job_dead once it removed the test VM; a new smoke may start meanwhile).
			if state, _ := s.JobState(ctx, rec.Smoke.JobID); state == "dead" {
				smokeRunning = false
			}
		}
		switch {
		case tagged && blocked:
			st.State = StateWithdrawn
		case blocked:
			// The digest is withdrawn but THIS copy is not tagged yet (published or staged).
			st.State = StateWithdrawIncomplete
			st.Warnings = append(st.Warnings, "the digest is withdrawn but this copy is not: withdraw again to finish it")
		case tagged:
			st.State = StateWithdrawn
			st.Warnings = append(st.Warnings, "tagged pc-tu-withdrawn but its digest is not on the blocklist: withdraw records it")
		case published && def == rec.VMID:
			st.State = StatePublishedDefault
		case published:
			st.State = StatePublishedOlder
		case smokeRunning:
			st.State = StateSmokeRunning
		case rec.Smoke != nil && rec.Smoke.Result == SmokePass:
			st.State = StateReady
		case rec.Smoke != nil && rec.Smoke.Result == SmokeFail:
			st.State = StateSmokeFailed
		default:
			st.State = StateStaged
		}
		done := tagged && blocked
		st.Actions = TemplateActions{
			Smoke:    !blocked && !tagged && !smokeRunning && s.VMEnabled,
			Publish:  !blocked && !tagged && !published && rec.Smoke != nil && rec.Smoke.Result == SmokePass && s.VMEnabled,
			Withdraw: !done && s.VMEnabled,
		}
		if !done {
			// The guards Withdraw evaluates, across every provider holding a copy of the artifact.
			ids := make([]string, 0, len(rows))
			for id := range rows {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, svc := range ids {
				if rows[svc].Rows == nil {
					continue
				}
				pl := planWithdraw(rows[svc].Rows, p)
				if pl.lastOnProvider {
					st.Actions.LastPublished = true
					st.Actions.LastOnProviders = append(st.Actions.LastOnProviders, svc)
				}
				for _, n := range pl.nodesLosingLast {
					st.Actions.NodesLosingLast = append(st.Actions.NodesLosingLast, svc+"/"+n)
				}
			}
		}
		if p.Variant == "kubernetes" && published && !tagged {
			st.VersionsMap = fmt.Sprintf("%q: %q", p.KubernetesVersion, versionsMapValue(pr.Rows, p))
		}
		out = append(out, st)
	}
	return out, nil
}

// lineDefault is the VMID new VMs of p's line get on node (0 = none published there).
func lineDefault(rows []pve.ClusterResource, p VMPayload, node string) int {
	best, bestVer := 0, ""
	for _, r := range rows {
		if r.Node != node || !servesLine(r, p) {
			continue
		}
		v := pve.TagValue(r.Tags, pve.TagTemplateVersion)
		if best == 0 || v > bestVer || v == bestVer && r.VMID > best {
			best, bestVer = r.VMID, v
		}
	}
	return best
}

// versionsMapValue is the kamaji provider's config.cluster.versions value for p's Kubernetes
// version: the newest published copy per node ("pve1=9210,pve2=9211").
func versionsMapValue(rows []pve.ClusterResource, p VMPayload) string {
	nodes := map[string]bool{}
	for _, r := range rows {
		if servesLine(r, p) {
			nodes[r.Node] = true
		}
	}
	names := make([]string, 0, len(nodes))
	for n := range nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		if id := lineDefault(rows, p, n); id > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", n, id))
		}
	}
	return strings.Join(parts, ",")
}

// SmokeHistory lists recent smoke receipts (the page's Jobs tab).
func (s *Service) SmokeHistory(ctx context.Context) ([]SmokeReceipt, error) {
	return s.Smokes.Recent(ctx, 50)
}
