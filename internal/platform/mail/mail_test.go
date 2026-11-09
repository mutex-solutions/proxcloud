package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	netmail "net/mail"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildMessage(t *testing.T) {
	// text-only
	b := string(buildMessage("a@x.io", Message{To: []string{"b@y.io"}, Subject: "Hi", TextBody: "hello"}))
	if !strings.Contains(b, "From: a@x.io") || !strings.Contains(b, "To: b@y.io") || !strings.Contains(b, "Subject: Hi") {
		t.Fatalf("missing headers:\n%s", b)
	}
	if !strings.Contains(b, "Content-Type: text/plain; charset=UTF-8") || !strings.Contains(b, "hello") {
		t.Fatalf("text body wrong:\n%s", b)
	}
	// html+text → multipart/alternative
	mb := string(buildMessage("a@x.io", Message{To: []string{"b@y.io"}, Subject: "S", TextBody: "t", HTMLBody: "<b>h</b>"}))
	if !strings.Contains(mb, "multipart/alternative") || !strings.Contains(mb, "text/html") || !strings.Contains(mb, "<b>h</b>") {
		t.Fatalf("multipart wrong:\n%s", mb)
	}
}

func TestFromEnvNoopWhenUnconfigured(t *testing.T) {
	t.Setenv("PROXCLOUD_MAIL_SMTP_HOST", "")
	t.Setenv("PROXCLOUD_MAIL_FROM", "")
	if _, ok := FromEnv().(NoopMailer); !ok {
		t.Fatal("expected NoopMailer when unconfigured")
	}
}

// TestSMTPSendSmoke is a LIVE send vs a real SMTP relay, gated on PROXCLOUD_MAIL_SMTP_HOST so normal
// `go test ./...` never sends. Run:
//
//	PROXCLOUD_MAIL_SMTP_HOST=smtp.sendgrid.net PROXCLOUD_MAIL_SMTP_USERNAME=apikey \
//	PROXCLOUD_MAIL_SMTP_PASSWORD=SG.xxx PROXCLOUD_MAIL_FROM=ops@example.com \
//	PROXCLOUD_MAIL_TEST_TO=ops@example.com go test ./internal/platform/mail -run TestSMTPSendSmoke -v
func TestSMTPSendSmoke(t *testing.T) {
	if os.Getenv("PROXCLOUD_MAIL_SMTP_HOST") == "" {
		t.Skip("PROXCLOUD_MAIL_SMTP_HOST not set — skipping live mail smoke")
	}
	to := os.Getenv("PROXCLOUD_MAIL_TEST_TO")
	if to == "" {
		to = os.Getenv("PROXCLOUD_MAIL_FROM")
	}
	m := FromEnv()
	if _, ok := m.(NoopMailer); ok {
		t.Fatal("mail env incomplete (need HOST + FROM)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := m.Send(ctx, Message{
		To:       []string{to},
		Subject:  "ProxCloud Go — mail package live smoke",
		TextBody: "Sent by internal/platform/mail SMTPMailer at " + time.Now().UTC().Format(time.RFC1123Z) + ".",
		HTMLBody: "<p>Sent by <code>internal/platform/mail</code> SMTPMailer.</p>",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	t.Logf("sent live test mail to %s", to)
}

// TestBuildMessageAttachments: an attachment wraps the body in multipart/mixed — the HTML part
// first, then the file base64-encoded with its filename — and a stock MIME parser gets the bytes
// back byte-for-byte (what any SMTP relay, SendGrid included, forwards as-is).
func TestBuildMessageAttachments(t *testing.T) {
	pdf := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte{0xff, 0x00, 'z'}, 200)...)
	raw := buildMessage("a@x.io", Message{To: []string{"b@y.io"}, Subject: "Inv", HTMLBody: "<b>h</b>",
		Attachments: []Attachment{{Filename: "INV-2026-000001.pdf", ContentType: "application/pdf", Data: pdf}}})
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/mixed" {
		t.Fatalf("content-type = %q %v (%v)", mt, params, err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	body, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if ct := body.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("first part = %q, want the html body", ct)
	}
	if b, _ := io.ReadAll(body); !strings.Contains(string(b), "<b>h</b>") {
		t.Fatalf("html body lost: %q", b)
	}
	att, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if att.FileName() != "INV-2026-000001.pdf" || !strings.HasPrefix(att.Header.Get("Content-Type"), "application/pdf") || att.Header.Get("Content-Transfer-Encoding") != "base64" {
		t.Fatalf("attachment headers: %v", att.Header)
	}
	enc, _ := io.ReadAll(att)
	got, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.ReplaceAll(string(enc), "\r\n", ""), "\n", ""))
	if err != nil || !bytes.Equal(got, pdf) {
		t.Fatalf("attachment bytes differ (err=%v, %d vs %d)", err, len(got), len(pdf))
	}
	for _, line := range strings.Split(string(enc), "\r\n") {
		if len(line) > 76 {
			t.Fatalf("base64 line longer than 76 chars: %d", len(line))
		}
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatalf("want exactly two parts, got extra: %v", err)
	}
	// No attachments → the plain single-part shape is unchanged.
	if plain := string(buildMessage("a@x.io", Message{To: []string{"b@y.io"}, Subject: "S", HTMLBody: "<b>h</b>"})); strings.Contains(plain, "multipart/mixed") {
		t.Fatal("no attachments must not produce multipart/mixed")
	}
}

type captureMailer struct{ last Message }

func (c *captureMailer) Send(_ context.Context, m Message) error { c.last = m; return nil }

// TestSendTemplateWithAttachments: the rendered template goes out with the files attached;
// SendTemplate stays attachment-free.
func TestSendTemplateWithAttachments(t *testing.T) {
	cm := &captureMailer{}
	svc := NewService(
		func(_ context.Context, key string, vars map[string]any) (string, string, bool, error) {
			return "Invoice " + vars["n"].(string), "<p>" + key + "</p>", true, nil
		},
		func(context.Context) (Mailer, string) { return cm, "billing@x.io" }, "Acme", nil)
	att := []Attachment{{Filename: "inv.pdf", ContentType: "application/pdf", Data: []byte("%PDF")}}
	if err := svc.SendTemplateWithAttachments(context.Background(), "bill_is_generated", []string{"c@y.io"}, map[string]any{"n": "1"}, att); err != nil {
		t.Fatal(err)
	}
	if cm.last.Subject != "Invoice 1" || len(cm.last.Attachments) != 1 || cm.last.Attachments[0].Filename != "inv.pdf" || !strings.Contains(cm.last.HTMLBody, "bill_is_generated") {
		t.Fatalf("message = %+v", cm.last)
	}
	if err := svc.SendTemplate(context.Background(), "bill_is_paid", []string{"c@y.io"}, map[string]any{"n": "2"}); err != nil {
		t.Fatal(err)
	}
	if len(cm.last.Attachments) != 0 || cm.last.Subject != "Invoice 2" {
		t.Fatalf("plain send = %+v", cm.last)
	}
}
