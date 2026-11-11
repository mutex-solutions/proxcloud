package registrypins

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func snapshotTestSpec() SnapshotSpec {
	return SnapshotSpec{ServiceID: "service1", SandboxID: "sbx-0123456789abcdef", SourceCRUID: "source-cr-uid", SourcePodUID: "source-pod-uid", SourceFingerprint: strings.Repeat("a", 64), ProviderRevisionHash: strings.Repeat("b", 64), Base: importTestSource, Destination: importTestOtherRepo + ":snapshot1", ActionSpecFingerprint: strings.Repeat("c", 64)}
}
func TestSnapshotRepresentationAndFixedEnvelope(t *testing.T) {
	s := &Store{host: importTestHost}
	spec := snapshotTestSpec()
	for _, base := range []string{spec.Base, importTestExternal, importTestHost + "/library/base@" + importTestDigest} {
		v := spec
		v.Base = base
		r, e := s.snapshotReferences("project1", v)
		want := 1
		if base == spec.Base {
			want = 2
		}
		if e != nil || len(r.need) != want || !r.need[v.Destination] {
			t.Fatal("exact roles", e)
		}
	}
	for _, base := range []string{"", "ubuntu@" + importTestDigest, "https://" + importTestExternal, "user:password@" + importTestExternal, importTestExternal + "?token=secret", importTestExternal + "#secret", " " + importTestExternal, "external.example/library/base:tag@" + importTestDigest, "external.example/library/base:tag", "External.example/library/base@" + importTestDigest, "external.example:443/library/base@" + importTestDigest, "external.example:05000/library/base@" + importTestDigest, "external.example/library//base@" + importTestDigest, "external.example/../base@" + importTestDigest, "external.example/library/%2e%2e@" + importTestDigest, importTestHost + "/other/base@" + importTestDigest, strings.Replace(spec.Base, "project1", "project2", 1), importTestHost + "/t/project1/garbage@" + importTestDigest, strings.Replace(spec.Base, "sha256:", "sha512:", 1), strings.Replace(spec.Base, strings.Repeat("a", 64), strings.Repeat("A", 64), 1)} {
		v := spec
		v.Base = base
		if _, e := s.snapshotReferences("project1", v); e == nil {
			t.Fatalf("invalid BASE admitted %q", base)
		}
	}
	for _, change := range []func(*SnapshotSpec){func(v *SnapshotSpec) { v.ServiceID = "" }, func(v *SnapshotSpec) { v.SandboxID = "other" }, func(v *SnapshotSpec) { v.SourceCRUID = "x?secret" }, func(v *SnapshotSpec) { v.SourcePodUID = "" }, func(v *SnapshotSpec) { v.SourceFingerprint = "" }, func(v *SnapshotSpec) { v.ProviderRevisionHash = strings.Repeat("A", 64) }, func(v *SnapshotSpec) { v.ActionSpecFingerprint = "bad" }, func(v *SnapshotSpec) { v.Destination = importTestOtherRepo + "@" + importTestDigest }, func(v *SnapshotSpec) { v.Destination = strings.Replace(v.Destination, "project1", "project2", 1) }, func(v *SnapshotSpec) { v.Destination = "https://" + v.Destination }, func(v *SnapshotSpec) {
		v.Destination = strings.Replace(v.Destination, importTestHost, importTestHost+":443", 1)
	}} {
		v := spec
		change(&v)
		if _, e := s.snapshotReferences("project1", v); e == nil {
			t.Fatal("invalid envelope admitted")
		}
	}
}
func TestSnapshotProofValueCopiesOneUseAndRedaction(t *testing.T) {
	spec := snapshotTestSpec()
	p := SnapshotProof{op: Operation{ID: "operation", ProjectID: "project1", Generation: "nonce"}, spec: spec, state: &proofState{}}
	bad := spec
	bad.ProviderRevisionHash = strings.Repeat("d", 64)
	if _, _, e := p.Consume("project1", bad); e != ErrConflict {
		t.Fatal("mismatch consumed")
	}
	if _, _, e := p.Consume("other", spec); e != ErrConflict {
		t.Fatal("project mismatch")
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		v := p
		wg.Go(func() {
			id, n, e := v.Consume("project1", spec)
			if e == nil {
				if id != "operation" || n != "nonce" {
					t.Error("identity drift")
				}
				wins.Add(1)
			} else if e != ErrConflict {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("copies", wins.Load())
	}
	for _, v := range []*SnapshotProof{nil, {}} {
		if _, _, e := v.Consume("project1", spec); e != ErrConflict {
			t.Fatal("zero proof")
		}
	}
	e := SnapshotPodEvidence{Spec: spec, UID: "PRIVATE_UID"}
	var log bytes.Buffer
	slog.New(slog.NewJSONHandler(&log, nil)).Info("snapshot", "spec", spec, "proof", p, "evidence", e)
	for _, out := range []string{fmt.Sprintf("%v %+v %#v", spec, spec, &spec), fmt.Sprintf("%v %+v %#v", p, &p, p), fmt.Sprintf("%v %+v %#v", e, &e, e), log.String()} {
		for _, secret := range []string{spec.Base, spec.Destination, spec.SourceFingerprint, spec.ProviderRevisionHash, spec.SourceCRUID, "PRIVATE_UID", "operation", "nonce"} {
			if strings.Contains(out, secret) {
				t.Fatal("snapshot diagnostic leak")
			}
		}
	}
}
