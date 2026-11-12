// Package templateupdates discovers newer published VM and sandbox templates on GitHub Packages
// (ghcr.io) and drives the admin "update" actions (docs/template-updates.md).
//
// Every GitHub REST and GHCR registry call is authenticated with the operator's personal access
// token (classic, read:packages) from the GitHub integration. There is no anonymous fallback for
// listing: without a token the surface reports "configure the GitHub integration". The token is
// never logged, returned, stored in a job payload or placed in an error string.
package templateupdates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultAPIBase and DefaultRegistry are the only origins this client talks to by default.
	DefaultAPIBase  = "https://api.github.com"
	DefaultRegistry = "ghcr.io"
	// Owner is the fixed organization that publishes ProxCloud templates. Not an admin input.
	Owner = "mutex-solutions"
	// maxPages bounds a package-version listing (100 per page): newest first, 2000 versions.
	maxPages     = 20
	maxJSONBytes = 4 << 20
)

// ErrNoToken means the GitHub integration is not configured (or has no token).
var ErrNoToken = errors.New("github integration is not configured")

// RateLimitError is a GitHub/GHCR refusal caused by a rate limit (primary or secondary).
type RateLimitError struct {
	Limit, Remaining int
	Reset            time.Time // zero when unknown
	RetryAfter       time.Duration
	Resource         string
}

func (e *RateLimitError) Error() string {
	msg := "github rate limit exceeded"
	if e.Resource != "" {
		msg += " (" + e.Resource + ")"
	}
	if !e.Reset.IsZero() {
		msg += "; resets at " + e.Reset.UTC().Format(time.RFC3339)
	}
	if e.RetryAfter > 0 {
		msg += "; retry after " + e.RetryAfter.String()
	}
	return msg
}

// HTTPError is any other non-2xx answer. Message is a short fixed classification plus GitHub's
// own message field (never echoing request headers).
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("github: %d %s", e.Status, e.Message) }

// RateLimit is the last observed rate-limit window of the REST API (X-RateLimit-*).
type RateLimit struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Used      int       `json:"used"`
	Reset     time.Time `json:"reset"`
	Resource  string    `json:"resource,omitempty"`
}

// Client is a small GitHub REST + GHCR (OCI distribution) reader. Zero value is not usable; use
// NewClient.
type Client struct {
	token    string
	apiBase  string
	registry string // host[:port]; https unless registryScheme says otherwise (tests)
	scheme   string
	// http serves the small API/manifest/token/small-blob reads, each bounded by requestTimeout.
	http *http.Client
	// blob streams large blobs (a VM disk is up to 8 GiB): NO Client.Timeout (that would also bound
	// the body read); bounded by the caller's ctx, the transport's ResponseHeaderTimeout and a
	// stall guard (blobIdle without a single byte aborts the transfer).
	blob           *http.Client
	requestTimeout time.Duration
	blobIdle       time.Duration
	// last is the newest X-RateLimit-* the REST API returned (nil before any call).
	last *RateLimit
	// regTokens caches registry bearer tokens per repository (pull scope).
	regTokens map[string]string
}

// Options overrides the endpoints and transport (tests). Production uses the defaults.
type Options struct {
	APIBase, Registry, RegistryScheme string
	// HTTP supplies the transport (its Timeout is ignored: timeouts are set per client below).
	HTTP *http.Client
	// RequestTimeout bounds one API/manifest/token call (default 30s); BlobIdleTimeout aborts a
	// blob stream that delivers no byte for that long (default 2m).
	RequestTimeout, BlobIdleTimeout time.Duration
}

const (
	defaultRequestTimeout = 30 * time.Second
	defaultBlobIdle       = 2 * time.Minute
)

// NewClient builds a client for token. An empty token is ErrNoToken: callers never get an
// anonymous client.
func NewClient(token string, o Options) (*Client, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrNoToken
	}
	if strings.ContainsAny(token, " \t\r\n") || len(token) > 512 {
		return nil, errors.New("github token is malformed")
	}
	c := &Client{token: token, apiBase: DefaultAPIBase, registry: DefaultRegistry, scheme: "https",
		requestTimeout: o.RequestTimeout, blobIdle: o.BlobIdleTimeout}
	if o.APIBase != "" {
		c.apiBase = strings.TrimRight(o.APIBase, "/")
	}
	if o.Registry != "" {
		c.registry = o.Registry
	}
	if o.RegistryScheme != "" {
		c.scheme = o.RegistryScheme
	}
	if c.requestTimeout <= 0 {
		c.requestTimeout = defaultRequestTimeout
	}
	if c.blobIdle <= 0 {
		c.blobIdle = defaultBlobIdle
	}
	var rt http.RoundTripper
	if o.HTTP != nil && o.HTTP.Transport != nil {
		rt = o.HTTP.Transport
	}
	if t, ok := rt.(*http.Transport); ok || rt == nil {
		if t == nil {
			t = http.DefaultTransport.(*http.Transport)
		}
		t = t.Clone()
		if t.ResponseHeaderTimeout == 0 {
			t.ResponseHeaderTimeout = c.requestTimeout
		}
		rt = t
	}
	// Never forward the Authorization header across a redirect to another host (blob storage):
	// net/http drops it on a cross-domain hop already; this also refuses non-https redirects.
	redirect := func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" && c.scheme == "https" {
			return errors.New("refusing non-https redirect")
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}
	c.http = &http.Client{Transport: rt, Timeout: c.requestTimeout, CheckRedirect: redirect}
	c.blob = &http.Client{Transport: rt, CheckRedirect: redirect}
	return c, nil
}

// String, GoString, Format and LogValue redact the client: formatting it (%v, %+v, %#v, slog)
// never prints the token.
func (c *Client) String() string   { return "templateupdates.Client{token:[REDACTED]}" }
func (c *Client) GoString() string { return c.String() }
func (c *Client) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.String())
}
func (c *Client) LogValue() slog.Value { return slog.StringValue(c.String()) }

// LastRateLimit returns the most recent REST rate-limit window (nil when unknown).
func (c *Client) LastRateLimit() *RateLimit {
	if c.last == nil {
		return nil
	}
	rl := *c.last
	return &rl
}

func parseRateLimit(h http.Header) *RateLimit {
	if h.Get("X-RateLimit-Limit") == "" {
		return nil
	}
	rl := &RateLimit{Resource: h.Get("X-RateLimit-Resource")}
	rl.Limit, _ = strconv.Atoi(h.Get("X-RateLimit-Limit"))
	rl.Remaining, _ = strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	rl.Used, _ = strconv.Atoi(h.Get("X-RateLimit-Used"))
	if s, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && s > 0 {
		rl.Reset = time.Unix(s, 0).UTC()
	}
	return rl
}

// classify turns a non-2xx response into a typed error. GitHub signals a primary rate limit with
// 403/429 + X-RateLimit-Remaining: 0, a secondary one with Retry-After (docs: "Rate limits for the
// REST API"). GHCR answers 429 with TOOMANYREQUESTS.
func classify(res *http.Response, body []byte) error {
	rl := parseRateLimit(res.Header)
	retry := time.Duration(0)
	if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s >= 0 {
		retry = time.Duration(s) * time.Second
	}
	if res.StatusCode == http.StatusTooManyRequests || (res.StatusCode == http.StatusForbidden && (retry > 0 || (rl != nil && rl.Remaining == 0))) {
		e := &RateLimitError{RetryAfter: retry}
		if rl != nil {
			e.Limit, e.Remaining, e.Reset, e.Resource = rl.Limit, rl.Remaining, rl.Reset, rl.Resource
		}
		return e
	}
	msg := http.StatusText(res.StatusCode)
	var env struct {
		Message string `json:"message"`
		Errors  []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &env) == nil {
		switch {
		case env.Message != "":
			msg = env.Message
		case len(env.Errors) > 0:
			msg = env.Errors[0].Code + ": " + env.Errors[0].Message
		}
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	switch res.StatusCode {
	case http.StatusUnauthorized:
		msg = "token rejected (" + msg + ")"
	case http.StatusForbidden:
		msg = "forbidden: the token needs the read:packages scope (classic PAT) (" + msg + ")"
	}
	return &HTTPError{Status: res.StatusCode, Message: msg}
}

func (c *Client) restGet(ctx context.Context, path string, q url.Values, out any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	u := c.apiBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "proxcloud-template-updates")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, scrub(err)
	}
	defer res.Body.Close()
	if rl := parseRateLimit(res.Header); rl != nil {
		c.last = rl
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxJSONBytes+1))
	if err != nil {
		return nil, scrub(err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.Header, classify(res, body)
	}
	if len(body) > maxJSONBytes {
		return nil, errors.New("github: response too large")
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return nil, fmt.Errorf("github: decode %s: %w", path, err)
		}
	}
	return res.Header, nil
}

// scrub drops anything from a transport error that could carry the request URL with a token in
// it. Tokens are only ever sent in headers, but a URL error stays cheap to sanitize.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("github: %s request failed: %w", ue.Op, ue.Err)
	}
	return err
}

// User is the authenticated identity (Validate).
type User struct {
	Login  string    `json:"login"`
	Scopes []string  `json:"scopes,omitempty"`
	Rate   RateLimit `json:"rateLimit"`
}

// Validate makes the cheap authenticated call GET /user and checks the classic token's scopes
// (X-OAuth-Scopes). GitHub Packages accepts only classic tokens with read:packages, so a token
// without that scope is refused here, before it is saved.
func (c *Client) Validate(ctx context.Context) (*User, error) {
	var u User
	h, err := c.restGet(ctx, "/user", nil, &u)
	if err != nil {
		return nil, err
	}
	// Classic tokens report their scopes in X-OAuth-Scopes; fine-grained and app tokens send no
	// such header at all, and GitHub Packages does not accept them.
	if len(h.Values("X-OAuth-Scopes")) == 0 {
		return nil, errors.New("github: this token type cannot read GitHub Packages; use a personal access token (classic) with read:packages")
	}
	for _, s := range strings.Split(h.Get("X-OAuth-Scopes"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			u.Scopes = append(u.Scopes, s)
		}
	}
	if !hasPackagesRead(u.Scopes) {
		return nil, errors.New("github: the token lacks the read:packages scope")
	}
	if c.last != nil {
		u.Rate = *c.last
	}
	return &u, nil
}

func hasPackagesRead(scopes []string) bool {
	for _, s := range scopes {
		if s == "read:packages" || s == "write:packages" || s == "delete:packages" {
			return true
		}
	}
	return false
}

// PackageVersion is one container package version (GitHub REST "Package Version").
type PackageVersion struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"` // the manifest digest, "sha256:<hex>"
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Metadata  struct {
		PackageType string `json:"package_type"`
		Container   struct {
			Tags []string `json:"tags"`
		} `json:"container"`
	} `json:"metadata"`
}

var packageNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

// linkNext extracts the rel="next" URL of a Link header, or "".
func linkNext(h http.Header) string {
	for _, part := range strings.Split(h.Get("Link"), ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		for _, s := range segs[1:] {
			if strings.TrimSpace(s) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(segs[0]), "<>")
			}
		}
	}
	return ""
}

// ListVersions lists the active versions of a container package owned by Owner, following Link
// pagination (100 per page, at most maxPages). GitHub returns newest first.
// GET /orgs/{org}/packages/container/{package_name}/versions
func (c *Client) ListVersions(ctx context.Context, pkg string) ([]PackageVersion, error) {
	if !packageNameRE.MatchString(pkg) {
		return nil, errors.New("github: invalid package name")
	}
	path := "/orgs/" + Owner + "/packages/container/" + url.PathEscape(pkg) + "/versions"
	q := url.Values{"per_page": {"100"}, "state": {"active"}}
	var out []PackageVersion
	for page := 1; page <= maxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		var batch []PackageVersion
		h, err := c.restGet(ctx, path, q, &batch)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		// The Link header only tells whether another page exists; the next request is built from
		// our own path + page number, never from a URL the response supplied.
		if linkNext(h) == "" || len(batch) == 0 {
			return out, nil
		}
	}
	return out, nil
}
