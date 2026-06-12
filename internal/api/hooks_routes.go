package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/hooks"
)

func (s *Server) hooksIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.hooks == nil {
		writeError(w, http.StatusServiceUnavailable, "hooks service is disabled")
		return
	}
	agentID, ok := s.requireAgent(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, hooks.MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "hook payload too large")
		return
	}
	ev, err := s.hooks.IngestRaw(r.Context(), agentID, body, r.URL.Query().Get("source"))
	if err != nil {
		switch {
		case errors.Is(err, hooks.ErrInvalidJSON):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, hooks.ErrBodyTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	if s.audit != nil {
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType:     "hook.ingest",
			AgentID:       agentID,
			ToolName:      ev.EventName,
			ResultSummary: ev.Source + " " + ev.ID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"event_id":        ev.ID,
		"agent_id":        agentID,
		"memory_ingested": ev.MemoryIngested,
	})
}

func (s *Server) hooksEvents(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.hooks == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	rows, err := s.hooks.List(r.Context(), hookQueryFromRequest(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) hooksExport(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.hooks == nil {
		writeError(w, http.StatusServiceUnavailable, "hooks service is disabled")
		return
	}
	q := hookQueryFromRequest(r)
	q.Limit = 10000
	rows, err := s.hooks.List(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="toolyard-hook-events.json"`)
		_ = json.NewEncoder(w).Encode(rows)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="toolyard-hook-events.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "ts_iso", "agent_id", "source", "event_name", "session_id", "turn_id", "conversation_id", "tool_name", "cwd", "text", "memory_ingested"})
	for _, ev := range rows {
		_ = cw.Write([]string{
			ev.ID,
			time.UnixMilli(ev.TS).UTC().Format(time.RFC3339),
			ev.AgentID,
			ev.Source,
			ev.EventName,
			ev.SessionID,
			ev.TurnID,
			ev.ConversationID,
			ev.ToolName,
			ev.CWD,
			ev.Text,
			strconv.FormatBool(ev.MemoryIngested),
		})
	}
	cw.Flush()
}

func hookQueryFromRequest(r *http.Request) hooks.Query {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	return hooks.Query{
		AgentID:   q.Get("agent_id"),
		Source:    q.Get("source"),
		EventName: q.Get("event_name"),
		SessionID: q.Get("session_id"),
		Q:         q.Get("q"),
		Since:     qInt(r, "since"),
		Until:     qInt(r, "until"),
		Before:    qInt(r, "before"),
		Limit:     limit,
	}
}
