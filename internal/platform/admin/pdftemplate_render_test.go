package admin

import (
	"strings"
	"testing"

	"github.com/cbroglie/mustache"
)

// The bundled default templates load and mustache-render against the dummy data
// (the preview/revert legs' core), including sections ({{#items}}) and dotted names.
func TestPDFTemplateDefaultsRender(t *testing.T) {
	for _, tc := range []struct{ asset, typ, mustContain string }{
		{"templates/invoice-template.html", "INVOICE", "ACME Corporation Ltd."},
		{"templates/statement-template.html", "STATEMENT", "tms-1-prod-nwb-1"},
	} {
		raw, err := defaultPDFTemplates.ReadFile(tc.asset)
		if err != nil {
			t.Fatalf("%s: embed read: %v", tc.asset, err)
		}
		html, err := mustache.Render(string(raw), pdfTemplateDummyData(tc.typ))
		if err != nil {
			t.Fatalf("%s: render: %v", tc.asset, err)
		}
		if !strings.Contains(html, tc.mustContain) {
			t.Errorf("%s: rendered HTML missing %q", tc.asset, tc.mustContain)
		}
	}
	// The STATEMENT dummy-data bug is kept: item2 has NO period.
	items := pdfTemplateDummyData("STATEMENT")["items"].([]map[string]any)
	if _, has := items[1]["period"]; has {
		t.Error("statement item2 must NOT carry a period (bug kept)")
	}
}
