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

// The script side. runScript (Starlark) and runJS (JavaScript) are what
// the worker process runs; the in-process executor used by tests runs the
// same functions with a direct sink. Nothing here knows tool targets,
// reasons or the gateway: a call is a (server identifier, member
// identifier, arguments) request to the sink, or a request by name,
// answered with text or an error.

// scriptName is the file name Starlark reports positions against.
const scriptName = "code.star"

// callRequest is one tool call a script asks for.
type callRequest struct {
	// id is the sink's number for the call; the parent answers with it,
	// and an uncaught JavaScript failure names it.
	id int
	// server and member name the call by identifier (BkCoreServices,
	// get_client); name names it as "Server.tool" instead, with both
	// empty.
	server, member, name string
	args                 map[string]any
	// try asks for the outcome as data: a failed or held call does not
	// end the script.
	try bool
}

// label names the call in messages.
func (c callRequest) label() string {
	if c.name != "" {
		return c.name
	}
	return c.server + "." + c.member
}

// sink is the parent as the script sees it.
type sink interface {
	// start asks the parent to make one tool call. It returns the call's
	// id and the channel its one answer arrives on; a non-empty Error in
	// the answer is the call's failure.
	start(req callRequest) (int, <-chan parentMsg)
	// print records one print() line.
	print(text string)
}

// await waits for a call's answer. Every call is answered: in process the
// call returns once its context ends, and in a worker the parent answers
// or kills the worker at the deadline. Waiting for the answer, rather
// than for the clock, keeps the call's record true (a call that finished
// is not outcome_unknown) and lets the interpreter report the deadline
// itself.
func await(ch <-chan parentMsg) parentMsg { return <-ch }

// scriptResult is what one run produced: a JSON result, or an error of
// kind "syntax" or "runtime". cause is the structured run's error kind
// when the side that failed the run knows it (ErrorTimeout, ErrorLimit,
// ErrorInternal, ErrorBusy); "" leaves it to the caller (a failed tool
// call, or a script error). call is the sink id of the call whose
// uncaught failure ended a JavaScript script, 0 otherwise.
type scriptResult struct {
	result    json.RawMessage
	hasResult bool
	errKind   string
	errMsg    string
	cause     string
	call      int
}

// runScript binds the servers and executes Starlark code under the
// limits. stopNote names the deadline reported when ctx ends the run.
func runScript(ctx context.Context, start startMsg, s sink, stopNote string) scriptResult {
	predeclared := starlark.StringDict{}
	// targets maps each tool builtin to the call it makes, for try_call
	// and parallel.
	targets := map[*starlark.Builtin]callRequest{}
	for _, srv := range start.Servers {
		members := starlark.StringDict{}
		for _, m := range srv.Members {
			b := toolBuiltin(ctx, srv.Ident, m, s)
			targets[b] = callRequest{server: srv.Ident, member: m}
			members[m] = b
		}
		for ident, msg := range srv.Refused {
			if _, taken := members[ident]; taken {
				continue
			}
			members[ident] = refusingBuiltin(ident, msg)
		}
		predeclared[srv.Ident] = starlarkstruct.FromStringDict(starlark.String(srv.Ident), members)
	}
	maxParallel := start.MaxParallel
	if maxParallel <= 0 || maxParallel > maxParallelCalls {
		maxParallel = maxParallelCalls
	}
	// A server of the same name keeps its binding: the helpers are new,
	// the server keys are what existing scripts use.
	for name, b := range map[string]*starlark.Builtin{
		"try_call": tryCallBuiltin(ctx, s, targets),
		"parallel": parallelBuiltin(ctx, s, targets, maxParallel),
	} {
		if _, taken := predeclared[name]; !taken {
			predeclared[name] = b
		}
	}

	// stepsHit records that the step limit, not the script, ended the run.
	// OnMaxSteps runs on the interpreter's goroutine, which is this one.
	stepsHit := false
	thread := &starlark.Thread{
		Name:  "codemode",
		Print: func(_ *starlark.Thread, msg string) { s.print(msg) },
		OnMaxSteps: func(th *starlark.Thread) {
			stepsHit = true
			th.Cancel("too many steps")
		},
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
		// fail("m" * (9 << 20)) must not become a message no pipe carries.
		res := scriptResult{errKind: kind, errMsg: cut(msg, maxErrorBytes, "… [error message truncated at 64 KiB]")}
		switch {
		case ctx.Err() != nil:
			res.cause = ErrorTimeout
		case stepsHit:
			res.cause = ErrorLimit
		}
		return res
	}
	v, ok := globals["result"]
	if !ok || v == starlark.None {
		return scriptResult{}
	}
	goValue, err := toGo(v)
	if err != nil {
		return scriptResult{errKind: "runtime", errMsg: "result cannot be returned: " + err.Error()}
	}
	// A huge string is the common case; refuse it before encoding a copy.
	if str, ok := goValue.(string); ok && len(str) > maxResultBytes {
		return resultTooBig(len(str))
	}
	raw, err := jsonText(goValue, "")
	if err != nil {
		return scriptResult{errKind: "runtime", errMsg: "result cannot be encoded as JSON: " + err.Error()}
	}
	if len(raw) > maxResultBytes {
		return resultTooBig(len(raw))
	}
	return scriptResult{result: json.RawMessage(raw), hasResult: true}
}

// resultTooBig refuses a returned value past maxResultBytes.
func resultTooBig(n int) scriptResult {
	return scriptResult{errKind: "runtime", cause: ErrorLimit, errMsg: fmt.Sprintf(
		"result is %d bytes; a returned value may be at most %d MiB (the response is truncated there anyway). Return less, or print a summary and return the key fields",
		n, maxResultBytes>>20)}
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
func toolBuiltin(ctx context.Context, server, member string, s sink) *starlark.Builtin {
	return starlark.NewBuiltin(member, func(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		callArgs, err := callArguments(fn.Name(), args, kwargs)
		if err != nil {
			return nil, err
		}
		if err := checkArgsSize(server+"."+member, callArgs); err != nil {
			return nil, err
		}
		req := callRequest{server: server, member: member, args: callArgs}
		_, ch := s.start(req)
		reply := await(ch)
		if reply.Error != "" {
			return nil, errors.New(reply.Error)
		}
		return replyValue(reply), nil
	})
}

// checkArgsSize refuses arguments the pipe should not carry.
func checkArgsSize(label string, callArgs map[string]any) error {
	encoded, err := json.Marshal(callArgs)
	if err != nil {
		return fmt.Errorf("arguments to %s cannot be encoded as JSON: %v", label, err)
	}
	if len(encoded) > maxArgsBytes {
		return fmt.Errorf("arguments to %s are %d bytes; one call may pass at most %d MiB. Pass less, or split the work across calls",
			label, len(encoded), maxArgsBytes>>20)
	}
	return nil
}

// replyValue is a successful answer as a Starlark value.
func replyValue(reply parentMsg) starlark.Value {
	if len(reply.JSON) > 0 {
		return decodeResult(string(reply.JSON))
	}
	return decodeResult(reply.Text)
}

// tryValue is an answer as try_call returns it: {"ok": True, "status":
// "succeeded", "result": ...} or {"ok": False, "status": ..., "error":
// ...} (with "approval_id" for a deferred call). Either has "dispatch_id"
// when the call reached a gateway dispatch.
func tryValue(reply parentMsg) starlark.Value {
	d := starlark.NewDict(5)
	if reply.DispatchID != "" {
		_ = d.SetKey(starlark.String("dispatch_id"), starlark.String(reply.DispatchID))
	}
	if reply.Error == "" {
		_ = d.SetKey(starlark.String("ok"), starlark.True)
		_ = d.SetKey(starlark.String("status"), starlark.String(StatusSucceeded))
		_ = d.SetKey(starlark.String("result"), replyValue(reply))
		return d
	}
	status := reply.Status
	if status == "" || status == StatusSucceeded {
		status = StatusFailed
	}
	_ = d.SetKey(starlark.String("ok"), starlark.False)
	_ = d.SetKey(starlark.String("status"), starlark.String(status))
	_ = d.SetKey(starlark.String("error"), starlark.String(reply.Error))
	if reply.ApprovalID != "" {
		_ = d.SetKey(starlark.String("approval_id"), starlark.String(reply.ApprovalID))
	}
	return d
}

// targetOf reads the tool a helper is asked to call: a tool function
// (BkCoreServices.get_client) or its name ("BkCoreServices.get_client").
func targetOf(fn string, v starlark.Value, targets map[*starlark.Builtin]callRequest) (callRequest, error) {
	switch t := v.(type) {
	case *starlark.Builtin:
		if req, ok := targets[t]; ok {
			return req, nil
		}
	case starlark.String:
		if name := strings.TrimSpace(string(t)); name != "" {
			return callRequest{name: name}, nil
		}
	}
	return callRequest{}, fmt.Errorf("%s: name the tool as Server.tool or as a string \"Server.tool\", not %s", fn, v.Type())
}

// tryCallBuiltin is try_call(tool, param=value) or try_call(tool, {dict}):
// one call whose failure or hold comes back as data instead of ending the
// script.
func tryCallBuiltin(ctx context.Context, s sink, targets map[*starlark.Builtin]callRequest) *starlark.Builtin {
	return starlark.NewBuiltin("try_call", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) == 0 {
			return nil, errors.New(`try_call takes the tool first: try_call(Server.tool, param=value) or try_call("Server.tool", {"param": value})`)
		}
		req, err := targetOf("try_call", args[0], targets)
		if err != nil {
			return nil, err
		}
		if req.args, err = callArguments("try_call", args[1:], kwargs); err != nil {
			return nil, err
		}
		if err := checkArgsSize(req.label(), req.args); err != nil {
			return nil, err
		}
		req.try = true
		_, ch := s.start(req)
		return tryValue(await(ch)), nil
	})
}

// parallelBuiltin is parallel(calls, limit=4): the calls, each a tool, a
// (tool, {args}) pair or a [tool, {args}] list, made at most limit at a
// time (at most maxParallel), answered as a list of try_call results in
// the order given.
func parallelBuiltin(ctx context.Context, s sink, targets map[*starlark.Builtin]callRequest, maxParallel int) *starlark.Builtin {
	return starlark.NewBuiltin("parallel", func(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var calls starlark.Value
		limit := defaultParallel
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "calls", &calls, "limit?", &limit); err != nil {
			return nil, err
		}
		if limit < 1 {
			return nil, errors.New("parallel: limit must be at least 1")
		}
		limit = min(limit, maxParallel)
		list, ok := calls.(starlark.Indexable)
		if _, isString := calls.(starlark.String); !ok || isString {
			return nil, errors.New(`parallel takes a list of calls: parallel([(Server.tool, {"param": value}), ("Server.other", {})], limit=4)`)
		}
		reqs := make([]callRequest, list.Len())
		for i := range reqs {
			item := list.Index(i)
			target, rest := item, starlark.Tuple(nil)
			switch it := item.(type) {
			case starlark.String, *starlark.Builtin:
			case starlark.Indexable:
				if it.Len() == 0 || it.Len() > 2 {
					return nil, fmt.Errorf("parallel: call %d must be a tool, or (tool, {args})", i)
				}
				target = it.Index(0)
				if it.Len() == 2 {
					rest = starlark.Tuple{it.Index(1)}
				}
			}
			req, err := targetOf(fmt.Sprintf("parallel: call %d", i), target, targets)
			if err != nil {
				return nil, err
			}
			if req.args, err = callArguments("parallel", rest, nil); err != nil {
				return nil, fmt.Errorf("parallel: call %d: %w", i, err)
			}
			if err := checkArgsSize(req.label(), req.args); err != nil {
				return nil, err
			}
			req.try = true
			reqs[i] = req
		}
		return starlark.NewList(runParallel(ctx, s, reqs, limit, tryValue)), nil
	})
}

// runParallel makes reqs at most limit at a time, starting each next call
// as one finishes (so the calls start in the order given), and returns
// each answer through value, in the order given. Once ctx ends no further
// call starts.
func runParallel(ctx context.Context, s sink, reqs []callRequest, limit int, value func(parentMsg) starlark.Value) []starlark.Value {
	type done struct {
		i     int
		reply parentMsg
	}
	results := make([]starlark.Value, len(reqs))
	fin := make(chan done, len(reqs))
	next, running := 0, 0
	launch := func() {
		i := next
		next++
		running++
		if err := ctx.Err(); err != nil {
			fin <- done{i, parentMsg{Status: StatusFailed, Error: fmt.Sprintf("tool call failed for %s: script stopped: %v", reqs[i].label(), err)}}
			return
		}
		_, ch := s.start(reqs[i])
		go func() { fin <- done{i, await(ch)} }()
	}
	for next < len(reqs) && running < limit {
		launch()
	}
	for range reqs {
		d := <-fin
		running--
		results[d.i] = value(d.reply)
		if next < len(reqs) {
			launch()
		}
	}
	return results
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
		if args[0] == starlark.None {
			break
		}
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
