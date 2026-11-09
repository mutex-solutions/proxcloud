package registrypins

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const importTestHost = "registry.internal"
const importTestRepo = importTestHost + "/t/project1/0123456789abcdef0123"
const importTestOtherRepo = importTestHost + "/t/project1/0123456789abcdef0124"

var importTestDigest = "sha256:" + strings.Repeat("a", 64)
var importTestSource = importTestRepo + "@" + importTestDigest
var importTestExternal = "external.example:5000/library/base@" + importTestDigest

func TestImportReferencesRepresentation(t *testing.T) {
	s := &Store{host: importTestHost}
	for _, tc := range []struct {
		name, source, destination, kind string
		pins                            []string
	}{
		{"verify", importTestSource, importTestRepo, "verify", []string{importTestSource}},
		{"external-copy", importTestExternal, importTestOtherRepo + ":build1", "copy", []string{importTestOtherRepo + ":build1"}},
		{"managed-host-library", importTestHost + "/library/base@" + importTestDigest, importTestOtherRepo + ":build1", "copy", []string{importTestOtherRepo + ":build1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := s.importReferences("project1", tc.source, tc.destination)
			if err != nil || r.kind != tc.kind || len(r.need) != len(tc.pins) {
				t.Fatalf("kind=%q need=%v err=%v", r.kind, r.need, err)
			}
			for _, pin := range tc.pins {
				if !r.need[pin] {
					t.Fatalf("missing %s", pin)
				}
			}
		})
	}
	for _, tc := range []struct{ source, destination string }{
		{strings.Replace(importTestSource, "/project1/", "/project2/", 1), importTestOtherRepo + ":build1"},
		{strings.Replace(importTestSource, "/project1/", "/project2/", 1), strings.Replace(importTestRepo, "/project1/", "/project2/", 1)},
		{importTestHost + "/t/project1/garbage@" + importTestDigest, importTestOtherRepo + ":build1"},
		{importTestRepo + ":mutable", importTestRepo},
		{importTestSource, importTestSource},
		{importTestSource, importTestRepo + ":overwrite"},
		{importTestSource, importTestOtherRepo + ":build1"},
		{importTestExternal, importTestOtherRepo},
		{importTestExternal, importTestOtherRepo + "@" + importTestDigest},
		{importTestExternal, strings.Replace(importTestOtherRepo, "project1", "project2", 1) + ":build1"},
	} {
		if _, err := s.importReferences("project1", tc.source, tc.destination); err == nil {
			t.Errorf("accepted source=%q destination=%q", tc.source, tc.destination)
		}
	}
}

func TestImportSourceRejectsNonCanonicalOrCredentialRefs(t *testing.T) {
	s := &Store{host: importTestHost}
	for _, source := range []string{
		"", "ubuntu@" + importTestDigest, "https://" + importTestExternal, "http://" + importTestExternal,
		"user:password@" + importTestExternal, "user@" + importTestExternal,
		importTestExternal + "?credential=secret", importTestExternal + "#fragment", " " + importTestExternal,
		"external.example/library/base:tag@" + importTestDigest, "external.example/library/base:tag",
		"External.example/library/base@" + importTestDigest, "external.example:443/library/base@" + importTestDigest,
		"external.example:05000/library/base@" + importTestDigest, "[::1]/library/base@" + importTestDigest,
		"external.example/library//base@" + importTestDigest, "external.example/../base@" + importTestDigest,
		"external.example/Library/base@" + importTestDigest, "external.example/library/base@sha256:" + strings.Repeat("A", 64),
		"external.example/library/base@sha512:" + strings.Repeat("a", 128), strings.Repeat("a", 513),
	} {
		if _, err := s.importReferences("project1", source, importTestOtherRepo+":build1"); err == nil {
			t.Errorf("accepted %q", source)
		}
	}
	_, err := s.importReferences("project1", strings.Replace(importTestSource, "project1", "project2", 1), importTestOtherRepo+":build1")
	if !errors.Is(err, ErrScope) {
		t.Fatal("foreign managed source must remain scope failure, not external", err)
	}
}

func TestImportProofValueCopiesAreOneShot(t *testing.T) {
	p := &ImportProof{operation: "operation", nonce: "nonce", project: "project1", source: importTestSource, destination: importTestRepo, state: &proofState{}}
	for _, args := range [][3]string{{"project2", importTestSource, importTestRepo}, {"project1", importTestExternal, importTestRepo}, {"project1", importTestSource, importTestRepo + ":tag"}} {
		if _, _, err := p.Consume(args[0], args[1], args[2]); !errors.Is(err, ErrConflict) {
			t.Fatal("mismatch consumed", err)
		}
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		clone := *p
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, nonce, err := clone.Consume("project1", importTestSource, importTestRepo)
			if err == nil {
				if op != "operation" || nonce != "nonce" {
					t.Error("frozen identity lost")
				}
				wins.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("value-copy winners=%d", wins.Load())
	}
	for _, invalid := range []*ImportProof{nil, {}, {state: &proofState{}}} {
		if _, _, err := invalid.Consume("project1", importTestSource, importTestRepo); !errors.Is(err, ErrConflict) {
			t.Fatal("zero proof consumed", err)
		}
	}
}
