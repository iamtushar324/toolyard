package api

// actors.go names the people behind API requests: the signed-in
// dashboard user as decider (approvals, inbox, grants) or raiser (the
// workbench), and the bearer agent with its owner as raiser (the CLI, a
// hook script), so every audit row, approval and inbox decision records
// who did it and how.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// Raiser.ClientKind and Via values the REST entry points record.
const (
	clientKindBrowser = "browser"
	clientKindCLI     = "cli"
	viaDashboard      = "dashboard"
	viaCLI            = "cli"
	viaHook           = "hook"
	// headerClientKind lets a REST client name itself (the CLI sends
	// "cli"): a known kind is recorded as is, anything else goes through
	// the gateway's clientInfo mapping (so "claude-code/1.2" still reads
	// claude_code, and a value we have never seen is "unknown").
	headerClientKind = "x-toolyard-client"
)

// knownClientKinds are the Raiser.ClientKind values a client may claim
// outright.
var knownClientKinds = map[string]bool{
	"t3": true, "claude_code": true, "codex": true, "cursor": true, "opencode": true, clientKindCLI: true, clientKindBrowser: true,
}

// declaredClientKind is the kind the request's x-toolyard-client header
// declares, or "" without one.
func declaredClientKind(r *http.Request) string {
	v := strings.ToLower(actor.Clean(r.Header.Get(headerClientKind)))
	switch {
	case v == "":
		return ""
	case knownClientKinds[v]:
		return v
	}
	return gateway.ClientKindOf(v)
}

// requireUserFull is requireUser returning the whole user, for handlers
// that record the person's email and name.
func (s *Server) requireUserFull(r *http.Request) (*identity.User, error) {
	u, ok := s.sessionUser(r)
	if !ok {
		return nil, errors.New("unauthorized")
	}
	return u, nil
}

// dashboardDecider is the signed-in user deciding through the dashboard.
// via says which route (dashboard, dashboard_batch, push_token with a
// session); Ref is their dashboard session id, the instrument that
// identified them.
func (s *Server) dashboardDecider(r *http.Request, u *identity.User, via string) actor.Decider {
	return actor.Decider{UserID: u.ID, Email: u.Email, Name: u.Label(), Via: via, Ref: s.currentSessionID(r)}
}

// batchDecider is dashboardDecider for one "Allow all / Deny all" click:
// Via dashboard_batch and one batch id as Ref on every row it decides.
func (s *Server) batchDecider(r *http.Request, u *identity.User) actor.Decider {
	d := s.dashboardDecider(r, u, actor.ViaDashboardBatch)
	d.Ref = "batch_" + uuid.NewString()
	return d
}

// dashboardRaiser is the signed-in user raising a call from the
// dashboard's workbench: caller "dashboard:<uid>", owner themselves,
// client their browser.
func (s *Server) dashboardRaiser(r *http.Request, u *identity.User) actor.Raiser {
	return actor.Raiser{
		CallerID:    "dashboard:" + u.ID,
		AgentKind:   viaDashboard,
		OwnerUserID: u.ID,
		OwnerEmail:  u.Email,
		OwnerName:   u.Label(),
		ClientIP:    s.security.ClientIP(r),
		ClientKind:  clientKindBrowser,
		Via:         viaDashboard,
	}
}

// agentRaiser is a bearer-token agent raising a call over REST: the
// agent, its owner, the client IP, the kind the client declared (else
// fallback) and the path in.
func (s *Server) agentRaiser(r *http.Request, ag *identity.Agent, fallbackKind, via string) actor.Raiser {
	kind := declaredClientKind(r)
	if kind == "" {
		kind = fallbackKind
	}
	return actor.Raiser{
		CallerID:    ag.ID,
		AgentName:   ag.Name,
		AgentKind:   ag.Kind,
		OwnerUserID: ag.Owner,
		OwnerEmail:  ag.OwnerEmail,
		OwnerName:   ag.OwnerName(),
		ClientIP:    s.security.ClientIP(r),
		ClientKind:  kind,
		Via:         via,
	}
}

// requireAgentFull resolves the bearer token to its agent, owner details
// included, writing the 401 itself when it can't.
func (s *Server) requireAgentFull(w http.ResponseWriter, r *http.Request) (*identity.Agent, bool) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		writeError(w, http.StatusUnauthorized, "bearer token required")
		return nil, false
	}
	ag, err := s.identity.VerifyAgentToken(r.Context(), strings.TrimPrefix(authz, "Bearer "))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid agent token")
		return nil, false
	}
	return ag, true
}
