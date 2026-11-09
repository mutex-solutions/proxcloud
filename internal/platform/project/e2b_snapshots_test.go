package project

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// A capture cut anywhere must be caught, a whole one not; the size counted is the archive's (it is billed).
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
	if n, err := checkLayer(bytes.NewReader(whole)); err != nil || n != int64(len(whole)) {
		t.Fatalf("whole archive: %d of %d, %v", n, len(whole), err)
	}
	for _, n := range []int{0, 5, len(whole) / 2, len(whole) - 4, len(whole) - 1} {
		if _, err := checkLayer(bytes.NewReader(whole[:n])); err == nil {
			t.Errorf("cut at %d of %d: accepted", n, len(whole))
		}
	}
	if _, err := checkLayer(strings.NewReader("not a gzip stream")); err == nil {
		t.Error("plain bytes accepted")
	}
}

// A snapshot taken again keeps only its tagged ready build (and any still building or failed), so the image billed is
// the image kept; an ordinary template keeps its older ready builds.
func TestTrimBuildsSnapshot(t *testing.T) {
	mk := func(kind string) *sandboxTemplateDoc {
		return &sandboxTemplateDoc{Kind: kind, Tags: map[string]string{"default": "b2"}, Builds: []templateBuild{
			{BuildID: "b1", Status: sandbox.BuildReady}, {BuildID: "b2", Status: sandbox.BuildReady},
			{BuildID: "b3", Status: sandbox.BuildError}, {BuildID: "b4", Status: sandbox.BuildBuilding}}}
	}
	s := mk("snapshot")
	trimBuilds(s)
	if len(s.Builds) != 3 || s.Builds[0].BuildID != "b2" {
		t.Fatalf("snapshot kept %+v", s.Builds)
	}
	tp := mk("")
	trimBuilds(tp)
	if len(tp.Builds) != 4 {
		t.Fatalf("template kept %+v", tp.Builds)
	}
}
