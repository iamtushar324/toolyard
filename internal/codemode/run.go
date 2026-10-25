package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Structured runs. Run executes a script exactly like ExecuteToolCode (the
// same worker, limits, catalog and gateway pipeline for every nested
// call, in Starlark or JavaScript) and returns what happened as data instead of Bifrost's text: the
// run's status, its JSON result, its print lines, one record per tool
// call, and the call that held it for approval. CallHooks let the caller
// answer a call from a record instead of sending it (a resumed run
// replays the calls before the held one), add arguments to a call (an
// Inbox grant), stop the run with a typed error, and observe each call
// the gateway answered.

// Run statuses (RunResult.Status) and call statuses (CallRecord.Status).
// A call is never "failed" for a run that ends permission_required,
// deferred or outcome_unknown other than the call that ended it.
const (
	StatusSucceeded          = "succeeded"
	StatusFailed             = "failed"
	StatusPermissionRequired = "permission_required"
	StatusDeferred           = "deferred"
	StatusOutcomeUnknown     = "outcome_unknown"
)

// Error kinds (RunResult.ErrorKind, CallError.Kind).
const (
	// ErrorScriptError: a syntax or runtime error in the script itself.
	ErrorScriptError = "script_error"
	// ErrorToolError: a tool call returned an error, or was refused.
	ErrorToolError = "tool_error"
	// ErrorTimeout: the wall clock (or the caller's deadline) ended the
	// script. With status outcome_unknown it cut a tool call short.
	ErrorTimeout = "timeout"
	// ErrorLimit: a run limit (tool calls, steps, memory, result size).
	ErrorLimit = "limit"
	// ErrorScriptDiverged: a resumed script made a call other than the
	// one recorded at its place (set by the caller's BeforeCall).
	ErrorScriptDiverged = "script_diverged"
	// ErrorGrantInvalid: the gateway refused a call's _grant.
	ErrorGrantInvalid = "grant_invalid"
	// ErrorOutcomeUnknown: the gateway could not say whether a call ran.
	ErrorOutcomeUnknown = "outcome_unknown"
	// ErrorInternal: the script worker failed to start or crashed, or a
	// hook failed.
	ErrorInternal = "internal"
	// ErrorBusy names ErrBusy for an API's error object; Run itself
	// returns ErrBusy and no RunResult.
	ErrorBusy = "busy"
	// ErrorInterrupted: the caller's context was cancelled (toolyard is
	// shutting down, or the client went away) before the script finished.
	// The run is outcome_unknown when a tool call had been sent, failed
	// (nothing ran) when none had. A script's own clock is ErrorTimeout.
	ErrorInterrupted = "interrupted"
)

// The messages of an interrupted script.
const (
	msgInterruptedAfterCalls = "toolyard stopped the script before it finished (the server is restarting, or the caller went away), after it sent %d tool call(s); a call may have taken effect, so check what the script changed before you run it again"
	msgInterruptedNothingRan = "toolyard stopped the script before it finished (the server is restarting, or the caller went away), before it sent any tool call; nothing ran. Run it again"
)

// OutcomeUnknownMarker is the result metadata key (Meta.AdditionalFields,
// value true) by which the gateway says a call may or may not have run:
// the upstream transport failed after dispatch, or its completion could
// not be audited. A run ends outcome_unknown on such a call.
const OutcomeUnknownMarker = "toolyard.outcome_unknown"

// grantInvalidMarker is the metadata key of the gateway's answer to a
// call whose _grant it refused.
const grantInvalidMarker = "toolyard.grant_invalid"

// approvalIDArg and grantArg are the gateway's argument names for a
// legacy approval resume and an Inbox grant token.
const (
	approvalIDArg = "_approval_id"
	grantArg      = "_grant"
)

// ErrBusy is Run's and AcquireSlot's answer when every script slot stayed
// taken for Worker.AcquireWait. No script ran.
var ErrBusy = errors.New("code mode is busy: every script slot is in use; try again in a moment")

// RunOptions configure one Run.
type RunOptions struct {
	// Reason is the run's reason; nested calls carry it unless they pass
	// their own. "" derives one from the script, as ExecuteToolCode does.
	Reason string
	// Hooks wrap every tool call; nil makes plain calls.
	Hooks CallHooks
	// Slot is a slot from this Runtime's AcquireSlot, taken before the
	// caller committed to the run (so it can answer busy without creating
	// one). Run releases it. nil makes Run take a slot itself.
	Slot *Slot
	// Language is LanguageStarlark (also "") or LanguageJavaScript.
	Language string
}

// PendingCall is one tool call a script is about to make.
type PendingCall struct {
	// Seq is the call's 1-based place among the script's calls. The same
	// script with the same tool answers makes the same calls in the same
	// order, so a resume meets each recorded call at its Seq.
	Seq int
	// Tool is the server key and the tool's own name, as listToolFiles and
	// the gateway's catalog know them: "context7.resolve-library-id",
	// "toolyard.inbox_request".
	Tool string
	// Target is the catalog name the gateway dispatches ("inbox.request"
	// for toolyard.inbox_request; the same as Tool for an upstream tool).
	Target string
	// Args are the arguments as the gateway receives them: the script's,
	// with the reason field filled. BeforeCall may add keys (a "_grant"
	// token), so never persist Args as they are.
	Args map[string]any
	// ScriptArgs is a deep copy of the script's own arguments, before the
	// reason field was filled and without any "_grant" token: the value to
	// record, and to compare with the record on a resume. Changing it
	// changes nothing that is sent.
	ScriptArgs map[string]any
	// ReasonField is the argument the call's reason travels in ("_reason",
	// or "__toolyard_reason" for a tool with its own _reason), which the
	// gateway strips before it checks the call against a grant.
	ReasonField string
}

// CallHooks wrap each tool call of a Run. Both run on the goroutine that
// makes the call. Calls a script runs in parallel run their hooks at the
// same time, so a hook guards its own state. Run returns only once every
// hook call it started has returned.
type CallHooks interface {
	// BeforeCall runs before a call goes to the gateway. Its context ends
	// when the run's clock does, and also as soon as the script has
	// stopped on another call (nothing more is sent then), so a hook that
	// waits (for the calls numbered before this one, say) watches it.
	// Returning handled=true answers the call with res instead (it is not
	// sent; its record says Replayed): give
	// mcp.NewToolResultText(CallOutcome.Text) to replay a recorded call
	// exactly, ReplayFailure to give the script the failure it saw, or
	// ReplayHold the hold. Changing c.Args (adding "_grant") changes what
	// is sent; nothing else in c is read back. Returning an error stops
	// the script at this call: a *CallError's Kind becomes the run's error
	// kind (ErrorOutcomeUnknown makes the run outcome_unknown), any other
	// error fails the run with ErrorInternal.
	BeforeCall(ctx context.Context, c *PendingCall) (res *mcp.CallToolResult, handled bool, err error)
	// AfterCall runs once the gateway answered a call that was sent (not
	// for a call BeforeCall answered or stopped), on a context with the
	// run's values that the script's clock does not cancel. It is skipped
	// for a call still running when Run returned: that call's record says
	// outcome_unknown.
	AfterCall(ctx context.Context, c PendingCall, out CallOutcome)
}

// CallError stops a run from BeforeCall with a kind the caller maps
// (ErrorScriptDiverged, ErrorGrantInvalid, ErrorOutcomeUnknown).
type CallError struct {
	Kind    string
	Message string
}

func (e *CallError) Error() string { return e.Message }

// replayedFailureMarker is the metadata key (value: the error kind) of a
// failure ReplayFailure made. It counts only on an answer BeforeCall gave
// (handled), never on one from the gateway.
const replayedFailureMarker = "toolyard.replayed_failure"

// ReplayFailure is a BeforeCall answer that gives the script a recorded
// failure again: the same text, and the same error kind (ErrorToolError,
// ErrorGrantInvalid or ErrorLimit; any other is ErrorToolError) should the
// script not catch it.
func ReplayFailure(kind, text string) *mcp.CallToolResult {
	switch kind {
	case ErrorToolError, ErrorGrantInvalid, ErrorLimit:
	default:
		kind = ErrorToolError
	}
	res := mcp.NewToolResultError(text)
	res.Meta = &mcp.Meta{AdditionalFields: map[string]any{replayedFailureMarker: kind}}
	return res
}

// ReplayHold is a BeforeCall answer that gives the script a recorded hold
// again: status StatusPermissionRequired or StatusDeferred, the gateway's
// text, and its structured content (the Inbox draft, the approval id),
// nil when it was not kept. Should the script not catch it, the run holds
// at this call as it did before.
func ReplayHold(status, text string, structured json.RawMessage) *mcp.CallToolResult {
	marker := "toolyard.permission_required"
	res := mcp.NewToolResultError(text)
	if status == StatusDeferred {
		marker = "toolyard.deferred"
		res = mcp.NewToolResultText(text)
	}
	if len(structured) > 0 && json.Valid(structured) {
		res.StructuredContent = structured
	}
	res.Meta = &mcp.Meta{AdditionalFields: map[string]any{marker: true}}
	return res
}

// replayedFailure reads the kind ReplayFailure put on res, "" for none.
func replayedFailure(res *mcp.CallToolResult) string {
	if res == nil || res.Meta == nil {
		return ""
	}
	kind, _ := res.Meta.AdditionalFields[replayedFailureMarker].(string)
	return kind
}

// CallOutcome is what AfterCall learns about a call the gateway answered.
type CallOutcome struct {
	// Record is the call's final record.
	Record CallRecord
	// Result and Err are the gateway's answer.
	Result *mcp.CallToolResult
	Err    error
	// Text is the value the script received when the call succeeded, ""
	// when it did not; mcp.NewToolResultText(Text) replays it.
	Text string
	// Message is the failure text the script saw ("tool call failed for
	// <tool>: <text>"), "" when the call succeeded.
	Message string
	// Kind is a failed call's error kind (ErrorToolError, ErrorGrantInvalid,
	// ErrorLimit), "" otherwise.
	Kind string
	// FailureText is the <text> part of Message for a failed or held call
	// (at most 64 KiB): ReplayFailure(Kind, FailureText) and
	// ReplayHold(Held.Status, FailureText, Held.Structured) give the
	// script the same answer again.
	FailureText string
	// Held is the hold of a permission_required or deferred call, with its
	// legacy approval id, nil otherwise.
	Held *HeldCall
}

// CallRecord is one tool call of a run.
type CallRecord struct {
	Seq    int
	Tool   string
	Target string
	// Status is one of the call statuses: succeeded, failed,
	// permission_required, deferred, outcome_unknown.
	Status string
	// Replayed marks a call BeforeCall answered from a record.
	Replayed bool
	// Sent marks a call that went to the gateway. A failed call that was
	// not sent (the script or its clock stopped first) changed nothing.
	Sent bool
	// StartedAt and FinishedAt are unix milliseconds.
	StartedAt  int64
	FinishedAt int64
}

// HeldCall is the call that held a run for approval.
type HeldCall struct {
	Seq    int
	Tool   string
	Target string
	// Status is permission_required (no grant: ask with inbox.request,
	// starting from Draft) or deferred (a legacy approval is pending or
	// its tool is executing; ApprovalID names it).
	Status string
	// Draft is the inbox.request draft the gateway returned (its
	// structured content's "draft"), nil when it returned none.
	Draft json.RawMessage
	// ApprovalID is the legacy approval's id, "" when there is none.
	ApprovalID string
	// Message is the gateway's text for the call (at most 64 KiB).
	Message string
	// Structured is the gateway's whole structured content for the call,
	// nil when it had none or it was over 6 MiB.
	Structured json.RawMessage
}

// RunResult is what one Run did.
type RunResult struct {
	// Status is one of the run statuses.
	Status string
	// Result is the script's `result` value as JSON, nil when it set none
	// or did not succeed.
	Result json.RawMessage
	// ErrorKind and ErrorMessage say why a failed or outcome_unknown run
	// stopped; both are "" otherwise.
	ErrorKind    string
	ErrorMessage string
	// Logs are the script's print() lines, up to the output limit.
	Logs []string
	// Calls are the tool calls, in order.
	Calls []CallRecord
	// Held is set when Status is permission_required or deferred.
	Held *HeldCall
}

// Slot is one of the runtime's concurrent script slots, held from
// AcquireSlot until Release.
type Slot struct {
	release func()
	once    sync.Once
}

// Release frees the slot. Releasing twice, or a nil slot, is harmless.
func (s *Slot) Release() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.release != nil {
			s.release()
		}
	})
}

// AcquireSlot takes one of the runtime's script slots for the ctx caller
// (WithAgentKey, else the Caller's AgentKey): at most
// Worker.MaxConcurrent scripts run at once, and at most
// Worker.MaxPerAgent for one caller. A run that cannot start waits in a
// fair queue up to Worker.AcquireWait: an error that is ErrBusy when no
// slot came free (its text says which cap), ctx's error when ctx ended
// first. Release the slot, or pass it to Run, which does.
func (r *Runtime) AcquireSlot(ctx context.Context) (*Slot, error) {
	release, err := r.limiter.acquire(ctx, r.agentKey(ctx), r.worker.AcquireWait)
	if err != nil {
		return nil, err
	}
	return &Slot{release: release}, nil
}

// slotsInUse is how many scripts hold a slot (tests).
func (r *Runtime) slotsInUse() int { return r.limiter.inUse() }

// SlotWaiters is how many scripts wait in the slot queue now.
func (r *Runtime) SlotWaiters() int { return r.limiter.waiting() }

// Run executes code for the ctx caller (the same context ExecuteToolCode
// takes: the agent identity every nested call runs as) and reports the
// outcome as data. A tool call that is held for approval, fails, or has
// an unknown outcome ends the script, as in ExecuteToolCode; the run's
// status says which. The error is non-nil only when no script ran:
// ErrBusy when no slot came free, or ctx's error when ctx ended while
// waiting for one.
func (r *Runtime) Run(ctx context.Context, code string, opts RunOptions) (*RunResult, error) {
	if opts.Slot != nil {
		defer opts.Slot.Release()
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return &RunResult{Status: StatusFailed, ErrorKind: ErrorScriptError,
			ErrorMessage: "code is required and must be a non-empty string"}, nil
	}
	lang, err := ParseLanguage(opts.Language)
	if err != nil {
		return &RunResult{Status: StatusFailed, ErrorKind: ErrorScriptError, ErrorMessage: err.Error()}, nil
	}
	if opts.Slot == nil {
		slot, err := r.AcquireSlot(ctx)
		if err != nil {
			return nil, err
		}
		defer slot.Release()
	}
	ex := r.execute(ctx, code, execOptions{reason: opts.Reason, hooks: opts.Hooks, structured: true, lang: lang})
	return ex.s.result(ex.res), nil
}

// result seals the session and assembles the RunResult. A call still
// running now (a worker run the clock ended) is outcome_unknown if it was
// sent, failed if it never was; it gets no AfterCall.
func (s *session) result(res scriptResult) *RunResult {
	s.mu.Lock()
	s.sealed = true
	now := nowMS()
	calls := make([]CallRecord, 0, len(s.records))
	inFlight := false
	var running CallRecord // the first call still running, when one is
	for _, st := range s.records {
		rec := st.rec
		rec.Sent = st.sent
		if !st.done {
			rec.FinishedAt = now
			rec.Status = StatusFailed
			if st.sent {
				rec.Status = StatusOutcomeUnknown
				if !inFlight {
					running = rec
				}
				inFlight = true
			}
		}
		calls = append(calls, rec)
	}
	stop := s.stop
	sentAny := s.sentCalls > 0
	prints := append([]string(nil), s.prints...)
	s.mu.Unlock()
	s.hookWG.Wait()

	rr := &RunResult{Logs: prints, Calls: calls}
	errText := func(fallback string) string {
		if res.errMsg != "" {
			return res.errMsg
		}
		return fallback
	}
	switch {
	case interruptedBy(res, stop):
		// The caller's context ended first (a drain, a shutdown, a client
		// that went away): not the script's own timeout.
		rr.Status, rr.ErrorKind, rr.ErrorMessage = StatusFailed, ErrorInterrupted, res.errMsg
		if sentAny || inFlight {
			rr.Status = StatusOutcomeUnknown
		}
	case stop != nil && stop.held != nil && inFlight:
		// Held, but a call the script ran beside the held one was still
		// running when the script ended: its outcome is unknown, and a
		// resume would refuse to repeat it, so the run cannot go on.
		rr.Status, rr.ErrorKind = StatusOutcomeUnknown, ErrorOutcomeUnknown
		rr.ErrorMessage = fmt.Sprintf("call %d (%s) held the run for approval while call %d (%s) was still running; "+
			"that call's outcome is unknown, so the run cannot be resumed: check whether it took effect, then start a new run",
			stop.held.Seq, stop.held.Tool, running.Seq, running.Tool)
	case stop != nil:
		rr.Status = stop.status
		if stop.held != nil {
			rr.Held = stop.held
			break
		}
		rr.ErrorKind = stop.kind
		rr.ErrorMessage = errText(stop.msg)
	case inFlight:
		rr.Status, rr.ErrorKind = StatusOutcomeUnknown, ErrorTimeout
		rr.ErrorMessage = errText("the script stopped while a tool call was running; its outcome is unknown, so check before you repeat a write")
	case res.errKind != "":
		rr.Status, rr.ErrorKind, rr.ErrorMessage = StatusFailed, ErrorScriptError, res.errMsg
		switch res.cause {
		case ErrorTimeout, ErrorLimit, ErrorInternal, ErrorBusy:
			rr.ErrorKind = res.cause
		}
	case res.hasResult && !json.Valid(res.result):
		rr.Status, rr.ErrorKind = StatusFailed, ErrorInternal
		rr.ErrorMessage = "the script worker returned a result that is not valid JSON"
	default:
		rr.Status = StatusSucceeded
		if res.hasResult {
			rr.Result = res.result
		}
	}
	return rr
}

// callOutcome is how one gateway answer reads: the call's status, and the
// run status and error kind it implies when it ends the script.
type callOutcome struct {
	status    string
	runStatus string
	kind      string
	held      *HeldCall
	// text is the value a successful call hands the script.
	text string
	// failText is the gateway's text for a failed or held call, as the
	// script saw it after "tool call failed for <tool>: ".
	failText string
}

// classify reads one answer to a call. cutByClock says the run's context
// had ended when the answer came, so an error may only mean the call was
// cut short. Order matters: the gateway's markers first, then its text
// contract for an unknown outcome, then plain errors.
func classify(res *mcp.CallToolResult, err error, args map[string]any, cutByClock bool) callOutcome {
	unknown := callOutcome{status: StatusOutcomeUnknown, runStatus: StatusOutcomeUnknown, kind: ErrorOutcomeUnknown}
	if cutByClock {
		unknown.kind = ErrorTimeout
	}
	if err != nil {
		// RouteCall answers every refusal with a result; an error means
		// the pipeline broke somewhere, possibly after dispatch.
		return unknown
	}
	if res == nil {
		return callOutcome{status: StatusSucceeded}
	}
	if metaFlag(res, OutcomeUnknownMarker) {
		unknown.kind = ErrorOutcomeUnknown
		return unknown
	}
	if metaFlag(res, grantInvalidMarker) {
		return callOutcome{status: StatusFailed, runStatus: StatusFailed, kind: ErrorGrantInvalid}
	}
	if st := heldStatus(res); st != "" {
		return callOutcome{status: st, runStatus: st}
	}
	if _, resumed := args[approvalIDArg].(string); resumed && structuredStatus(res) == StatusOutcomeUnknown {
		// A legacy approval resume answers an approval whose execution
		// outcome is unknown with its snapshot, not an error.
		unknown.kind = ErrorOutcomeUnknown
		return unknown
	}
	if res.IsError {
		// The gateway's text contract: "Tool outcome unknown: …" when the
		// completion audit failed (the same test it applies itself before
		// recording an approval's outcome).
		if strings.Contains(strings.ToLower(resultText(res, "")), "outcome unknown") {
			unknown.kind = ErrorOutcomeUnknown
			return unknown
		}
		if cutByClock {
			return unknown
		}
		return callOutcome{status: StatusFailed, runStatus: StatusFailed, kind: ErrorToolError}
	}
	return callOutcome{status: StatusSucceeded}
}

// metaFlag reports a true boolean under key in res's metadata.
func metaFlag(res *mcp.CallToolResult, key string) bool {
	if res == nil || res.Meta == nil {
		return false
	}
	v, ok := res.Meta.AdditionalFields[key].(bool)
	return ok && v
}

// heldStatus names the hold a result reports: permission_required for a
// restricted call made without a grant, deferred for a legacy approval
// that is pending or whose tool is executing, "" for none.
func heldStatus(res *mcp.CallToolResult) string {
	switch {
	case metaFlag(res, "toolyard.permission_required"):
		return StatusPermissionRequired
	case metaFlag(res, "toolyard.deferred"), metaFlag(res, "toolyard.executing"):
		return StatusDeferred
	}
	return ""
}

// structuredStatus is the "status" field of res's structured content, or
// "".
func structuredStatus(res *mcp.CallToolResult) string {
	if res.StructuredContent == nil {
		return ""
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return ""
	}
	var v struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return v.Status
}

// heldCall describes the call that held the run, keeping the gateway's
// draft and approval id from its structured content.
func heldCall(pc PendingCall, status string, res *mcp.CallToolResult, text string) *HeldCall {
	h := &HeldCall{Seq: pc.Seq, Tool: pc.Tool, Target: pc.Target, Status: status, Message: text}
	if res == nil || res.StructuredContent == nil {
		return h
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil || len(raw) > maxCallResultBytes {
		return h
	}
	h.Structured = raw
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return h
	}
	if d, ok := fields["draft"]; ok && string(d) != "null" {
		h.Draft = d
	}
	if a, ok := fields["approval_id"]; ok {
		var id string
		if json.Unmarshal(a, &id) == nil {
			h.ApprovalID = id
		}
	}
	return h
}

// scriptArgsOf copies a call's arguments for PendingCall.ScriptArgs.
func scriptArgsOf(args map[string]any) map[string]any {
	out, _ := copyValue(args).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	delete(out, grantArg)
	return out
}

// copyValue deep-copies the maps and lists of a JSON-shaped value.
func copyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = copyValue(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = copyValue(x)
		}
		return out
	}
	return v
}

func nowMS() int64 { return time.Now().UnixMilli() }
