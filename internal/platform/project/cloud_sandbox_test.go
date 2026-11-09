package project

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// The sandbox error contract (sandboxHTTPError; docs/managed-sandbox.md §3.1).
func TestSandboxHTTPError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		ok     bool
	}{
		{"billing gate", errSandboxForbidden("the billing profile is suspended"), http.StatusForbidden, true},
		{"live quota", fmt.Errorf("%w: at most 10 sandboxes", sandbox.ErrQuotaExceeded), http.StatusTooManyRequests, true},
		{"not found", sandbox.ErrNotFound, http.StatusNotFound, true},
		{"resourcequota backstop", fmt.Errorf("sandbox: create: %w", &kamajik8s.APIError{Status: 403, Message: `sandboxes "sbx-1" is forbidden: exceeded quota: proxcloud-sandbox`}), http.StatusTooManyRequests, true},
		{"exec start failure", &kamajik8s.APIError{Status: 400, Message: "exec failed: executable file not found in $PATH"}, http.StatusBadRequest, true},
		{"k8s validation", &kamajik8s.APIError{Status: 422, Message: "spec.podTemplate: Invalid value"}, http.StatusBadRequest, true},
		{"rbac / other k8s", &kamajik8s.APIError{Status: 403, Message: "forbidden: User system:serviceaccount:proxcloud-system:proxcloud"}, http.StatusServiceUnavailable, true},
		{"cluster unreachable", fmt.Errorf("%w: dial tcp 10.0.0.1:6443: i/o timeout", kamajik8s.ErrUnreachable), http.StatusServiceUnavailable, true},
		{"anything else", errors.New("boom"), 0, false},
	}
	for _, c := range cases {
		status, msg, ok := sandboxHTTPError(c.err)
		if status != c.status || ok != c.ok || (ok && msg == "") {
			t.Errorf("%s: got (%d, %q, %v), want (%d, _, %v)", c.name, status, msg, ok, c.status, c.ok)
		}
	}
	// An RBAC/internal message must not leak to the customer.
	if _, msg, _ := sandboxHTTPError(&kamajik8s.APIError{Status: 403, Message: "forbidden: User system:serviceaccount:x"}); msg != "sandboxes are not available" {
		t.Errorf("internal 403 leaked: %q", msg)
	}
}
