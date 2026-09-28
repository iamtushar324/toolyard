package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/passkey"
)

// Passkey routes (session cookie). A passkey, once registered, is needed
// to allow high-risk tools in the inbox and to remove a passkey.
//
//	GET  /v1/passkeys                          list
//	POST /v1/passkeys/register/begin           → {session_id, options}
//	POST /v1/passkeys/register/finish          {session_id, name, credential}
//	POST /v1/passkeys/{id}/remove/begin        → {session_id, options}
//	POST /v1/passkeys/{id}/remove              {session_id, response}
//	POST /v1/inbox/{id}/passkey                {decision} → {session_id, options}
func (s *Server) passkeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/passkeys", s.passkeysList)
	mux.HandleFunc("/v1/passkeys/", s.passkeysItem)
}

func (s *Server) passkeyUser(w http.ResponseWriter, r *http.Request) (passkey.User, bool) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return passkey.User{}, false
	}
	if s.passkeys == nil {
		writeError(w, http.StatusServiceUnavailable, "passkeys not enabled")
		return passkey.User{}, false
	}
	u := passkey.User{ID: uid}
	if usr, err := s.identity.GetUserByID(r.Context(), uid); err == nil && usr != nil {
		u.Name = usr.Username
	}
	return u, true
}

func (s *Server) passkeysList(w http.ResponseWriter, r *http.Request) {
	u, ok := s.passkeyUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	list, err := s.passkeys.List(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": list})
}

func (s *Server) passkeysItem(w http.ResponseWriter, r *http.Request) {
	u, ok := s.passkeyUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	ctx := r.Context()
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/passkeys/"), "/")
	parts := strings.Split(rest, "/")
	origin := r.Header.Get("Origin")
	switch {
	case rest == "register/begin":
		id, opts, err := s.passkeys.BeginRegistration(ctx, u, origin, r.Host)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "options": opts})
	case rest == "register/finish":
		var body struct {
			SessionID  string          `json:"session_id"`
			Name       string          `json:"name"`
			Credential json.RawMessage `json:"credential"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		pk, err := s.passkeys.FinishRegistration(ctx, u, body.SessionID, body.Name, body.Credential)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.auditPasskey(r, "passkey.register", pk.Name)
		writeJSON(w, http.StatusOK, map[string]any{"passkey": pk})
	case len(parts) == 3 && parts[1] == "remove" && parts[2] == "begin":
		id, opts, err := s.passkeys.BeginAssertion(ctx, u, "remove:"+parts[0], origin, r.Host)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "options": opts})
	case len(parts) == 2 && parts[1] == "remove":
		var body inbox.PasskeyAssertion
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := s.passkeys.FinishAssertion(ctx, body.SessionID, "remove:"+parts[0], body.Response); err != nil {
			writeError(w, http.StatusForbidden, "passkey check failed: "+err.Error())
			return
		}
		if err := s.passkeys.Remove(ctx, u.ID, parts[0]); err != nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.auditPasskey(r, "passkey.remove", parts[0])
		writeJSON(w, http.StatusOK, map[string]any{"removed": parts[0]})
	default:
		http.NotFound(w, r)
	}
}

// inboxPasskeyBegin starts the passkey confirmation for one decision.
func (s *Server) inboxPasskeyBegin(w http.ResponseWriter, r *http.Request, id string) {
	u, ok := s.passkeyUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		Decision inbox.Decision `json:"decision"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Decision.Passkey = nil
	sid, opts, err := s.passkeys.BeginAssertion(r.Context(), u, "decide:"+inbox.DecisionDigest(id, body.Decision), r.Header.Get("Origin"), r.Host)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, passkey.ErrNone) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "options": opts})
}

func (s *Server) auditPasskey(r *http.Request, ev, detail string) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{EventType: ev, Reason: detail})
}
