// Package billingclient talks to the standalone billing service.
//
// Billing is being extracted from this monolith: the RATING half (price plans, bills, credits,
// suspension) now lives in the billing service, while RESOLUTION — reading the cloud-resource cache
// and turning it into billable units — stays here, because it is OpenStack-specific. This package
// is the seam between the two.
//
// The wire contract is pkg/billingapi. Note that proxcloud and the billing service each keep their
// own copy of that schema rather than sharing a Go module: the billing service's module path is not
// go-gettable, the repos live in different GitHub organisations, and a JSON contract between two
// services is a better boundary than a shared compile-time dependency anyway. The cost is that the
// two copies can drift, so it is pinned by a golden fixture — see testdata/charge_request.golden.json
// and the matching test on the billing side.
package billingclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// Client is a billing-service HTTP client. The zero value is not usable; use New.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a client for the billing service at baseURL (e.g. an in-cluster
// http://cloud-billing.billing.svc.cluster.local:8080). A trailing slash is tolerated.
//
// Returns nil when baseURL is empty, so a not-configured client is a usable "disabled" value rather
// than a nil-pointer hazard: every method is nil-safe and reports ErrNotConfigured.
func New(baseURL, token string, timeout time.Duration) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{baseURL: baseURL, token: strings.TrimSpace(token), http: &http.Client{Timeout: timeout}}
}

// Configured reports whether the client can reach a billing service. Nil-safe.
func (c *Client) Configured() bool { return c != nil && c.baseURL != "" }

// ErrNotConfigured is returned by every call on an unconfigured client.
var ErrNotConfigured = fmt.Errorf("billingclient: no billing service URL configured")

// ServiceResources is one external service's resolved billable units. Mirrors the billing
// service's cloudsvc.ServiceResources.
type ServiceResources struct {
	ServiceID string                        `json:"serviceId"`
	Resources []*billingapi.BillingResource `json:"resources"`
}

// ChargeRequest is one profile's unit of work. It is deliberately self-contained — the resource
// type catalog travels with it — so the billing service never has to call back here to rate.
//
// Mirrors the billing service's cloudsvc.ChargeRequest; the golden fixture pins the JSON shape.
type ChargeRequest struct {
	ProfileID      string                            `json:"profileId"`
	TimeUnit       string                            `json:"timeUnit"`
	CycleStart     time.Time                         `json:"cycleStart"`
	CycleEnd       time.Time                         `json:"cycleEnd"`
	CycleTimestamp time.Time                         `json:"cycleTimestamp"`
	Services       []ServiceResources                `json:"services"`
	Catalog        []*billingapi.BillingResourceType `json:"catalog,omitempty"`
	TimeUnitLimits map[string]int                    `json:"timeUnitLimits,omitempty"`
}

// ChargeResponse reports what the charge did. Charged=false is a successful no-op (billing not
// configured, or the profile is no longer ACTIVE), not a failure.
type ChargeResponse struct {
	ProfileID string `json:"profileId"`
	Charged   bool   `json:"charged"`
	Reason    string `json:"reason,omitempty"`
	Services  int    `json:"servicesCharged"`
}

// envelope is the billing service's response wrapper.
type envelope[T any] struct {
	Data   T `json:"data"`
	Errors *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// ActiveProfileIDs returns the ACTIVE billing-profile ids — the work-list the charge cron iterates.
func (c *Client) ActiveProfileIDs(ctx context.Context) ([]string, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	var env envelope[struct {
		ProfileIDs []string `json:"profileIds"`
	}]
	if err := c.do(ctx, http.MethodGet, "/cloud/internal/v1/profiles/active", nil, &env); err != nil {
		return nil, err
	}
	return env.Data.ProfileIDs, nil
}

// Charge rates one profile's resolved resources onto its bill.
func (c *Client) Charge(ctx context.Context, req ChargeRequest) (*ChargeResponse, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	var env envelope[ChargeResponse]
	if err := c.do(ctx, http.MethodPost, "/cloud/internal/v1/charge", req, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// Health reports whether the billing service and its database are reachable.
func (c *Client) Health(ctx context.Context) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	var env envelope[map[string]any]
	return c.do(ctx, http.MethodGet, "/cloud/internal/v1/health", nil, &env)
}

// do performs one request. Responses are decoded with the contract's UseNumber decoder for the same
// reason the server encodes them carefully: any number that round-trips through this client may end
// up priced or persisted, and float64 would silently round integers above 2^53.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("billingclient: encode %s: %w", path, err)
		}
		rdr = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("billingclient: build %s: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// The billing service refuses to mount its internal API without a token, so a missing one here
	// surfaces as a 401 rather than silently working.
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("billingclient: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Cap the read so a misrouted response (an HTML error page from a gateway, say) cannot
	// exhaust memory on the charge path.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("billingclient: read %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("billingclient: %s %s: status %d: %s", method, path, resp.StatusCode, snippet(raw))
	}
	if out == nil {
		return nil
	}
	if err := billingapi.NewDecoder(bytes.NewReader(raw)).Decode(out); err != nil {
		return fmt.Errorf("billingclient: decode %s: %w (body: %s)", path, err, snippet(raw))
	}
	return nil
}

// snippet trims a response body for error messages.
func snippet(b []byte) string {
	const max = 512
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
