package admin

// platform_sandbox.go — what a platform run writes on its Proxmox provider for the sandbox machines, and the two
// refusals that belong to a platform's sandbox provider (docs/managed-services.md, Sandboxes).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// PlatformSandboxSite writes the sandbox settings of an installation on its Proxmox provider row, in one write:
// config.network.sandboxCidrs (the one range), config.sandbox (the block the provider sync fences by) and, when the
// run has it, secret.sandboxKubeconfig, sealed. A nil site.Config takes all three away again.
//
// One provider fences one sandbox pool and one sandbox network. A block that is not this installation's (the
// installer's sandbox cluster has none of the marker; another installation has its own) is never overwritten, and a
// sandbox network other than the installation's range is never dropped: a write over either answers
// platformlife.ErrProviderConflict and leaves the row as it was, and a removal takes away only what is the
// installation's own.
func (h *Handler) PlatformSandboxSite(ctx context.Context, in *platformlife.Installation, site *platformlife.SandboxSite) error {
	return h.repo.db.WithTx(ctx, func(ctx context.Context) error {
		if err := h.repo.db.LockXact(ctx, "platform-run:"+in.ID); err != nil {
			return err
		}
		doc, err := h.repo.FindDoc(ctx, externalServiceCollection, in.ServiceID)
		if err != nil {
			return err
		}
		if doc == nil {
			return serviceNotFoundErr(in.ServiceID)
		}
		changed, err := applySandboxSite(doc, in.ID, site, func(secret pgdoc.M, value string) { setSecretLeaf(h, secret, "sandboxKubeconfig", value) })
		if err != nil || !changed {
			return err
		}
		return h.repo.ReplaceDoc(ctx, externalServiceCollection, in.ServiceID, doc)
	})
}

// PlatformSandboxOff takes the identity of an installation's sandbox provider away and marks the row as having no
// placement since at (platformlife.SandboxProviderOff), under the installation's run lock, as PlatformRegister gives
// both back. The run calls it when the last sandbox machine is gone. A row that says so already is not written, and an
// installation without a sandbox provider has none. It answers the ids of the rows it wrote.
func (h *Handler) PlatformSandboxOff(ctx context.Context, installationID string, at time.Time) (written []string, err error) {
	err = h.repo.db.WithTx(ctx, func(ctx context.Context) error {
		if err := h.repo.db.LockXact(ctx, "platform-run:"+installationID); err != nil {
			return err
		}
		rows, err := h.repo.ListRaw(ctx, externalServiceCollection)
		if err != nil {
			return err
		}
		for _, row := range rows {
			cfg, _ := row["config"].(map[string]any)
			if cfg[platformlife.ProviderMarker] != installationID || !platformlife.SandboxProviderOff(row, at) {
				continue
			}
			id, _ := row["_id"].(string)
			if err := h.repo.ReplaceDoc(ctx, externalServiceCollection, id, row); err != nil {
				return err
			}
			written = append(written, id)
		}
		return nil
	})
	return written, err
}

// PlatformSandboxOffRun is PlatformSandboxOff as the run's hook (platformlife.Runner.SandboxOff).
func (h *Handler) PlatformSandboxOffRun(ctx context.Context, installationID string, at time.Time) error {
	_, err := h.PlatformSandboxOff(ctx, installationID, at)
	return err
}

// PlatformSandboxOffRepair runs once when the API starts, and makes good what v0.12.0-rc.2 left: its off took the
// sandbox machines away and kept the sandbox provider's kubeconfig, so every project attached to that provider failed
// its teardown with 401 from then on and its rows stayed. A DISABLED sandbox provider of an installation that records
// no built sandboxes (status.sandbox is empty: an off or a Remove ended, or the installation is gone) gets what the
// off writes now (PlatformSandboxOff), with the time of this start: the time of that off is not recorded anywhere. The
// next deletion sweep and the next sandbox read then end what was left, with nothing done by hand.
//
// A run records status.sandbox before it makes the first sandbox machine and takes it away after the last one is gone,
// and stores a kubeconfig only in between: a provider with built sandboxes is never touched here, and neither is one
// that is not DISABLED. Nor is a row that carries the mark already: an off of this release, or this pass, wrote it,
// and a kubeconfig that stands beside it was set by hand since, which is a placement (SandboxOffSince). Without that
// the pass would take such a kubeconfig away at every start and the rows of what runs on its cluster would be ended.
// It answers the ids of the rows it wrote.
// ponytail: once at the start; a pass that fails is logged and waits for the next start.
func (h *Handler) PlatformSandboxOffRepair(ctx context.Context) ([]string, error) {
	if h.platform == nil {
		return nil, nil
	}
	rows, err := h.repo.ListRaw(ctx, externalServiceCollection)
	if err != nil {
		return nil, err
	}
	var written []string
	for _, row := range rows {
		cfg, _ := row["config"].(map[string]any)
		installationID, _ := cfg[platformlife.ProviderMarker].(string)
		if cfg["provider"] != "sandbox" || installationID == "" || row["status"] != externalservice.StatusDisabled {
			continue
		}
		if sb, _ := cfg["sandbox"].(map[string]any); sb[externalservice.SandboxOffSinceKey] != nil {
			continue
		}
		in, err := h.platform.Get(ctx, installationID)
		if err != nil && !errors.Is(err, platformlife.ErrNotFound) {
			return written, err
		}
		if in != nil && in.Status.Sandbox != nil {
			continue
		}
		ids, err := h.PlatformSandboxOff(ctx, installationID, time.Now().UTC())
		if err != nil {
			return written, err
		}
		for _, id := range ids {
			slog.Info("platform: a sandbox provider of sandboxes that are switched off kept its identity; it is taken away and the provider has no placement now", "serviceId", id, "installation", installationID)
		}
		written = append(written, ids...)
	}
	return written, nil
}

// applySandboxSite makes the change on the provider document; changed is false when the document already says it.
func applySandboxSite(doc pgdoc.M, installationID string, site *platformlife.SandboxSite, seal func(secret pgdoc.M, value string)) (changed bool, err error) {
	cfg := ensureConfig(doc)
	network := ensureMap(cfg, "network")
	var cidrs []string
	switch raw := network["sandboxCidrs"].(type) {
	case []any:
		for _, c := range raw {
			cidrs = append(cidrs, strings.TrimSpace(fmt.Sprint(c)))
		}
	case []string:
		cidrs = raw
	}
	have, _ := cfg["sandbox"].(map[string]any)
	owner, _ := have[platformlife.ProviderMarker].(string)
	secret := ensureMap(doc, "secret")
	if site.Config == nil {
		// Only what is this installation's goes: its own block with the kubeconfig that belongs to it, and its own
		// range. A block or a sandbox network of another owner stays, and is no reason to stop a Remove.
		if have != nil && owner == installationID {
			delete(cfg, "sandbox")
			delete(secret, "sandboxKubeconfig")
			changed = true
		}
		if kept := slices.DeleteFunc(slices.Clone(cidrs), func(c string) bool { return c == site.Range }); len(kept) != len(cidrs) {
			changed = true
			if network["sandboxCidrs"] = kept; len(kept) == 0 {
				delete(network, "sandboxCidrs")
			}
		}
		return changed, nil
	}
	// Any block that is not this installation's, also one without a pool (inert for the sync, and still someone's
	// settings: a registry, an entry appliance): never overwritten.
	if len(have) > 0 && owner != installationID {
		pool, _ := have["pool"].(string)
		return false, fmt.Errorf("%w: the provider's config.sandbox (pool %q) is not installation %s's. One provider fences one sandbox pool: remove that sandbox cluster and its config.sandbox first",
			platformlife.ErrProviderConflict, pool, installationID)
	}
	if others := slices.DeleteFunc(slices.Clone(cidrs), func(c string) bool { return c == site.Range }); len(others) > 0 {
		return false, fmt.Errorf("%w: the provider names the sandbox network %s (config.network.sandboxCidrs), which is not installation %s's range %s. One provider fences one sandbox network",
			platformlife.ErrProviderConflict, strings.Join(others, ", "), installationID, site.Range)
	}
	if !sameJSONValue(have, site.Config) || !slices.Equal(cidrs, []string{site.Range}) {
		cfg["sandbox"], network["sandboxCidrs"], changed = site.Config, []any{site.Range}, true
	}
	if site.Kubeconfig != "" {
		// Sealed with a fresh nonce each time, so the stored text never compares: written whenever the run holds it.
		seal(secret, site.Kubeconfig)
		changed = true
	}
	return changed, nil
}

// sameJSONValue compares two values as the store would hold them (a map's keys in order, 20 and 20.0 alike).
func sameJSONValue(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// sandboxNeedsScheduler refuses a platform's sandbox provider "live" while the job scheduler is off. The sync that
// fences a sandbox machine, takes its unsecured taint off and puts it back on a machine that lost its fence runs on
// the scheduler, as do the minutely sandbox read and the abuse check; one operator sync fences once and nothing
// watches afterwards. The installer's sandbox providers are not asked: their sites decide the scheduler themselves.
func (h *Handler) sandboxNeedsScheduler(doc pgdoc.M) *httpx.HTTPError {
	cfg, _ := doc["config"].(map[string]any)
	if marker, _ := cfg[platformlife.ProviderMarker].(string); marker == "" || h.jobsScheduler {
		return nil
	}
	return httpx.NewError(http.StatusConflict, http.StatusConflict,
		"This sandbox provider cannot go live while the job scheduler is off: the provider sync that fences each sandbox machine and keeps watching its firewall, the minutely sandbox read "+
			"and the abuse check all run on it. Turn the scheduler on (./proxcloud-up.sh install --scheduler, or PROXCLOUD_JOBS_SCHEDULER_ENABLED=true), then set the provider live. "+
			"The same switch starts charging usage.")
}

// sandboxUnpricedNote is what the refusal says while no price rule prices sandbox minutes.
const sandboxUnpricedNote = "No enabled public price plan that bills this provider has a rule for sandbox minutes (resource type sandbox), so every sandbox minute would be charged nothing. " +
	"The Platform setup defaults add no sandbox rule. Add one under System, Price plans (docs/pricing-rate-card.md lists the default plan's rates per vCPU and per GB per minute), " +
	"or a rule at 0 to give sandbox minutes away knowingly, then set the provider live."

// sandboxUnpriced is why a platform's sandbox provider may not be live for its prices: "" while a rule prices sandbox
// minutes, sandboxUnpricedNote otherwise. The charge rates a resource that matches no rule as nothing and says
// nothing (unpricedflavors.go), and the default price plan of the Platform setup defaults has no sandbox rule
// (defaultRuleTypes), so without this a site would give sandbox minutes away without knowing. Like the unpriced
// flavors guard it reads the enabled public plans that cover the provider, on any time unit; a plan scoped to single
// billing profiles is not counted. A plan store that cannot be read counts as no rule. The installer's sandbox
// providers are not asked: their sites went live before this check and are left as they are.
func (h *Handler) sandboxUnpriced(ctx context.Context, doc pgdoc.M) string {
	cfg, _ := doc["config"].(map[string]any)
	if marker, _ := cfg[platformlife.ProviderMarker].(string); marker == "" {
		return ""
	}
	if h.pricing == nil {
		return sandboxUnpricedNote
	}
	id, _ := doc["_id"].(string)
	plans := pricing.SelectPricePlansForService(h.pricing.PlanSource(ctx), nil, true, id)
	for _, tu := range pricePlanTimeUnits {
		if slices.ContainsFunc(pricing.ApplicableRules(plans, h.pricing.RuleSource(ctx), tu), func(r pricing.PricePlanRule) bool {
			return strings.EqualFold(r.ResourceType, "sandbox")
		}) {
			return ""
		}
	}
	return sandboxUnpricedNote
}

// sandboxOffRefused is why an update may not switch off the sandboxes a run built: their provider is live.
func (h *Handler) sandboxOffRefused(ctx context.Context, old, next *platformlife.Installation) string {
	id, off := platformlife.SandboxSwitchedOff(old, next)
	if !off || id == "" || h.esSvc == nil {
		return ""
	}
	es, err := h.esSvc.Get(ctx, id)
	if err != nil || es == nil || !es.IsSandbox() {
		return ""
	}
	return platformlife.SandboxOffBlocked(es)
}
