package codemode

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// The text below mirrors Bifrost's tool-level binding (one .pyi per tool)
// line for line, including the header comments an agent has learnt to
// expect. Where toolyard has nothing to say (a caller with no servers) the
// wording is toolyard's own.

// noServersText answers listToolFiles for a caller who may use nothing.
const noServersText = "No servers are available to you. There are no virtual .pyi files available. " +
	"Ask a toolyard admin to grant you a server, then call listToolFiles again."

// ListToolFiles renders the servers/<Server>/<tool>.pyi tree for the ctx
// caller.
func (r *Runtime) ListToolFiles(ctx context.Context) string {
	cat := buildCatalog(r.caller.Tools(ctx))
	var files []string
	for _, s := range cat.servers {
		for _, b := range s.bound {
			files = append(files, fmt.Sprintf("servers/%s/%s.pyi", s.ident, b.ident))
		}
	}
	if len(files) == 0 {
		return noServersText
	}
	lines := []string{
		"# Workflow: listToolFiles -> readToolFile -> (optional) getToolDocs -> executeToolCode",
		"# Filenames below use the exact canonical tool identifiers available in executeToolCode.",
		"# Still call readToolFile before executeToolCode to confirm parameters and return shape.",
	}
	lines = append(lines, cat.refusedNotes()...)
	lines = append(lines, "", renderTree(files))
	return strings.Join(lines, "\n")
}

// renderTree lays a sorted list of slash paths out as an indented tree,
// directories suffixed with '/'.
func renderTree(files []string) string {
	type node struct {
		children map[string]*node
		dir      bool
	}
	root := &node{children: map[string]*node{}, dir: true}
	for _, f := range files {
		cur := root
		parts := strings.Split(f, "/")
		for i, p := range parts {
			next, ok := cur.children[p]
			if !ok {
				next = &node{children: map[string]*node{}, dir: i < len(parts)-1}
				cur.children[p] = next
			}
			cur = next
		}
	}
	var lines []string
	var walk func(n *node, indent string)
	walk = func(n *node, indent string) {
		keys := make([]string, 0, len(n.children))
		for k := range n.children {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := n.children[k]
			if child.dir {
				lines = append(lines, indent+k+"/")
				walk(child, indent+"  ")
			} else {
				lines = append(lines, indent+k)
			}
		}
	}
	walk(root, "")
	return strings.Join(lines, "\n")
}

// ReadToolFile renders one virtual .pyi file, servers/<Server>/<tool>.pyi
// for a tool or servers/<Server>.pyi for a whole server, matched
// case-insensitively with the extension optional. startLine and endLine
// (1-based, inclusive) slice the result; out-of-range values are clamped.
// ok is false when the answer is an explanation rather than a file.
func (r *Runtime) ReadToolFile(ctx context.Context, fileName string, startLine, endLine *int) (text string, ok bool) {
	cat := buildCatalog(r.caller.Tools(ctx))
	serverName, toolName, toolLevel, valid := parseFilePath(fileName)
	if !valid {
		return fmt.Sprintf("Invalid filename '%s'. Use servers/<serverName>/<toolName>.pyi as listed by listToolFiles.", fileName), false
	}
	srv, several, refused := findServer(cat, serverName)
	if len(refused) > 0 {
		return refusedServerText(serverName, refused), false
	}
	if len(several) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "Multiple servers match filename '%s':\n", fileName)
		for _, n := range several {
			fmt.Fprintf(&b, "  - %s\n", n)
		}
		b.WriteString("\nPlease use a more specific filename. Use the exact display name from listToolFiles to avoid ambiguity.")
		return b.String(), false
	}
	if srv == nil {
		var b strings.Builder
		fmt.Fprintf(&b, "No server found matching '%s'. Available virtual files are:\n", serverName)
		for _, s := range cat.servers {
			for _, bd := range s.bound {
				fmt.Fprintf(&b, "  - servers/%s/%s.pyi\n", s.ident, bd.ident)
			}
		}
		return b.String(), false
	}

	var tools []binding
	if toolLevel {
		b, names := srv.lookup(toolName)
		if len(names) > 0 {
			return ambiguityError(srv.ident, toolName, names).Error(), false
		}
		if b == nil {
			var sb strings.Builder
			fmt.Fprintf(&sb, "Tool '%s' not found in server '%s'. Available tools in this server are:\n", toolName, srv.ident)
			for _, bd := range srv.bound {
				fmt.Fprintf(&sb, "  - servers/%s/%s.pyi\n", srv.ident, bd.ident)
			}
			return sb.String(), false
		}
		tools = []binding{*b}
	} else {
		tools = srv.bound
	}

	body := compactSignatures(srv, tools, toolLevel)
	// The header line counts itself: Bifrost prepends it after measuring.
	total := len(strings.Split(body, "\n")) + 1
	content := fmt.Sprintf("# Total lines: %d (this is the complete file, no need to paginate)\n%s", total, body)
	if startLine == nil && endLine == nil {
		return content, true
	}
	lines := strings.Split(content, "\n")
	start, end := 1, len(lines)
	if startLine != nil {
		start = *startLine
	}
	if endLine != nil {
		end = *endLine
	}
	start = clamp(start, 1, len(lines))
	end = clamp(end, 1, len(lines))
	if start > end {
		end = start
	}
	return strings.Join(lines[start-1:end], "\n"), true
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// parseFilePath splits servers/<server>/<tool>.pyi (or servers/<server>.pyi)
// into its parts. valid is false for traversal attempts and deeper paths.
func parseFilePath(fileName string) (serverName, toolName string, toolLevel, valid bool) {
	p := strings.TrimSpace(fileName)
	p = strings.TrimSuffix(p, ".pyi")
	p = strings.TrimPrefix(p, "servers/")
	if p == "" || strings.Contains(p, "..") {
		return "", "", false, false
	}
	parts := strings.Split(p, "/")
	switch len(parts) {
	case 1:
		return parts[0], "", false, true
	case 2:
		if parts[1] == "" {
			return parts[0], "", false, true
		}
		return parts[0], parts[1], true, true
	}
	return "", "", false, false
}

// compactSignatures is the body of a .pyi file: a comment header and one
// def line per tool.
func compactSignatures(srv *server, tools []binding, toolLevel bool) string {
	var b strings.Builder
	if toolLevel && len(tools) == 1 {
		fmt.Fprintf(&b, "# %s.%s tool\n", srv.ident, tools[0].ident)
	} else {
		fmt.Fprintf(&b, "# %s server tools\n", srv.ident)
	}
	if srv.ident != srv.name {
		fmt.Fprintf(&b, "# Server %q is called %s in code.\n", srv.name, srv.ident)
	}
	fmt.Fprintf(&b, "# Usage: %s.tool_name(param=value)\n", srv.ident)
	b.WriteString("# The def names below are the exact callable names to use in executeToolCode.\n")
	b.WriteString("# Read this file before executeToolCode to confirm parameters and return shape.\n")
	fmt.Fprintf(&b, "# For detailed docs: use getToolDocs(server=%q, tool=\"tool_name\")\n", srv.ident)
	b.WriteString("# Note: Descriptions may be truncated. Use getToolDocs for full details.\n")
	if !toolLevel && len(srv.ambiguous) > 0 {
		idents := make([]string, 0, len(srv.ambiguous))
		for ident := range srv.ambiguous {
			idents = append(idents, ident)
		}
		sort.Strings(idents)
		for _, ident := range idents {
			fmt.Fprintf(&b, "# Not callable: %s (ambiguous, could be %s)\n", ident, strings.Join(quoteAll(srv.ambiguous[ident]), " or "))
		}
	}
	b.WriteString("\n")
	for _, t := range tools {
		params := pythonParams(t.tool.Properties, t.tool.Required)
		if desc := shortDescription(t.tool.Description); desc != "" {
			fmt.Fprintf(&b, "def %s(%s) -> dict:  # %s\n", t.ident, params, desc)
		} else {
			fmt.Fprintf(&b, "def %s(%s) -> dict\n", t.ident, params)
		}
	}
	return b.String()
}

// shortDescription is the first sentence when it ends within 80 bytes,
// else the first 77 bytes and an ellipsis.
func shortDescription(desc string) string {
	desc = strings.TrimSpace(desc)
	if idx := strings.Index(desc, ". "); idx > 0 && idx < 80 {
		return desc[:idx+1]
	}
	if len(desc) > 80 {
		return desc[:77] + "..."
	}
	return desc
}

// pythonParams renders required parameters first, then optional ones with
// `= None`, each group alphabetical.
func pythonParams(props map[string]any, required []string) string {
	req := map[string]bool{}
	for _, name := range required {
		req[name] = true
	}
	var reqNames, optNames []string
	for name := range props {
		if req[name] {
			reqNames = append(reqNames, name)
		} else {
			optNames = append(optNames, name)
		}
	}
	sort.Strings(reqNames)
	sort.Strings(optNames)
	parts := make([]string, 0, len(props))
	for _, name := range reqNames {
		parts = append(parts, fmt.Sprintf("%s: %s", name, pythonType(props[name])))
	}
	for _, name := range optNames {
		parts = append(parts, fmt.Sprintf("%s: %s = None", name, pythonType(props[name])))
	}
	return strings.Join(parts, ", ")
}

// pythonType maps one JSON Schema property to a Python type annotation.
func pythonType(prop any) string {
	p, ok := prop.(map[string]any)
	if !ok {
		return "Any"
	}
	// An enum decoded from JSON is []any; one a built-in tool declares in
	// Go is []string.
	var enum []any
	switch v := p["enum"].(type) {
	case []any:
		enum = v
	case []string:
		for _, s := range v {
			enum = append(enum, s)
		}
	}
	if len(enum) > 0 {
		vals := make([]string, len(enum))
		for i, e := range enum {
			vals[i] = literal(e)
		}
		return "Literal[" + strings.Join(vals, ", ") + "]"
	}
	if c, ok := p["const"]; ok {
		return "Literal[" + literal(c) + "]"
	}
	switch p["type"] {
	case "string":
		return "str"
	case "number":
		return "float"
	case "integer":
		return "int"
	case "boolean":
		return "bool"
	case "array":
		items := "Any"
		if it, ok := p["items"].(map[string]any); ok {
			items = pythonType(it)
		}
		return "list[" + items + "]"
	case "object":
		return "dict"
	case "null":
		return "None"
	}
	return "Any"
}

func literal(v any) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%v", v)
}

// GetToolDocs renders the full documentation of one tool. ok is false when
// the answer is an explanation rather than documentation.
func (r *Runtime) GetToolDocs(ctx context.Context, serverName, toolName string) (text string, ok bool) {
	cat := buildCatalog(r.caller.Tools(ctx))
	srv, several, refused := findServer(cat, serverName)
	if len(refused) > 0 {
		return refusedServerText(serverName, refused), false
	}
	if len(several) > 0 {
		return fmt.Sprintf("Multiple servers match '%s': %s. Use the exact display name from listToolFiles.",
			serverName, strings.Join(several, ", ")), false
	}
	if srv == nil {
		var b strings.Builder
		fmt.Fprintf(&b, "Server '%s' not found. Available servers are:\n", serverName)
		for _, s := range cat.servers {
			fmt.Fprintf(&b, "  - %s\n", s.ident)
		}
		return b.String(), false
	}
	bd, names := srv.lookup(toolName)
	if len(names) > 0 {
		return ambiguityError(srv.ident, toolName, names).Error(), false
	}
	if bd == nil {
		var b strings.Builder
		fmt.Fprintf(&b, "Tool '%s' not found in server '%s'. Available tools are:\n", toolName, srv.ident)
		for _, x := range srv.bound {
			fmt.Fprintf(&b, "  - %s\n", x.ident)
		}
		return b.String(), false
	}
	return toolDocs(srv.ident, *bd), true
}

// toolDocs is the getToolDocs body: header, signature and a docstring.
func toolDocs(serverName string, bd binding) string {
	var b strings.Builder
	rule := "# ============================================================================\n"
	b.WriteString(rule)
	fmt.Fprintf(&b, "# Documentation for %s.%s tool\n", serverName, bd.ident)
	b.WriteString(rule)
	b.WriteString("#\n")
	b.WriteString("# This file contains Python documentation for a specific tool on this MCP server.\n")
	b.WriteString("#\n")
	b.WriteString("# USAGE INSTRUCTIONS:\n")
	fmt.Fprintf(&b, "# Call tools using: result = %s.tool_name(param=value)\n", serverName)
	b.WriteString("# No async/await needed - calls are synchronous.\n")
	b.WriteString("#\n")
	b.WriteString("# STARLARK DIFFERENCE FROM PYTHON:\n")
	b.WriteString("# for/if/while at top level MUST be inside a function.\n")
	b.WriteString("# Wrap loops: def main(): for x in items: ... then result = main()\n")
	b.WriteString("#\n")
	b.WriteString("# CRITICAL - HANDLING RESPONSES:\n")
	b.WriteString("# Tool responses are dicts. To avoid runtime errors:\n")
	b.WriteString("# 1. Use print(result) to inspect the response structure first\n")
	b.WriteString("# 2. Access dict values with brackets: result[\"key\"] NOT result.key\n")
	b.WriteString("# 3. Use .get() for safe access: result.get(\"key\", default)\n")
	b.WriteString("#\n")
	b.WriteString("# Common error: \"key not found\" or \"has no attribute\"\n")
	b.WriteString("# Fix: Use print() to see actual structure, then use result[\"key\"] or .get()\n")
	b.WriteString(rule)
	b.WriteString("\n")

	t := bd.tool
	fmt.Fprintf(&b, "def %s(%s) -> dict:\n", bd.ident, pythonParams(t.Properties, t.Required))
	b.WriteString("    \"\"\"\n")
	if desc := strings.TrimSpace(t.Description); desc != "" {
		fmt.Fprintf(&b, "    %s\n\n", desc)
	}
	if len(t.Properties) > 0 {
		req := map[string]bool{}
		for _, n := range t.Required {
			req[n] = true
		}
		names := make([]string, 0, len(t.Properties))
		for n := range t.Properties {
			names = append(names, n)
		}
		sort.Strings(names)
		b.WriteString("    Args:\n")
		for _, n := range names {
			desc := n + " parameter"
			if p, ok := t.Properties[n].(map[string]any); ok {
				if d, ok := p["description"].(string); ok && d != "" {
					desc = d
				}
			}
			note := " (optional)"
			if req[n] {
				note = " (required)"
			}
			fmt.Fprintf(&b, "        %s (%s): %s%s\n", n, pythonType(t.Properties[n]), desc, note)
		}
		b.WriteString("\n")
	}
	b.WriteString("    Returns:\n")
	b.WriteString("        dict: Response from the tool. Structure varies by tool.\n")
	b.WriteString("              Use print(result) to inspect the actual structure.\n")
	b.WriteString("\n")
	b.WriteString("    Example:\n")
	fmt.Fprintf(&b, "        result = %s.%s(%s)\n", serverName, bd.ident, exampleParams(t.Properties, t.Required))
	b.WriteString("        print(result)  # Always inspect response first!\n")
	b.WriteString("        value = result.get(\"key\", default)  # Safe access\n")
	b.WriteString("    \"\"\"\n")
	b.WriteString("    ...\n\n")
	return b.String()
}

// exampleParams names the first required parameter (alphabetically), or
// the first parameter at all, as `name="..."`.
func exampleParams(props map[string]any, required []string) string {
	if len(props) == 0 {
		return ""
	}
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	req := map[string]bool{}
	for _, n := range required {
		req[n] = true
	}
	for _, n := range names {
		if req[n] {
			return fmt.Sprintf("%s=\"...\"", n)
		}
	}
	return fmt.Sprintf("%s=\"...\"", names[0])
}

// refusedServerText explains a server key that has no identifier of its
// own.
func refusedServerText(name string, keys []string) string {
	if len(keys) == 1 {
		return fmt.Sprintf("Server '%s' cannot be called from code: no Starlark identifier can be made of its name. Use tools.execute with the exact catalog name instead.", name)
	}
	return fmt.Sprintf("Server '%s' is ambiguous in code: %s all map to the same identifier. toolyard does not guess; use tools.execute with the exact catalog name instead.",
		name, strings.Join(quoteAll(keys), ", "))
}
