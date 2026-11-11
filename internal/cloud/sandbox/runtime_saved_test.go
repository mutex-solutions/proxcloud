package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func savedAdmissionConfig() Config {
	return Config{TemplateRegistry: "registry.example/t", Templates: map[string]Template{"external": {Image: "external.example/library/python:3.12", CPU: 1, MemoryGiB: 1}, "library": {Image: "registry.example/library/python:3.12", CPU: 1, MemoryGiB: 1}}}
}
func TestSavedAdmissionCurrentCuratedImageOnly(t *testing.T) {
	cfg := savedAdmissionConfig()
	if SavedLifecycleAvailable(cfg) {
		t.Fatal("saved capability must remain false")
	}
	for _, image := range []string{cfg.Templates["external"].Image, cfg.Templates["library"].Image} {
		if err := CheckSavedRuntimeImage(cfg, "p1", image); err != nil {
			t.Fatal(err)
		}
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, image := range []string{"", "python:3.12", "external.example/library/other:tag", "registry.example/t/p1/0123456789abcdef0123@" + digest, "registry.example/t/p2/0123456789abcdef0123@" + digest, "registry.example/unknown/python:tag", "https://external.example/library/python:3.12", "user:password@external.example/library/python:3.12", "external.example/library/python:3.12?token=secret", "Registry.example/library/python:3.12", "registry.example:443/library/python:3.12", "registry.example/library/python:tag@" + digest} {
		v := cfg
		v.Templates = map[string]Template{"attempt": {Image: image}}
		// Even listing a managed/malformed image in current Config is not an override;
		// an unknown external image is denied against the unmodified current catalog.
		if strings.Contains(image, "/t/") || strings.Contains(image, "?") || strings.Contains(image, "@external") || strings.HasPrefix(image, "https") || strings.Contains(image, ":443/") || strings.HasPrefix(image, "Registry") || strings.Contains(image, ":tag@") || image == "" || image == "python:3.12" || strings.Contains(image, "/unknown/") {
			if !errors.Is(CheckSavedRuntimeImage(v, "p1", image), ErrManagedSavedStartHeld) {
				t.Fatalf("admitted invalid configured image %q", image)
			}
		} else if !errors.Is(CheckSavedRuntimeImage(cfg, "p1", image), ErrManagedSavedStartHeld) {
			t.Fatalf("admitted unconfigured image %q", image)
		}
	}
	for _, reg := range []string{"", "https://registry.example/t", "registry.example/t/", "registry.example/other"} {
		v := cfg
		v.TemplateRegistry = reg
		if !errors.Is(CheckSavedRuntimeImage(v, "p1", cfg.Templates["external"].Image), ErrManagedSavedStartHeld) {
			t.Fatal("invalid/missing registry policy allowed")
		}
	}
}
func TestSavedAdmissionRefusalDoesNotTouchSavedMapOrAPI(t *testing.T) {
	cfg := savedAdmissionConfig()
	cfg.Templates["managed"] = Template{Image: "registry.example/t/p1/0123456789abcdef0123@sha256:" + strings.Repeat("a", 64), CPU: 1, MemoryGiB: 1}
	now := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewWithAPI(nil, cfg, "svc")
	s.now = func() time.Time { return now }
	for _, mode := range []string{"managed", "unknown", "cancelled", "scope", "expired"} {
		t.Run(mode, func(t *testing.T) {
			saved := BuildCR(cfg, "svc", Spec{ID: "sbx-0123456789abcdef", ProjectID: "p1", Template: "external", CPU: 1, MemoryGiB: 1}, now.Add(time.Hour))
			md := saved["metadata"].(map[string]any)
			ann := md["annotations"].(map[string]any)
			ann["proxcloud.io/curated-template"] = "true"
			ann["proxcloud.io/runtime-operation"] = "copied-create-authority"
			before, _ := json.Marshal(saved)
			ctx := context.Background()
			project := "p1"
			image := cfg.Templates["managed"].Image
			end := now.Add(time.Hour)
			want := ErrManagedSavedStartHeld
			switch mode {
			case "unknown":
				image = "external.example/library/unknown:tag"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "scope":
				project = "p2"
				want = ErrNotFound
			case "expired":
				end = now
				want = context.DeadlineExceeded
			}
			if e := s.ResumeSandbox(ctx, project, saved, image, end, nil); !errors.Is(e, want) {
				t.Fatal(e, want)
			}
			after, _ := json.Marshal(saved)
			if string(before) != string(after) {
				t.Fatal("refused saved runtime mutated map")
			}
		})
	}
}
func TestSavedAdmissionCuratedCompatibilityRetainsQuotaAndImage(t *testing.T) {
	cfg := savedAdmissionConfig()
	f := newFake()
	s := NewWithAPI(f, cfg, "svc")
	now := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	for _, name := range []string{"external", "library"} {
		saved := BuildCR(cfg, "svc", Spec{ID: NewSandboxID(), ProjectID: "p1", Template: name, CPU: 1, MemoryGiB: 1}, now.Add(time.Hour))
		if err := s.ResumeSandbox(context.Background(), "p1", saved, cfg.Templates[name].Image, now.Add(time.Hour), nil); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, obj := range f.crs {
		if obj["kind"] == "Sandbox" {
			n++
		}
	}
	if n != 2 {
		t.Fatal("curated compatibility missing", n)
	}
}
