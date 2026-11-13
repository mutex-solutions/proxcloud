package providerattach

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDecodePayload(t *testing.T) {
	if _, err := DecodePayload([]byte(`{"version":1,"runId":"r-1","serviceId":"svc"}`)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{}`, `{"version":2,"runId":"r","serviceId":"s"}`, `{"version":1,"runId":"","serviceId":"s"}`,
		`{"version":1,"runId":"r","serviceId":"s","extra":1}`, `{"version":1,"runId":"r","serviceId":"a b"}`, `nope`} {
		if _, err := DecodePayload([]byte(bad)); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}

func TestNormalizeScope(t *testing.T) {
	s, err := NormalizeScope(Scope{Mode: ScopeProjects, ProjectIDs: []string{"p2", "p1", "p2"}})
	if err != nil || strings.Join(s.ProjectIDs, ",") != "p1,p2" {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err := NormalizeScope(Scope{Mode: ScopeAll}); err != nil || s.Mode != ScopeAll {
		t.Fatalf("%+v %v", s, err)
	}
	many := make([]string, MaxScopeIDs+1)
	for i := range many {
		many[i] = fmt.Sprintf("p%d", i)
	}
	for _, bad := range []Scope{
		{Mode: ""}, {Mode: ScopeRetry, SourceRunID: "r"}, {Mode: ScopeAll, ProjectIDs: []string{"p"}},
		{Mode: ScopeProjects}, {Mode: ScopeOrganizations}, {Mode: ScopeProjects, ProjectIDs: []string{"bad id"}},
		{Mode: ScopeOrganizations, OrganizationIDs: []string{"o"}, ProjectIDs: []string{"p"}},
		{Mode: ScopeProjects, ProjectIDs: many},
	} {
		if _, err := NormalizeScope(bad); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
	for _, n := range []int{-1, 5, 100} {
		if _, err := NormalizeConcurrency(n); err == nil {
			t.Fatalf("concurrency %d accepted", n)
		}
	}
	if n, err := NormalizeConcurrency(0); err != nil || n != DefaultWorkers {
		t.Fatalf("default: %d %v", n, err)
	}
}

// Only fixed codes are ever stored: a provider's error text (which may carry an endpoint or a credential) never is.
func TestClassifyStoresFixedCodesOnly(t *testing.T) {
	secret := errors.New("POST https://admin:hunter2@rgw.example/admin/user: 403 SignatureDoesNotMatch")
	for _, tc := range []struct {
		outcome   string
		err       error
		timedOut  bool
		wantState string
		wantCode  string
	}{
		{OutcomeAttached, nil, false, ItemAttached, CodeAttached},
		{OutcomeAlreadyAttached, nil, false, ItemSkipped, CodeAlreadyAttached},
		{OutcomeDeleting, nil, false, ItemSkipped, CodeDeleting},
		{OutcomeNotFound, nil, false, ItemSkipped, CodeNotFound},
		{"", secret, false, ItemFailed, CodeProviderError},
		{"", secret, true, ItemFailed, CodeTimeout},
		{"", context.DeadlineExceeded, false, ItemFailed, CodeTimeout},
		{"weird", nil, false, ItemFailed, CodeProviderError},
	} {
		state, code := classify(tc.outcome, tc.err, tc.timedOut)
		if state != tc.wantState || code != tc.wantCode {
			t.Fatalf("%+v: got %s/%s", tc, state, code)
		}
		if strings.Contains(code, "hunter2") || strings.Contains(code, " ") {
			t.Fatalf("code leaks text: %q", code)
		}
	}
}

func TestTerminal(t *testing.T) {
	for _, s := range []string{RunSucceeded, RunCompletedWithFailures, RunCancelled, RunFailed} {
		if !Terminal(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{RunQueued, RunRunning, RunCancelling} {
		if Terminal(s) {
			t.Fatal(s)
		}
	}
}
