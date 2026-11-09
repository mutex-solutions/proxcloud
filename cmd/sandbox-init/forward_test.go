package main

import (
	"bufio"
	"net"
	"testing"
	"time"
)

const tcp4 = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1 1
   1: 00000000:1F41 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 2 1
   2: 0100007F:C33F 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 1
   3: 0100007F:1F41 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 4 1
   4: 0100007F:2328 0100007F:9C40 01 00000000:00000000 00:00000000 00000000     0        0 5 1
`
const tcp6 = `  sl  local_address                         remote_address                        st
   0: 00000000000000000000000001000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000
`

func TestForwardPorts(t *testing.T) {
	lo, other := tcpListeners(tcp4, tcp6)
	got := forwardPorts(lo, other, nil)
	// 8080 (127.0.0.1 only) and 3000 (::1 only, dialled on ::1) are forwarded; 8001 also listens on
	// 0.0.0.0 (no need); 49983 is envd's own; 9000 is an established connection.
	if len(got) != 2 || got[8080] != "127.0.0.1" || got[3000] != "::1" {
		t.Fatalf("forward ports = %v", got)
	}
	// Next poll: our own <podIP>:8080 listener shows up; 8080 must stay forwarded.
	own := "   9: 0A00000A:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 9 1\n"
	lo, other = tcpListeners(tcp4+own, tcp6)
	if got := forwardPorts(lo, other, map[int]bool{8080: true}); got[8080] != "127.0.0.1" {
		t.Fatalf("a held forward must survive its own listener: %v", got)
	}
	// ::ffff:127.0.0.1 counts as IPv4 loopback.
	mapped := "   1: 0000000000000000FFFF00000100007F:1388 00000000000000000000000000000000:0000 0A 00000000:00000000\n"
	lo, other = tcpListeners(tcp6 + mapped)
	if got := forwardPorts(lo, other, nil); got[5000] != "127.0.0.1" {
		t.Fatalf("mapped loopback: %v", got)
	}
}

func TestRelay(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		line, _ := bufio.NewReader(c).ReadString('\n')
		_, _ = c.Write([]byte("echo " + line))
		c.Close()
	}()
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go relay(front, backend.Addr().String())
	c, err := net.DialTimeout("tcp", front.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("hi\n"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	reply, _ := bufio.NewReader(c).ReadString('\n')
	if reply != "echo hi\n" {
		t.Fatalf("reply = %q", reply)
	}
	_ = front.Close() // a closed forwarder stops accepting
	if _, err := net.DialTimeout("tcp", front.Addr().String(), time.Second); err == nil {
		t.Error("closed forwarder still accepts")
	}
}
