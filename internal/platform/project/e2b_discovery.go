package project

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

const e2bDocsPath = "/docs/api/e2b"

// SetE2BConsoleURL uses the configured customer console, not the sandbox API host or
// untrusted Host/forwarded headers. The docs route needs no sign-in; network/VPN
// restrictions are unchanged. Without a configured URL, give a console-relative hint.
func (h *Handler) SetE2BConsoleURL(base string) { h.consoleURL = strings.TrimRight(base, "/") }

func (h *Handler) e2bDocs() string {
	if h.consoleURL == "" {
		return "console " + e2bDocsPath
	}
	return h.consoleURL + e2bDocsPath
}

func (h *Handler) unknownTemplate(ctx context.Context, c *e2bCaller, ref string) string {
	catalog := readyTemplateCatalog(c.proj.ID, c.ss.Config(), h.projectTemplates(ctx, c.proj.ID))
	return fmt.Sprintf("unknown template %.128q (available: %s); use name[:tag] or template ID; GET /templates for this project's catalog; see %s", ref, catalog, h.e2bDocs())
}

// readyTemplateCatalog is intentionally bounded and defensively project-scoped even
// when passed a mixed collection. Internal pause/fork snapshots and not-ready builds
// must not leak through an error message. Named, public-facing snapshots are allowed.
func readyTemplateCatalog(projectID string, cfg sandbox.Config, docs []sandboxTemplateDoc) string {
	items := cfg.TemplateNames()
	for _, t := range docs {
		if t.ProjectID != projectID || t.Internal {
			continue
		}
		if b := t.defaultBuild(); b != nil && b.Image != "" {
			items = append(items, fmt.Sprintf("%s (ID %s)", t.Name, t.ID))
		}
	}
	sort.Strings(items)
	shown := make([]string, 0, min(len(items), 12))
	bytes := 0
	for _, item := range items {
		if len(shown) >= 12 || bytes+len(item) > 1000 {
			break
		}
		shown = append(shown, item)
		bytes += len(item) + 2
	}
	if len(shown) == 0 {
		return "none ready"
	}
	out := strings.Join(shown, ", ")
	if len(shown) < len(items) {
		out += ", … (more in GET /templates)"
	}
	return out
}
