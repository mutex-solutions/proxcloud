package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
)

func TestDurableSMTPSTARTTLSAuthPlainAndLogin(t *testing.T) {
	for _, mechanism := range []string{"PLAIN", "LOGIN"} {
		t.Run(mechanism, func(t *testing.T) {
			// httptest supplies an ephemeral loopback-only certificate. Only this
			// package-private TLS config trusts it; the public adapter never skips
			// certificate verification and never permits STARTTLS downgrade.
			certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			cert := certServer.TLS.Certificates[0]
			leaf := certServer.Certificate()
			certServer.Close()
			roots := x509.NewCertPool()
			roots.AddCert(leaf)
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			done := make(chan []string, 1)
			go func() {
				lines := []string{}
				defer func() { done <- lines }()
				conn, err := l.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				r := bufio.NewReader(conn)
				fmt.Fprint(conn, "220 tls.invalid\r\n")
				secure := false
				loginStep := 0
				inData := false
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSpace(line)
					lines = append(lines, line)
					if inData {
						if line == "." {
							fmt.Fprint(conn, "250 accepted\r\n")
							inData = false
						}
						continue
					}
					if loginStep > 0 {
						want := "test-user"
						response := "334 password\r\n"
						if loginStep == 2 {
							want = "test-secret"
							response = "235 authenticated\r\n"
						}
						if line != base64.StdEncoding.EncodeToString([]byte(want)) {
							return
						}
						loginStep++
						if loginStep == 3 {
							loginStep = 0
						}
						fmt.Fprint(conn, response)
						continue
					}
					switch {
					case strings.HasPrefix(line, "EHLO"):
						if secure {
							fmt.Fprintf(conn, "250-tls.invalid\r\n250 AUTH %s\r\n", mechanism)
						} else {
							fmt.Fprint(conn, "250-tls.invalid\r\n250 STARTTLS\r\n")
						}
					case line == "STARTTLS":
						fmt.Fprint(conn, "220 upgrade\r\n")
						tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
						if err = tlsConn.Handshake(); err != nil {
							return
						}
						conn = tlsConn
						r = bufio.NewReader(conn)
						secure = true
					case strings.HasPrefix(line, "AUTH PLAIN "):
						if !secure || line != "AUTH PLAIN "+base64.StdEncoding.EncodeToString([]byte("\x00test-user\x00test-secret")) {
							return
						}
						fmt.Fprint(conn, "235 authenticated\r\n")
					case line == "AUTH LOGIN":
						if !secure {
							return
						}
						loginStep = 1
						fmt.Fprint(conn, "334 username\r\n")
					case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
						if !secure {
							return
						}
						fmt.Fprint(conn, "250 ok\r\n")
					case line == "DATA":
						fmt.Fprint(conn, "354 go\r\n")
						inData = true
					case line == "QUIT":
						return
					}
				}
			}()
			host, port, _ := net.SplitHostPort(l.Addr().String())
			a := NewDurableSMTP(func(context.Context) (Config, error) {
				return Config{Host: host, Port: port, Username: "test-user", Password: "test-secret", From: "sender@example.invalid"}, nil
			})
			a.tlsConfig = &tls.Config{RootCAs: roots}
			r := a.Deliver(t.Context(), testEnvelope(t))
			lines := <-done
			if r.Delivery != mailoutbox.RelayAccepted {
				t.Fatal("TLS/auth failed", r)
			}
			ehlos := 0
			starttls := -1
			auth := -1
			for i, line := range lines {
				if strings.HasPrefix(line, "EHLO") {
					ehlos++
				}
				if line == "STARTTLS" {
					starttls = i
				}
				if strings.HasPrefix(line, "AUTH") {
					auth = i
				}
			}
			if ehlos != 2 || starttls < 0 || auth <= starttls {
				t.Fatal("TLS/auth transcript ordering")
			}
		})
	}
}

func TestDurableSMTPCancelDuringTLSHandshake(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	handshake := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 fake.invalid\r\n")
		_, _ = r.ReadString('\n')
		fmt.Fprint(conn, "250-fake.invalid\r\n250 STARTTLS\r\n")
		_, _ = r.ReadString('\n')
		fmt.Fprint(conn, "220 go\r\n")
		buf := make([]byte, 4096)
		if _, err = conn.Read(buf); err == nil {
			close(handshake)
			_, _ = conn.Read(buf)
		}
	}()
	host, port, _ := net.SplitHostPort(l.Addr().String())
	a := NewDurableSMTP(func(context.Context) (Config, error) {
		return Config{Host: host, Port: port, From: "sender@example.invalid"}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	e := testEnvelope(t)
	out := make(chan mailoutbox.Receipt, 1)
	go func() { out <- a.Deliver(ctx, e) }()
	<-handshake
	cancel()
	r := <-out
	<-done
	if r.Delivery != mailoutbox.NotAccepted || r.Class != mailoutbox.ClassTLS {
		t.Fatal("TLS cancellation", r)
	}
}
