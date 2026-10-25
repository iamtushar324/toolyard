package codemode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
)

// heldMarkers are the result metadata keys the gateway sets when a call
// did not run: queued for approval, approved but still executing, or
// coached towards inbox.request. Starlark has no try/except, so such a
// result aborts the script like any tool error, carrying the gateway's
// text (approval id, next steps) into the error output; try_call and
// JavaScript get it as data or a catchable error instead.
var heldMarkers = []string{"toolyard.deferred", "toolyard.executing", "toolyard.permission_required"}

// nestedCallLoop names a panic recovered in a nested call (the metric
// label and crash dump name).
const nestedCallLoop = "codemode-nested-call"

// ExecOptions choose how one executeToolCode run reads and reports.
type ExecOptions struct {
	// Reason is the outer call's _reason, or "" when the client sent none
	// (Bifrost clients never do); nested calls carry it, or one derived
	// from the script.
	Reason string
	// Language is LanguageStarlark (also "") or LanguageJavaScript.
	Language string
	// Echo is EchoFull (also ""), EchoPreview or EchoNone.
	Echo string
}

// ExecResult is one executeToolCode run.
type ExecResult struct {
	// Text is the response text; Failed marks a syntax error, a runtime
	// error, a tool call that did not succeed and was not caught, or a
	// worker that did not survive the script.
	Text   string
	Failed bool
	// Calls is the call ledger: every tool call the script made, in order.
	Calls []LedgerEntry
}

// LedgerEntry is one tool call in a run's ledger.
type LedgerEntry struct {
	Seq    int    `json:"seq"`
	Tool   string `json:"tool"`
	Status string `json:"status"`
	MS     int64  `json:"ms"`
}

// ExecuteToolCode runs a Starlark script for the ctx caller with
// Bifrost's output. reason is the outer call's _reason, or "". failed is
// true for a syntax error, a runtime error, a tool call that did not
// succeed, or a worker that did not survive the script.
func (r *Runtime) ExecuteToolCode(ctx context.Context, code, reason string) (text string, failed bool) {
	res := r.Execute(ctx, code, ExecOptions{Reason: reason})
	return res.Text, res.Failed
}

// Execute runs a script for the ctx caller and renders executeToolCode's
// text. With Language Starlark and Echo full the text is Bifrost's, byte
// for byte; the other modes add the call ledger to its Environment block.
func (r *Runtime) Execute(ctx context.Context, code string, opts ExecOptions) ExecResult {
	lang, err := ParseLanguage(opts.Language)
	if err != nil {
		return ExecResult{Text: err.Error(), Failed: true}
	}
	echo, err := ParseEcho(opts.Echo)
	if err != nil {
		return ExecResult{Text: err.Error(), Failed: true}
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return ExecResult{Text: "code parameter is required and must be a non-empty string", Failed: true}
	}
	ex := r.execute(ctx, code, execOptions{reason: opts.Reason, lang: lang, echo: echo})
	text, failed := r.render(ex, code)
	return ExecResult{Text: text, Failed: failed, Calls: ex.s.ledgerSnapshot()}
}

// execOptions configure execute.
type execOptions struct {
	reason string
	// hooks wrap every nested call (structured runs only).
	hooks CallHooks
	// structured marks a Run: the caller already holds a concurrency
	// slot, and the session keeps call records and print lines.
	structured bool
	lang, echo string
}

// execution is one finished script: the catalog it ran against, the
// session that made its calls, and its outcome.
type execution struct {
	cat        *catalog
	s          *session
	res        scriptResult
	lang, echo string
}

// execute runs code for the ctx caller; code is trimmed and non-empty.
// A structured run (Run) holds a concurrency slot already; otherwise
// (Execute) a worker run takes a slot itself, or reports code mode busy.
func (r *Runtime) execute(ctx context.Context, code string, o execOptions) execution {
	hooks := o.hooks
	if !o.structured {
		hooks = nil
	}
	reason := o.reason
	if strings.TrimSpace(reason) == "" {
		reason = deriveReason(code)
	}
	lang, echo := o.lang, o.echo
	if lang == "" {
		lang = LanguageStarlark
	}
	if echo == "" {
		echo = EchoFull
	}
	cat := buildCatalog(r.caller.Tools(ctx))

	stopNote := fmt.Sprintf("limit %s", r.limits.ScriptTimeout)
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < r.limits.ScriptTimeout {
		stopNote = "the caller's deadline"
	}
	runCtx, cancel := context.WithTimeout(ctx, r.limits.ScriptTimeout)
	defer cancel()

	s := &session{r: r, ctx: runCtx, reason: reason, cat: cat, hooks: hooks, record: o.structured,
		lang: lang, echo: echo, deferred: map[int]*callStop{}}
	start := startMsg{Type: msgStart, Code: code, Servers: cat.wire(), Limits: r.limits,
		Language: lang, MaxParallel: maxParallelCalls, MemoryMiB: r.worker.MemoryMiB}
	var res scriptResult
	switch {
	case r.inProcess && lang == LanguageJavaScript:
		res = runJS(runCtx, start, s, stopNote)
	case r.inProcess:
		res = runScript(runCtx, start, s, stopNote)
	case o.structured:
		res = r.runWorker(runCtx, s, start, stopNote)
	default:
		res = r.runInWorker(runCtx, s, start, stopNote)
	}
	if res.errKind != "" && res.call != 0 {
		s.promote(res.call)
	}
	return execution{cat: cat, s: s, res: res, lang: lang, echo: echo}
}

// session is the parent's state for one script: the print log, the call
// budget, how nested calls are made, and, for a structured run, the call
// records and the call that stopped the run.
type session struct {
	r      *Runtime
	ctx    context.Context
	reason string
	cat    *catalog
	// hooks wrap each nested call (Run only; nil otherwise).
	hooks CallHooks
	// record keeps call records and print lines for a RunResult.
	record bool
	// lang is the script's language; echo how tool answers are echoed.
	lang, echo string

	mu        sync.Mutex
	logs      []string
	logBytes  int
	truncated bool
	calls     int
	// echoBytes counts the "[TOOL]" lines, which may use only their share
	// of the output; echoCut is set once they reached it.
	echoBytes int
	echoCut   bool
	// reqIDs numbers in-process calls (a worker numbers its own).
	reqIDs int

	// prints are the print() lines alone, for RunResult.Logs.
	prints     []string
	printBytes int
	printsCut  bool
	// records are the calls that got a sequence number, in order.
	records []*callState
	// stop is the call outcome that ended the script; once set, no other
	// call is made, whatever the worker asks.
	stop *callStop
	// deferred holds, by call id, the outcome of a failed call the script
	// may still catch (try_call, parallel, any JavaScript call). It
	// becomes the run's stop only when the script reports that call's
	// failure as what ended it (promote).
	deferred map[int]*callStop
	// ledger is every call that got a sequence number, with its status
	// and duration.
	ledger []LedgerEntry
	// sealed is set when Run has taken its snapshot: a call still running
	// then gets no AfterCall, and nothing more is sent.
	sealed bool
	// hookWG counts hook invocations in progress, so Run returns only
	// once none is running.
	hookWG sync.WaitGroup
}

// callState is one recorded call while the run goes on.
type callState struct {
	rec  CallRecord
	sent bool // RouteCall was entered
	done bool // the call has its final status
}

// callStop is the outcome of the call that ended the script.
type callStop struct {
	status string // the run status it implies
	kind   string // the run's error kind ("" for a held call)
	msg    string
	held   *HeldCall
}

// preparedCall is a call admitted with its sequence number.
type preparedCall struct {
	req   callRequest
	tool  Tool
	label string
	seq   int
}

// start makes a call for an in-process script: admitted at once (so calls
// are numbered in the order the script asks for them), made on a
// goroutine of its own.
func (s *session) start(req callRequest) (int, <-chan parentMsg) {
	s.mu.Lock()
	s.reqIDs++
	req.id = s.reqIDs
	s.mu.Unlock()
	ch := make(chan parentMsg, 1)
	pc, refusal := s.admit(req)
	if refusal != nil {
		ch <- *refusal
		return req.id, ch
	}
	go func() { ch <- s.perform(pc) }()
	return req.id, ch
}

// stopsNow reports whether a failed call ends the script at once: a plain
// Starlark call does; a try_call or parallel call, and any JavaScript call
// (a script can catch it), leave that to the script.
func (s *session) stopsNow(req callRequest) bool {
	return !req.try && s.lang != LanguageJavaScript
}

// stopFor records stop as the outcome of req's failure: the run's stop
// at once, or deferred until the script reports it uncaught.
func (s *session) stopFor(req callRequest, stop *callStop) {
	if s.stopsNow(req) {
		s.stopRun(stop)
		return
	}
	if req.id == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.deferred[req.id]; !ok {
		s.deferred[req.id] = stop
	}
}

// promote makes call id's deferred outcome the run's stop: the script
// ended on that call's failure, uncaught.
func (s *session) promote(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		return
	}
	if stop, ok := s.deferred[id]; ok {
		s.stop = stop
	}
}

// admit checks one call a script asks for and gives it its sequence
// number, in the order calls arrive: an unknown tool, a script that has
// stopped, or the call limit refuses it with the answer to send back.
func (s *session) admit(req callRequest) (*preparedCall, *parentMsg) {
	// fail refuses a call that never got a number.
	fail := func(kind, msg string) (*preparedCall, *parentMsg) {
		s.stopFor(req, &callStop{status: StatusFailed, kind: kind, msg: msg})
		return nil, &parentMsg{Status: StatusFailed, Error: msg}
	}
	var srv *server
	var b *binding
	if req.name != "" {
		var msg string
		if srv, b, msg = s.cat.resolveName(req.name); msg != "" {
			return fail(ErrorScriptError, msg)
		}
	} else {
		if srv = s.cat.byIdent(req.server); srv == nil {
			return fail(ErrorScriptError, fmt.Sprintf("tool call failed: unknown server %q", req.server))
		}
		var ok bool
		if b, ok = srv.byIdent[req.member]; !ok {
			if names, amb := srv.ambiguous[canonicalName(req.member)]; amb {
				return fail(ErrorScriptError, ambiguityError(srv.ident, req.member, names).Error())
			}
			return fail(ErrorScriptError, fmt.Sprintf("tool call failed: %s has no tool %q", srv.ident, req.member))
		}
	}
	t := b.tool
	label := srv.ident + "." + strings.ReplaceAll(t.Name, "-", "_")

	s.mu.Lock()
	if s.stop != nil || s.sealed {
		// An honest Starlark script never gets here: a failed call aborts
		// it. A worker that keeps calling anyway is refused.
		s.mu.Unlock()
		return nil, &parentMsg{Status: StatusFailed, Error: fmt.Sprintf("tool call failed for %s: the script already stopped", label)}
	}
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if n > s.r.limits.MaxCalls {
		msg := fmt.Sprintf("this script already made %d tool calls, the limit for one executeToolCode run; split the work across runs", s.r.limits.MaxCalls)
		s.echoLine(fmt.Sprintf("[TOOL] %s error: %s", label, msg), true)
		full := fmt.Sprintf("tool call failed for %s: %s", label, msg)
		s.stopRun(&callStop{status: StatusFailed, kind: ErrorLimit, msg: full})
		return nil, &parentMsg{Status: StatusFailed, Error: full}
	}
	return &preparedCall{req: req, tool: t, label: label, seq: n}, nil
}

// perform makes one admitted call. Every failure is returned as the
// message the script sees, naming the tool and carrying the gateway's
// text. A panic in the call is one of those failures: the call has its
// own goroutine, where an unrecovered panic would end the whole gateway.
func (s *session) perform(pc *preparedCall) (reply parentMsg) {
	req, t, label, n := pc.req, pc.tool, pc.label, pc.seq
	var st *callState
	sent := false
	started := time.Now()
	ledgerStatus := StatusFailed
	defer func() { s.addLedger(n, label, ledgerStatus, started) }()
	defer func() {
		if r := recover(); r != nil {
			goroutines.Report(nestedCallLoop, r)
			if sent {
				ledgerStatus = StatusOutcomeUnknown
				reply = parentMsg{Status: StatusOutcomeUnknown, Error: fmt.Sprintf("tool call failed for %s: the gateway hit an internal error during the call; its outcome is unknown, so check before you repeat a write", req.label())}
				s.finish(st, StatusOutcomeUnknown, &callStop{status: StatusOutcomeUnknown, kind: ErrorOutcomeUnknown, msg: reply.Error})
				return
			}
			reply = parentMsg{Status: StatusFailed, Error: fmt.Sprintf("tool call failed for %s: an internal error stopped the call before it was made", req.label())}
			s.finish(st, StatusFailed, &callStop{status: StatusFailed, kind: ErrorInternal, msg: reply.Error})
		}
	}()
	args := req.args
	if args == nil {
		args = map[string]any{}
	}
	if err := s.ctx.Err(); err != nil {
		msg := fmt.Sprintf("tool call failed for %s: script stopped: %v", label, err)
		s.stopRun(&callStop{status: StatusFailed, kind: ErrorTimeout, msg: msg})
		return parentMsg{Status: StatusFailed, Error: msg}
	}
	var scriptArgs map[string]any
	if s.record {
		scriptArgs = scriptArgsOf(args)
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
			s.echoLine(fmt.Sprintf("[TOOL] %s error: %s must be a string", label, field), true)
			msg := fmt.Sprintf("tool call failed for %s: %s must be a string", label, field)
			s.stopFor(req, &callStop{status: StatusFailed, kind: ErrorScriptError, msg: msg})
			return parentMsg{Status: StatusFailed, Error: msg}
		}
	}

	pcall := PendingCall{Seq: n, Tool: t.Server + "." + t.Name, Target: t.Target, Args: args, ScriptArgs: scriptArgs, ReasonField: t.ReasonField}
	st, err := s.begin(pcall)
	if err != nil {
		return parentMsg{Status: StatusFailed, Error: fmt.Sprintf("tool call failed for %s: %v", label, err)}
	}

	var res *mcp.CallToolResult
	handled := false
	if s.hooks != nil {
		var herr error
		res, handled, herr = s.beforeCall(&pcall)
		if herr != nil {
			status, kind := StatusFailed, ErrorInternal
			var ce *CallError
			if errors.As(herr, &ce) && ce.Kind != "" {
				kind = ce.Kind
				if ce.Kind == ErrorOutcomeUnknown {
					status = StatusOutcomeUnknown
				}
			}
			msg := fmt.Sprintf("tool call failed for %s: %v", label, herr)
			s.echoLine(fmt.Sprintf("[TOOL] %s error: %v", label, herr), true)
			s.finish(st, status, &callStop{status: status, kind: kind, msg: msg})
			ledgerStatus = status
			return parentMsg{Status: status, Error: msg}
		}
		if handled && st != nil {
			s.mu.Lock()
			st.rec.Replayed = true
			s.mu.Unlock()
		}
	}

	if !handled {
		if err := s.ctx.Err(); err != nil || s.isSealed() {
			if err == nil {
				err = context.Canceled
			}
			msg := fmt.Sprintf("tool call failed for %s: script stopped: %v", label, err)
			s.finish(st, StatusFailed, &callStop{status: StatusFailed, kind: ErrorTimeout, msg: msg})
			return parentMsg{Status: StatusFailed, Error: msg}
		}
		sent = true
		s.markSent(st)
		res, err = s.r.caller.RouteCall(s.ctx, Via, t.Target, pcall.Args)
	}
	// cls is how the answer reads, for the script's status and the
	// ledger. Only a structured run lets it steer the run (out): the
	// Starlark text of executeToolCode stays Bifrost's.
	cls := classify(res, err, pcall.Args, !handled && s.ctx.Err() != nil)
	var out callOutcome
	if s.record {
		out = cls
	}
	failStatus := cls.status
	if failStatus == "" || failStatus == StatusSucceeded {
		failStatus = StatusFailed
	}

	if err != nil {
		msg := fmt.Sprintf("tool call failed for %s: %v", label, err)
		s.echoLine(fmt.Sprintf("[TOOL] %s error: %v", label, err), true)
		s.complete(st, req, pcall, out, res, err, msg, handled)
		ledgerStatus = failStatus
		return parentMsg{Status: failStatus, Error: msg}
	}
	text := resultText(res, t.Name)
	if res != nil && (res.IsError || held(res)) {
		// The failure text goes to the script; bound it before it is logged
		// or sent, so a huge error result is still a clean failure.
		text = cut(text, maxErrorBytes, "… [error text truncated at 64 KiB]")
		s.echoLine(fmt.Sprintf("[TOOL] %s error result: %s", label, text), true)
		msg := fmt.Sprintf("tool call failed for %s: %s", label, text)
		if s.record && out.status == StatusSucceeded {
			// A failed result the classifier did not place.
			out = callOutcome{status: StatusFailed, runStatus: StatusFailed, kind: ErrorToolError}
		}
		reply := parentMsg{Status: failStatus, Error: msg}
		if failStatus == StatusPermissionRequired || failStatus == StatusDeferred {
			h := heldCall(pcall, failStatus, res, text)
			reply.ApprovalID = h.ApprovalID
			if out.status == StatusPermissionRequired || out.status == StatusDeferred {
				out.held = h
			}
		}
		s.complete(st, req, pcall, out, res, nil, msg, handled)
		ledgerStatus = failStatus
		return reply
	}
	if out.status == StatusOutcomeUnknown {
		// A result that reads as successful but reports an unknown outcome
		// (a legacy _approval_id resume of such an approval): the script
		// must not carry on as if the write had run.
		text = cut(text, maxErrorBytes, "… [error text truncated at 64 KiB]")
		s.echoLine(fmt.Sprintf("[TOOL] %s error result: %s", label, text), true)
		msg := fmt.Sprintf("tool call failed for %s: %s", label, text)
		s.complete(st, req, pcall, out, res, nil, msg, handled)
		ledgerStatus = StatusOutcomeUnknown
		return parentMsg{Status: StatusOutcomeUnknown, Error: msg}
	}
	quoted, err := jsonText(text, "")
	if err != nil || len(quoted) > maxCallResultBytes {
		// The cap is on the JSON text the wire carries, which escaping can
		// make larger than the result itself; say both sizes.
		msg := fmt.Sprintf("the result is %d bytes (%d as JSON text), more than code mode passes to a script (limit %d MiB); ask the tool for less data, or page through it",
			len(text), len(quoted), maxCallResultBytes>>20)
		s.echoLine(fmt.Sprintf("[TOOL] %s error: %s", label, msg), true)
		// The call itself succeeded; the run stops on the size limit.
		out.runStatus, out.kind = StatusFailed, ErrorLimit
		full := fmt.Sprintf("tool call failed for %s: %s", label, msg)
		s.complete(st, req, pcall, out, res, nil, full, handled)
		ledgerStatus = StatusSucceeded
		return parentMsg{Status: StatusFailed, Error: full}
	}
	s.echoResponse(label, quoted, text)
	out.text = text
	reply = parentMsg{Status: StatusSucceeded, Text: text}
	if s.lang == LanguageJavaScript {
		// JavaScript gets the structured content, when the tool sent one
		// that fits (the record keeps that value, so a resume replays it);
		// else text that is JSON goes as that JSON value. Either way the
		// value is compact JSON made here, where memory is plentiful, so
		// the worker passes it on to the engine without decoding a string
		// or copying it again.
		if res != nil && res.StructuredContent != nil {
			if raw, err := jsonText(res.StructuredContent, ""); err == nil && len(raw) <= maxCallResultBytes {
				reply = parentMsg{Status: StatusSucceeded, JSON: json.RawMessage(raw)}
				out.text = raw
			}
		}
		if reply.JSON == nil {
			var buf bytes.Buffer
			buf.Grow(len(text))
			if json.Compact(&buf, []byte(text)) == nil {
				reply = parentMsg{Status: StatusSucceeded, JSON: json.RawMessage(buf.Bytes())}
			}
		}
	}
	s.complete(st, req, pcall, out, res, nil, "", handled)
	ledgerStatus = StatusSucceeded
	if cls.status != "" {
		ledgerStatus = cls.status
	}
	return reply
}

// begin gives a structured run's call its record. It refuses the call
// when Run has already sealed the session.
func (s *session) begin(pc PendingCall) (*callState, error) {
	if !s.record {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return nil, errors.New("script stopped")
	}
	st := &callState{rec: CallRecord{Seq: pc.Seq, Tool: pc.Tool, Target: pc.Target, StartedAt: nowMS()}}
	s.records = append(s.records, st)
	if s.hooks != nil {
		s.hookWG.Add(1) // for BeforeCall
	}
	return st, nil
}

// beforeCall runs the BeforeCall hook; begin counted it in hookWG.
func (s *session) beforeCall(pc *PendingCall) (*mcp.CallToolResult, bool, error) {
	defer s.hookWG.Done()
	return s.hooks.BeforeCall(s.ctx, pc)
}

func (s *session) markSent(st *callState) {
	if st == nil {
		return
	}
	s.mu.Lock()
	st.sent = true
	s.mu.Unlock()
}

func (s *session) isSealed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealed
}

// stopRun records the outcome that ends the script, unless one already
// did.
func (s *session) stopRun(stop *callStop) {
	if stop == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop == nil {
		s.stop = stop
	}
}

// finish gives st its final status (unless Run sealed the session
// first) and records stop, when set, as the outcome that ends the script.
func (s *session) finish(st *callState, status string, stop *callStop) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st != nil && !st.done && !s.sealed {
		st.rec.Status = status
		st.rec.FinishedAt = nowMS()
		st.done = true
	}
	if stop != nil && s.stop == nil {
		s.stop = stop
	}
}

// complete closes a call that got an answer (from the gateway, or from
// BeforeCall's record): its record, the stop it implies, and AfterCall
// for a call the gateway answered. failMsg is the failure text the script
// sees, "" when the call succeeded.
func (s *session) complete(st *callState, req callRequest, pc PendingCall, out callOutcome, res *mcp.CallToolResult, err error, failMsg string, handled bool) {
	var stop *callStop
	if failMsg != "" {
		stop = &callStop{status: out.runStatus, kind: out.kind, msg: failMsg, held: out.held}
		if stop.status == "" {
			stop.status = StatusFailed
		}
	}
	callAfter := false
	s.mu.Lock()
	if st != nil && !st.done && !s.sealed {
		st.rec.Status = out.status
		st.rec.FinishedAt = nowMS()
		st.done = true
		if s.hooks != nil && !handled {
			s.hookWG.Add(1)
			callAfter = true
		}
	}
	var rec CallRecord
	if st != nil {
		rec = st.rec
	}
	s.mu.Unlock()
	if stop != nil {
		s.stopFor(req, stop)
	}
	if callAfter {
		s.afterCall(pc, CallOutcome{Record: rec, Result: res, Err: err, Text: out.text, Message: failMsg})
	}
}

// afterCall runs the AfterCall hook on a context the script's clock does
// not cancel, so the hook can still record a call the clock cut short;
// complete counted it in hookWG.
func (s *session) afterCall(pc PendingCall, out CallOutcome) {
	defer s.hookWG.Done()
	s.hooks.AfterCall(context.WithoutCancel(s.ctx), pc, out)
}

// print makes a session the in-process script's print sink.
func (s *session) print(text string) {
	s.log(text)
	if s.record {
		s.keepPrint(text)
	}
}

// keepPrint appends one print() line to a structured run's Logs, up to
// the output limit.
func (s *session) keepPrint(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.printsCut {
		return
	}
	if s.printBytes+len(text) > s.r.limits.MaxOutputBytes {
		s.printsCut = true
		s.prints = append(s.prints, fmt.Sprintf("[print output truncated: %d byte limit reached]", s.r.limits.MaxOutputBytes))
		return
	}
	s.printBytes += len(text) + 1
	s.prints = append(s.prints, text)
}

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

// echoBudget is the share of the output "[TOOL]" lines may use: three
// quarters, so the script's own prints and its result always have room.
func (s *session) echoBudget() int {
	return s.r.limits.MaxOutputBytes - s.r.limits.MaxOutputBytes/4
}

// echoResponse writes a successful call's echo line as the echo mode
// says: the whole answer (full), its first bytes (preview), or nothing.
func (s *session) echoResponse(label, quoted, text string) {
	switch s.echo {
	case EchoNone:
	case EchoPreview:
		preview := strings.Join(strings.Fields(text), " ")
		more := ""
		if len(preview) > echoPreviewBytes {
			cut := echoPreviewBytes
			for cut > 0 && !utf8.RuneStart(preview[cut]) {
				cut--
			}
			preview, more = preview[:cut], "…"
		}
		s.echoLine(fmt.Sprintf("[TOOL] %s preview (%d bytes): %s%s", label, len(text), preview, more), false)
	default:
		s.echoLine(fmt.Sprintf("[TOOL] %s raw response: %s", label, quoted), false)
	}
}

// echoLine records one "[TOOL]" line within the echo share of the output.
// Past the share, answers are no longer echoed (one line says so) and
// failure lines are cut short; under echo preview and none failure lines
// are always cut short.
func (s *session) echoLine(line string, failure bool) {
	if failure && s.echo != EchoFull {
		line = cut(line, echoErrorBytes, "… [cut]")
	}
	budget := s.echoBudget()
	s.mu.Lock()
	if s.echoBytes+len(line) > budget {
		if !failure {
			first := !s.echoCut
			s.echoCut = true
			s.mu.Unlock()
			if first {
				s.log(fmt.Sprintf("[TOOL] echo stopped: tool answers used their %d KiB share of the output; print what you need, or run with echo \"preview\" or \"none\"", budget>>10))
			}
			return
		}
		line = cut(line, echoErrorBytes, "… [cut]")
	}
	s.echoBytes += len(line) + 1
	s.mu.Unlock()
	s.log(line)
}

func (s *session) snapshotLogs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.logs...)
}

// addLedger records one call's line in the ledger.
func (s *session) addLedger(seq int, label, status string, started time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ledger = append(s.ledger, LedgerEntry{Seq: seq, Tool: label, Status: status, MS: time.Since(started).Milliseconds()})
}

// ledgerSnapshot is the ledger in call order.
func (s *session) ledgerSnapshot() []LedgerEntry {
	s.mu.Lock()
	out := append([]LedgerEntry(nil), s.ledger...)
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
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

// render is the response text for each outcome: Bifrost's for a Starlark
// run with echo full, the same shape with the call ledger in the
// Environment block and JavaScript's hints and note otherwise. The print
// output gives way first when the text would pass the output limit, so the
// error or the returned value stays whole.
func (r *Runtime) render(ex execution, code string) (string, bool) {
	serverKeys := ex.cat.idents()
	logs := ex.s.snapshotLogs()
	res := ex.res
	js := ex.lang == LanguageJavaScript
	keys := strings.Join(serverKeys, ", ")
	env := "Environment:\n  Available server keys: " + keys
	if js || ex.echo != EchoFull {
		env += ledgerText(ex.s.ledgerSnapshot())
	}
	max := r.limits.MaxOutputBytes
	var text string
	failed := false
	switch {
	case res.errKind != "":
		failed = true
		var hints []string
		if js {
			hints = jsErrorHints(res.errMsg, serverKeys)
		} else {
			hints = errorHints(res.errKind, res.errMsg, usesExceptions(code), serverKeys)
		}
		head := fmt.Sprintf("Execution %s error:\n\n%s\n\nHints:\n%s", res.errKind, res.errMsg, strings.Join(hints, "\n"))
		tail := "\n\n" + env
		logText := ""
		if len(logs) > 0 {
			logText = fitLogs("\n\nPrint Output:\n", logs, "\n", max-len(head)-len(tail))
		}
		text = head + logText + tail
	case len(logs) == 0 && !res.hasResult:
		hints := []string{
			"Add print() statements throughout your code to debug and see what's happening at each step",
			"Assign the final value to 'result' variable if you want to return it: result = computed_value",
			"Check that your tool calls are actually executing and returning data",
		}
		what := "(no print output and no result variable)"
		if js {
			hints = []string{
				"Add console.log() calls throughout your code to see what happens at each step",
				"Return the value you want back from the script: return computedValue",
				"Check that every tool call is awaited: const r = await tools.Server.tool({...})",
			}
			what = "(no console.log output and no returned value)"
		}
		text = fmt.Sprintf("Execution completed but produced no data:\n\n"+
			"The code executed without errors but returned no output %s.\n\n"+
			"Hints:\n%s\n\n%s", what, strings.Join(hints, "\n"), env)
	default:
		body := "Execution completed successfully."
		if res.hasResult {
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, res.result, "", "  "); err == nil {
				body += "\nReturn value: " + pretty.String()
			} else {
				body += "\nReturn value: " + string(res.result)
			}
		}
		body += "\n\n" + env
		if js {
			body += "\nNote: This is a JavaScript (QuickJS) environment; the script is the body of an async function. Use MCP tools for external interactions."
		} else {
			body += "\nNote: This is a Starlark (Python subset) environment. Use MCP tools for external interactions."
		}
		text = body
		if len(logs) > 0 {
			text = fitLogs("Print output:\n", logs, "\n\n", max-len(body)) + body
		}
	}
	if len(text) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + fmt.Sprintf("\n\n[output truncated at %d bytes]", max)
	}
	return text, failed
}

// fitLogs is the print section: prefix, the lines, suffix, cut to room
// bytes when it would not fit, so what follows it (the error, the
// returned value) stays whole. With too little room it is left whole and
// the overall cut applies.
func fitLogs(prefix string, logs []string, suffix string, room int) string {
	joined := strings.Join(logs, "\n")
	whole := prefix + joined + suffix
	if len(whole) <= room {
		return whole
	}
	note := fmt.Sprintf("\n[print output truncated: %d bytes left out to keep the rest of the answer]", len(joined))
	keep := room - len(prefix) - len(suffix) - len(note)
	if keep < 1024 {
		return whole
	}
	for keep > 0 && !utf8.RuneStart(joined[keep]) {
		keep--
	}
	note = fmt.Sprintf("\n[print output truncated: %d bytes left out to keep the rest of the answer]", len(joined)-keep)
	return prefix + joined[:keep] + note + suffix
}

// ledgerText is the call ledger as Environment lines, "" without calls.
func ledgerText(entries []LedgerEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n  Tool calls: %d", len(entries))
	for _, e := range entries {
		fmt.Fprintf(&b, "\n    %d. %s %s %dms", e.Seq, e.Tool, e.Status, e.MS)
	}
	return b.String()
}

// jsErrorHints are the debugging hints for a failed JavaScript script.
func jsErrorHints(msg string, keys []string) []string {
	var hints []string
	switch {
	case strings.Contains(msg, "SyntaxError"):
		hints = append(hints,
			"JavaScript syntax error: check brackets, quotes and commas.",
			"The script is the body of an async function: use await, and return the value you want back.")
	case strings.Contains(msg, "is not defined"):
		hints = append(hints,
			"A name is not defined. Each executeToolCode call runs in a fresh scope; nothing persists between calls.",
			"Tools hang off the tools object: await tools.Server.tool({ param: value })")
	case strings.Contains(msg, "not a function"):
		hints = append(hints,
			"You called something that is not a function.",
			"Check the tool's name with listToolFiles(language=\"javascript\") and its .d.ts stub.")
	case strings.Contains(msg, "of undefined") || strings.Contains(msg, "of null"):
		hints = append(hints,
			"A value is undefined or null where the script expected an object.",
			"Use console.log(JSON.stringify(result)) to see the actual shape first.")
	case strings.HasPrefix(msg, "tool call failed"):
		hints = append(hints,
			"A tool call failed and nothing caught it.",
			"Use tools.tryCall(\"Server.tool\", args) for {ok, status, result | error} instead of an exception, or wrap the call in try/catch.")
	}
	if len(hints) == 0 {
		hints = append(hints, "Check the error message above for details.")
	}
	if len(keys) > 0 {
		hints = append(hints, "Available server keys: "+strings.Join(keys, ", "))
	}
	return append(hints, "Call tools with: const r = await tools.Server.tool({ param: value })")
}

// deriveReason is the reason nested calls carry when the outer call had
// none: "code mode: " and the script's first comment line (# or //), or its first
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
		case strings.HasPrefix(t, "#"), strings.HasPrefix(t, "//"):
			// A Starlark comment, or a JavaScript one.
			if snippet == "" {
				snippet = strings.TrimSpace(strings.TrimLeft(t, "#/"))
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
