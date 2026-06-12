package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// policiesCollection handles GET (list) and POST (upsert) on /v1/policies.
func (s *Server) policiesCollection(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.policy == nil {
		writeError(w, http.StatusServiceUnavailable, "policy engine not wired")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.policy.List())
	case http.MethodPost:
		var body struct {
			Scope  string `json:"scope"`
			Target string `json:"target"`
			Action string `json:"action"`
			Note   string `json:"note"`
			Force  bool   `json:"force"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		p, err := s.policy.Set(r.Context(), body.Scope, body.Target, body.Action, body.Note, body.Force)
		if err != nil {
			if errors.Is(err, policy.ErrForceRequired) {
				// UI should confirm, then resend with force=true.
				writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "needs_force": true})
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Anti-fight: an explicit ask/deny on a tool disables any learned
		// kind=tool auto-approval rule so the two systems don't conflict.
		if body.Scope == policy.ScopeTool && (body.Action == "ask" || body.Action == "deny") && s.autoApproval != nil {
			_ = s.autoApproval.SetToolPolicy(r.Context(), body.Target, false)
		}
		writeJSON(w, http.StatusOK, p)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST only")
	}
}

// policiesItem handles DELETE /v1/policies/{id}.
func (s *Server) policiesItem(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.policy == nil {
		writeError(w, http.StatusServiceUnavailable, "policy engine not wired")
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "DELETE only")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/policies/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if err := s.policy.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
