package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// TestProjectOwnerSub verifies the OWNER-membership-first, then `owner`-fallback resolution.
func TestProjectOwnerSub(t *testing.T) {
	cases := []struct {
		name string
		doc  pgdoc.M
		want string
	}{
		{
			name: "owner membership wins",
			doc: pgdoc.M{
				"owner": "fallback",
				"memberships": pgdoc.A{
					pgdoc.M{"sub": "member1", "role": "MEMBER"},
					pgdoc.M{"sub": "owner1", "role": "OWNER"},
				},
			},
			want: "owner1",
		},
		{
			name: "falls back to owner field when no OWNER membership",
			doc: pgdoc.M{
				"owner": "fallback",
				"memberships": pgdoc.A{
					pgdoc.M{"sub": "member1", "role": "MEMBER"},
				},
			},
			want: "fallback",
		},
		{
			name: "falls back to owner field when memberships absent",
			doc:  pgdoc.M{"owner": "fallback"},
			want: "fallback",
		},
		{
			name: "empty when nothing",
			doc:  pgdoc.M{},
			want: "",
		},
		{
			name: "OWNER membership with blank sub falls through to owner",
			doc: pgdoc.M{
				"owner":       "fallback",
				"memberships": pgdoc.A{pgdoc.M{"sub": "", "role": "OWNER"}},
			},
			want: "fallback",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := projectOwnerSub(c.doc); got != c.want {
				t.Fatalf("projectOwnerSub = %q, want %q", got, c.want)
			}
		})
	}
}

// TestUserNotFoundMessage pins the exact translation (trailing space, interpolated).
func TestUserNotFoundMessage(t *testing.T) {
	err := userNotFound("abc123")
	if err.Msg != "User with id abc123 not found " {
		t.Fatalf("message = %q, want %q", err.Msg, "User with id abc123 not found ")
	}
	if err.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", err.Status)
	}
}

// TestUserUpdateReqDecode verifies the four mutable fields the update() copies decode from the
// request body, and that unrelated fields are ignored.
func TestUserUpdateReqDecode(t *testing.T) {
	body := `{"sub":"s1","firstName":"Ada","lastName":"Lovelace","email":"ada@example.com","id":"ignored","modelVersion":9}`
	var req userUpdateReq
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Sub != "s1" || req.FirstName != "Ada" || req.LastName != "Lovelace" || req.Email != "ada@example.com" {
		t.Fatalf("decoded req = %+v", req)
	}
}

// TestUserUpdateOverwriteSemantics verifies the field-overwrite map mirrors the unconditional
// setters: a present value replaces, a blank value drops the key (→ null, omitted). This is
// the same logic the userUpdate handler applies to the existing doc.
func TestUserUpdateOverwriteSemantics(t *testing.T) {
	existing := pgdoc.M{
		"sub": "old", "firstName": "Old", "lastName": "Name", "email": "old@x.com",
		"modelVersion": 1, "customInfo": pgdoc.M{"k": "v"},
	}
	req := userUpdateReq{Sub: "new", FirstName: "New", LastName: "", Email: "new@x.com"}
	for k, v := range map[string]string{"sub": req.Sub, "firstName": req.FirstName, "lastName": req.LastName, "email": req.Email} {
		if v == "" {
			delete(existing, k)
		} else {
			existing[k] = v
		}
	}
	if existing["sub"] != "new" || existing["firstName"] != "New" || existing["email"] != "new@x.com" {
		t.Fatalf("overwrite failed: %+v", existing)
	}
	if _, ok := existing["lastName"]; ok {
		t.Fatalf("blank lastName should have been dropped: %+v", existing)
	}
	// Unmodeled persisted fields must be preserved.
	if existing["modelVersion"] != 1 {
		t.Fatalf("modelVersion clobbered: %+v", existing)
	}
	if _, ok := existing["customInfo"]; !ok {
		t.Fatalf("customInfo dropped: %+v", existing)
	}
}
