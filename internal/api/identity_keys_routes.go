package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/identitykeys"
)

// Identity key routes: each user's one "Beknown key", provisioned by an
// admin and revealed once by its owner.
//
//	GET    /v1/me/identity-key                 status for the caller
//	POST   /v1/me/identity-key/reveal          {"key","fingerprint"} once; 409 already_revealed, 404 no_key
//	POST   /v1/users/{id}/identity-key         admin: issue, or rotate when one exists → status
//	DELETE /v1/users/{id}/identity-key         admin: revoke → {"ok":true}; 404 no_key
//	POST   /v1/users/{id}/identity-key/register admin: retry the registry upsert → status
//
// The status object is identitykeys.Status:
//
//	{"has_key","revealed","fingerprint"?,"created_at"?,"registrations":[{"upstream","status","error","updated_at"}]}
//
// The admin routes hang off usersItem; only the /me pair registers here.
func (s *Server) identityKeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/me/identity-key", s.meIdentityKey)
	mux.HandleFunc("/v1/me/identity-key/reveal", s.meIdentityKeyReveal)
}

// identityKeyUser is requireUser plus the "is the feature wired" check.
func (s *Server) identityKeyUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	if s.identityKeys == nil {
		writeError(w, http.StatusServiceUnavailable, "identity keys not enabled")
		return "", false
	}
	return uid, true
}

func (s *Server) meIdentityKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	uid, ok := s.identityKeyUser(w, r)
	if !ok {
		return
	}
	st, err := s.identityKeys.Status(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// meIdentityKeyReveal hands the owner their raw key exactly once. The
// service records revealed_at and audits it; the key is never logged.
func (s *Server) meIdentityKeyReveal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, ok := s.identityKeyUser(w, r)
	if !ok {
		return
	}
	key, fp, err := s.identityKeys.Reveal(r.Context(), uid)
	if err != nil {
		writeIdentityKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key, "fingerprint": fp})
}

// usersIdentityKey serves the admin actions under /v1/users/{id}/; sub is
// "identity-key" or "identity-key/register". Every registry call runs as
// the calling admin, so prime records them as created_by.
func (s *Server) usersIdentityKey(w http.ResponseWriter, r *http.Request, caller, target *identity.User, sub string) {
	if s.identityKeys == nil {
		writeError(w, http.StatusServiceUnavailable, "identity keys not enabled")
		return
	}
	ctx := r.Context()
	switch {
	case sub == "identity-key" && r.Method == http.MethodPost:
		st, err := s.identityKeys.Provision(ctx, caller.ID, target.ID)
		if err != nil {
			writeIdentityKeyError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	case sub == "identity-key" && r.Method == http.MethodDelete:
		if _, err := s.identityKeys.Revoke(ctx, caller.ID, target.ID); err != nil {
			writeIdentityKeyError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case sub == "identity-key/register" && r.Method == http.MethodPost:
		st, err := s.identityKeys.Register(ctx, caller.ID, target.ID)
		if err != nil {
			writeIdentityKeyError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	default:
		writeError(w, http.StatusMethodNotAllowed, "POST or DELETE /identity-key, or POST /identity-key/register")
	}
}

// writeIdentityKeyError maps the service's sentinel errors to the codes
// the dashboard switches on.
func writeIdentityKeyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, identitykeys.ErrNoKey):
		writeError(w, http.StatusNotFound, "no_key")
	case errors.Is(err, identitykeys.ErrAlreadyRevealed):
		writeError(w, http.StatusConflict, "already_revealed")
	case errors.Is(err, identitykeys.ErrNoClerkIdentity):
		writeError(w, http.StatusConflict, "no_clerk_identity")
	case errors.Is(err, identitykeys.ErrKeyExists):
		writeError(w, http.StatusConflict, "key_exists")
	case errors.Is(err, identity.ErrNoUser):
		writeError(w, http.StatusNotFound, "user not found")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// identityKeyStatus is the per-user status for the Users page, nil when
// the feature isn't wired.
func (s *Server) identityKeyStatus(ctx context.Context, uid string) (*identitykeys.Status, error) {
	if s.identityKeys == nil {
		return nil, nil
	}
	return s.identityKeys.Status(ctx, uid)
}
