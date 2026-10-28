// Package user is the domain User: the tenant principal, keyed by `sub`.
// Backed by the `users` collection with get-or-create-by-sub semantics, so
// every authed request resolves to (and, on first sight, creates) a User row.
package user

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Identity is one federated/OIDC identity attached to a user.
type Identity struct {
	Sub    string         `json:"sub"`
	Issuer string         `json:"issuer"`
	Claims map[string]any `json:"claims,omitempty"`
}

// User is a document in the `users` collection. Money-free, so no money fields here.
type User struct {
	ID               string         `json:"id,omitempty"`
	Sub              string         `json:"sub"`
	ModelVersion     int            `json:"modelVersion"`
	Consent          []string       `json:"consent"`
	CustomInfo       map[string]any `json:"customInfo"`
	FirstName        string         `json:"firstName,omitempty"`
	LastName         string         `json:"lastName,omitempty"`
	Email            string         `json:"email,omitempty"`
	EmailConfirmedAt *time.Time     `json:"emailConfirmedAt,omitempty"`
	Tags             []string       `json:"tags,omitempty"`
	Identities       []Identity     `json:"identities,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	CreatedAt        *time.Time     `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time     `json:"updatedAt,omitempty"`
}

// MarshalJSON serializes the User: consent + customInfo are
// ALWAYS present (empty [] / {}, never null/omitted) and a computed `language`
// field is added (RO if customInfo.lang == ro-ro, else EN).
func (u User) MarshalJSON() ([]byte, error) {
	type alias User
	a := alias(u)
	if a.Consent == nil {
		a.Consent = []string{}
	}
	if a.CustomInfo == nil {
		a.CustomInfo = map[string]any{}
	}
	return json.Marshal(&struct {
		alias
		Language string `json:"language"`
	}{a, computeLanguage(a.CustomInfo)})
}

// FullName returns the trimmed "first last", or email when blank.
func (u *User) FullName() string {
	full := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if full == "" {
		return u.Email
	}
	return full
}

func computeLanguage(ci map[string]any) string {
	if ci != nil {
		if v, ok := ci["lang"].(string); ok && strings.EqualFold(v, "ro-ro") {
			return "RO"
		}
	}
	return "EN"
}

// Claims is the minimal principal info extracted from a validated token.
type Claims struct {
	Sub           string
	Email         string
	EmailVerified bool // the IdP's email_verified claim
	GivenName     string
	FamilyName    string
	Issuer        string
}

// Repo provides persistence operations for User documents in the `users` collection.
type Repo struct {
	col *pgdoc.Store
}

// NewRepo constructs a user repository backed by the `users` collection.
func NewRepo(db *pgdoc.DB) *Repo {
	return &Repo{col: db.C("users")}
}

// EnsureIndexes creates the table + sub unique index + email index (app-enforced).
func (r *Repo) EnsureIndexes(ctx context.Context) error {
	if err := r.col.Ensure(ctx); err != nil {
		return err
	}
	if err := r.col.EnsureIndex(ctx, "sub_unique", true, pgdoc.F("sub")); err != nil {
		return err
	}
	return r.col.EnsureIndex(ctx, "email", false, pgdoc.F("email"))
}

func (r *Repo) FindByEmail(ctx context.Context, email string) (*User, error) {
	var u User
	found, err := r.col.FindOne(ctx, pgdoc.M{"email": email}, &u)
	if err != nil || !found {
		return nil, err
	}
	return &u, nil
}

// FindVerifiedByEmail returns the user whose email the IdP has verified, or nil.
// Matching a person by email (org add-member, project invites) must use this:
// anyone can sign up with an address they do not own.
func (r *Repo) FindVerifiedByEmail(ctx context.Context, email string) (*User, error) {
	var u User
	found, err := r.col.FindOne(ctx, pgdoc.M{"email": email, "emailConfirmedAt": pgdoc.M{"$ne": nil}}, &u)
	if err != nil || !found {
		return nil, err
	}
	return &u, nil
}

// EmailVerified reports whether the IdP has confirmed the user's current email.
func (u *User) EmailVerified() bool { return u.Email != "" && u.EmailConfirmedAt != nil }

func (r *Repo) FindBySub(ctx context.Context, sub string) (*User, error) {
	var u User
	found, err := r.col.FindOne(ctx, pgdoc.M{"sub": sub}, &u)
	if err != nil || !found {
		return nil, err
	}
	return &u, nil
}

// ExistsByID reports whether a user with the given id exists (used by the
// affiliate cfy check). An unknown/malformed id is "not found" (no error).
func (r *Repo) ExistsByID(ctx context.Context, id string) (bool, error) {
	return r.col.Exists(ctx, pgdoc.M{"_id": id})
}

// Require loads the User for the principal, GET-OR-CREATING it from the validated
// request-context claims on first sight (the auth-layer get-or-create). This makes the
// platform user exist by the time any authed client handler runs, independent of the FE's
// one-shot POST /user init (which races the dashboard's first queries and can leave a
// social-login user "not initialized"). Falls back to the 400 only when there are no
// usable claims in context (e.g. a non-request ctx in tests).
func (r *Repo) Require(ctx context.Context, sub string) (*User, error) {
	u, err := r.FindBySub(ctx, sub)
	if err != nil {
		return nil, err
	}
	if u != nil {
		return u, nil
	}
	// Absent → create from the already-validated access-token claims carried on the context.
	if rc := httpx.RC(ctx); rc != nil && rc.Sub != "" && rc.Sub == sub {
		return r.FromClaims(ctx, Claims{
			Sub: rc.Sub, Email: rc.Email, EmailVerified: rc.EmailVerified,
			GivenName: rc.GivenName, FamilyName: rc.FamilyName, Issuer: rc.Issuer,
		})
	}
	return nil, httpx.BadRequest("User is not initialized")
}

// FromClaims resolves the principal to a User, creating it on first sight
// (get-or-create by sub) and refreshing name/identity. The returned User
// always exists in the datastore.
func (r *Repo) FromClaims(ctx context.Context, c Claims) (*User, error) {
	existing, err := r.FindBySub(ctx, c.Sub)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if existing == nil {
		u := &User{
			Sub:          c.Sub,
			Email:        c.Email,
			FirstName:    c.GivenName,
			LastName:     c.FamilyName,
			ModelVersion: 1,
			Identities:   []Identity{{Sub: c.Sub, Issuer: c.Issuer}},
			CreatedAt:    &now,
			UpdatedAt:    &now,
		}
		if c.Email != "" && c.EmailVerified {
			u.EmailConfirmedAt = &now
		}
		id, err := r.col.InsertOne(ctx, u)
		if err != nil {
			// Lost a concurrent first-sight race (sub_unique): another request
			// created the user between our find and insert — read theirs back.
			if again, ferr := r.FindBySub(ctx, c.Sub); ferr == nil && again != nil {
				return again, nil
			}
			return nil, err
		}
		u.ID = id
		return u, nil
	}
	// Refresh email/name from claims if changed; skip the write entirely when nothing
	// changed — FromClaims runs on every request and an unconditional updatedAt bump
	// would issue a users UPDATE per API call.
	set := pgdoc.M{}
	var unset []string
	if c.Email != "" {
		changed := c.Email != existing.Email
		if changed {
			set["email"] = c.Email
			existing.Email = c.Email
		}
		// emailConfirmedAt mirrors the claim for the CURRENT email: stamped when it
		// becomes verified (or changes while verified), cleared when it is not.
		switch {
		case c.EmailVerified && (changed || existing.EmailConfirmedAt == nil):
			set["emailConfirmedAt"] = now
			existing.EmailConfirmedAt = &now
		case !c.EmailVerified && existing.EmailConfirmedAt != nil:
			unset = append(unset, "emailConfirmedAt")
			existing.EmailConfirmedAt = nil
		}
	}
	if c.GivenName != "" && c.GivenName != existing.FirstName {
		set["firstName"] = c.GivenName
		existing.FirstName = c.GivenName
	}
	if c.FamilyName != "" && c.FamilyName != existing.LastName {
		set["lastName"] = c.FamilyName
		existing.LastName = c.FamilyName
	}
	if len(set) > 0 || len(unset) > 0 {
		set["updatedAt"] = now
		if _, err := r.col.SetFieldsOne(ctx, pgdoc.M{"sub": c.Sub}, set, unset); err != nil {
			return nil, err
		}
		existing.UpdatedAt = &now
	}
	return existing, nil
}

// UpdateName applies firstName/lastName only when non-blank.
// Returns the updated user and whether anything changed.
func (r *Repo) UpdateName(ctx context.Context, sub, first, last string) (*User, bool, error) {
	u, err := r.FindBySub(ctx, sub)
	if err != nil || u == nil {
		return u, false, err
	}
	set := pgdoc.M{}
	if first != "" && first != u.FirstName {
		set["firstName"] = first
		u.FirstName = first
	}
	if last != "" && last != u.LastName {
		set["lastName"] = last
		u.LastName = last
	}
	if len(set) == 0 {
		return u, false, nil
	}
	now := time.Now().UTC()
	set["updatedAt"] = now
	if _, err := r.col.SetFieldsOne(ctx, pgdoc.M{"sub": sub}, set, nil); err != nil {
		return nil, false, err
	}
	u.UpdatedAt = &now
	return u, true, nil
}

// SetCustomInfo sets customInfo[key]=value (initializing the map) and returns
// the whole map (echoes the full customInfo).
func (r *Repo) SetCustomInfo(ctx context.Context, sub, key string, value any) (map[string]any, error) {
	u, err := r.FindBySub(ctx, sub)
	if err != nil || u == nil {
		return nil, err
	}
	if u.CustomInfo == nil {
		u.CustomInfo = map[string]any{}
	}
	u.CustomInfo[key] = value
	if err := r.replaceCustomInfo(ctx, sub, u.CustomInfo); err != nil {
		return nil, err
	}
	return u.CustomInfo, nil
}

// DeleteCustomInfo removes customInfo[key] and returns the whole map.
func (r *Repo) DeleteCustomInfo(ctx context.Context, sub, key string) (map[string]any, error) {
	u, err := r.FindBySub(ctx, sub)
	if err != nil || u == nil {
		return nil, err
	}
	if u.CustomInfo == nil {
		u.CustomInfo = map[string]any{}
	}
	delete(u.CustomInfo, key)
	if err := r.replaceCustomInfo(ctx, sub, u.CustomInfo); err != nil {
		return nil, err
	}
	return u.CustomInfo, nil
}

func (r *Repo) replaceCustomInfo(ctx context.Context, sub string, ci map[string]any) error {
	_, err := r.col.SetFieldsOne(ctx, pgdoc.M{"sub": sub},
		pgdoc.M{"customInfo": ci, "updatedAt": time.Now().UTC()}, nil)
	return err
}
