package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

// The script side. runScript is what the worker process runs; the
// in-process executor used by tests runs the same function with a direct
// sink. Nothing here knows tool targets, reasons or the gateway: a call is
// a (server identifier, member identifier, arguments) request to the sink,
// answered with text or an error.

// scriptName is the file name Starlark reports positions against.
const scriptName = "code.star"

// sink is the parent as the script sees it.
type sink interface {
	// call asks the parent to make one tool call. A non-empty Error is the
	// message that aborts the script.
	call(server, member string, args map[string]any) parentMsg
	// print records one print() line.
	print(text string)
}

// scriptResult is what one run produced: a JSON result, or an error of
// kind "syntax" or "runtime".
type scriptResult struct {
	result    json.RawMessage
	hasResult bool
	errKind   string
	errMsg    string
}

// runScript binds the servers and executes code under the limits. stopNote
// names the deadline reported when ctx ends the run.
func runScript(ctx context.Context, start startMsg, s sink, stopNote string) scriptResult {
	predeclared := starlark.StringDict{}
	for _, srv := range start.Servers {
		members := starlark.StringDict{}
		for _, m := range srv.Members {
			members[m] = toolBuiltin(srv.Ident, m, s)
		}
		for ident, msg := range srv.Refused {
			if _, taken := members[ident]; taken {
				continue
			}
			members[ident] = refusingBuiltin(ident, msg)
		}
		predeclared[srv.Ident] = starlarkstruct.FromStringDict(starlark.String(srv.Ident), members)
	}

	thread := &starlark.Thread{
		Name:  "codemode",
		Print: func(_ *starlark.Thread, msg string) { s.print(msg) },
	}
	thread.SetMaxExecutionSteps(start.Limits.MaxSteps)
	// The wall clock stops the interpreter too, not only the next nested
	// call: a script spinning without calling anything still ends.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			thread.Cancel(fmt.Sprintf("script stopped: %v (%s)", ctx.Err(), stopNote))
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
	globals, err := starlark.ExecFileOptions(opts, thread, scriptName, start.Code, predeclared)
	if err != nil {
		kind, msg := classifyError(err)
		return scriptResult{errKind: kind, errMsg: msg}
	}
	v, ok := globals["result"]
	if !ok || v == starlark.None {
		return scriptResult{}
	}
	goValue, err := toGo(v)
	if err != nil {
		return scriptResult{errKind: "runtime", errMsg: "result cannot be returned: " + err.Error()}
	}
	tooBig := func(n int) scriptResult {
		return scriptResult{errKind: "runtime", errMsg: fmt.Sprintf(
			"result is %d bytes; a returned value may be at most %d MiB (the response is truncated there anyway). Return less, or print a summary and return the key fields",
			n, maxResultBytes>>20)}
	}
	// A huge string is the common case; refuse it before encoding a copy.
	if str, ok := goValue.(string); ok && len(str) > maxResultBytes {
		return tooBig(len(str))
	}
	raw, err := jsonText(goValue, "")
	if err != nil {
		return scriptResult{errKind: "runtime", errMsg: "result cannot be encoded as JSON: " + err.Error()}
	}
	if len(raw) > maxResultBytes {
		return tooBig(len(raw))
	}
	return scriptResult{result: json.RawMessage(raw), hasResult: true}
}

// classifyError names a failed run's kind and message. starlark-go's parse
// errors read `got ':', want ')'` with no "syntax error" in them, so the
// kind comes from the error type; evaluation errors carry their traceback
// so the agent sees the line.
func classifyError(err error) (kind, msg string) {
	msg = err.Error()
	var evalErr *starlark.EvalError
	if errors.As(err, &evalErr) {
		msg = evalErr.Backtrace()
	}
	kind = "runtime"
	var synErr syntax.Error
	if errors.As(err, &synErr) || strings.Contains(msg, "syntax error") {
		kind = "syntax"
	}
	return kind, msg
}

// toolBuiltin binds one tool as a Starlark function taking keyword
// arguments (or a single dict).
func toolBuiltin(server, member string, s sink) *starlark.Builtin {
	return starlark.NewBuiltin(member, func(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		callArgs, err := callArguments(fn.Name(), args, kwargs)
		if err != nil {
			return nil, err
		}
		if encoded, err := json.Marshal(callArgs); err != nil {
			return nil, fmt.Errorf("arguments to %s.%s cannot be encoded as JSON: %v", server, member, err)
		} else if len(encoded) > maxArgsBytes {
			return nil, fmt.Errorf("arguments to %s.%s are %d bytes; one call may pass at most %d MiB. Pass less, or split the work across calls",
				server, member, len(encoded), maxArgsBytes>>20)
		}
		reply := s.call(server, member, callArgs)
		if reply.Error != "" {
			return nil, errors.New(reply.Error)
		}
		return decodeResult(reply.Text), nil
	})
}

// refusingBuiltin is what an ambiguous identifier calls: an explanation.
func refusingBuiltin(ident, msg string) *starlark.Builtin {
	return starlark.NewBuiltin(ident, func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
		return nil, errors.New(msg)
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
		v, err := toGo(kv[1])
		if err != nil {
			return nil, fmt.Errorf("argument %s: %w", string(k), err)
		}
		out[string(k)] = v
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
			v, err := toGo(item[1])
			if err != nil {
				return nil, fmt.Errorf("argument %s: %w", string(k), err)
			}
			out[string(k)] = v
		}
	default:
		return nil, fmt.Errorf("%s() takes keyword arguments: %s(param=value)", name, name)
	}
	return out, nil
}
