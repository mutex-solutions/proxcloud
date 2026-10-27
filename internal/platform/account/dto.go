package account

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/user"
)

// AccountDetailsDTO carries the account fields: id, sub, createdAt, consent,
// customInfo, firstName, lastName, email, identities, language, gravatarUrl.
// NOTE: no updatedAt; consent + customInfo are ALWAYS present (empty [] / {}),
// not omitted.
type AccountDetailsDTO struct {
	ID               string          `json:"id,omitempty"`
	Sub              string          `json:"sub"`
	CreatedAt        *time.Time      `json:"createdAt,omitempty"`
	Consent          []string        `json:"consent"`
	CustomInfo       map[string]any  `json:"customInfo"`
	FirstName        string          `json:"firstName,omitempty"`
	LastName         string          `json:"lastName,omitempty"`
	Email            string          `json:"email,omitempty"`
	EmailConfirmedAt *time.Time      `json:"emailConfirmedAt,omitempty"`
	Tags             []string        `json:"tags,omitempty"`
	Identities       []user.Identity `json:"identities,omitempty"`
	Language         string          `json:"language"`
	GravatarURL      string          `json:"gravatarUrl"`
}

func toAccountDetails(u *user.User) AccountDetailsDTO {
	consent := u.Consent
	if consent == nil {
		consent = []string{}
	}
	ci := u.CustomInfo
	if ci == nil {
		ci = map[string]any{}
	}
	return AccountDetailsDTO{
		ID:               u.ID,
		Sub:              u.Sub,
		CreatedAt:        u.CreatedAt,
		Consent:          consent,
		CustomInfo:       ci,
		FirstName:        u.FirstName,
		LastName:         u.LastName,
		Email:            u.Email,
		EmailConfirmedAt: u.EmailConfirmedAt,
		Tags:             u.Tags,
		Identities:       u.Identities,
		Language:         language(ci),
		GravatarURL:      gravatar(u.Email),
	}
}

// gravatar = https://www.gravatar.com/avatar/<md5(lower(trim(email)))>?d=mp
func gravatar(email string) string {
	sum := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "https://www.gravatar.com/avatar/" + hex.EncodeToString(sum[:]) + "?d=mp"
}

// language: RO when customInfo.lang == "ro-ro", else EN.
func language(ci map[string]any) string {
	if ci != nil {
		if v, ok := ci["lang"].(string); ok && strings.EqualFold(v, "ro-ro") {
			return "RO"
		}
	}
	return "EN"
}
