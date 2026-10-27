package mail

import "testing"

// TestPortToStr pins the JSON number→string coercion + the 587 default.
func TestPortToStr(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{float64(465), "465"}, // JSON number
		{int32(2525), "2525"},
		{int64(587), "587"},
		{int(25), "25"},
		{"1025", "1025"},
		{float64(0), "587"}, // zero → default
		{"", "587"},         // empty → default
		{nil, "587"},        // absent → default
	}
	for _, c := range cases {
		if got := portToStr(c.in); got != c.want {
			t.Errorf("portToStr(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
