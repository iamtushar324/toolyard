package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/jobs"
)

// MCP Tasks (protocol 2025-11-25) for async tools. mcp-go keeps tasks in
// one process's memory; toolyard answers them from the jobs table
// instead, so any replica answers any task and a task survives a restart.
// MCPTasks sits in front of the MCP transport, after authentication:
//
//   - tools/call carrying params.task runs the call as a job, through the
//     same pipeline as any call, and answers CreateTaskResult. A call
//     refused before it could run (policy, permission, approval) still
//     gets a task: a final one whose result is that answer.
//   - tasks/get, tasks/result, tasks/cancel and tasks/list read and cancel
//     the caller's jobs. A 2026-07-28 request gets tasks/get and
//     tasks/cancel (tasks/get carries the result once the task ended);
//     tasks/result and tasks/list are gone in that version, as in mcp-go.
//
// Everything else goes to the transport. MCPTasks applies the transport's
// own checks first: DNS rebinding protection (a loopback listener with a
// Host that is not loopback is refused, unless that protection is off, as
// with -public-url), and for a 2026-07-28 request the protocol version and
// Mcp-Method / Mcp-Name headers, which must match the body. tools/list
// marks every tool with execution.taskSupport "optional" (markTaskSupport):
// any tool can run as a job. Session ids are not checked on these methods:
// the bearer token is the credential, and a job is scoped to its agent.

// taskPollIntervalMs is the polling hint in every task.
const taskPollIntervalMs = 2000

// tasksBridgeKey marks a request that came through MCPTasks.
type tasksBridgeKey struct{}

func tasksBridged(ctx context.Context) bool {
	v, _ := ctx.Value(tasksBridgeKey{}).(bool)
	return v
}

// taskRequest is one intercepted JSON-RPC request.
type taskRequest struct {
	id     json.RawMessage
	method string
	params json.RawMessage
	// modern is a 2026-07-28 request: errors carry an HTTP status and
	// results a resultType.
	modern bool
}

// MCPTasks wraps the /mcp handler. contextFunc is the transport's
// HTTPContextFunc: it puts the authenticated caller on the context.
// localhostProtection mirrors the transport's DNS rebinding protection
// (off with -public-url, where /mcp always needs a token).
func (g *Gateway) MCPTasks(next http.Handler, contextFunc func(context.Context, *http.Request) context.Context, localhostProtection bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.jobs == nil {
			next.ServeHTTP(w, r)
			return
		}
		if localhostProtection && rebindingRequest(r) {
			http.Error(w, fmt.Sprintf("Forbidden: invalid Host header %q", r.Host), http.StatusForbidden)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), tasksBridgeKey{}, true))
		if r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		raw, ok := rawMCPBody(r.Context())
		if !ok {
			var err error
			if raw, err = io.ReadAll(r.Body); err != nil {
				http.Error(w, "invalid MCP body", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(trimmed, &msg) != nil ||
			len(msg.ID) == 0 || string(msg.ID) == "null" || msg.JSONRPC != mcp.JSONRPC_VERSION {
			next.ServeHTTP(w, r)
			return
		}
		switch msg.Method {
		case string(mcp.MethodToolsCall):
			var p struct {
				Task *mcp.TaskParams `json:"task"`
			}
			if json.Unmarshal(msg.Params, &p) != nil || p.Task == nil {
				next.ServeHTTP(w, r)
				return
			}
		case string(mcp.MethodTasksGet), string(mcp.MethodTasksResult), string(mcp.MethodTasksCancel), string(mcp.MethodTasksList):
		default:
			next.ServeHTTP(w, r)
			return
		}
		req := taskRequest{id: msg.ID, method: msg.Method, params: msg.Params}
		var meta json.RawMessage
		var p struct {
			Meta json.RawMessage `json:"_meta"`
		}
		if json.Unmarshal(msg.Params, &p) == nil {
			meta = p.Meta
		}
		if !checkModernRequest(w, r, &req, meta) {
			return
		}
		ctx := r.Context()
		if contextFunc != nil {
			ctx = contextFunc(ctx, r)
		}
		switch msg.Method {
		case string(mcp.MethodToolsCall):
			g.taskToolCall(withRequestMeta(ctx, meta), w, req)
		case string(mcp.MethodTasksGet):
			g.taskGet(ctx, w, req)
		case string(mcp.MethodTasksResult):
			g.taskResult(ctx, w, req)
		case string(mcp.MethodTasksCancel):
			g.taskCancel(ctx, w, req)
		case string(mcp.MethodTasksList):
			g.taskList(ctx, w, req)
		}
	})
}

// rebindingRequest is mcp-go's DNS rebinding test: a request that reached
// a loopback listener with a Host that is not loopback.
func rebindingRequest(r *http.Request) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || local == nil {
		return false
	}
	return loopbackHost(local.String()) && !loopbackHost(r.Host)
}

func loopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = strings.Trim(addr, "[]")
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// checkModernRequest applies the 2026-07-28 transport checks mcp-go makes
// (the version in the header and in _meta must agree; Mcp-Method and
// Mcp-Name must match the body) and marks req modern. It writes the error
// answer and returns false when the request is refused.
func checkModernRequest(w http.ResponseWriter, r *http.Request, req *taskRequest, meta json.RawMessage) bool {
	headerVersion := r.Header.Get(mcp.HeaderProtocolVersion)
	var metaVersion string
	if len(meta) > 0 {
		var m map[string]json.RawMessage
		if json.Unmarshal(meta, &m) == nil {
			_ = json.Unmarshal(m[mcp.MetaKeyProtocolVersion], &metaVersion)
		}
	}
	if !mcp.IsModernProtocol(metaVersion) && !mcp.IsModernProtocol(headerVersion) {
		return true
	}
	req.modern = true
	switch {
	case headerVersion == "":
		writeRPCError(w, *req, mcp.HEADER_MISMATCH, mcp.HeaderProtocolVersion+" header is required for requests carrying "+mcp.MetaKeyProtocolVersion)
		return false
	case metaVersion == "":
		writeRPCError(w, *req, mcp.INVALID_PARAMS, "missing or invalid _meta field "+mcp.MetaKeyProtocolVersion)
		return false
	case headerVersion != metaVersion:
		writeRPCError(w, *req, mcp.HEADER_MISMATCH, mcp.HeaderProtocolVersion+" header "+headerVersion+" does not match request "+
			mcp.MetaKeyProtocolVersion+" "+metaVersion)
		return false
	}
	if err := mcp.ValidateStandardHeaders(r.Header.Get, metaVersion, mcp.MCPMethod(req.method), req.params); err != nil {
		code := mcp.INVALID_PARAMS
		var mismatch mcp.HeaderMismatchError
		if errors.As(err, &mismatch) {
			code = mcp.HEADER_MISMATCH
		}
		writeRPCError(w, *req, code, err.Error())
		return false
	}
	return true
}

// taskToolCall runs a task-augmented tools/call as a job.
func (g *Gateway) taskToolCall(ctx context.Context, w http.ResponseWriter, req taskRequest) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	dec := json.NewDecoder(bytes.NewReader(req.params))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		writeRPCError(w, req, mcp.INVALID_PARAMS, "invalid tools/call params: "+err.Error())
		return
	}
	// A published name (tool_names.go) runs its canonical tool, as on a
	// plain tools/call; an unknown name is answered with the name sent.
	name := g.resolveToolName(p.Name)
	args := g.exactArguments(ctx, name, p.Arguments)
	if args == nil {
		args = map[string]any{}
	}
	g.mu.RLock()
	entry, known := g.tools[name]
	g.mu.RUnlock()
	notFound := fmt.Sprintf("tool '%s' not found", p.Name)
	if !known {
		writeRPCError(w, req, mcp.INVALID_PARAMS, notFound)
		return
	}
	if raw, has := args[callbackRefArg]; has && !ownCallbackRef(entry) {
		ref, isStr := raw.(string)
		if !isStr || jobs.CheckCallbackRef(strings.TrimSpace(ref)) != nil {
			writeRPCError(w, req, mcp.INVALID_PARAMS, callbackRefArg+" must be a string of at most 512 printable ASCII characters")
			return
		}
	}
	agentID := agentIDFromContext(ctx)
	if !g.allowsEntry(ctx, agentID, entry) {
		// Routed for its audit row and metric; the answer matches an
		// unknown tool, as for a plain tools/call.
		_, _ = g.routeEntry(ctx, entry, viaDirect, args)
		writeRPCError(w, req, mcp.INVALID_PARAMS, notFound)
		return
	}
	areq := &asyncRequest{via: jobViaTask}
	// The same panic guard mcp-go's middleware gives a plain tools/call.
	res, err := recoverToolPanics(g.handlerFor(name))(withAsyncRequest(ctx, areq), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	job, jerr := areq.result()
	if job == nil {
		// Answered without a job (refused, held, or the job could not be
		// stored): keep that answer as a final task. An unknown outcome
		// stays one (a resumed approval, an inline jobs.* call or a held
		// call may have run), and so does a handler error or a recovered
		// panic: the task says it may have run rather than that it failed.
		out := jobOutcome(res, err)
		if res == nil {
			msg := "the call returned no result"
			if err != nil {
				msg = err.Error()
			} else if jerr != nil {
				msg = jerr.Error()
			}
			res = mcp.NewToolResultError(msg)
		}
		reason, _ := args[entry.reasonField].(string)
		job, err = g.jobs.Record(ctx, jobs.StartRequest{
			AgentID: agentID, OwnerUserID: g.ownerOf(ctx, agentID), Tool: entry.tool.Name, Upstream: entry.upstream,
			ParamsHash: paramsHash(args), Reason: reason, Via: jobViaTask, CallbackRef: callbackRefFrom(withCallbackRefFor(ctx, entry, args)),
		}, out)
		var ie *jobs.InputError
		if errors.As(err, &ie) {
			writeRPCError(w, req, mcp.INVALID_PARAMS, ie.Error())
			return
		}
		if errors.Is(err, jobs.ErrTooManyRecorded) {
			// Too many stored answers, so no task. A refusal that proves
			// nothing ran is an error the client can retry; an answer that
			// may have had an effect is sent as the plain tool result, so
			// the client still learns what happened.
			if out.NothingRan {
				writeRPCError(w, req, mcp.INTERNAL_ERROR, "no task was stored ("+err.Error()+"); the call's answer: "+clipText(summariseResult(res), 2000))
				return
			}
			plain := *res
			if req.modern {
				plain.ResultType = mcp.ResultTypeComplete
			}
			writeRPCResult(w, req.id, &plain)
			return
		}
		if err != nil {
			writeRPCError(w, req, mcp.INTERNAL_ERROR, "could not store the task: "+err.Error())
			return
		}
	}
	result := mcp.CreateTaskResult{Task: g.taskOf(job)}
	if req.modern {
		result.ResultType = mcp.ResultTypeComplete
	}
	writeRPCResult(w, req.id, result)
}

// taskIDParam reads params.taskId.
func taskIDParam(params json.RawMessage) string {
	var p struct {
		TaskID string `json:"taskId"`
	}
	_ = json.Unmarshal(params, &p)
	return strings.TrimSpace(p.TaskID)
}

func (g *Gateway) taskJob(ctx context.Context, w http.ResponseWriter, req taskRequest) (*jobs.Job, bool) {
	taskID := taskIDParam(req.params)
	if taskID == "" {
		writeRPCError(w, req, mcp.INVALID_PARAMS, "taskId is required")
		return nil, false
	}
	j, err := g.visibleJob(ctx, taskID)
	if errors.Is(err, jobs.ErrNotFound) {
		writeRPCError(w, req, mcp.INVALID_PARAMS, "task not found: "+taskID)
		return nil, false
	}
	if err != nil {
		writeRPCError(w, req, mcp.INTERNAL_ERROR, "read task: "+err.Error())
		return nil, false
	}
	return j, true
}

func (g *Gateway) taskGet(ctx context.Context, w http.ResponseWriter, req taskRequest) {
	j, ok := g.taskJob(ctx, w, req)
	if !ok {
		return
	}
	result := mcp.NewGetTaskResult(g.taskOf(j))
	if !req.modern {
		writeRPCResult(w, req.id, result)
		return
	}
	// 2026-07-28 has no tasks/result: an ended task carries its result.
	result.ResultType = mcp.ResultTypeComplete
	body := modernTaskGet{GetTaskResult: result}
	if j.Final() {
		body.TaskResult = g.taskToolResult(ctx, j)
	}
	writeRPCResult(w, req.id, body)
}

// modernTaskGet is a 2026-07-28 tasks/get answer: the task, and the
// result of an ended one, encoded once.
type modernTaskGet struct {
	mcp.GetTaskResult
	TaskResult *mcp.TaskResultResult `json:"result,omitempty"`
}

// taskToolResult is an ended job's tool result as tasks/result answers it.
func (g *Gateway) taskToolResult(ctx context.Context, j *jobs.Job) *mcp.TaskResultResult {
	out := &mcp.TaskResultResult{Result: mcp.Result{Meta: mcp.WithRelatedTask(j.ID)}}
	res, err := g.jobResult(ctx, j)
	if err != nil {
		res = mcp.NewToolResultError("the task's result could not be read: " + err.Error())
	}
	if res == nil {
		text := j.Error
		if text == "" {
			text = "(the tool returned no content)"
		}
		res = mcp.NewToolResultText(text)
		res.IsError = j.Status != jobs.StatusSucceeded
	}
	out.Content = res.Content
	out.StructuredContent = res.StructuredContent
	out.IsError = res.IsError
	if res.Meta != nil {
		for k, v := range res.Meta.AdditionalFields {
			if _, taken := out.Meta.AdditionalFields[k]; !taken {
				out.Meta.AdditionalFields[k] = v
			}
		}
	}
	return out
}

// taskResult waits for the task to end, then answers the tool's result.
func (g *Gateway) taskResult(ctx context.Context, w http.ResponseWriter, req taskRequest) {
	if req.modern {
		writeRPCError(w, req, mcp.METHOD_NOT_FOUND, `"tasks/result" was removed in protocol version 2026-07-28: tasks/get carries the result`)
		return
	}
	j, ok := g.taskJob(ctx, w, req)
	if !ok {
		return
	}
	if !j.Final() {
		done, err := g.jobs.BeginWait(agentIDFromContext(ctx))
		if err != nil {
			writeRPCError(w, req, mcp.INTERNAL_ERROR, err.Error()+": tasks/get answers at once; call tasks/result again later")
			return
		}
		defer done()
	}
	for !j.Final() {
		list, timedOut, err := g.jobs.WaitFor(ctx, []string{j.ID}, false, jobs.MaxWait)
		if err != nil {
			if ctx.Err() != nil {
				return // the client went away
			}
			writeRPCError(w, req, mcp.INTERNAL_ERROR, "could not read the task state ("+err.Error()+"); the task is not affected: call tasks/result again in a few seconds")
			return
		}
		if len(list) != 1 || list[0] == nil {
			// Gone (purged) while waiting.
			writeRPCError(w, req, mcp.INVALID_PARAMS, "task not found: "+j.ID)
			return
		}
		j = list[0]
		if !timedOut || j.Final() {
			continue
		}
		select {
		case <-g.jobs.Released():
			writeRPCError(w, req, mcp.INTERNAL_ERROR, "toolyard is restarting; call tasks/result again in a few seconds (the task keeps running)")
			return
		case <-ctx.Done():
			return
		default:
		}
	}
	writeRPCResult(w, req.id, g.taskToolResult(ctx, j))
}

func (g *Gateway) taskCancel(ctx context.Context, w http.ResponseWriter, req taskRequest) {
	j, ok := g.taskJob(ctx, w, req)
	if !ok {
		return
	}
	if j.Final() {
		writeRPCError(w, req, mcp.INVALID_PARAMS, "task "+j.ID+" is already in a terminal status ("+string(taskStatus(j.Status))+")")
		return
	}
	j, _, err := g.CancelJob(ctx, j.ID)
	if err != nil {
		writeRPCError(w, req, mcp.INTERNAL_ERROR, "cancel task: "+err.Error())
		return
	}
	// A running call takes a moment to stop: report the final status when
	// it comes quickly.
	if !j.Final() {
		if list, _, err := g.jobs.WaitFor(ctx, []string{j.ID}, false, 2*time.Second); err == nil && len(list) == 1 && list[0] != nil {
			j = list[0]
		}
	}
	result := mcp.CancelTaskResult{Task: g.taskOf(j)}
	if req.modern {
		result.ResultType = mcp.ResultTypeComplete
	}
	writeRPCResult(w, req.id, result)
}

// taskList lists the caller's jobs as tasks, newest first, 50 a page. The
// cursor is "<created_at>:<job id>" of the last task of the page before.
func (g *Gateway) taskList(ctx context.Context, w http.ResponseWriter, req taskRequest) {
	if req.modern {
		writeRPCError(w, req, mcp.METHOD_NOT_FOUND, `"tasks/list" was removed in protocol version 2026-07-28`)
		return
	}
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(req.params, &p)
	out := mcp.ListTasksResult{Tasks: []mcp.Task{}}
	f, err := g.jobFilter(ctx, "")
	if err != nil {
		writeRPCResult(w, req.id, out)
		return
	}
	const page = 50
	f.Limit = page
	if p.Cursor != "" {
		ts, jid, ok := strings.Cut(p.Cursor, ":")
		n, perr := strconv.ParseInt(ts, 10, 64)
		if !ok || perr != nil || jid == "" {
			writeRPCError(w, req, mcp.INVALID_PARAMS, "invalid cursor")
			return
		}
		f.BeforeCreated, f.BeforeID = n, jid
	}
	list, err := g.jobs.List(ctx, f)
	if err != nil {
		writeRPCError(w, req, mcp.INTERNAL_ERROR, "list tasks: "+err.Error())
		return
	}
	for _, j := range list {
		out.Tasks = append(out.Tasks, g.taskOf(j))
	}
	if len(list) == page {
		last := list[len(list)-1]
		out.NextCursor = mcp.Cursor(strconv.FormatInt(last.CreatedAt, 10) + ":" + last.ID)
	}
	writeRPCResult(w, req.id, out)
}

// taskStatus maps a job status to an MCP task status.
func taskStatus(status string) mcp.TaskStatus {
	switch status {
	case jobs.StatusSucceeded:
		return mcp.TaskStatusCompleted
	case jobs.StatusFailed, jobs.StatusOutcomeUnknown:
		return mcp.TaskStatusFailed
	case jobs.StatusCancelled:
		return mcp.TaskStatusCancelled
	}
	return mcp.TaskStatusWorking
}

// taskOf renders a job as an MCP task. The task id is the job id, so
// jobs.status and the REST route read the same work.
func (g *Gateway) taskOf(j *jobs.Job) mcp.Task {
	ttl := g.jobs.Retention().Milliseconds()
	if j.ExpiresAt > 0 {
		// A recorded answer is kept a short while from its creation.
		ttl = max(j.ExpiresAt-j.CreatedAt, 0)
	}
	poll := int64(taskPollIntervalMs)
	t := mcp.Task{
		TaskId: j.ID, Status: taskStatus(j.Status), CreatedAt: msTime(j.CreatedAt), LastUpdatedAt: msTime(j.UpdatedAt),
		TTL: &ttl, PollInterval: &poll,
	}
	switch {
	case j.Status == jobs.StatusOutcomeUnknown:
		t.StatusMessage = "outcome_unknown: " + j.Error
	case j.Error != "":
		t.StatusMessage = j.Error
	case j.Progress != nil && j.Progress.Message != "":
		t.StatusMessage = j.Progress.Message
	case j.Status == jobs.StatusQueued:
		t.StatusMessage = "queued"
	}
	return t
}

// markTaskSupport is the tools/list hook that marks every tool as
// task-capable for clients on MCPTasks: any tool can run as a job. The
// jobs.* tools themselves are left unmarked.
func (g *Gateway) markTaskSupport(ctx context.Context, _ any, _ *mcp.ListToolsRequest, res *mcp.ListToolsResult) {
	if res == nil || !tasksBridged(ctx) {
		return
	}
	for i := range res.Tools {
		if strings.HasPrefix(res.Tools[i].Name, jobsUpstream+".") {
			continue
		}
		res.Tools[i].Execution = &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}
	}
}

// writeRPCResult writes a JSON-RPC success answer.
func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	body, err := json.Marshal(map[string]any{"jsonrpc": mcp.JSONRPC_VERSION, "id": id, "result": result})
	if err != nil {
		writeRPCError(w, taskRequest{id: id}, mcp.INTERNAL_ERROR, "encode result: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeRPCError writes a JSON-RPC error answer, with the HTTP status
// 2026-07-28 prescribes for its code on a modern request (200 before).
func writeRPCError(w http.ResponseWriter, req taskRequest, code int, message string) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": mcp.JSONRPC_VERSION, "id": req.id,
		"error": map[string]any{"code": code, "message": message},
	})
	status := http.StatusOK
	if req.modern {
		switch code {
		case mcp.METHOD_NOT_FOUND:
			status = http.StatusNotFound
		case mcp.HEADER_MISMATCH, mcp.INVALID_REQUEST, mcp.INVALID_PARAMS, mcp.PARSE_ERROR:
			status = http.StatusBadRequest
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
