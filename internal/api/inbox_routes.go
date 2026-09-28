package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/docs"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// Owner-facing inbox routes (session cookie) plus the guide, which agents
// without MCP read with their bearer token.
//
//	GET  /v1/inbox?view=open|done|all          list
//	GET  /v1/inbox/{id}                        one request, with its grants
//	POST /v1/inbox/{id}/decide                 approve | deny | return | answer | snooze | read
//	POST /v1/inbox/{id}/summarize              toolyard's own summary (on request)
//	POST /v1/inbox/{id}/explain                toolyard's reading of one tool call
//	GET  /v1/inbox/sessions                    agent sessions + live grants
//	GET  /v1/inbox/grants?status=active        grants
//	POST /v1/inbox/grants/{id}/revoke          revoke one grant
//	POST /v1/inbox/grants/revoke-all           kill switch
//	GET  /v1/inbox/blobs/{sha256}              a copied attachment
//	GET  /v1/guide[?topic=]                    the agent protocol (bearer or session)
//	GET  /v1/guide/skill                       the toolyard-inbox SKILL.md
func (s *Server) inboxRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/inbox", s.inboxList)
	mux.HandleFunc("/v1/inbox/", s.inboxItem)
	mux.HandleFunc("/v1/guide", s.guideHandler)
	mux.HandleFunc("/v1/guide/skill", s.guideSkill)
}

// inboxCard is a request plus the names the dashboard shows next to it.
type inboxCard struct {
	*inbox.Request
	AttachmentCount int    `json:"attachment_count"`
	AgentName       string `json:"agent_name"`
	SessionTitle    string `json:"session_title,omitempty"`
	SessionRepo     string `json:"session_repo,omitempty"`
	SessionHost     string `json:"session_host,omitempty"`
}

func (s *Server) agentNames(ctx context.Context, uid string) map[string]string {
	out := map[string]string{}
	if s.identity == nil {
		return out
	}
	if ags, err := s.identity.ListAgents(ctx, uid); err == nil {
		for _, a := range ags {
			out[a.ID] = a.Name
		}
	}
	return out
}

func (s *Server) cardFor(ctx context.Context, r *inbox.Request, names map[string]string, sessions map[string]*inbox.Session) inboxCard {
	c := inboxCard{Request: r, AttachmentCount: len(r.Attachments), AgentName: names[r.AgentID]}
	if c.AgentName == "" {
		c.AgentName = "agent " + shortAgent(r.AgentID)
	}
	if r.SessionID != "" {
		ss, ok := sessions[r.SessionID]
		if !ok {
			ss, _ = s.inbox.GetSession(ctx, r.SessionID)
			sessions[r.SessionID] = ss
		}
		if ss != nil {
			c.SessionTitle, c.SessionRepo, c.SessionHost = ss.Title, ss.Repo, ss.Host
		}
	}
	return c
}

func shortAgent(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (s *Server) inboxList(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.inbox == nil {
		writeJSON(w, http.StatusOK, map[string]any{"requests": []any{}})
		return
	}
	f := inbox.ListFilter{Limit: 500}
	switch r.URL.Query().Get("view") {
	case "done":
		f.Closed, f.Limit = true, 100
	case "all":
		f.Limit = 200
	default:
		f.Open = true
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n < f.Limit {
		f.Limit = n
	}
	reqs, err := s.inbox.List(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	names := s.agentNames(r.Context(), uid)
	sessions := map[string]*inbox.Session{}
	out := make([]inboxCard, 0, len(reqs))
	for i := range reqs {
		c := s.cardFor(r.Context(), &reqs[i], names, sessions)
		// The list only needs what the cards show; attachments and the
		// timeline come with GET /v1/inbox/{id}.
		slim := *c.Request
		slim.Attachments, slim.Activity = nil, nil
		c.Request = &slim
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

func (s *Server) inboxItem(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.inbox == nil {
		writeError(w, http.StatusServiceUnavailable, "inbox not enabled")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/inbox/"), "/")
	parts := strings.Split(rest, "/")
	switch {
	case rest == "sessions":
		s.inboxSessions(w, r, uid)
	case rest == "grants":
		s.inboxGrants(w, r)
	case len(parts) == 2 && parts[0] == "grants" && parts[1] == "revoke-all":
		s.inboxRevokeAll(w, r)
	case len(parts) == 3 && parts[0] == "grants" && parts[2] == "revoke":
		s.inboxRevoke(w, r, parts[1])
	case len(parts) == 2 && parts[0] == "blobs":
		s.inboxBlob(w, r, parts[1])
	case len(parts) == 1 && parts[0] != "":
		s.inboxGet(w, r, uid, parts[0])
	case len(parts) == 2 && parts[1] == "decide":
		s.inboxDecide(w, r, uid, parts[0])
	case len(parts) == 2 && parts[1] == "summarize":
		s.inboxSummarize(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "explain":
		s.inboxExplain(w, r, parts[0])
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) inboxGet(w http.ResponseWriter, r *http.Request, uid, id string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	req, err := s.inbox.Get(r.Context(), id)
	if err != nil {
		writeInboxErr(w, err)
		return
	}
	grants, _ := s.inbox.ListGrants(r.Context(), "", req.AgentID, 200)
	var mine []inbox.Grant
	for _, g := range grants {
		if g.RequestID == req.ID {
			mine = append(mine, g)
		}
	}
	card := s.cardFor(r.Context(), req, s.agentNames(r.Context(), uid), map[string]*inbox.Session{})
	writeJSON(w, http.StatusOK, map[string]any{"request": card, "grants": mine})
}

func (s *Server) inboxDecide(w http.ResponseWriter, r *http.Request, uid, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body inbox.Decision
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.By = "owner"
	if u, err := s.identity.GetUserByID(r.Context(), uid); err == nil && u != nil {
		body.By = u.Username
	}
	req, err := s.inbox.Decide(r.Context(), id, body)
	if err != nil {
		writeInboxErr(w, err)
		return
	}
	if s.audit != nil {
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType:  "inbox.decide",
			AgentID:    req.AgentID,
			Decision:   body.Action,
			Reason:     body.Note,
			ApprovalID: req.ID,
		})
	}
	card := s.cardFor(r.Context(), req, s.agentNames(r.Context(), uid), map[string]*inbox.Session{})
	writeJSON(w, http.StatusOK, map[string]any{"request": card})
}

func (s *Server) inboxSummarize(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	text, src, err := s.inbox.Summarize(r.Context(), id)
	if err != nil {
		writeInboxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "source": src})
}

func (s *Server) inboxExplain(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		ToolIndex int `json:"tool_index"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	text, src, err := s.inbox.Explain(r.Context(), id, body.ToolIndex)
	if err != nil {
		writeInboxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "source": src})
}

// sessionCard is a session plus what the Sessions view needs.
type sessionCard struct {
	inbox.Session
	AgentName   string `json:"agent_name"`
	OpenCount   int    `json:"open_count"`
	Blocking    int    `json:"blocking_count"` // open questions/blockers
	Stale       bool   `json:"stale"`
	Coached24h  int    `json:"coached_24h"`
	LiveGrants  int    `json:"live_grants"`
	DerivedFrom string `json:"derived_status"`
}

const staleAfter = 15 * time.Minute

func (s *Server) inboxSessions(w http.ResponseWriter, r *http.Request, uid string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	ctx := r.Context()
	names := s.agentNames(ctx, uid)
	sessions, err := s.inbox.ListSessions(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	open, _ := s.inbox.List(ctx, inbox.ListFilter{Open: true, Limit: 500})
	grants, _ := s.inbox.ListGrants(ctx, inbox.GrantActive, "", 500)
	coached := map[string]int{}
	if s.audit != nil {
		evs, _ := s.audit.Query(ctx, audit.Filter{EventType: "call.coached", Since: time.Now().Add(-24 * time.Hour).UnixMilli(), Limit: 1000})
		for _, e := range evs {
			coached[e.AgentID]++
		}
	}
	grantsByAgent := map[string]int{}
	for _, g := range grants {
		grantsByAgent[g.AgentID]++
	}
	now := time.Now().UnixMilli()
	out := make([]sessionCard, 0, len(sessions))
	for _, ss := range sessions {
		c := sessionCard{Session: ss, AgentName: names[ss.AgentID], Coached24h: coached[ss.AgentID], LiveGrants: grantsByAgent[ss.AgentID]}
		if c.AgentName == "" {
			c.AgentName = "agent " + shortAgent(ss.AgentID)
		}
		for _, q := range open {
			if q.SessionID == ss.ID && q.Kind != inbox.KindUpdate {
				c.OpenCount++
				if q.Kind == inbox.KindQuestion || q.Kind == inbox.KindBlocker {
					c.Blocking++
				}
			}
		}
		c.Stale = ss.EndedAt == 0 && now-ss.LastHeartbeatAt > staleAfter.Milliseconds()
		switch {
		case ss.Status == inbox.SessionDone:
			c.DerivedFrom = "done"
		case c.Blocking > 0 || ss.Status == inbox.SessionBlockedOnOwner:
			c.DerivedFrom = "blocked"
		case c.OpenCount > 0 || ss.Status == inbox.SessionWaitingOnOwner:
			c.DerivedFrom = "waiting"
		case c.Stale:
			c.DerivedFrom = "stale"
		default:
			c.DerivedFrom = "working"
		}
		out = append(out, c)
	}
	rank := map[string]int{"blocked": 0, "waiting": 1, "stale": 2, "working": 3, "done": 4}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].DerivedFrom] < rank[out[j].DerivedFrom] })
	type grantCard struct {
		inbox.Grant
		AgentName string `json:"agent_name"`
	}
	gc := make([]grantCard, 0, len(grants))
	for _, g := range grants {
		n := names[g.AgentID]
		if n == "" {
			n = "agent " + shortAgent(g.AgentID)
		}
		gc = append(gc, grantCard{Grant: g, AgentName: n})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out, "grants": gc})
}

func (s *Server) inboxGrants(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	gs, err := s.inbox.ListGrants(r.Context(), r.URL.Query().Get("status"), r.URL.Query().Get("agent_id"), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if gs == nil {
		gs = []inbox.Grant{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": gs})
}

func (s *Server) inboxRevoke(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if err := s.inbox.RevokeGrant(r.Context(), id, "You"); err != nil {
		writeInboxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) inboxRevokeAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
	}
	if r.ContentLength > 0 {
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	n, err := s.inbox.RevokeAll(r.Context(), body.AgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}

func (s *Server) inboxBlob(w http.ResponseWriter, r *http.Request, sha string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.snapshots == nil {
		http.NotFound(w, r)
		return
	}
	f, ct, err := s.snapshots.Open(r.Context(), sha)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	// Only images and video render inline; everything else downloads.
	if !strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "video/") {
		name := r.URL.Query().Get("name")
		if name == "" || strings.ContainsAny(name, "\"\r\n/\\") {
			name = sha[:12]
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; media-src 'self'; sandbox")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func writeInboxErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, inbox.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, inbox.ErrNotPending):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, inbox.ErrRequiredRefused), errors.Is(err, inbox.ErrNothingAllowed), errors.Is(err, inbox.ErrBadDecision):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// guideHandler serves the agent protocol to agents with a bearer token (or
// the owner's session).
func (s *Server) guideHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if !s.agentOrUser(w, r) {
		return
	}
	text := docs.AgentProtocol
	if s.guide != nil {
		t, ok := s.guide.Topic(r.URL.Query().Get("topic"))
		if !ok {
			writeError(w, http.StatusNotFound, "unknown topic; try one of: "+strings.Join(s.guide.Topics(), ", "))
			return
		}
		text = t
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(text))
}

func (s *Server) guideSkill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if !s.agentOrUser(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(docs.InboxSkill)))
	_, _ = w.Write([]byte(docs.InboxSkill))
}

func (s *Server) agentOrUser(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		_, ok := s.requireAgent(w, r)
		return ok
	}
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "bearer token or session required")
		return false
	}
	return true
}
