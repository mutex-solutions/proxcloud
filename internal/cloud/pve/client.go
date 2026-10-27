// Package pve is a purpose-built Proxmox VE API client — no SDK. It speaks the
// api2/json surface: form-encoded mutations, a {"data": ...} envelope, and a UPID
// task id returned by every asynchronous mutation (WaitTask polls it to completion).
//
// Auth is a single admin API token (header `Authorization: PVEAPIToken=user@realm!tokenid=secret`);
// tenancy is enforced platform-side (the ceph-s3 precedent): every list is filtered by pool and
// every mutation re-verifies ownership against the platform's cached row, because vnets,
// firewall groups and PCI mappings have no pool scoping in Proxmox at all.
//
// TLS: homelab clusters run self-signed certs; Config.Fingerprint pins the server cert by its
// SHA-256 fingerprint (the same scheme `pvesh`/PVE itself uses between nodes). A CA-signed
// endpoint needs neither.
package pve

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config is everything needed to reach one PVE cluster. Assembled by
// externalservice.PVEConfig() from the externalService doc (config + decrypted secret).
type Config struct {
	// APIURL is the base URL up to and including the host:port, e.g. "https://pve1.lab:8006".
	// "/api2/json" is appended here — operators paste the UI origin.
	APIURL string
	// TokenID is the full token identity "user@realm!tokenid" (config side, not secret).
	TokenID string
	// Secret is the token's secret UUID (secret side, encrypted at rest).
	Secret string
	// Fingerprint pins the server certificate: SHA-256, colon-separated hex, case-insensitive
	// (the format `pvenode cert info` prints). Empty + InsecureTLS=false → system CA pool.
	Fingerprint string
	// InsecureTLS skips all certificate verification. Last resort; Fingerprint is the
	// homelab-grade option.
	InsecureTLS bool
	// TimeoutSeconds bounds each HTTP call (not task waits). Default 30.
	TimeoutSeconds int
}

// Client is a thin transport over one PVE cluster's API.
type Client struct {
	base  string // ".../api2/json"
	token string // "PVEAPIToken=user@realm!tokenid=secret"
	http  *http.Client
}

// APIError is a non-2xx response from PVE. Proxmox reports parameter errors in an
// `errors` map and everything else in the HTTP status line.
type APIError struct {
	Status  int
	Message string
	Errors  map[string]string
}

func (e *APIError) Error() string {
	if len(e.Errors) > 0 {
		parts := make([]string, 0, len(e.Errors))
		for k, v := range e.Errors {
			parts = append(parts, k+": "+strings.TrimSpace(v))
		}
		return fmt.Sprintf("pve: %d %s (%s)", e.Status, e.Message, strings.Join(parts, "; "))
	}
	return fmt.Sprintf("pve: %d %s", e.Status, e.Message)
}

// IsNotFound reports whether err is a PVE 404 (or a "does not exist" 500 — PVE is
// inconsistent: a missing vmid is a 500 "Configuration file ... does not exist",
// and per-parameter misses land in the errors map instead of the status line).
func IsNotFound(err error) bool {
	ae, ok := err.(*APIError)
	if !ok {
		return false
	}
	if ae.Status == http.StatusNotFound {
		return true
	}
	return ae.Status == http.StatusInternalServerError && strings.Contains(ae.Error(), "does not exist")
}

// New builds a Client. It performs no network I/O — Version() is the connectivity probe.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	if base == "" {
		return nil, fmt.Errorf("pve: apiUrl is required")
	}
	if !strings.HasSuffix(base, "/api2/json") {
		base += "/api2/json"
	}
	if cfg.TokenID == "" || cfg.Secret == "" {
		return nil, fmt.Errorf("pve: tokenId and secret are required")
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		base:  base,
		token: AuthHeaderFor(cfg),
		http: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: TLSConfigFor(cfg)},
		},
	}, nil
}

// AuthHeaderFor renders the API-token Authorization header value (also used by
// the platform console proxy when dialing vncwebsocket).
func AuthHeaderFor(cfg Config) string {
	return "PVEAPIToken=" + cfg.TokenID + "=" + cfg.Secret
}

// TLSConfigFor builds the cluster's TLS config: fingerprint pin, insecure, or
// system CA (also used by the console proxy transport).
func TLSConfigFor(cfg Config) *tls.Config {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case cfg.InsecureTLS:
		tlsCfg.InsecureSkipVerify = true
	case cfg.Fingerprint != "":
		want := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(cfg.Fingerprint), ":", ""))
		// Fingerprint pinning: chain verification is disabled and replaced by an exact
		// SHA-256 match on the leaf certificate — self-signed PVE certs never chain.
		tlsCfg.InsecureSkipVerify = true
		tlsCfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return fmt.Errorf("pve: no peer certificate")
			}
			sum := sha256.Sum256(raw[0])
			if hex.EncodeToString(sum[:]) != want {
				return fmt.Errorf("pve: certificate fingerprint mismatch")
			}
			return nil
		}
	}
	return tlsCfg
}

// do performs one API call. form is sent urlencoded for POST/PUT and as the query
// string for GET/DELETE. out (if non-nil) receives the unmarshalled "data" field.
func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	u := c.base + path
	var body io.Reader
	if form != nil {
		if method == http.MethodGet || method == http.MethodDelete {
			u += "?" + form.Encode()
		} else {
			body = strings.NewReader(form.Encode())
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("pve: %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("pve: read %s: %w", path, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		ae := &APIError{Status: res.StatusCode, Message: strings.TrimSpace(strings.TrimPrefix(res.Status, fmt.Sprintf("%d", res.StatusCode)))}
		var env struct {
			Errors map[string]string `json:"errors"`
		}
		if json.Unmarshal(raw, &env) == nil {
			ae.Errors = env.Errors
		}
		return ae
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("pve: decode %s: %w", path, err)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("pve: decode %s data: %w", path, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, form url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, form, out)
}

func (c *Client) post(ctx context.Context, path string, form url.Values, out any) error {
	return c.do(ctx, http.MethodPost, path, form, out)
}

func (c *Client) put(ctx context.Context, path string, form url.Values, out any) error {
	return c.do(ctx, http.MethodPut, path, form, out)
}

func (c *Client) delete(ctx context.Context, path string, form url.Values, out any) error {
	return c.do(ctx, http.MethodDelete, path, form, out)
}

// postTask performs a mutation that returns a UPID.
func (c *Client) postTask(ctx context.Context, path string, form url.Values) (UPID, error) {
	var upid UPID
	if err := c.post(ctx, path, form, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

func (c *Client) deleteTask(ctx context.Context, path string, form url.Values) (UPID, error) {
	var upid UPID
	if err := c.delete(ctx, path, form, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// Version probes connectivity + auth. GET /version.
func (c *Client) Version(ctx context.Context) (map[string]any, error) {
	var v map[string]any
	if err := c.get(ctx, "/version", nil, &v); err != nil {
		return nil, err
	}
	return v, nil
}
