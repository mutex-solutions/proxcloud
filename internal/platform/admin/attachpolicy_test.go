package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// PUT /service/{id}/attach-policy takes {"attachToNewProjects": bool} and nothing else. (The write itself is a
// single-key jsonb_set — tested against PostgreSQL in test/integration/provider_attach_test.go.)
func TestDecodeAttachPolicy(t *testing.T) {
	decode := func(body string) (bool, error) {
		v, herr := decodeAttachPolicy(httptest.NewRequest(http.MethodPut, "/service/s/attach-policy", strings.NewReader(body)))
		if herr != nil {
			return v, herr
		}
		return v, nil
	}
	if v, err := decode(`{"attachToNewProjects":false}`); err != nil || v {
		t.Fatalf("false: %v %v", v, err)
	}
	if v, err := decode(`{"attachToNewProjects":true}`); err != nil || !v {
		t.Fatalf("true: %v %v", v, err)
	}
	for _, bad := range []string{`{}`, `{"attachToNewProjects":"false"}`, `{"attachToNewProjects":0}`, `{"attachToNewProjects":null}`,
		`{"attachToNewProjects":true,"services":{}}`, `not json`} {
		if _, err := decode(bad); err == nil {
			t.Fatalf("%s accepted", bad)
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
