package project

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
)

// AdoptSandboxImages moves built-in templates to the image digest the prepull DaemonSet runs, once every node has
// pulled it (sandbox.Adoptions). The installer mirrors a new digest into the sandbox registry and bumps the DaemonSet;
// this job follows, so a new image never reaches a sandbox before it is on every node (docs/managed-sandbox.md §5).
// Off unless a sandbox provider sets imageAdoption ("dry-run" logs only).
func (h *Handler) AdoptSandboxImages(ctx context.Context) {
	if h.sandboxFor == nil || h.esSvc == nil {
		return
	}
	es, err := h.esSvc.List(ctx)
	if err != nil {
		slog.Error("sandbox image adoption: list providers", "err", err)
		return
	}
	for i := range es {
		e := &es[i]
		if !e.IsSandbox() {
			continue
		}
		ss, err := h.sandboxFor(e)
		if err != nil {
			continue
		}
		mode := ss.Config().ImageAdoption
		if mode == "" || mode == "off" {
			continue
		}
		ads, err := ss.TemplateAdoptions(ctx)
		if err != nil {
			slog.Error("sandbox image adoption: read prepull pods", "service", e.ID, "err", err)
			continue
		}
		for _, a := range ads {
			l := slog.With("service", e.ID, "template", a.Template, "from", a.From, "to", a.To)
			if !a.Ready {
				l.Info("sandbox image adoption waiting", "pulled", fmt.Sprintf("%d/%d", a.Pulled, a.Total))
				continue
			}
			if mode != "on" {
				l.Info("sandbox image adoption: would adopt (dry-run)")
				continue
			}
			changed, err := h.esSvc.SetTemplateImage(ctx, e.ID, a.Template, a.From, a.To)
			if err != nil {
				l.Error("sandbox image adoption", "err", err)
				continue
			}
			if !changed {
				continue // edited in the meantime: the next run compares again
			}
			l.Info("sandbox image adopted")
			if h.audit != nil {
				ev := audit.SystemEvent()
				ev.Action = audit.ActionUpdate
				ev.ResourceType = "EXTERNAL_SERVICE"
				ev.ResourceID = e.ID
				ev.ResourceDisplayName = e.Name
				ev.Changes = []audit.PropertyChange{{Field: "config.sandbox.templates." + a.Template + ".image", OldValue: a.From, NewValue: a.To}}
				ev.Outcome = audit.OutcomeSuccess
				h.audit.LogAsync(ev)
			}
		}
	}
}
