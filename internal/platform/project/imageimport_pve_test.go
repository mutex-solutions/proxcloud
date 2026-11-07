package project

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

func TestValidateImageImport(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for _, c := range []struct {
		name, format, checksum, algorithm string
		ok                                bool
	}{
		{"debian-13", "qcow2", "", "", true},
		{"a", "vmdk", sum, "sha256", true},
		{"a", "qcow2", strings.Repeat("a", 128), "sha512", true},
		{"", "qcow2", "", "", false},
		{"-bad", "qcow2", "", "", false},
		{"has space", "qcow2", "", "", false},
		{"../x", "qcow2", "", "", false},
		{"a", "raw", "", "", false},
		{"a", "ova", "", "", false},
		{"a", "", "", "", false},
		{"a", "qcow2", sum, "", false},
		{"a", "qcow2", "", "sha256", false},
		{"a", "qcow2", sum, "md5", false},
		{"a", "qcow2", sum[:63] + "z", "sha256", false},
		{"a", "qcow2", sum, "sha512", false},
	} {
		if got := validateImageImport(c.name, c.format, c.checksum, c.algorithm) == nil; got != c.ok {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}

func TestImportAlso(t *testing.T) {
	also := importAlso([]string{"203.0.113.0/24", "not-a-cidr"})
	if !also(netip.MustParseAddr("203.0.113.9")) || !also(netip.MustParseAddr("::ffff:203.0.113.9")) || also(netip.MustParseAddr("198.51.100.1")) {
		t.Fatal("the operator's networks are refused, and only those")
	}
}

func TestPickImportNode(t *testing.T) {
	disk := map[string]pve.StorageNodeState{"a": {Avail: 5}, "b": {Avail: 9}, "c": {Avail: 9}, "d": {Avail: 99}}
	imp := pve.StorageCapability{Nodes: map[string]pve.StorageNodeState{"a": {Active: true}, "b": {Active: true}, "c": {Active: true}, "d": {Active: false}}}
	if got := pickImportNode(disk, imp); got != "b" {
		t.Fatalf("the most room among nodes with both stores, by name on a tie: %q", got)
	}
	if got := pickImportNode(map[string]pve.StorageNodeState{"d": {Avail: 1}}, imp); got != "" {
		t.Fatalf("no node with both: %q", got)
	}
}

func TestSpoolImage(t *testing.T) {
	f, n, err := spoolImage(strings.NewReader("abcd"), 4)
	if err != nil || f == nil || n != 4 {
		t.Fatalf("%v %d %v", f, n, err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != "abcd" {
		t.Fatalf("read back from the start: %q", got)
	}
	f2, n, err := spoolImage(strings.NewReader("abcde"), 4)
	if f2 != nil || err != nil || n != 5 {
		t.Fatalf("over the limit: %v %d %v", f2, n, err)
	}
}

// A source that goes silent or drips ends the fetch, over a real connection, so the upload lock comes free.
func TestImportBodyEndsASlowSource(t *testing.T) {
	stall, grace := importStall, importGrace
	t.Cleanup(func() { importStall, importGrace = stall, grace })
	origin := func(send func(http.ResponseWriter, http.Flusher, <-chan struct{})) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			send(w, w.(http.Flusher), r.Context().Done())
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	read := func(url string) (time.Duration, error) {
		ctx, end := context.WithCancelCause(context.Background())
		defer end(nil)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		start := time.Now()
		_, err = io.Copy(io.Discard, newImportBody(res.Body, end))
		if err != nil && !errors.Is(context.Cause(ctx), errImportSourceSlow) {
			t.Fatalf("the cause must say why: %v", context.Cause(ctx))
		}
		return time.Since(start), err
	}
	const limit = 2 * time.Second // far below what the handlers below would last

	importStall, importGrace = 100*time.Millisecond, time.Hour
	took, err := read(origin(func(w http.ResponseWriter, f http.Flusher, gone <-chan struct{}) {
		w.Write([]byte("some"))
		f.Flush()
		<-gone
	}))
	if err == nil || took > limit {
		t.Fatalf("silent: %v after %v", err, took)
	}

	// Every byte in time for the stall, nothing like the floor.
	importStall, importGrace = time.Minute, 100*time.Millisecond
	took, err = read(origin(func(w http.ResponseWriter, f http.Flusher, gone <-chan struct{}) {
		for {
			select {
			case <-gone:
				return
			case <-time.After(10 * time.Millisecond):
				w.Write([]byte("x"))
				f.Flush()
			}
		}
	}))
	if err == nil || took > limit {
		t.Fatalf("drip: %v after %v", err, took)
	}

	importStall, importGrace = time.Minute, time.Minute
	if _, err := read(origin(func(w http.ResponseWriter, _ http.Flusher, _ <-chan struct{}) { w.Write([]byte("all of it")) })); err != nil {
		t.Fatalf("a source that keeps up: %v", err)
	}

	// Time outside a read is PVE's, not the source's.
	importStall, importGrace = 50*time.Millisecond, 50*time.Millisecond
	ctx, end := context.WithCancelCause(context.Background())
	defer end(nil)
	b := newImportBody(strings.NewReader("abcd"), end)
	for {
		if _, err := b.Read(make([]byte, 1)); err != nil {
			break
		}
		time.Sleep(60 * time.Millisecond)
	}
	if context.Cause(ctx) != nil {
		t.Fatalf("a slow consumer is not a slow source: %v", context.Cause(ctx))
	}
}

func TestImagesCountInQuota(t *testing.T) {
	rows := []cloud.CloudResource{
		{ServiceID: "s", Type: cloud.TypeVolume, Data: map[string]any{"volume": map[string]any{"size": 10}}},
		{ServiceID: "s", Type: cloud.TypeImage, Data: map[string]any{"image": map[string]any{"size": int64(1 << 30)}}},
		{ServiceID: "s", Type: cloud.TypeImage, Data: map[string]any{"image": map[string]any{"size": float64(5<<30 + 1)}}},
		{ServiceID: "other", Type: cloud.TypeImage, Data: map[string]any{"image": map[string]any{"size": int64(50 << 30)}}},
	}
	if got := pveUsageOf(rows, "s").DiskGb; got != 17 {
		t.Fatalf("10 GB of volumes, 1 and 6 of images: %d", got)
	}
}
