package truenas

import (
	"testing"
)

func TestEmptyAccessNeverMeansEveryone(t *testing.T) {
	if got := exportList(nil); len(got) != 1 || got[0] != "127.0.0.1/32" {
		t.Fatalf("exportList(nil) = %v", got)
	}
	if got := exportList([]string{"10.0.0.0/24"}); got[0] != "10.0.0.0/24" {
		t.Fatalf("exportList = %v", got)
	}
}

func TestPlainHTTPRefused(t *testing.T) {
	for _, u := range []string{"http://nas", "ws://nas/api/current"} {
		if _, err := New(Config{URL: u, Username: "u", APIKey: "k"}); err == nil {
			t.Fatalf("%s accepted", u)
		}
	}
	c, err := New(Config{URL: "https://nas:8443/", Username: "u", APIKey: "k"})
	if err != nil || c.cfg.URL != "wss://nas:8443/api/current" {
		t.Fatalf("url = %v, err %v", c, err)
	}
}

func TestSMBUserStaysWithinTheLimit(t *testing.T) {
	if u := smbUser("6aee381651ff712e6b0029bd", "a-very-long-share-name-of-32-char"); len(u) > 32 {
		t.Fatalf("%s is %d long", u, len(u))
	}
}
