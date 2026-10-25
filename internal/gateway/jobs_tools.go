package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/jobs"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// Async tools (docs/guidelines/async-tools.md). A call to a tool the
// administrator marked async, a call made through jobs.start or POST
// /v1/jobs, and a task-augmented MCP call run as durable jobs
// (internal/jobs). The job only starts once routeEntry has admitted the
// call: access, the drain check, policy, approval or a redeemed grant and
// the call.start / call.allowed audit rows all come first, exactly as for
// a call that runs inline. A call that needs approval answers as it always
// did (permission_required, or the legacy deferred envelope, whose
// executor already runs the tool in the background); it never becomes a
// job that waits.
//
// The job runs the same dispatch (identity forwarding, the call.dispatch
// and outcome audit rows, metrics) on a context that keeps the call's
// values but not its cancellation, under the job's own time limit instead
// of the per-call upstream timeout. Calls made inside a job (a script's,
// tools.execute's) run inline.

// Job tool names. They live under the reserved "jobs" upstream, an
// always-on access group, and are pinned like the approval pollers.
const (
	MetaJobsStatus = "jobs.status"
	MetaJobsResult = "jobs.result"
	MetaJobsWait   = "jobs.wait"
	MetaJobsCancel = "jobs.cancel"
	MetaJobsList   = "jobs.list"
	MetaJobsStart  = "jobs.start"

	jobsUpstream = "jobs"
	// viaAsyncJob is the metric Via of a job's own dispatch.
	viaAsyncJob = "async-job"
	// jobWaitMaxSeconds caps jobs.wait.
	jobWaitMaxSeconds = 60
	// jobSnapshotResultLimit is the largest result a status or wait
	// snapshot carries; larger ones come from jobs.result.
	jobSnapshotResultLimit = 256 << 10
	// eventJobStarted and eventJobCancelled are the audit rows of a job.
	eventJobStarted   = "job.started"
	eventJobCancelled = "job.cancelled"
)

// Paths into a job (Job.Via) besides the call's own via.
const (
	jobViaREST  = "rest"
	jobViaStart = MetaJobsStart
	jobViaTask  = "mcp_task"
)

var jobToolNames = []string{MetaJobsStatus, MetaJobsResult, MetaJobsWait, MetaJobsCancel, MetaJobsList, MetaJobsStart}

func init() {
	for _, n := range jobToolNames {
		PinnedTools[n] = struct{}{}
	}
}

// AsyncPolicy is the administrator's async setting of each upstream
// server: *internal/upstreams.Service.
type AsyncPolicy interface {
	AsyncSetting(upstream string) jobs.AsyncSetting
}

// jobsState is the gateway's async-tool state, embedded in Gateway.
type jobsState struct {
	jobs        *jobs.Service
	asyncPolicy AsyncPolicy
}

// SetAsyncPolicy wires the administrator's async settings. Without it only
// jobs.start, POST /v1/jobs and MCP tasks start jobs.
func (g *Gateway) SetAsyncPolicy(p AsyncPolicy) { g.asyncPolicy = p }

// Jobs is the job service (nil when async tools are off).
func (g *Gateway) Jobs() *jobs.Service { return g.jobs }

// asyncSettingOf is a server's async setting for servers.list (nil when
// it has none or async tools are off).
func (g *Gateway) asyncSettingOf(upstream string) *jobs.AsyncSetting {
	if g.jobs == nil || g.asyncPolicy == nil {
		return nil
	}
	s := g.asyncPolicy.AsyncSetting(upstream)
	if s.IsZero() {
		return nil
	}
	n := s.Normalized()
	return &n
}

// jobRunKey marks the context of a job's own run: calls made in it run
// inline.
type jobRunKey struct{}

func inJob(ctx context.Context) bool {
	v, _ := ctx.Value(jobRunKey{}).(bool)
	return v
}

// asyncRequestKey carries an explicit request to run the next routed call
// as a job: jobs.start, POST /v1/jobs or a task-augmented MCP call.
type asyncRequestKey struct{}

type asyncRequest struct {
	via            string
	idempotencyKey string
	used           atomic.Bool

	mu  sync.Mutex
	job *jobs.Job
	err error
}

func withAsyncRequest(ctx context.Context, r *asyncRequest) context.Context {
	return context.WithValue(ctx, asyncRequestKey{}, r)
}

func (r *asyncRequest) started(job *jobs.Job, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.job, r.err = job, err
}

func (r *asyncRequest) result() (*jobs.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.job, r.err
}

// asyncPlan says how an admitted call runs as a job.
type asyncPlan struct {
	// afterMs > 0 waits that long for an inline answer first.
	afterMs int
	req     *asyncRequest
}

// asyncPlanFor decides whether the admitted call to entry runs as a job.
func (g *Gateway) asyncPlanFor(ctx context.Context, entry toolEntry) *asyncPlan {
	if g.jobs == nil || entry.upstream == jobsUpstream || inJob(ctx) {
		return nil
	}
	if r, ok := ctx.Value(asyncRequestKey{}).(*asyncRequest); ok && r != nil && r.used.CompareAndSwap(false, true) {
		return &asyncPlan{req: r}
	}
	if g.asyncPolicy == nil || internalEntry(entry) {
		return nil
	}
	rule := g.asyncPolicy.AsyncSetting(entry.upstream).RuleFor(entry.originalName)
	switch rule.Mode {
	case jobs.AsyncAlways:
		return &asyncPlan{}
	case jobs.AsyncAfter:
		return &asyncPlan{afterMs: rule.AfterMs}
	}
	return nil
}

// jobRun is the dispatch a job runs: the call routeEntry admitted.
type jobRun func(ctx context.Context, entry toolEntry, ev *metrics.Event) (*mcp.CallToolResult, error)

// jobCall is an admitted call that may run as a job: what routeEntry
// knows about it.
type jobCall struct {
	entry       toolEntry
	args        map[string]any
	agentID     string
	reason      string
	intent      string
	callbackRef string
	// grant is set for a call admitted by a redeemable grant (its run
	// redeems it); otherwise the policy allowed the call.
	grant bool
}

// runOrStartJob runs the admitted call inline, or as a job when
// asyncPlanFor says so.
func (g *Gateway) runOrStartJob(ctx context.Context, c jobCall, ev *metrics.Event, run jobRun) (*mcp.CallToolResult, error) {
	plan := g.asyncPlanFor(ctx, c.entry)
	switch {
	case plan == nil:
		return run(ctx, c.entry, ev)
	case plan.afterMs > 0:
		return g.runAfter(ctx, plan, c, ev, run)
	}
	return g.startJob(ctx, plan, c, ev, run)
}

// jobRequest is the stored description of c's job.
func (g *Gateway) jobRequest(ctx context.Context, plan *asyncPlan, c jobCall, ev *metrics.Event) jobs.StartRequest {
	raiser, _ := actor.RaiserFrom(ctx)
	req := jobs.StartRequest{
		AgentID: c.agentID, OwnerUserID: g.ownerOf(ctx, c.agentID), SessionID: raiser.ClientSessionID,
		Tool: c.entry.tool.Name, Upstream: c.entry.upstream, ParamsHash: paramsHash(c.args), Reason: c.reason,
		Via: ev.Via, CallbackRef: c.callbackRef,
	}
	if plan.req != nil {
		req.Via, req.IdempotencyKey = plan.req.via, plan.req.idempotencyKey
	}
	return req
}

// jobContext is the context a job's call runs on: the call's values, no
// per-call timeout of its own beyond the job limit, nested calls inline,
// and no connect link minted into a result that is stored.
func jobContext(ctx context.Context) context.Context {
	return WithoutConnectLinks(context.WithValue(ctx, jobRunKey{}, true))
}

// jobMetrics records the metric of a job's own dispatch.
func (g *Gateway) jobMetrics(started time.Time, res *mcp.CallToolResult, derr error, jev *metrics.Event) {
	jev.TotalLatencyMs = int(time.Since(started).Milliseconds())
	if jev.Outcome == "" {
		jev.Outcome = metrics.OutcomeOK
		if derr != nil || (res != nil && res.IsError) {
			jev.Outcome = metrics.OutcomeError
		}
	}
	if res != nil {
		jev.ResultSizeBytes = approxResultSize(res)
	}
	if g.metrics != nil {
		g.metrics.Record(*jev)
	}
}

func newJobEvent(base metrics.Event) metrics.Event {
	jev := base
	jev.TS, jev.Via = time.Now().UnixMilli(), viaAsyncJob
	jev.Outcome, jev.ErrorClass, jev.UpstreamLatencyMs, jev.TotalLatencyMs = "", "", 0, 0
	return jev
}

// startJob starts the admitted call as a queued job and answers with the
// job envelope. When the job budget is full, a tool the administrator
// made async runs inline instead; an explicit job request (jobs.start,
// POST /v1/jobs, an MCP task) is refused.
func (g *Gateway) startJob(ctx context.Context, plan *asyncPlan, c jobCall, ev *metrics.Event, run jobRun) (*mcp.CallToolResult, error) {
	req := g.jobRequest(ctx, plan, c, ev)
	jobEntry := c.entry
	jobEntry.noCallTimeout = true
	base := *ev
	job, h, err := g.jobs.Start(jobContext(ctx), req, func(rctx context.Context, progress jobs.ProgressFunc) jobs.Outcome {
		// Queued for a while: the access and policy that admitted the
		// call must still hold when it starts.
		if refusal := g.recheckJob(rctx, c); refusal != "" {
			return jobs.Outcome{Status: jobs.StatusFailed, Envelope: encodeJobResult(mcp.NewToolResultError(refusal)), IsError: true, Error: refusal}
		}
		// Cancelled (jobs.cancel, shutdown) before the call was sent:
		// nothing ran.
		if rctx.Err() != nil {
			return jobs.Outcome{Status: jobs.StatusCancelled, Error: "cancelled before the call was sent; nothing ran"}
		}
		jev := newJobEvent(base)
		started := time.Now()
		res, derr := run(withProgressSink(rctx, progress), jobEntry, &jev)
		g.jobMetrics(started, res, derr, &jev)
		return jobOutcome(res, derr)
	})
	if plan.req != nil {
		plan.req.started(job, err)
	}
	if err != nil {
		if errors.Is(err, jobs.ErrTooManyActive) && plan.req == nil {
			return run(ctx, c.entry, ev)
		}
		ev.Outcome, ev.ErrorClass = metrics.OutcomeError, "job"
		return jobStartError(c.entry, err), nil
	}
	ev.Outcome, ev.ErrorClass = metrics.OutcomeDeferred, "async_job"
	if h != nil {
		g.auditJobStarted(ctx, c, job)
	}
	return jobEnvelope(job), nil
}

// runAfter is the `after` mode: the call starts at once, inline, and only
// a call still running after the plan's wait becomes a job (Adopt), so a
// fast call stores nothing and takes no job slot. When the job budget is
// full, or the server drains, the caller keeps waiting for the answer
// inline.
func (g *Gateway) runAfter(ctx context.Context, plan *asyncPlan, c jobCall, ev *metrics.Event, run jobRun) (*mcp.CallToolResult, error) {
	jobEntry := c.entry
	jobEntry.noCallTimeout = true
	callCtx, cancelTimeout := context.WithTimeout(context.WithoutCancel(jobContext(ctx)), g.jobs.MaxDuration())
	callCtx, cancelCall := context.WithCancel(callCtx)
	cancel := func() { cancelCall(); cancelTimeout() }
	relay := jobs.NewProgressRelay()
	done := make(chan struct{})
	base := *ev
	jev := newJobEvent(base)
	started := time.Now()
	var res *mcp.CallToolResult
	var derr error
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				derr = fmt.Errorf("panic in %s: %v", c.entry.tool.Name, p)
			}
		}()
		res, derr = run(withProgressSink(callCtx, relay.Report), jobEntry, &jev)
	}()
	wait := time.NewTimer(time.Duration(plan.afterMs) * time.Millisecond)
	defer wait.Stop()
	select {
	case <-done:
		cancel()
		// Answered in time: as if it ran inline (and no callback is sent).
		mergeEvent(ev, &jev)
		return res, derr
	case <-wait.C:
	case <-ctx.Done():
	}
	job, err := g.jobs.Adopt(ctx, g.jobRequest(ctx, plan, c, ev), jobs.Adoption{
		StartedAt: started, Cancel: cancel, Done: done, Relay: relay,
		Outcome: func() jobs.Outcome {
			g.jobMetrics(started, res, derr, &jev)
			return jobOutcome(res, derr)
		},
	})
	if err != nil {
		// No job (budget full, draining): the call keeps running and its
		// caller waits for it, as without async tools.
		<-done
		cancel()
		mergeEvent(ev, &jev)
		return res, derr
	}
	ev.Outcome, ev.ErrorClass = metrics.OutcomeDeferred, "async_job"
	g.auditJobStarted(ctx, c, job)
	return jobEnvelope(job), nil
}

// mergeEvent copies the dispatch's metric fields of a call that ended
// inline onto the call's own event.
func mergeEvent(ev, jev *metrics.Event) {
	ev.Outcome, ev.ErrorClass, ev.UpstreamLatencyMs = jev.Outcome, jev.ErrorClass, jev.UpstreamLatencyMs
}

// auditJobStarted records that c became job.
func (g *Gateway) auditJobStarted(ctx context.Context, c jobCall, job *jobs.Job) {
	_ = g.audit.Write(ctx, audit.Event{
		EventType: eventJobStarted, AgentID: c.agentID, UpstreamName: c.entry.upstream, ToolName: c.entry.tool.Name,
		Reason: c.reason, ResultSummary: job.ID,
	})
}

// recheckJob is the admission check again, when a queued job starts: the
// caller's access, and the policy (a call the policy allowed must still be
// allowed; a granted call must not be denied now). "" when it holds.
func (g *Gateway) recheckJob(ctx context.Context, c jobCall) string {
	if !g.allowsEntry(ctx, c.agentID, c.entry) {
		_ = g.audit.Write(ctx, audit.Event{
			EventType: audit.EventCallDenied, AgentID: c.agentID, UpstreamName: c.entry.upstream, ToolName: c.entry.tool.Name,
			Decision: "deny", Reason: accessDeniedReason,
		})
		return "access to " + access.GroupOf(c.entry.upstream, c.entry.tool.Name) + " was removed before the job started; nothing ran"
	}
	d := g.decide(c.entry, c.agentID, c.intent, c.reason, c.args)
	switch {
	case d.Action == policy.ActionDeny:
		return "denied by policy before the job started; nothing ran: " + d.Reason
	case d.Action != policy.ActionAllow && !c.grant:
		return "the call needs approval now (the policy changed while the job waited); nothing ran: ask with inbox.request"
	}
	return ""
}

// attachJobs wires the job service's audit log and the access guard that
// stops running jobs whose agent lost access (disabled, blocked, server
// access withdrawn).
func (g *Gateway) attachJobs(svc *jobs.Service) {
	svc.SetAuditor(jobAuditor{g: g})
	svc.SetGuard(func(ctx context.Context, agentID, upstream, tool string) bool {
		return g.scopeFor(ctx, agentID).AllowsTool(upstream, tool)
	})
}

// jobAuditor writes the job events the job service decides itself.
type jobAuditor struct{ g *Gateway }

func (a jobAuditor) JobEvent(ctx context.Context, eventType string, j jobs.Job, detail string) {
	_ = a.g.audit.Write(ctx, audit.Event{
		EventType: eventType, AgentID: j.AgentID, UpstreamName: j.Upstream, ToolName: j.Tool,
		Reason: j.Reason, ResultSummary: clipText(j.ID+": "+detail, 500),
	})
}

// jobStartError is the answer when a job could not be stored.
func jobStartError(entry toolEntry, err error) *mcp.CallToolResult {
	var ie *jobs.InputError
	switch {
	case errors.Is(err, jobs.ErrDraining):
		return restartingResult(entry.tool.Name, entry.tool.Name+" did not run and no job was started. Call it again in a few seconds.")
	case errors.Is(err, jobs.ErrTooManyActive):
		res := mcp.NewToolResultError(err.Error() + ". Nothing ran. List them with jobs.list(status=\"active\").")
		res.StructuredContent = map[string]any{"status": "job_budget_exceeded", "executed": false, "tool": entry.tool.Name}
		return res
	case errors.Is(err, jobs.ErrIdempotencyKeyReused):
		return mcp.NewToolResultError("this Idempotency-Key was used for another tool or other arguments; nothing ran; use a new key")
	case errors.As(err, &ie):
		return mcp.NewToolResultErrorf("%s; nothing ran", ie.Error())
	}
	return mcp.NewToolResultErrorFromErr("could not start the job; nothing ran", err)
}

// jobOutcome turns a dispatch answer into the job's outcome.
func jobOutcome(res *mcp.CallToolResult, err error) jobs.Outcome {
	if res == nil && err == nil {
		err = errors.New("the tool returned no result")
	}
	env := encodeJobResult(res)
	switch {
	case err != nil:
		return jobs.Outcome{Status: jobs.StatusOutcomeUnknown, Envelope: env, IsError: true,
			Error: "the call may or may not have run: " + clipText(err.Error(), 500)}
	case metaTrue(res, codemode.OutcomeUnknownMarker):
		return jobs.Outcome{Status: jobs.StatusOutcomeUnknown, Envelope: env, IsError: true,
			Error: "the call may or may not have run: " + clipText(summariseResult(res), 500)}
	case res.IsError:
		return jobs.Outcome{Status: jobs.StatusFailed, Envelope: env, IsError: true, Error: clipText(summariseResult(res), 500)}
	}
	return jobs.Outcome{Status: jobs.StatusSucceeded, Envelope: env}
}

// encodeJobResult stores the whole tool result (text, images, structured
// content, metadata), with connect links taken out: the row is read back
// later and by the agent's owner.
func encodeJobResult(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	raw, err := json.Marshal(res)
	if err != nil {
		raw, _ = json.Marshal(mcp.NewToolResultError("the tool result could not be encoded: " + err.Error()))
	}
	return audit.RedactConnectLinks(string(raw))
}

// decodeJobResult is encodeJobResult's inverse.
func decodeJobResult(env string) (*mcp.CallToolResult, error) {
	if env == "" {
		return nil, nil
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal([]byte(env), &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func metaTrue(res *mcp.CallToolResult, key string) bool {
	if res == nil || res.Meta == nil {
		return false
	}
	v, ok := res.Meta.AdditionalFields[key].(bool)
	return ok && v
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// paramsHash is the sha256 of the canonical JSON of a call's arguments
// (keys sorted, numbers as float64), so an Idempotency-Key replay can
// tell the same call from another one. The arguments are never stored.
func paramsHash(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err == nil {
		var v any
		if err = json.Unmarshal(raw, &v); err == nil {
			raw, err = json.Marshal(v)
		}
	}
	if err != nil {
		raw = []byte(fmt.Sprintf("unencodable:%v", args))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// jobEnvelope is the answer to a call that became a job: the job id and
// how to get the result, as structured content and as text.
func jobEnvelope(job *jobs.Job) *mcp.CallToolResult {
	body := map[string]any{
		"job_id": job.ID,
		"status": job.Status,
		"tool":   job.Tool,
		"poll":   MetaJobsStatus,
		"wait":   MetaJobsWait,
		"result": MetaJobsResult,
		"cancel": MetaJobsCancel,
		"next_steps_for_agent": map[string]any{
			"wait_for_result": map[string]any{
				"tool": MetaJobsWait, "args": map[string]any{"job_id": job.ID, "timeout_seconds": jobWaitMaxSeconds, "_reason": "(your reason)"},
				"hint": "blocks up to 60 seconds and returns the result as soon as the job ends; call it again while status is queued or running",
			},
			"check_status": map[string]any{
				"tool": MetaJobsStatus, "args": map[string]any{"job_id": job.ID, "_reason": "(your reason)"},
				"hint": "does not block; carries the result once the job ended",
			},
			"cancel_if_no_longer_needed": map[string]any{
				"tool": MetaJobsCancel, "args": map[string]any{"job_id": job.ID, "_reason": "(your reason)"},
			},
		},
		"message": "The call runs as an async job. Do not call the tool again for the same work: wait for this job, or end your turn and read it later.",
	}
	if job.CallbackRef != "" {
		body["callback_ref"] = job.CallbackRef
		body["callback"] = "toolyard sends a job.completed or job.failed event to this receiver when the job ends"
	}
	text := fmt.Sprintf("%s runs as an async job.\njob_id: %s (status %s)\n"+
		"Get the result: jobs.wait(job_id=%q, timeout_seconds=60) blocks until it ends (up to 60 s); jobs.status(job_id=%q) does not block. "+
		"Do not call %s again for the same work.", job.Tool, job.ID, job.Status, job.ID, job.ID, job.Tool)
	res := mcp.NewToolResultText(text)
	res.StructuredContent = body
	res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.job": true, "toolyard.job_id": job.ID}}
	return res
}

// ---- visibility ---------------------------------------------------------

// jobVisible reports whether the ctx caller may read or cancel j: the
// agent that started it, or a dashboard caller who owns that agent (an
// admin: any). A mismatch answers exactly like an unknown id.
func (g *Gateway) jobVisible(ctx context.Context, j *jobs.Job) bool {
	if j == nil {
		return false
	}
	caller := agentIDFromContext(ctx)
	if j.AgentID == caller {
		return true
	}
	if uid, ok := strings.CutPrefix(caller, "dashboard:"); ok && uid != "" {
		return j.OwnerUserID == uid || g.scopeFor(ctx, caller).All
	}
	return false
}

// visibleJob returns job id when the ctx caller may see it.
func (g *Gateway) visibleJob(ctx context.Context, id string) (*jobs.Job, error) {
	j, err := g.jobs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !g.jobVisible(ctx, j) {
		return nil, jobs.ErrNotFound
	}
	return j, nil
}

// ---- snapshots ----------------------------------------------------------

func msTime(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

// jobSnapshot is the JSON shape of one job in the jobs.* answers. With
// withResult a final job's result rides along when it is small.
func (g *Gateway) jobSnapshot(ctx context.Context, j *jobs.Job, withResult bool) map[string]any {
	out := map[string]any{
		"job_id":      j.ID,
		"status":      j.Status,
		"final":       j.Final(),
		"tool":        j.Tool,
		"upstream":    j.Upstream,
		"agent_id":    j.AgentID,
		"reason":      j.Reason,
		"via":         j.Via,
		"created_at":  msTime(j.CreatedAt),
		"updated_at":  msTime(j.UpdatedAt),
		"age_seconds": int((time.Now().UnixMilli() - j.CreatedAt) / 1000),
	}
	if j.StartedAt > 0 {
		out["started_at"] = msTime(j.StartedAt)
	}
	if j.FinishedAt > 0 {
		out["finished_at"] = msTime(j.FinishedAt)
		if j.StartedAt > 0 {
			out["duration_ms"] = j.FinishedAt - j.StartedAt
		}
	}
	if j.Progress != nil {
		p := map[string]any{"progress": j.Progress.Progress, "at": msTime(j.Progress.At)}
		if j.Progress.Total != nil {
			p["total"] = *j.Progress.Total
		}
		if j.Progress.Message != "" {
			p["message"] = j.Progress.Message
		}
		out["progress"] = p
	}
	if j.Error != "" {
		out["error"] = j.Error
	}
	if j.CallbackRef != "" {
		out["callback_ref"] = j.CallbackRef
	}
	if j.CallbackError != "" {
		out["callback_error"] = j.CallbackError
	}
	if j.CancelRequested && !j.Final() {
		out["cancel_requested"] = true
	}
	switch j.Status {
	case jobs.StatusQueued, jobs.StatusRunning:
		out["how_to_proceed"] = "Still going. Block on it with jobs.wait (up to 60 s), or end your turn and check jobs.status later. Do not call the tool again."
	case jobs.StatusSucceeded:
		out["how_to_proceed"] = "Done. result is exactly what the tool returned."
	case jobs.StatusFailed:
		out["how_to_proceed"] = "The tool answered with an error (result) or the job never started (error). Read it before you call the tool again."
	case jobs.StatusCancelled:
		out["how_to_proceed"] = "Cancelled. If it was running, a write may already have taken effect: check the target before you call the tool again."
		out["may_have_run"] = j.StartedAt > 0
	case jobs.StatusOutcomeUnknown:
		out["how_to_proceed"] = "The outcome is unknown: the call may or may not have run. Inspect the target's own state. Do not repeat a write blindly."
		out["may_have_run"] = true
	}
	if j.Final() {
		out["result_available"] = j.HasResult()
		if j.HasResult() {
			out["result_bytes"] = j.ResultBytes
		}
		if withResult && j.HasResult() {
			if j.ResultBytes > jobSnapshotResultLimit {
				out["result_omitted"] = "the result is large; read it with jobs.result"
			} else if res, err := g.jobResult(ctx, j); err != nil {
				out["result_error"] = err.Error()
			} else if res != nil {
				out["result"] = map[string]any{
					"is_error":           res.IsError,
					"content":            res.Content,
					"structured_content": res.StructuredContent,
				}
			}
		}
	}
	return out
}

// jobResult reads and decodes a final job's tool result (nil when it kept
// none).
func (g *Gateway) jobResult(ctx context.Context, j *jobs.Job) (*mcp.CallToolResult, error) {
	env, err := g.jobs.Result(ctx, j)
	if err != nil {
		return nil, err
	}
	return decodeJobResult(env)
}

// JobResult is jobResult for the REST route.
func (g *Gateway) JobResult(ctx context.Context, j *jobs.Job) (*mcp.CallToolResult, error) {
	return g.jobResult(ctx, j)
}

// ---- tools --------------------------------------------------------------

// jobsTools returns the jobs.* entries, or none when async tools are off.
func (g *Gateway) jobsTools() []toolEntry {
	if g.jobs == nil {
		return nil
	}
	allow := policy.ActionAllow
	entry := func(name, desc string, props map[string]any, required []string, h directHandler) toolEntry {
		return toolEntry{
			tool: mcp.Tool{
				Name:        name,
				Description: descriptionBanner + desc,
				InputSchema: mcp.ToolInputSchema{Type: "object", Required: append([]string{ReasonField}, required...), Properties: addMetaProps(props)},
			},
			upstream: jobsUpstream, originalName: strings.TrimPrefix(name, "jobs."), reasonField: ReasonField,
			handle: h, forcedAction: &allow,
		}
	}
	jobID := map[string]any{"type": "string", "description": "The job_id a call returned (job_…)."}
	return []toolEntry{
		entry(MetaJobsStatus,
			"Status of one async job you started: queued, running, succeeded, failed, cancelled or outcome_unknown, its last progress, and its result once it ended (when small). Non-blocking. A tool the administrator marked async answers with a job_id instead of its result; read the result here, with jobs.wait or with jobs.result. Never call the tool again for the same work.",
			map[string]any{"job_id": jobID}, []string{"job_id"}, g.handleJobsStatus()),
		entry(MetaJobsResult,
			"The result of an async job that ended, exactly as the tool returned it (content, structured content, isError). While the job is still queued or running it answers with the job's status instead; use jobs.wait to block until it ends.",
			map[string]any{"job_id": jobID}, []string{"job_id"}, g.handleJobsResult()),
		entry(MetaJobsWait,
			"Block until async jobs end, up to timeout_seconds (at most 60), then return their status and results. Pass job_id for one job or job_ids (up to 32) for several; mode all (default) waits for every job, any for the first. Call it again while a job is still running. A job also keeps running if you stop waiting.",
			map[string]any{
				"job_id":          jobID,
				"job_ids":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 32, "description": "Several job ids to wait on."},
				"mode":            map[string]any{"type": "string", "enum": []string{"all", "any"}, "description": "all (default): return when every job ended. any: return when one ended."},
				"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": jobWaitMaxSeconds, "description": "How long to wait. Default and maximum 60."},
			}, nil, g.handleJobsWait()),
		entry(MetaJobsCancel,
			"Cancel one of your async jobs. A queued job is cancelled before it starts (nothing ran). A running job's upstream call is interrupted, but a write may already have taken effect: check the target before you call the tool again. Cancelling a job that already ended changes nothing.",
			map[string]any{"job_id": jobID}, []string{"job_id"}, g.handleJobsCancel()),
		entry(MetaJobsList,
			"List your async jobs, newest first: status filter (queued, running, succeeded, failed, cancelled, outcome_unknown, or active for queued and running) and limit (default 20, max 100). Results are not included; read them with jobs.status or jobs.result.",
			map[string]any{
				"status":   map[string]any{"type": "string", "enum": []string{"active", jobs.StatusQueued, jobs.StatusRunning, jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusCancelled, jobs.StatusOutcomeUnknown}},
				"limit":    map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				"agent_id": map[string]any{"type": "string", "description": "Owners and admins only (dashboard callers): one agent's jobs."},
			}, nil, g.handleJobsList()),
		entry(MetaJobsStart,
			"Run any tool you may call as an async job and get its job_id at once: the call goes through the same access, policy and approval rules as a direct call (a call that needs permission answers as it always does and starts no job). Use it for slow tools, or from code mode to start work and wait for it with jobs.wait. Called inside a job (a script that itself runs as a job), the call runs inline there and its result is the tool's own. callback_ref (optional) names a receiver for a job.completed or job.failed callback.",
			map[string]any{
				"tool":         map[string]any{"type": "string", "description": "Catalog name of the tool (as tools.search shows it)."},
				"arguments":    map[string]any{"type": "object", "description": "The tool's arguments. _reason on this call is used when they carry none."},
				"callback_ref": map[string]any{"type": "string", "description": "Optional opaque receiver reference for the job's callback."},
			}, []string{"tool"}, g.handleJobsStart()),
	}
}

func (g *Gateway) handleJobsStatus() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		id := strings.TrimSpace(stringArg(args, "job_id"))
		if id == "" {
			return mcp.NewToolResultError("job_id is required"), nil
		}
		j, err := g.visibleJob(ctx, id)
		if errors.Is(err, jobs.ErrNotFound) {
			return mcp.NewToolResultError("unknown job_id"), nil
		}
		if err != nil {
			return mcp.NewToolResultErrorFromErr("read job", err), nil
		}
		return jsonResultMap(g.jobSnapshot(ctx, j, true))
	}
}

func (g *Gateway) handleJobsResult() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		id := strings.TrimSpace(stringArg(args, "job_id"))
		if id == "" {
			return mcp.NewToolResultError("job_id is required"), nil
		}
		j, err := g.visibleJob(ctx, id)
		if errors.Is(err, jobs.ErrNotFound) {
			return mcp.NewToolResultError("unknown job_id"), nil
		}
		if err != nil {
			return mcp.NewToolResultErrorFromErr("read job", err), nil
		}
		return g.finalJobAnswer(ctx, j)
	}
}

// finalJobAnswer is jobs.result's answer: the tool's own result for a job
// that ended with one, else the snapshot.
func (g *Gateway) finalJobAnswer(ctx context.Context, j *jobs.Job) (*mcp.CallToolResult, error) {
	if !j.Final() || !j.HasResult() {
		snap := g.jobSnapshot(ctx, j, false)
		res, err := jsonResultMap(snap)
		if res != nil && j.Final() && j.Status != jobs.StatusSucceeded {
			res.IsError = true
		}
		return res, err
	}
	res, err := g.jobResult(ctx, j)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("read job result", err), nil
	}
	if res == nil {
		res = mcp.NewToolResultText("(the tool returned no content)")
	}
	if res.Meta == nil {
		res.Meta = &mcp.Meta{}
	}
	if res.Meta.AdditionalFields == nil {
		res.Meta.AdditionalFields = map[string]any{}
	}
	res.Meta.AdditionalFields["toolyard.job"] = map[string]any{"job_id": j.ID, "status": j.Status}
	return res, nil
}

func (g *Gateway) handleJobsWait() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		var ids []string
		if id := strings.TrimSpace(stringArg(args, "job_id")); id != "" {
			ids = append(ids, id)
		}
		if raw, ok := args["job_ids"].([]any); ok {
			for _, v := range raw {
				s, _ := v.(string)
				if s = strings.TrimSpace(s); s == "" {
					return mcp.NewToolResultError("job_ids contains an empty id"), nil
				}
				ids = append(ids, s)
			}
		}
		if len(ids) == 0 {
			return mcp.NewToolResultError("job_id or job_ids is required"), nil
		}
		if len(ids) > 32 {
			return mcp.NewToolResultError("at most 32 jobs per wait"), nil
		}
		mode := "all"
		if m := stringArg(args, "mode"); m != "" {
			if m != "all" && m != "any" {
				return mcp.NewToolResultError(`mode must be "all" or "any"`), nil
			}
			mode = m
		}
		timeout := jobWaitMaxSeconds
		if n, ok := asInt(args["timeout_seconds"]); ok && n > 0 {
			timeout = min(n, jobWaitMaxSeconds)
		}
		timeout = clampToDeadline(ctx, timeout)
		// Only the caller's own jobs are waited on; another agent's id
		// reads as unknown.
		var visible []string
		for _, id := range ids {
			if _, err := g.visibleJob(ctx, id); err == nil {
				visible = append(visible, id)
			}
		}
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-g.drainReleaseCh():
				cancel()
			case <-wctx.Done():
			}
		}()
		got := map[string]*jobs.Job{}
		timedOut := false
		if len(visible) > 0 {
			list, to, err := g.jobs.WaitFor(wctx, visible, mode == "any", time.Duration(timeout)*time.Second)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("wait for jobs", err), nil
			}
			timedOut = to
			for i, j := range list {
				if j != nil {
					got[visible[i]] = j
				}
			}
		}
		snaps := make([]map[string]any, 0, len(ids))
		allFinal, anyFinal := true, false
		for _, id := range ids {
			j, ok := got[id]
			if !ok {
				snaps = append(snaps, map[string]any{"job_id": id, "status": "unknown", "final": true})
				anyFinal = true
				continue
			}
			if j.Final() {
				anyFinal = true
			} else {
				allFinal = false
			}
			snaps = append(snaps, g.jobSnapshot(ctx, j, true))
		}
		if len(ids) == 1 && stringArg(args, "job_id") != "" && args["job_ids"] == nil {
			snap := snaps[0]
			if snap["status"] == "unknown" {
				return mcp.NewToolResultError("unknown job_id"), nil
			}
			snap["timed_out"] = timedOut && !allFinal
			return jsonResultMap(snap)
		}
		return jsonResultMap(map[string]any{
			"count": len(snaps), "mode": mode, "all_final": allFinal, "any_final": anyFinal,
			"timed_out": timedOut && !((mode == "all" && allFinal) || (mode == "any" && anyFinal)),
			"results":   snaps,
		})
	}
}

func (g *Gateway) handleJobsCancel() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		id := strings.TrimSpace(stringArg(args, "job_id"))
		if id == "" {
			return mcp.NewToolResultError("job_id is required"), nil
		}
		if _, err := g.visibleJob(ctx, id); err != nil {
			if errors.Is(err, jobs.ErrNotFound) {
				return mcp.NewToolResultError("unknown job_id"), nil
			}
			return mcp.NewToolResultErrorFromErr("read job", err), nil
		}
		j, cancelled, err := g.CancelJob(ctx, id)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("cancel job", err), nil
		}
		snap := g.jobSnapshot(ctx, j, false)
		snap["cancel_accepted"] = cancelled
		switch {
		case !cancelled:
			snap["message"] = "The job had already ended; nothing changed."
		case j.Status == jobs.StatusRunning:
			snap["message"] = "Cancel requested: the upstream call is being interrupted. A write may already have taken effect; check the target before you call the tool again. jobs.wait shows the final status."
		}
		return jsonResultMap(snap)
	}
}

// CancelJob cancels job id and records it in the audit log. The caller
// checked that the job is the ctx caller's.
func (g *Gateway) CancelJob(ctx context.Context, id string) (*jobs.Job, bool, error) {
	j, cancelled, err := g.jobs.Cancel(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if cancelled {
		_ = g.audit.Write(ctx, audit.Event{
			EventType: eventJobCancelled, AgentID: agentIDFromContext(ctx), UpstreamName: j.Upstream, ToolName: j.Tool,
			ResultSummary: j.ID + " (" + j.Status + ")",
		})
	}
	return j, cancelled, nil
}

func (g *Gateway) handleJobsList() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		limit := 20
		if n, ok := asInt(args["limit"]); ok && n > 0 {
			limit = min(n, 100)
		}
		f, err := g.jobFilter(ctx, stringArg(args, "agent_id"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		f.Status, f.Limit = strings.TrimSpace(stringArg(args, "status")), limit
		list, err := g.jobs.List(ctx, f)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("list jobs", err), nil
		}
		snaps := make([]map[string]any, 0, len(list))
		for _, j := range list {
			snaps = append(snaps, g.jobSnapshot(ctx, j, false))
		}
		return jsonResultMap(map[string]any{"count": len(snaps), "jobs": snaps})
	}
}

// jobFilter scopes jobs.list to the ctx caller: an agent lists its own
// jobs; a dashboard caller its agents' (an admin: everyone's), optionally
// one agent's.
func (g *Gateway) jobFilter(ctx context.Context, agent string) (jobs.Filter, error) {
	caller := agentIDFromContext(ctx)
	agent = strings.TrimSpace(agent)
	if uid, ok := strings.CutPrefix(caller, "dashboard:"); ok && uid != "" {
		f := jobs.Filter{AgentID: agent}
		if g.scopeFor(ctx, caller).All {
			f.All = true
		} else {
			f.OwnerUserID = uid
		}
		return f, nil
	}
	if caller == "" {
		return jobs.Filter{}, errors.New("only authenticated agents can list their jobs")
	}
	if agent != "" && agent != caller {
		return jobs.Filter{}, errors.New("agent_id is for owners and admins; an agent lists only its own jobs")
	}
	return jobs.Filter{AgentID: caller}, nil
}

func (g *Gateway) handleJobsStart() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		target := strings.TrimSpace(stringArg(args, "tool"))
		if target == "" {
			return mcp.NewToolResultError("tool is required"), nil
		}
		if strings.HasPrefix(target, jobsUpstream+".") {
			return mcp.NewToolResultErrorf("%s cannot run as a job", target), nil
		}
		if r, ok := actor.RaiserFrom(ctx); ok && r.Via == codemode.Via && isCodeModeTool(target) {
			return mcp.NewToolResultErrorf("%s cannot be called from code mode: call the tools you need directly in the script", target), nil
		}
		ref := strings.TrimSpace(stringArg(args, "callback_ref"))
		if err := jobs.CheckCallbackRef(ref); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		inner := map[string]any{}
		if a, ok := args["arguments"].(map[string]any); ok {
			for k, v := range a {
				inner[k] = v
			}
		}
		if _, ok := inner[ReasonField]; !ok {
			inner[ReasonField] = callReasonFromContext(ctx)
		}
		if ref != "" {
			inner[callbackRefArg] = ref
		}
		req := &asyncRequest{via: jobViaStart}
		return g.RouteCall(withAsyncRequest(ctx, req), MetaJobsStart, target, inner)
	}
}

// StartJob runs one call as a job for POST /v1/jobs: the full pipeline
// (access, drain, policy, approval or grant, identity forwarding, audit)
// under via "rest". job is nil when the call was answered without
// starting one (unknown tool, refused, held for approval); res is then
// that answer. err is the job service's refusal (an Idempotency-Key used
// for another call, too many active jobs, bad input), when that was why.
func (g *Gateway) StartJob(ctx context.Context, agentID, tool string, args map[string]any, reason, callbackRef, idempotencyKey string) (*jobs.Job, *mcp.CallToolResult, error) {
	if g.jobs == nil {
		return nil, nil, errors.New("async jobs are not enabled on this server")
	}
	ctx = WithAgentID(ctx, agentID)
	call := make(map[string]any, len(args)+2)
	for k, v := range args {
		call[k] = v
	}
	call[ReasonField] = reason
	if callbackRef != "" {
		call[callbackRefArg] = callbackRef
	} else {
		delete(call, callbackRefArg)
	}
	req := &asyncRequest{via: jobViaREST, idempotencyKey: idempotencyKey}
	res, err := g.RouteCall(withAsyncRequest(ctx, req), jobViaREST, tool, call)
	if err != nil {
		return nil, res, err
	}
	job, jerr := req.result()
	return job, res, jerr
}

// VisibleJob returns job id when the ctx caller may see it, for the REST
// route.
func (g *Gateway) VisibleJob(ctx context.Context, id string) (*jobs.Job, error) {
	if g.jobs == nil {
		return nil, jobs.ErrNotFound
	}
	return g.visibleJob(ctx, id)
}

// JobFilter is jobFilter for the REST route.
func (g *Gateway) JobFilter(ctx context.Context, agent string) (jobs.Filter, error) {
	return g.jobFilter(ctx, agent)
}

// ---- servers.list -------------------------------------------------------

// annotateAsync fills each server's async setting for servers.list.
func (g *Gateway) annotateAsync(rows []ServerInfo) {
	for i := range rows {
		rows[i].Async = g.asyncSettingOf(rows[i].Name)
	}
}

// ---- upstream progress --------------------------------------------------

// progressSinkKey carries a job's progress recorder to its dispatch.
type progressSinkKey struct{}

func withProgressSink(ctx context.Context, fn jobs.ProgressFunc) context.Context {
	return context.WithValue(ctx, progressSinkKey{}, fn)
}

// progressRoutes maps the progress token of each in-flight upstream call
// made by a job to that job's recorder.
var progressRoutes sync.Map // string -> jobs.ProgressFunc

// upstreamProgressMeta returns the _meta for an upstream tools/call made
// inside a job: a fresh progress token routed to the job, and the func
// that forgets it once the call ends. nil outside a job.
func upstreamProgressMeta(ctx context.Context) (*mcp.Meta, func()) {
	fn, ok := ctx.Value(progressSinkKey{}).(jobs.ProgressFunc)
	if !ok || fn == nil {
		return nil, func() {}
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	token := "tyjob-" + hex.EncodeToString(b[:])
	progressRoutes.Store(token, fn)
	return &mcp.Meta{ProgressToken: token}, func() { progressRoutes.Delete(token) }
}

// routeUpstreamProgress hands an upstream's notifications/progress to the
// job whose call carries its token. Installed on every upstream client.
func routeUpstreamProgress(n mcp.JSONRPCNotification) {
	if n.Method != string(mcp.MethodNotificationProgress) {
		return
	}
	p := n.Params.AdditionalFields
	if p == nil {
		return
	}
	token := fmt.Sprint(p["progressToken"])
	v, ok := progressRoutes.Load(token)
	if !ok {
		return
	}
	fn, _ := v.(jobs.ProgressFunc)
	if fn == nil {
		return
	}
	progress, ok := numericArgument(p["progress"])
	if !ok {
		return
	}
	var total *float64
	if t, ok := numericArgument(p["total"]); ok && t > 0 {
		total = &t
	}
	msg, _ := p["message"].(string)
	fn(progress, total, msg)
}
