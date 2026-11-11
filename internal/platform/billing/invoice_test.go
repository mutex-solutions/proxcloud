package billing

import "testing"

func TestFormatInvoiceNumber(t *testing.T) {
	cases := []struct {
		year int
		seq  int64
		want string
	}{
		{2026, 1, "INV-2026-000001"},
		{2026, 42, "INV-2026-000042"},
		{2027, 999999, "INV-2027-999999"},
		{2027, 1000000, "INV-2027-1000000"}, // overflow widens, never truncates
	}
	for _, c := range cases {
		if got := FormatInvoiceNumber(c.year, c.seq); got != c.want {
			t.Errorf("FormatInvoiceNumber(%d,%d)=%q want %q", c.year, c.seq, got, c.want)
		}
	}
}
