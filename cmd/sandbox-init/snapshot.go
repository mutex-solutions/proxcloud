package main

// snapshot.go — `sandbox-init -snapshot`: writes a gzip tar of every file, directory and symlink
// created or changed since the sandbox booted to stdout. The portal runs it over exec when a sandbox
// is paused or snapshotted, and appends the archive as one layer on top of the sandbox's image.
//
// "Changed" = ctime after the boot marker (sandbox-init writes it before anything else runs).
// Files deleted from the image are not recorded (no whiteouts): they come back after a resume.

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const bootPath = "/run/proxcloud/boot"

// snapshotSkip are never part of a snapshot: kernel and runtime filesystems, the platform's own mount,
// and the per-boot run state.
var snapshotSkip = []string{"/proc", "/sys", "/dev", "/run", "/.proxcloud", "/var/run", nativeBootPath}

func markBoot() {
	_ = os.MkdirAll(filepath.Dir(bootPath), 0o755)
	_ = os.WriteFile(bootPath, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o644)
}

func bootTime() time.Time {
	for _, p := range []string{nativeBootPath, bootPath} {
		b, err := os.ReadFile(p)
		if err == nil {
			if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

func skipped(p string, skip []string) bool {
	for _, s := range skip {
		if p == s || strings.HasPrefix(p, s+"/") {
			return true
		}
	}
	return false
}

func ctimeOf(fi fs.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(int64(st.Ctim.Sec), int64(st.Ctim.Nsec))
	}
	return fi.ModTime()
}

// writeSnapshot archives entries under root changed after since. Paths in the archive are
// relative to root ("home/user/x"), as an image layer expects.
func writeSnapshot(w io.Writer, root string, since time.Time, skip []string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		abs := "/" + strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		if abs != "/" && skipped(abs, skip) {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err != nil {
			return err
		} // unreadable data must fail capture, never silently lose files
		if abs == "/" {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !ctimeOf(fi).After(since) {
			return nil
		}
		mode := fi.Mode()
		if !mode.IsRegular() && !mode.IsDir() && mode&fs.ModeSymlink == 0 {
			return nil // sockets, fifos, devices: not part of a filesystem snapshot
		}
		link := ""
		if mode&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = strings.TrimPrefix(abs, "/")
		if mode.IsDir() {
			hdr.Name += "/"
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			hdr.Uid, hdr.Gid = int(st.Uid), int(st.Gid)
		}
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if mode.IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.CopyN(tw, f, hdr.Size)
			f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func runSnapshot() int {
	since := bootTime()
	if since.IsZero() {
		os.Stderr.WriteString("no boot marker: this sandbox was not started by sandbox-init\n")
		return 2
	}
	if err := writeSnapshot(os.Stdout, "/", since, snapshotSkip); err != nil {
		os.Stderr.WriteString("snapshot: " + err.Error() + "\n")
		return 1
	}
	return 0
}
