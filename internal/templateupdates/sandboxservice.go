package templateupdates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

// SandboxRequest is the admin's mirror request: one built-in image, the digest discovery offered (confirmed in the
// dialog) and the sandbox provider whose registry and prepull DaemonSets it targets.
type SandboxRequest struct {
	ServiceID     string `json:"serviceId"`
	Name          string `json:"name"`
	Digest        string `json:"digest"`
	ConfirmDigest string `json:"confirmDigest"`
}

// SandboxRollbackRequest re-points the prepull DaemonSets at the pin a job replaced.
type SandboxRollbackRequest struct {
	ServiceID string `json:"serviceId"`
	Name      string `json:"name"`
	// ConfirmDigest must repeat the digest the rollback returns to (shown in the dialog).
	ConfirmDigest string `json:"confirmDigest"`
}

// SandboxTarget is what the admin handler resolves from the provider before enqueue (no secret).
type SandboxTarget struct {
	ServiceID string
	Registry  string // host[:port]
	// Current is what the prepull DaemonSets pull for the image now ("" when none), read live.
	Current []string
}

// Errors the admin surface maps to HTTP answers.
var (
	ErrSandboxDisabled   = errors.New("sandbox template updates are not enabled on this site (PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED)")
	ErrSandboxSameDigest = errors.New("the prepull DaemonSets already run this digest: there is nothing to update")
	ErrSandboxBusy       = errors.New("another update of this sandbox image is queued or running; wait for it to finish")
	ErrSandboxNoRollback = errors.New("there is no earlier pinned digest to roll back to")
)

// EnqueueSandbox re-lists the package and re-verifies the image by digest server-side (the client's tag is never
// trusted), then in ONE transaction under an advisory lock on the mirrored repository refuses a concurrent job for the
// same image and enqueues the job with its receipt.
func (s *Service) EnqueueSandbox(ctx context.Context, req SandboxRequest, t SandboxTarget, actor string) (*SandboxReceipt, error) {
	if !s.SandboxEnabled {
		return nil, ErrSandboxDisabled
	}
	if req.ConfirmDigest != req.Digest || !digestRE.MatchString(req.Digest) {
		return nil, errors.New("confirm the exact digest to mirror")
	}
	if !sandboxNameRE.MatchString(req.Name) {
		return nil, errors.New("invalid sandbox image name")
	}
	reg, _, err := s.registry(ctx)
	if err != nil {
		return nil, err
	}
	versions, err := reg.ListVersions(ctx, SandboxPackage(req.Name))
	if err != nil {
		return nil, err
	}
	var cand *SandboxCandidate
	for _, c := range NewestSandbox(req.Name, versions) {
		if c.Digest == req.Digest {
			c := c
			cand = &c
			break
		}
	}
	if cand == nil {
		return nil, errors.New("that digest is not a published version of the image with a contract tag")
	}
	m, _, err := reg.GetManifest(ctx, cand.Package, cand.Digest)
	if err == nil {
		err = CheckSandboxImageManifest(m)
	}
	if err == nil && m.MediaType != MediaOCIManifest && m.MediaType != MediaDockerManifest {
		// The mirror copies one linux/amd64 image manifest by its own digest; an index digest cannot be preserved.
		err = errors.New("the published digest is a multi-platform index, not a linux/amd64 image manifest")
	}
	if err != nil {
		return nil, fmt.Errorf("the image failed verification: %w", err)
	}
	p := SandboxPayload{Version: 1, Action: SandboxMirror, ServiceID: t.ServiceID, Name: req.Name, Registry: t.Registry, Tag: cand.Tag, Digest: cand.Digest}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	for _, cur := range t.Current {
		if _, d, _ := strings.Cut(cur, "@"); d == p.Digest {
			return nil, ErrSandboxSameDigest
		}
	}
	return s.enqueueSandbox(ctx, p, actor, func(tc context.Context) error {
		pin, err := s.Sandbox.GetPin(tc, p.Repo())
		if err != nil {
			return err
		}
		if pin != nil && pin.Digest == p.Digest && len(t.Current) == 0 {
			return ErrSandboxSameDigest
		}
		return nil
	})
}

// EnqueueSandboxRollback re-points the prepull DaemonSets at the pin record's previous ref (adoption follows). The
// image is never deleted, so the previous digest is still in the registry; the job re-verifies that by digest.
func (s *Service) EnqueueSandboxRollback(ctx context.Context, req SandboxRollbackRequest, t SandboxTarget, actor string) (*SandboxReceipt, error) {
	if !s.SandboxEnabled {
		return nil, ErrSandboxDisabled
	}
	if !sandboxNameRE.MatchString(req.Name) {
		return nil, errors.New("invalid sandbox image name")
	}
	repo := t.Registry + "/library/sandbox-" + req.Name
	pin, err := s.Sandbox.GetPin(ctx, repo)
	if err != nil {
		return nil, err
	}
	if pin == nil || pin.Previous == "" {
		return nil, ErrSandboxNoRollback
	}
	prevRepo, tag, digest, ok := splitPinnedRef(pin.Previous)
	if !ok || prevRepo != repo || !sandboximport.MirrorTagRE.MatchString(tag) {
		return nil, errors.New("the previous pin is not a <registry>/library/sandbox-<name>:<YYYYMMDD>-<sha7>@<digest> ref; roll back through images.yaml and make upgrade")
	}
	if req.ConfirmDigest != digest {
		return nil, errors.New("confirm the exact digest to roll back to")
	}
	p := SandboxPayload{Version: 1, Action: SandboxRollback, ServiceID: t.ServiceID, Name: req.Name, Registry: t.Registry, Tag: tag, Digest: digest}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	for _, cur := range t.Current {
		if cur == p.Ref() {
			return nil, ErrSandboxSameDigest
		}
	}
	return s.enqueueSandbox(ctx, p, actor, func(tc context.Context) error {
		again, err := s.Sandbox.GetPin(tc, repo)
		if err != nil {
			return err
		}
		if again == nil || again.Previous != pin.Previous {
			return errors.New("the pin changed meanwhile; reload the page")
		}
		return nil
	})
}

func (s *Service) enqueueSandbox(ctx context.Context, p SandboxPayload, actor string, check func(tc context.Context) error) (*SandboxReceipt, error) {
	raw, _ := json.Marshal(p)
	now := time.Now().UTC()
	rec := &SandboxReceipt{Kind: "sandbox", Payload: p, RequestedBy: actor, CreatedAt: now, UpdatedAt: now, Step: SbxQueued}
	err := s.DB.WithTx(ctx, func(tc context.Context) error {
		if err := s.DB.LockXact(tc, p.LockKey()); err != nil {
			return err
		}
		prior, err := s.Sandbox.ByRepo(tc, p.Registry, p.Name)
		if err != nil {
			return err
		}
		same := 0
		for _, r := range prior {
			if r.Payload.Key() == p.Key() {
				same++
			}
			if SandboxTerminal(&r) {
				continue
			}
			if state, err := s.JobState(tc, r.ID); err == nil && state == "dead" {
				continue
			}
			return ErrSandboxBusy // one job per image at a time (dedup per template)
		}
		if err := check(tc); err != nil {
			return err
		}
		key := p.Key()
		if same > 0 {
			key = fmt.Sprintf("%s-%d", key, same)
		}
		id, err := s.Jobs.EnqueueTx(tc, durablejobs.TemplateUpdateSandbox, key, raw, 3)
		if err != nil {
			return err
		}
		if existing, err := s.Sandbox.Load(tc, id); err != nil {
			return err
		} else if existing != nil {
			return ErrSandboxBusy
		}
		rec.ID = id
		return s.Sandbox.Save(tc, rec)
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// splitPinnedRef splits <repo>:<tag>@<digest>.
func splitPinnedRef(ref string) (repo, tag, digest string, ok bool) {
	repoTag, digest, ok := strings.Cut(ref, "@")
	if !ok || !digestRE.MatchString(digest) {
		return "", "", "", false
	}
	i := strings.LastIndex(repoTag, ":")
	if i <= strings.LastIndex(repoTag, "/") {
		return "", "", "", false
	}
	return repoTag[:i], repoTag[i+1:], digest, true
}

// SandboxHistory returns the newest sandbox receipts.
func (s *Service) SandboxHistory(ctx context.Context) ([]SandboxReceipt, error) {
	return s.Sandbox.Recent(ctx, 50)
}

// SandboxPinList lists the pin records (installer stage 55 and the page).
func (s *Service) SandboxPinList(ctx context.Context, registry string) ([]SandboxPin, error) {
	return s.Sandbox.Pins(ctx, registry)
}

// ImagesYAMLLine is the deploy/templates/images.yaml line to commit for a pin (the source ref, same digest).
func ImagesYAMLLine(name, tag, digest string) string {
	return "  " + name + ": " + DefaultRegistry + "/" + Owner + "/" + SandboxPackage(name) + ":" + tag + "@" + digest
}

// OrphanCluster is what the orphan sweep needs from one sandbox provider's cluster.
type OrphanCluster interface {
	ListAllMirrorPods(ctx context.Context) ([]sandbox.MirrorPod, error)
	DeleteMirrorPod(ctx context.Context, name, uid string) error
}

// SweepMirrorOrphans deletes the mirror worker pods of one provider (and, by ownerReference, their credential
// Secrets) whose job is not live: no durable job row for the pod's job label, or a job that is dead or succeeded.
// A pod of a pending/running job is never touched. jobLive maps the 16-hex job label to "is that job live".
func SweepMirrorOrphans(ctx context.Context, cl OrphanCluster, jobLive func(ctx context.Context, short string) (bool, error)) (int, error) {
	pods, err := cl.ListAllMirrorPods(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range pods {
		live, err := jobLive(ctx, p.JobShort)
		if err != nil {
			return n, err
		}
		if live {
			continue
		}
		if err := cl.DeleteMirrorPod(ctx, p.Name, p.UID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// JobLiveByShort reports whether a sandbox job whose id starts with the 16-hex label is pending or running.
func (s PGSandbox) JobLiveByShort(ctx context.Context, short string) (bool, error) {
	if len(short) != 16 || strings.Trim(short, "0123456789abcdef") != "" {
		return false, nil
	}
	var live bool
	err := s.DB.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM durable_job WHERE kind=$1 AND state IN ('pending','running')
	 AND replace(id::text,'-','') LIKE $2)`, durablejobs.TemplateUpdateSandbox, short+"%").Scan(&live)
	return live, err
}
