package api

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
)

func qInt(r *http.Request, key string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return v
}

// auditFilterFromQuery reads the shared audit filter params.
func auditFilterFromQuery(r *http.Request) audit.Filter {
	q := r.URL.Query()
	return audit.Filter{
		Since:     qInt(r, "since"),
		Until:     qInt(r, "until"),
		Before:    qInt(r, "before"),
		AgentID:   q.Get("agent_id"),
		EventType: q.Get("event_type"),
		Tool:      q.Get("tool"),
		Decision:  q.Get("decision"),
	}
}

// auditExport streams the filtered audit log as CSV (default) or JSON.
func (s *Server) auditExport(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	f := auditFilterFromQuery(r)
	rows, err := s.audit.Query(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="toolyard-audit.json"`)
		_ = json.NewEncoder(w).Encode(rows)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="toolyard-audit.csv"`)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{"id", "ts_iso", "event_type", "agent_id", "upstream", "tool", "decision", "reason", "result_summary", "approval_id"})
	for _, e := range rows {
		_ = cw.Write([]string{
			e.ID, time.UnixMilli(e.TS).UTC().Format(time.RFC3339), e.EventType,
			e.AgentID, e.UpstreamName, e.ToolName, e.Decision, e.Reason, e.ResultSummary, e.ApprovalID,
		})
	}
}

// approvalsExport streams approval rows as CSV (default) or JSON. The
// decision_token (a capability token) is always excluded.
func (s *Server) approvalsExport(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rows, err := s.approval.Export(r.Context(), qInt(r, "since"), qInt(r, "until"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.URL.Query().Get("format") == "json" {
		// Re-marshal through a token-free view so decision_token never leaks.
		type safeApproval struct {
			ID             string `json:"id"`
			AgentID        string `json:"agent_id"`
			UpstreamName   string `json:"upstream_name"`
			ToolName       string `json:"tool_name"`
			Status         string `json:"status"`
			IntentCategory string `json:"intent_category,omitempty"`
			Reason         string `json:"reason,omitempty"`
			DecidedBy      string `json:"decided_by,omitempty"`
			CreatedAt      int64  `json:"created_at"`
			DecidedAt      int64  `json:"decided_at,omitempty"`
			ExpiresAt      int64  `json:"expires_at"`
		}
		out := make([]safeApproval, 0, len(rows))
		for _, a := range rows {
			out = append(out, safeApproval{
				ID: a.ID, AgentID: a.AgentID, UpstreamName: a.UpstreamName, ToolName: a.ToolName,
				Status: a.Status, IntentCategory: a.IntentCategory, Reason: a.Reason,
				DecidedBy: a.DecidedBy, CreatedAt: a.CreatedAt, DecidedAt: a.DecidedAt, ExpiresAt: a.ExpiresAt,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="toolyard-approvals.json"`)
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="toolyard-approvals.csv"`)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{"id", "created_iso", "agent_id", "upstream", "tool", "status", "intent_category", "reason", "decided_by", "decided_iso", "expires_iso"})
	iso := func(ms int64) string {
		if ms == 0 {
			return ""
		}
		return time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	for _, a := range rows {
		_ = cw.Write([]string{
			a.ID, iso(a.CreatedAt), a.AgentID, a.UpstreamName, a.ToolName, a.Status,
			a.IntentCategory, a.Reason, a.DecidedBy, iso(a.DecidedAt), iso(a.ExpiresAt),
		})
	}
}

// memoryBackup is the versioned envelope for memory export/import.
type memoryBackup struct {
	Version    int            `json:"version"`
	ExportedAt int64          `json:"exported_at,omitempty"`
	Mode       string         `json:"mode,omitempty"` // import only: merge | replace
	Entries    []memory.Entry `json:"entries"`
}

// memoryExport returns the full memory store as a versioned JSON document.
func (s *Server) memoryExport(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.memory == nil {
		writeError(w, http.StatusServiceUnavailable, "memory not wired")
		return
	}
	entries, err := s.memory.ExportAll(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="toolyard-memory.json"`)
	_ = json.NewEncoder(w).Encode(memoryBackup{Version: 1, Entries: entries})
}

// memoryImport upserts a memory backup. mode=merge (default) keeps existing
// entries; mode=replace wipes first. Applied in one transaction.
func (s *Server) memoryImport(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.memory == nil {
		writeError(w, http.StatusServiceUnavailable, "memory not wired")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var doc memoryBackup
	if err := decode(r, &doc); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	mode := doc.Mode
	if mode == "" {
		mode = r.URL.Query().Get("mode")
	}
	if mode != "" && mode != "merge" && mode != "replace" {
		writeError(w, http.StatusBadRequest, "mode must be merge or replace")
		return
	}
	n, err := s.memory.Import(r.Context(), doc.Entries, mode == "replace")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": n, "mode": mode})
}
