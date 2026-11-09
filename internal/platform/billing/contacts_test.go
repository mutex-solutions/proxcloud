package billing

import (
	"slices"
	"testing"
)

func TestRecipients(t *testing.T) {
	bp := &BillingProfile{
		Email: " Ann@Acme.sg ",
		Contacts: []Contact{
			{Name: "AP", Email: "ap@acme.sg"},
			{Email: "ann@acme.sg"}, // duplicate of the profile email, different case
			{Email: "   "},         // blank → dropped
			{Email: "AP@acme.sg"},  // duplicate contact, different case
			{Name: "CFO", Email: "cfo@acme.sg"},
		},
	}
	got := Recipients(bp)
	want := []string{"Ann@Acme.sg", "ap@acme.sg", "cfo@acme.sg"}
	if !slices.Equal(got, want) {
		t.Fatalf("Recipients = %v, want %v", got, want)
	}
	if got := Recipients(&BillingProfile{Contacts: []Contact{{Email: "only@acme.sg"}}}); !slices.Equal(got, []string{"only@acme.sg"}) {
		t.Fatalf("no profile email: %v", got)
	}
	if got := Recipients(&BillingProfile{}); len(got) != 0 {
		t.Fatalf("empty profile: %v", got)
	}
	if got := Recipients(nil); got != nil {
		t.Fatalf("nil profile: %v", got)
	}
}

func TestNormalizeContacts(t *testing.T) {
	got, err := NormalizeContacts([]Contact{
		{Name: " AP ", Email: " ap@acme.sg "},
		{Email: "AP@acme.sg"},
		{Name: "blank", Email: ""},
		{Name: "CFO", Email: "cfo@acme.sg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Contact{{Name: "AP", Email: "ap@acme.sg"}, {Name: "CFO", Email: "cfo@acme.sg"}}
	if !slices.Equal(got, want) {
		t.Fatalf("normalized = %+v, want %+v", got, want)
	}
	if got, err := NormalizeContacts(nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("nil in → non-nil empty out, got %v err %v", got, err)
	}
	for _, bad := range []string{"not-an-email", "a@b", "Ann <ann@acme.sg>", "two@acme.sg, three@acme.sg"} {
		if _, err := NormalizeContacts([]Contact{{Email: bad}}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	many := make([]Contact, MaxContacts+1)
	for i := range many {
		many[i].Email = string(rune('a'+i)) + "@acme.sg"
	}
	if _, err := NormalizeContacts(many); err == nil {
		t.Fatal("over MaxContacts accepted")
	}
	if _, err := NormalizeContacts(many[:MaxContacts]); err != nil {
		t.Fatalf("exactly MaxContacts rejected: %v", err)
	}
}
