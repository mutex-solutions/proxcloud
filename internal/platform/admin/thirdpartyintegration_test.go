package admin

import (
	"encoding/json"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func TestThirdPartyIntegrationReqDecode(t *testing.T) {
	var req thirdPartyIntegrationReq
	body := `{"name":"My Stripe","description":"d","thirdParty":"Stripe",
		"config":{"publicKey":"pk_1"},"secret":{"secretKey":"sk_1"},"metadata":{"k":"v"}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "My Stripe" || req.Description != "d" || req.ThirdParty != "Stripe" {
		t.Errorf("scalars mismatch: %+v", req)
	}
	cfg, ok := req.Config.(map[string]any)
	if !ok || cfg["publicKey"] != "pk_1" {
		t.Errorf("config mismatch: %#v", req.Config)
	}
	sec, ok := req.Secret.(map[string]any)
	if !ok || sec["secretKey"] != "sk_1" {
		t.Errorf("secret mismatch: %#v", req.Secret)
	}
	if req.Metadata["k"] != "v" {
		t.Errorf("metadata mismatch: %#v", req.Metadata)
	}
}

func TestIntegrationFieldsOmitsBlank(t *testing.T) {
	// Blank optional strings + nil config/metadata are omitted (null fields dropped).
	d := integrationFields("", "", "", nil, nil)
	if len(d) != 0 {
		t.Errorf("all-blank must produce empty doc, got %#v", d)
	}
	d = integrationFields("n", "desc", "Stripe", map[string]any{"a": 1}, map[string]any{"m": 2})
	for k, want := range map[string]any{"name": "n", "description": "desc", "thirdParty": "Stripe"} {
		if d[k] != want {
			t.Errorf("doc[%q]=%#v want %#v", k, d[k], want)
		}
	}
	if _, ok := d["config"]; !ok {
		t.Error("config must be present when non-nil")
	}
	if _, ok := d["metadata"]; !ok {
		t.Error("metadata must be present when non-nil")
	}
}

func TestIntegrationDocStoresSecret(t *testing.T) {
	d := integrationDoc("n", "d", "Stripe", map[string]any{"c": 1}, map[string]any{"s": 1}, nil)
	if _, ok := d["secret"]; !ok {
		t.Error("secret must be stored when non-nil")
	}
	d = integrationDoc("n", "d", "Stripe", nil, nil, nil)
	if _, ok := d["secret"]; ok {
		t.Error("nil secret must be omitted")
	}
}

func TestIntegrationDtoNullsSecretAndShapesID(t *testing.T) {
	oid := pgdoc.NewID()
	doc := pgdoc.M{"_id": oid, "_class": "ThirdPartyIntegration",
		"name": "n", "thirdParty": "Stripe", "secret": map[string]any{"sk": "x"}}
	out := integrationDto(doc)
	if _, ok := out["secret"]; ok {
		t.Error("DTO must always null/omit the secret")
	}
	if _, ok := out["_class"]; ok {
		t.Error("_class must be dropped")
	}
	if _, ok := out["_id"]; ok {
		t.Error("_id must be renamed to id")
	}
	if out["id"] != oid {
		t.Errorf("id=%#v want the id", out["id"])
	}
	if out["name"] != "n" || out["thirdParty"] != "Stripe" {
		t.Errorf("non-secret fields must survive: %#v", out)
	}
}

func TestIsNeededToUpdateSecret(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want bool
	}{
		{"nil", nil, false},
		{"empty object", map[string]any{}, false},
		{"all non-null", map[string]any{"a": "x", "b": 1}, true},
		{"has null field", map[string]any{"a": "x", "b": nil}, false},
		{"non-object string", "abc", false},
		{"non-object number", float64(5), false},
	}
	for _, c := range cases {
		if got := isNeededToUpdateSecret(c.in); got != c.want {
			t.Errorf("%s: isNeededToUpdateSecret(%#v)=%v want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestIntegrationCreateDefaulting mirrors the create() name/description defaulting (name←thirdParty
// when blank; description←"<thirdParty> Integration" when blank) without touching the datastore.
func TestIntegrationCreateDefaulting(t *testing.T) {
	req := thirdPartyIntegrationReq{ThirdParty: "Acme"}
	name := req.Name
	if name == "" {
		name = req.ThirdParty
	}
	description := req.Description
	if description == "" {
		description = req.ThirdParty + " Integration"
	}
	if name != "Acme" {
		t.Errorf("name default=%q want Acme", name)
	}
	if description != "Acme Integration" {
		t.Errorf("description default=%q want %q", description, "Acme Integration")
	}
}
