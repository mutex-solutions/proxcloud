package kamajik8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/websocket"
)

// ExecResult is the outcome of one non-interactive command run in a container.
type ExecResult struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exitCode"`
	Truncated bool   `json:"truncated,omitempty"` // stdout or stderr hit the output cap
}

// Kubernetes remotecommand stream ids (v4/v5.channel.k8s.io): one leading byte per frame.
const (
	chanStdin  = 0
	chanStdout = 1
	chanStderr = 2
	chanError  = 3
	chanClose  = 255 // v5 only: "close this stream" (payload = stream id)
)

// execProtocols: v5 adds the stdin close signal; v4 is the fallback for older API servers
// (stdin is then only usable for commands that do not wait for EOF).
var execProtocols = []string{"v5.channel.k8s.io", "v4.channel.k8s.io"}

// Exec runs cmd in a container over the API server's exec WebSocket and waits for it to exit
// (or ctx to end). Non-interactive: stdin is sent in full then closed, stdout/stderr are each
// capped at maxOutput bytes (the rest is drained and dropped, Truncated set).
//
// Hand-rolled on golang.org/x/net/websocket rather than client-go (not a dependency of this
// module; see kubeconfig.go). Requires `create` on pods/exec.
func (c *Client) Exec(ctx context.Context, ns, pod, container string, cmd []string, stdin []byte, maxOutput int) (*ExecResult, error) {
	if len(cmd) == 0 {
		return nil, errors.New("exec: empty command")
	}
	q := url.Values{"stdout": {"true"}, "stderr": {"true"}}
	if container != "" {
		q.Set("container", container)
	}
	if stdin != nil {
		q.Set("stdin", "true")
	}
	for _, a := range cmd {
		q.Add("command", a)
	}
	base, err := url.Parse(strings.TrimRight(c.rc.server, "/"))
	if err != nil {
		return nil, fmt.Errorf("exec: server url: %w", err)
	}
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/exec", ns, pod)
	u.RawQuery = q.Encode()
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	origin := *base
	cfg, err := websocket.NewConfig(u.String(), origin.String())
	if err != nil {
		return nil, err
	}
	cfg.Protocol = execProtocols
	cfg.TlsConfig = c.rc.tlsCfg
	cfg.Header = http.Header{}
	if t := c.rc.bearer(); t != "" {
		cfg.Header.Set("Authorization", "Bearer "+t)
	}
	ws, err := cfg.DialContext(ctx)
	if err != nil {
		// Also a refused upgrade (e.g. RBAC): the handshake never became a stream.
		return nil, fmt.Errorf("%w: exec: dial: %w", ErrUnreachable, err)
	}
	defer ws.Close()
	// Unblock the read loop when ctx ends (timeout or client gone).
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()

	if stdin != nil {
		if err := websocket.Message.Send(ws, append([]byte{chanStdin}, stdin...)); err != nil {
			return nil, fmt.Errorf("exec: stdin: %w", err)
		}
		if p := ws.Config().Protocol; len(p) > 0 && p[0] == "v5.channel.k8s.io" {
			_ = websocket.Message.Send(ws, []byte{chanClose, chanStdin})
		}
	}

	var out, errOut strings.Builder
	res := &ExecResult{}
	appendCapped := func(b *strings.Builder, p []byte) {
		if room := maxOutput - b.Len(); room < len(p) {
			res.Truncated = true
			if room > 0 {
				b.Write(p[:room])
			}
			return
		}
		b.Write(p)
	}
	var status []byte
	for {
		var frame []byte
		if err := websocket.Message.Receive(ws, &frame); err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("exec: %w", ctx.Err())
			}
			break // server closed the stream: the command is done
		}
		if len(frame) == 0 {
			continue
		}
		switch frame[0] {
		case chanStdout:
			appendCapped(&out, frame[1:])
		case chanStderr:
			appendCapped(&errOut, frame[1:])
		case chanError:
			status = append(status, frame[1:]...)
		}
	}
	res.Stdout, res.Stderr = out.String(), errOut.String()
	code, err := exitCodeFromStatus(status)
	if err != nil {
		return nil, err
	}
	res.ExitCode = code
	return res, nil
}

// exitCodeFromStatus reads the metav1.Status the API server writes on the error channel:
// Success → 0; NonZeroExitCode → details.causes[reason=ExitCode].message; anything else (the
// command could not be started, e.g. not found in the image) is an error.
func exitCodeFromStatus(raw []byte) (int, error) {
	if len(raw) == 0 {
		return 0, nil // v4 servers may close without a status on success
	}
	var st struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
		Details struct {
			Causes []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"causes"`
		} `json:"details"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return 0, fmt.Errorf("exec: decode status: %w", err)
	}
	if st.Status == "Success" {
		return 0, nil
	}
	if st.Reason == "NonZeroExitCode" {
		for _, c := range st.Details.Causes {
			if c.Reason == "ExitCode" {
				if n, err := strconv.Atoi(c.Message); err == nil {
					return n, nil
				}
			}
		}
	}
	return 0, &APIError{Status: http.StatusBadRequest, Message: "exec failed: " + st.Message}
}

// ExecStream runs cmd like Exec but streams stdout into w (no cap: for archives) and keeps at most
// maxStderr bytes of stderr. stdin is not used.
func (c *Client) ExecStream(ctx context.Context, ns, pod, container string, cmd []string, w io.Writer, maxStderr int) (exitCode int, stderr string, err error) {
	if len(cmd) == 0 {
		return 0, "", errors.New("exec: empty command")
	}
	q := url.Values{"stdout": {"true"}, "stderr": {"true"}}
	if container != "" {
		q.Set("container", container)
	}
	for _, a := range cmd {
		q.Add("command", a)
	}
	base, err := url.Parse(strings.TrimRight(c.rc.server, "/"))
	if err != nil {
		return 0, "", fmt.Errorf("exec: server url: %w", err)
	}
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/exec", ns, pod)
	u.RawQuery = q.Encode()
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	cfg, err := websocket.NewConfig(u.String(), base.String())
	if err != nil {
		return 0, "", err
	}
	cfg.Protocol = execProtocols
	cfg.TlsConfig = c.rc.tlsCfg
	cfg.Header = http.Header{}
	if t := c.rc.bearer(); t != "" {
		cfg.Header.Set("Authorization", "Bearer "+t)
	}
	ws, err := cfg.DialContext(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("%w: exec: dial: %w", ErrUnreachable, err)
	}
	defer ws.Close()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()

	var errOut strings.Builder
	var status []byte
	var werr error
	for {
		var frame []byte
		if err := websocket.Message.Receive(ws, &frame); err != nil {
			if ctx.Err() != nil {
				return 0, "", fmt.Errorf("exec: %w", ctx.Err())
			}
			break
		}
		if len(frame) == 0 {
			continue
		}
		switch frame[0] {
		case chanStdout:
			if werr == nil {
				_, werr = w.Write(frame[1:])
			}
		case chanStderr:
			if room := maxStderr - errOut.Len(); room > 0 {
				p := frame[1:]
				if len(p) > room {
					p = p[:room]
				}
				errOut.Write(p)
			}
		case chanError:
			status = append(status, frame[1:]...)
		}
	}
	if werr != nil {
		return 0, errOut.String(), fmt.Errorf("exec: write output: %w", werr)
	}
	code, err := exitCodeFromStatus(status)
	return code, errOut.String(), err
}
