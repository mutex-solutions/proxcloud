package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// audit_mw.go — a global admin audit trail. A per-resource audit event (with a
// PropertyChange diff) could be emitted from each service mutation; wiring that into all ~129 handlers
// is deferred (// TODO(audit) markers remain). This middleware gives the audit trail in ONE place: after any
// SUCCESSFUL (2xx) admin POST/PUT/DELETE it logs an ADMIN_AREA / PLATFORM event with the actor (from
// the token), action (method), resourceType (from the path), and resourceId (response data.id or an
// id-like path segment). Coarser (no field-level diff), but every admin mutation is now
// recorded and shows up in GET /admin/audit. 501s never reach 2xx → never audited.

// auditMiddleware wraps the /admin routes (registered via r.Use in Routes()).
func (h *Handler) auditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.audit == nil || !isMutation(r.Method) || auditSkip(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		rc := httpx.RC(r.Context())
		// Carry a snapshot holder so a mutation handler can RecordSnapshots(before, after); we diff
		// them into the event's Changes (diffSnapshots(before, after)).
		ctx, holder := audit.WithSnapshotCapture(r.Context())
		r = r.WithContext(ctx)
		cap := &auditCapture{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(cap, r)
		if cap.status < 200 || cap.status >= 300 {
			return // failures / 501 / 4xx → not audited (only SUCCESS is logged here)
		}
		// Actor: the OIDC subject, else the SigV4 key id (MCP api-key principals have no Sub —
		// without this their mutations would audit with an empty actor).
		actor := rc.Sub
		if actor == "" {
			actor = rc.SigV4KeyID
		}
		ev := audit.AdminEvent(actor, actor)
		ev.EventContext = audit.ContextPlatform
		ev.Action = methodToAction(r.Method)
		ev.ResourceType = pathResourceType(r.URL.Path)
		ev.ResourceID = bodyDataID(cap.body.Bytes())
		if ev.ResourceID == "" {
			ev.ResourceID = pathTailID(r.URL.Path)
		}
		ev.Changes = holder.Changes() // field-level diff when the handler recorded before/after
		ev.Outcome = audit.OutcomeSuccess
		if ev.Actor != nil {
			ev.Actor.IPAddress = clientIP(r)
			ev.Actor.UserAgent = r.UserAgent()
		}
		h.audit.LogEvent(ev)
	})
}

func isMutation(m string) bool {
	return m == http.MethodPost || m == http.MethodPut || m == http.MethodDelete
}

// auditSkip drops read-via-POST admin endpoints (the Proxmox and object store connection tests — no
// state change → not an auditable mutation).
func auditSkip(path string) bool {
	return strings.HasSuffix(path, "/service/proxmox/auth") || strings.HasSuffix(path, "/object-store/test")
}

func methodToAction(m string) string {
	switch m {
	case http.MethodPost:
		return audit.ActionCreate
	case http.MethodPut:
		return audit.ActionUpdate
	case http.MethodDelete:
		return audit.ActionDelete
	default:
		return m
	}
}

// auditResourceTypes maps the first /admin/<seg> path segment to the AuditResourceType enum name.
var auditResourceTypes = map[string]string{
	"menu":                        "CUSTOM_MENU_ITEM",
	"organizations":               "ORGANIZATION",
	"project":                     "PROJECT",
	"projects":                    "PROJECT",
	"billing-profile":             "BILLING_PROFILE",
	"billing":                     "BILLING_CONFIGURATION",
	"bill":                        "BILL",
	"price-plan":                  "PRICE_PLAN",
	"price-adjustment-rules":      "PRICE_PLAN_RULE",
	"promotion-codes":             "PROMOTION_CODE",
	"promotional-credits":         "PROMOTIONAL_CREDIT",
	"account-credit":              "ACCOUNT_CREDIT",
	"account-credit-transactions": "ACCOUNT_CREDIT_TRANSACTION",
	"collect-transactions":        "COLLECT_TRANSACTION",
	"credit-card-transaction":     "CREDIT_CARD_TRANSACTION",
	"savings-plans":               "SAVINGS_PLAN",
	"savings-contracts":           "SAVINGS_CONTRACT",
	"admin-roles":                 "ADMIN_ROLE",
	"admin-permissions":           "ADMIN_PERMISSION",
	"bank-transfer":               "BANK_TRANSFER",
	"integrations":                "THIRD_PARTY_INTEGRATION",
	"service":                     "EXTERNAL_SERVICE",
	"external-resource-providers": "EXTERNAL_RESOURCE_PROVIDER",
	"cloud-resource":              "CLOUD_RESOURCE",
	"message-templates":           "MESSAGE_TEMPLATE",
	"pdf-templates":               "PDF_TEMPLATE",
	"hmac-keys":                   "HMAC_KEY",
	"instance-metadata-options":   "INSTANCE_METADATA_OPTION",
	"images":                      "IMAGE_GROUP",
	"flavor-categories":           "FLAVOR_CATEGORY",
	"user":                        "USER",
	"user-management":             "USER",
	"platform-configuration":      "PLATFORM_CONFIGURATION",
}

// pathResourceType maps the first segment after /api/v1/admin/ to a resource type.
func pathResourceType(path string) string {
	i := strings.Index(path, "/admin/")
	if i < 0 {
		return ""
	}
	rest := strings.Trim(path[i+len("/admin/"):], "/")
	seg := rest
	if j := strings.IndexByte(seg, '/'); j >= 0 {
		seg = seg[:j]
	}
	if rt, ok := auditResourceTypes[seg]; ok {
		return rt
	}
	return strings.ToUpper(strings.ReplaceAll(seg, "-", "_"))
}

var idLike = regexp.MustCompile(`^([0-9a-fA-F]{24}|[0-9a-fA-F-]{36})$`)

// pathTailID returns the first id-like (hex id / UUID) segment in the path, else "".
func pathTailID(path string) string {
	for _, seg := range strings.Split(path, "/") {
		if idLike.MatchString(seg) {
			return seg
		}
	}
	return ""
}

// bodyDataID extracts data.id from a CustomHttpResponse body (the created/updated doc), else "".
func bodyDataID(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &env) == nil {
		return env.Data.ID
	}
	return ""
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if c := strings.IndexByte(xff, ','); c >= 0 {
			return strings.TrimSpace(xff[:c])
		}
		return strings.TrimSpace(xff)
	}
	if h, _, ok := strings.Cut(r.RemoteAddr, ":"); ok {
		return h
	}
	return r.RemoteAddr
}

// auditCapture records the response status + a bounded copy of the body (to read data.id).
type auditCapture struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *auditCapture) WriteHeader(s int) {
	c.status = s
	c.ResponseWriter.WriteHeader(s)
}

func (c *auditCapture) Write(b []byte) (int, error) {
	if c.body.Len() < 8192 {
		c.body.Write(b)
	}
	return c.ResponseWriter.Write(b)
}
