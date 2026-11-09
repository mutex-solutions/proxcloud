package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceAuthIsExplicitAndNeverUsesPlatformFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := sourceCredential(path, "registry.example.test"); err == nil {
		t.Fatal("missing source auth file selected anonymous")
	}
	for _, invalid := range []string{"", `{}`, `{"auths":null}`, `{"auths":{"registry.example.test":{"username":"caller"}}}`, `{"auths":{"other":{"username":"robot","password":"secret"}}}`} {
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := sourceCredential(path, "registry.example.test"); err == nil {
			t.Fatalf("accepted invalid source auth: %s", invalid)
		}
	}
	if err := os.WriteFile(path, []byte(`{"auths":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	auth, err := sourceCredential(path, "registry.example.test")
	if err != nil || auth.Username != "" || auth.Password != "" {
		t.Fatalf("anonymous=%+v %v", auth, err)
	}
	if _, err = credential(path, "registry.example.test"); err == nil {
		t.Fatal("platform credential accepted anonymous document")
	}
}
