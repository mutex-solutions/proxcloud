package server

import (
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/account"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/adminapi"
	"github.com/mutex-solutions/proxcloud/internal/platform/affiliate"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/catalog"
	"github.com/mutex-solutions/proxcloud/internal/platform/feature"
	"github.com/mutex-solutions/proxcloud/internal/platform/job"
	"github.com/mutex-solutions/proxcloud/internal/platform/mcp"
	"github.com/mutex-solutions/proxcloud/internal/platform/order"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/platformconfig"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/projectinvite"
	"github.com/mutex-solutions/proxcloud/internal/platform/promotion"
	"github.com/mutex-solutions/proxcloud/internal/platform/sse"
	"github.com/mutex-solutions/proxcloud/internal/platform/sso"
	"github.com/mutex-solutions/proxcloud/pkg/auth"
)

// TestMCPToolRoutesExist checks every MCP tool's method and path against the real app router: a
// tool whose route was renamed or removed would otherwise only fail when an agent calls it. The
// handlers are zero values; only route registration runs.
func TestMCPToolRoutesExist(t *testing.T) {
	mcpH := mcp.New(slog.Default(), nil, "", "", "")
	r := AppRouter(slog.Default(), nil, &auth.Authenticator{}, &account.Handler{}, &org.Handler{}, &org.RoleHandler{},
		&org.AuditHandler{}, &org.BillingHandler{}, &billing.ConfigHandler{}, &project.Handler{}, &platformconfig.Handler{},
		&feature.Handler{}, &promotion.Handler{}, &affiliate.Handler{}, &catalog.Handler{}, &order.Handler{},
		&projectinvite.Handler{}, &admin.Handler{}, &adminapi.Handler{}, &sse.Handler{}, &job.Handler{}, mcpH, &sso.Handler{})
	routes, ok := r.(chi.Routes)
	if !ok {
		t.Fatal("AppRouter is not a chi router")
	}
	// The matcher must be able to say no, or the loop below proves nothing.
	if routes.Match(chi.NewRouteContext(), "GET", "/api/v1/project/x1/no-such-route") ||
		routes.Match(chi.NewRouteContext(), "DELETE", "/api/v1/project/x1/service") {
		t.Fatal("router matched a route that does not exist")
	}
	placeholder := regexp.MustCompile(`\{[^}]+\}`)
	tools := mcp.ToolRoutes()
	if len(tools) < 179 {
		t.Fatalf("only %d tools listed", len(tools))
	}
	for _, tr := range tools {
		path, _, _ := strings.Cut(tr.Path, "?")
		path = placeholder.ReplaceAllString(path, "x1")
		if !routes.Match(chi.NewRouteContext(), tr.Method, path) {
			t.Errorf("%s: no route for %s %s", tr.Name, tr.Method, tr.Path)
		}
	}
}
