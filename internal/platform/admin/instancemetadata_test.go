package admin

import (
	"encoding/json"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func strp(s string) *string { return &s }

func TestInstanceMetadataReqDecode(t *testing.T) {
	var req instanceMetadataOptionReq
	body := `{"key":"team","displayName":"Team","description":"d","type":"PREDEFINED_VALUES",
		"options":[{"displayName":"Alpha","value":"a","enabled":true}],
		"serviceIds":["svc1"],"regions":["r1"],"userEditable":true,"showInline":true}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Key != "team" || req.Type == nil || *req.Type != "PREDEFINED_VALUES" {
		t.Errorf("decode mismatch: %+v", req)
	}
	if len(req.Options) != 1 || req.Options[0].Value != "a" {
		t.Errorf("options decode mismatch: %+v", req.Options)
	}
	if !req.UserEditable || !req.ShowInline {
		t.Errorf("flags decode mismatch: %+v", req)
	}
	// omitted type/numericRange/serviceIds stay nil
	var empty instanceMetadataOptionReq
	if err := json.Unmarshal([]byte(`{"key":"x"}`), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Type != nil || empty.NumericRange != nil || empty.Options != nil || empty.ServiceIds != nil || empty.Regions != nil {
		t.Errorf("omitted nullable fields must stay nil, got %+v", empty)
	}
}

func TestValidateInstanceMetadataKey(t *testing.T) {
	if e := validateInstanceMetadataKey("   "); e == nil || e.Msg != "Metadata key is required" {
		t.Errorf("blank key want 'Metadata key is required', got %v", e)
	}
	if e := validateInstanceMetadataKey(""); e == nil || e.Msg != "Metadata key is required" {
		t.Errorf("empty key want 'Metadata key is required', got %v", e)
	}
	// reserved prefixes (case-insensitive)
	for _, k := range []string{"hw:foo", "os_bar", "proxcloud_baz", "HW:up", "PROXCLOUD_X"} {
		e := validateInstanceMetadataKey(k)
		if e == nil || e.Msg[:41] != "Metadata key cannot start with reserved p" {
			t.Errorf("reserved key %q should be rejected, got %v", k, e)
		}
	}
	// valid keys
	for _, k := range []string{"team", "department", "cost-center", "proxcloudfoo"} {
		if e := validateInstanceMetadataKey(k); e != nil {
			t.Errorf("valid key %q want nil, got %v", k, e)
		}
	}
	// exact prefix message
	if e := validateInstanceMetadataKey("hw:x"); e == nil || e.Msg != "Metadata key cannot start with reserved prefix: hw:" {
		t.Errorf("hw: message mismatch, got %v", e)
	}
}

func TestValidateTypeRequired(t *testing.T) {
	req := instanceMetadataOptionReq{Key: "k"} // no type
	if e := req.validateTypeAndShape(); e == nil || e.Msg != "Metadata option type is required" {
		t.Errorf("nil type want 'Metadata option type is required', got %v", e)
	}
}

func TestValidatePredefinedValues(t *testing.T) {
	// numericRange present → reject
	r1 := instanceMetadataOptionReq{Type: strp("PREDEFINED_VALUES"), NumericRange: &numericRangeReq{Min: 1, Max: 2}}
	if e := r1.validateTypeAndShape(); e == nil || e.Msg != "numericRange must be null for PREDEFINED_VALUES type" {
		t.Errorf("predefined+numericRange want reject, got %v", e)
	}
	// no options → reject
	r2 := instanceMetadataOptionReq{Type: strp("PREDEFINED_VALUES")}
	if e := r2.validateTypeAndShape(); e == nil || e.Msg != "At least one value option is required for PREDEFINED_VALUES type" {
		t.Errorf("predefined+no options want reject, got %v", e)
	}
	// blank value → reject
	r3 := instanceMetadataOptionReq{Type: strp("PREDEFINED_VALUES"), Options: []metadataValueOptionReq{{DisplayName: "A", Value: "  "}}}
	if e := r3.validateTypeAndShape(); e == nil || e.Msg != "Each value option must have a non-blank value" {
		t.Errorf("blank value want reject, got %v", e)
	}
	// blank displayName → reject
	r4 := instanceMetadataOptionReq{Type: strp("PREDEFINED_VALUES"), Options: []metadataValueOptionReq{{DisplayName: "", Value: "a"}}}
	if e := r4.validateTypeAndShape(); e == nil || e.Msg != "Each value option must have a non-blank displayName" {
		t.Errorf("blank displayName want reject, got %v", e)
	}
	// duplicate values (case-insensitive) → reject
	r5 := instanceMetadataOptionReq{Type: strp("PREDEFINED_VALUES"), Options: []metadataValueOptionReq{
		{DisplayName: "A", Value: "x"}, {DisplayName: "B", Value: "X"}}}
	if e := r5.validateTypeAndShape(); e == nil || e.Msg != "Duplicate values are not allowed in options" {
		t.Errorf("dup values want reject, got %v", e)
	}
	// valid
	r6 := instanceMetadataOptionReq{Type: strp("PREDEFINED_VALUES"), Options: []metadataValueOptionReq{
		{DisplayName: "A", Value: "x"}, {DisplayName: "B", Value: "y"}}}
	if e := r6.validateTypeAndShape(); e != nil {
		t.Errorf("valid predefined want nil, got %v", e)
	}
}

func TestValidateNumericRange(t *testing.T) {
	// options non-empty → reject
	r1 := instanceMetadataOptionReq{Type: strp("NUMERIC_RANGE"), Options: []metadataValueOptionReq{{Value: "x", DisplayName: "X"}}}
	if e := r1.validateTypeAndShape(); e == nil || e.Msg != "options must be empty for NUMERIC_RANGE type" {
		t.Errorf("numeric+options want reject, got %v", e)
	}
	// nil numericRange → reject
	r2 := instanceMetadataOptionReq{Type: strp("NUMERIC_RANGE")}
	if e := r2.validateTypeAndShape(); e == nil || e.Msg != "numericRange is required for NUMERIC_RANGE type" {
		t.Errorf("numeric+nil range want reject, got %v", e)
	}
	// min >= max → reject
	r3 := instanceMetadataOptionReq{Type: strp("NUMERIC_RANGE"), NumericRange: &numericRangeReq{Min: 5, Max: 5}}
	if e := r3.validateTypeAndShape(); e == nil || e.Msg != "numericRange.min must be less than numericRange.max" {
		t.Errorf("min>=max want reject, got %v", e)
	}
	// valid
	r4 := instanceMetadataOptionReq{Type: strp("NUMERIC_RANGE"), NumericRange: &numericRangeReq{Min: 1, Max: 10, Unit: "GB"}}
	if e := r4.validateTypeAndShape(); e != nil {
		t.Errorf("valid numeric want nil, got %v", e)
	}
}

func TestValidateRegionsRequireServiceIds(t *testing.T) {
	// regions without serviceIds → reject (PREDEFINED with valid options to reach the regions check)
	r1 := instanceMetadataOptionReq{
		Type:    strp("PREDEFINED_VALUES"),
		Options: []metadataValueOptionReq{{DisplayName: "A", Value: "a"}},
		Regions: []string{"r1"},
	}
	if e := r1.validateTypeAndShape(); e == nil || e.Msg != "Regions cannot be specified without service IDs" {
		t.Errorf("regions w/o serviceIds want reject, got %v", e)
	}
	// regions with serviceIds → ok
	r2 := instanceMetadataOptionReq{
		Type:       strp("PREDEFINED_VALUES"),
		Options:    []metadataValueOptionReq{{DisplayName: "A", Value: "a"}},
		Regions:    []string{"r1"},
		ServiceIds: []string{"svc1"},
	}
	if e := r2.validateTypeAndShape(); e != nil {
		t.Errorf("regions+serviceIds want nil, got %v", e)
	}
}

func TestInstanceMetadataMutableDoc(t *testing.T) {
	// minimal: blank optional strings + nil collections omitted; primitives always present.
	req := instanceMetadataOptionReq{Key: "k", Type: strp("PREDEFINED_VALUES")}
	d := req.mutableDoc()
	if d["key"] != "k" {
		t.Errorf("key must be set, got %#v", d["key"])
	}
	if d["type"] != "PREDEFINED_VALUES" {
		t.Errorf("type must be set, got %#v", d["type"])
	}
	if _, ok := d["userEditable"]; !ok {
		t.Error("userEditable (primitive) must always be present")
	}
	if _, ok := d["showInline"]; !ok {
		t.Error("showInline (primitive) must always be present")
	}
	for _, k := range []string{"displayName", "description", "options", "numericRange", "serviceIds", "regions"} {
		if _, ok := d[k]; ok {
			t.Errorf("blank/nil %q must be omitted from mutableDoc()", k)
		}
	}

	// type nil → omitted
	d2 := instanceMetadataOptionReq{Key: "k"}.mutableDoc()
	if _, ok := d2["type"]; ok {
		t.Error("nil type must be omitted from mutableDoc()")
	}

	// present (even empty) serviceIds/regions/options → emitted (non-null empties kept)
	d3 := instanceMetadataOptionReq{
		Key:        "k",
		Options:    []metadataValueOptionReq{},
		ServiceIds: []string{},
		Regions:    []string{},
	}.mutableDoc()
	for _, k := range []string{"options", "serviceIds", "regions"} {
		if _, ok := d3[k]; !ok {
			t.Errorf("present (empty) %q must be emitted", k)
		}
	}

	// numericRange with unit
	d4 := instanceMetadataOptionReq{Key: "k", NumericRange: &numericRangeReq{Min: 1, Max: 2, Unit: "GB"}}.mutableDoc()
	nr, ok := d4["numericRange"].(pgdoc.M)
	if !ok {
		t.Fatalf("numericRange must be a pgdoc.M, got %T", d4["numericRange"])
	}
	if nr["min"] != float64(1) || nr["max"] != float64(2) || nr["unit"] != "GB" {
		t.Errorf("numericRange fields mismatch: %#v", nr)
	}
	// numericRange without unit → unit omitted
	d5 := instanceMetadataOptionReq{Key: "k", NumericRange: &numericRangeReq{Min: 1, Max: 2}}.mutableDoc()
	nr5, _ := d5["numericRange"].(pgdoc.M)
	if _, ok := nr5["unit"]; ok {
		t.Error("blank unit must be omitted from numericRange")
	}
}

func TestValueOptionDoc(t *testing.T) {
	// blank fields omitted, enabled always present
	d := valueOptionDoc(metadataValueOptionReq{Enabled: true})
	if v, ok := d["enabled"]; !ok || v != true {
		t.Errorf("enabled must always be present, got %#v", d)
	}
	if _, ok := d["displayName"]; ok {
		t.Error("blank displayName must be omitted")
	}
	if _, ok := d["value"]; ok {
		t.Error("blank value must be omitted")
	}
	d2 := valueOptionDoc(metadataValueOptionReq{DisplayName: "A", Value: "a", Enabled: false})
	if d2["displayName"] != "A" || d2["value"] != "a" || d2["enabled"] != false {
		t.Errorf("populated value option mismatch: %#v", d2)
	}
}
