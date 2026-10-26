package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// clerkDirectory is the slice of *clerk.Client the api uses. An interface
// so tests can fake Clerk without standing up a JWKS server.
type clerkDirectory interface {
	VerifySessionToken(ctx context.Context, token string) (clerk.Claims, error)
	OrgMembership(ctx context.Context, clerkUserID string) (clerk.Member, error)
	OrgMembers(ctx context.Context) (map[string]clerk.Member, error)
	// VerifiedPrimaryEmail and VerifyGoogleAccount guard the owner role:
	// an owner-email sign-in must show a verified primary address, and a
	// transfer in personal mode a verified Google account.
	VerifiedPrimaryEmail(ctx context.Context, clerkUserID string) (string, error)
	VerifyGoogleAccount(ctx context.Context, clerkUserID, email string) error
}

// Per-IP budget for POST /v1/auth/clerk/session. Every attempt costs a
// signature check and, when the signature holds, one Clerk API call; a
// legitimate browser needs one attempt per sign-in.
const (
	clerkSessionMaxAttempts = 30
	clerkSessionWindow      = 15 * time.Minute
)

// authConfig tells the login page which sign-in methods exist. Public: the
// page has no session yet. The publishable key is public by design.
//
//	GET /v1/auth/config -> {"clerk":{"publishable_key":"pk_…","frontend_api":"clerk.example.com"}|null,
//	                       "password_login":true}
func (s *Server) authConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	var clerkCfg any
	if s.clerk != nil {
		clerkCfg = map[string]string{
			"publishable_key": s.clerkPublishableKey,
			"frontend_api":    s.clerkFrontendAPI,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"clerk":   clerkCfg,
		"version": s.buildVersion, "environment": s.environment,
		"capabilities": map[string]bool{"voice": s.voice != nil, "notes": s.notes != nil && s.notes.Enabled(), "memory": s.mempalace != nil,
			// The skills hub (-skills): the dashboard shows the Skills page only while it is on.
			"skills": s.skillsAdmin != nil},
		// The local password account stays as the break-glass login.
		"password_login": !s.clerkOwnerOnly,
		"owner_only":     s.clerkOwnerOnly,
	})
}

// authClerkSession swaps a Clerk session token for a toolyard session.
//
//	POST /v1/auth/clerk/session {"token":"<clerk session JWT>"}
//
// verify (RS256, exp/nbf, iss, azp) -> org membership -> local user upsert
// -> cookie. Errors are stable codes the login page switches on:
// 401 invalid_token, 403 not_org_member, 403 blocked, 503 clerk_unavailable,
// 404 when Clerk sign-in is off. Same-origin fetch from the dashboard, so
// the CSRF header and JSON content-type rules in HardenAPI apply unchanged.
func (s *Server) authClerkSession(w http.ResponseWriter, r *http.Request) {
	if s.clerk == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	ip := s.security.ClientIP(r)
	throttleKey := "clerk:" + ip
	if !s.unauthLimit.AllowN(throttleKey, clerkSessionMaxAttempts, clerkSessionWindow) {
		writeError(w, http.StatusTooManyRequests, "too many sign-in attempts; try again later")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Token) == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	ctx := r.Context()
	claims, err := s.clerk.VerifySessionToken(ctx, body.Token)
	if err != nil {
		if errors.Is(err, clerk.ErrUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "clerk_unavailable")
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid_token")
		return
	}
	member, err := s.clerk.OrgMembership(ctx, claims.Subject)
	if err != nil {
		if errors.Is(err, clerk.ErrNotMember) {
			_ = s.audit.Write(ctx, audit.Event{
				EventType: audit.EventUserLogin, Decision: "denied", Reason: "not_org_member",
				ResultSummary: claims.Subject,
			})
			writeError(w, http.StatusForbidden, "not_org_member")
			return
		}
		// ErrUnavailable and anything unexpected: fail closed without
		// telling the browser more than "try again".
		writeError(w, http.StatusServiceUnavailable, "clerk_unavailable")
		return
	}
	// Personal sign-in admits only the owner: the stored owner's email,
	// or the configured -owner-email until there is an owner with one
	// (identity.OwnerSignInEmail). The Clerk client checks the same
	// address on the verified Google account.
	if s.clerkOwnerOnly {
		allowed, err := s.identity.OwnerSignInEmail(ctx, s.ownerEmail)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "owner lookup failed")
			return
		}
		if !identity.EmailEqual(member.Email, allowed) {
			writeError(w, http.StatusForbidden, "not_org_member")
			return
		}
	}
	u, err := s.upsertClerkMember(ctx, claims.Subject, member)
	if errors.Is(err, clerk.ErrUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "clerk_unavailable")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The row must be the owner, too: a concurrent first sign-in may have
	// claimed the role, or the address may belong to someone who is not
	// (or no longer) the owner.
	if s.clerkOwnerOnly && !u.Owner {
		_ = s.audit.Write(ctx, audit.Event{
			EventType: audit.EventUserLogin, AgentID: "user:" + u.ID, Decision: "denied", Reason: "not_owner",
			ResultSummary: u.Username,
		})
		writeError(w, http.StatusForbidden, "not_org_member")
		return
	}
	if u.Status != identity.StatusActive {
		_ = s.audit.Write(ctx, audit.Event{
			EventType: audit.EventUserLogin, AgentID: "user:" + u.ID, Decision: "denied", Reason: "blocked",
			ResultSummary: u.Username,
		})
		writeError(w, http.StatusForbidden, "blocked")
		return
	}
	s.unauthLimit.Reset(throttleKey)
	if err := s.issueSession(w, r, u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.audit.Write(ctx, audit.Event{
		EventType: audit.EventUserLogin, AgentID: "user:" + u.ID, Reason: "clerk", ResultSummary: u.Username,
	})
	writeJSON(w, http.StatusOK, u)
}

// upsertClerkMember records a verified Clerk user who is a member of the
// organisation as a local user (profile and org role from the membership),
// the one way every Clerk-authenticated route creates or refreshes users.
//
// An address equal to the owner email links the setup account and may
// claim the owner role, so it must be the person's verified primary email:
// personal access already checked that (member.EmailVerified); an
// organisation membership's identifier is checked with the Backend API
// here. Not verified: the sign-in goes on as a plain one. Clerk
// unreachable: clerk.ErrUnavailable, and the caller refuses the sign-in
// rather than record the owner as someone else.
func (s *Server) upsertClerkMember(ctx context.Context, clerkUserID string, member clerk.Member) (*identity.User, error) {
	p := identity.ClerkProfile{
		ClerkUserID: clerkUserID,
		Email:       member.Email,
		DisplayName: clerkDisplayName(member),
		AvatarURL:   member.ImageURL,
		OrgRole:     member.Role,
	}
	if !member.EmailVerified && identity.EmailEqual(member.Email, s.ownerEmail) {
		p.EmailUnverified = true
		if s.clerk != nil {
			v, err := s.clerk.VerifiedPrimaryEmail(ctx, clerkUserID)
			switch {
			case errors.Is(err, clerk.ErrUnavailable):
				return nil, err
			case err == nil && identity.EmailEqual(v, member.Email):
				p.EmailUnverified = false
			}
		}
	}
	return s.identity.UpsertClerkUser(ctx, p, s.ownerEmail)
}

// clerkDisplayName is "First Last", falling back to the email when Clerk
// has no name for the account.
func clerkDisplayName(m clerk.Member) string {
	name := strings.TrimSpace(strings.TrimSpace(m.FirstName) + " " + strings.TrimSpace(m.LastName))
	if name == "" {
		return strings.TrimSpace(m.Email)
	}
	return name
}
