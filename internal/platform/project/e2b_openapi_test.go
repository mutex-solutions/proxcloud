package project

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Routes that are not part of the E2B API: the document itself.
var e2bOpenAPIExcluded = map[string]bool{
	"GET /openapi.json": true,
}

// TestE2BOpenAPIParity: every routed E2B operation is documented and every documented one is
// routed; path parameters are declared; every $ref resolves.
func TestE2BOpenAPIParity(t *testing.T) {
	var doc struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(e2bOpenAPI, &doc); err != nil || doc.OpenAPI != "3.1.0" {
		t.Fatalf("document: %v %q", err, doc.OpenAPI)
	}
	documented := map[string]bool{}
	for p, ops := range doc.Paths {
		for m, op := range ops {
			if m == "parameters" {
				continue
			}
			documented[strings.ToUpper(m)+" "+p] = true
			// Every {param} in the path is declared (on the operation or the path item).
			raw := string(op) + string(ops["parameters"])
			for _, pm := range regexp.MustCompile(`\{(\w+)\}`).FindAllStringSubmatch(p, -1) {
				if !strings.Contains(raw, `"name":"`+pm[1]+`"`) && !strings.Contains(raw, `"name": "`+pm[1]+`"`) && !refDeclares(raw, pm[1]) {
					t.Errorf("%s %s: path parameter %s not declared", m, p, pm[1])
				}
			}
		}
	}
	r := chi.NewRouter()
	(&Handler{}).E2BRoutes(r)
	routed := map[string]bool{}
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		k := method + " " + strings.TrimSuffix(route, "/")
		if k == method+" " {
			k = method + " /"
		}
		routed[k] = true
		return nil
	})
	var missing, extra []string
	for k := range routed {
		if !documented[k] && !e2bOpenAPIExcluded[k] {
			missing = append(missing, k)
		}
	}
	for k := range documented {
		if !routed[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing)+len(extra) > 0 {
		t.Fatalf("routed but not documented: %v\ndocumented but not routed: %v", missing, extra)
	}

	// Every $ref resolves.
	var all map[string]any
	_ = json.Unmarshal(e2bOpenAPI, &all)
	for _, ref := range regexp.MustCompile(`"\$ref":\s*"#/([^"]+)"`).FindAllStringSubmatch(string(e2bOpenAPI), -1) {
		var cur any = all
		for _, part := range strings.Split(ref[1], "/") {
			m, _ := cur.(map[string]any)
			cur = m[part]
		}
		if cur == nil {
			t.Errorf("unresolved $ref #/%s", ref[1])
		}
	}
}

// refDeclares: the parameter comes from a $ref to components/parameters named like it
// (upstream's convention, e.g. #/components/parameters/sandboxID).
func refDeclares(raw, name string) bool {
	return strings.Contains(raw, `#/components/parameters/`+name+`"`)
}
