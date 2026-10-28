package codemode

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// TypeScript stubs for JavaScript scripts: the files listToolFiles shows
// with language "javascript", generated from each tool's input and output
// JSON Schemas.
//
//	servers/index.d.ts           the tools object, its helpers and console
//	servers/<Server>.d.ts        one server's tools, as an interface
//	servers/<Server>/<tool>.d.ts one tool: Input, Output, Call
//
// Objects become inline object types (required properties first, each
// group alphabetical, descriptions as JSDoc), arrays T[], enums and const
// literal unions, anyOf/oneOf unions, allOf intersections; what the
// generator cannot express ($ref, a schema nested too deep) is unknown.

const (
	// tsMaxDepth is how deep a schema is rendered before unknown.
	tsMaxDepth = 8
	// tsIndexFile is the file name of the index, under servers/.
	tsIndexFile = "index.d.ts"
)

// tsHelperNames are the tools object's own members; a server of the same
// name is reachable only by name (tools.call("call.x", ...)).
var tsHelperNames = map[string]bool{"call": true, "tryCall": true, "parallel": true, "search": true}

// listTSFiles renders the servers/ tree of .d.ts files for the ctx caller.
func (r *Runtime) listTSFiles(ctx context.Context) string {
	cat := buildCatalog(r.caller.Tools(ctx))
	var files []string
	for _, s := range cat.servers {
		for _, b := range s.bound {
			files = append(files, fmt.Sprintf("servers/%s/%s.d.ts", s.ident, b.ident))
		}
	}
	if len(files) == 0 {
		return "No servers are available to you. There are no virtual .d.ts files available. " +
			"Ask a toolyard admin to grant you a server, then call listToolFiles again."
	}
	files = append(files, "servers/"+tsIndexFile)
	lines := []string{
		"// Workflow (JavaScript): listToolFiles -> readToolFile -> executeToolCode with language \"javascript\".",
		"// Each tool is servers/<Server>/<tool>.d.ts; servers/index.d.ts declares the tools object, its helpers",
		"// (tools.call, tools.tryCall, tools.parallel, tools.search) and console.",
		"// In a script: const r = await tools.<Server>.<tool>({ param: value });",
	}
	for _, note := range cat.refusedNotes() {
		lines = append(lines, "//"+strings.TrimPrefix(note, "#"))
	}
	lines = append(lines, "", renderTree(files))
	return strings.Join(lines, "\n")
}

// readTSFile renders one .d.ts file: p is the path without servers/ and
// .d.ts ("index", "<Server>", "<Server>/<tool>").
func (r *Runtime) readTSFile(ctx context.Context, fileName, p string) (string, bool) {
	cat := buildCatalog(r.caller.Tools(ctx))
	if p == "index" {
		return tsIndex(cat), true
	}
	serverName, toolName, _ := strings.Cut(p, "/")
	if serverName == "" || strings.Contains(toolName, "/") {
		return fmt.Sprintf("Invalid filename '%s'. Use servers/<serverName>/<toolName>.d.ts as listToolFiles shows with language \"javascript\".", fileName), false
	}
	srv, several, refused := findServer(cat, serverName)
	switch {
	case len(refused) > 0:
		return refusedServerText(serverName, refused), false
	case len(several) > 0:
		return fmt.Sprintf("Multiple servers match filename '%s': %s. Use the exact name from listToolFiles.", fileName, strings.Join(several, ", ")), false
	case srv == nil:
		var b strings.Builder
		fmt.Fprintf(&b, "No server found matching '%s'. Available servers are:\n", serverName)
		for _, s := range cat.servers {
			fmt.Fprintf(&b, "  - servers/%s.d.ts\n", s.ident)
		}
		return b.String(), false
	}
	if toolName == "" {
		return tsServer(srv), true
	}
	b, names := srv.lookup(toolName)
	if len(names) > 0 {
		return ambiguityError(srv.ident, toolName, names).Error(), false
	}
	if b == nil {
		var sb strings.Builder
		fmt.Fprintf(&sb, "Tool '%s' not found in server '%s'. Available tools in this server are:\n", toolName, srv.ident)
		for _, bd := range srv.bound {
			fmt.Fprintf(&sb, "  - servers/%s/%s.d.ts\n", srv.ident, bd.ident)
		}
		return sb.String(), false
	}
	return tsTool(srv, *b), true
}

// tsTool is servers/<Server>/<tool>.d.ts.
func tsTool(srv *server, bd binding) string {
	t := bd.tool
	call := srv.ident + "." + bd.ident
	var b strings.Builder
	head := "// tools." + call
	if desc := tsShort(t.Description); desc != "" {
		head += ": " + desc
	}
	b.WriteString(head + "\n")
	b.WriteString("// Usage (executeToolCode, language \"javascript\"):\n")
	fmt.Fprintf(&b, "//   const r = await tools.%s(%s);\n", call, tsExample(t.Properties, t.Required))
	b.WriteString("// A call resolves to the tool's structuredContent when it sends one, else to its\n")
	b.WriteString("// text parsed as JSON, else to the text. A failed or held call rejects with a\n")
	fmt.Fprintf(&b, "// ToolCallError; tools.tryCall(%q, args) resolves to\n", call)
	b.WriteString("// { ok, status, result } or { ok: false, status, error } instead.\n")
	if srv.ident != srv.name {
		fmt.Fprintf(&b, "// Server %q is called %s in code.\n", srv.name, srv.ident)
	}
	b.WriteString("\n")
	writeDoc(&b, "", strings.TrimSpace(t.Description))
	fmt.Fprintf(&b, "export interface Input %s\n\n", tsObjectBody(t.Properties, stringSet(t.Required), nil, "", 0))
	writeOutput(&b, t.OutputSchema)
	fmt.Fprintf(&b, "\nexport type Call = (%s) => Promise<Output>;\n", tsArgs("Input", t.Required))
	return b.String()
}

// tsShort is a description's first sentence on its first line, as the
// .pyi stubs shorten it.
func tsShort(desc string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(desc), "\n")
	return shortDescription(first)
}

// tsArgs is a call's parameter: optional when the tool requires nothing.
func tsArgs(typ string, required []string) string {
	if len(required) == 0 {
		return "args?: " + typ
	}
	return "args: " + typ
}

// writeOutput writes the Output declaration.
func writeOutput(b *strings.Builder, schema map[string]any) {
	if len(schema) == 0 {
		b.WriteString("// The tool declares no output schema.\n")
		b.WriteString("export type Output = unknown;\n")
		return
	}
	desc, _ := schema["description"].(string)
	writeDoc(b, "", strings.TrimSpace(desc))
	props, _ := schema["properties"].(map[string]any)
	if len(props) > 0 && !hasType(schema, "array") {
		fmt.Fprintf(b, "export interface Output %s\n", tsObjectBody(props, stringSet(schema["required"]), schema["additionalProperties"], "", 0))
		return
	}
	fmt.Fprintf(b, "export type Output = %s;\n", tsType(schema, "", 0))
}

// tsServer is servers/<Server>.d.ts.
func tsServer(srv *server) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// tools.%s: the %s tools, for executeToolCode with language \"javascript\".\n", srv.ident, srv.name)
	fmt.Fprintf(&b, "// Each tool's own stub, servers/%s/<tool>.d.ts, has its full documentation.\n", srv.ident)
	if srv.ident != srv.name {
		fmt.Fprintf(&b, "// Server %q is called %s in code.\n", srv.name, srv.ident)
	}
	idents := make([]string, 0, len(srv.ambiguous))
	for ident := range srv.ambiguous {
		idents = append(idents, ident)
	}
	sort.Strings(idents)
	for _, ident := range idents {
		fmt.Fprintf(&b, "// Not callable: %s (ambiguous, could be %s)\n", ident, strings.Join(quoteAll(srv.ambiguous[ident]), " or "))
	}
	b.WriteString("\nexport interface Tools {\n")
	for _, bd := range srv.bound {
		if desc := tsShort(bd.tool.Description); desc != "" {
			writeDoc(&b, "  ", desc)
		}
		file := fmt.Sprintf("./%s/%s", srv.ident, bd.ident)
		fmt.Fprintf(&b, "  %s(%s): Promise<import(%q).Output>;\n", tsKey(bd.ident),
			tsArgs(fmt.Sprintf("import(%q).Input", file), bd.tool.Required), file)
	}
	b.WriteString("}\n")
	return b.String()
}

// tsIndexHead is the fixed part of servers/index.d.ts.
const tsIndexHead = `// toolyard code mode: what a JavaScript script sees (executeToolCode with
// language "javascript"). The script is the body of an async function:
// await tool calls, and return the value you want back.
//
//   const issue = await tools.Linear.get_issue({ id: "TEC-1" });
//   const s = await tools.tryCall("Linear.get_issue", { id: "x" }); // { ok, status, result | error }
//   const all = await tools.parallel([() => tools.A.x({}), () => tools.B.y({})], { limit: 4 });
//   const found = await tools.search("create a linear issue", { limit: 5 }); // [{ name, code, run, ... }]
//   console.log("...");
//   return { summary: "..." };

/** The outcome of tools.tryCall: never a rejection. dispatch_id names the call's dispatch when it reached one: look it up with toolyard.audit_mine({ dispatch_id }). */
export type TryResult<T = unknown> =
  | { ok: true; status: "succeeded"; result: T; dispatch_id?: string }
  | { ok: false; status: "failed" | "permission_required" | "deferred" | "outcome_unknown"; error: string; approval_id?: string; dispatch_id?: string };

export interface SearchOptions {
  /** auto (the default: by meaning with Jev where the operator turned it on, else keyword), semantic (Jev, refused where it is off; falls back to keyword when Jev fails), keyword (ranked words) or regex (an RE2 pattern of at most 200 characters, case-insensitive). */
  mode?: "auto" | "semantic" | "keyword" | "regex";
  /** name, summary (the default) or full (with the input and output schemas). */
  detail?: "name" | "summary" | "full";
  /** At most this many results: 50 by default, 200 at most. */
  limit?: number;
  /** Only the tools of this upstream server. */
  upstream?: string;
}

export interface SearchResult {
  /** The catalog name. */
  name: string;
  /** The name an MCP client's tools/list shows for it, where the gateway lists OpenAI-safe names (inbox_request for inbox.request); scripts keep using code. */
  mcp_name?: string;
  upstream?: string;
  description?: string;
  /** The name to call it by: tools.<code>, or tools.call("<code>", args). */
  code?: string;
  /** Jev's probability (0 to 1) that the tool fits when Jev ranked, else the keyword or regex score; absent when not scored. */
  score?: number;
  /** What a call gets now: "now" runs; "needs_approval" needs your owner's yes (ask with toolyard.inbox_request, every restricted call of this round in one request); "blocked" is denied by policy. Absent at detail "name". */
  run?: "now" | "needs_approval" | "blocked";
  input_schema?: Record<string, unknown>;
  output_schema?: Record<string, unknown>;
}

/** A tool function, such as tools.Linear.get_issue. */
export type ToolFunction = (args?: Record<string, unknown>) => Promise<unknown>;

export interface ToolHelpers {
  /** Call a tool by name ("Server.tool"); a failure rejects, as a direct call does. */
  call(name: string, args?: Record<string, unknown>): Promise<unknown>;
  /** Call a tool and get its outcome as data; a failed or held call does not reject. */
  tryCall<T = unknown>(tool: string | ToolFunction, args?: Record<string, unknown>): Promise<TryResult<T>>;
  /** Run the functions at most limit at a time (4 by default, 8 at most); rejects on the first failure, like Promise.all. */
  parallel<T>(fns: Array<() => Promise<T>>, opts?: { limit?: number }): Promise<T[]>;
  /** Search the tools you may use (toolyard.tools_search); the results only. tools.call("toolyard.tools_search", { query }) returns the whole answer, with ranked_by and fallback. */
  search(query: string, opts?: SearchOptions): Promise<SearchResult[]>;
}
`

// tsIndex is servers/index.d.ts.
func tsIndex(cat *catalog) string {
	var b strings.Builder
	b.WriteString(tsIndexHead)
	b.WriteString("\nexport interface Tools extends ToolHelpers {\n")
	for _, s := range cat.servers {
		if tsHelperNames[s.ident] {
			fmt.Fprintf(&b, "  // Server %q: call its tools by name, tools.call(\"%s.<tool>\", args).\n", s.name, s.ident)
			continue
		}
		fmt.Fprintf(&b, "  %s: import(%q).Tools;\n", tsKey(s.ident), "./"+s.ident)
	}
	b.WriteString("}\n")
	if notes := cat.refusedNotes(); len(notes) > 0 {
		b.WriteString("\n")
		for _, note := range notes {
			b.WriteString("//" + strings.TrimPrefix(note, "#") + "\n")
		}
	}
	b.WriteString(`
declare global {
  const tools: Tools;
  const console: {
    log(...args: unknown[]): void;
    info(...args: unknown[]): void;
    warn(...args: unknown[]): void;
    error(...args: unknown[]): void;
    debug(...args: unknown[]): void;
  };
}
`)
	return b.String()
}

// tsExample is the argument object of the usage line: the first required
// parameter (alphabetically), or {} when there is none.
func tsExample(props map[string]any, required []string) string {
	names := append([]string(nil), required...)
	sort.Strings(names)
	for _, n := range names {
		if p, ok := props[n]; ok {
			return fmt.Sprintf("{ %s: %s }", tsKey(n), tsSample(p))
		}
	}
	return "{}"
}

// tsSample is a placeholder value of a parameter's type.
func tsSample(v any) string {
	p, _ := v.(map[string]any)
	if enum := enumValues(p); len(enum) > 0 {
		return tsLiteral(enum[0])
	}
	switch {
	case hasType(p, "string"):
		return `"..."`
	case hasType(p, "integer"), hasType(p, "number"):
		return "0"
	case hasType(p, "boolean"):
		return "true"
	case hasType(p, "array"):
		return "[]"
	case hasType(p, "object"):
		return "{}"
	}
	return `"..."`
}

// tsType renders one schema as a type; indent is the indentation of the
// line the type starts on.
func tsType(v any, indent string, depth int) string {
	p, ok := v.(map[string]any)
	if !ok || depth > tsMaxDepth {
		return "unknown"
	}
	if enum := enumValues(p); len(enum) > 0 {
		parts := make([]string, len(enum))
		for i, e := range enum {
			parts[i] = tsLiteral(e)
		}
		return tsUnion(parts)
	}
	if c, ok := p["const"]; ok {
		return tsLiteral(c)
	}
	if _, ok := p["$ref"]; ok {
		return "unknown"
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if alts, ok := p[key].([]any); ok && len(alts) > 0 {
			parts := make([]string, len(alts))
			for i, a := range alts {
				parts[i] = tsType(a, indent, depth+1)
			}
			return tsUnion(parts)
		}
	}
	if all, ok := p["allOf"].([]any); ok && len(all) > 0 {
		parts := make([]string, len(all))
		for i, a := range all {
			parts[i] = tsParen(tsType(a, indent, depth+1))
		}
		return strings.Join(parts, " & ")
	}
	types := typeNames(p)
	if len(types) == 0 {
		switch {
		case p["properties"] != nil:
			types = []string{"object"}
		case p["items"] != nil:
			types = []string{"array"}
		default:
			return "unknown"
		}
	}
	parts := make([]string, 0, len(types))
	for _, t := range types {
		switch t {
		case "string", "boolean", "null":
			parts = append(parts, t)
		case "number", "integer":
			parts = append(parts, "number")
		case "array":
			parts = append(parts, tsArray(p, indent, depth))
		case "object":
			parts = append(parts, tsObject(p, indent, depth))
		default:
			parts = append(parts, "unknown")
		}
	}
	return tsUnion(parts)
}

// tsArray renders an array schema.
func tsArray(p map[string]any, indent string, depth int) string {
	switch items := p["items"].(type) {
	case map[string]any:
		return tsParen(tsType(items, indent, depth+1)) + "[]"
	case []any:
		parts := make([]string, len(items))
		for i, it := range items {
			parts[i] = tsType(it, indent, depth+1)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return "unknown[]"
}

// tsObject renders an object schema.
func tsObject(p map[string]any, indent string, depth int) string {
	props, _ := p["properties"].(map[string]any)
	if len(props) == 0 {
		switch addl := p["additionalProperties"].(type) {
		case map[string]any:
			return "Record<string, " + tsType(addl, indent, depth+1) + ">"
		case bool:
			if !addl {
				return "Record<string, never>"
			}
		}
		return "Record<string, unknown>"
	}
	return tsObjectBody(props, stringSet(p["required"]), p["additionalProperties"], indent, depth)
}

// tsObjectBody is an object type's braces and members, required members
// first, each group alphabetical, each described by its JSDoc.
func tsObjectBody(props map[string]any, required map[string]bool, addl any, indent string, depth int) string {
	_, indexed := addl.(map[string]any)
	if b, ok := addl.(bool); ok && b {
		indexed = true
	}
	if len(props) == 0 && !indexed {
		return "{}"
	}
	var req, opt []string
	for name := range props {
		if required[name] {
			req = append(req, name)
		} else {
			opt = append(opt, name)
		}
	}
	sort.Strings(req)
	sort.Strings(opt)
	inner := indent + "  "
	var b strings.Builder
	b.WriteString("{\n")
	for _, name := range append(req, opt...) {
		ps, _ := props[name].(map[string]any)
		writeDoc(&b, inner, propDoc(ps))
		mark := "?"
		if required[name] {
			mark = ""
		}
		fmt.Fprintf(&b, "%s%s%s: %s;\n", inner, tsKey(name), mark, tsType(props[name], inner, depth+1))
	}
	if indexed {
		fmt.Fprintf(&b, "%s[key: string]: unknown;\n", inner)
	}
	b.WriteString(indent + "}")
	return b.String()
}

// propDoc is a property's JSDoc text: its description, and its default.
func propDoc(p map[string]any) string {
	desc, _ := p["description"].(string)
	desc = strings.TrimSpace(desc)
	if d, ok := p["default"]; ok {
		if raw, err := json.Marshal(d); err == nil {
			if desc != "" {
				desc += "\n"
			}
			desc += "@default " + string(raw)
		}
	}
	return desc
}

// writeDoc writes text as a JSDoc comment at indent: one line when it is
// one line, else a block. Empty text writes nothing.
func writeDoc(b *strings.Builder, indent, text string) {
	if text == "" {
		return
	}
	text = strings.ReplaceAll(text, "*/", "*\\/")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) == 1 {
		fmt.Fprintf(b, "%s/** %s */\n", indent, lines[0])
		return
	}
	fmt.Fprintf(b, "%s/**\n", indent)
	for _, l := range lines {
		if l = strings.TrimRight(l, " \t"); l == "" {
			fmt.Fprintf(b, "%s *\n", indent)
		} else {
			fmt.Fprintf(b, "%s * %s\n", indent, l)
		}
	}
	fmt.Fprintf(b, "%s */\n", indent)
}

// tsUnion joins alternatives, dropping repeats.
func tsUnion(parts []string) string {
	seen := map[string]bool{}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return strings.Join(out, " | ")
}

// tsParen wraps a type in parentheses when it has a top-level | or &.
func tsParen(t string) string {
	depth := 0
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case '{', '[', '(', '<':
			depth++
		case '}', ']', ')', '>':
			depth--
		case '"':
			// Skip a string literal.
			for i++; i < len(t) && t[i] != '"'; i++ {
				if t[i] == '\\' {
					i++
				}
			}
		case '|', '&':
			if depth == 0 {
				return "(" + t + ")"
			}
		}
	}
	return t
}

// tsKey is a property name as TypeScript writes it: bare when it is an
// identifier, quoted otherwise.
func tsKey(name string) string {
	if name == "" {
		return `""`
	}
	for i, r := range name {
		ok := r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return tsLiteral(name)
		}
	}
	return name
}

// tsLiteral is a JSON value as a TypeScript literal type.
func tsLiteral(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "unknown"
	}
	return string(raw)
}

// enumValues are a schema's enum values ([]any from JSON, []string from a
// built-in tool's Go schema).
func enumValues(p map[string]any) []any {
	switch v := p["enum"].(type) {
	case []any:
		return v
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out
	}
	return nil
}

// typeNames is a schema's "type", a string or a list.
func typeNames(p map[string]any) []string {
	switch v := p["type"].(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, t := range v {
			if s, ok := t.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	}
	return nil
}

func hasType(p map[string]any, t string) bool {
	for _, x := range typeNames(p) {
		if x == t {
			return true
		}
	}
	return false
}

// stringSet reads a "required" list.
func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	switch l := v.(type) {
	case []string:
		for _, s := range l {
			out[s] = true
		}
	case []any:
		for _, x := range l {
			if s, ok := x.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}
