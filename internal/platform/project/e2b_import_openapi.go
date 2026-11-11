package project

import "encoding/json"

// Extensions are kept outside the upstream generator so regeneration cannot drop
// local routes. The operator policy is never supplied by a tenant request.
func init() {
	var doc map[string]any
	if err := json.Unmarshal(e2bOpenAPI, &doc); err != nil {
		panic(err)
	}
	paths := doc["paths"].(map[string]any)
	security := []any{map[string]any{"ApiKeyAuth": []any{}}, map[string]any{"ApiKeyBearer": []any{}}}
	name := map[string]any{"type": "string", "pattern": "^[a-z0-9][a-z0-9._-]{0,62}$"}
	aliases := map[string]any{"type": "array", "maxItems": 19, "items": name}
	operation := func(id, description string) map[string]any {
		return map[string]any{"operationId": id, "description": description, "security": security, "responses": map[string]any{"200": map[string]any{"description": "Success"}, "400": map[string]any{"description": "Invalid request"}, "401": map[string]any{"description": "Project API key required"}, "404": map[string]any{"description": "Project template not found"}, "409": map[string]any{"description": "Identity conflict or reference outcome held"}, "429": map[string]any{"description": "Project capacity exceeded"}, "503": map[string]any{"description": "Trusted worker, policy or location unavailable"}}}
	}
	body := func(op map[string]any, schema any) {
		op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schema}}}
	}
	param := []any{map[string]any{"in": "path", "name": "templateID", "required": true, "schema": map[string]any{"type": "string"}}}
	imp := operation("importTemplateDigest", "ProxCloud extension: verify an immutable current-project managed digest or copy an operator-approved EXACT source repository. External access requires caller credentials; anonymous auths={} is used only with explicit operator allowAnonymous. Trusted worker revalidates policy and runtime pull access; no Kaniko. Imported ENV is retained; sandbox-init replaces ENTRYPOINT/CMD. An identical source/runtime settings request adds same-project names, never redispatches an uncertain operation.")
	imp["responses"].(map[string]any)["202"] = map[string]any{"description": "Accepted; poll template/build status; reused identifies same immutable settings"}
	props := map[string]any{"source": map[string]any{"type": "string", "maxLength": 512, "pattern": "^[a-z0-9.:-]+/[a-z0-9/._-]+@sha256:[a-f0-9]{64}$"}, "name": name, "aliases": aliases, "tags": map[string]any{"type": "array", "maxItems": 64, "items": name}, "cpuCount": map[string]any{"type": "integer", "minimum": 1}, "memoryMB": map[string]any{"type": "integer", "minimum": 1}, "user": map[string]any{"type": "string"}, "workdir": map[string]any{"type": "string"}, "startCmd": map[string]any{"type": "string", "maxLength": 32768}, "readyCmd": map[string]any{"type": "string", "maxLength": 32768}, "registry": map[string]any{"type": "object", "required": []any{"username", "password"}, "properties": map[string]any{"username": map[string]any{"type": "string"}, "password": map[string]any{"type": "string", "format": "password", "writeOnly": true}}}}
	body(imp, map[string]any{"type": "object", "required": []any{"source", "name"}, "properties": props})
	paths["/templates/import"] = map[string]any{"post": imp}
	ops := map[string]any{}
	for _, method := range []string{"post", "delete"} {
		op := operation(method+"TemplateAliases", "Add/remove alternate project template names; aliases are not version tags. Primary name is not removed.")
		op["parameters"] = param
		body(op, map[string]any{"type": "object", "required": []any{"aliases"}, "properties": map[string]any{"aliases": aliases}})
		ops[method] = op
	}
	paths["/templates/{templateID}/aliases"] = ops
	ops = map[string]any{}
	for _, method := range []string{"get", "post"} {
		op := operation(method+"TemplatePrepare", "Finite eligible-node N/M cache coverage. Default-off; ready means cache coverage, not workload readiness. GET observes; POST admits or refreshes preparation intent.")
		op["parameters"] = param
		ops[method] = op
	}
	paths["/templates/{templateID}/prepare"] = ops
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if env, ok := schemas["EnvVars"].(map[string]any); ok {
		env["maxProperties"] = 256
		env["description"] = "Environment map: at most 256 entries and 32768 UTF-8 bytes combined; E2B_SANDBOX_ID, PROXCLOUD_ and KUBERNETES_ keys are reserved. Image ENV is preserved; authoritative sandbox ID wins."
	}
	for _, raw := range schemas {
		if schema, ok := raw.(map[string]any); ok {
			if props, ok := schema["properties"].(map[string]any); ok {
				if _, ok := props["cpuCount"]; ok {
					if _, build := props["tags"]; build {
						for _, field := range []string{"buildCpu", "buildMemoryGiB", "buildDiskGiB"} {
							props[field] = map[string]any{"type": "integer", "minimum": 1, "description": "Independent builder budget within location policy, not runtime sandbox size"}
						}
					}
				}
			}
		}
	}
	for _, path := range []string{"/sandboxes", "/v2/sandboxes"} {
		if item, ok := paths[path].(map[string]any); ok {
			if op, ok := item["post"].(map[string]any); ok {
				op["description"] = "Wait for phased startup and committed first-Ready runtime TTL, with one outer cancellation/deadline covering cache and final access reads. Defaults total/scheduling/pull/runtime: 900/120/720/600 seconds, hard total cap 1800. Runtime TTL starts once at first Ready, never shifts on restarts. Failure cleanup is bounded to 30 seconds. Existing project quota, usage and release gates apply."
			}
		}
	}
	var err error
	e2bOpenAPI, err = json.Marshal(doc)
	if err != nil {
		panic(err)
	}
}
