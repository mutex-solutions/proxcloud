package powerdns

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fake is the smallest PowerDNS: zones by name, rrsets replaced on PATCH.
func fake(t *testing.T) *Service {
	t.Helper()
	zones := map[string]*Zone{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/servers/localhost/zones")
		name = strings.TrimPrefix(name, "/")
		switch {
		case r.Method == http.MethodGet && name == "":
			out := []Zone{}
			for _, z := range zones {
				out = append(out, *z)
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost:
			var in struct {
				Name, Account string
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if zones[in.Name] != nil {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"Conflict"}`))
				return
			}
			zones[in.Name] = &Zone{ID: in.Name, Name: in.Name, Account: in.Account, RRSets: []RRSet{{Name: in.Name, Type: "SOA"}, {Name: in.Name, Type: "NS"}}}
			_ = json.NewEncoder(w).Encode(zones[in.Name])
		case zones[name] == nil:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(zones[name])
		case r.Method == http.MethodPatch:
			var in struct{ RRSets []RRSet }
			_ = json.NewDecoder(r.Body).Decode(&in)
			for _, rr := range in.RRSets {
				var kept []RRSet
				for _, x := range zones[name].RRSets {
					if x.Name != rr.Name || x.Type != rr.Type {
						kept = append(kept, x)
					}
				}
				if rr.ChangeType == "REPLACE" {
					if rr.Type == "A" && rr.Records[0].Content == "bad" {
						w.WriteHeader(http.StatusUnprocessableEntity)
						_, _ = w.Write([]byte(`{"error":"Record bad: Parsing record content"}`))
						return
					}
					kept = append(kept, rr)
				}
				zones[name].RRSets = kept
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			delete(zones, name)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	s, err := New(srv.URL, "", "k", []string{"ns1.example.net"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestZoneOwnership(t *testing.T) {
	s, ctx := fake(t), context.Background()
	z, err := s.Create(ctx, "p1", "Example.ORG")
	if err != nil || z.Name != "example.org." {
		t.Fatalf("create: %+v %v", z, err)
	}
	if _, err := s.Create(ctx, "p2", "example.org."); !errors.Is(err, ErrZone) {
		t.Fatalf("a taken name: %v", err)
	}
	if _, err := s.Create(ctx, "p1", "nodot"); !errors.Is(err, ErrZone) {
		t.Fatalf("a bad name: %v", err)
	}
	// Another project sees nothing and can change nothing.
	if list, _ := s.List(ctx, "p2"); len(list) != 0 {
		t.Fatalf("p2 lists %v", list)
	}
	if _, err := s.Records(ctx, "p2", "example.org."); !errors.Is(err, ErrZone) {
		t.Fatalf("foreign records: %v", err)
	}
	if err := s.SetRecord(ctx, "p2", "example.org.", "www", "A", 60, []string{"192.0.2.1"}); !errors.Is(err, ErrZone) {
		t.Fatalf("foreign write: %v", err)
	}
	if err := s.Delete(ctx, "p2", "example.org."); err != nil {
		t.Fatalf("foreign delete is a no-op: %v", err)
	}
	if list, _ := s.List(ctx, "p1"); len(list) != 1 {
		t.Fatalf("the zone must survive a foreign delete: %v", list)
	}
}

func TestRecords(t *testing.T) {
	s, ctx := fake(t), context.Background()
	if _, err := s.Create(ctx, "p1", "example.org"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRecord(ctx, "p1", "example.org.", "www", "a", 0, []string{"192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRecord(ctx, "p1", "example.org.", "@", "TXT", 60, []string{"hello"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Records(ctx, "p1", "example.org.")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]RRSet{}
	for _, r := range rows {
		if r.Type == "SOA" {
			t.Fatal("the SOA must stay out")
		}
		found[r.Name+r.Type] = r
	}
	if www := found["www.example.org.A"]; www.TTL != 3600 || www.Records[0].Content != "192.0.2.1" {
		t.Fatalf("www = %+v", www)
	}
	if txt := found["example.org.TXT"]; txt.Records[0].Content != `"hello"` {
		t.Fatalf("txt = %+v", txt)
	}
	for _, tt := range []struct{ name, typ, content string }{
		{"www.other.org.", "A", "192.0.2.1"}, // outside the zone
		{"@", "SOA", "x"},                    // the server's
		{"@", "NS", "ns.evil.example."},      // who serves the zone
		{"www", "A", "bad"},                  // the server refuses the content
	} {
		if err := s.SetRecord(ctx, "p1", "example.org.", tt.name, tt.typ, 60, []string{tt.content}); !errors.Is(err, ErrZone) {
			t.Fatalf("%s %s: %v", tt.name, tt.typ, err)
		}
	}
	if err := s.SetRecord(ctx, "p1", "example.org.", "sub", "NS", 60, []string{"ns.sub.example."}); err != nil {
		t.Fatalf("a delegation below the apex: %v", err)
	}
	if err := s.DeleteRecord(ctx, "p1", "example.org.", "www", "A"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRecord(ctx, "p1", "example.org.", "@", "NS"); !errors.Is(err, ErrZone) {
		t.Fatalf("deleting the apex NS: %v", err)
	}
}

// A zone must not sit above or below what the project does not own: the server answers from the longest match.
func TestZoneOverlap(t *testing.T) {
	s, ctx := fake(t), context.Background()
	if _, err := s.Create(ctx, "a", "victim.org"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "a", "deep.parent.org"); err != nil {
		t.Fatal(err)
	}
	var operator Zone
	if err := s.do(ctx, http.MethodPost, "/zones", map[string]any{"name": "operator.org.", "account": ""}, &operator); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"www.victim.org",    // a child of another project's zone
		"a.b.victim.org",    // deeper
		"parent.org",        // a parent of another project's zone
		"api.operator.org",  // a child of an operator zone
		"example.net",       // holds the name server host
		"ns1.example.net",   // is the name server host
		"x.ns1.example.net", // below it
		"co.uk",             // a public suffix
	} {
		if _, err := s.Create(ctx, "b", name); !errors.Is(err, ErrZone) {
			t.Fatalf("%s: %v", name, err)
		}
		if list, _ := s.List(ctx, "b"); len(list) != 0 {
			t.Fatalf("%s left a zone: %v", name, list)
		}
	}
	for _, name := range []string{"notvictim.org", "other.co.uk"} { // a shared suffix is not a parent
		if _, err := s.Create(ctx, "b", name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := s.Create(ctx, "a", "www.victim.org"); err != nil {
		t.Fatalf("a child of the project's own zone: %v", err)
	}
	if _, err := s.Create(ctx, "a", "parent.org"); err != nil {
		t.Fatalf("a parent of the project's own zone: %v", err)
	}
}

// Two creates passing the check together: the one that sees the other after its POST gives its zone back.
func TestZoneOverlapRace(t *testing.T) {
	s, ctx := fake(t), context.Background()
	inner := s.HTTP.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	raced := false
	s.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && !raced { // the rival lands right after our check
			raced = true
			if _, err := s.Create(ctx, "a", "victim.org"); err != nil {
				t.Fatal(err)
			}
		}
		return inner.RoundTrip(r)
	})
	if _, err := s.Create(ctx, "b", "www.victim.org"); !errors.Is(err, ErrZone) {
		t.Fatalf("the overlap must be refused: %v", err)
	}
	if list, _ := s.List(ctx, "b"); len(list) != 0 {
		t.Fatalf("the loser's zone stayed: %v", list)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRecordName(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "example.org.",
		"@":                "example.org.",
		"www":              "www.example.org.",
		"www.example.org":  "www.example.org.",
		"WWW.Example.org.": "www.example.org.",
		"example.org":      "example.org.",
		"example.org.":     "example.org.",
		"a.b":              "a.b.example.org.",
		"notexample.org":   "notexample.org.example.org.",
	} {
		if got, err := recordName("example.org.", in); err != nil || got != want {
			t.Errorf("%q = %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := recordName("example.org.", "www.other.org."); !errors.Is(err, ErrZone) {
		t.Fatalf("outside the zone: %v", err)
	}
}
