package codemode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
)

// heldMarkers are the result metadata keys the gateway sets when a call
// did not run: queued for approval, approved but still executing, or
// coached towards inbox.request. Starlark has no try/except, so such a
// result aborts the script like any tool error, carrying the gateway's
// text (approval id, next steps) into the error output.
var heldMarkers = []string{"toolyard.deferred", "toolyard.executing", "toolyard.permission_required"}

// ExecuteToolCode runs a script for the ctx caller. reason is the outer
// call's _reason, or "" when the client sent none (Bifrost clients never
// do); nested calls carry it, or one derived from the script. failed is
// true for a syntax error, a runtime error, a tool call that did not
// succeed, or a worker that did not survive the script.
func (r *Runtime) ExecuteToolCode(ctx context.Context, code, reason string) (text string, failed bool) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "code parameter is required and must be a non-empty string", true
	}
	if strings.TrimSpace(reason) == "" {
		reason = deriveReason(code)
	}
	cat := buildCatalog(r.caller.Tools(ctx))

	stopNote := fmt.Sprintf("limit %s", r.limits.ScriptTimeout)
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < r.limits.ScriptTimeout {
		stopNote = "the caller's deadline"
	}
	runCtx, cancel := context.WithTimeout(ctx, r.limits.ScriptTimeout)
	defer cancel()

	s := &session{r: r, ctx: runCtx, reason: reason, cat: cat}
	start := startMsg{Type: msgStart, Code: code, Servers: cat.wire(), Limits: r.limits}
	var res scriptResult
	if r.inProcess {
		res = runScript(runCtx, start, s, stopNote)
	} else {
		res = r.runInWorker(runCtx, s, start, stopNote)
	}
	return r.render(code, cat.idents(), s.snapshotLogs(), res)
}

// session is the parent's state for one script: the print log, the call
// budget, and how nested calls are made.
type session struct {
	r      *Runtime
	ctx    context.Context
	reason string
	cat    *catalog

	mu        sync.Mutex
	logs      []string
	logBytes  int
	truncated bool
	calls     int
}

// handleCall performs one nested call for the script. Every failure is
// returned as the message that aborts the script, naming the tool and
// carrying the gateway's text.
func (s *session) handleCall(serverIdent, member string, args map[string]any) parentMsg {
	fail := func(msg string) parentMsg { return parentMsg{Error: msg} }
	srv := s.cat.byIdent(serverIdent)
	if srv == nil {
		return fail(fmt.Sprintf("tool call failed: unknown server %q", serverIdent))
	}
	b, ok := srv.byIdent[member]
	if !ok {
		if names, amb := srv.ambiguous[canonicalName(member)]; amb {
			return fail(ambiguityError(srv.ident, member, names).Error())
		}
		return fail(fmt.Sprintf("tool call failed: %s has no tool %q", srv.ident, member))
	}
	t := b.tool
	label := srv.ident + "." + strings.ReplaceAll(t.Name, "-", "_")
	if args == nil {
		args = map[string]any{}
	}

	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if n > s.r.limits.MaxCalls {
		msg := fmt.Sprintf("this script already made %d tool calls, the limit for one executeToolCode run; split the work across runs", s.r.limits.MaxCalls)
		s.log(fmt.Sprintf("[TOOL] %s error: %s", label, msg))
		return fail(fmt.Sprintf("tool call failed for %s: %s", label, msg))
	}
	if err := s.ctx.Err(); err != nil {
		return fail(fmt.Sprintf("tool call failed for %s: script stopped: %v", label, err))
	}
	if field := t.ReasonField; field != "" {
		switch given := args[field].(type) {
		case nil:
			args[field] = s.reason
		case string:
			if strings.TrimSpace(given) == "" {
				args[field] = s.reason
			}
		default:
			s.log(fmt.Sprintf("[TOOL] %s error: %s must be a string", label, field))
			return fail(fmt.Sprintf("tool call failed for %s: %s must be a string", label, field))
		}
	}

	res, err := s.r.caller.RouteCall(s.ctx, Via, t.Target, args)
	if err != nil {
		s.log(fmt.Sprintf("[TOOL] %s error: %v", label, err))
		return fail(fmt.Sprintf("tool call failed for %s: %v", label, err))
	}
	text := resultText(res, t.Name)
	if res != nil && (res.IsError || held(res)) {
		// The failure text aborts the script; bound it before it is logged
		// or sent, so a huge error result is still a clean abort.
		text = cut(text, maxErrorBytes, "… [error text truncated at 64 KiB]")
		s.log(fmt.Sprintf("[TOOL] %s error result: %s", label, text))
		return fail(fmt.Sprintf("tool call failed for %s: %s", label, text))
	}
	quoted, err := jsonText(text, "")
	if err != nil || len(quoted) > maxCallResultBytes {
		// The cap is on the JSON text the wire carries, which escaping can
		// make larger than the result itself; say both sizes.
		msg := fmt.Sprintf("the result is %d bytes (%d as JSON text), more than code mode passes to a script (limit %d MiB); ask the tool for less data, or page through it",
			len(text), len(quoted), maxCallResultBytes>>20)
		s.log(fmt.Sprintf("[TOOL] %s error: %s", label, msg))
		return fail(fmt.Sprintf("tool call failed for %s: %s", label, msg))
	}
	s.log(fmt.Sprintf("[TOOL] %s raw response: %s", label, quoted))
	return parentMsg{Text: text}
}

// call and print make a session the in-process script's sink.
func (s *session) call(server, member string, args map[string]any) parentMsg {
	return s.handleCall(server, member, args)
}

func (s *session) print(text string) { s.log(text) }

// log appends one line, up to the output limit.
func (s *session) log(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.truncated {
		return
	}
	if s.logBytes+len(msg) > s.r.limits.MaxOutputBytes {
		s.truncated = true
		s.logs = append(s.logs, fmt.Sprintf("[print output truncated: %d byte limit reached]", s.r.limits.MaxOutputBytes))
		return
	}
	s.logBytes += len(msg) + 1
	s.logs = append(s.logs, msg)
}

func (s *session) snapshotLogs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.logs...)
}

// held reports a result whose call did not run.
func held(res *mcp.CallToolResult) bool {
	if res.Meta == nil {
		return false
	}
	for _, k := range heldMarkers {
		if v, ok := res.Meta.AdditionalFields[k].(bool); ok && v {
			return true
		}
	}
	return false
}

// resultText flattens a tool result to the text a script sees.
func resultText(res *mcp.CallToolResult, toolName string) string {
	if res == nil {
		return fmt.Sprintf("MCP tool '%s' executed successfully", toolName)
	}
	var b strings.Builder
	for _, c := range res.Content {
		switch v := c.(type) {
		case mcp.TextContent:
			b.WriteString(v.Text)
		case *mcp.TextContent:
			b.WriteString(v.Text)
		case mcp.ImageContent:
			fmt.Fprintf(&b, "[Image Response: MIME %s, %d bytes]\n", v.MIMEType, len(v.Data))
		case mcp.AudioContent:
			fmt.Fprintf(&b, "[Audio Response: MIME %s, %d bytes]\n", v.MIMEType, len(v.Data))
		case mcp.EmbeddedResource:
			b.WriteString("[Embedded Resource Response]\n")
		default:
			if s, err := jsonText(c, ""); err == nil {
				b.WriteString(s)
			}
		}
	}
	if b.Len() == 0 && res.StructuredContent != nil {
		if s, err := jsonText(res.StructuredContent, ""); err == nil {
			return s
		}
	}
	if b.Len() == 0 {
		return fmt.Sprintf("MCP tool '%s' executed successfully", toolName)
	}
	return strings.TrimSpace(b.String())
}

// render is Bifrost's response text for each outcome.
func (r *Runtime) render(code string, serverKeys, logs []string, res scriptResult) (string, bool) {
	keys := strings.Join(serverKeys, ", ")
	var text string
	failed := false
	switch {
	case res.errKind != "":
		failed = true
		hints := errorHints(res.errKind, res.errMsg, usesExceptions(code), serverKeys)
		logText := ""
		if len(logs) > 0 {
			logText = fmt.Sprintf("\n\nPrint Output:\n%s\n", strings.Join(logs, "\n"))
		}
		text = fmt.Sprintf("Execution %s error:\n\n%s\n\nHints:\n%s%s\n\nEnvironment:\n  Available server keys: %s",
			res.errKind, res.errMsg, strings.Join(hints, "\n"), logText, keys)
	case len(logs) == 0 && !res.hasResult:
		hints := []string{
			"Add print() statements throughout your code to debug and see what's happening at each step",
			"Assign the final value to 'result' variable if you want to return it: result = computed_value",
			"Check that your tool calls are actually executing and returning data",
		}
		text = fmt.Sprintf("Execution completed but produced no data:\n\n"+
			"The code executed without errors but returned no output (no print output and no result variable).\n\n"+
			"Hints:\n%s\n\nEnvironment:\n  Available server keys: %s", strings.Join(hints, "\n"), keys)
	default:
		if len(logs) > 0 {
			text = fmt.Sprintf("Print output:\n%s\n\nExecution completed successfully.", strings.Join(logs, "\n"))
		} else {
			text = "Execution completed successfully."
		}
		if res.hasResult {
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, res.result, "", "  "); err == nil {
				text += "\nReturn value: " + pretty.String()
			} else {
				text += "\nReturn value: " + string(res.result)
			}
		}
		text += "\n\nEnvironment:\n  Available server keys: " + keys
		text += "\nNote: This is a Starlark (Python subset) environment. Use MCP tools for external interactions."
	}
	if max := r.limits.MaxOutputBytes; len(text) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + fmt.Sprintf("\n\n[output truncated at %d bytes]", max)
	}
	return text, failed
}

// deriveReason is the reason nested calls carry when the outer call had
// none: "code mode: " and the script's first comment line, or its first
// 120 characters on one line. It varies with the script, so a detector
// watching for one reason repeated does not fire on code mode, and it is
// padded past the gateway's minimum length when the script is tiny.
func deriveReason(code string) string {
	var snippet string
	var codeLines []string
	for _, line := range strings.Split(code, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
		case strings.HasPrefix(t, "#"):
			if snippet == "" {
				snippet = strings.TrimSpace(strings.TrimLeft(t, "#"))
			}
		default:
			codeLines = append(codeLines, t)
		}
	}
	if snippet == "" {
		snippet = strings.Join(strings.Fields(strings.Join(codeLines, " ")), " ")
	}
	if snippet == "" {
		snippet = strings.Join(strings.Fields(code), " ")
	}
	if n := utf8.RuneCountInString(snippet); n > 120 {
		runes := []rune(snippet)
		snippet = strings.TrimSpace(string(runes[:117])) + "..."
	}
	reason := "code mode: " + snippet
	if len(reason) < 20 {
		reason += " (executeToolCode script)"
	}
	return reason
}

var undefinedRE = []*regexp.Regexp{
	regexp.MustCompile(`name ['"]([^'"]+)['"] is not defined`),
	regexp.MustCompile(`undefined:\s*([A-Za-z_][A-Za-z0-9_]*)`),
	regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)[^A-Za-z0-9_]+(?:undefined|not defined)`),
}

var exceptionLineRE = regexp.MustCompile(`(?m)^\s*(?:try\s*:|except\b|finally\s*:|raise\b)`)

// usesExceptions reports a script written with Python exception handling,
// which Starlark has no syntax for.
func usesExceptions(code string) bool { return exceptionLineRE.MatchString(code) }

// errorHints are the debugging hints for a failed script. Adapted from
// maximhq/bifrost core/mcp/codemode (starlark/utils.go, Apache-2.0): the
// wording is what agents already know how to act on. Triggers differ where
// starlark-go's real messages do: parse errors carry no "syntax error", so
// the kind decides, and a dict miss reads `not in dict`.
func errorHints(kind, msg string, exceptions bool, keys []string) []string {
	serverKeys := ""
	if len(keys) > 0 {
		serverKeys = "Available server keys: " + strings.Join(keys, ", ")
	}
	var hints []string
	switch {
	case exceptions && kind == "syntax":
		hints = append(hints,
			"Starlark does NOT support try/except/finally/raise — there is no exception handling.",
			"Instead, check return values for errors:",
			"  result = server.tool(param=\"value\")",
			"  if result == None or (type(result) == \"dict\" and \"error\" in result):",
			"    print(\"Error:\", result)")
	case strings.Contains(msg, "undefined") || strings.Contains(msg, "not defined"):
		var name string
		for _, re := range undefinedRE {
			if m := re.FindStringSubmatch(msg); len(m) > 1 {
				name = m[1]
				break
			}
		}
		if name != "" {
			hints = append(hints,
				fmt.Sprintf("Variable '%s' is not defined.", name),
				"Note: Each executeToolCode call runs in a fresh scope — no variables persist between calls.")
			if serverKeys != "" {
				hints = append(hints, serverKeys, "Access tools using: server_name.tool_name(param=\"value\")")
			}
		}
	case strings.Contains(msg, "not within a function"):
		hints = append(hints,
			"Starlark requires for/if/while statements to be inside functions at the top level.",
			"Wrap your code in a function, then call it:",
			"  def fetch_all():",
			"    results = []",
			"    for id in ids:",
			"      results.append(server.get(id=id))",
			"    return results",
			"  result = fetch_all()")
	case kind == "syntax":
		hints = append(hints,
			"Python syntax error detected.",
			"Check for proper indentation (use spaces, not tabs).",
			"Ensure colons after if/for/def statements.",
			"Check for matching parentheses and brackets.")
	case strings.Contains(msg, "has no") && strings.Contains(msg, "attribute"):
		hints = append(hints,
			"You're trying to access an attribute that doesn't exist.",
			"Use dict access syntax: result[\"key\"] instead of result.key",
			"Use print(result) to see the actual structure.")
		if serverKeys != "" {
			hints = append(hints, serverKeys)
		}
	case strings.Contains(msg, "not callable"):
		hints = append(hints,
			"You're trying to call something that is not a function.",
			"Ensure you're using the correct tool name.")
		if serverKeys != "" {
			hints = append(hints, serverKeys)
		}
		hints = append(hints, "Use readToolFile to see available tools for a server.")
	case strings.Contains(msg, "key") && (strings.Contains(msg, "not found") || strings.Contains(msg, "not in dict")):
		hints = append(hints,
			"Dictionary key not found.",
			"Use print() to inspect the dict structure before accessing keys.",
			"Use .get(\"key\", default) for safe access.")
	}
	if len(hints) == 0 {
		hints = append(hints, "Check the error message above for details.")
		if serverKeys != "" {
			hints = append(hints, serverKeys)
		}
		hints = append(hints,
			"Use: result = server_name.tool_name(param=\"value\")",
			"Access dict values with brackets: result[\"key\"]")
	}
	return hints
}
