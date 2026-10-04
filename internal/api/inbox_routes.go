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
	"github.com/tusharbhardwaj/toolyard/internal/actor"
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
//	GET  /v1/inbox/info                        judge/voice availability, quiet hours, next digest
//	POST /v1/inbox/batch                       {ids, action: read|deny|snooze} (never approve)
//	POST /v1/inbox/decide-by-token             a notification action (signed token; no cookie)
//	GET  /v1/guide[?topic=]                    the agent protocol (bearer or session)
//	GET  /v1/guide/skill                       the toolyard-inbox SKILL.md
func (s *Server) inboxRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/inbox/voice-key", s.inboxVoiceKey)
	mux.HandleFunc("/v1/inbox", s.inboxList)
	mux.HandleFunc("/v1/inbox/", s.inboxItem)
	mux.HandleFunc("/v1/inbox/decide-by-token", s.inboxDecideByToken)
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
	// Import records retain their original owner after agent deletion.
	if s.inbox != nil {
		if legacy, err := s.inbox.LegacyAgentNames(ctx, uid); err == nil {
			for id, name := range legacy {
				if _, exists := out[id]; !exists {
					out[id] = name
				}
			}
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

func (s *Server) ownsInboxRequest(ctx context.Context, uid string, r *inbox.Request, names map[string]string) bool {
	if r.ExecutionMode == "legacy" {
		return s.inbox.OwnsLegacyRequest(ctx, r.ID, uid)
	}
	_, owned := names[r.AgentID]
	return owned
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
	names := s.agentNames(r.Context(), uid)
	f.AgentIDs = make([]string, 0, len(names))
	for id := range names {
		f.AgentIDs = append(f.AgentIDs, id)
	}
	reqs, err := s.inbox.List(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sessions := map[string]*inbox.Session{}
	out := make([]inboxCard, 0, len(reqs))
	for i := range reqs {
		if !s.ownsInboxRequest(r.Context(), uid, &reqs[i], names) {
			continue
		}
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
	owned := s.agentNames(r.Context(), uid)
	if len(parts) > 0 && parts[0] != "sessions" && parts[0] != "grants" && parts[0] != "info" && parts[0] != "batch" && parts[0] != "blobs" {
		req, e := s.inbox.Get(r.Context(), parts[0])
		if e != nil {
			writeInboxErr(w, inbox.ErrNotFound)
			return
		}
		if !s.ownsInboxRequest(r.Context(), uid, req, owned) {
			writeInboxErr(w, inbox.ErrNotFound)
			return
		}
	}
	if len(parts) >= 2 && parts[0] == "grants" && parts[1] != "revoke-all" {
		g, e := s.inbox.Grant(r.Context(), parts[1])
		if e != nil {
			writeInboxErr(w, inbox.ErrNotFound)
			return
		}
		if _, ok := owned[g.AgentID]; !ok {
			writeInboxErr(w, inbox.ErrNotFound)
			return
		}
	}
	if len(parts) == 2 && parts[0] == "blobs" {
		ok := false
		for aid := range owned {
			if s.inbox.BlobBelongsToAgent(r.Context(), aid, parts[1]) {
				ok = true
				break
			}
		}
		if !ok {
			writeInboxErr(w, inbox.ErrNotFound)
			return
		}
	}

	switch {
	case rest == "sessions":
		s.inboxSessions(w, r, uid)
	case rest == "grants":
		s.inboxGrants(w, r)
	case rest == "info":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		ids := make([]string, 0, len(owned))
		for id := range owned {
			ids = append(ids, id)
		}
		info := s.inbox.InfoForAgents(r.Context(), ids)
		pk := 0
		if s.passkeys != nil {
			if l, err := s.passkeys.List(r.Context(), uid); err == nil {
				pk = len(l)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"info": info, "passkeys": pk})
	case rest == "batch":
		s.inboxBatch(w, r, uid)
	case len(parts) == 2 && parts[0] == "grants" && parts[1] == "revoke-all":
		s.inboxRevokeAll(w, r)
	case len(parts) == 3 && parts[0] == "grants" && parts[2] == "revoke":
		s.inboxRevoke(w, r, parts[1])
	case len(parts) == 2 && parts[0] == "blobs":
		s.inboxBlob(w, r, parts[1])
	case len(parts) == 1 && parts[0] != "":
		s.inboxGet(w, r, uid, parts[0])
	case len(parts) == 4 && parts[1] == "callbacks" && parts[3] == "retry":
		if r.Method != http.MethodPost {
			writeError(w, 405, "POST only")
			return
		}
		req, e := s.inbox.Get(r.Context(), parts[0])
		if e != nil || s.callbacks == nil {
			writeInboxErr(w, inbox.ErrNotFound)
			return
		}
		events, e := s.callbacks.History(r.Context(), req.AgentID, req.ID)
		if e != nil {
			federationError(w, e)
			return
		}
		found := false
		for _, ev := range events {
			if ev.ID == parts[2] {
				found = true
				break
			}
		}
		if !found {
			writeInboxErr(w, inbox.ErrNotFound)
			return
		}
		if e = s.callbacks.Retry(r.Context(), req.AgentID, parts[2]); e != nil {
			federationError(w, e)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case len(parts) == 2 && parts[1] == "decide":
		s.inboxDecide(w, r, uid, parts[0])
	case len(parts) == 2 && parts[1] == "audio":
		s.inboxListenAudio(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "passkey":
		s.inboxPasskeyBegin(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "summarize":
		s.inboxSummarize(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "explain":
		s.inboxExplain(w, r, parts[0])
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) inboxListenAudio(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if operatorFromContext(r.Context()) != nil {
		writeError(w, http.StatusForbidden, "listen in the owner dashboard")
		return
	}
	if _, err := s.requireAdmin(r); err != nil {
		writeAuthError(w, err)
		return
	}
	audio, err := s.inbox.ListenAudio(r.Context(), id)
	if err != nil {
		writeInboxErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"audio": audio})
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
			g.Execution, _ = s.inbox.Execution(r.Context(), g.ID, req.AgentID)
			if g.Execution != nil {
				g.Execution.Result = nil
			}
			mine = append(mine, g)
		}
	}
	card := s.cardFor(r.Context(), req, s.agentNames(r.Context(), uid), map[string]*inbox.Session{})
	body := map[string]any{"request": card, "grants": mine}
	if s.callbacks != nil {
		history, e := s.callbacks.History(r.Context(), req.AgentID, req.ID)
		if e != nil {
			writeError(w, http.StatusInternalServerError, "callback history unavailable")
			return
		}
		body["callbacks"] = history
	}
	writeJSON(w, http.StatusOK, body)
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
	u, err := s.requireUserFull(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	body.By = u.Label()
	body.Decider = s.dashboardDecider(r, u, actor.ViaDashboard)
	req, err := s.inbox.Decide(r.Context(), id, body)
	if err != nil {
		writeInboxErr(w, err)
		return
	}
	if s.audit != nil && !req.Replayed {
		// The recorded decider: the person, and passkey when one signed
		// the decision.
		ev := audit.Event{
			EventType:  "inbox.decide",
			AgentID:    req.AgentID,
			Decision:   body.Action,
			Reason:     body.Note,
			ApprovalID: req.ID,
		}
		ev.SetDecider(req.Decider())
		_ = s.audit.Write(r.Context(), ev)
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
		if _, owned := names[ss.AgentID]; !owned {
			continue
		}
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
		if _, owned := names[g.AgentID]; !owned {
			continue
		}
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
	uid, e := s.requireUser(r)
	if e != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	owned := s.agentNames(r.Context(), uid)
	filtered := []inbox.Grant{}
	for _, g := range gs {
		if _, ok := owned[g.AgentID]; ok {
			filtered = append(filtered, g)
		}
	}
	gs = filtered
	writeJSON(w, http.StatusOK, map[string]any{"grants": gs})
}

func (s *Server) inboxRevoke(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	u, err := s.requireUserFull(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := s.inbox.RevokeGrantBy(r.Context(), id, s.dashboardDecider(r, u, actor.ViaDashboard)); err != nil {
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
	u, err := s.requireUserFull(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	owned := s.agentNames(r.Context(), u.ID)
	if body.AgentID != "" {
		if _, ok := owned[body.AgentID]; !ok {
			writeInboxErr(w, inbox.ErrNotFound)
			return
		}
		owned = map[string]string{body.AgentID: owned[body.AgentID]}
	}
	n := 0
	for aid := range owned {
		if aid == "" {
			continue
		}
		count, e := s.inbox.RevokeAllBy(r.Context(), aid, s.dashboardDecider(r, u, actor.ViaDashboard))
		if e != nil {
			writeError(w, 500, e.Error())
			return
		}
		n += count
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
	// Only images, video and audio render inline; everything else downloads.
	if !strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "video/") && !strings.HasPrefix(ct, "audio/") {
		name := r.URL.Query().Get("name")
		if name == "" || strings.ContainsAny(name, "\"\r\n/\\") {
			name = sha[:12]
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; media-src 'self'; sandbox")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// inboxBatch applies one action to many requests. Approving is never
// batched: each access request is read and decided on its own.
func (s *Server) inboxBatch(w http.ResponseWriter, r *http.Request, uid string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		IDs           []string `json:"ids"`
		Action        string   `json:"action"`
		Note          string   `json:"note"`
		SnoozeMinutes int      `json:"snooze_minutes"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch body.Action {
	case "read", "deny", "snooze":
	default:
		writeError(w, http.StatusBadRequest, "batch action must be read, deny or snooze")
		return
	}
	if len(body.IDs) == 0 || len(body.IDs) > 200 {
		writeError(w, http.StatusBadRequest, "ids: 1 to 200 request IDs")
		return
	}
	u, err := s.requireUserFull(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// One batch id on every request this click decides.
	decider := s.batchDecider(r, u)
	done, failed := 0, map[string]string{}
	owned := s.agentNames(r.Context(), uid)
	for _, id := range body.IDs {
		current, e := s.inbox.Get(r.Context(), id)
		if e != nil {
			failed[id] = "not found"
			continue
		}
		if !s.ownsInboxRequest(r.Context(), uid, current, owned) {
			failed[id] = "not found"
			continue
		}
		req, err := s.inbox.Decide(r.Context(), id, inbox.Decision{Action: body.Action, Note: body.Note, SnoozeMinutes: body.SnoozeMinutes, By: u.Label(), Decider: decider})
		if err != nil {
			failed[id] = err.Error()
			continue
		}
		done++
		if s.audit != nil {
			ev := audit.Event{EventType: "inbox.decide", AgentID: req.AgentID, Decision: body.Action, Reason: "batch", ApprovalID: req.ID}
			ev.SetDecider(req.Decider())
			_ = s.audit.Write(r.Context(), ev)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": done, "failed": failed})
}

// inboxDecideByToken handles a tap on a notification action. The signed
// token names one request and the actions its notification offered; none
// of them allow tools.
func (s *Server) inboxDecideByToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.inbox == nil {
		writeError(w, http.StatusServiceUnavailable, "inbox not enabled")
		return
	}
	var body struct {
		Token  string `json:"token"`
		Action string `json:"action"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := s.inbox.DecideByTap(r.Context(), body.Token, body.Action)
	if err != nil {
		if errors.Is(err, inbox.ErrTapToken) {
			writeError(w, http.StatusGone, err.Error())
			return
		}
		writeInboxErr(w, err)
		return
	}
	if s.audit != nil {
		// The token's bound recipient (or an anonymous push tap), as the
		// inbox recorded it.
		ev := audit.Event{
			EventType:  "inbox.decide",
			AgentID:    req.AgentID,
			Decision:   body.Action,
			Reason:     "notification",
			ApprovalID: req.ID,
		}
		ev.SetDecider(req.Decider())
		_ = s.audit.Write(r.Context(), ev)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": req.Status, "snoozed_until": req.SnoozedUntil, "answer": req.Answer})
}

func writeInboxErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, inbox.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, inbox.ErrExpired):
		writeError(w, http.StatusGone, err.Error())
	case errors.Is(err, inbox.ErrConflict), errors.Is(err, inbox.ErrNotPending):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, inbox.ErrPasskeyRequired):
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{"error": err.Error(), "code": "passkey_required"})
	case errors.Is(err, inbox.ErrPasskeyFailed):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "code": "passkey_failed"})
	case errors.Is(err, inbox.ErrRequiredRefused), errors.Is(err, inbox.ErrNothingAllowed), errors.Is(err, inbox.ErrBadDecision), errors.Is(err, inbox.ErrWiden):
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
	if _, isOp := operatorBearer(r); !isOp && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		_, ok := s.requireAgent(w, r)
		return ok
	}
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "bearer token or session required")
		return false
	}
	return true
}
