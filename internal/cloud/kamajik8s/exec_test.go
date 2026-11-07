package kamajik8s

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/websocket"
)

// fakeExecServer speaks the server side of v5.channel.k8s.io: echoes stdin to stdout, writes
// "warn" to stderr, then a Status on the error channel with the exit code from ?command=exit-N.
func fakeExecServer(t *testing.T, gotAuth *string, gotCmd *[]string) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(websocket.Server{
		Handshake: func(cfg *websocket.Config, r *http.Request) error {
			*gotAuth = r.Header.Get("Authorization")
			*gotCmd = r.URL.Query()["command"]
			cfg.Protocol = []string{"v5.channel.k8s.io"}
			return nil
		},
		Handler: func(ws *websocket.Conn) {
			ws.PayloadType = websocket.BinaryFrame
			var stdin []byte
			for ws.Request().URL.Query().Get("stdin") == "true" {
				var f []byte
				if err := websocket.Message.Receive(ws, &f); err != nil {
					return
				}
				if f[0] == chanStdin {
					stdin = append(stdin, f[1:]...)
				}
				if f[0] == chanClose {
					break
				}
			}
			_ = websocket.Message.Send(ws, append([]byte{chanStdout}, []byte("out:"+string(stdin))...))
			_ = websocket.Message.Send(ws, append([]byte{chanStderr}, []byte("warn")...))
			status := `{"status":"Success"}`
			if c := ws.Request().URL.Query()["command"]; len(c) > 0 && strings.HasPrefix(c[0], "exit-") {
				status = fmt.Sprintf(`{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"%s"}]}}`, strings.TrimPrefix(c[0], "exit-"))
			}
			if c := ws.Request().URL.Query()["command"]; len(c) > 0 && c[0] == "missing" {
				status = `{"status":"Failure","message":"executable file not found in $PATH"}`
			}
			_ = websocket.Message.Send(ws, append([]byte{chanError}, []byte(status)...))
		},
	})
	t.Cleanup(srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	kc := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: %s\n    certificate-authority-data: %s\nusers:\n- name: u\n  user:\n    token: tok\ncontexts:\n- name: c\n  context: {cluster: c, user: u}\ncurrent-context: c\n",
		srv.URL, base64.StdEncoding.EncodeToString(ca))
	c, err := New(kc)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExec(t *testing.T) {
	var auth string
	var cmd []string
	c := fakeExecServer(t, &auth, &cmd)
	ctx := context.Background()

	res, err := c.Exec(ctx, "sb-p1", "sbx-1", "main", []string{"python", "-c", "x"}, []byte("hello"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || len(cmd) != 3 || cmd[0] != "python" {
		t.Errorf("auth=%q cmd=%v", auth, cmd)
	}
	if res.Stdout != "out:hello" || res.Stderr != "warn" || res.ExitCode != 0 || res.Truncated {
		t.Errorf("res = %+v", res)
	}

	res, err = c.Exec(ctx, "sb-p1", "sbx-1", "main", []string{"exit-3"}, []byte{}, 1<<20)
	if err != nil || res.ExitCode != 3 {
		t.Errorf("exit code: %+v %v", res, err)
	}

	res, err = c.Exec(ctx, "sb-p1", "sbx-1", "main", []string{"cat"}, []byte("0123456789"), 6)
	if err != nil || res.Stdout != "out:01" || !res.Truncated {
		t.Errorf("cap: %+v %v", res, err)
	}

	if _, err := c.Exec(ctx, "sb-p1", "sbx-1", "main", []string{"missing"}, nil, 100); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing executable: %v", err)
	}
}
