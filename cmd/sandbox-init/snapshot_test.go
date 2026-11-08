package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestWriteSnapshot(t *testing.T) {
	root := t.TempDir()
	mk := func(p, body string) {
		full := filepath.Join(root, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	mk("etc/old.conf", "from the image")
	mk("proc/cpuinfo", "kernel")
	since := time.Now()
	time.Sleep(20 * time.Millisecond)
	mk("home/user/new.txt", "written after boot")
	mk("proc/self", "skip me")
	_ = os.Symlink("new.txt", filepath.Join(root, "home/user/link"))

	var buf bytes.Buffer
	if err := writeSnapshot(&buf, root, since, []string{"/proc"}); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b) + h.Linkname
	}
	var names []string
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if got["home/user/new.txt"] != "written after boot" || got["home/user/link"] != "new.txt" {
		t.Errorf("changed files missing: %v", names)
	}
	for _, n := range names {
		if n == "etc/old.conf" || n == "proc/self" || n == "proc/cpuinfo" {
			t.Errorf("%s must not be in the snapshot (unchanged or skipped): %v", n, names)
		}
	}
}
