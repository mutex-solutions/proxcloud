package account

import "testing"

func TestGravatar_NormalizesEmail(t *testing.T) {
	// lower-cased + trimmed before hashing → these must all match.
	base := gravatar("test@example.com")
	for _, in := range []string{"test@example.com", "  test@example.com ", "TEST@Example.COM", "Test@Example.Com\n"} {
		if got := gravatar(in); got != base {
			t.Errorf("gravatar(%q)=%q, want %q", in, got, base)
		}
	}
	const want = "https://www.gravatar.com/avatar/55502f40dc8b7c769880b10874abc9d0?d=mp"
	if base != want {
		t.Errorf("gravatar(test@example.com)=%q, want %q", base, want)
	}
}

func TestLanguage(t *testing.T) {
	cases := []struct {
		ci   map[string]any
		want string
	}{
		{nil, "EN"},
		{map[string]any{}, "EN"},
		{map[string]any{"lang": "ro-ro"}, "RO"},
		{map[string]any{"lang": "RO-RO"}, "RO"}, // EqualFold
		{map[string]any{"lang": "en-us"}, "EN"},
		{map[string]any{"lang": 123}, "EN"}, // non-string ignored
	}
	for _, c := range cases {
		if got := language(c.ci); got != c.want {
			t.Errorf("language(%v)=%q, want %q", c.ci, got, c.want)
		}
	}
}
