package admin

import (
	"encoding/json"
	"testing"
)

func TestMessageTemplateReqDecode(t *testing.T) {
	var req messageTemplateReq
	body := `{"key":"bill_is_paid","category":"BILL","messageTitle":"Paid","messageBody":"<p>hi</p>","disabled":true,"systemTemplate":false,"targets":{"EMAIL":{"enabled":true}}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Key != "bill_is_paid" || req.Category != "BILL" || req.MessageTitle != "Paid" || req.MessageBody != "<p>hi</p>" {
		t.Errorf("decoded scalar mismatch: %+v", req)
	}
	if !req.Disabled || req.SystemTemplate {
		t.Errorf("decoded bool mismatch: disabled=%v systemTemplate=%v", req.Disabled, req.SystemTemplate)
	}
	if req.Targets == nil {
		t.Errorf("targets should decode to a non-nil map, got nil")
	}
}

func TestMessageTemplateCreateDocOmitsBlank(t *testing.T) {
	// Blank optional strings + nil targets are omitted (null fields dropped); the two primitive bools
	// are always present.
	d := messageTemplateReq{Disabled: false, SystemTemplate: false}.createDoc()
	for _, k := range []string{"key", "category", "messageTitle", "messageBody", "targets"} {
		if _, ok := d[k]; ok {
			t.Errorf("blank %q must be omitted, got %#v", k, d[k])
		}
	}
	if d["disabled"] != false {
		t.Errorf("disabled must always be present, got %#v", d["disabled"])
	}
	if d["systemTemplate"] != false {
		t.Errorf("systemTemplate must always be present, got %#v", d["systemTemplate"])
	}
}

func TestMessageTemplateCreateDocFull(t *testing.T) {
	req := messageTemplateReq{
		Key:            "bill_is_paid",
		Category:       "BILL",
		MessageTitle:   "Paid",
		MessageBody:    "<p>hi</p>",
		Disabled:       true,
		SystemTemplate: true,
		Targets:        map[string]any{"EMAIL": map[string]any{"enabled": true}},
	}
	d := req.createDoc()
	for k, want := range map[string]any{
		"key": "bill_is_paid", "category": "BILL", "messageTitle": "Paid",
		"messageBody": "<p>hi</p>", "disabled": true, "systemTemplate": true,
	} {
		if d[k] != want {
			t.Errorf("doc[%q]=%#v want %#v", k, d[k], want)
		}
	}
	if d["targets"] == nil {
		t.Errorf("targets must be present when non-nil")
	}
}

func TestMessageTemplatePlaceholderMap(t *testing.T) {
	m := messageTemplatePlaceholderMap()
	// All 7 categories present.
	for _, cat := range []string{"INVOICE", "BILL", "PAYMENT", "SUSPENSION", "BANK_TRANSFER", "BILLING", "PROJECT"} {
		if _, ok := m[cat]; !ok {
			t.Errorf("category %q missing from placeholder map", cat)
		}
	}
	// Every category begins with the 4 common placeholders, in order.
	common := []string{"{{firstName}}", "{{lastName}}", "{{fullName}}", "{{businessName}}"}
	for cat, list := range m {
		if len(list) < len(common) {
			t.Fatalf("category %q has %d placeholders, fewer than the %d common", cat, len(list), len(common))
		}
		for i, key := range common {
			if list[i].Key != key {
				t.Errorf("category %q placeholder[%d]=%q want common %q", cat, i, list[i].Key, key)
			}
		}
	}
	// Spot-check a category-specific extra preserves order (common THEN extras).
	billing := m["BILLING"]
	if last := billing[len(billing)-1]; last.Key != "{{loginUrl}}" || last.Description != "Login URL for the customer" {
		t.Errorf("BILLING last placeholder=%+v want {{loginUrl}}", last)
	}
	// INVOICE extras count check (4 common + 4 extras).
	if got := len(m["INVOICE"]); got != 8 {
		t.Errorf("INVOICE placeholder count=%d want 8", got)
	}
	// SUSPENSION (4 common + 4 extras).
	if got := len(m["SUSPENSION"]); got != 8 {
		t.Errorf("SUSPENSION placeholder count=%d want 8", got)
	}
}
