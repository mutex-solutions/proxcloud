package admin

import (
	"encoding/json"
	"testing"
)

func TestCustomMenuItemReqDoc(t *testing.T) {
	// Optional blank fields are omitted (null fields dropped); order is always present.
	d := customMenuItemReq{Order: 2}.doc()
	if d["order"] != 2 {
		t.Errorf("order must be set, got %#v", d["order"])
	}
	for _, k := range []string{"displayName", "url", "icon", "renderMode"} {
		if _, ok := d[k]; ok {
			t.Errorf("blank %q must be omitted", k)
		}
	}
	d = customMenuItemReq{DisplayName: "Docs", URL: "/docs", Icon: "book", RenderMode: "IFRAME", Order: 1}.doc()
	for k, want := range map[string]any{"displayName": "Docs", "url": "/docs", "icon": "book", "renderMode": "IFRAME", "order": 1} {
		if d[k] != want {
			t.Errorf("doc[%q]=%#v want %#v", k, d[k], want)
		}
	}
}

func TestMenuURLPlaceholders(t *testing.T) {
	m := menuURLPlaceholders()
	want := map[string][]string{
		"project":        {"id", "name"},
		"user":           {"id", "email", "firstName", "lastName"},
		"billingProfile": {"id", "name"},
	}
	b1, _ := json.Marshal(m)
	b2, _ := json.Marshal(want)
	if string(b1) != string(b2) {
		t.Errorf("placeholders=%s want %s", b1, b2)
	}
}

func TestCustomMenuItemReqDecode(t *testing.T) {
	var req customMenuItemReq
	if err := json.Unmarshal([]byte(`{"displayName":"X","url":"/x","icon":"i","renderMode":"OPEN_NEW_WINDOW","order":7}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.DisplayName != "X" || req.URL != "/x" || req.Icon != "i" || req.RenderMode != "OPEN_NEW_WINDOW" || req.Order != 7 {
		t.Errorf("decoded req mismatch: %+v", req)
	}
}
