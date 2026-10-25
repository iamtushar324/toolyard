package docs

import (
	"encoding/json"
	"strings"
	"testing"
)

// codeRoutes are the routes of docs/design/code-rest-route.md, with their
// methods. The OpenAPI document must describe every one.
var codeRoutes = map[string][]string{
	"/v1/agents/session-tokens":            {"post"},
	"/v1/agents/session-tokens/{token_id}": {"delete"},
	"/v1/code/run":                         {"post"},
	"/v1/code/runs/{id}":                   {"get"},
	"/v1/code/runs/{id}/resume":            {"post"},
	"/v1/code/files":                       {"get"},
	"/v1/code/files/{server}/{tool}.pyi":   {"get"},
	"/v1/code/files/{server}/{tool}.d.ts":  {"get"},
	"/v1/code/search":                      {"get"},
	"/v1/code/docs/{server}/{tool}":        {"get"},
	"/v1/code/openapi.json":                {"get"},
	"/v1/code/ty":                          {"get"},
}

func TestCodeOpenAPI(t *testing.T) {
	var doc struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
		RunHTTP map[string]int                        `json:"x-run-status-http"`
	}
	if err := json.Unmarshal([]byte(CodeOpenAPI), &doc); err != nil {
		t.Fatalf("openapi/code.json is not valid JSON: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1.") {
		t.Errorf("openapi = %q, want 3.1.x", doc.OpenAPI)
	}
	for path, methods := range codeRoutes {
		ops, ok := doc.Paths[path]
		if !ok {
			t.Errorf("openapi/code.json is missing %s", path)
			continue
		}
		for _, m := range methods {
			if _, ok := ops[m]; !ok {
				t.Errorf("openapi/code.json is missing %s %s", strings.ToUpper(m), path)
			}
		}
	}
	// The HTTP status per run status, from the contract.
	want := map[string]int{
		"succeeded": 200, "failed": 200, "permission_required": 403,
		"deferred": 202, "queued": 202, "running": 202, "outcome_unknown": 409,
	}
	for status, code := range want {
		if doc.RunHTTP[status] != code {
			t.Errorf("x-run-status-http[%s] = %d, want %d", status, doc.RunHTTP[status], code)
		}
	}
	// Every run route documents every HTTP status a run status maps to.
	for _, route := range [][2]string{{"/v1/code/run", "post"}, {"/v1/code/runs/{id}", "get"}, {"/v1/code/runs/{id}/resume", "post"}} {
		var op struct {
			Responses map[string]json.RawMessage `json:"responses"`
		}
		if err := json.Unmarshal(doc.Paths[route[0]][route[1]], &op); err != nil {
			t.Errorf("%s %s: %v", strings.ToUpper(route[1]), route[0], err)
			continue
		}
		for _, code := range []string{"200", "202", "403", "409"} {
			if _, ok := op.Responses[code]; !ok {
				t.Errorf("%s %s does not document HTTP %s", strings.ToUpper(route[1]), route[0], code)
			}
		}
	}
}

func TestTyScript(t *testing.T) {
	if !strings.HasPrefix(TyScript, "#!/usr/bin/env python3\n") {
		t.Errorf("ty must start with #!/usr/bin/env python3")
	}
	for _, cmd := range []string{"run", "status", "resume", "files", "stub", "call", "skills"} {
		if !strings.Contains(TyScript, `add_parser("`+cmd+`"`) {
			t.Errorf("ty has no %q subcommand", cmd)
		}
	}
	for _, want := range []string{"TOOLYARD_URL", "TOOLYARD_TOKEN_FILE", "https://toolyard.beknown.live", "Idempotency-Key", "X-Toolyard-Reason", "text/x-starlark"} {
		if !strings.Contains(TyScript, want) {
			t.Errorf("ty should mention %q", want)
		}
	}
}
