package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/autoapproval"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// rangeFromQuery parses a ?range=24h|7d|30d|90d query into a metrics.Range.
// Defaults to 7d. We deliberately keep the set tiny so the dashboard can
// render time-pickers as a fixed segmented control.
func rangeFromQuery(r *http.Request) metrics.Range {
	q := strings.ToLower(r.URL.Query().Get("range"))
	now := time.Now()
	switch q {
	case "1h", "60m":
		return metrics.Range{From: now.Add(-1 * time.Hour), To: now}
	case "24h", "1d":
		return metrics.Range{From: now.Add(-24 * time.Hour), To: now}
	case "30d":
		return metrics.Range{From: now.Add(-30 * 24 * time.Hour), To: now}
	case "90d":
		return metrics.Range{From: now.Add(-90 * 24 * time.Hour), To: now}
	default:
		return metrics.Range{From: now.Add(-7 * 24 * time.Hour), To: now}
	}
}

func (s *Server) insightsOverview(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.metrics == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	o, err := s.metrics.Overview(r.Context(), rangeFromQuery(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// toolRowWithPolicy is the wire shape returned by /v1/insights/tools: the
// underlying metrics.ToolRow plus the two policy flags the dashboard needs
// to render the quick-switch toggle. The metrics row is embedded so existing
// JSON consumers see the same fields they always did.
type toolRowWithPolicy struct {
	metrics.ToolRow
	AutoApprove   bool `json:"auto_approve"`
	IsDestructive bool `json:"is_destructive"`
}

func (s *Server) insightsTools(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.metrics == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	rows, err := s.metrics.Tools(r.Context(), rangeFromQuery(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []metrics.ToolRow{}
	}
	var policies map[string]bool
	if s.autoApproval != nil {
		policies = s.autoApproval.ToolPolicies(r.Context())
	}
	out := make([]toolRowWithPolicy, 0, len(rows))
	for _, row := range rows {
		enriched := toolRowWithPolicy{ToolRow: row}
		if policies != nil {
			enriched.AutoApprove = policies[row.ToolName]
		}
		if s.autoApproval != nil {
			enriched.IsDestructive = s.autoApproval.IsDestructive(r.Context(), row.ToolName)
		}
		out = append(out, enriched)
	}
	writeJSON(w, http.StatusOK, out)
}

// insightsToolPolicy handles POST /v1/insights/tools/{tool_name}/policy with
// body {"auto_approve": bool}. It flips a kind=tool auto-approval rule for
// the tool: ON creates (or re-enables) a rule, OFF disables every enabled
// rule of that kind. Destructive tools are accepted but the response carries
// a `destructive_veto` flag so the UI can warn the user that auto-approval
// will still block at decision time.
func (s *Server) insightsToolPolicy(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.autoApproval == nil {
		writeError(w, http.StatusServiceUnavailable, "auto-approval not wired")
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/v1/insights/tools/")
	if !strings.HasSuffix(tail, "/policy") {
		http.NotFound(w, r)
		return
	}
	tool := strings.TrimSuffix(tail, "/policy")
	if tool == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		// Mode is the 5-way control: default | auto | allow | ask | deny.
		// Empty falls back to the legacy auto_approve bool for back-compat.
		Mode        string `json:"mode"`
		AutoApprove bool   `json:"auto_approve"`
		Force       bool   `json:"force"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	mode := body.Mode
	if mode == "" {
		if body.AutoApprove {
			mode = "auto"
		} else {
			mode = "default"
		}
	}

	// Explicit allow/ask/deny require the policy engine.
	explicit := mode == "allow" || mode == "ask" || mode == "deny"
	if explicit && s.policy == nil {
		writeError(w, http.StatusServiceUnavailable, "policy engine not wired")
		return
	}

	ctx := r.Context()
	switch mode {
	case "default":
		// Heuristic only: clear any explicit policy AND any learned auto rule.
		if s.policy != nil {
			_ = s.policy.DeleteTarget(ctx, policy.ScopeTool, tool)
		}
		if err := s.autoApproval.SetToolPolicy(ctx, tool, false); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	case "auto":
		// Learned auto-approve (still vetoed/rate-limited inside the pipeline).
		// Clear any explicit policy so it doesn't shadow the learned rule.
		if s.policy != nil {
			_ = s.policy.DeleteTarget(ctx, policy.ScopeTool, tool)
		}
		if err := s.autoApproval.SetToolPolicyBy(ctx, tool, true, uid); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	case "allow", "ask", "deny":
		if _, err := s.policy.Set(ctx, policy.ScopeTool, tool, mode, "", body.Force); err != nil {
			if errors.Is(err, policy.ErrForceRequired) {
				writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "needs_force": true})
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Anti-fight: any explicit policy disables a learned auto rule for it.
		_ = s.autoApproval.SetToolPolicy(ctx, tool, false)
	default:
		writeError(w, http.StatusBadRequest, "mode must be default, auto, allow, ask, or deny")
		return
	}

	resp := map[string]any{
		"ok":        true,
		"tool_name": tool,
		"mode":      mode,
		// auto_approve kept for back-compat with older dashboard builds.
		"auto_approve": mode == "auto",
	}
	if (mode == "auto" || mode == "allow") && s.autoApproval.IsDestructive(ctx, tool) {
		resp["destructive_veto"] = mode == "auto" // allow with force bypasses the veto intentionally
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) insightsAgents(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.metrics == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	rows, err := s.metrics.Agents(r.Context(), rangeFromQuery(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []metrics.AgentRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// insightsAgentDetail handles /v1/insights/agents/{id} — returns the agent's
// rollup row plus its hour-of-day heatmap for the chosen range.
func (s *Server) insightsAgentDetail(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/insights/agents/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if s.metrics == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	heat, err := s.metrics.AgentHeatmap(r.Context(), id, 14)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if heat == nil {
		heat = []metrics.HourBucket{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": id,
		"heatmap":  heat,
	})
}

func (s *Server) insightsAnomalies(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.metrics == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	includeDismissed := r.URL.Query().Get("include_dismissed") == "1"
	out, err := s.metrics.Anomalies(r.Context(), limit, includeDismissed)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out == nil {
		out = []metrics.AnomalyEvent{}
	}
	writeJSON(w, http.StatusOK, out)
}

// insightsAnomalyAction handles /v1/insights/anomalies/{id}/dismiss.
func (s *Server) insightsAnomalyAction(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/v1/insights/anomalies/")
	if !strings.HasSuffix(tail, "/dismiss") {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSuffix(tail, "/dismiss")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.metrics == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if err := s.metrics.DismissAnomaly(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) insightsCost(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	rows := []metrics.CostRow{}
	if s.metrics != nil {
		var err error
		rows, err = s.metrics.Cost(r.Context(), rangeFromQuery(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if rows == nil {
			rows = []metrics.CostRow{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":       rows,
		"status":     "unmetered",
		"billed_usd": nil,
		"reason":     "Tool calls do not include the client model or provider token usage. Payload estimates cannot measure billed spend.",
	})
}

// insightsPricing reads the local snapshot, never a remote URL on a request.
// The source is fixed and the catalog refreshes in the background.
func (s *Server) insightsPricing(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	limit := 30
	if r.URL.Query().Has("limit") {
		n, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || n < 0 || n > 100 {
			writeError(w, http.StatusBadRequest, "limit must be from 0 to 100")
			return
		}
		limit = n
	}
	offset := 0
	if r.URL.Query().Has("offset") {
		n, err := strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be zero or more")
			return
		}
		offset = n
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	writeJSON(w, http.StatusOK, s.pricing.View(r.URL.Query().Get("provider"), r.URL.Query().Get("q"), offset, limit))
}

// autoRulesCollection handles GET (list all rules) and POST (operator-created
// static rule).
func (s *Server) autoRulesCollection(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.autoApproval == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	switch r.Method {
	case http.MethodGet:
		out, err := s.autoApproval.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if out == nil {
			out = []autoapproval.Rule{}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var body autoapproval.Rule
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.Kind == "" {
			body.Kind = "static"
		}
		body.Source = "user"
		body.Enabled = true
		body.CreatedBy = uid
		body.EnabledBy = uid
		out, err := s.autoApproval.CreateOrUpdate(r.Context(), body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

// autoRulesItem handles per-rule actions: enable, disable, delete.
//
//	POST /v1/insights/auto/rules/{id}/enable
//	POST /v1/insights/auto/rules/{id}/disable
//	DELETE /v1/insights/auto/rules/{id}
func (s *Server) autoRulesItem(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.autoApproval == nil {
		writeError(w, http.StatusServiceUnavailable, "auto-approval not wired")
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/v1/insights/auto/rules/")
	parts := strings.SplitN(tail, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}
	switch {
	case subpath == "enable" && r.Method == http.MethodPost:
		if err := s.autoApproval.Enable(r.Context(), id, uid); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case subpath == "disable" && r.Method == http.MethodPost:
		if err := s.autoApproval.Disable(r.Context(), id); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case subpath == "" && r.Method == http.MethodDelete:
		if err := s.autoApproval.Delete(r.Context(), id); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case subpath == "" && r.Method == http.MethodGet:
		out, err := s.autoApproval.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	default:
		writeError(w, http.StatusMethodNotAllowed, "POST {enable,disable} or DELETE")
	}
}

// insightsExport streams a CSV dump of call_events for the supplied range.
// CSV is the universal interchange format — DuckDB / pandas / SQLite all
// load it without coercion.
func (s *Server) insightsExport(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.metrics == nil {
		writeError(w, http.StatusServiceUnavailable, "metrics not wired")
		return
	}
	rg := rangeFromQuery(r)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="toolyard-call-events.csv"`)
	if err := s.metrics.ExportCSV(r.Context(), w, rg); err != nil {
		// Headers may already be written; just log and bail.
		return
	}
}

// insightsPurgeAgent removes all metrics rows for the named agent. Privacy
// "forget this agent" affordance.
func (s *Server) insightsPurgeAgent(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.metrics == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.metrics.PurgeAgent(r.Context(), body.AgentID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
