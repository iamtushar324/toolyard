package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// Connections: each person's own sign-in to the servers whose auth_mode is
// per_user. Members reach these (role_guard memberRoutes); nothing here
// ever returns a token.
//
//	GET    /v1/me/connections                 per_user servers the caller may use, with their state
//	POST   /v1/me/connections/{server}/begin  start the caller's sign-in -> {authorize_url}
//	DELETE /v1/me/connections/{server}        drop the caller's token (revoked at the provider when it can be)
//	GET    /v1/servers/{name}/connections     admin: who has connected the server, and in what state
func (s *Server) connectionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/me/connections", s.meConnections)
	mux.HandleFunc("/v1/me/connections/", s.meConnectionsItem)
}

// connectionView is one row of My connections.
type connectionView struct {
	Server    string `json:"server"`
	Transport string `json:"transport"`
	// Host is the server URL's host only: a URL can embed a key in its
	// path or query, and this list is for members.
	Host string `json:"host,omitempty"`
	// State: connected | needs_signin | expired | needs_reauth.
	State        string `json:"state"`
	AccountLabel string `json:"account_label,omitempty"`
	ConnectedAt  int64  `json:"connected_at,omitempty"`
	LastRefresh  int64  `json:"last_refresh_at,omitempty"`
	AccessExpiry int64  `json:"access_expires_at,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	// Ready: the server's OAuth client is registered, so Connect can start.
	// False means an admin still has to run discovery for the server.
	Ready bool `json:"ready"`
	// ServerStatus is the server's last_status ("ok", "waiting_signin",
	// "disabled", an error...), so the page can say when nobody has
	// connected yet.
	ServerStatus string `json:"server_status,omitempty"`
	Enabled      bool   `json:"enabled"`
	ToolCount    int    `json:"tool_count"`
}

// connectionUser resolves the caller and checks the feature is wired.
func (s *Server) connectionUser(w http.ResponseWriter, r *http.Request) (*identity.User, bool) {
	u, ok := s.sessionUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	if s.oauth == nil || s.upstreams == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth service not wired")
		return nil, false
	}
	return u, true
}

// perUserServersFor lists the per_user servers the user may use: every one
// for an admin, the granted ones for a member.
func (s *Server) perUserServersFor(ctx context.Context, u *identity.User) ([]upstreams.Server, error) {
	all, err := s.upstreams.List(ctx)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	if u.Role != identity.RoleAdmin {
		granted, err := s.userGroups(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		for _, g := range granted {
			allowed[g] = true
		}
	}
	var out []upstreams.Server
	for _, sv := range all {
		if sv.AuthMode != upstreams.AuthPerUser || sv.AssertionProfile != "" {
			continue
		}
		if u.Role == identity.RoleAdmin || allowed[sv.Name] {
			out = append(out, sv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// mayUseServer reports whether the user may connect sv: an admin always,
// a member when the server is granted to them.
func (s *Server) mayUseServer(ctx context.Context, u *identity.User, name string) (bool, error) {
	if u.Role == identity.RoleAdmin {
		return true, nil
	}
	granted, err := s.userGroups(ctx, u.ID)
	if err != nil {
		return false, err
	}
	for _, g := range granted {
		if g == name {
			return true, nil
		}
	}
	return false, nil
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// urlHost is the host of a server URL, or "" when it does not parse.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// meConnections — any signed-in user.
//
//	GET /v1/me/connections -> [connectionView]
func (s *Server) meConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	u, ok := s.connectionUser(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	servers, err := s.perUserServersFor(ctx, u)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	mine, err := s.oauth.UserConnections(ctx, u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]connectionView, 0, len(servers))
	for _, sv := range servers {
		ready, err := s.oauth.HasClient(ctx, sv.Name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		v := connectionView{
			Server: sv.Name, Transport: sv.Transport, Host: urlHost(sv.URL), State: oauth.ConnNeedsSignIn, Ready: ready,
			ServerStatus: sv.LastStatus, Enabled: sv.Enabled, ToolCount: sv.ToolCount,
		}
		if !sv.Enabled {
			v.ServerStatus = "disabled"
		}
		if s.gateway != nil {
			if c := s.gateway.UpstreamToolCount(sv.Name); c > 0 {
				v.ToolCount = c
			}
		}
		if c, ok := mine[sv.Name]; ok {
			v.State = c.State
			v.AccountLabel = c.AccountLabel
			v.ConnectedAt = ms(c.ConnectedAt)
			v.LastRefresh = ms(c.LastRefreshAt)
			v.AccessExpiry = ms(c.AccessExpiresAt)
			v.LastError = c.LastError
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// meConnectionsItem dispatches the per-server actions:
//
//	POST   /v1/me/connections/{server}/begin -> {"authorize_url","state","expires_in"}
//	DELETE /v1/me/connections/{server}       -> {"ok":true}
func (s *Server) meConnectionsItem(w http.ResponseWriter, r *http.Request) {
	u, ok := s.connectionUser(w, r)
	if !ok {
		return
	}
	name, sub, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/me/connections/"), "/")
	if name == "" {
		http.NotFound(w, r)
		return
	}
	switch {
	case sub == "begin" && r.Method == http.MethodPost:
		s.meConnectionBegin(w, r, u, name)
	case sub == "" && r.Method == http.MethodDelete:
		s.meConnectionDelete(w, r, u, name)
	default:
		writeError(w, http.StatusMethodNotAllowed, "POST /begin or DELETE")
	}
}

// meConnectionBegin starts the caller's own sign-in to a per_user server.
// The pending row is flagged per_user, so the callback stores the token on
// the caller's row and only if the caller's browser comes back.
func (s *Server) meConnectionBegin(w http.ResponseWriter, r *http.Request, u *identity.User, name string) {
	ctx := r.Context()
	sv, err := s.upstreams.Get(ctx, name)
	if err != nil {
		if errors.Is(err, upstreams.ErrNotFound) {
			writeError(w, http.StatusNotFound, "server not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	allowed, err := s.mayUseServer(ctx, u, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !allowed {
		// Same answer as an unknown server: a member learns nothing about
		// servers not granted to them.
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	if s.assertionOAuthRefused(w, r, name) {
		return
	}
	if sv.AuthMode != upstreams.AuthPerUser {
		writeError(w, http.StatusBadRequest, "this server uses one shared account; there is nothing to connect")
		return
	}
	if !sv.Enabled {
		writeError(w, http.StatusConflict, "this server is disabled")
		return
	}
	// The token is stored only for a browser the callback can tie to the
	// person (the flow cookie set below). An operator token is not a
	// browser, so there is nothing to bind the return to.
	if operatorFromContext(ctx) != nil {
		writeError(w, http.StatusConflict, "a personal sign-in has to be started from the dashboard's My connections page, in the browser that will finish it")
		return
	}
	authURL, state, err := s.oauth.BeginForUser(ctx, name, u.ID)
	if err != nil {
		if errors.Is(err, oauth.ErrClientNotFound) {
			writeError(w, http.StatusConflict, "an admin still has to finish this server's OAuth setup (Servers → Auth…)")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.setOAuthFlowCookie(w, r, state, u.ID)
	_ = s.audit.Write(ctx, audit.Event{
		EventType: "oauth.user_begin", AgentID: "user:" + u.ID, UpstreamName: name, ResultSummary: name,
		Raiser: actor.Raiser{OwnerUserID: u.ID, OwnerEmail: u.Email, OwnerName: u.Label()},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"authorize_url": authURL,
		"state":         state,
		"expires_in":    int(oauth.PendingTTL.Seconds()),
	})
}

// meConnectionDelete drops the caller's own token on a server (revoking it
// at the provider when it has a revocation endpoint) and closes their
// connection. Works whatever the server's current mode, so a row left by
// a server switched back to shared can still be removed.
func (s *Server) meConnectionDelete(w http.ResponseWriter, r *http.Request, u *identity.User, name string) {
	if s.assertionOAuthRefused(w, r, name) {
		return
	}
	ctx := r.Context()
	if err := s.oauth.DisconnectUser(ctx, name, u.ID); err != nil {
		if errors.Is(err, oauth.ErrNoUserToken) {
			writeError(w, http.StatusNotFound, "you have not connected this server")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.upstreams.DropUserConnection(name, u.ID)
	_ = s.audit.Write(ctx, audit.Event{
		EventType: "oauth.user_disconnect", AgentID: "user:" + u.ID, UpstreamName: name, ResultSummary: name,
		Raiser: actor.Raiser{OwnerUserID: u.ID, OwnerEmail: u.Email, OwnerName: u.Label()},
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// serverConnectionView is one person on the admin's "who signs in" list.
type serverConnectionView struct {
	UserID       string `json:"user_id"`
	Label        string `json:"label,omitempty"`
	Email        string `json:"email,omitempty"`
	State        string `json:"state"`
	AccountLabel string `json:"account_label,omitempty"`
	ConnectedAt  int64  `json:"connected_at,omitempty"`
	LastRefresh  int64  `json:"last_refresh_at,omitempty"`
	LastError    string `json:"last_error,omitempty"`
}

// serversConnections — admin.
//
//	GET /v1/servers/{name}/connections -> {"server","auth_mode","status","connections":[serverConnectionView]}
func (s *Server) serversConnections(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if _, err := s.requireAdmin(r); err != nil {
		writeAuthError(w, err)
		return
	}
	if s.assertionOAuthRefused(w, r, name) {
		return
	}
	if s.oauth == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth service not wired")
		return
	}
	ctx := r.Context()
	sv, err := s.upstreams.Get(ctx, name)
	if err != nil {
		if errors.Is(err, upstreams.ErrNotFound) {
			writeError(w, http.StatusNotFound, "server not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows, err := s.oauth.ListUserConnections(ctx, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]serverConnectionView, 0, len(rows))
	for _, c := range rows {
		v := serverConnectionView{
			UserID: c.UserID, State: c.State, AccountLabel: c.AccountLabel,
			ConnectedAt: ms(c.ConnectedAt), LastRefresh: ms(c.LastRefreshAt), LastError: c.LastError,
		}
		if u, err := s.identity.GetUserByID(ctx, c.UserID); err == nil {
			v.Label, v.Email = u.Label(), u.Email
		}
		out = append(out, v)
	}
	status := sv.LastStatus
	if !sv.Enabled {
		status = "disabled"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server":          name,
		"auth_mode":       sv.AuthMode,
		"status":          status,
		"waiting_sign_in": status == gateway.StatusWaitingSignIn,
		"connections":     out,
	})
}
