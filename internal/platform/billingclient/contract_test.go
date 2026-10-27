package billingclient

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

var update = flag.Bool("update", false, "rewrite the golden contract fixture")

// goldenPath is the shared wire-contract fixture. The billing service has a byte-identical copy and
// a test that decodes it into its own ChargeRequest and re-encodes it. Between the two, a field
// rename, a json-tag change, a type change or a dropped field on EITHER side fails a build.
//
// This is the guard that replaces a shared Go module: proxcloud and the billing service deliberately
// keep independent copies of the schema (different GitHub orgs, and the billing module path is not
// go-gettable), so the contract is pinned by data rather than by types.
//
// When the contract legitimately changes: run `go test ./internal/platform/billingclient -update`,
// copy the regenerated file to the billing repo at test/contract/charge_request.golden.json, and
// ship both sides together.
const goldenPath = "testdata/charge_request.golden.json"

// canonicalChargeRequest is a fully-populated request — every field set, including the ones that
// are usually omitted — so the fixture exercises the whole schema rather than the happy subset.
func canonicalChargeRequest() ChargeRequest {
	yes := true
	created := time.Date(2026, 6, 1, 8, 30, 0, 0, time.UTC)
	deleted := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

	trafficType := &billingapi.BillingResourceType{
		ResourceType: "instance_traffic",
		Attributes: []billingapi.ResourceAttribute{
			{Name: "total_traffic_mb", Type: "number", IsUsage: &yes},
			{Name: "display_name", Type: "string"},
		},
	}

	return ChargeRequest{
		ProfileID:      "bp-1",
		TimeUnit:       "minute",
		CycleStart:     time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		CycleEnd:       time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		CycleTimestamp: time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
		Services: []ServiceResources{{
			ServiceID: "svc-x",
			Resources: []*billingapi.BillingResource{{
				ResourceID:   "instance_traffic-s1",
				ProjectID:    "proj-1",
				ResourceType: "instance_traffic",
				CreatedAt:    &created,
				DeletedAt:    &deleted,
				DisplayPrice: true,
				Values: map[string]any{
					// The value shapes that actually matter on the wire: a decimal (ram_gb-style),
					// an integer beyond float64's exact range, a bool and a string.
					"total_traffic_mb": decimal.RequireFromString("100.5"),
					"usage_bytes":      int64(9007199254740993),
					"is_bareMetal":     false,
					"display_name":     "vm",
				},
				NotEligibleForBilling: true,
				BillingResourceType:   trafficType,
			}},
		}},
		Catalog:        []*billingapi.BillingResourceType{trafficType},
		TimeUnitLimits: map[string]int{"minute": 43200, "hour": 720, "month": 1},
	}
}

// TestChargeRequestMatchesGoldenContract pins proxcloud's side of the wire contract.
func TestChargeRequestMatchesGoldenContract(t *testing.T) {
	got, err := json.MarshalIndent(canonicalChargeRequest(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s — copy it to the billing repo's test/contract/ and ship both sides together", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the charge-request wire contract changed.\n"+
			"If this is intentional, run:\n"+
			"  go test ./internal/platform/billingclient -update\n"+
			"and copy testdata/charge_request.golden.json to the billing repo's\n"+
			"test/contract/charge_request.golden.json — the two services must ship together.\n\n"+
			"got:\n%s\nwant:\n%s", got, want)
	}
}

// TestGoldenContractDecodesLosslessly proves the fixture survives the decoder proxcloud and the
// billing service both use: numbers must come back as exact literals, not rounded float64s.
func TestGoldenContractDecodesLosslessly(t *testing.T) {
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Skipf("golden not generated yet: %v", err)
	}

	var decoded ChargeRequest
	if err := billingapi.NewDecoder(newReader(raw)).Decode(&decoded); err != nil {
		t.Fatalf("decode golden: %v", err)
	}

	if len(decoded.Services) != 1 || len(decoded.Services[0].Resources) != 1 {
		t.Fatalf("unexpected shape after decode: %+v", decoded.Services)
	}
	vals := decoded.Services[0].Resources[0].Values

	// 2^53+1 must survive exactly — this is the failure mode a float64 decode would introduce.
	got, ok := vals["usage_bytes"].(json.Number)
	if !ok {
		t.Fatalf("usage_bytes decoded as %T, want json.Number (UseNumber not in effect)", vals["usage_bytes"])
	}
	if got.String() != "9007199254740993" {
		t.Errorf("usage_bytes = %s, want 9007199254740993", got)
	}

	// A decimal is carried as a quoted string by shopspring's marshaller; it must stay parseable.
	if d, err := decimal.NewFromString(trimQuotes(vals["total_traffic_mb"])); err != nil ||
		!d.Equal(decimal.RequireFromString("100.5")) {
		t.Errorf("total_traffic_mb = %v (%T), want 100.5", vals["total_traffic_mb"], vals["total_traffic_mb"])
	}
}

// newReader is a tiny helper so the test reads naturally without importing bytes at the top.
func newReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// trimQuotes renders a decoded value as the string shopspring produced.
func trimQuotes(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		return ""
	}
}
