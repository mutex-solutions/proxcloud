package billingclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// TestChargeRoundTripsThroughAServer drives the client against a stub billing service and asserts
// the server sees exactly what was sent — in particular that an integer above 2^53 and a decimal
// arrive intact. This is the client-side half of the guarantee the billing service's e2e suite
// proves on its own side.
func TestChargeRoundTripsThroughAServer(t *testing.T) {
	const exact = int64(9007199254740993)

	var gotPath, gotContentType string
	var received ChargeRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		// Decode the way the real service does.
		if err := billingapi.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("server decode: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"profileId":"bp-1","charged":true,"servicesCharged":1}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", 5*time.Second)
	resp, err := c.Charge(context.Background(), ChargeRequest{
		ProfileID: "bp-1",
		TimeUnit:  "minute",
		Services: []ServiceResources{{
			ServiceID: "svc-x",
			Resources: []*billingapi.BillingResource{{
				ResourceID: "r-1",
				Values: map[string]any{
					"usage_bytes": exact,
					"ram_gb":      decimal.RequireFromString("1.5"),
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	if gotPath != "/cloud/internal/v1/charge" {
		t.Errorf("path = %s, want /cloud/internal/v1/charge", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotContentType)
	}
	if !resp.Charged || resp.ProfileID != "bp-1" || resp.Services != 1 {
		t.Errorf("response = %+v, want charged bp-1 with 1 service", resp)
	}

	vals := received.Services[0].Resources[0].Values
	num, ok := vals["usage_bytes"].(json.Number)
	if !ok {
		t.Fatalf("server saw usage_bytes as %T, want json.Number", vals["usage_bytes"])
	}
	if num.String() != "9007199254740993" {
		t.Errorf("server saw usage_bytes = %s, want 9007199254740993 (precision lost in transit)", num)
	}
	if got := vals["ram_gb"]; got != "1.5" {
		t.Errorf("server saw ram_gb = %v (%T), want \"1.5\"", got, got)
	}
}

func TestActiveProfileIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cloud/internal/v1/profiles/active" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"profileIds":["bp-1","bp-2"]}}`))
	}))
	defer srv.Close()

	ids, err := New(srv.URL, "tok", time.Second).ActiveProfileIDs(context.Background())
	if err != nil {
		t.Fatalf("active profiles: %v", err)
	}
	if len(ids) != 2 || ids[0] != "bp-1" || ids[1] != "bp-2" {
		t.Errorf("ids = %v, want [bp-1 bp-2]", ids)
	}
}

// TestNonSuccessIsAnError: a charge that the service rejected must surface as an error rather than
// being mistaken for a completed charge, or the cycle would be silently skipped.
func TestNonSuccessIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errors":{"code":500,"message":"internal.error"}}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "tok", time.Second).Charge(context.Background(), ChargeRequest{ProfileID: "bp-1"}); err == nil {
		t.Error("expected an error for a 500 response")
	}
}

// TestUnconfiguredClientIsSafe: an unset billing URL yields a usable "disabled" client rather than
// a nil-pointer panic on the charge path.
func TestUnconfiguredClientIsSafe(t *testing.T) {
	c := New("", "tok", time.Second)
	if c.Configured() {
		t.Error("empty URL should not be configured")
	}
	if _, err := c.ActiveProfileIDs(context.Background()); err != ErrNotConfigured {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
	if _, err := c.Charge(context.Background(), ChargeRequest{}); err != ErrNotConfigured {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
	if err := c.Health(context.Background()); err != ErrNotConfigured {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}

// TestBaseURLTrailingSlash guards a trivial but easy-to-hit misconfiguration.
func TestBaseURLTrailingSlash(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	if err := New(srv.URL+"/", "tok", time.Second).Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	if path != "/cloud/internal/v1/health" {
		t.Errorf("path = %q, want /cloud/internal/v1/health (trailing slash not trimmed)", path)
	}
}
