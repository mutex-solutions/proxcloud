package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/account"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/affiliate"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/catalog"
	"github.com/mutex-solutions/proxcloud/internal/platform/order"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/platformconfig"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/projectinvite"
	"github.com/mutex-solutions/proxcloud/internal/platform/promotion"
	"github.com/mutex-solutions/proxcloud/internal/platform/sso"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestUntypedErrorAnswers500 runs one route of each handler package that answers through
// httpx.WriteError against a database that refuses every connection. The untyped error must answer
// 500 with the generic envelope (before, several answered 200 with an empty body) and the error text
// must stay out of the response.
func TestUntypedErrorAnswers500(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := &pgdoc.DB{Pool: pool}
	users := user.NewRepo(db)
	billingRepo := billing.NewRepo(db)
	orgRepo := org.NewRepo(db)
	auditSvc := audit.NewService(audit.NewRepo(db), slog.Default())
	adminH := admin.NewHandler(admin.NewRepo(db), catalog.NewRepo(db), users, nil, pricing.NewRepo(db), billingRepo,
		cloud.NewRepo(db), platformconfig.NewRepo(db), auditSvc, nil, "", "", "")
	secret := errors.New("lookup failed: secret detail")

	cases := []struct {
		pkg, method, path, body string
		routes                  func(chi.Router)
		rc                      httpx.RequestContext
	}{
		{"account", "GET", "/account/details", "", account.NewHandler(users, auditSvc, nil).Routes, httpx.RequestContext{Sub: "u1"}},
		{"admin", "POST", "/admin/tax", `{"level":"COUNTRY"}`, adminH.Routes, httpx.RequestContext{SigV4KeyID: "k1"}},
		{"affiliate", "GET", "/affiliate/project/p1/config", "", affiliate.NewHandler(affiliate.NewRepo(db), nil, nil, billingRepo, users).Routes, httpx.RequestContext{Sub: "u1"}},
		{"catalog", "GET", "/flavor-categories", "", catalog.NewHandler(catalog.NewRepo(db)).Routes, httpx.RequestContext{Sub: "u1"}},
		{"order", "GET", "/orders/o1", "", order.NewHandler(order.NewRepo(db), users).Routes, httpx.RequestContext{Sub: "u1"}},
		{"org", "GET", "/organizations", "", org.NewHandler(nil, org.NewPolicy(orgRepo), orgRepo, users, auditSvc).Routes, httpx.RequestContext{Sub: "u1"}},
		{"project", "GET", "/project", "", project.NewHandler(nil, nil, nil, users, auditSvc, billingRepo, nil, nil, nil, nil, "").Routes, httpx.RequestContext{Sub: "u1"}},
		{"projectinvite", "GET", "/project-invites/mine", "", projectinvite.NewHandler(projectinvite.NewRepo(db), users, nil, orgRepo, auditSvc, nil, "").Routes, httpx.RequestContext{Sub: "u1"}},
		{"promotion", "POST", "/promotion/bp1/code?code=X", "", promotion.NewHandler(billingRepo, func(context.Context, string, string) (string, string, error) {
			return "", "", secret
		}).Routes, httpx.RequestContext{Sub: "u1"}},
		{"sso", "GET", "/organizations/o1/sso", "", sso.NewHandler(nil, org.NewPolicy(orgRepo), users, "", nil).Routes, httpx.RequestContext{Sub: "u1"}},
	}
	const want = `{"errors":{"code":500,"message":"internal.error"}}`
	for _, c := range cases {
		t.Run(c.pkg, func(t *testing.T) {
			r := chi.NewRouter()
			r.Use(middleware.RequestID, exposeRequestID)
			rc := c.rc
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					next.ServeHTTP(w, req.WithContext(httpx.WithRC(req.Context(), &rc)))
				})
			})
			c.routes(r)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
			body := strings.TrimSpace(rec.Body.String())
			if rec.Code != http.StatusInternalServerError || body != want {
				t.Fatalf("%s %s = %d %q, want 500 %s", c.method, c.path, rec.Code, body, want)
			}
			if rec.Header().Get(httpx.RequestIDHeader) == "" {
				t.Fatal("no request id header on the response")
			}
		})
	}
}
