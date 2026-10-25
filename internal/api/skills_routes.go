package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// The skills hub for agents over REST (docs/skills-hub.md, "For agents").
// Like the job routes, these take an agent token or a session token as a
// bearer and never read a cookie; operator tokens are refused. Every route
// but the download is the matching skills.* tool run through the
// gateway's call pipeline (RouteCall, via "rest"), so access, an explicit
// deny policy, the audit rows and the metrics are those of the tool call,
// and the body is the tool's structured answer:
//
//	GET  /v1/skills/search   ?query=&mode=keyword|regex&source=&limit=&include_host=   skills.search
//	GET  /v1/skills/get      ?skill=&include_files=                                   skills.get
//	GET  /v1/skills/read     ?uri=&offset=&limit=                                     skills.read
//	GET  /v1/skills/grep     ?pattern=&source=&path=&ignore_case=&limit=&include_host= skills.grep
//	GET  /v1/skills/sources                                                           skills.sources
//	GET  /v1/skills/list     ?cursor=&limit=&source=&include_host=                    skills.list
//	GET  /v1/skills/proposal ?id=                                                     skills.proposal: one of the caller's proposals
//	POST /v1/skills/bundle   {skills, reason}                                         skills.bundle: {url, expires_at, skills}
//	GET  /v1/skills/bundle/{ticket}                                                   the .tar.gz
//
// Each GET takes ?reason= for the audit record (20-2000 characters;
// default: the route). An unknown query parameter is refused, so a typo
// never silently widens a search. Refusals carry {error, code}: code
// not_found 404, conflict 409, invalid 400, limit 422, unavailable 503,
// and a call the gateway refused (an explicit deny policy, the agent's own
// limits) is 403.
//
// The download needs no token: the single-use ticket in its path is the
// credential (10 minutes), so a person can open it in a browser or hand it
// to curl. It runs as the caller that asked for it, whose visibility is
// checked again when it is redeemed.
//
// Route layout with the admin surface: /v1/skills/sources is shared. This
// file serves a GET that carries an agent or session token (the agent
// listing); every other request to that exact path (an admin's dashboard
// GET, a POST that creates a source) belongs to the admin routes, which
// own the /v1/skills/sources/ subtree too.

const (
	skillsBundlePath  = "/v1/skills/bundle"
	skillsSourcesPath = "/v1/skills/sources"
	// maxSkillsBundleBody caps POST /v1/skills/bundle: a list of up to 50
	// skill references and a reason.
	maxSkillsBundleBody = 64 << 10
)

// skillsParamKind is how a query parameter is read.
type skillsParamKind int

const (
	paramString skillsParamKind = iota
	paramInt
	paramBool
)

// skillsReadRoute is one GET route: the tool it runs and its parameters.
type skillsReadRoute struct {
	tool   string
	params map[string]skillsParamKind
}

// skillsReadRoutes are the GET routes by path.
var skillsReadRoutes = map[string]skillsReadRoute{
	"/v1/skills/search": {gateway.SkillsSearchTool, map[string]skillsParamKind{
		"query": paramString, "mode": paramString, "source": paramString, "limit": paramInt, "include_host": paramBool}},
	"/v1/skills/get": {gateway.SkillsGetTool, map[string]skillsParamKind{
		"skill": paramString, "include_files": paramBool}},
	"/v1/skills/read": {gateway.SkillsReadTool, map[string]skillsParamKind{
		"uri": paramString, "offset": paramInt, "limit": paramInt}},
	"/v1/skills/grep": {gateway.SkillsGrepTool, map[string]skillsParamKind{
		"pattern": paramString, "source": paramString, "path": paramString, "ignore_case": paramBool, "limit": paramInt, "include_host": paramBool}},
	"/v1/skills/sources": {gateway.SkillsSourcesTool, map[string]skillsParamKind{}},
	"/v1/skills/list": {gateway.SkillsListTool, map[string]skillsParamKind{
		"cursor": paramString, "limit": paramInt, "source": paramString, "include_host": paramBool}},
	"/v1/skills/proposal": {gateway.SkillsProposalTool, map[string]skillsParamKind{"id": paramString}},
}

// registerSkillsRoutes adds the agent skills routes to mux. The paths are
// literals so the route scan in role_guard_test.go reads them.
func (s *Server) registerSkillsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/skills/search", s.skillsRead)
	mux.HandleFunc("/v1/skills/get", s.skillsRead)
	mux.HandleFunc("/v1/skills/read", s.skillsRead)
	mux.HandleFunc("/v1/skills/grep", s.skillsRead)
	mux.HandleFunc("/v1/skills/list", s.skillsRead)
	mux.HandleFunc("/v1/skills/proposal", s.skillsRead)
	mux.HandleFunc("/v1/skills/sources", s.skillsSources)
	mux.HandleFunc("/v1/skills/bundle", s.skillsBundle)
	mux.HandleFunc("/v1/skills/bundle/", s.skillsBundleDownload)
}

// skillsAgentPath reports whether path is one of the agent skills routes:
// bearer-only (or, for a download, ticket-only), never reading a cookie.
// /v1/skills/sources is left out: the admin surface shares it.
func skillsAgentPath(path string) bool {
	if _, ok := skillsReadRoutes[path]; ok && path != skillsSourcesPath {
		return true
	}
	return path == skillsBundlePath || skillsBundleDownloadPath(path)
}

// skillsBundleDownloadPath reports whether path is a bundle download: one
// ticket segment under the download prefix.
func skillsBundleDownloadPath(path string) bool {
	ticket, ok := strings.CutPrefix(path, gateway.SkillBundlePathPrefix)
	return ok && ticket != "" && !strings.Contains(ticket, "/")
}

// skillsReady reports whether the skills hub is on, writing 503 when it is
// not.
func (s *Server) skillsReady(w http.ResponseWriter) bool {
	if s.gateway == nil || !s.gateway.SkillHubEnabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the skills hub is off on this server", "code": gateway.SkillsErrUnavailable})
		return false
	}
	return true
}

// agentBearer reports whether r carries a bearer that may be an agent or
// session token (anything but an operator token).
func agentBearer(r *http.Request) bool {
	tok, ok := bearerToken(r)
	return ok && !strings.HasPrefix(tok, identity.OperatorTokenPrefix)
}

// GET /v1/skills/sources. Only a GET with an agent or session token is the
// agent listing; the rest of this path is the admin surface's.
func (s *Server) skillsSources(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && agentBearer(r) {
		s.skillsRead(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "not found")
}

// GET /v1/skills/{search,get,read,grep,sources,list,proposal}
func (s *Server) skillsRead(w http.ResponseWriter, r *http.Request) {
	route, ok := skillsReadRoutes[r.URL.Path]
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	ag, sessionID, viaSession, err := s.codeCaller(r)
	if err != nil {
		writeCallerError(w, err)
		return
	}
	if !s.skillsReady(w) {
		return
	}
	args, reason, err := skillsQueryArgs(r.URL.Query(), route.params)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": gateway.SkillsErrInvalid})
		return
	}
	s.skillsCall(w, r, ag, sessionID, viaSession, route.tool, args, reason)
}

// skillsQueryArgs reads a GET route's query into tool arguments, and the
// reason parameter.
func skillsQueryArgs(q url.Values, params map[string]skillsParamKind) (map[string]any, string, error) {
	args := map[string]any{}
	reason := ""
	for name, values := range q {
		if len(values) != 1 {
			return nil, "", errors.New("give " + name + " once")
		}
		v := values[0]
		if name == "reason" {
			reason = v
			continue
		}
		kind, ok := params[name]
		if !ok {
			return nil, "", errors.New("unknown parameter " + strconv.Quote(name))
		}
		switch kind {
		case paramString:
			args[name] = v
		case paramInt:
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, "", errors.New(name + " must be an integer")
			}
			args[name] = n
		case paramBool:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, "", errors.New(name + " must be true or false")
			}
			args[name] = b
		}
	}
	return args, reason, nil
}

// skillsCall runs tool for the caller with args and writes its answer.
// reason "" stands for the route.
func (s *Server) skillsCall(w http.ResponseWriter, r *http.Request, ag *identity.Agent, sessionID string, viaSession bool, tool string, args map[string]any, reason string) {
	if strings.TrimSpace(reason) == "" {
		reason = r.Method + " " + r.URL.Path + " (skills over REST)"
	}
	if n := len(strings.TrimSpace(reason)); n < 20 || n > 2000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "reason must be 20-2000 characters (got " + strconv.Itoa(n) + ")", "code": gateway.SkillsErrInvalid})
		return
	}
	args[gateway.ReasonField] = reason
	ctx := actor.WithRaiser(r.Context(), s.codeRaiser(r, ag, sessionID, viaSession, clientKindCLI, viaREST))
	res, err := s.gateway.RouteCall(gateway.WithAgentID(ctx, ag.ID), viaREST, tool, args)
	if err != nil {
		logx.For("api").Error("skills", "tool", tool, "err", err.Error())
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeSkillsResult(w, res)
}

// writeSkillsResult writes a skills tool's answer: its structured content,
// or a refusal with the status its code maps to.
func writeSkillsResult(w http.ResponseWriter, res *mcp.CallToolResult) {
	w.Header().Set("Cache-Control", "no-store")
	if res == nil {
		writeError(w, http.StatusInternalServerError, "the call returned no result")
		return
	}
	if !res.IsError {
		writeJSON(w, http.StatusOK, res.StructuredContent)
		return
	}
	code, msg := "", toolResultText(res)
	if m, ok := res.StructuredContent.(map[string]any); ok {
		code, _ = m["error"].(string)
		if s, ok := m["message"].(string); ok && s != "" {
			msg = s
		}
	}
	status := http.StatusForbidden // refused by the gateway: a deny policy, the agent's own limits
	switch code {
	case gateway.SkillsErrNotFound:
		status = http.StatusNotFound
	case gateway.SkillsErrConflict:
		status = http.StatusConflict
	case gateway.SkillsErrInvalid:
		status = http.StatusBadRequest
	case gateway.SkillsErrLimit:
		status = http.StatusUnprocessableEntity
	case gateway.SkillsErrUnavailable:
		status = http.StatusServiceUnavailable
	case gateway.SkillsErrInternal:
		status = http.StatusInternalServerError
	case "":
		code = "refused"
	}
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// toolResultText is a tool result's text content.
func toolResultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// skillsBundleBody is a POST /v1/skills/bundle body.
type skillsBundleBody struct {
	Skills []string `json:"skills"`
	Reason string   `json:"reason"`
}

// POST /v1/skills/bundle
func (s *Server) skillsBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	ag, sessionID, viaSession, err := s.codeCaller(r)
	if err != nil {
		writeCallerError(w, err)
		return
	}
	if !s.skillsReady(w) {
		return
	}
	if mediaType(r) != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "only application/json is accepted")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSkillsBundleBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "the body is over 64 KiB")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body skillsBundleBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.More() {
		msg := "the body must be one JSON object {skills, reason}"
		if err != nil {
			msg += ": " + err.Error()
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "code": gateway.SkillsErrInvalid})
		return
	}
	refs := make([]any, 0, len(body.Skills))
	for _, ref := range body.Skills {
		refs = append(refs, ref)
	}
	s.skillsCall(w, r, ag, sessionID, viaSession, gateway.SkillsBundleTool, map[string]any{"skills": refs}, body.Reason)
}

// GET /v1/skills/bundle/{ticket}
func (s *Server) skillsBundleDownload(w http.ResponseWriter, r *http.Request) {
	if !skillsBundleDownloadPath(r.URL.Path) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if !s.skillsReady(w) {
		return
	}
	ticket := strings.TrimPrefix(r.URL.Path, gateway.SkillBundlePathPrefix)
	b, err := s.gateway.OpenSkillBundle(r.Context(), ticket)
	switch {
	case errors.Is(err, gateway.ErrSkillBundleTicket):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error(), "code": gateway.SkillsErrNotFound})
		return
	case errors.Is(err, skillhub.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "a skill in this bundle is no longer one its requester can see; ask for a new bundle", "code": gateway.SkillsErrNotFound})
		return
	case errors.Is(err, skillhub.ErrDisabled):
		s.skillsReady(w)
		return
	case err != nil:
		logx.For("api").Error("skills bundle", "err", err.Error())
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := &bundleWriter{w: w}
	if err := b.Write(r.Context(), out); err != nil {
		logx.For("api").Error("skills bundle", "err", err.Error())
		if !out.started {
			writeError(w, http.StatusInternalServerError, "the bundle could not be built; ask for a new one")
		}
	}
}

// bundleWriter sends the download's headers with its first byte, so a
// bundle that fails before writing anything still answers with an error.
type bundleWriter struct {
	w       http.ResponseWriter
	started bool
}

func (b *bundleWriter) Write(p []byte) (int, error) {
	if !b.started {
		b.started = true
		h := b.w.Header()
		h.Set("Content-Type", "application/gzip")
		h.Set("Content-Disposition", `attachment; filename="skills.tar.gz"`)
		h.Set("Cache-Control", "no-store")
		b.w.WriteHeader(http.StatusOK)
	}
	return b.w.Write(p)
}
