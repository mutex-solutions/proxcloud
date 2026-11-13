package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// PUT /service/{id}/attach-policy sets config.attachToNewProjects and nothing else: the services map, the quota and every
// other config key stay as stored (several provider saves replace a whole map, this one must not).
func TestApplyAttachPolicyKeepsOtherConfig(t *testing.T) {
	stored := func() pgdoc.M {
		return pgdoc.M{"name": "pve", "config": pgdoc.M{
			"provider": "proxmox",
			"services": pgdoc.M{"compute": pgdoc.M{"r1": true}},
			"provisioning": pgdoc.M{"quota": pgdoc.M{"servers": 5}},
		}}
	}
	put := func(doc pgdoc.M, body string) error {
		if herr := applyAttachPolicy(httptest.NewRequest(http.MethodPut, "/service/s/attach-policy", strings.NewReader(body)), doc); herr != nil {
			return herr
		}
		return nil
	}
	doc := stored()
	if err := put(doc, `{"attachToNewProjects":false}`); err != nil {
		t.Fatal(err)
	}
	cfg := doc["config"].(pgdoc.M)
	if cfg[externalservice.AttachPolicyKey] != false || cfg["provider"] != "proxmox" ||
		cfg["services"].(pgdoc.M)["compute"].(pgdoc.M)["r1"] != true || cfg["provisioning"].(pgdoc.M)["quota"].(pgdoc.M)["servers"] != 5 {
		t.Fatalf("config after the policy save: %#v", cfg)
	}
	if err := put(doc, `{"attachToNewProjects":true}`); err != nil || doc["config"].(pgdoc.M)[externalservice.AttachPolicyKey] != true {
		t.Fatalf("turning it back on: %v %#v", err, doc["config"])
	}
	for _, bad := range []string{`{}`, `{"attachToNewProjects":"false"}`, `{"attachToNewProjects":0}`, `{"attachToNewProjects":null}`,
		`{"attachToNewProjects":true,"services":{}}`, `not json`} {
		doc := stored()
		if err := put(doc, bad); err == nil {
			t.Fatalf("%s accepted", bad)
		}
		if _, set := doc["config"].(pgdoc.M)[externalservice.AttachPolicyKey]; set {
			t.Fatalf("%s wrote the policy", bad)
		}
	}
}

// The Connection-tab save (PUT /service/{id}, a top-level config merge) validates the type too, before it merges.
func TestApplyServiceConnectionBodyValidatesAttachPolicy(t *testing.T) {
	h := &Handler{}
	put := func(doc pgdoc.M, cfg string) error {
		if herr := h.applyServiceConnectionBody(httptest.NewRequest(http.MethodPut, "/service/s", strings.NewReader(`{"config":{`+cfg+`}}`)), doc); herr != nil {
			return herr
		}
		return nil
	}
	doc := pgdoc.M{"config": pgdoc.M{"provider": "powerdns", "apiUrl": "http://a"}}
	if err := put(doc, `"attachToNewProjects":"no","apiUrl":"http://b"`); err == nil {
		t.Fatal("a string policy was accepted")
	}
	if doc["config"].(pgdoc.M)["apiUrl"] != "http://a" {
		t.Fatal("a refused save merged anyway")
	}
	if err := put(doc, `"attachToNewProjects":false`); err != nil {
		t.Fatal(err)
	}
	if c := doc["config"].(pgdoc.M); c[externalservice.AttachPolicyKey] != false || c["apiUrl"] != "http://a" {
		t.Fatalf("%#v", c)
	}
}
