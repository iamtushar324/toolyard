package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// Daily tool caps (TEC-2060): the admin routes that cap successful calls
// of a tool per caller and in total, per UTC day (gateway/tool_caps.go,
// internal/toolcaps). Admin only: RoleGuard keeps members and guests off,
// and every handler checks requireAdmin too. An operator token needs the
// owner scope to change them (operatorOwnerPrefixes).
//
//	GET    /v1/tool-caps          -> {"day","reset_at","caps":[toolCapView]}
//	POST   /v1/tool-caps          {"tool","per_caller_daily"?,"total_daily"?,"note"?}
//	                              create or replace the cap on tool -> toolCapView
//	DELETE /v1/tool-caps/{tool}   remove it (today's counters stay)
//
// Every change writes a tool_cap.change audit row (decision applied or
// cleared).

const toolCapsPath = "/v1/tool-caps"

// toolCapView is a cap with today's use: the total and each caller's.
type toolCapView struct {
	gateway.ToolCap
	UsedTodayTotal int64           `json:"used_today_total"`
	UsedToday      []toolCapCaller `json:"used_today"`
}

// toolCapCaller is one caller's successful calls today. Caller is
// "user:<id>" for a person (then UserID and Username name them), else the
// gateway caller id.
type toolCapCaller struct {
	Caller   string `json:"caller"`
	UserID   string `json:"user_id,omitempty"`
	Username string `json:"username,omitempty"`
	Guest    bool   `json:"guest,omitempty"`
	Calls    int64  `json:"calls"`
}

func (s *Server) toolCaps() gateway.ToolCaps {
	if s.gateway == nil {
		return nil
	}
	return s.gateway.ToolCaps()
}

func (s *Server) toolCapsCollection(w http.ResponseWriter, r *http.Request) {
	caller, err := s.requireAdmin(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	c := s.toolCaps()
	if c == nil {
		writeError(w, http.StatusServiceUnavailable, "tool caps not wired")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.toolCapsList(w, r, c)
	case http.MethodPost:
		s.toolCapsPut(w, r, c, caller)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func (s *Server) toolCapsItem(w http.ResponseWriter, r *http.Request) {
	caller, err := s.requireAdmin(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	c := s.toolCaps()
	if c == nil {
		writeError(w, http.StatusServiceUnavailable, "tool caps not wired")
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "DELETE only")
		return
	}
	// r.URL.Path is decoded once already; the client escapes the tool
	// name once (encodeURIComponent).
	tool := strings.TrimPrefix(r.URL.Path, toolCapsPath+"/")
	if tool == "" || strings.Contains(tool, "/") {
		writeError(w, http.StatusBadRequest, "the path must be /v1/tool-caps/{tool}")
		return
	}
	ctx := r.Context()
	caps, err := c.List(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, cp := range caps {
		if cp.Tool != tool {
			continue
		}
		if _, err := c.Clear(ctx, cp.Upstream, cp.Tool); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = s.audit.Write(ctx, audit.Event{
			EventType: gateway.EventToolCapChange, AgentID: "user:" + caller.ID, UpstreamName: cp.Upstream, ToolName: cp.Tool,
			Decision: "cleared", ResultSummary: "cap removed",
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tool": cp.Tool, "upstream": cp.Upstream})
		return
	}
	writeError(w, http.StatusNotFound, "no cap on that tool")
}

func (s *Server) toolCapsList(w http.ResponseWriter, r *http.Request, c gateway.ToolCaps) {
	ctx := r.Context()
	caps, err := c.List(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	day, reset := c.Today()
	usage, err := c.Usage(ctx, day)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	users := map[string]identity.User{}
	if s.identity != nil {
		if list, err := s.identity.ListUsers(ctx); err == nil {
			for _, u := range list {
				users[u.ID] = u.User
			}
		}
	}
	type key struct{ upstream, tool string }
	byTool := map[key]*toolCapView{}
	out := make([]*toolCapView, 0, len(caps))
	for _, cp := range caps {
		v := &toolCapView{ToolCap: cp, UsedToday: []toolCapCaller{}}
		byTool[key{cp.Upstream, cp.Tool}] = v
		out = append(out, v)
	}
	for _, u := range usage {
		v, ok := byTool[key{u.Upstream, u.Tool}]
		if !ok {
			continue
		}
		if u.Caller == gateway.CapTotalCaller {
			v.UsedTodayTotal = u.Calls
			continue
		}
		if u.Calls == 0 {
			continue
		}
		row := toolCapCaller{Caller: u.Caller, Calls: u.Calls}
		if uid, ok := strings.CutPrefix(u.Caller, "user:"); ok {
			row.UserID = uid
			if us, ok := users[uid]; ok {
				row.Username = us.Username
				row.Guest = us.Role == identity.RoleGuest
			}
		}
		v.UsedToday = append(v.UsedToday, row)
	}
	for _, v := range out {
		sort.SliceStable(v.UsedToday, func(i, j int) bool { return v.UsedToday[i].Calls > v.UsedToday[j].Calls })
	}
	writeJSON(w, http.StatusOK, map[string]any{"day": day, "reset_at": reset.UTC().Format(time.RFC3339), "caps": out})
}

func (s *Server) toolCapsPut(w http.ResponseWriter, r *http.Request, c gateway.ToolCaps, caller *identity.User) {
	var body struct {
		Tool           string `json:"tool"`
		PerCallerDaily int64  `json:"per_caller_daily"`
		TotalDaily     int64  `json:"total_daily"`
		Note           string `json:"note"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	tool := strings.TrimSpace(body.Tool)
	if tool == "" {
		writeError(w, http.StatusBadRequest, "tool is required (the full name tools/list shows)")
		return
	}
	upstream := ""
	if s.gateway != nil {
		for _, e := range s.gateway.Catalog() {
			if e.Name == tool {
				upstream = e.Upstream
				break
			}
		}
	}
	if upstream == "" {
		writeError(w, http.StatusBadRequest, "unknown tool: "+tool+" (use the full name tools/list shows; the server must be connected)")
		return
	}
	saved, err := c.Set(ctx, gateway.ToolCap{Upstream: upstream, Tool: tool, PerCallerDaily: body.PerCallerDaily, TotalDaily: body.TotalDaily, Note: body.Note}, caller.ID)
	if errors.Is(err, gateway.ErrInvalidCap) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.audit.Write(ctx, audit.Event{
		EventType: gateway.EventToolCapChange, AgentID: "user:" + caller.ID, UpstreamName: saved.Upstream, ToolName: saved.Tool,
		Decision: "applied", ResultSummary: fmt.Sprintf("per_caller_daily=%d total_daily=%d", saved.PerCallerDaily, saved.TotalDaily),
	})
	writeJSON(w, http.StatusOK, toolCapView{ToolCap: saved, UsedToday: []toolCapCaller{}})
}
