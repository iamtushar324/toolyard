package api

import (
	"encoding/json"
	"net/http"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
)

// Handoff sources recorded on the dashboard.handoff_opened audit event.
const (
	handoffSourceLocal      = "local"
	handoffSourceHost       = "host"
	handoffSourceFederation = "federation"
)

// openHandoff finishes a dashboard handoff link after its single-use code has
// been consumed. Handoff links open the Inbox; they never sign a browser in:
// no cookie is set and no sessions row is created. A browser that already has
// a session lands in the Inbox; a browser without one sees the dashboard's
// normal sign-in. Only issueSession creates a session, and only after a person
// has proved who they are.
func (s *Server) openHandoff(w http.ResponseWriter, r *http.Request, userID, source string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if s.audit != nil {
		args, _ := json.Marshal(map[string]any{"source": source, "session_issued": false})
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType: "dashboard.handoff_opened", AgentID: "user:" + userID,
			Reason: source, ResultSummary: "no session issued", Arguments: args,
			Raiser: actor.Raiser{OwnerUserID: userID, ClientIP: s.security.ClientIP(r)},
		})
	}
	http.Redirect(w, r, "/#inbox", http.StatusSeeOther)
}
