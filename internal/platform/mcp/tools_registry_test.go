package mcp

import "testing"

// TestToolRegistryIntegrity guards the declarative tool rows: unique names, complete
// method/path, resolvable path params, and the rawbody contract (a single rawbody param,
// never mixed with named body params).
func TestToolRegistryIntegrity(t *testing.T) {
	for _, set := range []struct {
		name string
		defs []toolDef
	}{{"admin", adminAllTools()}, {"client", clientTools}} {
		seen := map[string]bool{}
		for _, d := range set.defs {
			if d.name == "" || d.method == "" || d.path == "" {
				t.Fatalf("%s: incomplete tool def %+v", set.name, d)
			}
			if seen[d.name] {
				t.Fatalf("%s: duplicate tool name %q", set.name, d.name)
			}
			seen[d.name] = true
			raw, body := 0, 0
			for _, p := range d.params {
				switch p.in {
				case "path", "query":
				case "body":
					body++
				case "rawbody":
					raw++
				default:
					t.Fatalf("%s/%s: param %q has invalid in=%q", set.name, d.name, p.name, p.in)
				}
			}
			if raw > 1 || (raw == 1 && body > 0) {
				t.Fatalf("%s/%s: rawbody must be single and unmixed (raw=%d body=%d)", set.name, d.name, raw, body)
			}
		}
	}
}

// TestToolAnnotations checks the read/write hint classification and its safety invariant: a tool
// annotated readOnly must never map to a mutating HTTP method (a "read" that secretly deletes would
// mislead an agent into auto-running it), and every write must carry an explicit destructiveHint.
func TestToolAnnotations(t *testing.T) {
	cases := []struct {
		d                                 toolDef
		readOnly, destructive, idempotent bool
	}{
		{toolDef{name: "list_servers", method: "POST"}, true, false, false}, // a POST that only lists is still a read
		{toolDef{name: "get_project", method: "GET"}, true, false, false},
		{toolDef{name: "search_audit_log", method: "POST"}, true, false, false},
		{toolDef{name: "delete_user", method: "DELETE"}, false, true, false},
		{toolDef{name: "reject_bank_transfer", method: "POST"}, false, true, false},
		{toolDef{name: "create_project", method: "POST"}, false, false, false}, // additive
		{toolDef{name: "update_project", method: "PUT"}, false, false, true},   // idempotent
		{toolDef{name: "set_project_quota", method: "POST"}, false, false, true},
	}
	for _, c := range cases {
		a := toolAnnotations(c.d)
		if a.ReadOnlyHint != c.readOnly {
			t.Errorf("%s: readOnly=%v want %v", c.d.name, a.ReadOnlyHint, c.readOnly)
		}
		if c.readOnly {
			continue
		}
		if a.DestructiveHint == nil || *a.DestructiveHint != c.destructive {
			t.Errorf("%s: destructive=%v want %v", c.d.name, a.DestructiveHint, c.destructive)
		}
		if a.IdempotentHint != c.idempotent {
			t.Errorf("%s: idempotent=%v want %v", c.d.name, a.IdempotentHint, c.idempotent)
		}
	}

	for _, defs := range [][]toolDef{adminAllTools(), clientTools} {
		for _, d := range defs {
			a := toolAnnotations(d)
			if a.ReadOnlyHint {
				if d.method == "DELETE" || d.method == "PUT" {
					t.Errorf("%s: annotated readOnly but method=%s (a read must not mutate)", d.name, d.method)
				}
			} else if a.DestructiveHint == nil {
				t.Errorf("%s: write tool missing explicit destructiveHint", d.name)
			}
		}
	}
}
