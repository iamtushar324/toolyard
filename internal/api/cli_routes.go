package api

import (
	"net/http"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
)

// Routes registered here serve the `toolyard` CLI: a thin one-shot REST
// client that auths with the same long-lived agent token issued by the
// existing enrollment-code flow. Unlike the dashboard's /v1/tools and
// /v1/tools/run (session-cookie auth), these accept Authorization: Bearer
// <agent-token> and return JSON the CLI can render directly.

func (s *Server) cliRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/agents/tools", s.cliToolsList)
	mux.HandleFunc("/v1/agents/tools/run", s.cliToolsRun)
	mux.HandleFunc("/v1/agents/whoami", s.cliWhoami)
	mux.HandleFunc("/v1/agents/approvals/", s.cliApprovalsOne)
}

// GET /v1/agents/whoami — handy for `toolyard auth status`. Returns the
// authenticated agent's identity so the CLI can confirm the token is
// live before doing anything else.
func (s *Server) cliWhoami(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		writeError(w, http.StatusUnauthorized, "bearer token required")
		return
	}
	ag, err := s.verifyAgentToken(r.Context(), strings.TrimPrefix(authz, "Bearer "))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid agent token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": ag.ID,
		"name":     ag.Name,
	})
}

// GET /v1/agents/tools — bearer-auth catalog listing for the CLI, limited
// to the tool groups the agent's owner may use.
func (s *Server) cliToolsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	agentID, ok := s.requireAgent(w, r)
	if !ok {
		return
	}
	if s.gateway == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	ctx := gateway.WithAgentID(r.Context(), agentID)
	writeJSON(w, http.StatusOK, s.gateway.CatalogFor(ctx))
}

// GET /v1/agents/approvals/{id} — bearer-auth approval status polling
// for the CLI. The agent can only see its own approvals; a mismatched
// agent_id returns 404 (same shape as "no such id") so callers can't
// enumerate other agents' IDs.
func (s *Server) cliApprovalsOne(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	agentID, ok := s.requireAgent(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/agents/approvals/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	req, err := s.approval.Get(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if req.AgentID != "" && req.AgentID != agentID {
		http.NotFound(w, r)
		return
	}
	// The decision token is the human's one-tap approval credential and
	// decide-by-token is unauthenticated: never hand it to the agent.
	out := *req
	out.DecisionToken = ""
	writeJSON(w, http.StatusOK, out)
}

// POST /v1/agents/tools/run — bearer-auth one-shot tool execution.
//
// Body: {"tool":"<name>", "arguments":{...}}
//
// Returns the CallToolResult shape directly so the CLI can pretty-print
// or pipe the structured content. Deferred (approval-required) responses
// surface unchanged — the CLI handles the deferred envelope by polling
// /v1/approvals/<id> with the same bearer.
func (s *Server) cliToolsRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	ag, ok := s.requireAgentFull(w, r)
	if !ok {
		return
	}
	if s.gateway == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway not wired")
		return
	}
	var body struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Tool) == "" {
		writeError(w, http.StatusBadRequest, "tool is required")
		return
	}
	if body.Arguments == nil {
		body.Arguments = map[string]any{}
	}
	// The raiser (agent, owner, client) rides on ctx: the call.cli row
	// below and every row the gateway writes for this call carry it.
	ctx := actor.WithRaiser(gateway.WithAgentID(r.Context(), ag.ID), s.agentRaiser(r, ag, clientKindCLI, viaCLI))
	_ = s.audit.Write(ctx, audit.Event{
		EventType:     "call.cli",
		AgentID:       ag.ID,
		ToolName:      body.Tool,
		ResultSummary: "cli:" + body.Tool,
	})
	res, err := s.gateway.RouteCall(ctx, viaCLI, body.Tool, body.Arguments)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{
		"is_error":           res.IsError,
		"content":            res.Content,
		"structured_content": res.StructuredContent,
	}
	if res.Meta != nil {
		out["_meta"] = res.Meta.AdditionalFields
	}
	writeJSON(w, http.StatusOK, out)
}
