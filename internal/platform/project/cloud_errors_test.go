package project

import (
	"errors"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func TestCloudQuotaConflict(t *testing.T) {
	providerErr := gophercloud.ErrUnexpectedResponseCode{
		Actual: http.StatusForbidden,
		Body:   []byte(`{"forbidden":{"code":403,"message":"Quota exceeded for cores: requested 4"}}`),
	}
	got := cloudQuotaConflict(providerErr)
	var httpErr *httpx.HTTPError
	if !errors.As(got, &httpErr) {
		t.Fatalf("cloudQuotaConflict() = %T, want *httpx.HTTPError", got)
	}
	if httpErr.Status != http.StatusConflict || httpErr.Msg != "Quota exceeded for cores: requested 4" {
		t.Fatalf("cloudQuotaConflict() = %+v", httpErr)
	}
}

func TestCloudQuotaConflictDoesNotRewritePermissionErrors(t *testing.T) {
	err := gophercloud.ErrUnexpectedResponseCode{
		Actual: http.StatusForbidden,
		Body:   []byte(`{"forbidden":{"message":"Policy does not allow this operation"}}`),
	}
	if got := cloudQuotaConflict(err); got != nil {
		t.Fatalf("cloudQuotaConflict() = %v, want nil", got)
	}
}

func TestCloudQuotaConflictRecognizesCinderLimit(t *testing.T) {
	err := gophercloud.ErrUnexpectedResponseCode{
		Actual: http.StatusRequestEntityTooLarge,
		Body:   []byte(`{"overLimit":{"message":"VolumeLimitExceeded: maximum number of volumes exceeded"}}`),
	}
	if got := cloudQuotaConflict(err); got == nil {
		t.Fatal("cloudQuotaConflict() = nil, want quota conflict")
	}
}

func TestCloudQuotaConflictNonJSONBodyUsesFallbackMessage(t *testing.T) {
	err := gophercloud.ErrUnexpectedResponseCode{
		Actual: http.StatusForbidden,
		Body:   []byte(`<html>Quota exceeded</html>`),
	}
	got := cloudQuotaConflict(err)
	var httpErr *httpx.HTTPError
	if !errors.As(got, &httpErr) {
		t.Fatalf("cloudQuotaConflict() = %T, want *httpx.HTTPError", got)
	}
	if httpErr.Status != http.StatusConflict || httpErr.Msg == "" {
		t.Fatalf("cloudQuotaConflict() = %+v, want 409 with fallback message", httpErr)
	}
}

func TestCloudQuotaConflictHandles409(t *testing.T) {
	err := gophercloud.ErrUnexpectedResponseCode{
		Actual: http.StatusConflict,
		Body:   []byte(`{"conflictingRequest":{"message":"LimitExceeded: too many instances"}}`),
	}
	if got := cloudQuotaConflict(err); got == nil {
		t.Fatal("cloudQuotaConflict() = nil, want quota conflict")
	}
}

func TestCloudQuotaConflictIgnoresWrappedNonHTTPErrors(t *testing.T) {
	if got := cloudQuotaConflict(errors.New("quota exceeded")); got != nil {
		t.Fatalf("cloudQuotaConflict() = %v, want nil for non-gophercloud error", got)
	}
}
