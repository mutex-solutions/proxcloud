package admin

import (
	"encoding/json"
	"testing"
)

func TestPricePlanReqDecodeAndDoc(t *testing.T) {
	var req pricePlanReq
	body := `{"name":"Gold","enabled":true,"accessMode":"SCOPED","serviceProviders":[{"serviceId":"svc1"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "Gold" || !req.Enabled {
		t.Errorf("name/enabled mismatch: %+v", req)
	}
	if req.AccessMode == nil || *req.AccessMode != "SCOPED" {
		t.Errorf("accessMode should decode to SCOPED, got %v", req.AccessMode)
	}
	if req.ServiceProviders == nil {
		t.Error("serviceProviders pointer should be non-nil when present")
	}
	d := req.doc()
	if d["name"] != "Gold" || d["enabled"] != true {
		t.Errorf("doc name/enabled mismatch: %#v", d)
	}
	if _, ok := d["serviceProviders"]; !ok {
		t.Error("serviceProviders present in request must be emitted")
	}
	// doc() never sets accessMode (callers manage it).
	if _, ok := d["accessMode"]; ok {
		t.Error("doc() must NOT set accessMode (caller-managed)")
	}
}

func TestPricePlanReqDocOmitsServiceProviders(t *testing.T) {
	var req pricePlanReq
	if err := json.Unmarshal([]byte(`{"name":"Basic","enabled":false}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.AccessMode != nil {
		t.Error("omitted accessMode must stay nil (create→PUBLIC; update→preserve)")
	}
	if req.ServiceProviders != nil {
		t.Error("omitted serviceProviders must stay nil (omitted from doc)")
	}
	d := req.doc()
	if _, ok := d["serviceProviders"]; ok {
		t.Error("absent serviceProviders must be omitted from doc()")
	}
	if d["enabled"] != false {
		t.Errorf("enabled must serialize as false (primitive), got %#v", d["enabled"])
	}
}

func TestPricePlanRuleReqDecodeAndDoc(t *testing.T) {
	var req pricePlanRuleReq
	body := `{"name":"Traffic","timeUnit":"month","resourceType":"server","pricePlanId":"pp1",
		"applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"x","tiers":[]}],"filters":[],"modifiers":[]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	d := req.doc()
	for _, k := range []string{"name", "timeUnit", "resourceType", "pricePlanId", "applyMethod", "prices", "filters", "modifiers"} {
		if _, ok := d[k]; !ok {
			t.Errorf("doc() missing key %q: %#v", k, d)
		}
	}
}

func TestPricePlanRuleReqDocOmitsOptional(t *testing.T) {
	var req pricePlanRuleReq
	if err := json.Unmarshal([]byte(`{"name":"R","timeUnit":"hour","resourceType":"volume"}`), &req); err != nil {
		t.Fatal(err)
	}
	d := req.doc()
	if d["name"] != "R" || d["timeUnit"] != "hour" || d["resourceType"] != "volume" {
		t.Errorf("required fields mismatch: %#v", d)
	}
	for _, k := range []string{"pricePlanId", "applyMethod", "prices", "filters", "modifiers"} {
		if _, ok := d[k]; ok {
			t.Errorf("omitted optional %q must be absent from doc()", k)
		}
	}
}

func TestValidatePricePlanRule(t *testing.T) {
	// missing name
	if e := validatePricePlanRule(pricePlanRuleReq{TimeUnit: "month", ResourceType: "x"}); e == nil || e.Msg != "Name must not be null" {
		t.Errorf("missing name want 'Name must not be null', got %v", e)
	}
	// missing timeUnit
	if e := validatePricePlanRule(pricePlanRuleReq{Name: "n", ResourceType: "x"}); e == nil || e.Msg != "Time unit must not be null" {
		t.Errorf("missing timeUnit want 'Time unit must not be null', got %v", e)
	}
	// missing resourceType
	if e := validatePricePlanRule(pricePlanRuleReq{Name: "n", TimeUnit: "month"}); e == nil || e.Msg != "Resource type must not be null" {
		t.Errorf("missing resourceType want 'Resource type must not be null', got %v", e)
	}
	// valid, no prices
	if e := validatePricePlanRule(pricePlanRuleReq{Name: "n", TimeUnit: "month", ResourceType: "x"}); e != nil {
		t.Errorf("valid rule want nil, got %v", e)
	}
}

func TestValidateTiers(t *testing.T) {
	// to < from → PRICE_TIER error
	bad := json.RawMessage(`[{"tiers":[{"from":10,"to":5}]}]`)
	if e := validateTiers(bad); e == nil {
		t.Error("to<from must error")
	}
	// to == from → ok
	eq := json.RawMessage(`[{"tiers":[{"from":5,"to":5}]}]`)
	if e := validateTiers(eq); e != nil {
		t.Errorf("to==from want nil, got %v", e)
	}
	// to > from → ok
	gt := json.RawMessage(`[{"tiers":[{"from":1,"to":100}]}]`)
	if e := validateTiers(gt); e != nil {
		t.Errorf("to>from want nil, got %v", e)
	}
	// missing bounds → ok (compared only when both present)
	open := json.RawMessage(`[{"tiers":[{"from":5}]},{"tiers":[{"to":3}]}]`)
	if e := validateTiers(open); e != nil {
		t.Errorf("open tier want nil, got %v", e)
	}
	// malformed prices → not a tier error (left to store path)
	if e := validateTiers(json.RawMessage(`"oops"`)); e != nil {
		t.Errorf("malformed prices want nil, got %v", e)
	}
}

func TestValidatePricePlanRuleTierBubbles(t *testing.T) {
	raw := json.RawMessage(`[{"tiers":[{"from":100,"to":1}]}]`)
	req := pricePlanRuleReq{Name: "n", TimeUnit: "month", ResourceType: "x", Prices: &raw}
	if e := validatePricePlanRule(req); e == nil {
		t.Error("a bad tier in prices must fail validation")
	}
}

func TestClonePricePlanItemIncludeRulesDefault(t *testing.T) {
	// absent includeRules → true (defaultValue=true)
	var absent clonePricePlanItem
	if err := json.Unmarshal([]byte(`{"pricePlanId":"p"}`), &absent); err != nil {
		t.Fatal(err)
	}
	if !absent.includeRules() {
		t.Error("absent includeRules must default to true")
	}
	// explicit false → false
	var off clonePricePlanItem
	if err := json.Unmarshal([]byte(`{"pricePlanId":"p","includeRules":false}`), &off); err != nil {
		t.Fatal(err)
	}
	if off.includeRules() {
		t.Error("explicit includeRules:false must be false")
	}
	// explicit true → true
	var on clonePricePlanItem
	if err := json.Unmarshal([]byte(`{"pricePlanId":"p","includeRules":true}`), &on); err != nil {
		t.Fatal(err)
	}
	if !on.includeRules() {
		t.Error("explicit includeRules:true must be true")
	}
}

func TestCloneRequestDecode(t *testing.T) {
	var req clonePricePlanRuleRequest
	body := `{"targetPricePlanId":"tgt","rules":[{"ruleId":"r1","newName":"N","overwrite":true},{"ruleId":"r2"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.TargetPricePlanID != "tgt" || len(req.Rules) != 2 {
		t.Errorf("clone rule request decode mismatch: %+v", req)
	}
	if req.Rules[0].NewName != "N" || !req.Rules[0].Overwrite {
		t.Errorf("rule item 0 mismatch: %+v", req.Rules[0])
	}
	if req.Rules[1].Overwrite {
		t.Error("rule item 1 overwrite should default false")
	}

	var pp clonePricePlanRequest
	if err := json.Unmarshal([]byte(`{"pricePlans":[{"pricePlanId":"a","newName":"X"}]}`), &pp); err != nil {
		t.Fatal(err)
	}
	if len(pp.PricePlans) != 1 || pp.PricePlans[0].NewName != "X" {
		t.Errorf("clone price plan request decode mismatch: %+v", pp)
	}
}

func TestPricePlanDocID(t *testing.T) {
	if id, ok := pricePlanDocID(map[string]any{"_id": "abc"}); !ok || id != "abc" {
		t.Errorf("_id string want abc, got %q ok=%v", id, ok)
	}
	if id, ok := pricePlanDocID(map[string]any{"id": "xyz"}); !ok || id != "xyz" {
		t.Errorf("id fallback want xyz, got %q ok=%v", id, ok)
	}
	if _, ok := pricePlanDocID(nil); ok {
		t.Error("nil doc must return ok=false")
	}
	if _, ok := pricePlanDocID(map[string]any{"name": "n"}); ok {
		t.Error("no id key must return ok=false")
	}
}
