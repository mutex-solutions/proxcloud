package mail

// This bounded adapter is additive: SMTPMailer.Send and all legacy business
// callers keep their historical behavior. Only the dormant mail outbox uses
// typed receipts. No raw server reply/error ever leaves this file.
import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
)

type DurableConfigResolver func(context.Context) (Config, error)
type DurableSMTP struct {
	resolve      DurableConfigResolver
	loopbackTest bool
	timeout      time.Duration
	// Only package tests can trust an ephemeral local TLS certificate.
	tlsConfig *tls.Config
	// Test-only deterministic boundary hook; never set by app constructors.
	beforeEndData func() error
}

func NewDurableSMTP(resolve DurableConfigResolver) *DurableSMTP {
	return &DurableSMTP{resolve: resolve, timeout: 30 * time.Second}
}

// NewLoopbackSMTPForTest is the ONLY plaintext allowance and test-dispatch
// capability. Resolve must still return a literal loopback IP on every send.
// DNS names, non-loopback IPs and production adapter instances cannot bypass TLS.
func NewLoopbackSMTPForTest(resolve DurableConfigResolver) *DurableSMTP {
	t := NewDurableSMTP(resolve)
	t.loopbackTest = true
	return t
}
func (t *DurableSMTP) LoopbackTestOnly() bool { return t != nil && t.loopbackTest }

func smtpReceipt(d mailoutbox.Delivery, s mailoutbox.Stage, c mailoutbox.SafeClass, permanent bool) mailoutbox.Receipt {
	return mailoutbox.Receipt{Delivery: d, Stage: s, Class: c, Permanent: permanent}
}
func unsent(s mailoutbox.Stage, err error) mailoutbox.Receipt {
	c := mailoutbox.ClassNetwork
	permanent := false
	var reply *textproto.Error
	if errors.As(err, &reply) {
		if reply.Code >= 500 && reply.Code < 600 {
			c = mailoutbox.ClassPermanent
			permanent = true
		} else if reply.Code >= 400 && reply.Code < 500 {
			c = mailoutbox.ClassTemporary
		} else {
			c = mailoutbox.ClassConfig
			permanent = true
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		c = mailoutbox.ClassCancelled
	}
	return smtpReceipt(mailoutbox.NotAccepted, s, c, permanent)
}
func configReceipt() mailoutbox.Receipt {
	return smtpReceipt(mailoutbox.NotAccepted, mailoutbox.StageConfig, mailoutbox.ClassConfig, true)
}

// Deliver opens the socket ONLY after BeginDispatch has committed intent. The
// whole call has one bounded deadline and cancellation closes the real socket.
// It sends one MAIL/RCPT pair, dot-stuffed immutable bytes, then a SEPARATE EOD.
// Any I/O failure before attempting EOD proves no message could be accepted.
// Any attempted EOD + missing definitive reply is UNKNOWN, never auto-retry.
func (t *DurableSMTP) Deliver(ctx context.Context, e mailoutbox.Envelope) mailoutbox.Receipt {
	if t == nil || t.resolve == nil {
		return configReceipt()
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	cfg, err := t.resolve(ctx)
	if err != nil || !cfg.Enabled() {
		return configReceipt()
	}
	if cfg.Port == "" {
		cfg.Port = "587"
	}
	port, err := strconv.Atoi(cfg.Port)
	if err != nil || port < 1 || port > 65535 || strings.ContainsAny(cfg.Host, "\r\n\x00") {
		return configReceipt()
	}
	if t.loopbackTest {
		ip := net.ParseIP(cfg.Host)
		if ip == nil || !ip.IsLoopback() || !strings.HasSuffix(strings.ToLower(e.To()), ".invalid") || !strings.HasSuffix(strings.ToLower(e.From()), ".invalid") {
			return configReceipt()
		}
	}
	wire := e.Wire()
	if len(wire) == 0 || len(wire) > mailoutbox.MaxWireBytes || e.From() == "" || e.To() == "" {
		return configReceipt()
	}
	conn, err := (&net.Dialer{Timeout: t.timeout}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, cfg.Port))
	if err != nil {
		return unsent(mailoutbox.StageConnect, err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return unsent(mailoutbox.StageConnect, err)
	}
	rawConn := conn // immutable cancellation target, including through TLS wrappers
	stop := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stop()
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	if t.tlsConfig != nil {
		tlsCfg = t.tlsConfig.Clone()
		tlsCfg.ServerName = cfg.Host
		tlsCfg.MinVersion = tls.VersionTLS12
	}
	secure := false
	if cfg.Port == "465" {
		tlsConn := tls.Client(conn, tlsCfg)
		if err = tlsConn.HandshakeContext(ctx); err != nil {
			return smtpReceipt(mailoutbox.NotAccepted, mailoutbox.StageTLS, mailoutbox.ClassTLS, true)
		}
		conn = tlsConn
		secure = true
	}
	p := textproto.NewConn(conn)
	if _, _, err = p.ReadResponse(220); err != nil {
		return unsent(mailoutbox.StageConnect, err)
	}
	command := func(expect int, format string, args ...any) (string, error) {
		if err := p.PrintfLine(format, args...); err != nil {
			return "", err
		}
		_, msg, err := p.ReadResponse(expect)
		return msg, err
	}
	ehlo, err := command(250, "EHLO proxcloud.invalid")
	if err != nil {
		return unsent(mailoutbox.StageConnect, err)
	}
	if !secure && !t.loopbackTest {
		if !hasSMTPFeature(ehlo, "STARTTLS") {
			return smtpReceipt(mailoutbox.NotAccepted, mailoutbox.StageTLS, mailoutbox.ClassTLS, true)
		}
		if _, err = command(220, "STARTTLS"); err != nil {
			return smtpReceipt(mailoutbox.NotAccepted, mailoutbox.StageTLS, mailoutbox.ClassTLS, true)
		}
		tlsConn := tls.Client(conn, tlsCfg)
		if err = tlsConn.HandshakeContext(ctx); err != nil {
			return smtpReceipt(mailoutbox.NotAccepted, mailoutbox.StageTLS, mailoutbox.ClassTLS, true)
		}
		conn = tlsConn
		p = textproto.NewConn(conn)
		secure = true
		ehlo, err = command(250, "EHLO proxcloud.invalid")
		if err != nil {
			return unsent(mailoutbox.StageTLS, err)
		}
	}
	if cfg.Username != "" {
		mechs := authMechanisms(ehlo)
		switch {
		case mechs["PLAIN"]:
			_, err = command(235, "AUTH PLAIN %s", base64.StdEncoding.EncodeToString([]byte("\x00"+cfg.Username+"\x00"+cfg.Password)))
		case mechs["LOGIN"]:
			_, err = command(334, "AUTH LOGIN")
			if err == nil {
				_, err = command(334, "%s", base64.StdEncoding.EncodeToString([]byte(cfg.Username)))
			}
			if err == nil {
				_, err = command(235, "%s", base64.StdEncoding.EncodeToString([]byte(cfg.Password)))
			}
		default:
			return configReceipt()
		}
		if err != nil {
			return unsent(mailoutbox.StageAuth, err)
		}
	}
	if _, err = command(250, "MAIL FROM:<%s>", e.From()); err != nil {
		return unsent(mailoutbox.StageEnvelope, err)
	}
	if _, err = command(25, "RCPT TO:<%s>", e.To()); err != nil {
		return unsent(mailoutbox.StageEnvelope, err)
	}
	if _, err = command(354, "DATA"); err != nil {
		return unsent(mailoutbox.StageData, err)
	}
	// Bodies are base64 encoded by preparation; dot-stuff defensively anyway.
	data := dotStuff(wire)
	if err = writeAll(conn, data); err != nil {
		return unsent(mailoutbox.StageBody, err)
	}
	// A cancellation observed before EOD is known unsent. Once the terminator
	// write is attempted, even n==0/err cannot prove the relay did not consume it.
	if t.beforeEndData != nil {
		if err := t.beforeEndData(); err != nil {
			return unsent(mailoutbox.StageBody, err)
		}
	}
	if ctx.Err() != nil {
		return unsent(mailoutbox.StageBody, ctx.Err())
	}
	unknown := smtpReceipt(mailoutbox.Indeterminate, mailoutbox.StageEndData, mailoutbox.ClassUncertain, false)
	if err = writeAll(conn, []byte(".\r\n")); err != nil {
		return unknown
	}
	code, _, err := p.ReadResponse(250)
	if err != nil {
		var refusal *textproto.Error
		if errors.As(err, &refusal) && refusal.Code >= 400 && refusal.Code < 600 {
			return unsent(mailoutbox.StageReply, err)
		}
		return unknown
	}
	if code != 250 {
		return unknown
	}
	// DATA 250 is the acceptance boundary; QUIT errors never reverse it. Do not
	// wait for a QUIT reply: the socket is bounded/closed regardless of its fate.
	_ = p.PrintfLine("QUIT")
	return smtpReceipt(mailoutbox.RelayAccepted, mailoutbox.StageReply, mailoutbox.ClassOK, false)
}
func hasSMTPFeature(ehlo, feature string) bool {
	for _, line := range strings.Split(ehlo, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.EqualFold(fields[0], feature) {
			return true
		}
	}
	return false
}
func authMechanisms(ehlo string) map[string]bool {
	m := map[string]bool{}
	for _, line := range strings.Split(ehlo, "\n") {
		f := strings.Fields(strings.ToUpper(line))
		if len(f) > 0 && f[0] == "AUTH" {
			for _, v := range f[1:] {
				m[v] = true
			}
		}
	}
	return m
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func dotStuff(wire []byte) []byte {
	var b strings.Builder
	b.Grow(len(wire) + 4)
	lineStart := true
	for _, c := range wire {
		if lineStart && c == '.' {
			b.WriteByte('.')
		}
		b.WriteByte(c)
		lineStart = c == '\n'
	}
	if !strings.HasSuffix(b.String(), "\r\n") {
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}
