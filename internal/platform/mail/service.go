package mail

import (
	"context"
	"log/slog"
)

// service.go is the mail send path: render a message template (key + vars) → wrap in the
// branded shell → send via the resolved Mailer. The renderer is injected as a func so mail stays
// decoupled from the message package (and is trivially fakeable in tests). The Mailer + From are
// resolved per-send via a Resolver so the gateway can be (re)configured at runtime (the admin
// SMTP integration) without a restart. A missing/disabled template or an unconfigured gateway →
// no-op (the gated email side-effects degrade gracefully).

// RenderFunc renders a template body by key with the given vars (message.Repo.Render adapter).
type RenderFunc func(ctx context.Context, key string, vars map[string]any) (title, body string, ok bool, err error)

// Resolver returns the currently-active Mailer and the From address to send as. Called on every
// send so an operator can configure the mail gateway at runtime (DB integration) — return
// (nil, "") when no gateway is configured and SendTemplate no-ops.
type Resolver func(ctx context.Context) (mailer Mailer, from string)

// Service sends templated email.
type Service struct {
	render   RenderFunc
	resolve  Resolver
	business string // branding name injected as {{businessName}} when absent
	log      *slog.Logger
}

func NewService(render RenderFunc, resolve Resolver, business string, log *slog.Logger) *Service {
	return &Service{render: render, resolve: resolve, business: business, log: log}
}

// SendTemplate renders the template (key+vars), wraps it in the branded shell, and sends to the
// recipients. No-op when the service/resolver is nil, no gateway is configured, or the template is
// missing/disabled.
func (s *Service) SendTemplate(ctx context.Context, key string, to []string, vars map[string]any) error {
	if s == nil || s.render == nil || s.resolve == nil || len(to) == 0 {
		return nil
	}
	mailer, from := s.resolve(ctx)
	if mailer == nil {
		return nil
	}
	if vars == nil {
		vars = map[string]any{}
	}
	if _, ok := vars["businessName"]; !ok && s.business != "" {
		vars["businessName"] = s.business
	}
	title, body, ok, err := s.render(ctx, key, vars)
	if err != nil {
		return err
	}
	if !ok {
		return nil // template missing/disabled → skip
	}
	if err := mailer.Send(ctx, Message{From: from, To: to, Subject: title, HTMLBody: wrapShell(body)}); err != nil {
		// Callers dispatch mail best-effort (most discard this error); log here so a broken gateway
		// is visible in the logs instead of silently swallowed.
		if s.log != nil {
			s.log.Warn("mail send failed", "template", key, "to", to, "err", err)
		}
		return err
	}
	return nil
}

// wrapShell applies a minimal branded HTML shell around the rendered body (the faithful Jinja
// defaultTemplate.html branding shell — logo/social links — is deferred; this keeps emails valid HTML).
func wrapShell(content string) string {
	return `<!DOCTYPE html><html><head><meta charset="UTF-8"></head>` +
		`<body style="font-family:Arial,Helvetica,sans-serif;color:#222;line-height:1.5">` +
		content + `</body></html>`
}
