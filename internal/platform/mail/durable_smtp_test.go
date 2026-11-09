package mail

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
)

// All peers are literal loopback listeners; all recipients use .invalid. Server
// transcript/channel hooks, not elapsed-time guesses, determine fault boundaries.
type smtpScript struct {
	mu        sync.Mutex
	lines     []string
	eod       bool
	done      chan struct{}
	conn      net.Conn
	mode      string
	beforeEOD func()
}

func (s *smtpScript) add(v string) { s.mu.Lock(); s.lines = append(s.lines, v); s.mu.Unlock() }
func (s *smtpScript) snapshot() ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...), s.eod
}
func newSMTPPeer(t *testing.T, mode string) (Config, *smtpScript) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpScript{mode: mode, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		s.conn = c
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(c)
		fmt.Fprint(c, "220 loopback.invalid ESMTP\r\n")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			s.add(line)
			if inData {
				if line == "." {
					s.mu.Lock()
					s.eod = true
					s.mu.Unlock()
					if mode == "lose250" {
						return
					}
					if mode == "data450" {
						fmt.Fprint(c, "450 retry\r\n")
						return
					}
					fmt.Fprint(c, "250 accepted\r\n")
					inData = false
					continue
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				fmt.Fprint(c, "250-loopback.invalid\r\n250 AUTH PLAIN LOGIN\r\n")
			case strings.HasPrefix(line, "AUTH PLAIN"):
				fmt.Fprint(c, "235 authenticated\r\n")
			case strings.HasPrefix(line, "MAIL FROM"):
				fmt.Fprint(c, "250 sender\r\n")
			case strings.HasPrefix(line, "RCPT TO"):
				if mode == "rcpt450" {
					fmt.Fprint(c, "450 refused private-recipient\r\n")
				} else if mode == "rcpt550" {
					fmt.Fprint(c, "550 refused private-recipient\r\n")
				} else if mode == "forward251" {
					fmt.Fprint(c, "251 forwarded\r\n")
				} else if mode == "accept252" {
					fmt.Fprint(c, "252 accepted\r\n")
				} else {
					fmt.Fprint(c, "250 recipient\r\n")
				}
			case line == "DATA":
				if mode == "beforeDataEOF" {
					return
				}
				fmt.Fprint(c, "354 send\r\n")
				inData = true
			case line == "QUIT":
				fmt.Fprint(c, "500 quit failed\r\n")
				return
			default:
				fmt.Fprint(c, "500 bad command\r\n")
			}
		}
	}()
	t.Cleanup(func() { _ = l.Close(); <-s.done })
	host, port, _ := net.SplitHostPort(l.Addr().String())
	return Config{Host: host, Port: port, From: "sender@example.invalid"}, s
}
func testEnvelope(t *testing.T) mailoutbox.Envelope {
	t.Helper()
	es, err := mailoutbox.PrepareReminder(mailoutbox.Reminder{ContractID: "contract", ExpiryGeneration: "v1", Date: time.Date(2026, 11, 9, 1, 2, 3, 0, time.UTC), EndDate: time.Date(2026, 11, 10, 1, 2, 3, 0, time.UTC), Recipients: []mailoutbox.Recipient{{Key: "opaque", Address: "recipient@example.invalid"}}, From: "sender@example.invalid", Subject: "private subject", TextBody: "private body"})
	if err != nil {
		t.Fatal(err)
	}
	return es[0]
}
func TestDurableSMTPTranscriptOutcomes(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		want      mailoutbox.Delivery
		permanent bool
		eod       bool
	}{
		{"quitFail", mailoutbox.RelayAccepted, false, true},
		{"lose250", mailoutbox.Indeterminate, false, true},
		{"rcpt450", mailoutbox.NotAccepted, false, false},
		{"rcpt550", mailoutbox.NotAccepted, true, false},
		{"beforeDataEOF", mailoutbox.NotAccepted, false, false},
		{"data450", mailoutbox.NotAccepted, false, true},
		{"forward251", mailoutbox.RelayAccepted, false, true},
		{"accept252", mailoutbox.RelayAccepted, false, true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			cfg, s := newSMTPPeer(t, tc.mode)
			adapter := NewLoopbackSMTPForTest(func(context.Context) (Config, error) { return cfg, nil })
			r := adapter.Deliver(context.Background(), testEnvelope(t))
			<-s.done
			if r.Delivery != tc.want || r.Permanent != tc.permanent {
				t.Fatalf("safe receipt=%+v", r)
			}
			lines, eod := s.snapshot()
			if eod != tc.eod {
				t.Fatalf("EOD=%t want%t", eod, tc.eod)
			}
			rcpts := 0
			for _, v := range lines {
				if strings.HasPrefix(v, "RCPT TO:") {
					rcpts++
				}
			}
			if rcpts != 1 {
				t.Fatal("multiple RCPTs")
			}
			if strings.Contains(fmt.Sprint(r), "private") {
				t.Fatal("receipt leaked reply")
			}
		})
	}
}
func TestDurableSMTPEOFBeforeTerminatorIsKnownUnsent(t *testing.T) {
	cfg, s := newSMTPPeer(t, "beforeEOD")
	a := NewLoopbackSMTPForTest(func(context.Context) (Config, error) { return cfg, nil })
	a.beforeEndData = func() error { return io.EOF }
	r := a.Deliver(t.Context(), testEnvelope(t))
	<-s.done
	lines, eod := s.snapshot()
	if r.Delivery != mailoutbox.NotAccepted || eod {
		t.Fatal("pre-EOD failure not proven unsent", r)
	}
	if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "Content-Transfer-Encoding: base64") {
		t.Fatal("body not consumed before EOF hook")
	}
}

func TestDurableSMTPRequiresTLSAndExplicitLoopback(t *testing.T) {
	cfg, s := newSMTPPeer(t, "tlsMissing")
	a := NewDurableSMTP(func(context.Context) (Config, error) { return cfg, nil })
	r := a.Deliver(context.Background(), testEnvelope(t))
	<-s.done
	if r.Class != mailoutbox.ClassTLS || r.Delivery != mailoutbox.NotAccepted {
		t.Fatalf("TLS downgrade=%+v", r)
	}
	lines, eod := s.snapshot()
	if eod {
		t.Fatal("TLS-less delivery")
	}
	for _, v := range lines {
		if strings.HasPrefix(v, "MAIL") {
			t.Fatal("MAIL before TLS")
		}
	}
	a = NewLoopbackSMTPForTest(func(context.Context) (Config, error) {
		return Config{Host: "localhost", Port: "25", From: "x@example.invalid"}, nil
	})
	if r = a.Deliver(context.Background(), testEnvelope(t)); r.Class != mailoutbox.ClassConfig {
		t.Fatal("test DNS bypass")
	}
	if NewDurableSMTP(nil).LoopbackTestOnly() {
		t.Fatal("production test capability")
	}
}
func TestDurableSMTPCancellationClosesSocketBeforeDATA(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	connected := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		close(connected)
		var b [1]byte
		_, _ = c.Read(b[:])
		close(closed)
	}()
	h, p, _ := net.SplitHostPort(l.Addr().String())
	a := NewLoopbackSMTPForTest(func(context.Context) (Config, error) { return Config{Host: h, Port: p, From: "x@example.invalid"}, nil })
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan mailoutbox.Receipt, 1)
	e := testEnvelope(t)
	go func() { out <- a.Deliver(ctx, e) }()
	<-connected
	cancel()
	r := <-out
	<-closed
	if r.Delivery != mailoutbox.NotAccepted {
		t.Fatalf("pre-DATA cancellation=%+v", r)
	}
}
