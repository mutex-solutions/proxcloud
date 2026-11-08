package project

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

// A pause deletes the sandbox before its layer is assembled: a capture cut anywhere must be caught, a whole one not.
func TestCheckLayer(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := strings.Repeat("x", 100_000)
	_ = tw.WriteHeader(&tar.Header{Name: "home/user/a", Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()
	whole := buf.Bytes()
	if err := checkLayer(bytes.NewReader(whole)); err != nil {
		t.Fatalf("whole archive: %v", err)
	}
	for _, n := range []int{0, 5, len(whole) / 2, len(whole) - 4, len(whole) - 1} {
		if err := checkLayer(bytes.NewReader(whole[:n])); err == nil {
			t.Errorf("cut at %d of %d: accepted", n, len(whole))
		}
	}
	if err := checkLayer(strings.NewReader("not a gzip stream")); err == nil {
		t.Error("plain bytes accepted")
	}
}
