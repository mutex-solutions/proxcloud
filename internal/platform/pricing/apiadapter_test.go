package pricing

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// acrossTheWire is the Phase-2 charge path in miniature: the resolution side serializes the
// billable units, the rating side decodes them with the contract's decoder and maps them into
// engine types. Anything these tests catch here would otherwise show up as a wrong bill only after
// billing was split out.
func acrossTheWire(t *testing.T, in []*billingapi.BillingResource) []*BillingResource {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded, err := billingapi.UnmarshalResources(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return FromAPIResources(decoded)
}

// wireComputeType mirrors engine_test.go's computeType in the wire contract.
func wireComputeType() *billingapi.BillingResourceType {
	yes := true
	return &billingapi.BillingResourceType{ResourceType: "compute", Attributes: []billingapi.ResourceAttribute{
		{Name: "vcpu", Type: "number"},
		{Name: "region", Type: "string"},
		{Name: "tier", Type: "string"},
		{Name: "backups", Type: "boolean"},
		{Name: "usageBytes", Type: "number", IsUsage: &yes},
	}}
}

// TestWireRoundTrip_RatesIdentically is the golden guard for the charge-flow split: rating a
// resource in-process must produce exactly the same amount as rating the same resource after it has
// crossed the wire. It covers the value shapes the real providers emit — plain ints, decimal.Decimal
// (ram_gb), floats, numeric strings, bools — plus an int64 above 2^53, which is precisely where a
// float64 decode would silently corrupt the bill.
func TestWireRoundTrip_RatesIdentically(t *testing.T) {
	e := NewEngine(SystemClock())
	flatTier := []PriceTier{{Value: dp("1")}} // from==nil → rates the whole value

	cases := []struct {
		name  string
		attr  string
		value any
		want  string
	}{
		{"int", "vcpu", 15, "15"},
		{"int64_above_2^53", "usageBytes", int64(9007199254740993), "9007199254740993"},
		{"decimal", "vcpu", decimal.RequireFromString("1.5"), "1.5"},
		{"float", "vcpu", 2.25, "2.25"},
		{"numeric_string", "vcpu", "3", "3"},
		{"high_precision_decimal", "vcpu", decimal.RequireFromString("0.123456789012345678"), "0.123456789012345678"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire := []*billingapi.BillingResource{{
				ResourceType:        "compute",
				Values:              map[string]any{tc.attr: tc.value},
				BillingResourceType: wireComputeType(),
			}}

			local := FromAPIResources(wire)[0]
			remote := acrossTheWire(t, wire)[0]

			rule := priceRule(tc.attr, flatTier)
			localRes, okL := rateSingle(t, e, rule, local)
			remoteRes, okR := rateSingle(t, e, rule, remote)
			if !okL || !okR {
				t.Fatalf("expected both to rate (local=%v remote=%v)", okL, okR)
			}
			gotLocal := localRes.Amounts[0].NetAmount
			gotRemote := remoteRes.Amounts[0].NetAmount

			if !gotLocal.Equal(mustDec(tc.want)) {
				t.Errorf("local net = %s, want %s", gotLocal, tc.want)
			}
			if !gotRemote.Equal(gotLocal) {
				t.Errorf("WIRE DRIFT: remote net = %s, local net = %s (value %v of type %T)", gotRemote, gotLocal, tc.value, tc.value)
			}
		})
	}
}

// TestWireRoundTrip_PreservesBillMetadata pins the other half of the contract: Values is persisted
// verbatim as BillItem.Metadata, so a resource that crossed the wire must serialize into the bill
// byte-for-byte identically to one that did not. This is what makes a stored bill independent of
// whether rating happened in-process or remotely.
func TestWireRoundTrip_PreservesBillMetadata(t *testing.T) {
	created := time.Date(2026, 6, 1, 8, 30, 0, 0, time.UTC)
	wire := []*billingapi.BillingResource{{
		ResourceID:   "s-1",
		ProjectID:    "p-1",
		ResourceType: "compute",
		CreatedAt:    &created,
		Values: map[string]any{
			"vcpu":         2,
			"usageBytes":   int64(9007199254740993),
			"ram_gb":       decimal.RequireFromString("1.5"),
			"display_name": "big-vm",
			"backups":      true,
			"createdAt":    created,
		},
		BillingResourceType: wireComputeType(),
	}}

	local := FromAPIResources(wire)[0]
	remote := acrossTheWire(t, wire)[0]

	localMeta, err := json.Marshal(local.Values)
	if err != nil {
		t.Fatalf("marshal local metadata: %v", err)
	}
	remoteMeta, err := json.Marshal(remote.Values)
	if err != nil {
		t.Fatalf("marshal remote metadata: %v", err)
	}
	if string(localMeta) != string(remoteMeta) {
		t.Errorf("bill metadata drifted across the wire:\n local  = %s\n remote = %s", localMeta, remoteMeta)
	}

	// CreatedAt drives mid-month proration (engine.go getTierValue), so it must survive exactly.
	if remote.CreatedAt == nil || !remote.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", remote.CreatedAt, created)
	}
}

// TestWireRoundTrip_ProratesIdentically exercises the proration path specifically: a resource
// created mid-cycle is charged a fraction of the month, and that fraction is derived from
// CreatedAt. A CreatedAt that did not survive the wire would silently over-bill a full month.
func TestWireRoundTrip_ProratesIdentically(t *testing.T) {
	e := NewEngine(SystemClock())
	created := time.Now().UTC().AddDate(0, 0, -1) // yesterday → inside the current month
	wire := []*billingapi.BillingResource{{
		ResourceType:        "compute",
		CreatedAt:           &created,
		Values:              map[string]any{"vcpu": 4},
		BillingResourceType: wireComputeType(),
	}}

	local := FromAPIResources(wire)[0]
	remote := acrossTheWire(t, wire)[0]

	// A month-timeUnit rule is the one that prorates from CreatedAt.
	rule := PricePlanRule{TimeUnit: "month", ResourceType: "compute",
		Prices: []PricePlanRulePrice{{AttributeName: "vcpu", Tiers: []PriceTier{{Value: dp("10")}}}}}

	localOut, err := e.ApplyPricePlanRules([]PricePlanRule{rule}, local, "month")
	if err != nil {
		t.Fatalf("local rating: %v", err)
	}
	remoteOut, err := e.ApplyPricePlanRules([]PricePlanRule{rule}, remote, "month")
	if err != nil {
		t.Fatalf("remote rating: %v", err)
	}
	if len(localOut) == 0 || len(remoteOut) == 0 {
		t.Fatalf("expected both to rate (local=%d remote=%d)", len(localOut), len(remoteOut))
	}
	l, r := localOut[0].Amounts[0].NetAmount, remoteOut[0].Amounts[0].NetAmount
	if !l.Equal(r) {
		t.Errorf("PRORATION DRIFT: remote = %s, local = %s", r, l)
	}
}

// TestNaiveDecodeWouldCorruptTheBill proves the guard above has teeth: decoding the same payload
// with a plain json.Unmarshal (numbers → float64) silently rounds a large integer and would
// under/over-bill. If this test ever fails, encoding/json changed its default and
// billingapi.UnmarshalResources' UseNumber contract should be re-examined — do NOT "simplify"
// UnmarshalResources to a plain json.Unmarshal.
func TestNaiveDecodeWouldCorruptTheBill(t *testing.T) {
	const exact = int64(9007199254740993) // 2^53 + 1: not representable as a float64
	raw, err := json.Marshal([]*billingapi.BillingResource{{
		Values:              map[string]any{"usageBytes": exact},
		BillingResourceType: wireComputeType(),
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var naive []*billingapi.BillingResource
	if err := json.Unmarshal(raw, &naive); err != nil { // deliberately NOT billingapi.UnmarshalResources
		t.Fatalf("unmarshal: %v", err)
	}
	naiveDec, ok := toDecimal(naive[0].Values["usageBytes"])
	if !ok {
		t.Fatalf("naive value not convertible")
	}
	if naiveDec.Equal(decimal.NewFromInt(exact)) {
		t.Fatalf("expected the naive float64 decode to lose precision on %d, but it did not — "+
			"the UseNumber guard may no longer be load-bearing", exact)
	}

	// The contract's decoder keeps it exact.
	safe, err := billingapi.UnmarshalResources(raw)
	if err != nil {
		t.Fatalf("UnmarshalResources: %v", err)
	}
	safeDec, _ := toDecimal(safe[0].Values["usageBytes"])
	if !safeDec.Equal(decimal.NewFromInt(exact)) {
		t.Errorf("UseNumber decode = %s, want exact %d", safeDec, exact)
	}
}

// TestFromAPI_NilHandling: the adapters must be total — nil in, nil/empty out, never a panic on the
// charge path.
func TestFromAPI_NilHandling(t *testing.T) {
	if got := FromAPIResource(nil); got != nil {
		t.Errorf("FromAPIResource(nil) = %v, want nil", got)
	}
	if got := FromAPIResourceType(nil); got != nil {
		t.Errorf("FromAPIResourceType(nil) = %v, want nil", got)
	}
	if got := FromAPIResources(nil); got == nil || len(got) != 0 {
		t.Errorf("FromAPIResources(nil) = %v, want empty non-nil", got)
	}
	if got := FromAPIResourceTypes(nil); got == nil || len(got) != 0 {
		t.Errorf("FromAPIResourceTypes(nil) = %v, want empty non-nil", got)
	}
	// nil entries are dropped rather than propagated as nil resources into the engine.
	if got := FromAPIResources([]*billingapi.BillingResource{nil, {ResourceID: "a"}}); len(got) != 1 {
		t.Errorf("FromAPIResources should drop nil entries, got %d", len(got))
	}
}

// TestBillingContextRoundTrip pins the context mapping used by the resolution side.
func TestBillingContextRoundTrip(t *testing.T) {
	in := BillingContext{TimeUnitLimits: map[string]int{"hour": 720}}
	out := FromAPIBillingContext(ToAPIBillingContext(in))
	if out.TimeUnitLimits["hour"] != 720 {
		t.Errorf("round-tripped limits = %v, want hour=720", out.TimeUnitLimits)
	}
}
