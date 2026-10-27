package billing

import (
	"context"
	"strings"
)

// notify.go is the billing-jobs' email hook. The concrete mail.Service is
// injected via SetNotifier on the jobs; when absent (no mail gateway configured) the calls are
// no-ops, so the gated email side-effects degrade gracefully. Email failures are swallowed —
// a notification must never break the billing state machine.

// Notifier sends a templated email (mail.Service satisfies it).
//
// EXTRACTION SEAM: mail templates and the SMTP integration live in proxcloud today, so on extraction
// this becomes a billing→proxcloud callback (POST /internal/v1/notify) until the billing service owns
// its own mailer. The signature is already wire-ready: every argument is JSON-serializable.
type Notifier interface {
	SendTemplate(ctx context.Context, key string, to []string, vars map[string]any) error
}

// notify is the nil-safe, best-effort send helper.
func notify(ctx context.Context, n Notifier, key, to string, vars map[string]any) {
	if n == nil || strings.TrimSpace(to) == "" {
		return
	}
	_ = n.SendTemplate(ctx, key, []string{to}, vars)
}

// profileFullName is the profile's full name (firstName + " " + lastName).
func profileFullName(p *BillingProfile) string {
	return strings.TrimSpace(p.FirstName + " " + p.LastName)
}
