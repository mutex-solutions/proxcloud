package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func TestLegacyLOGINRequiresTLSAndHost(t *testing.T) {
	const host = "relay.example.invalid"
	for _, tc := range []struct {
		name string
		info *smtp.ServerInfo
	}{
		{"nil", nil},
		{"plaintext", &smtp.ServerInfo{Name: host}},
		{"localhost plaintext", &smtp.ServerInfo{Name: "localhost"}},
		{"wrong host", &smtp.ServerInfo{Name: "other.example.invalid", TLS: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &loginAuth{host: host, user: "fixture-user", pass: "fixture-password"}
			if mech, initial, err := a.Start(tc.info); err == nil || mech != "" || len(initial) != 0 {
				t.Fatal("unsafe LOGIN Start accepted")
			}
			if response, err := a.Next(nil, true); err == nil || len(response) != 0 {
				t.Fatal("LOGIN released a response after rejected Start")
			}
		})
	}
	a := &loginAuth{host: host, user: "fixture-user", pass: "fixture-password"}
	if response, err := a.Next(nil, true); err == nil || len(response) != 0 {
		t.Fatal("LOGIN released a response before Start")
	}
	if mech, initial, err := a.Start(&smtp.ServerInfo{Name: host, TLS: true}); err != nil || mech != "LOGIN" || len(initial) != 0 {
		t.Fatal("TLS LOGIN Start rejected")
	}
	for _, want := range []string{"fixture-user", "fixture-password"} {
		response, err := a.Next(nil, true)
		if err != nil || string(response) != want {
			t.Fatal("TLS LOGIN challenge failed")
		}
	}
	if _, err := a.Next(nil, false); err != nil {
		t.Fatal(err)
	}
	if response, err := a.Next(nil, true); err == nil || len(response) != 0 {
		t.Fatal("LOGIN released a response after completion")
	}
}

// All credentials, certificates and message content in this file are disposable
// fixtures. Peers are net.Pipe or isolated loopback only, with finite deadlines.
const legacyTLSHost = "relay.example.invalid"

func legacyTLSFixture(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "SMTP test fixture"},
		DNSNames: []string{legacyTLSHost}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}},
		&tls.Config{RootCAs: roots, ServerName: legacyTLSHost}
}

type legacyPeerResult struct {
	auth, credentials, mail, data int
	mechanism                     string
	err                           error
}

// legacyPeer speaks only the finite SMTP dialogue used by these tests. It never
// relays, persists, logs or sends a message outside this connection.
func legacyPeer(conn net.Conn, implicit, startTLS bool, cfg *tls.Config, mechanisms string) (result legacyPeerResult) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	var wire net.Conn = conn
	if implicit {
		secure := tls.Server(conn, cfg)
		if err := secure.Handshake(); err != nil {
			result.err = err
			return
		}
		wire = secure
	}
	r := bufio.NewReader(wire)
	write := func(s string) error { _, err := io.WriteString(wire, s+"\r\n"); return err }
	read := func() (string, error) {
		line, err := r.ReadString('\n')
		return strings.TrimSpace(line), err
	}
	if result.err = write("220 fixture ESMTP"); result.err != nil {
		return
	}
	for commands := 0; commands < 16; commands++ {
		line, err := read()
		if err != nil {
			if err != io.EOF {
				result.err = err
			}
			return
		}
		switch {
		case strings.HasPrefix(line, "EHLO "):
			reply := "250-fixture\r\n"
			if startTLS {
				reply += "250-STARTTLS\r\n"
			}
			if mechanisms != "" {
				reply += "250-AUTH " + mechanisms + "\r\n"
			}
			result.err = write(reply + "250 OK")
		case line == "STARTTLS":
			if result.err = write("220 upgrade"); result.err != nil {
				return
			}
			secure := tls.Server(conn, cfg)
			if result.err = secure.Handshake(); result.err != nil {
				return
			}
			wire, r, startTLS = secure, bufio.NewReader(secure), false
		case strings.HasPrefix(line, "AUTH "):
			result.auth++
			if line == "AUTH LOGIN" {
				result.mechanism = "LOGIN"
				for _, value := range []string{"fixture-user", "fixture-password"} {
					if result.err = write("334 Zml4dHVyZQ=="); result.err != nil {
						return
					}
					response, err := read()
					if err != nil {
						result.err = err
						return
					}
					if response != base64.StdEncoding.EncodeToString([]byte(value)) {
						result.err = fmt.Errorf("unexpected fixture LOGIN response")
						return
					}
					result.credentials++
				}
			} else if strings.HasPrefix(line, "AUTH PLAIN ") {
				result.mechanism = "PLAIN"
				if strings.TrimPrefix(line, "AUTH PLAIN ") != base64.StdEncoding.EncodeToString([]byte("\x00fixture-user\x00fixture-password")) {
					result.err = fmt.Errorf("unexpected fixture PLAIN response")
					return
				}
				result.credentials = 2
			} else {
				result.err = fmt.Errorf("unexpected fixture AUTH mechanism")
				return
			}
			result.err = write("235 authenticated")
		case strings.HasPrefix(line, "MAIL FROM:"):
			result.mail++
			result.err = write("250 OK")
		case strings.HasPrefix(line, "RCPT TO:"):
			result.err = write("250 OK")
		case line == "DATA":
			result.data++
			if result.err = write("354 data"); result.err != nil {
				return
			}
			_, result.err = io.Copy(io.Discard, textproto.NewReader(r).DotReader())
			if result.err != nil {
				return
			}
			result.err = write("250 accepted by fixture only")
		case line == "QUIT":
			if result.err = write("221 bye"); result.err != nil {
				return
			}
			if _, secure := wire.(*tls.Conn); secure {
				// smtp.Client.Quit closes TLS after receiving 221. Keep reading
				// until its close_notify arrives rather than closing net.Pipe early.
				var extra [1]byte
				n, err := r.Read(extra[:])
				if n != 0 || err != io.EOF {
					result.err = fmt.Errorf("fixture expected TLS close notification")
				}
			}
			return
		default:
			result.err = fmt.Errorf("unexpected fixture SMTP command")
			return
		}
		if result.err != nil {
			return
		}
	}
	result.err = fmt.Errorf("fixture command limit exceeded")
	return
}

func legacyPipeClient(t *testing.T, implicit, startTLS bool, serverTLS, clientTLS *tls.Config, mechanisms string) (*smtp.Client, <-chan legacyPeerResult) {
	t.Helper()
	client, server := net.Pipe()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan legacyPeerResult, 1)
	go func() { done <- legacyPeer(server, implicit, startTLS, serverTLS, mechanisms) }()
	t.Cleanup(func() { client.Close(); server.Close() })
	var conn net.Conn = client
	smtpHost := legacyTLSHost
	if implicit {
		conn = tls.Client(client, clientTLS)
		smtpHost = clientTLS.ServerName
	}
	c, err := smtp.NewClient(conn, smtpHost)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Hello("fixture"); err != nil {
		t.Fatal(err)
	}
	return c, done
}

func legacyPeerDone(t *testing.T, done <-chan legacyPeerResult) legacyPeerResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(4 * time.Second):
		t.Fatal("fixture peer exceeded deadline")
		return legacyPeerResult{}
	}
}

func TestLegacyAuthForRefusesPlaintext(t *testing.T) {
	for _, mechanisms := range []string{"LOGIN", "PLAIN LOGIN", ""} {
		t.Run("advertises_"+mechanisms, func(t *testing.T) {
			c, done := legacyPipeClient(t, false, false, nil, nil, mechanisms)
			auth, err := authFor(c, legacyTLSHost, "fixture-user", "fixture-password")
			if err == nil || auth != nil || err.Error() != "mail: SMTP credentials require verified TLS" {
				t.Fatal("plaintext credentials not refused with fixed safe error")
			}
			c.Close()
			result := legacyPeerDone(t, done)
			if result.err != nil || result.auth != 0 || result.credentials != 0 || result.mail != 0 || result.data != 0 {
				t.Fatal("plaintext peer observed protected SMTP activity")
			}
		})
	}
}

func TestLegacyAuthForVerifiedTLS(t *testing.T) {
	serverTLS, clientTLS := legacyTLSFixture(t)
	for _, implicit := range []bool{false, true} {
		for _, mechanisms := range []string{"LOGIN", "PLAIN LOGIN"} {
			t.Run(fmt.Sprintf("implicit_%t_%s", implicit, mechanisms), func(t *testing.T) {
				c, done := legacyPipeClient(t, implicit, !implicit, serverTLS, clientTLS, mechanisms)
				if !implicit {
					if err := c.StartTLS(clientTLS); err != nil {
						t.Fatal(err)
					}
				}
				state, secure := c.TLSConnectionState()
				if !secure || len(state.VerifiedChains) == 0 || state.ServerName != legacyTLSHost {
					t.Fatal("positive fixture lacks verified configured-host TLS")
				}
				if auth, err := authFor(c, "other.example.invalid", "fixture-user", "fixture-password"); err == nil || auth != nil {
					t.Fatal("authFor accepted a different configured host")
				}
				auth, err := authFor(c, legacyTLSHost, "fixture-user", "fixture-password")
				if err != nil || auth == nil {
					t.Fatal("verified TLS AUTH unavailable")
				}
				if err := c.Auth(auth); err != nil {
					t.Fatal(err)
				}
				if err := c.Quit(); err != nil {
					t.Fatal(err)
				}
				result := legacyPeerDone(t, done)
				want := "LOGIN"
				if strings.Contains(mechanisms, "PLAIN") {
					want = "PLAIN"
				}
				if result.err != nil || result.auth != 1 || result.credentials != 2 || result.mechanism != want || result.mail != 0 || result.data != 0 {
					t.Fatal("verified TLS fixture AUTH or PLAIN preference failed")
				}
			})
		}
	}
}

func TestLegacyVerifiedTLSHostIdentity(t *testing.T) {
	serverTLS, trustedTLS := legacyTLSFixture(t)
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, host := range []string{legacyTLSHost, "RELAY.EXAMPLE.INVALID", legacyTLSHost + ".", "127.0.0.1"} {
			t.Run(fmt.Sprintf("tls_%x_%s", version, host), func(t *testing.T) {
				clientTLS := trustedTLS.Clone()
				clientTLS.ServerName, clientTLS.MinVersion, clientTLS.MaxVersion = host, version, version
				c, done := legacyPipeClient(t, true, false, serverTLS, clientTLS, "LOGIN")
				auth, err := authFor(c, host, "fixture-user", "fixture-password")
				if err != nil || auth == nil {
					t.Fatal("valid verified DNS/IP identity rejected")
				}
				if err := c.Auth(auth); err != nil {
					t.Fatal(err)
				}
				if err := c.Quit(); err != nil {
					t.Fatal(err)
				}
				result := legacyPeerDone(t, done)
				if result.err != nil {
					t.Fatal(result.err)
				}
			})
		}
	}
}

func TestLegacyStartTLSRejectsUntrustedOrWrongHost(t *testing.T) {
	serverTLS, trustedTLS := legacyTLSFixture(t)
	for _, wrongHost := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrong_host_%t", wrongHost), func(t *testing.T) {
			clientTLS := trustedTLS.Clone()
			if wrongHost {
				clientTLS.ServerName = "other.example.invalid"
			} else {
				clientTLS.RootCAs = x509.NewCertPool()
			}
			c, done := legacyPipeClient(t, false, true, serverTLS, clientTLS, "LOGIN")
			if err := c.StartTLS(clientTLS); err == nil {
				t.Fatal("invalid certificate accepted")
			}
			if auth, err := authFor(c, legacyTLSHost, "fixture-user", "fixture-password"); err == nil || auth != nil {
				t.Fatal("failed TLS allowed credentials")
			}
			c.Close()
			result := legacyPeerDone(t, done)
			if result.err == nil || result.auth != 0 || result.credentials != 0 || result.mail != 0 || result.data != 0 {
				t.Fatal("invalid TLS did not fail before protected SMTP activity")
			}
		})
	}
}

func TestLegacySendTLSGateAndNoAuth(t *testing.T) {
	serverTLS, _ := legacyTLSFixture(t)
	for _, tc := range []struct {
		name, user, mechanisms string
		startTLS, wantOK       bool
	}{
		{"stripped LOGIN", "fixture-user", "LOGIN", false, false},
		{"stripped PLAIN", "fixture-user", "PLAIN LOGIN", false, false},
		{"stripped AUTH", "fixture-user", "", false, false},
		{"untrusted TLS", "fixture-user", "LOGIN", true, false},
		{"explicit NoAuth", "", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
			done := make(chan legacyPeerResult, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- legacyPeerResult{err: err}
					return
				}
				done <- legacyPeer(conn, false, tc.startTLS, serverTLS, tc.mechanisms)
			}()
			host, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			mailer := NewSMTPMailer(Config{Host: host, Port: port, Username: tc.user, Password: "fixture-password", From: "fixture@example.invalid"})
			err = mailer.Send(context.Background(), Message{To: []string{"recipient@example.invalid"}, TextBody: "fixture only"})
			if (err == nil) != tc.wantOK {
				t.Fatal("unexpected Send TLS-gate result")
			}
			result := legacyPeerDone(t, done)
			if result.auth != 0 || result.credentials != 0 {
				t.Fatal("Send released credentials without verified TLS")
			}
			if tc.wantOK {
				if result.err != nil || result.mail != 1 || result.data != 1 {
					t.Fatal("NoAuth relay compatibility failed")
				}
			} else if result.mail != 0 || result.data != 0 {
				t.Fatal("Send began envelope before TLS security gate")
			}
		})
	}
}

func TestLegacyNoAuthNeedsNoTLSClient(t *testing.T) {
	// The explicit NoAuth path must return before consulting transport or EHLO.
	if auth, err := authFor(nil, "relay.example.invalid", "", ""); err != nil || auth != nil {
		t.Fatal("NoAuth unexpectedly requires TLS")
	}
}
