package billing

import (
	"fmt"
	"net/mail"
	"strings"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// MaxContacts caps the additional billing contacts a profile may carry.
const MaxContacts = 10

// Recipients is every address a customer-facing billing mail goes to: the profile's own email
// first, then each contact's email. Trimmed, de-duplicated case-insensitively (first spelling
// wins), blanks dropped.
func Recipients(bp *BillingProfile) []string {
	if bp == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(e string) {
		e = strings.TrimSpace(e)
		k := strings.ToLower(e)
		if e == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, e)
	}
	add(bp.Email)
	for _, c := range bp.Contacts {
		add(c.Email)
	}
	return out
}

// NormalizeContacts validates + cleans a contacts list from a request body: names/emails trimmed,
// entries without an email dropped, syntax checked (net/mail), duplicates (case-insensitive)
// collapsed, at most MaxContacts. Returns a 400 *httpx.HTTPError on a bad address or overflow.
// The result is never nil so a cleared list persists as [] rather than being skipped.
func NormalizeContacts(in []Contact) ([]Contact, error) {
	out := make([]Contact, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		email := strings.TrimSpace(c.Email)
		if email == "" {
			continue
		}
		// Bare address only (no display name) and a dotted domain — net/mail accepts "a@b".
		addr, err := mail.ParseAddress(email)
		if err != nil || addr.Address != email || !strings.Contains(email[strings.LastIndex(email, "@")+1:], ".") {
			return nil, httpx.BadRequest(fmt.Sprintf("Invalid contact email: %s", email))
		}
		k := strings.ToLower(email)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, Contact{Name: strings.TrimSpace(c.Name), Email: email})
	}
	if len(out) > MaxContacts {
		return nil, httpx.BadRequest(fmt.Sprintf("At most %d billing contacts are allowed", MaxContacts))
	}
	return out, nil
}
