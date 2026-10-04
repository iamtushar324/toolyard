package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/callbacks"
	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/federation"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

func (s *Server) federationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/.well-known/toolyard-instance", func(w http.ResponseWriter, r *http.Request) {
		if s.federation == nil || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{"instance_id": s.federation.InstanceID, "protocol": federation.Protocol, "capabilities": []string{"inbox.batch.v1", "callbacks.standard-webhooks.v1", "federation.ed25519.v1", "dashboard.handoff.v1"}})
	})
	mux.HandleFunc("/v1/federation/register", s.federationRegister)
	mux.HandleFunc("/v1/federation/", s.federationAction)
	mux.HandleFunc("/v1/callbacks/receivers", s.callbackReceiver)
	mux.HandleFunc("/v1/callbacks/receivers/", s.callbackReceiver)
	mux.HandleFunc("/v1/callbacks/retry/", s.callbackRetry)
	mux.HandleFunc("/v1/admin/callback-receivers", s.adminCallbackReceiver)
	mux.HandleFunc("/federation/handoff", s.federationHandoff)
}

// Private generic destinations require an explicit administrator registration.
// The URL is immutable; a different destination requires a new binding. This
// cookie-authenticated route retains the normal origin and CSRF checks.
func (s *Server) adminCallbackReceiver(w http.ResponseWriter, r *http.Request) {
	admin, err := s.requireAdmin(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	if s.callbacks == nil {
		writeError(w, 503, "callbacks_unavailable")
		return
	}
	var b struct {
		AgentID          string `json:"agent_id"`
		Destination      string `json:"destination"`
		Secret           string `json:"secret"`
		ClientReceiverID string `json:"client_receiver_id"`
	}
	if err = decode(r, &b); err != nil {
		writeError(w, 400, "invalid_receiver")
		return
	}
	// An administrator cannot assign a receiver to another user's agent.
	agents, err := s.identity.ListAgents(r.Context(), admin.ID)
	if err != nil {
		writeError(w, 500, "agent_lookup_failed")
		return
	}
	owned := false
	for _, a := range agents {
		if a.ID == b.AgentID && !a.Disabled {
			owned = true
		}
	}
	if !owned {
		writeError(w, 404, "agent_not_found")
		return
	}
	receiver, err := s.callbacks.Register(r.Context(), b.AgentID, "", b.ClientReceiverID, b.Destination, b.Secret, true)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, receiver)
}

func federationError(w http.ResponseWriter, err error) {
	code, status := "internal_error", 500
	switch {
	case errors.Is(err, federation.ErrUnavailable):
		code, status = "membership_unavailable", 503
	case errors.Is(err, federation.ErrTrustRevoked):
		code, status = "trust_revoked", 403
	case errors.Is(err, federation.ErrNotMember):
		code, status = "not_org_member", 403
	case errors.Is(err, federation.ErrUnauthorized):
		code, status = "invalid_assertion", 401
	case errors.Is(err, federation.ErrRevoked):
		code, status = "connection_revoked", 403
	case errors.Is(err, federation.ErrConflict), errors.Is(err, callbacks.ErrConflict):
		code, status = "conflict", 409
	case errors.Is(err, callbacks.ErrReceiver):
		code, status = "receiver_unavailable", 404
	}
	writeError(w, status, code)
}

func (s *Server) federationRegister(w http.ResponseWriter, r *http.Request) {
	if s.federation == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	var body struct {
		federation.Trust
		Token string `json:"token"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, 400, "invalid_registration")
		return
	}
	verifier, ok := s.clerk.(connectVerifier)
	if !ok {
		writeError(w, 503, "membership_unavailable")
		return
	}
	claims, err := verifier.VerifySessionTokenFor(r.Context(), body.Token, []string{body.Origin})
	if err != nil {
		writeError(w, 401, "invalid_token")
		return
	}
	member, err := verifier.OrgMembership(r.Context(), claims.Subject)
	if err != nil {
		if errors.Is(err, clerk.ErrNotMember) {
			writeError(w, 403, "not_org_member")
		} else {
			writeError(w, 503, "membership_unavailable")
		}
		return
	}
	// Registration cannot turn a new eligible member into an administrator.
	u, err := s.identity.UpsertClerkUser(r.Context(), identity.ClerkProfile{ClerkUserID: claims.Subject, Email: member.Email, DisplayName: clerkDisplayName(member), AvatarURL: member.ImageURL}, "")
	if err != nil || u.Role != identity.RoleAdmin || u.Status != identity.StatusActive {
		writeError(w, 403, "admin_required")
		return
	}
	if err = s.federation.Register(r.Context(), body.Trust, u.ID); err != nil {
		federationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"instance_id": s.federation.InstanceID, "protocol": federation.Protocol})
}

func (s *Server) federationAction(w http.ResponseWriter, r *http.Request) {
	if s.federation == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	var body struct {
		Assertion       string `json:"assertion"`
		ExpectedVersion int    `json:"expected_version"`
		ReturnURL       string `json:"return_url"`
		Scope           string `json:"scope"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, 400, "invalid_request")
		return
	}
	verify := s.federation.Verify
	if r.URL.Path == "/v1/federation/revoke" {
		verify = s.federation.VerifyRevocation
	}
	p, err := verify(r.Context(), body.Assertion)
	if err != nil {
		federationError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch strings.TrimPrefix(r.URL.Path, "/v1/federation/") {
	case "connect":
		credential, err := s.federation.Connect(r.Context(), p, body.ExpectedVersion)
		if err != nil {
			federationError(w, err)
			return
		}
		writeJSON(w, 200, credential)
	case "handoff":
		code, expiry, err := s.federation.Handoff(r.Context(), p, body.ReturnURL)
		if err != nil {
			federationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"url": strings.TrimRight(s.security.PublicURL, "/") + "/federation/handoff?code=" + url.QueryEscape(code), "expires_at": expiry.UTC().Format(time.RFC3339)})
	case "revoke":
		if err = s.federation.Revoke(r.Context(), p, body.Scope); err != nil {
			federationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) federationHandoff(w http.ResponseWriter, r *http.Request) {
	if s.federation == nil || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	userID, err := s.federation.ConsumeHandoff(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		writeError(w, 401, "handoff_expired_or_used")
		return
	}
	if err = s.issueSession(w, r, userID); err != nil {
		writeError(w, 500, "session_failed")
		return
	}
	http.Redirect(w, r, "/#inbox", http.StatusSeeOther)
}

func (s *Server) callbackAgent(w http.ResponseWriter, r *http.Request) (*identity.Agent, bool) {
	if s.callbacks == nil || s.federation == nil {
		http.NotFound(w, r)
		return nil, false
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeError(w, 401, "agent_token_required")
		return nil, false
	}
	a, err := s.federation.VerifyAgent(r.Context(), strings.TrimPrefix(auth, "Bearer "))
	if err != nil {
		writeError(w, 401, "invalid_agent")
		return nil, false
	}
	return a, true
}
func (s *Server) callbackReceiver(w http.ResponseWriter, r *http.Request) {
	a, ok := s.callbackAgent(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ref := strings.TrimPrefix(r.URL.Path, "/v1/callbacks/receivers")
	ref = strings.TrimPrefix(ref, "/")
	switch r.Method {
	case http.MethodPost:
		if ref != "" {
			writeError(w, 405, "use PATCH")
			return
		}
		var b struct {
			Destination      string `json:"destination"`
			Secret           string `json:"secret"`
			ClientReceiverID string `json:"client_receiver_id"`
			EnvironmentID    string `json:"environment_id"`
		}
		if err := decode(r, &b); err != nil {
			writeError(w, 400, "invalid_receiver")
			return
		}
		private := s.federation.PrivateReceiverAllowed(r.Context(), a.ID, b.EnvironmentID, b.Destination)
		// A claimed environment must be the verified connection, not caller text.
		if b.EnvironmentID != "" && !private {
			writeError(w, 403, "environment_destination_mismatch")
			return
		}
		receiver, err := s.callbacks.Register(r.Context(), a.ID, b.EnvironmentID, b.ClientReceiverID, b.Destination, b.Secret, private)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, receiver)
	case http.MethodGet:
		receiver, err := s.callbacks.Get(r.Context(), a.ID, ref)
		if err != nil {
			federationError(w, err)
			return
		}
		history, err := s.callbacks.ReceiverHistory(r.Context(), a.ID, ref)
		if err != nil {
			federationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"receiver": receiver, "deliveries": history})
	case http.MethodPatch:
		var b struct {
			Action           string `json:"action"`
			Secret           string `json:"secret"`
			ExpectedRevision int    `json:"expected_revision"`
		}
		if err := decode(r, &b); err != nil {
			writeError(w, 400, "invalid_change")
			return
		}
		receiver, err := s.callbacks.Change(r.Context(), a.ID, ref, b.Action, b.Secret, b.ExpectedRevision)
		if err != nil {
			federationError(w, err)
			return
		}
		writeJSON(w, 200, receiver)
	default:
		writeError(w, 405, "method_not_allowed")
	}
}
func (s *Server) callbackRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	a, ok := s.callbackAgent(w, r)
	if !ok {
		return
	}
	if err := s.callbacks.Retry(r.Context(), a.ID, strings.TrimPrefix(r.URL.Path, "/v1/callbacks/retry/")); err != nil {
		federationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// Keep Clerk error semantics explicit for registration tests.
var _ = clerk.ErrNotMember
