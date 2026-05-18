package api

import (
	"errors"
	"net/http"

	"github.com/tusharbhardwaj/toolyard/internal/voice"
)

// voiceRoutes registers the dashboard-facing API for live voice calls.
// All routes are session-cookie authenticated. The WebSocket upgrade is
// the load-bearing path; the GET/POST routes are tiny status helpers.
//
//	GET  /v1/voice/ws        upgrade and start a live call
//	GET  /v1/voice/sessions  list active calls (single-user gateway today,
//	                          but the array shape future-proofs multi-user)
//	POST /v1/voice/hangup    terminate the caller's active call
//
// Voice service is optional: when not configured, every route 503s with
// a hint instead of 404 so the dashboard can render a clear "voice not
// configured" state.
func (s *Server) voiceRoutes(mux *http.ServeMux) {
	if s.voice == nil {
		return
	}
	mux.HandleFunc("/v1/voice/ws", s.voiceWS)
	mux.HandleFunc("/v1/voice/sessions", s.voiceSessions)
	mux.HandleFunc("/v1/voice/hangup", s.voiceHangup)
}

func (s *Server) voiceWS(w http.ResponseWriter, r *http.Request) {
	if s.voice == nil {
		writeError(w, http.StatusServiceUnavailable, "voice not configured")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// HandleWS takes over the response writer on success. It returns an
	// error in three cases we care about:
	//   - ErrNoAPIKey  → 503; the dashboard should prompt for a key
	//   - ErrAlreadyActive → 409 with the active call id
	//   - everything else → log + best-effort 500; the upgrade may or
	//     may not have completed before the failure, so writing a body
	//     after the upgrade is undefined behavior — we don't try.
	err = s.voice.HandleWS(r.Context(), w, r, uid)
	if err == nil {
		return
	}
	var dup *voice.ErrAlreadyActive
	switch {
	case errors.As(err, &dup):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":          "another voice call is already active",
			"active_call_id": dup.CallID,
		})
	case errors.Is(err, voice.ErrNoAPIKey):
		writeError(w, http.StatusServiceUnavailable,
			"voice not configured: set GEMINI_API_KEY on the gateway")
	default:
		// Upgrade may already have happened; writing to w may be a
		// no-op. The error is logged in HandleWS, so just return.
		return
	}
}

func (s *Server) voiceSessions(w http.ResponseWriter, r *http.Request) {
	if s.voice == nil {
		writeError(w, http.StatusServiceUnavailable, "voice not configured")
		return
	}
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": s.voice.Snapshot(),
	})
}

func (s *Server) voiceHangup(w http.ResponseWriter, r *http.Request) {
	if s.voice == nil {
		writeError(w, http.StatusServiceUnavailable, "voice not configured")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	hung := s.voice.Hangup(uid)
	writeJSON(w, http.StatusOK, map[string]any{"hung_up": hung})
}
