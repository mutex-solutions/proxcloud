package textcrypt

import (
	"encoding/base64"
	"reflect"
	"testing"
)

const testKey = "dev-359ac834f15e8e013db24c77b6b3e64c" // shape of the real ProxCloud key (ASCII)

func TestRoundTrip(t *testing.T) {
	e := New(testKey)
	for _, pt := range []string{"", "p", "swordfish", "a-much-longer-secret-value-0123456789-äöü", "exactly-16-bytes"} {
		ct := e.Encrypt(pt)
		if ct == pt && pt != "" {
			t.Fatalf("ciphertext equals plaintext for %q", pt)
		}
		if got := e.Decrypt(ct); got != pt {
			t.Fatalf("round-trip %q: got %q", pt, got)
		}
	}
}

func TestWireFormat(t *testing.T) {
	e := New(testKey)
	raw, err := base64.StdEncoding.DecodeString(e.Encrypt("hello"))
	if err != nil {
		t.Fatal(err)
	}
	// salt(16) + iv(16) + at least one cipher block(16).
	if len(raw) != saltLen+ivLen+16 {
		t.Fatalf("expected %d bytes (salt+iv+1 block), got %d", saltLen+ivLen+16, len(raw))
	}
}

func TestNonDeterministic(t *testing.T) {
	e := New(testKey)
	if e.Encrypt("x") == e.Encrypt("x") {
		t.Fatal("encryption must use a fresh random salt+IV each call")
	}
}

func TestDecryptPassThroughOnGarbage(t *testing.T) {
	e := New(testKey)
	// Not base64 / not ours → returned unchanged (pass-through behaviour).
	for _, s := range []string{"not-encrypted", "plain text value", "!!!"} {
		if got := e.Decrypt(s); got != s {
			t.Fatalf("expected pass-through for %q, got %q", s, got)
		}
	}
}

func TestNoKeyPassThrough(t *testing.T) {
	e := New("")
	if e.HasKey() {
		t.Fatal("empty key should report HasKey()=false")
	}
	if got := e.Encrypt("secret"); got != "secret" {
		t.Fatalf("no-key Encrypt should pass through, got %q", got)
	}
	if got := e.Decrypt("secret"); got != "secret" {
		t.Fatalf("no-key Decrypt should pass through, got %q", got)
	}
}

func TestObjectWalk(t *testing.T) {
	e := New(testKey)
	secret := map[string]any{
		"adminPassword":               "super-secret",
		"applicationCredentialSecret": "another-secret",
		"vhiOstorAuth": map[string]any{ // nested object → recurse
			"accessKey": "ak",
			"secretKey": "sk",
		},
		"enabled": true,            // non-textual leaf → untouched
		"count":   42,              // number → untouched
		"tags":    []any{"a", "b"}, // array → untouched (arrays are skipped)
	}
	enc := e.EncryptObject(secret).(map[string]any)

	// textual leaves changed
	if enc["adminPassword"] == "super-secret" {
		t.Fatal("adminPassword not encrypted")
	}
	if nested := enc["vhiOstorAuth"].(map[string]any); nested["accessKey"] == "ak" {
		t.Fatal("nested accessKey not encrypted")
	}
	// non-textual leaves untouched
	if enc["enabled"] != true || enc["count"] != 42 {
		t.Fatal("non-textual leaves must pass through unchanged")
	}
	if !reflect.DeepEqual(enc["tags"], []any{"a", "b"}) {
		t.Fatal("array must pass through unchanged")
	}

	dec := e.DecryptObject(enc).(map[string]any)
	if dec["adminPassword"] != "super-secret" || dec["applicationCredentialSecret"] != "another-secret" {
		t.Fatalf("object round-trip failed: %#v", dec)
	}
	if nested := dec["vhiOstorAuth"].(map[string]any); nested["accessKey"] != "ak" || nested["secretKey"] != "sk" {
		t.Fatalf("nested object round-trip failed: %#v", nested)
	}
}

func TestNonMapObjectUnchanged(t *testing.T) {
	e := New(testKey)
	if got := e.DecryptObject("just a string"); got != "just a string" {
		t.Fatalf("non-map input must pass through, got %v", got)
	}
	if got := e.DecryptObject(nil); got != nil {
		t.Fatalf("nil must pass through, got %v", got)
	}
}
