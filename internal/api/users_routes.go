package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/identitykeys"
)

// groupInfo is one grantable tool group: an upstream server or a built-in
// data group (memory, events, notes).
type groupInfo struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // "server" | "builtin"
	ToolCount int    `json:"tool_count"`
	// Status is the upstream's last_status for servers ("connected",
	// "error", …; "disabled" when switched off) and "ok" for built-ins.
	Status string `json:"status"`
}

// userView is what the Users page renders per row.
type userView struct {
	identity.UserSummary
	Servers []string `json:"servers"`
	// IdentityKey is the user's Beknown key status; absent when identity
	// keys aren't wired.
	IdentityKey *identitykeys.Status `json:"identity_key,omitempty"`
}

// toolGroups lists every grantable group, sorted by name: each upstream
// server (whether or not it is connected right now), plus each built-in
// group that has at least one registered tool. Always-on groups (the
// meta-tools, inbox, session) never appear — they need no grant. Tool
// counts come from the live catalog through access.GroupOf, so a server
// and a built-in group sharing a name (notes, skills) count as one group.
func (s *Server) toolGroups(ctx context.Context) ([]groupInfo, error) {
	counts := map[string]int{}
	if s.gateway != nil {
		for _, e := range s.gateway.Catalog() {
			counts[access.GroupOf(e.Upstream, e.Name)]++
		}
	}
	seen := map[string]bool{}
	out := []groupInfo{}
	if s.upstreams != nil {
		servers, err := s.upstreams.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, sv := range servers {
			if sv.Name == "" || access.AlwaysOn(sv.Name) || seen[sv.Name] {
				continue
			}
			seen[sv.Name] = true
			status := sv.LastStatus
			if !sv.Enabled {
				status = "disabled"
			} else if status == "" {
				status = "unknown"
			}
			out = append(out, groupInfo{Name: sv.Name, Kind: "server", ToolCount: counts[sv.Name], Status: status})
		}
	}
	for _, g := range access.BuiltinGroups {
		if seen[g] || counts[g] == 0 {
			continue
		}
		seen[g] = true
		out = append(out, groupInfo{Name: g, Kind: "builtin", ToolCount: counts[g], Status: "ok"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// userGroups is the member's grants, [] when access isn't wired.
func (s *Server) userGroups(ctx context.Context, uid string) ([]string, error) {
	if s.access == nil {
		return []string{}, nil
	}
	g, err := s.access.Groups(ctx, uid)
	if err != nil {
		return nil, err
	}
	if g == nil {
		g = []string{}
	}
	return g, nil
}

func (s *Server) userViewFor(ctx context.Context, u *identity.User) (*userView, error) {
	agents, err := s.identity.ListAgents(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	servers, err := s.userGroups(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	ik, err := s.identityKeyStatus(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return &userView{UserSummary: identity.UserSummary{User: *u, AgentCount: len(agents)}, Servers: servers, IdentityKey: ik}, nil
}

// usersList — admin only.
//
//	GET /v1/users -> {"users":[User + "servers":[…] + "agent_count":n + "identity_key":status], "groups":[groupInfo]}
func (s *Server) usersList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if _, err := s.requireAdmin(r); err != nil {
		writeAuthError(w, err)
		return
	}
	ctx := r.Context()
	users, err := s.identity.ListUsers(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	groups, err := s.toolGroups(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]userView, 0, len(users))
	for _, u := range users {
		servers, err := s.userGroups(ctx, u.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		ik, err := s.identityKeyStatus(ctx, u.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, userView{UserSummary: u, Servers: servers, IdentityKey: ik})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "groups": groups})
}

// usersItem dispatches the per-user admin actions:
//
//	PATCH  /v1/users/{id}                       {"role"?, "status"?, "blocked_reason"?, "servers"?:[…]}
//	POST   /v1/users/{id}/make-owner            {"confirm":"<their username, email or name>", "revoke_previous"?:bool} (the owner only)
//	POST   /v1/users/{id}/revoke-sessions
//	POST   /v1/users/{id}/identity-key          issue or rotate the Beknown key
//	DELETE /v1/users/{id}/identity-key          revoke it
//	POST   /v1/users/{id}/identity-key/register retry its registry upsert
func (s *Server) usersItem(w http.ResponseWriter, r *http.Request) {
	caller, err := s.requireAdmin(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	id, sub, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/users/"), "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	target, err := s.identity.GetUserByID(r.Context(), id)
	if errors.Is(err, identity.ErrNoUser) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch {
	case sub == "" && r.Method == http.MethodPatch:
		s.usersPatch(w, r, caller, target)
	case sub == "make-owner" && r.Method == http.MethodPost:
		s.usersMakeOwner(w, r, caller, target)
	case sub == "revoke-sessions" && r.Method == http.MethodPost:
		if err := s.identity.RevokeAllForUser(r.Context(), target.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType: "user.revoke_sessions", AgentID: "user:" + caller.ID, ResultSummary: target.Username,
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case sub == "identity-key" || sub == "identity-key/register":
		s.usersIdentityKey(w, r, caller, target, sub)
	default:
		writeError(w, http.StatusMethodNotAllowed, "PATCH, POST /make-owner, POST /revoke-sessions, POST or DELETE /identity-key, or POST /identity-key/register")
	}
}

// usersPatch applies role, status and server grants. Everything is
// validated before anything is written; unknown groups are 400, the
// last-admin rule, the owner rule (the owner is never demoted or blocked)
// and self-edits are 409. A role change needs an admin's browser session:
// an operator token, whatever its scopes, gets 403 session_required. Each
// applied change gets its own audit row, and the access cache for the user
// is dropped so agents see the new scope on their next call.
func (s *Server) usersPatch(w http.ResponseWriter, r *http.Request, caller, target *identity.User) {
	var body struct {
		Role          *string   `json:"role"`
		Status        *string   `json:"status"`
		BlockedReason *string   `json:"blocked_reason"`
		Servers       *[]string `json:"servers"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Role == nil && body.Status == nil && body.Servers == nil {
		writeError(w, http.StatusBadRequest, "nothing to change: send role, status or servers")
		return
	}
	ctx := r.Context()
	changeRole := body.Role != nil && *body.Role != target.Role
	changeStatus := body.Status != nil && *body.Status != target.Status
	if (changeRole || changeStatus) && target.ID == caller.ID {
		writeError(w, http.StatusConflict, "cannot change your own role or status")
		return
	}
	if changeRole && operatorFromContext(ctx) != nil {
		writeSessionRequired(w, "changing a person's role")
		return
	}
	if body.Role != nil && *body.Role != identity.RoleAdmin && *body.Role != identity.RoleMember {
		writeError(w, http.StatusBadRequest, identity.ErrInvalidRole.Error())
		return
	}
	if body.Status != nil && *body.Status != identity.StatusActive && *body.Status != identity.StatusBlocked {
		writeError(w, http.StatusBadRequest, identity.ErrInvalidStatus.Error())
		return
	}
	// The owner is always an active admin: say so before anything else is
	// written (SetRole and SetStatus refuse it too).
	if target.Owner && ((changeRole && *body.Role != identity.RoleAdmin) || (changeStatus && *body.Status == identity.StatusBlocked)) {
		s.writeUserChangeError(w, identity.ErrOwner)
		return
	}
	var servers []string
	if body.Servers != nil {
		if s.access == nil {
			writeError(w, http.StatusServiceUnavailable, "access control not wired")
			return
		}
		groups, err := s.toolGroups(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		known := map[string]bool{}
		for _, g := range groups {
			known[g.Name] = true
		}
		// Every built-in group is grantable even while it has no tools
		// (a group that is switched off today may be on tomorrow).
		for _, g := range access.BuiltinGroups {
			known[g] = true
		}
		for _, g := range *body.Servers {
			g = strings.TrimSpace(g)
			if !known[g] && !access.AlwaysOn(g) {
				writeError(w, http.StatusBadRequest, "unknown server or group: "+g)
				return
			}
			servers = append(servers, g)
		}
		if servers == nil {
			servers = []string{}
		}
	}

	actor := "user:" + caller.ID
	if changeRole {
		if err := s.identity.SetRole(ctx, target.ID, *body.Role); err != nil {
			s.writeUserChangeError(w, err)
			return
		}
		_ = s.audit.Write(ctx, audit.Event{
			EventType: "user.role", AgentID: actor,
			ResultSummary: fmt.Sprintf("%s role=%s", target.Username, *body.Role),
		})
	}
	if changeStatus {
		reason := ""
		if body.BlockedReason != nil {
			reason = strings.TrimSpace(*body.BlockedReason)
		}
		if *body.Status == identity.StatusBlocked && reason == "" {
			reason = "manual"
		}
		if err := s.identity.SetStatus(ctx, target.ID, *body.Status, reason); err != nil {
			s.writeUserChangeError(w, err)
			return
		}
		_ = s.audit.Write(ctx, audit.Event{
			EventType: "user.status", AgentID: actor, Reason: reason,
			ResultSummary: fmt.Sprintf("%s status=%s", target.Username, *body.Status),
		})
	}
	if body.Servers != nil {
		if err := s.access.SetGroups(ctx, target.ID, servers, caller.ID); err != nil {
			if errors.Is(err, access.ErrInvalidGroup) {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = s.audit.Write(ctx, audit.Event{
			EventType: "user.servers", AgentID: actor,
			ResultSummary: fmt.Sprintf("%s servers=%s", target.Username, strings.Join(servers, ",")),
		})
	}
	if s.access != nil {
		s.access.Invalidate(target.ID)
	}
	// A block must stop the user's identity key being forwarded at once,
	// not after the resolver's cache expires.
	if s.identityKeys != nil && changeStatus {
		s.identityKeys.Invalidate(target.ID)
	}

	updated, err := s.identity.GetUserByID(ctx, target.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	view, err := s.userViewFor(ctx, updated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// usersMakeOwner transfers the owner role to target. Only the owner may,
// in their own browser session: other admins get 403 not_owner, and an
// operator token, whatever its scopes, 403 session_required (RoleGuard
// already refuses the path to operator tokens, operatorScopeFor). The body
// must repeat the new owner's username, email or display name (a fresh,
// typed confirmation; 400 confirm_mismatch otherwise). The target must be
// another active user (409 already_owner, user_blocked). In personal
// sign-in mode (-clerk-owner-only) only the owner signs in, so the target
// must have signed in with Google before (409 no_sign_in) and Clerk must
// still show a verified Google account with their email (409
// no_google_sign_in; 503 clerk_unavailable when Clerk cannot say). The new
// owner becomes an admin and the caller stays one; identity audits the
// move (user.owner, with both user ids) in the same transaction. With
// revoke_previous the caller's agents are disabled and their operator
// tokens revoked in that transaction too. In personal sign-in mode that is
// always done, every operator token that is not the new owner's is revoked
// as well (only the owner signs in there), and after the commit the
// caller's identity key is revoked (identitykeys, which calls the
// registries; a failure is reported in the answer as identity_key_error).
func (s *Server) usersMakeOwner(w http.ResponseWriter, r *http.Request, caller, target *identity.User) {
	ctx := r.Context()
	if operatorFromContext(ctx) != nil {
		writeSessionRequired(w, "transferring ownership")
		return
	}
	if !caller.Owner {
		writeError(w, http.StatusForbidden, "not_owner")
		return
	}
	var body struct {
		Confirm        string `json:"confirm"`
		RevokePrevious bool   `json:"revoke_previous"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if target.ID == caller.ID {
		writeError(w, http.StatusConflict, "already_owner")
		return
	}
	if target.Status != identity.StatusActive {
		writeError(w, http.StatusConflict, "user_blocked")
		return
	}
	if !ownerConfirmMatches(body.Confirm, target) {
		writeError(w, http.StatusBadRequest, "confirm_mismatch")
		return
	}
	if s.clerkOwnerOnly {
		if target.ClerkUserID == "" || strings.TrimSpace(target.Email) == "" {
			writeError(w, http.StatusConflict, "no_sign_in")
			return
		}
		if s.clerk != nil {
			err := s.clerk.VerifyGoogleAccount(ctx, target.ClerkUserID, target.Email)
			switch {
			case errors.Is(err, clerk.ErrNotMember):
				writeError(w, http.StatusConflict, "no_google_sign_in")
				return
			case err != nil:
				writeError(w, http.StatusServiceUnavailable, "clerk_unavailable")
				return
			}
		}
	}
	opts := identity.TransferOptions{RevokePrevious: body.RevokePrevious}
	if s.clerkOwnerOnly {
		opts = identity.TransferOptions{RevokePrevious: true, RevokeOtherOperatorTokens: true}
	}
	if _, err := s.identity.TransferOwnershipWith(ctx, caller.ID, target.ID, opts); err != nil {
		s.writeUserChangeError(w, err)
		return
	}
	identityKeyErr := ""
	if s.clerkOwnerOnly && s.identityKeys != nil {
		if _, err := s.identityKeys.Revoke(ctx, caller.ID, caller.ID); err != nil && !errors.Is(err, identitykeys.ErrNoKey) {
			identityKeyErr = err.Error()
		}
	}
	// The new owner is an admin now: their agents see every server on the
	// next call.
	if s.access != nil {
		s.access.Invalidate(target.ID)
		s.access.Invalidate(caller.ID)
	}
	updated, err := s.identity.GetUserByID(ctx, target.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	view, err := s.userViewFor(ctx, updated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if identityKeyErr != "" {
		writeJSON(w, http.StatusOK, map[string]any{"user": view, "identity_key_error": identityKeyErr})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// ownerConfirmMatches reports whether typed names the user: their
// username, email or display name, ignoring case and surrounding spaces.
func ownerConfirmMatches(typed string, u *identity.User) bool {
	typed = strings.TrimSpace(typed)
	if typed == "" {
		return false
	}
	for _, v := range []string{u.Username, u.Email, u.DisplayName} {
		if v = strings.TrimSpace(v); v != "" && strings.EqualFold(typed, v) {
			return true
		}
	}
	return false
}

// writeSessionRequired refuses an operator token an action that needs a
// person in a dashboard session.
func writeSessionRequired(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": "session_required",
		"hint":  what + " needs a person signed in to the dashboard; operator tokens cannot do it",
	})
}

func (s *Server) writeUserChangeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, identity.ErrLastAdmin):
		writeError(w, http.StatusConflict, "last_admin")
	case errors.Is(err, identity.ErrOwner):
		writeError(w, http.StatusConflict, "owner_protected")
	case errors.Is(err, identity.ErrNotOwner):
		writeError(w, http.StatusForbidden, "not_owner")
	case errors.Is(err, identity.ErrAlreadyOwner):
		writeError(w, http.StatusConflict, "already_owner")
	case errors.Is(err, identity.ErrUserBlocked):
		writeError(w, http.StatusConflict, "user_blocked")
	case errors.Is(err, identity.ErrNoUser):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, identity.ErrInvalidRole), errors.Is(err, identity.ErrInvalidStatus):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// meServers — any signed-in user.
//
//	GET /v1/me/servers -> [groupInfo] the caller may use (admins: every group)
func (s *Server) meServers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	u, ok := s.sessionUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ctx := r.Context()
	groups, err := s.toolGroups(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if u.Role == identity.RoleAdmin {
		writeJSON(w, http.StatusOK, groups)
		return
	}
	granted, err := s.userGroups(ctx, u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	allowed := map[string]bool{}
	for _, g := range granted {
		allowed[g] = true
	}
	out := []groupInfo{}
	for _, g := range groups {
		if allowed[g.Name] {
			out = append(out, g)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
