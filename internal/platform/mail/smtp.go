package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net/smtp"
	"strings"
	"time"
)

// Config is the SMTP mail-gateway config (env-bound; SendGrid / Azure ACS / any SMTP relay).
type Config struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
}

// Enabled reports whether a usable gateway is configured.
func (c Config) Enabled() bool { return c.Host != "" && c.From != "" }

// SMTPMailer sends over STARTTLS (:587) or implicit TLS (:465), picking AUTH PLAIN or LOGIN per
// what the server advertises (SendGrid → PLAIN, Azure Communication Services → LOGIN only).
type SMTPMailer struct {
	cfg     Config
	timeout time.Duration
}

func NewSMTPMailer(cfg Config) *SMTPMailer { return &SMTPMailer{cfg: cfg, timeout: 30 * time.Second} }

func (s *SMTPMailer) Send(_ context.Context, m Message) error {
	if len(m.To) == 0 {
		return errors.New("mail: no recipients")
	}
	from := m.From
	if from == "" {
		from = s.cfg.From
	}
	c, err := s.dial()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("proxcloud"); err != nil {
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
			return err
		}
	}
	if auth, aerr := authFor(c, s.cfg.Host, s.cfg.Username, s.cfg.Password); aerr != nil {
		return aerr
	} else if auth != nil {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("mail auth: %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, to := range m.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(buildMessage(from, m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// dial opens the SMTP client: implicit TLS for :465, plain (then STARTTLS) otherwise.
func (s *SMTPMailer) dial() (*smtp.Client, error) {
	addr := s.cfg.Host + ":" + s.cfg.Port
	if s.cfg.Port == "465" {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: s.cfg.Host})
		if err != nil {
			return nil, err
		}
		return smtp.NewClient(conn, s.cfg.Host)
	}
	return smtp.Dial(addr)
}

// authFor picks the auth mechanism the server advertises (PLAIN preferred, else LOGIN).
func authFor(c *smtp.Client, host, user, pass string) (smtp.Auth, error) {
	ok, mechs := c.Extension("AUTH")
	if !ok || user == "" {
		return nil, nil
	}
	switch {
	case strings.Contains(mechs, "PLAIN"):
		return smtp.PlainAuth("", user, pass, host), nil
	case strings.Contains(mechs, "LOGIN"):
		return &loginAuth{user: user, pass: pass}, nil
	default:
		return nil, fmt.Errorf("mail: no supported AUTH mechanism (server offers %q)", mechs)
	}
}

// headerSafe strips CR/LF so an address can never smuggle extra headers into the message.
// net/smtp already rejects CR/LF in the envelope (Mail/Rcpt), so this is defense-in-depth
// for the header section built here.
func headerSafe(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// buildMessage renders the RFC 5322 message bytes (headers + text/plain or text/html or
// multipart/alternative when both bodies are present).
func buildMessage(from string, m Message) []byte {
	var b strings.Builder
	b.WriteString("From: " + headerSafe(from) + "\r\n")
	b.WriteString("To: " + headerSafe(strings.Join(m.To, ", ")) + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("UTF-8", m.Subject) + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	switch {
	case m.HTMLBody != "" && m.TextBody != "":
		boundary := "proxcloud_alt_boundary_x9f3"
		b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
		b.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" + m.TextBody + "\r\n")
		b.WriteString("--" + boundary + "\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n" + m.HTMLBody + "\r\n")
		b.WriteString("--" + boundary + "--\r\n")
	case m.HTMLBody != "":
		b.WriteString("Content-Type: text/html; charset=UTF-8\r\n\r\n" + m.HTMLBody + "\r\n")
	default:
		b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n" + m.TextBody + "\r\n")
	}
	return []byte(b.String())
}

// loginAuth implements SMTP AUTH LOGIN (Azure Communication Services rejects AUTH PLAIN).
type loginAuth struct {
	user, pass string
	step       int
}

func (a *loginAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }
func (a *loginAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	switch a.step {
	case 1:
		return []byte(a.user), nil
	case 2:
		return []byte(a.pass), nil
	}
	return nil, errors.New("mail: unexpected LOGIN challenge")
}
