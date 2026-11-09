package powerdns

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestCanonicalOwnershipZone(t *testing.T) {
	for in, want := range map[string]string{" Example.ORG. ": "example.org.", "BÜCHER.de": "xn--bcher-kva.de.", "api.example.co.uk": "api.example.co.uk.", "tenant.github.io": "tenant.github.io."} {
		got, err := CanonicalZone(in)
		if err != nil || got != want {
			t.Errorf("%q = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"co.uk", "com", "github.io", "localhost", "foo.internal", "a..example.org", "example.org..", "a_b.example.org", "*.example.org", "example.org/path", "evil.org\nexample.org", "example.org:53", "127.0.0.1", "-a.example.org", strings.Repeat("a", 64) + ".org"} {
		if _, err := CanonicalZone(in); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestOwnershipResolverConfig(t *testing.T) {
	if r, err := OwnershipResolvers(nil); err != nil || strings.Join(r, ",") != "1.1.1.1:53,8.8.8.8:53" {
		t.Fatal(r, err)
	}
	for _, v := range []any{[]any{}, "8.8.8.8:53", []any{"https://evil.org/dns-query"}, []any{"dns.local:53"}, []any{"127.0.0.1:0"}, []any{"0.0.0.0:53"}, []any{"224.0.0.1:53"}, []any{"[fe80::1%eth0]:53"}, []any{42}} {
		if _, err := OwnershipResolvers(v); err == nil {
			t.Errorf("accepted %v", v)
		}
	}
	if _, err := OwnershipResolvers([]any{"10.88.0.10:5300", "[2606:4700:4700::1111]:53"}); err != nil {
		t.Fatal(err)
	}
}

func TestProofDNSWire(t *testing.T) {
	name := "_proxcloud-ownership.example.org."
	n, _ := dnsmessage.NewName(name)
	base := func() dnsmessage.Message {
		return dnsmessage.Message{Header: dnsmessage.Header{ID: 41, Response: true, RecursionAvailable: true}, Questions: []dnsmessage.Question{{Name: n, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}}, Answers: []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: n, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.TXTResource{TXT: []string{"proxcloud-", "token"}}}}}
	}
	tests := []struct {
		name   string
		change func(*dnsmessage.Message)
		bad    bool
		count  int
	}{
		{"exact chunked RR", func(m *dnsmessage.Message) {}, false, 1},
		{"foreign owner", func(m *dnsmessage.Message) { m.Answers[0].Header.Name, _ = dnsmessage.NewName("else.example.org.") }, false, 0},
		{"CNAME plus correct TXT", func(m *dnsmessage.Message) {
			m.Answers = append(m.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: n, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET}, Body: &dnsmessage.CNAMEResource{CNAME: n}})
		}, true, 0},
		{"wrong ID", func(m *dnsmessage.Message) { m.ID++ }, true, 0},
		{"truncated", func(m *dnsmessage.Message) { m.Truncated = true }, true, 0},
		{"servfail", func(m *dnsmessage.Message) { m.RCode = dnsmessage.RCodeServerFailure }, true, 0},
		{"NXDOMAIN no proof", func(m *dnsmessage.Message) { m.RCode = dnsmessage.RCodeNameError }, false, 0},
		{"question mismatch", func(m *dnsmessage.Message) { m.Questions[0].Type = dnsmessage.TypeA }, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.change(&m)
			wire, err := m.Pack()
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseProofTXT(wire, 41, name)
			if (err != nil) != tc.bad || len(got) != tc.count {
				t.Fatalf("got %v, %v", got, err)
			}
			if tc.count == 1 && got[0] != "proxcloud-token" {
				t.Fatal(got)
			}
		})
	}
	if _, err := parseProofTXT([]byte("bad"), 41, name); err == nil {
		t.Fatal("malformed response accepted")
	}
}

func TestProofResolverUsesFixedTCP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	done := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		var size [2]byte
		if _, err = io.ReadFull(c, size[:]); err != nil {
			done <- err
			return
		}
		wire := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err = io.ReadFull(c, wire); err != nil {
			done <- err
			return
		}
		var q dnsmessage.Message
		if err = q.Unpack(wire); err != nil {
			done <- err
			return
		}
		q.Response = true
		q.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}, Body: &dnsmessage.TXTResource{TXT: []string{"exact-token"}}}}
		wire, err = q.Pack()
		if err == nil {
			binary.BigEndian.PutUint16(size[:], uint16(len(wire)))
			_, err = c.Write(append(size[:], wire...))
		}
		done <- err
	}()
	got, err := LookupProofTXT(context.Background(), []string{l.Addr().String()}, "_proxcloud-ownership.example.org.")
	if err != nil || len(got) != 1 || got[0] != "exact-token" {
		t.Fatalf("%v %v", got, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
