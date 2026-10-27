// Package mcp serves the Model Context Protocol endpoint at /mcp (streamable
// HTTP, stateless — safe behind multiple api replicas).
//
// Auth (dual scheme):
//   - OIDC JWT (Keycloak) — the platform Enforce middleware already validates
//     any Bearer JWT on this (public-listed) path and populates the
//     RequestContext; we only map issuer → toolset. Clients realm → client
//     tools, admin realm → admin tools. MCP clients obtain the token via the
//     standard MCP OAuth flow: 401 → RFC 9728 resource metadata → Keycloak
//     (discovery + dynamic client registration + PKCE localhost redirect).
//   - API key — `Authorization: Bearer <pk>.<sk>` (an hmac_keys pair). Grants
//     the admin toolset. The pair is validated against hmac_keys with a
//     constant-time compare.
//
// Tools are declarative rows (see tools_client.go / tools_admin.go): each maps
// to an existing REST endpoint and is executed by in-process dispatch through
// the full router, so org/project policy, DTO shapes and audit behave exactly
// like the public API. JWT principals dispatch with their own bearer; api-key
// principals dispatch with a SigV4 signature we mint from the validated pair.
package mcp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mutex-solutions/proxcloud/pkg/auth"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type Handler struct {
	log         *slog.Logger
	lookup      auth.HmacKeyLookup
	mainIssuer  string // clients realm → client toolset
	adminIssuer string // master realm → admin toolset
	baseURL     string // public api base URL (resource metadata)
	root        http.Handler

	clientH http.Handler // streamable handler serving the client toolset
	adminH  http.Handler // streamable handler serving the admin toolset
}

// credential is what dispatch uses to re-authenticate the in-process request.
type credKey struct{}

type cred struct {
	jwt    string // raw bearer (JWT principals)
	pk, sk string // hmac pair (api-key principals)
}

func New(log *slog.Logger, lookup auth.HmacKeyLookup, mainIssuer, adminIssuer, baseURL string) *Handler {
	h := &Handler{log: log, lookup: lookup, mainIssuer: mainIssuer, adminIssuer: adminIssuer, baseURL: strings.TrimRight(baseURL, "/")}

	mk := func(name string, defs []toolDef) http.Handler {
		srv := sdk.NewServer(&sdk.Implementation{Name: name, Version: "1.0.0"}, nil)
		for _, d := range defs {
			h.register(srv, d)
		}
		return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv },
			&sdk.StreamableHTTPOptions{Stateless: true, Logger: log})
	}
	h.clientH = mk("proxcloud", clientTools)
	h.adminH = mk("proxcloud-admin", adminAllTools())
	return h
}

// adminAllTools is the full admin toolset: the Admin-API rows plus the internal-admin
// config surfaces (pricing, platform/provider config, project ops, billing ops).
func adminAllTools() []toolDef {
	out := []toolDef{}
	for _, set := range [][]toolDef{adminTools, adminPricingTools, adminPlatformTools, adminCatalogTools, adminProjectOpsTools, adminBillingOpsTools} {
		out = append(out, set...)
	}
	return out
}

// SetRoot wires the full app router for in-process dispatch (set after the
// router is built — Handler is itself mounted on that router).
func (h *Handler) SetRoot(root http.Handler) { h.root = root }

func (h *Handler) Routes(r chi.Router) {
	r.Get("/.well-known/oauth-protected-resource", h.resourceMetadata)
	r.Handle("/mcp", h.gate())
	r.Handle("/mcp/*", h.gate())
}

// resourceMetadata is the RFC 9728 document MCP clients use to discover the
// authorization server (Keycloak clients realm) after a 401.
func (h *Handler) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	servers := []string{}
	if h.mainIssuer != "" {
		servers = append(servers, h.mainIssuer)
	}
	if h.adminIssuer != "" && h.adminIssuer != h.mainIssuer {
		servers = append(servers, h.adminIssuer)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resource":                 h.baseURL + "/mcp",
		"authorization_servers":    servers,
		"bearer_methods_supported": []string{"header"},
	})
}

var apiKeyRe = regexp.MustCompile(`^(pk[0-9a-f]{32})\.(sk[0-9a-f]{40})$`)

// gate authenticates the request and routes it to the toolset its principal
// is entitled to.
func (h *Handler) gate() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := httpx.RC(r.Context())
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

		// JWT already validated by the platform Enforce middleware.
		if rc != nil && rc.Sub != "" {
			ctx := context.WithValue(r.Context(), credKey{}, cred{jwt: bearer})
			switch rc.Issuer {
			case h.adminIssuer:
				h.adminH.ServeHTTP(w, r.WithContext(ctx))
			case h.mainIssuer:
				h.clientH.ServeHTTP(w, r.WithContext(ctx))
			default:
				httpForbidden(w)
			}
			return
		}

		// API key: Bearer pk.sk → admin toolset.
		if m := apiKeyRe.FindStringSubmatch(bearer); m != nil && h.lookup != nil {
			pk, sk := m[1], m[2]
			if secret, ok := h.lookup(r.Context(), pk); ok &&
				subtle.ConstantTimeCompare([]byte(secret), []byte(sk)) == 1 {
				ctx := context.WithValue(r.Context(), credKey{}, cred{pk: pk, sk: sk})
				ctx = httpx.WithRC(ctx, &httpx.RequestContext{SigV4KeyID: pk})
				h.adminH.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		// RFC 9728 challenge → MCP clients start the OAuth flow from this.
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer resource_metadata=%q`, h.baseURL+"/.well-known/oauth-protected-resource"))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusUnauthorized)
	})
}

func httpForbidden(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
}

// ---------------------------------------------------------------------------
// Declarative tools → in-process REST dispatch
// ---------------------------------------------------------------------------

type param struct {
	name     string
	typ      string // "string" | "integer" | "boolean" | "object" | "array"
	desc     string
	required bool
	// in: "path" | "query" | "body" | "rawbody". "body" params are wrapped into one JSON
	// object keyed by param name; a "rawbody" param is sent VERBATIM as the whole request
	// body (for endpoints that decode a bare array or a passthrough document). A tool uses
	// either named body params or a single rawbody param, never both.
	in string
}

type toolDef struct {
	name   string
	desc   string
	method string
	path   string // REST path with {param} placeholders
	params []param
}

func (h *Handler) register(srv *sdk.Server, d toolDef) {
	props := map[string]any{}
	var required []string
	for _, p := range d.params {
		props[p.name] = map[string]any{"type": p.typ, "description": p.desc}
		if p.required {
			required = append(required, p.name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	srv.AddTool(&sdk.Tool{Name: d.name, Description: d.desc, InputSchema: schema, Annotations: toolAnnotations(d)},
		func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var args map[string]any
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return errResult(fmt.Sprintf("bad arguments: %v", err)), nil
				}
			}
			status, body, err := h.dispatch(ctx, d, args)
			if err != nil {
				return errResult(err.Error()), nil
			}
			text := strings.TrimSpace(string(body))
			if text == "" {
				text = fmt.Sprintf(`{"status":%d}`, status)
			}
			res := &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}
			if status >= 400 {
				res.IsError = true
				res.Content = []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("HTTP %d: %s", status, text)}}
			}
			return res, nil
		})
}

// toolAnnotations derives the MCP behavioural hints (readOnly / destructive / idempotent) from the
// tool's verb + HTTP method, so an agent can tell a read from a write without calling it. Reads are
// a GET or a list_/get_/search_ tool — note the resource-list tools are POST (/resource?type=…) yet
// pure reads, so the name is the reliable signal, not the method. For writes the MCP spec defaults
// destructiveHint to true, so we state it explicitly: a DELETE or delete_/remove_/reject_ tool is
// destructive; a create/update is additive (destructive=false); a PUT or set_/update_ tool is
// idempotent. (Verified in tools_registry_test.go: no read-named tool mutates, no write-named GET.)
func toolAnnotations(d toolDef) *sdk.ToolAnnotations {
	if d.method == "GET" || hasAnyPrefix(d.name, "list_", "get_", "search_") {
		return &sdk.ToolAnnotations{ReadOnlyHint: true}
	}
	destructive := d.method == "DELETE" || hasAnyPrefix(d.name, "delete_", "remove_", "reject_")
	return &sdk.ToolAnnotations{
		DestructiveHint: &destructive,
		IdempotentHint:  d.method == "PUT" || hasAnyPrefix(d.name, "set_", "update_"),
	}
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// dispatch runs the tool's REST call through the full router in-process, so
// auth, policy, DTOs and audit are identical to the public API.
func (h *Handler) dispatch(ctx context.Context, d toolDef, args map[string]any) (int, []byte, error) {
	if h.root == nil {
		return 0, nil, fmt.Errorf("mcp dispatch not wired")
	}
	c, _ := ctx.Value(credKey{}).(cred)
	if c.jwt == "" && c.pk == "" {
		return 0, nil, fmt.Errorf("no credential in context")
	}

	path := d.path
	q := url.Values{}
	body := map[string]any{}
	var rawBody any
	hasRaw := false
	for _, p := range d.params {
		v, ok := args[p.name]
		if !ok || v == nil {
			if p.required {
				return 0, nil, fmt.Errorf("missing required argument %q", p.name)
			}
			continue
		}
		switch p.in {
		case "path":
			path = strings.ReplaceAll(path, "{"+p.name+"}", url.PathEscape(fmt.Sprintf("%v", v)))
		case "query":
			q.Set(p.name, fmt.Sprintf("%v", v))
		case "body":
			body[p.name] = v
		case "rawbody":
			rawBody, hasRaw = v, true
		}
	}
	if strings.Contains(path, "{") {
		return 0, nil, fmt.Errorf("unresolved path parameter in %s", path)
	}

	var payload []byte
	// http.NoBody (never nil): verifySigV4 and handlers read the body
	// unconditionally, and a hand-built request with a nil reader would give
	// them r.Body == nil.
	var rd io.Reader = http.NoBody
	if hasRaw {
		payload, _ = json.Marshal(rawBody)
		rd = bytes.NewReader(payload)
	} else if len(body) > 0 {
		payload, _ = json.Marshal(body)
		rd = bytes.NewReader(payload)
	}
	u := path
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	// The tool ctx descends from the POST /mcp request and still carries chi's
	// RouteContext (RouteMethod=POST). chi reuses an inherited RouteContext, so
	// without stripping it every dispatched GET/PUT/DELETE would be routed as
	// POST (live-caught: GET list tools 400/405'd). Shadow the key with nil.
	ctx = context.WithValue(ctx, chi.RouteCtxKey, nil)
	req, err := http.NewRequestWithContext(ctx, d.method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Host = "mcp.internal"
	req.RemoteAddr = "127.0.0.1:0"
	if len(payload) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.jwt != "" {
		req.Header.Set("Authorization", "Bearer "+c.jwt)
	} else {
		auth.SignSigV4(req, c.pk, c.sk, payload, time.Now())
	}

	rec := httptest.NewRecorder()
	h.root.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), nil
}

func errResult(msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: msg}}}
}
