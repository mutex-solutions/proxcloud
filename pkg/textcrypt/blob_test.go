package textcrypt

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestBlobStrictRoundTripAndFailures(t *testing.T) {
	for _, secret := range []string{"", "x", strings.Repeat("a", 31)} {
		if _, err := NewBlobKeys("v1", map[string]string{"v1": secret}); !errors.Is(err, ErrBlobKey) {
			t.Fatal("weak/missing key accepted")
		}
	}
	b, err := NewBlobKeys("v1", map[string]string{"v1": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("private body")
	aad := []byte("opaqueID/kind/v1")
	id, a, err := b.Seal(pt, aad)
	if err != nil {
		t.Fatal(err)
	}
	_, c, err := b.Seal(pt, aad)
	if err != nil || bytes.Equal(a, c) || bytes.Contains(a, pt) {
		t.Fatal("random seal failed")
	}
	got, err := b.Open(id, a, aad)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatal("roundtrip failed")
	}
	fp, _ := b.Fingerprint(id, pt)
	fp2, _ := b.Fingerprint(id, pt)
	if !bytes.Equal(fp, fp2) {
		t.Fatal("random fingerprint")
	}
	other, _ := NewBlobKeys("v1", map[string]string{"v1": strings.Repeat("z", 32)})
	badVersion := bytes.Clone(a)
	badVersion[0] = 2
	tampered := bytes.Clone(a)
	tampered[len(tampered)-1] ^= 1
	for _, f := range []func() ([]byte, error){func() ([]byte, error) { return b.Open("missing", a, aad) }, func() ([]byte, error) { return b.Open(id, a, []byte("wrong")) }, func() ([]byte, error) { return b.Open(id, badVersion, aad) }, func() ([]byte, error) { return b.Open(id, tampered, aad) }, func() ([]byte, error) { return other.Open(id, a, aad) }, func() ([]byte, error) { return b.Open(id, []byte{1}, aad) }} {
		if got, err := f(); err == nil || len(got) != 0 || strings.Contains(err.Error(), string(pt)) {
			t.Fatal("failure returned data")
		}
	}
	rotated, _ := NewBlobKeys("v2", map[string]string{"v1": strings.Repeat("k", 32), "v2": strings.Repeat("b", 32)})
	if _, err = rotated.Open("v1", a, aad); err != nil {
		t.Fatal("retained key failed")
	}
	removed, _ := NewBlobKeys("v2", map[string]string{"v2": strings.Repeat("b", 32)})
	if _, err = removed.Open("v1", a, aad); !errors.Is(err, ErrBlobKey) {
		t.Fatal("missing old key allowed")
	}
	var nilKeys *BlobKeys
	if _, _, err = nilKeys.Seal(pt, aad); !errors.Is(err, ErrBlobKey) {
		t.Fatal("nil seal allowed")
	}
}
