package mail

import (
	"net/smtp"
	"testing"
)

func TestLegacyLOGINRequiresTLSAndHost(t *testing.T) {
	const host = "relay.example.invalid"
	for _, tc := range []struct {
		name string
		info *smtp.ServerInfo
	}{
		{"nil", nil},
		{"plaintext", &smtp.ServerInfo{Name: host}},
		{"localhost plaintext", &smtp.ServerInfo{Name: "localhost"}},
		{"wrong host", &smtp.ServerInfo{Name: "other.example.invalid", TLS: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &loginAuth{host: host, user: "fixture-user", pass: "fixture-password"}
			if mech, initial, err := a.Start(tc.info); err == nil || mech != "" || len(initial) != 0 {
				t.Fatal("unsafe LOGIN Start accepted")
			}
			if response, err := a.Next(nil, true); err == nil || len(response) != 0 {
				t.Fatal("LOGIN released a response after rejected Start")
			}
		})
	}
	a := &loginAuth{host: host, user: "fixture-user", pass: "fixture-password"}
	if response, err := a.Next(nil, true); err == nil || len(response) != 0 {
		t.Fatal("LOGIN released a response before Start")
	}
	if mech, initial, err := a.Start(&smtp.ServerInfo{Name: host, TLS: true}); err != nil || mech != "LOGIN" || len(initial) != 0 {
		t.Fatal("TLS LOGIN Start rejected")
	}
	for _, want := range []string{"fixture-user", "fixture-password"} {
		response, err := a.Next(nil, true)
		if err != nil || string(response) != want {
			t.Fatal("TLS LOGIN challenge failed")
		}
	}
	if _, err := a.Next(nil, false); err != nil {
		t.Fatal(err)
	}
	if response, err := a.Next(nil, true); err == nil || len(response) != 0 {
		t.Fatal("LOGIN released a response after completion")
	}
}

func TestLegacyNoAuthNeedsNoTLSClient(t *testing.T) {
	// The explicit NoAuth path must return before consulting transport or EHLO.
	if auth, err := authFor(nil, "relay.example.invalid", "", ""); err != nil || auth != nil {
		t.Fatal("NoAuth unexpectedly requires TLS")
	}
}
