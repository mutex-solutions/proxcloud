package registrypins

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func runtimeTestSpec() RuntimeSpec {
	return RuntimeSpec{ServiceID: "0123456789abcdef01234567", SandboxID: "sbx-0123456789abcdef", Mode: RuntimeCreate, Image: importTestSource, Fingerprint: strings.Repeat("b", 64)}
}

func TestRuntimeSpecStrictManagedDigestAndIdentity(t *testing.T) {
	s := &Store{host: importTestHost}
	for _, mode := range []RuntimeMode{RuntimeCreate, RuntimeResume, RuntimeFork} {
		for _, sandbox := range []string{"sbx-01234567", "sbx-0123456789abcdef"} {
			spec := runtimeTestSpec()
			spec.Mode, spec.SandboxID = mode, sandbox
			if _, err := s.runtimeReference("project1", spec); err != nil {
				t.Fatal(spec, err)
			}
		}
	}
	mutations := []func(*RuntimeSpec){
		func(s *RuntimeSpec) { s.ServiceID = "" },
		func(s *RuntimeSpec) { s.ServiceID = "Service" },
		func(s *RuntimeSpec) { s.ServiceID = "service/id" },
		func(s *RuntimeSpec) { s.ServiceID = "user@service" },
		func(s *RuntimeSpec) { s.ServiceID = strings.Repeat("a", 129) },
		func(s *RuntimeSpec) { s.SandboxID = "sbx-012345678" },
		func(s *RuntimeSpec) { s.SandboxID = "sbx-0123456789ABCDEF" },
		func(s *RuntimeSpec) { s.SandboxID = "sbx-not-real" },
		func(s *RuntimeSpec) { s.Mode = "connect" },
		func(s *RuntimeSpec) { s.Mode = "" },
		func(s *RuntimeSpec) { s.Fingerprint = strings.Repeat("A", 64) },
		func(s *RuntimeSpec) { s.Fingerprint = strings.Repeat("a", 63) },
		func(s *RuntimeSpec) { s.Fingerprint = "sha256:" + strings.Repeat("a", 64) },
	}
	for _, image := range []string{"", importTestExternal, importTestHost + "/library/base@" + importTestDigest, importTestRepo, importTestRepo + ":ready", importTestRepo + ":ready@" + importTestDigest, importTestSource + "@" + importTestDigest, "https://" + importTestSource, "user@" + importTestSource, " " + importTestSource, strings.Replace(importTestSource, "registry.internal", "Registry.Internal", 1), strings.Replace(importTestSource, "registry.internal", "registry.internal:443", 1), strings.Replace(importTestSource, "project1", "project2", 1), strings.Replace(importTestSource, "0123456789abcdef0123", "invalid", 1)} {
		mutations = append(mutations, func(s *RuntimeSpec) { s.Image = image })
	}
	for i, mutate := range mutations {
		spec := runtimeTestSpec()
		mutate(&spec)
		if _, err := s.runtimeReference("project1", spec); err == nil {
			t.Errorf("accepted mutation %d: %+v", i, spec)
		}
	}
}

func TestRuntimeProofFrozenFieldsAndValueCopyOnce(t *testing.T) {
	spec := runtimeTestSpec()
	p := &RuntimeProof{op: Operation{ID: "operation", Generation: "nonce", ProjectID: "project1"}, spec: spec, state: &proofState{}}
	for _, mutate := range []func(*RuntimeSpec){
		func(s *RuntimeSpec) { s.ServiceID += "1" },
		func(s *RuntimeSpec) { s.SandboxID = "sbx-abcdef01" },
		func(s *RuntimeSpec) { s.Mode = RuntimeResume },
		func(s *RuntimeSpec) { s.Image = importTestOtherRepo + "@" + importTestDigest },
		func(s *RuntimeSpec) { s.Fingerprint = strings.Repeat("c", 64) },
	} {
		wrong := spec
		mutate(&wrong)
		if _, _, err := p.Consume("project1", wrong); !errors.Is(err, ErrConflict) || p.state.used.Load() {
			t.Fatal("mismatch consumed matching envelope", err)
		}
	}
	if _, _, err := p.Consume("project2", spec); !errors.Is(err, ErrConflict) || p.state.used.Load() {
		t.Fatal("wrong project consumed envelope", err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		clone := *p
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, nonce, err := clone.Consume("project1", spec)
			if err == nil {
				if op != "operation" || nonce != "nonce" {
					t.Error("lost frozen identity")
				}
				wins.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("copy consume winners", wins.Load())
	}
	for _, invalid := range []*RuntimeProof{nil, {}} {
		if _, _, err := invalid.Consume("project1", spec); !errors.Is(err, ErrConflict) {
			t.Fatal("zero proof consumed", err)
		}
	}
}

func TestRuntimeEvidenceIsActualPodPhaseNotCRCandidate(t *testing.T) {
	s := &Store{host: importTestHost}
	op := importPGOperation()
	e := RuntimePodEvidence{UID: "pod-uid", CRUID: "cr-uid", Operation: op.ID, Nonce: op.Generation, Spec: runtimeTestSpec()}
	for _, phase := range []string{"Running", "Succeeded", "Failed"} {
		e.Phase = phase
		if !s.validRuntimePod(op, e) {
			t.Fatal("valid phase rejected", phase)
		}
	}
	for _, phase := range []string{"", "Pending", "Ready", "ReadyCandidate", "Unknown", "NotFound", "Deleted", "DeadlineExceeded", "204"} {
		e.Phase = phase
		if s.validRuntimePod(op, e) {
			t.Fatal("non-pod or nonpositive phase accepted", phase)
		}
	}
	e.Phase, e.CRUID = "Running", ""
	if s.validRuntimePod(op, e) {
		t.Fatal("CR UID required independently of pod UID")
	}
}
