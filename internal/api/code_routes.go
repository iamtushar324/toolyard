package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/docs"
	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/coderuns"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// Code mode over REST (docs/design/code-rest-route.md, TEC-1627): the
// backup route an agent uses when its MCP connection fails. Every route
// but discovery takes an agent token or a session token as a bearer and
// never reads a cookie; operator tokens are refused by the operator guard
// (operatorScopeFor) before a handler runs.
//
//	POST /v1/code/run                         start a run (text/x-starlark, text/javascript or JSON)
//	GET  /v1/code/runs/{id}                   read a run (Prefer: wait=N)
//	POST /v1/code/runs/{id}/resume            resume a held run {request_id}
//	GET  /v1/code/files                       listToolFiles (?language=javascript for .d.ts)
//	GET  /v1/code/files/{server}/{tool}.pyi   readToolFile
//	GET  /v1/code/files/{server}/{tool}.d.ts  readToolFile, TypeScript (also {server}.d.ts, index.d.ts)
//	GET  /v1/code/docs/{server}/{tool}        getToolDocs
//	GET  /v1/code/search                      tools.search (?q=, mode, detail, limit, upstream)
//	GET  /v1/code/openapi.json                this API as OpenAPI 3.1 (no token)
//	GET  /v1/code/ty                          the ty command (no token)

const (
	codePathPrefix    = "/v1/code/"
	codeRunPath       = "/v1/code/run"
	codeRunsPrefix    = "/v1/code/runs/"
	codeFilesPrefix   = "/v1/code/files/"
	codeDocsPrefix    = "/v1/code/docs/"
	codeOpenAPIPath   = "/v1/code/openapi.json"
	codeTyPath        = "/v1/code/ty"
	codeSearchPath    = "/v1/code/search"
	contentTypeScript = "text/x-starlark"
	// contentTypeJS (or application/javascript) posts a JavaScript script.
	contentTypeJS = "text/javascript"
	headerReason  = "X-Toolyard-Reason"
	headerIdemKey = "Idempotency-Key"
	// defaultCodeWait is how long a start or resume waits for a final or
	// held status when the request names no wait.
	defaultCodeWait = 25 * time.Second
	// maxCodeRunBody is the POST /v1/code/run body cap: 4 MiB, the outer
	// LimitBodyByPath ceiling, so a 1 MiB script still fits after JSON
	// escaping up to four times its size (ty sends unescaped UTF-8). The
	// script itself is checked against 1 MiB after decoding; a raw
	// text/x-starlark body is read to 1 MiB + 1 only.
	maxCodeRunBody = 4 * coderuns.MaxScriptBytes
	// maxCodeResumeBody caps the resume body ({"request_id": "..."}).
	maxCodeResumeBody = 4 << 10
	// busyRetryAfter and drainRetryAfter are the Retry-After seconds.
	busyRetryAfter  = "2"
	drainRetryAfter = "5"
)

// Audit event types for runs. The script is never logged; the reason is.
const (
	eventCodeRun    = "code.run"
	eventCodeResume = "code.resume"
)

// registerCodeRoutes adds the code routes to mux. The paths are literals so
// the route scan in role_guard_test.go reads them.
func (s *Server) registerCodeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/code/run", s.codeRun)
	mux.HandleFunc("/v1/code/runs/", s.codeRunsItem)
	mux.HandleFunc("/v1/code/files", s.codeFiles)
	mux.HandleFunc("/v1/code/files/", s.codeFile)
	mux.HandleFunc("/v1/code/docs/", s.codeDocs)
	mux.HandleFunc("/v1/code/search", s.codeSearch)
	mux.HandleFunc("/v1/code/openapi.json", s.codeOpenAPI)
	mux.HandleFunc("/v1/code/ty", s.codeTy)
}

// POST /v1/code/run
//
// Body: the raw script with Content-Type: text/x-starlark (or
// text/javascript) and the reason in X-Toolyard-Reason, or JSON
// {"code","reason","language","wait_seconds"}. Optional Idempotency-Key
// and Prefer: wait=N (0-60, default 25; wait_seconds wins).
func (s *Server) codeRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	ag, sessionID, viaSession, err := s.codeCaller(r)
	if err != nil {
		writeCallerError(w, err)
		return
	}
	if s.codeRuns == nil {
		writeError(w, http.StatusServiceUnavailable, coderuns.ErrUnavailable.Error())
		return
	}
	in, ok := readRunRequest(w, r)
	if !ok {
		return
	}
	keys := r.Header.Values(headerIdemKey)
	key := ""
	switch len(keys) {
	case 0:
	case 1:
		if key = keys[0]; key == "" {
			writeError(w, http.StatusBadRequest, "Idempotency-Key must be 1-128 printable ASCII characters")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "send one Idempotency-Key")
		return
	}
	ctx := actor.WithRaiser(r.Context(), s.codeRaiser(r, ag, sessionID, viaSession, clientKindCLI, codemode.Via))
	run, created, err := s.codeRuns.Start(ctx, coderuns.StartRequest{
		AgentID: ag.ID, SessionID: sessionID, Code: in.code, Reason: in.reason, IdempotencyKey: key, Language: in.language,
	})
	if err != nil {
		writeRunError(w, err)
		return
	}
	if created {
		s.auditCodeRun(ctx, eventCodeRun, ag, run, nil)
	}
	s.answerRun(w, r, run, in.wait)
}

// runRequest is a parsed POST /v1/code/run body.
type runRequest struct {
	code, reason, language string
	wait                   time.Duration
}

// readRunRequest parses a run body, writing the error answer itself.
func readRunRequest(w http.ResponseWriter, r *http.Request) (runRequest, bool) {
	in := runRequest{wait: defaultCodeWait}
	if d, ok := preferWait(r); ok {
		in.wait = d
	}
	switch mt := mediaType(r); mt {
	case contentTypeScript, contentTypeJS, "application/javascript":
		in.language = codemode.LanguageStarlark
		if mt != contentTypeScript {
			in.language = codemode.LanguageJavaScript
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, coderuns.MaxScriptBytes+1))
		if err != nil {
			writeBodyError(w, err)
			return in, false
		}
		if len(body) > coderuns.MaxScriptBytes {
			writeError(w, http.StatusRequestEntityTooLarge, coderuns.ErrTooLarge.Error())
			return in, false
		}
		if !utf8.Valid(body) {
			writeError(w, http.StatusBadRequest, "the script is not UTF-8 text")
			return in, false
		}
		in.code, in.reason = string(body), r.Header.Get(headerReason)
	case "application/json":
		var body struct {
			Code        *string `json:"code"`
			Reason      string  `json:"reason"`
			Language    string  `json:"language"`
			WaitSeconds *int    `json:"wait_seconds"`
		}
		if err := decodeLimited(w, r, &body, maxCodeRunBody); err != nil {
			writeBodyError(w, err)
			return in, false
		}
		if body.Code == nil {
			writeError(w, http.StatusBadRequest, "code is required")
			return in, false
		}
		if len(*body.Code) > coderuns.MaxScriptBytes {
			writeError(w, http.StatusRequestEntityTooLarge, coderuns.ErrTooLarge.Error())
			return in, false
		}
		if body.WaitSeconds != nil {
			n := *body.WaitSeconds
			if n < 0 || n > int(coderuns.MaxWait/time.Second) {
				writeError(w, http.StatusBadRequest, "wait_seconds must be between 0 and 60")
				return in, false
			}
			in.wait = time.Duration(n) * time.Second
		}
		lang, err := codemode.ParseLanguage(body.Language)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return in, false
		}
		in.code, in.reason, in.language = *body.Code, body.Reason, lang
	default:
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be text/x-starlark or text/javascript (with X-Toolyard-Reason), or application/json")
		return in, false
	}
	return in, true
}

// GET  /v1/code/runs/{id}         the run view (Prefer: wait=N, default 0)
// POST /v1/code/runs/{id}/resume  resume a held run
func (s *Server) codeRunsItem(w http.ResponseWriter, r *http.Request) {
	ag, sessionID, viaSession, err := s.codeCaller(r)
	if err != nil {
		writeCallerError(w, err)
		return
	}
	id, sub, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, codeRunsPrefix), "/")
	if id == "" || strings.Contains(sub, "/") || (sub != "" && sub != "resume") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if s.codeRuns == nil {
		writeError(w, http.StatusServiceUnavailable, coderuns.ErrUnavailable.Error())
		return
	}
	if sub == "" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		wait, _ := preferWait(r)
		run, err := s.codeRuns.Get(r.Context(), ag.ID, id, wait)
		if err != nil {
			writeRunError(w, err)
			return
		}
		writeRunView(w, run)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	requestID, ok := readResumeRequest(w, r)
	if !ok {
		return
	}
	wait := defaultCodeWait
	if d, ok := preferWait(r); ok {
		wait = d
	}
	ctx := actor.WithRaiser(r.Context(), s.codeRaiser(r, ag, sessionID, viaSession, clientKindCLI, codemode.Via))
	run, err := s.codeRuns.Resume(ctx, coderuns.ResumeRequest{AgentID: ag.ID, SessionID: sessionID, RunID: id, RequestID: requestID})
	if errors.Is(err, coderuns.ErrNotHeld) && run != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": coderuns.ErrNotHeld.Error(), "status": run.Status,
			"hint": "only a permission_required or deferred run can be resumed; read it with GET " + coderuns.RunPath(run.ID),
		})
		return
	}
	if err != nil {
		writeRunError(w, err)
		return
	}
	var args map[string]any
	if requestID != "" {
		args = map[string]any{"request_id": requestID}
	}
	s.auditCodeRun(ctx, eventCodeResume, ag, run, args)
	s.answerRun(w, r, run, wait)
}

// readResumeRequest reads the optional {"request_id": "..."} body.
func readResumeRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxCodeResumeBody+1))
	if err != nil {
		writeBodyError(w, err)
		return "", false
	}
	if len(raw) > maxCodeResumeBody {
		writeError(w, http.StatusRequestEntityTooLarge, "the resume body is over 4 KiB")
		return "", false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", true
	}
	if mediaType(r) != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "only application/json is accepted")
		return "", false
	}
	var body struct {
		RequestID string `json:"request_id"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.More() {
		writeError(w, http.StatusBadRequest, "the body must be {\"request_id\": \"rq_...\"} or empty")
		return "", false
	}
	id := strings.TrimSpace(body.RequestID)
	if len(id) > 128 {
		writeError(w, http.StatusBadRequest, "request_id is too long")
		return "", false
	}
	return id, true
}

// answerRun waits up to wait for run to leave queued and running, then
// writes the run view.
func (s *Server) answerRun(w http.ResponseWriter, r *http.Request, run *coderuns.Run, wait time.Duration) {
	cur, err := s.codeRuns.Get(r.Context(), run.AgentID, run.ID, wait)
	if err != nil {
		writeRunError(w, err)
		return
	}
	writeRunView(w, cur)
}

// writeRunView writes the run view with the HTTP status of its run status.
func writeRunView(w http.ResponseWriter, run *coderuns.Run) {
	code := coderuns.HTTPStatus(run.Status)
	if code == http.StatusAccepted {
		w.Header().Set("Location", coderuns.RunPath(run.ID))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, run.View())
}

// writeRunError writes the answer for a coderuns error.
func writeRunError(w http.ResponseWriter, err error) {
	var ie *coderuns.InputError
	switch {
	case errors.As(err, &ie):
		writeError(w, http.StatusBadRequest, ie.Message)
	case errors.Is(err, coderuns.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, coderuns.ErrNotFound):
		writeError(w, http.StatusNotFound, "run not found")
	case errors.Is(err, coderuns.ErrIdempotencyKeyReused):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": coderuns.ErrIdempotencyKeyReused.Error(),
			"hint":  "this Idempotency-Key was used with another script or reason; use a new key",
		})
	case errors.Is(err, coderuns.ErrNotHeld):
		writeError(w, http.StatusUnprocessableEntity, coderuns.ErrNotHeld.Error())
	case errors.Is(err, codemode.ErrBusy):
		w.Header().Set("Retry-After", busyRetryAfter)
		msg := "code mode is busy: every script slot is in use, so no run was created; try again in a moment"
		if detail := err.Error(); detail != codemode.ErrBusy.Error() {
			msg = detail + " (no run was created)"
		}
		writeError(w, http.StatusTooManyRequests, msg)
	case errors.Is(err, coderuns.ErrDraining):
		w.Header().Set("Retry-After", drainRetryAfter)
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, coderuns.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("Retry-After", busyRetryAfter)
		writeError(w, http.StatusServiceUnavailable, "the request ended before the run started; nothing ran")
	default:
		logx.For("api").Error("code run", "err", err.Error())
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// writeBodyError answers a body that could not be read or decoded.
func writeBodyError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeError(w, http.StatusRequestEntityTooLarge, coderuns.ErrTooLarge.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

// decodeLimited is decode with a larger cap (a run body carries a 1 MiB
// script, which JSON escaping can grow past decode's 1 MiB).
func decodeLimited(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty body")
		}
		return err
	}
	if dec.More() {
		return errors.New("body has trailing JSON tokens")
	}
	return nil
}

// mediaType is the request's lowercased media type without parameters.
func mediaType(r *http.Request) string {
	ct := r.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		return mt
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// preferWait reads "Prefer: wait=N" (RFC 7240): N seconds, at most 60.
// ok is false without a usable wait preference.
func preferWait(r *http.Request) (time.Duration, bool) {
	for _, v := range r.Header.Values("Prefer") {
		for _, pref := range strings.Split(v, ",") {
			tok, _, _ := strings.Cut(pref, ";")
			name, val, found := strings.Cut(strings.TrimSpace(tok), "=")
			if !found || !strings.EqualFold(strings.TrimSpace(name), "wait") {
				continue
			}
			n, err := strconv.Atoi(strings.Trim(strings.TrimSpace(val), `"`))
			if err != nil || n < 0 {
				continue
			}
			return min(time.Duration(n)*time.Second, coderuns.MaxWait), true
		}
	}
	return 0, false
}

// auditCodeRun records a run start or resume: the agent, its owner and
// client (the raiser on ctx), the reason and the run id. Never the script.
func (s *Server) auditCodeRun(ctx context.Context, eventType string, ag *identity.Agent, run *coderuns.Run, args map[string]any) {
	if s.audit == nil || ag == nil || run == nil {
		return
	}
	ev := audit.Event{EventType: eventType, AgentID: ag.ID, Reason: run.Reason, ResultSummary: run.ID, RunID: run.ID}
	if args != nil {
		if raw, err := json.Marshal(args); err == nil {
			ev.Arguments = raw
		}
	}
	_ = s.audit.Write(context.WithoutCancel(ctx), ev)
}

// GET /v1/code/files — the listToolFiles tree for the caller (text/plain);
// ?language=javascript lists the TypeScript stubs.
func (s *Server) codeFiles(w http.ResponseWriter, r *http.Request) {
	rt, ctx, ok := s.codeStubs(w, r)
	if !ok {
		return
	}
	lang, err := codemode.ParseLanguage(r.URL.Query().Get("language"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeText(w, http.StatusOK, rt.ListFiles(ctx, lang))
}

// GET /v1/code/files/{server}/{tool}.pyi — one stub, as readToolFile; the
// TypeScript stubs are /v1/code/files/{server}/{tool}.d.ts,
// /v1/code/files/{server}.d.ts and /v1/code/files/index.d.ts.
func (s *Server) codeFile(w http.ResponseWriter, r *http.Request) {
	rt, ctx, ok := s.codeStubs(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, codeFilesPrefix)
	if p, isTS := strings.CutSuffix(rest, ".d.ts"); isTS && p != "" && strings.Count(p, "/") <= 1 && !strings.HasSuffix(p, "/") && !strings.HasPrefix(p, "/") {
		text, ok := rt.ReadToolFile(ctx, "servers/"+rest, nil, nil)
		if !ok {
			writeError(w, http.StatusNotFound, text)
			return
		}
		writeText(w, http.StatusOK, text)
		return
	}
	server, file, found := strings.Cut(rest, "/")
	tool, isStub := strings.CutSuffix(file, ".pyi")
	if !found || server == "" || !isStub || tool == "" || strings.Contains(tool, "/") {
		writeError(w, http.StatusNotFound, "no such file: use /v1/code/files/{server}/{tool}.pyi (or .d.ts) as GET /v1/code/files lists them")
		return
	}
	text, ok := rt.ReadToolFile(ctx, "servers/"+server+"/"+tool+".pyi", nil, nil)
	if !ok {
		writeError(w, http.StatusNotFound, text)
		return
	}
	writeText(w, http.StatusOK, text)
}

// GET /v1/code/docs/{server}/{tool} — one tool's docs, as getToolDocs.
func (s *Server) codeDocs(w http.ResponseWriter, r *http.Request) {
	rt, ctx, ok := s.codeStubs(w, r)
	if !ok {
		return
	}
	server, tool, found := strings.Cut(strings.TrimPrefix(r.URL.Path, codeDocsPrefix), "/")
	if !found || server == "" || tool == "" || strings.Contains(tool, "/") {
		writeError(w, http.StatusNotFound, "no such tool: use /v1/code/docs/{server}/{tool}")
		return
	}
	text, ok := rt.GetToolDocs(ctx, server, tool)
	if !ok {
		writeError(w, http.StatusNotFound, text)
		return
	}
	writeText(w, http.StatusOK, text)
}

// codeStubs checks a stub request (GET, a code caller) and returns the
// runtime and the agent-scoped context the stubs render for, or writes the
// error answer itself.
func (s *Server) codeStubs(w http.ResponseWriter, r *http.Request) (*codemode.Runtime, context.Context, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return nil, nil, false
	}
	ag, _, _, err := s.codeCaller(r)
	if err != nil {
		writeCallerError(w, err)
		return nil, nil, false
	}
	if s.gateway == nil {
		writeError(w, http.StatusServiceUnavailable, coderuns.ErrUnavailable.Error())
		return nil, nil, false
	}
	rt := s.gateway.CodeMode()
	if rt == nil {
		writeError(w, http.StatusServiceUnavailable, coderuns.ErrUnavailable.Error())
		return nil, nil, false
	}
	return rt, gateway.WithAgentID(r.Context(), ag.ID), true
}

// GET /v1/code/search?q=...&mode=keyword|regex&detail=name|summary|full&limit=N&upstream=...
// — tools.search for the caller, as JSON.
func (s *Server) codeSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	ag, _, _, err := s.codeCaller(r)
	if err != nil {
		writeCallerError(w, err)
		return
	}
	if s.gateway == nil {
		writeError(w, http.StatusServiceUnavailable, coderuns.ErrUnavailable.Error())
		return
	}
	q := r.URL.Query()
	req := gateway.SearchRequest{Query: q.Get("q"), Mode: q.Get("mode"), Detail: q.Get("detail"), Upstream: q.Get("upstream")}
	if q.Get("q") == "" {
		req.Query = q.Get("query")
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a whole number of at least 1")
			return
		}
		req.Limit = n
	}
	body, err := s.gateway.SearchTools(gateway.WithAgentID(r.Context(), ag.ID), req)
	var se *gateway.SearchError
	switch {
	case errors.As(err, &se):
		writeError(w, http.StatusBadRequest, se.Message)
		return
	case err != nil:
		logx.For("api").Error("code search", "err", err.Error())
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, body)
}

// GET /v1/code/openapi.json — public: no token.
func (s *Server) codeOpenAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = io.WriteString(w, docs.CodeOpenAPI)
}

// GET /v1/code/ty — the ty command (Python 3, standard library); public.
func (s *Server) codeTy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = io.WriteString(w, docs.TyScript)
}

// writeText writes a plain-text answer.
func writeText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}
