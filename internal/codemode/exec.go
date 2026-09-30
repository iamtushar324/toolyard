package codemode

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

// heldMarkers are the result metadata keys the gateway sets when a call
// did not run: queued for approval, approved but still executing, or
// coached towards inbox.request. Starlark has no try/except, so such a
// result aborts the script like any tool error, carrying the gateway's
// text (approval id, next steps) into the error output.
var heldMarkers = []string{"toolyard.deferred", "toolyard.executing", "toolyard.permission_required"}

// scriptName is the file name Starlark reports positions against.
const scriptName = "code.star"

// ExecuteToolCode runs a script for the ctx caller. reason is the outer
// call's _reason, or "" when the client sent none (Bifrost clients never
// do); nested calls carry it, or one derived from the script. failed is
// true for a syntax error, a runtime error or a tool call that did not
// succeed.
func (r *Runtime) ExecuteToolCode(ctx context.Context, code, reason string) (text string, failed bool) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "code parameter is required and must be a non-empty string", true
	}
	if strings.TrimSpace(reason) == "" {
		reason = deriveReason(code)
	}
	servers := buildServers(r.caller.Tools(ctx))
	res := r.execute(ctx, code, reason, servers)
	return r.render(res)
}

// execOutcome is what one script run produced.
type execOutcome struct {
	result  any
	hasRes  bool
	logs    []string
	err     *execError
	servers []string
}

// execError is a failed run: kind is "syntax" or "runtime".
type execError struct {
	kind    string
	message string
	hints   []string
}

// run is the mutable state of one script: its print log and call budget.
type run struct {
	r      *Runtime
	ctx    context.Context
	reason string
	// stopNote names the deadline that ends the script when it runs out.
	stopNote string

	mu        sync.Mutex
	logs      []string
	logBytes  int
	truncated bool
	calls     int
}

// execute binds the servers and runs the script under the limits.
func (r *Runtime) execute(ctx context.Context, code, reason string, servers []*server) execOutcome {
	stopNote := fmt.Sprintf("limit %s", r.limits.ScriptTimeout)
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < r.limits.ScriptTimeout {
		stopNote = "the caller's deadline"
	}
	runCtx, cancel := context.WithTimeout(ctx, r.limits.ScriptTimeout)
	defer cancel()
	rn := &run{r: r, ctx: runCtx, reason: reason, stopNote: stopNote}

	keys := make([]string, 0, len(servers))
	predeclared := starlark.StringDict{}
	for _, s := range servers {
		keys = append(keys, s.name)
		predeclared[s.name] = rn.serverStruct(s)
	}
	sort.Strings(keys)

	thread := &starlark.Thread{
		Name:  "codemode",
		Print: func(_ *starlark.Thread, msg string) { rn.log(msg) },
	}
	thread.SetMaxExecutionSteps(r.limits.MaxSteps)
	// The wall clock stops the interpreter too, not only the next nested
	// call: a script spinning without calling anything still ends.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-runCtx.Done():
			thread.Cancel(fmt.Sprintf("script stopped: %v (%s)", runCtx.Err(), stopNote))
		case <-done:
		}
	}()

	opts := &syntax.FileOptions{
		TopLevelControl: true,
		While:           true,
		Set:             true,
		GlobalReassign:  true,
		Recursion:       true,
	}
	globals, err := starlark.ExecFileOptions(opts, thread, scriptName, code, predeclared)
	out := execOutcome{logs: rn.snapshotLogs(), servers: keys}
	if err != nil {
		msg := err.Error()
		var evalErr *starlark.EvalError
		if errors.As(err, &evalErr) {
			msg = evalErr.Backtrace()
		}
		// starlark-go's parse errors read `got ':', want ')'` with no
		// "syntax error" in them, so the kind comes from the error type.
		kind := "runtime"
		var synErr syntax.Error
		if errors.As(err, &synErr) || strings.Contains(msg, "syntax error") {
			kind = "syntax"
		}
		out.err = &execError{kind: kind, message: msg, hints: errorHints(kind, msg, usesExceptions(code), keys)}
		return out
	}
	if v, ok := globals["result"]; ok && v != starlark.None {
		out.result = fromStarlark(v)
		out.hasRes = true
	}
	return out
}

// serverStruct is the Starlark object a server key names: one method per
// bound identifier (and alias), plus a refusing method per ambiguous one.
func (rn *run) serverStruct(s *server) *starlarkstruct.Struct {
	members := starlark.StringDict{}
	for ident, b := range s.byIdent {
		members[ident] = rn.toolFunc(s.name, ident, b.tool)
	}
	for ident, names := range s.ambiguous {
		if _, taken := members[ident]; taken {
			continue
		}
		err := ambiguityError(s.name, ident, names)
		members[ident] = starlark.NewBuiltin(ident, func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
			return nil, err
		})
	}
	return starlarkstruct.FromStringDict(starlark.String(s.name), members)
}

// toolFunc binds one tool as a Starlark function taking keyword arguments
// (or a single dict).
func (rn *run) toolFunc(serverName, ident string, t Tool) *starlark.Builtin {
	return starlark.NewBuiltin(ident, func(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		callArgs, err := callArguments(fn.Name(), args, kwargs)
		if err != nil {
			return nil, err
		}
		return rn.call(serverName, t, callArgs)
	})
}

// callArguments turns a call's kwargs (or one positional dict) into the
// tool's argument map.
func callArguments(name string, args starlark.Tuple, kwargs []starlark.Tuple) (map[string]any, error) {
	out := map[string]any{}
	for _, kv := range kwargs {
		if len(kv) != 2 {
			continue
		}
		k, _ := kv[0].(starlark.String)
		out[string(k)] = fromStarlark(kv[1])
	}
	switch {
	case len(args) == 0:
	case len(args) == 1 && len(kwargs) == 0:
		d, ok := args[0].(*starlark.Dict)
		if !ok {
			return nil, fmt.Errorf("%s() takes keyword arguments: %s(param=value), or a single dict of them", name, name)
		}
		for _, item := range d.Items() {
			k, ok := item[0].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("%s(): argument names must be strings, got %s", name, item[0].Type())
			}
			out[string(k)] = fromStarlark(item[1])
		}
	default:
		return nil, fmt.Errorf("%s() takes keyword arguments: %s(param=value)", name, name)
	}
	return out, nil
}

// call routes one nested tool call through the gateway and converts the
// answer. Every failure aborts the script with a message that names the
// tool and carries the gateway's text.
func (rn *run) call(serverName string, t Tool, args map[string]any) (starlark.Value, error) {
	label := serverName + "." + strings.ReplaceAll(t.Name, "-", "_")
	rn.mu.Lock()
	rn.calls++
	n := rn.calls
	rn.mu.Unlock()
	if n > rn.r.limits.MaxCalls {
		msg := fmt.Sprintf("this script already made %d tool calls, the limit for one executeToolCode run; split the work across runs", rn.r.limits.MaxCalls)
		rn.log(fmt.Sprintf("[TOOL] %s error: %s", label, msg))
		return nil, fmt.Errorf("tool call failed for %s: %s", label, msg)
	}
	if err := rn.ctx.Err(); err != nil {
		return nil, fmt.Errorf("tool call failed for %s: script stopped: %v (%s)", label, err, rn.stopNote)
	}
	if field := t.ReasonField; field != "" {
		if given, _ := args[field].(string); strings.TrimSpace(given) == "" {
			args[field] = rn.reason
		}
	}

	res, err := rn.r.caller.RouteCall(rn.ctx, Via, t.Target, args)
	if err != nil {
		rn.log(fmt.Sprintf("[TOOL] %s error: %v", label, err))
		return nil, fmt.Errorf("tool call failed for %s: %v", label, err)
	}
	text := resultText(res, t.Name)
	if res != nil && (res.IsError || held(res)) {
		rn.log(fmt.Sprintf("[TOOL] %s error result: %s", label, text))
		return nil, fmt.Errorf("tool call failed for %s: %s", label, text)
	}
	quoted, _ := jsonText(text, "")
	rn.log(fmt.Sprintf("[TOOL] %s raw response: %s", label, quoted))
	return decodeResult(text), nil
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

// log appends one print line, up to the output limit.
func (rn *run) log(msg string) {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	if rn.truncated {
		return
	}
	if rn.logBytes+len(msg) > rn.r.limits.MaxOutputBytes {
		rn.truncated = true
		rn.logs = append(rn.logs, fmt.Sprintf("[print output truncated: %d byte limit reached]", rn.r.limits.MaxOutputBytes))
		return
	}
	rn.logBytes += len(msg) + 1
	rn.logs = append(rn.logs, msg)
}

func (rn *run) snapshotLogs() []string {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	return append([]string(nil), rn.logs...)
}

// render is Bifrost's response text for each outcome.
func (r *Runtime) render(o execOutcome) (string, bool) {
	keys := strings.Join(o.servers, ", ")
	var text string
	failed := false
	switch {
	case o.err != nil:
		failed = true
		logs := ""
		if len(o.logs) > 0 {
			logs = fmt.Sprintf("\n\nPrint Output:\n%s\n", strings.Join(o.logs, "\n"))
		}
		text = fmt.Sprintf("Execution %s error:\n\n%s\n\nHints:\n%s%s\n\nEnvironment:\n  Available server keys: %s",
			o.err.kind, o.err.message, strings.Join(o.err.hints, "\n"), logs, keys)
	case len(o.logs) == 0 && !o.hasRes:
		hints := []string{
			"Add print() statements throughout your code to debug and see what's happening at each step",
			"Assign the final value to 'result' variable if you want to return it: result = computed_value",
			"Check that your tool calls are actually executing and returning data",
		}
		text = fmt.Sprintf("Execution completed but produced no data:\n\n"+
			"The code executed without errors but returned no output (no print output and no result variable).\n\n"+
			"Hints:\n%s\n\nEnvironment:\n  Available server keys: %s", strings.Join(hints, "\n"), keys)
	default:
		if len(o.logs) > 0 {
			text = fmt.Sprintf("Print output:\n%s\n\nExecution completed successfully.", strings.Join(o.logs, "\n"))
		} else {
			text = "Execution completed successfully."
		}
		if o.hasRes {
			if js, err := jsonText(o.result, "  "); err == nil {
				text += "\nReturn value: " + js
			} else {
				text += fmt.Sprintf("\nReturn value: %v", o.result)
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
