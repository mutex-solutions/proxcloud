package project

import "testing"

// consoleBackend validates the PVE vncwebsocket URL (pve.Service.Console) we reverse-proxy to.
func TestConsoleBackend(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{
			// The VNC and serial (termproxy) consoles both hand back this socket URL.
			name: "pve vncwebsocket url passes through",
			raw:  "https://pve1:8006/api2/json/nodes/pve1/qemu/101/vncwebsocket?port=5900&vncticket=PVEVNC%3Aabc",
			want: "https://pve1:8006/api2/json/nodes/pve1/qemu/101/vncwebsocket?port=5900&vncticket=PVEVNC%3Aabc",
		},
		{
			// ReverseProxy dials http(s) and performs the Upgrade itself, so ws(s) normalises.
			name: "wss normalises to https",
			raw:  "wss://pve1:8006/api2/json/nodes/pve1/qemu/101/vncwebsocket?port=5901&vncticket=t",
			want: "https://pve1:8006/api2/json/nodes/pve1/qemu/101/vncwebsocket?port=5901&vncticket=t",
		},
		{
			name: "ws normalises to http",
			raw:  "ws://pve1:8006/x/vncwebsocket?port=5902&vncticket=t",
			want: "http://pve1:8006/x/vncwebsocket?port=5902&vncticket=t",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := consoleBackend(c.raw)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got.String() != c.want {
				t.Fatalf("got %q want %q", got.String(), c.want)
			}
		})
	}

	bad := []struct{ name, raw string }{
		{"unparseable", "::not a url::"},
		{"empty", ""},
		// Only http(s)/ws(s) origins are proxyable.
		{"non-http scheme", "file://host/vncwebsocket?vncticket=abc"},
		// No vncticket → nothing would authenticate the socket.
		{"no vncticket", "https://pve1:8006/api2/json/nodes/pve1/qemu/101/vncwebsocket?port=5900"},
	}
	for _, c := range bad {
		t.Run("reject/"+c.name, func(t *testing.T) {
			if _, err := consoleBackend(c.raw); err == nil {
				t.Fatalf("expected an error for %q", c.raw)
			}
		})
	}
}
