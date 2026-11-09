package main

// forward.go — makes services that listen only on localhost reachable from outside the pod.
// sandbox-proxy dials <pod IP>:<port>; E2B's envd forwards localhost ports for this, but to the
// Firecracker gateway address, which a pod does not have. So sandbox-init does it: every second
// it reads the kernel's TCP listener table and, for each port listened on only at 127.0.0.1/::1
// (never envd's), listens on the pod's own address and relays to localhost. A forwarder stops
// when its localhost listener goes away.

import (
	"bufio"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const envdPort = 49983

// tcpListeners parses /proc/net/tcp{,6} content: for each LISTEN socket, its port and whether it
// is bound to loopback (value: the loopback address to dial, 127.0.0.1 unless only ::1 listens)
// or to something else (wildcard, pod IP).
func tcpListeners(tables ...string) (loopback map[int]string, other map[int]bool) {
	loopback, other = map[int]string{}, map[int]bool{}
	for _, t := range tables {
		sc := bufio.NewScanner(strings.NewReader(t))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 4 || f[3] != "0A" { // 0A = TCP_LISTEN; also skips the header line
				continue
			}
			host, portHex, ok := strings.Cut(f[1], ":")
			if !ok {
				continue
			}
			port, err := strconv.ParseInt(portHex, 16, 32)
			if err != nil {
				continue
			}
			switch host {
			case "0100007F", "0000000000000000FFFF00000100007F": // 127.0.0.1, ::ffff:127.0.0.1
				loopback[int(port)] = "127.0.0.1"
			case "00000000000000000000000001000000": // ::1
				if loopback[int(port)] == "" {
					loopback[int(port)] = "::1"
				}
			default:
				other[int(port)] = true
			}
		}
	}
	return loopback, other
}

// forwardPorts are the ports to relay (port -> loopback address to dial): loopback-only
// listeners, not envd's. A port we already relay stays: its non-loopback entry is our own
// listener (nothing else can bind that port on the pod IP or the wildcard while we hold it).
func forwardPorts(loopback map[int]string, other map[int]bool, active map[int]bool) map[int]string {
	out := map[int]string{}
	for p, addr := range loopback {
		if p != envdPort && (!other[p] || active[p]) {
			out[p] = addr
		}
	}
	return out
}

func podIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}

func forwardLocalhost(every time.Duration) {
	ip := podIPv4()
	if ip == "" {
		return
	}
	// ponytail: a service that later re-binds the same port on 0.0.0.0 can hit EADDRINUSE for up
	// to one poll while we still hold <podIP>:port (E2B avoids it with a dedicated address).
	var mu sync.Mutex
	active := map[int]net.Listener{}
	for {
		var tables []string
		for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
			if b, err := os.ReadFile(f); err == nil {
				tables = append(tables, string(b))
			}
		}
		loopback, other := tcpListeners(tables...)
		mu.Lock()
		held := map[int]bool{}
		for p := range active {
			held[p] = true
		}
		want := forwardPorts(loopback, other, held)
		for p, l := range active {
			if _, ok := want[p]; !ok {
				_ = l.Close()
				delete(active, p)
			}
		}
		for p, addr := range want {
			if _, ok := active[p]; ok {
				continue
			}
			l, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(p)))
			if err != nil {
				continue // taken meanwhile; retried next round
			}
			active[p] = l
			go relay(l, net.JoinHostPort(addr, strconv.Itoa(p)))
		}
		mu.Unlock()
		time.Sleep(every)
	}
}

func relay(l net.Listener, target string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return // closed: the localhost listener went away
		}
		go func() {
			defer c.Close()
			t, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				return
			}
			defer t.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(t, c); closeWrite(t); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, t); closeWrite(c); done <- struct{}{} }()
			<-done
			// One side finished: give the other a while to answer a half-close, then drop both
			// (a peer that ignores half-close would otherwise pin the goroutines and fds).
			select {
			case <-done:
			case <-time.After(time.Minute):
			}
		}()
	}
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}
