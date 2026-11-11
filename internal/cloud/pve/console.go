package pve

import (
	"context"
	"strconv"
)

// VNCProxy is POST /nodes/{node}/qemu/{vmid}/vncproxy {websocket:1} — mints a
// one-shot VNC ticket + port. The platform's console proxy then dials
// GET /nodes/{node}/qemu/{vmid}/vncwebsocket?port=&vncticket= with the API-token
// header and bridges the browser's noVNC WebSocket to it.
type VNCProxyTicket struct {
	Ticket string `json:"ticket"`
	Port   any    `json:"port"` // PVE returns a string; keep loose
	User   string `json:"user"`
	Cert   string `json:"cert"`
}

// PortString normalizes the port field.
func (t *VNCProxyTicket) PortString() string {
	switch v := t.Port.(type) {
	case string:
		return v
	case float64:
		return strconv.Itoa(int(v))
	default:
		return ""
	}
}

// VNCProxy mints a websocket-capable VNC ticket for a VM.
func (c *Client) VNCProxy(ctx context.Context, node string, vmid int) (*VNCProxyTicket, error) {
	form := formVals{"websocket": "1"}.values()
	var t VNCProxyTicket
	if err := c.post(ctx, qemuPath(node, vmid)+"/vncproxy", form, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// TermProxy mints a serial-terminal ticket (xterm.js path) — same bridge shape.
func (c *Client) TermProxy(ctx context.Context, node string, vmid int) (*VNCProxyTicket, error) {
	var t VNCProxyTicket
	if err := c.post(ctx, qemuPath(node, vmid)+"/termproxy", nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// VNCWebsocketPath is the backend path the console proxy dials (with the
// PVEAPIToken header + the ticket/port as query params).
func VNCWebsocketPath(node string, vmid int, port, ticket string) string {
	return qemuPath(node, vmid) + "/vncwebsocket"
}

// AuthHeader exposes the token header for the console WS proxy dial.
func (c *Client) AuthHeader() string { return c.token }

// BaseURL exposes the api2/json base for the console WS proxy dial.
func (c *Client) BaseURL() string { return c.bases[c.goodIndex()] }
