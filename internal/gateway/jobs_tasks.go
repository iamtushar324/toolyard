package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
//     the caller's jobs.
//
// Everything else goes to the transport. tools/list marks the tools the
// administrator made async with execution.taskSupport "optional"
// (markTaskSupport). Session ids are not checked on these methods: the
// bearer token is the credential, and a job is scoped to its agent.

// taskPollIntervalMs is the polling hint in every task.
const taskPollIntervalMs = 2000

// tasksBridgeKey marks a request that came through MCPTasks.
type tasksBridgeKey struct{}

func tasksBridged(ctx context.Context) bool {
	v, _ := ctx.Value(tasksBridgeKey{}).(bool)
	return v
}

// MCPTasks wraps the /mcp handler. contextFunc is the transport's
// HTTPContextFunc: it puts the authenticated caller on the context.
func (g *Gateway) MCPTasks(next http.Handler, contextFunc func(context.Context, *http.Request) context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.jobs == nil {
			next.ServeHTTP(w, r)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), tasksBridgeKey{}, true))
		if r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid MCP body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
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
		ctx := r.Context()
		if contextFunc != nil {
			ctx = contextFunc(ctx, r)
		}
		switch msg.Method {
		case string(mcp.MethodToolsCall):
			var p struct {
				Name string          `json:"name"`
				Task *mcp.TaskParams `json:"task"`
				Meta json.RawMessage `json:"_meta"`
			}
			if json.Unmarshal(msg.Params, &p) != nil || p.Task == nil {
				next.ServeHTTP(w, r)
				return
			}
			g.taskToolCall(withRequestMeta(ctx, p.Meta), w, msg.ID, msg.Params, p.Name)
		case string(mcp.MethodTasksGet):
			g.taskGet(ctx, w, msg.ID, msg.Params)
		case string(mcp.MethodTasksResult):
			g.taskResult(ctx, w, msg.ID, msg.Params)
		case string(mcp.MethodTasksCancel):
			g.taskCancel(ctx, w, msg.ID, msg.Params)
		case string(mcp.MethodTasksList):
			g.taskList(ctx, w, msg.ID, msg.Params)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// taskToolCall runs a task-augmented tools/call as a job.
func (g *Gateway) taskToolCall(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage, name string) {
	var p struct {
		Arguments map[string]any `json:"arguments"`
	}
	dec := json.NewDecoder(bytes.NewReader(params))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		writeRPCError(w, id, mcp.INVALID_PARAMS, "invalid tools/call params: "+err.Error())
		return
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	g.mu.RLock()
	entry, known := g.tools[name]
	g.mu.RUnlock()
	notFound := fmt.Sprintf("tool '%s' not found", name)
	if !known {
		writeRPCError(w, id, mcp.INVALID_PARAMS, notFound)
		return
	}
	agentID := agentIDFromContext(ctx)
	if !g.allowsEntry(ctx, agentID, entry) {
		// Routed for its audit row and metric; the answer matches an
		// unknown tool, as for a plain tools/call.
		_, _ = g.routeEntry(ctx, entry, viaDirect, p.Arguments)
		writeRPCError(w, id, mcp.INVALID_PARAMS, notFound)
		return
	}
	req := &asyncRequest{via: jobViaTask}
	res, err := g.handlerFor(name)(withAsyncRequest(ctx, req), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: p.Arguments}})
	job, jerr := req.result()
	if job == nil {
		// Answered without a job (refused, held, or the job could not be
		// stored): keep that answer as a final task.
		if res == nil {
			msg := "the call returned no result"
			if err != nil {
				msg = err.Error()
			} else if jerr != nil {
				msg = jerr.Error()
			}
			res = mcp.NewToolResultError(msg)
		}
		reason, _ := p.Arguments[entry.reasonField].(string)
		out := jobOutcome(res, nil)
		if out.Status == jobs.StatusOutcomeUnknown {
			out.Status = jobs.StatusFailed
		}
		job, err = g.jobs.Record(ctx, jobs.StartRequest{
			AgentID: agentID, OwnerUserID: g.ownerOf(ctx, agentID), Tool: entry.tool.Name, Upstream: entry.upstream,
			ParamsHash: paramsHash(p.Arguments), Reason: reason, Via: jobViaTask, CallbackRef: callbackRefFrom(withCallbackRef(ctx, p.Arguments)),
		}, out)
		if err != nil {
			writeRPCError(w, id, mcp.INTERNAL_ERROR, "could not store the task: "+err.Error())
			return
		}
	}
	writeRPCResult(w, id, mcp.CreateTaskResult{Task: g.taskOf(job)})
}

// taskIDParam reads params.taskId.
func taskIDParam(params json.RawMessage) string {
	var p struct {
		TaskID string `json:"taskId"`
	}
	_ = json.Unmarshal(params, &p)
	return strings.TrimSpace(p.TaskID)
}

func (g *Gateway) taskJob(ctx context.Context, w http.ResponseWriter, id, params json.RawMessage) (*jobs.Job, bool) {
	taskID := taskIDParam(params)
	if taskID == "" {
		writeRPCError(w, id, mcp.INVALID_PARAMS, "taskId is required")
		return nil, false
	}
	j, err := g.visibleJob(ctx, taskID)
	if errors.Is(err, jobs.ErrNotFound) {
		writeRPCError(w, id, mcp.INVALID_PARAMS, "task not found: "+taskID)
		return nil, false
	}
	if err != nil {
		writeRPCError(w, id, mcp.INTERNAL_ERROR, "read task: "+err.Error())
		return nil, false
	}
	return j, true
}

func (g *Gateway) taskGet(ctx context.Context, w http.ResponseWriter, id, params json.RawMessage) {
	j, ok := g.taskJob(ctx, w, id, params)
	if !ok {
		return
	}
	writeRPCResult(w, id, mcp.NewGetTaskResult(g.taskOf(j)))
}

// taskResult waits for the task to end, then answers the tool's result.
func (g *Gateway) taskResult(ctx context.Context, w http.ResponseWriter, id, params json.RawMessage) {
	j, ok := g.taskJob(ctx, w, id, params)
	if !ok {
		return
	}
	for !j.Final() {
		list, timedOut, err := g.jobs.WaitFor(ctx, []string{j.ID}, false, jobs.MaxWait)
		if err != nil {
			writeRPCError(w, id, mcp.INTERNAL_ERROR, "wait for task: "+err.Error())
			return
		}
		if len(list) == 1 && list[0] != nil {
			j = list[0]
		}
		if !timedOut || j.Final() {
			continue
		}
		select {
		case <-g.jobs.Released():
			writeRPCError(w, id, mcp.INTERNAL_ERROR, "toolyard is restarting; call tasks/result again in a few seconds (the task keeps running)")
			return
		case <-ctx.Done():
			return
		default:
		}
	}
	out := mcp.TaskResultResult{Result: mcp.Result{Meta: mcp.WithRelatedTask(j.ID)}}
	res, err := g.jobResult(ctx, j)
	if err != nil {
		writeRPCError(w, id, mcp.INTERNAL_ERROR, "read task result: "+err.Error())
		return
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
	writeRPCResult(w, id, out)
}

func (g *Gateway) taskCancel(ctx context.Context, w http.ResponseWriter, id, params json.RawMessage) {
	j, ok := g.taskJob(ctx, w, id, params)
	if !ok {
		return
	}
	if j.Final() {
		writeRPCError(w, id, mcp.INVALID_PARAMS, "task "+j.ID+" is already in a terminal status ("+string(taskStatus(j.Status))+")")
		return
	}
	j, _, err := g.CancelJob(ctx, j.ID)
	if err != nil {
		writeRPCError(w, id, mcp.INTERNAL_ERROR, "cancel task: "+err.Error())
		return
	}
	// A running call takes a moment to stop: report the final status when
	// it comes quickly.
	if !j.Final() {
		if list, _, err := g.jobs.WaitFor(ctx, []string{j.ID}, false, 2*time.Second); err == nil && len(list) == 1 && list[0] != nil {
			j = list[0]
		}
	}
	writeRPCResult(w, id, mcp.CancelTaskResult{Task: g.taskOf(j)})
}

// taskList lists the caller's jobs as tasks, newest first, 50 a page. The
// cursor is "<created_at>:<job id>" of the last task of the page before.
func (g *Gateway) taskList(ctx context.Context, w http.ResponseWriter, id, params json.RawMessage) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(params, &p)
	out := mcp.ListTasksResult{Tasks: []mcp.Task{}}
	f, err := g.jobFilter(ctx, "")
	if err != nil {
		writeRPCResult(w, id, out)
		return
	}
	const page = 50
	f.Limit = page
	if p.Cursor != "" {
		ts, jid, ok := strings.Cut(p.Cursor, ":")
		n, perr := strconv.ParseInt(ts, 10, 64)
		if !ok || perr != nil || jid == "" {
			writeRPCError(w, id, mcp.INVALID_PARAMS, "invalid cursor")
			return
		}
		f.BeforeCreated, f.BeforeID = n, jid
	}
	list, err := g.jobs.List(ctx, f)
	if err != nil {
		writeRPCError(w, id, mcp.INTERNAL_ERROR, "list tasks: "+err.Error())
		return
	}
	for _, j := range list {
		out.Tasks = append(out.Tasks, g.taskOf(j))
	}
	if len(list) == page {
		last := list[len(list)-1]
		out.NextCursor = mcp.Cursor(strconv.FormatInt(last.CreatedAt, 10) + ":" + last.ID)
	}
	writeRPCResult(w, id, out)
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

// markTaskSupport is the tools/list hook that marks the tools the
// administrator made async as task-capable, for clients on MCPTasks.
func (g *Gateway) markTaskSupport(ctx context.Context, _ any, _ *mcp.ListToolsRequest, res *mcp.ListToolsResult) {
	if res == nil || g.asyncPolicy == nil || !tasksBridged(ctx) {
		return
	}
	type ref struct {
		i        int
		upstream string
		tool     string
	}
	var refs []ref
	g.mu.RLock()
	for i, t := range res.Tools {
		if e, ok := g.tools[t.Name]; ok && !internalEntry(e) && e.upstream != jobsUpstream {
			refs = append(refs, ref{i, e.upstream, e.originalName})
		}
	}
	g.mu.RUnlock()
	for _, r := range refs {
		if g.asyncPolicy.AsyncSetting(r.upstream).RuleFor(r.tool).Enabled() {
			res.Tools[r.i].Execution = &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}
		}
	}
}

// writeRPCResult writes a JSON-RPC success answer.
func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	body, err := json.Marshal(map[string]any{"jsonrpc": mcp.JSONRPC_VERSION, "id": id, "result": result})
	if err != nil {
		writeRPCError(w, id, mcp.INTERNAL_ERROR, "encode result: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeRPCError writes a JSON-RPC error answer.
func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": mcp.JSONRPC_VERSION, "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
