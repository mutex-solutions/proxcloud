package mail

import (
	"context"
	"strconv"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// integration.go builds a Mailer from the admin-configured "SMTP" thirdPartyIntegration (Admin →
// Integrations → Mail). This is the DB mail gateway an operator sets up in the UI; it takes
// precedence over the PROXCLOUD_MAIL_* env gateway (which stays as the bootstrap/fallback).

// smtpIntegration decodes the "SMTP" thirdPartyIntegration doc. Field keys match the admin
// integrations page SMTP schema (web/admin IntegrationsPage): config.{domain,port,username,
// fromName,fromEmail,noAuth} + secret.password. The password is stored ENCRYPTED at rest (admin
// write path → esSvc.EncryptSecret) and decrypted here with the passed encryptor; textcrypt is
// fail-open, so a nil encryptor or a legacy plaintext value passes through unchanged.
type smtpIntegration struct {
	Config struct {
		Domain    string `json:"domain"`
		Port      any    `json:"port"`
		Username  string `json:"username"`
		FromName  string `json:"fromName"`
		FromEmail string `json:"fromEmail"`
		NoAuth    bool   `json:"noAuth"`
	} `json:"config"`
	Secret struct {
		Password string `json:"password"`
	} `json:"secret"`
}

// SMTPFromStore builds an SMTP Mailer from the "SMTP" thirdPartyIntegration in the given store.
// Returns ok=false when no SMTP integration is configured, or it lacks host/from — the caller then
// falls back to the PROXCLOUD_MAIL_* env gateway (FromEnv). enc is an OPTIONAL, nil-safe encryptor
// (variadic so existing 2-arg callers/tests compile unchanged) used to decrypt secret.password;
// textcrypt fail-open means a nil enc or a legacy plaintext value passes through untouched.
func SMTPFromStore(ctx context.Context, store *pgdoc.Store, enc ...*textcrypt.Encryptor) (Mailer, string, bool) {
	if store == nil {
		return nil, "", false
	}
	var doc smtpIntegration
	ok, err := store.FindOne(ctx, pgdoc.M{"thirdParty": "SMTP"}, &doc)
	if err != nil || !ok {
		return nil, "", false
	}
	if len(enc) > 0 && enc[0] != nil {
		doc.Secret.Password = enc[0].Decrypt(doc.Secret.Password)
	}
	c := doc.Config
	if c.Domain == "" || c.FromEmail == "" { // mirror Config.Enabled(): need host + from
		return nil, "", false
	}
	cfg := Config{
		Host:     c.Domain,
		Port:     portToStr(c.Port),
		Username: c.Username,
		Password: doc.Secret.Password,
		From:     c.FromEmail,
	}
	if c.NoAuth {
		cfg.Username, cfg.Password = "", ""
	}
	from := c.FromEmail
	if c.FromName != "" {
		from = c.FromName + " <" + c.FromEmail + ">"
	}
	return NewSMTPMailer(cfg), from, true
}

// portToStr renders the JSON-decoded port (JSON yields float64/int32/int64/string) as a
// string, defaulting to 587 when absent or zero.
func portToStr(v any) string {
	switch n := v.(type) {
	case string:
		if n != "" {
			return n
		}
	case float64:
		if n != 0 {
			return strconv.FormatInt(int64(n), 10)
		}
	case int32:
		if n != 0 {
			return strconv.FormatInt(int64(n), 10)
		}
	case int64:
		if n != 0 {
			return strconv.FormatInt(n, 10)
		}
	case int:
		if n != 0 {
			return strconv.Itoa(n)
		}
	}
	return "587"
}
