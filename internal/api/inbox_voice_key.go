package api

import (
	"net/http"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
)

// inboxVoiceKey accepts a key from the owner's dashboard without returning it.
// Operator/agent credentials cannot use this browser-only credential route.
func (s *Server) inboxVoiceKey(w http.ResponseWriter, r *http.Request) {
	if operatorFromContext(r.Context()) != nil {
		writeError(w, http.StatusForbidden, "set the voice key in the owner dashboard")
		return
	}
	u, err := s.requireAdmin(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings unavailable")
		return
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid key request")
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" || len(key) > 1024 || strings.ContainsAny(key, "\r\n") {
		writeError(w, http.StatusBadRequest, "enter a valid ElevenLabs API key")
		return
	}
	if err := s.settings.Set(r.Context(), settings.ElevenLabsAPIKey, key); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save voice key")
		return
	}
	if s.audit != nil {
		_ = s.audit.Write(r.Context(), audit.Event{EventType: "settings.voice_key", AgentID: "user:" + u.ID, ResultSummary: settings.ElevenLabsAPIKey})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]bool{"elevenlabs_api_key_present": true})
}
