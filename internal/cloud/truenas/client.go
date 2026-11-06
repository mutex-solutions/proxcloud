// Package truenas is the TrueNAS client: JSON-RPC 2.0 over one authenticated WebSocket (wss://<nas>/api/current,
// auth.login_ex with API_KEY_PLAIN). TrueNAS 26 removes the REST API and revokes a key sent over plain HTTP, so
// neither exists here. Minimum TrueNAS 25.10.
package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// Config is one NAS. Fingerprint pins the self-signed certificate like a PVE provider does.
type Config struct {
	URL         string // https://nas.example or wss://nas.example, with or without /api/current
	Username    string // the account owning the API key
	APIKey      string
	Fingerprint string
	InsecureTLS bool
}

// RPCError is a JSON-RPC error from the middleware.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    *struct {
		Reason string `json:"reason"`
	} `json:"data"`
}

func (e *RPCError) Error() string {
	if e.Data != nil && e.Data.Reason != "" {
		return fmt.Sprintf("truenas: %s", strings.TrimSpace(e.Data.Reason))
	}
	return fmt.Sprintf("truenas: %d %s", e.Code, e.Message)
}

// errTooManyCalls is the middleware refusing a call because the connection has too many in flight.
const errTooManyCalls = -32000

// Client holds one lazily opened, authenticated connection. Calls are serialised on it, and Close ends it.
type Client struct {
	cfg  Config
	http *http.Client

	mu   sync.Mutex
	conn *websocket.Conn
	id   int
}

// New builds a client. It does no I/O.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" || cfg.APIKey == "" || cfg.Username == "" {
		return nil, fmt.Errorf("truenas: url, username and apiKey are required")
	}
	u := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "ws://") {
		return nil, fmt.Errorf("truenas: refusing plain HTTP: TrueNAS revokes an API key sent over it")
	}
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "wss://")
	if !strings.HasSuffix(u, "/api/current") {
		u += "/api/current"
	}
	cfg.URL = "wss://" + u
	return &Client{cfg: cfg, http: &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: pve.TLSConfigFor(pve.Config{Fingerprint: cfg.Fingerprint, InsecureTLS: cfg.InsecureTLS})},
	}}, nil
}

// Close ends the connection; a later Call opens a new one.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close(websocket.StatusNormalClosure, "")
		c.conn = nil
	}
}

// Call runs one method. params is the positional list; out receives the result (nil to ignore).
func (c *Client) Call(ctx context.Context, method string, params []any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		if err := c.connect(ctx); err != nil {
			return err
		}
	}
	var err error
	for range 4 {
		var res json.RawMessage
		if res, err = c.roundTrip(ctx, method, params); err == nil {
			if out == nil || len(res) == 0 {
				return nil
			}
			return json.Unmarshal(res, out)
		}
		var re *RPCError
		if !errors.As(err, &re) || re.Code != errTooManyCalls {
			break
		}
		time.Sleep(time.Second)
	}
	// A broken connection is not reused; an RPC error leaves it healthy.
	var re *RPCError
	if !errors.As(err, &re) && c.conn != nil {
		_ = c.conn.CloseNow()
		c.conn = nil
	}
	return err
}

func (c *Client) connect(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, c.cfg.URL, &websocket.DialOptions{HTTPClient: c.http})
	if err != nil {
		return fmt.Errorf("truenas: connect: %w", err)
	}
	conn.SetReadLimit(16 << 20)
	c.conn = conn
	var login struct {
		ResponseType string `json:"response_type"`
	}
	res, err := c.roundTrip(ctx, "auth.login_ex", []any{map[string]any{
		"mechanism": "API_KEY_PLAIN", "username": c.cfg.Username, "api_key": c.cfg.APIKey,
	}})
	if err == nil {
		err = json.Unmarshal(res, &login)
	}
	if err == nil && login.ResponseType != "SUCCESS" {
		err = fmt.Errorf("truenas: login refused: %s", login.ResponseType)
	}
	if err != nil {
		_ = conn.CloseNow()
		c.conn = nil
	}
	return err
}

// roundTrip writes one request and reads until its response. Server-sent notifications are skipped.
func (c *Client) roundTrip(ctx context.Context, method string, params []any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c.id++
	if params == nil {
		params = []any{}
	}
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, req); err != nil {
		return nil, fmt.Errorf("truenas: %s: %w", method, err)
	}
	for {
		_, raw, err := c.conn.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("truenas: %s: %w", method, err)
		}
		var res struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *RPCError       `json:"error"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("truenas: %s: bad response: %w", method, err)
		}
		if res.ID == nil || *res.ID != c.id {
			continue
		}
		if res.Error != nil {
			return nil, res.Error
		}
		return res.Result, nil
	}
}
