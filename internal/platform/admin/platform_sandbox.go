package admin

// platform_sandbox.go — what a platform run writes on its Proxmox provider for the sandbox machines, and the two
// refusals that belong to a platform's sandbox provider (docs/managed-services.md, Sandboxes).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
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
	if pool, _ := have["pool"].(string); pool != "" && owner != installationID {
		return false, fmt.Errorf("%w: the provider's config.sandbox fences pool %s, which is not installation %s's. One provider fences one sandbox pool: remove that sandbox cluster and its config.sandbox first",
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
			"The same switch starts charging usage, and the sandbox price rule holds a placeholder rate until you set one (or 0).")
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
