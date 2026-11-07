package externalservice

import "testing"

func TestParseSNATSources(t *testing.T) {
	got, err := ParseSNATSources([]any{"192.0.2.1", "192.0.2.2/32", "2001:db8::1", "2001:db8::2/128"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0] != "192.0.2.1/32" || got[1] != "192.0.2.2/32" || got[2] != "2001:db8::1/128" || got[3] != "2001:db8::2/128" {
		t.Fatalf("got %v", got)
	}
	if got, err := ParseSNATSources(nil); err != nil || len(got) != 0 {
		t.Fatalf("unset: %v, %v", got, err)
	}
	for _, bad := range []any{"192.0.2.1", []any{"192.0.2.0/24"}, []any{"0.0.0.0/0"}, []any{"0.0.0.0"}, []any{"nat.example"}, []any{""}, []any{1}} {
		if got, err := ParseSNATSources(bad); err == nil {
			t.Fatalf("%v accepted as %v", bad, got)
		}
	}
}
