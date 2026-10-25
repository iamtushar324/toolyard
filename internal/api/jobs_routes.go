package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/jobs"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// Async tool jobs over REST (docs/guidelines/async-tools.md). Like the
// code routes, every job route takes an agent token or a session token as
// a bearer and never reads a cookie; operator tokens are refused by the
// operator guard (operatorScopeFor) before a handler runs.
//
//	POST /v1/jobs                start a call as a job {tool, arguments, reason, callback_ref, wait_seconds};
//	                             Idempotency-Key, Prefer: wait=N (0-60, default 0)
//	GET  /v1/jobs                the agent's jobs (?status=active|queued|...&limit=1-100&before=<cursor>)
//	GET  /v1/jobs/{id}           one job (Prefer: wait=N)
//	GET  /v1/jobs/{id}/result    a finished job's tool result (Prefer: wait=N)
//	POST /v1/jobs/{id}/cancel    cancel a job
//
// Administrators read and set a server's async setting with
// GET/PUT /v1/servers/{name}/async (serversAsync).

const (
	jobsPath   = "/v1/jobs"
	jobsPrefix = "/v1/jobs/"
	// maxJobStartBody caps POST /v1/jobs: a tool call's arguments.
	maxJobStartBody = 4 << 20
	// viaREST is the path in of a REST job call.
	viaREST = "rest"
)

// registerJobsRoutes adds the job routes to mux. The paths are literals
// so the route scan in role_guard_test.go reads them.
func (s *Server) registerJobsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/jobs", s.jobsCollection)
	mux.HandleFunc("/v1/jobs/", s.jobsItem)
}

// jobsReady reports whether this server runs async jobs, writing 503
// when it does not.
func (s *Server) jobsReady(w http.ResponseWriter) bool {
	if s.jobs == nil || s.gateway == nil || s.gateway.Jobs() == nil {
		writeError(w, http.StatusServiceUnavailable, "async jobs are not enabled on this server")
		return false
	}
	return true
}

// POST /v1/jobs, GET /v1/jobs
func (s *Server) jobsCollection(w http.ResponseWriter, r *http.Request) {
	ag, sessionID, viaSession, err := s.codeCaller(r)
	if err != nil {
		writeCallerError(w, err)
		return
	}
	if !s.jobsReady(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.jobsList(w, r, ag.ID)
	case http.MethodPost:
		ctx := actor.WithRaiser(r.Context(), s.codeRaiser(r, ag, sessionID, viaSession, clientKindCLI, viaREST))
		s.jobsStart(w, r, ctx, ag.ID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

// jobStartBody is a POST /v1/jobs body.
type jobStartBody struct {
	Tool        string         `json:"tool"`
	Arguments   map[string]any `json:"arguments"`
	Reason      string         `json:"reason"`
	CallbackRef string         `json:"callback_ref"`
	WaitSeconds *int           `json:"wait_seconds"`
}

func (s *Server) jobsStart(w http.ResponseWriter, r *http.Request, ctx context.Context, agentID string) {
	if mediaType(r) != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "only application/json is accepted")
		return
	}
	var body jobStartBody
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJobStartBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "the body is over 4 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil || dec.More() {
		msg := "the body must be one JSON object {tool, arguments, reason, callback_ref, wait_seconds}"
		if err != nil {
			msg += ": " + err.Error()
		}
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	body.Tool = strings.TrimSpace(body.Tool)
	if body.Tool == "" {
		writeError(w, http.StatusBadRequest, "tool is required")
		return
	}
	if n := len(strings.TrimSpace(body.Reason)); n < 20 || n > 2000 {
		writeError(w, http.StatusBadRequest, "reason must be 20-2000 characters (got "+strconv.Itoa(n)+")")
		return
	}
	if err := jobs.CheckCallbackRef(body.CallbackRef); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	wait := time.Duration(0)
	if d, ok := preferWait(r); ok {
		wait = d
	}
	if body.WaitSeconds != nil {
		n := *body.WaitSeconds
		if n < 0 || n > int(jobs.MaxWait/time.Second) {
			writeError(w, http.StatusBadRequest, "wait_seconds must be between 0 and 60")
			return
		}
		wait = time.Duration(n) * time.Second
	}
	key := ""
	switch keys := r.Header.Values(headerIdemKey); len(keys) {
	case 0:
	case 1:
		key = keys[0]
		if key == "" || jobs.CheckIdempotencyKey(key) != nil {
			writeError(w, http.StatusBadRequest, "Idempotency-Key must be 1-128 printable ASCII characters")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "send one Idempotency-Key")
		return
	}
	job, res, err := s.gateway.StartJob(ctx, agentID, body.Tool, body.Arguments, body.Reason, body.CallbackRef, key)
	if err != nil {
		writeJobError(w, err)
		return
	}
	if job == nil {
		// Answered without a job: unknown tool, refused, or held for
		// approval. Nothing runs in the background.
		code := http.StatusOK
		if res == nil || res.IsError {
			code = http.StatusUnprocessableEntity
		}
		writeJSON(w, code, map[string]any{
			"job_id": nil,
			"status": "not_started",
			"hint":   "the call was answered without starting a job; result is the gateway's answer (a refusal, permission_required, or a pending approval)",
			"result": resultView(res),
		})
		return
	}
	s.answerJob(w, r.Context(), job, wait)
}

// GET /v1/jobs
func (s *Server) jobsList(w http.ResponseWriter, r *http.Request, agentID string) {
	q := r.URL.Query()
	f := jobs.Filter{AgentID: agentID, Limit: 20}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "limit must be 1-100")
			return
		}
		f.Limit = n
	}
	switch st := q.Get("status"); st {
	case "", "active", jobs.StatusQueued, jobs.StatusRunning, jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusCancelled, jobs.StatusOutcomeUnknown:
		f.Status = st
	default:
		writeError(w, http.StatusBadRequest, "status must be active, queued, running, succeeded, failed, cancelled or outcome_unknown")
		return
	}
	if v := q.Get("before"); v != "" {
		ts, id, ok := strings.Cut(v, ":")
		n, err := strconv.ParseInt(ts, 10, 64)
		if !ok || err != nil || id == "" {
			writeError(w, http.StatusBadRequest, "before must be a cursor from a previous page")
			return
		}
		f.BeforeCreated, f.BeforeID = n, id
	}
	list, err := s.jobs.List(r.Context(), f)
	if err != nil {
		writeJobError(w, err)
		return
	}
	views := make([]jobView, 0, len(list))
	for _, j := range list {
		views = append(views, newJobView(j))
	}
	out := map[string]any{"jobs": views, "next": nil}
	if len(list) == f.Limit {
		last := list[len(list)-1]
		out["next"] = strconv.FormatInt(last.CreatedAt, 10) + ":" + last.ID
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// GET  /v1/jobs/{id}
// GET  /v1/jobs/{id}/result
// POST /v1/jobs/{id}/cancel
func (s *Server) jobsItem(w http.ResponseWriter, r *http.Request) {
	ag, _, _, err := s.codeCaller(r)
	if err != nil {
		writeCallerError(w, err)
		return
	}
	id, sub, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, jobsPrefix), "/")
	if id == "" || strings.Contains(sub, "/") || (sub != "" && sub != "result" && sub != "cancel") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !s.jobsReady(w) {
		return
	}
	j, err := s.jobs.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) || (err == nil && j.AgentID != ag.ID) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		writeJobError(w, err)
		return
	}
	wait, _ := preferWait(r)
	switch sub {
	case "":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		s.answerJob(w, r.Context(), j, wait)
	case "result":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		j = s.waitJob(r.Context(), j, wait)
		if !j.Final() {
			writeJobView(w, j)
			return
		}
		res, err := s.gateway.JobResult(r.Context(), j)
		if err != nil {
			writeJobError(w, err)
			return
		}
		out := map[string]any{"job_id": j.ID, "status": j.Status, "result": nil}
		if res != nil {
			out["result"] = resultView(res)
		}
		if j.Error != "" {
			out["error"] = j.Error
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, out)
	case "cancel":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST only")
			return
		}
		ctx := gateway.WithAgentID(actor.WithRaiser(r.Context(), s.agentRaiser(r, ag, clientKindCLI, viaREST)), ag.ID)
		cur, cancelled, err := s.gateway.CancelJob(ctx, j.ID)
		if err != nil {
			writeJobError(w, err)
			return
		}
		v := newJobView(cur)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"job": v, "cancel_accepted": cancelled})
	}
}

// waitJob waits up to wait for j to be final and returns its latest state.
func (s *Server) waitJob(ctx context.Context, j *jobs.Job, wait time.Duration) *jobs.Job {
	if j.Final() || wait <= 0 {
		return j
	}
	list, _, err := s.jobs.WaitFor(ctx, []string{j.ID}, false, min(wait, jobs.MaxWait))
	if err == nil && len(list) == 1 && list[0] != nil {
		return list[0]
	}
	return j
}

// answerJob waits up to wait, then writes the job view.
func (s *Server) answerJob(w http.ResponseWriter, ctx context.Context, j *jobs.Job, wait time.Duration) {
	writeJobView(w, s.waitJob(ctx, j, wait))
}

// jobView is the job view every job route answers.
type jobView struct {
	JobID           string         `json:"job_id"`
	Status          string         `json:"status"`
	Final           bool           `json:"final"`
	Tool            string         `json:"tool"`
	Upstream        string         `json:"upstream"`
	Reason          string         `json:"reason"`
	Via             string         `json:"via"`
	Progress        *jobs.Progress `json:"progress"`
	Error           *string        `json:"error"`
	MayHaveRun      *bool          `json:"may_have_run,omitempty"`
	CancelRequested bool           `json:"cancel_requested,omitempty"`
	CallbackRef     string         `json:"callback_ref,omitempty"`
	CallbackError   string         `json:"callback_error,omitempty"`
	ResultAvailable bool           `json:"result_available"`
	ResultBytes     int            `json:"result_bytes"`
	CreatedAt       int64          `json:"created_at"`
	UpdatedAt       int64          `json:"updated_at"`
	StartedAt       *int64         `json:"started_at"`
	FinishedAt      *int64         `json:"finished_at"`
	Links           jobLinks       `json:"links"`
}

type jobLinks struct {
	Self   string `json:"self"`
	Result string `json:"result"`
	Cancel string `json:"cancel"`
}

func newJobView(j *jobs.Job) jobView {
	v := jobView{
		JobID: j.ID, Status: j.Status, Final: j.Final(), Tool: j.Tool, Upstream: j.Upstream, Reason: j.Reason, Via: j.Via,
		Progress: j.Progress, CallbackRef: j.CallbackRef, CallbackError: j.CallbackError,
		ResultAvailable: j.Final() && j.HasResult(), ResultBytes: j.ResultBytes, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
		CancelRequested: j.CancelRequested && !j.Final(),
		Links:           jobLinks{Self: jobsPrefix + j.ID, Result: jobsPrefix + j.ID + "/result", Cancel: jobsPrefix + j.ID + "/cancel"},
	}
	if j.Error != "" {
		e := j.Error
		v.Error = &e
	}
	if j.StartedAt > 0 {
		t := j.StartedAt
		v.StartedAt = &t
	}
	if j.FinishedAt > 0 {
		t := j.FinishedAt
		v.FinishedAt = &t
	}
	switch j.Status {
	case jobs.StatusOutcomeUnknown:
		yes := true
		v.MayHaveRun = &yes
	case jobs.StatusCancelled:
		ran := j.StartedAt > 0
		v.MayHaveRun = &ran
	}
	return v
}

// writeJobView writes the view: 200 for a final job, 202 with Location
// while it is queued or running.
func writeJobView(w http.ResponseWriter, j *jobs.Job) {
	code := http.StatusOK
	if !j.Final() {
		code = http.StatusAccepted
		w.Header().Set("Location", jobsPrefix+j.ID)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, newJobView(j))
}

// resultView is a tool result as the job routes show it.
func resultView(res *mcp.CallToolResult) map[string]any {
	if res == nil {
		return nil
	}
	content := res.Content
	if content == nil {
		content = []mcp.Content{}
	}
	return map[string]any{"is_error": res.IsError, "content": content, "structured_content": res.StructuredContent}
}

// writeJobError writes the answer for a job service error.
func writeJobError(w http.ResponseWriter, err error) {
	var ie *jobs.InputError
	switch {
	case errors.As(err, &ie):
		writeError(w, http.StatusBadRequest, ie.Error())
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "job not found")
	case errors.Is(err, jobs.ErrIdempotencyKeyReused):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": jobs.ErrIdempotencyKeyReused.Error(),
			"hint":  "this Idempotency-Key was used with another tool or other arguments; use a new key",
		})
	case errors.Is(err, jobs.ErrTooManyActive):
		w.Header().Set("Retry-After", busyRetryAfter)
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, jobs.ErrDraining):
		w.Header().Set("Retry-After", drainRetryAfter)
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("Retry-After", busyRetryAfter)
		writeError(w, http.StatusServiceUnavailable, "the request ended early")
	default:
		logx.For("api").Error("jobs", "err", err.Error())
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// GET /v1/servers/{name}/async  the server's async setting
// PUT /v1/servers/{name}/async  replace it (admin) {mode, after_ms, tools: {tool: {mode, after_ms}}}
//
// mode is off, always (a call answers with a job id at once) or after (it
// answers inline when the call ends within after_ms, else with a job id).
// A tool rule overrides the server's. Saving never reconnects the server.
func (s *Server) serversAsync(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodGet:
		srv, err := s.upstreams.Get(r.Context(), name)
		if errors.Is(err, upstreams.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, asyncAnswer(name, srv.Async))
	case http.MethodPut:
		u, err := s.requireAdmin(r)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		var body jobs.AsyncSetting
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		set, err := s.upstreams.SetAsync(r.Context(), name, body)
		switch {
		case errors.Is(err, upstreams.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
			return
		case errors.Is(err, upstreams.ErrInvalid):
			writeError(w, http.StatusBadRequest, err.Error())
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		summary := name + ": async off"
		if set != nil {
			if raw, err := json.Marshal(set); err == nil {
				summary = name + ": async " + string(raw)
			}
		}
		_ = s.audit.Write(r.Context(), audit.Event{EventType: "server.async", AgentID: "user:" + u.ID, ResultSummary: summary})
		writeJSON(w, http.StatusOK, asyncAnswer(name, set))
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or PUT")
	}
}

func asyncAnswer(name string, set *jobs.AsyncSetting) map[string]any {
	if set == nil {
		set = &jobs.AsyncSetting{Mode: jobs.AsyncOff}
	}
	return map[string]any{"server": name, "async": set}
}
