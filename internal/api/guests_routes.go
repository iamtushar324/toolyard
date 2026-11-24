package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// Guest invites (TEC-2058): the admin routes that decide who from outside
// the organisation may sign in, and what they reach (guest_signin.go,
// access/guests.go). Admin only: RoleGuard keeps members and guests off,
// and every handler checks requireAdmin too. An operator token needs the
// owner scope to change them (operatorOwnerPrefixes).
//
//	GET    /v1/guests          -> {"invites":[guestInviteView]}
//	POST   /v1/guests          {"email","servers":[…],"tools"?:[…],"expires_at"?:<unix ms>,"note"?}
//	                           create or replace the invite for email -> guestInviteView
//	DELETE /v1/guests/{email}  remove it; the guest (if they signed in) is blocked at once
//
// Every create, update and delete writes a guest.invite audit row
// (reason created, updated or deleted).

const guestsPath = "/v1/guests"

// EventGuestInvite is the audit event of every invite change.
const EventGuestInvite = "guest.invite"

// guestInviteView is an invite plus the state of the guest's account, if
// they have signed in.
type guestInviteView struct {
	access.GuestInvite
	Expired    bool   `json:"expired"`
	UserID     string `json:"user_id,omitempty"`
	Username   string `json:"username,omitempty"`
	UserStatus string `json:"user_status,omitempty"`
}

func (s *Server) guestsCollection(w http.ResponseWriter, r *http.Request) {
	caller, err := s.requireAdmin(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if s.access == nil {
		writeError(w, http.StatusServiceUnavailable, "access control not wired")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.guestsList(w, r)
	case http.MethodPost:
		s.guestsPut(w, r, caller)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func (s *Server) guestsItem(w http.ResponseWriter, r *http.Request) {
	caller, err := s.requireAdmin(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if s.access == nil {
		writeError(w, http.StatusServiceUnavailable, "access control not wired")
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "DELETE only")
		return
	}
	// r.URL.Path is already decoded once (net/url): decoding it again
	// would turn a literal % in an address (guest%ops@…) into an error, or
	// let "%2541" name the invite for "A…". The client escapes the email
	// once (url.PathEscape), and that one decoding is all.
	email := strings.TrimPrefix(r.URL.Path, guestsPath+"/")
	if email == "" || strings.Contains(email, "/") {
		writeError(w, http.StatusBadRequest, "the path must be /v1/guests/{email}")
		return
	}
	s.guestsDelete(w, r, caller, email)
}

// guestUsers maps a lower-case email to the guest rows with it.
func (s *Server) guestUsers(ctx context.Context) (map[string][]identity.User, error) {
	users, err := s.identity.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]identity.User{}
	for _, u := range users {
		if u.Role != identity.RoleGuest {
			continue
		}
		e, err := access.NormalizeEmail(u.Email)
		if err != nil {
			continue
		}
		out[e] = append(out[e], u.User)
	}
	return out, nil
}

func (s *Server) inviteView(inv access.GuestInvite, users map[string][]identity.User) guestInviteView {
	v := guestInviteView{GuestInvite: inv, Expired: inv.Expired(time.Now())}
	if us := users[inv.Email]; len(us) > 0 {
		v.UserID, v.Username, v.UserStatus = us[0].ID, us[0].Username, us[0].Status
	}
	return v
}

func (s *Server) guestsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	invites, err := s.access.ListGuestInvites(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	users, err := s.guestUsers(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]guestInviteView, 0, len(invites))
	for _, inv := range invites {
		out = append(out, s.inviteView(inv, users))
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": out})
}

func (s *Server) guestsPut(w http.ResponseWriter, r *http.Request, caller *identity.User) {
	var body struct {
		Email     string   `json:"email"`
		Servers   []string `json:"servers"`
		Tools     []string `json:"tools"`
		ExpiresAt int64    `json:"expires_at"`
		Note      string   `json:"note"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	inv := access.GuestInvite{Email: body.Email, Servers: body.Servers, Tools: body.Tools, ExpiresAt: body.ExpiresAt, Note: body.Note}
	// Shape first (email, built-in groups, expiry), then the live catalog.
	norm, err := inv.Normalize(time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if msg := s.checkInviteCatalog(ctx, norm); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	saved, created, err := s.access.PutGuestInvite(ctx, norm, caller.ID)
	if errors.Is(err, access.ErrInvalidInvite) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	reason := "updated"
	if created {
		reason = "created"
	}
	expires := "never"
	if saved.ExpiresAt > 0 {
		expires = time.UnixMilli(saved.ExpiresAt).UTC().Format(time.RFC3339)
	}
	_ = s.audit.Write(ctx, audit.Event{
		EventType: EventGuestInvite, AgentID: "user:" + caller.ID, Reason: reason,
		ResultSummary: fmt.Sprintf("%s servers=%s tools=%d expires=%s",
			saved.Email, strings.Join(saved.Servers, ","), len(saved.Tools), expires),
	})
	users, err := s.guestUsers(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A guest who signed in before sees the new servers on the Users page
	// now; their scope already follows the invite.
	for _, u := range users[saved.Email] {
		if err := s.settleGuest(ctx, u.ID, &saved); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, s.inviteView(saved, users))
}

// checkInviteCatalog checks an invite against what toolyard serves: every
// server must be a configured upstream, and every tool a registered tool
// of one of those servers (so a typo never reads as "nothing allowed").
// It returns the error message, or "".
func (s *Server) checkInviteCatalog(ctx context.Context, inv access.GuestInvite) string {
	groups, err := s.toolGroups(ctx)
	if err != nil {
		return "list servers: " + err.Error()
	}
	known := map[string]bool{}
	for _, g := range groups {
		if g.Kind == "server" {
			known[g.Name] = true
		}
	}
	servers := map[string]bool{}
	for _, g := range inv.Servers {
		if !known[g] {
			return "unknown server: " + g
		}
		servers[g] = true
	}
	if len(inv.Tools) == 0 || s.gateway == nil {
		return ""
	}
	groupOf := map[string]string{}
	for _, e := range s.gateway.Catalog() {
		groupOf[e.Name] = access.GroupOf(e.Upstream, e.Name)
	}
	for _, t := range inv.Tools {
		g, ok := groupOf[t]
		if !ok {
			return "unknown tool: " + t + " (use the full name tools/list shows; the server must be connected)"
		}
		if !servers[g] {
			return fmt.Sprintf("tool %s belongs to server %s, which the invite does not list", t, g)
		}
	}
	return ""
}

func (s *Server) guestsDelete(w http.ResponseWriter, r *http.Request, caller *identity.User, email string) {
	ctx := r.Context()
	e, err := access.NormalizeEmail(email)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	existed, err := s.access.DeleteGuestInvite(ctx, e)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "no guest invite for that email")
		return
	}
	_ = s.audit.Write(ctx, audit.Event{
		EventType: EventGuestInvite, AgentID: "user:" + caller.ID, Reason: "deleted", ResultSummary: e,
	})
	// The guest's scope is already empty (no invite); blocking them also
	// ends their sessions and stops their agents now, not at the next
	// hourly sync.
	users, err := s.guestUsers(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	blocked := []string{}
	lg := logx.For("guests")
	for _, u := range users[e] {
		if u.Status != identity.StatusActive {
			continue
		}
		if s.blockGuest(ctx, lg, u, "invite_removed", "user:"+caller.ID) {
			blocked = append(blocked, u.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": e, "blocked_users": blocked})
}
