package mail

import (
	"context"
	"errors"
	"net/mail"
	"os"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

var errDurableConfig = errors.New("mail outbox: SMTP configuration unavailable")

// DurableConfigFromStore resolves credentials in memory each attempt, preserving
// DB-over-env precedence. A present but broken DB gateway is BLOCKED, never a
// silent fallback to an unrelated env relay. Reads/errors never log credentials.
// Legacy credential encoding stays interoperable; the OUTBOX BLOB is always the
// separate strict AEAD format, never Encryptor.Encrypt/Decrypt pass-throughs.
func DurableConfigFromStore(store *pgdoc.Store, enc *textcrypt.Encryptor) DurableConfigResolver {
	return func(ctx context.Context) (Config, error) {
		if store != nil {
			var d smtpIntegration
			found, err := store.FindOne(ctx, pgdoc.M{"thirdParty": "SMTP"}, &d)
			if err != nil {
				return Config{}, errDurableConfig
			}
			if found {
				c := d.Config
				password := d.Secret.Password
				if enc != nil {
					password = enc.Decrypt(password)
				}
				from := c.FromEmail
				if c.FromName != "" {
					from = (&mail.Address{Name: c.FromName, Address: c.FromEmail}).String()
				}
				cfg := Config{Host: c.Domain, Port: portToStr(c.Port), Username: c.Username, Password: password, From: from}
				if c.NoAuth {
					cfg.Username = ""
					cfg.Password = ""
				}
				if !cfg.Enabled() || (!c.NoAuth && cfg.Username == "") {
					return Config{}, errDurableConfig
				}
				return cfg, nil
			}
		}
		cfg := Config{Host: os.Getenv("PROXCLOUD_MAIL_SMTP_HOST"), Port: envOr("PROXCLOUD_MAIL_SMTP_PORT", "587"), Username: os.Getenv("PROXCLOUD_MAIL_SMTP_USERNAME"), Password: os.Getenv("PROXCLOUD_MAIL_SMTP_PASSWORD"), From: os.Getenv("PROXCLOUD_MAIL_FROM")}
		if !cfg.Enabled() {
			return Config{}, errDurableConfig
		}
		return cfg, nil
	}
}

// LegacyFrozenSender is a deliberately separate raw-envelope capability: an old
// Message-only Mailer is NOT adapted by re-rendering frozen Date/Message-ID.
// Callers must explicitly supply an immutable-envelope sender. This is not wired
// in the app and does not grant the loopback-only dispatcher test capability.
type LegacyFrozenSender interface {
	SendFrozen(context.Context, mailoutbox.Envelope) error
}
type LegacyDurableAdapter struct{ Sender LegacyFrozenSender }

func (a LegacyDurableAdapter) Deliver(ctx context.Context, e mailoutbox.Envelope) mailoutbox.Receipt {
	if a.Sender == nil {
		return configReceipt()
	}
	if err := a.Sender.SendFrozen(ctx, e); err != nil {
		return smtpReceipt(mailoutbox.Indeterminate, mailoutbox.StageLegacy, mailoutbox.ClassLegacy, false)
	}
	return smtpReceipt(mailoutbox.RelayAccepted, mailoutbox.StageReply, mailoutbox.ClassOK, false)
}
