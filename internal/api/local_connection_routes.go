package api

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/federation"
)

func (s *Server) localConnection(w http.ResponseWriter, r *http.Request) {
	if s.federation == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeError(w, 401, "agent_token_required")
		return
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	w.Header().Set("Cache-Control", "no-store")
	var err error
	switch strings.TrimPrefix(r.URL.Path, "/v1/connections/") {
	case "api-key":
		var b federation.LocalBinding
		if decode(r, &b) != nil {
			writeError(w, 400, "invalid_request")
			return
		}
		credential, e := s.federation.ConnectLocal(r.Context(), token, b)
		if e != nil {
			federationError(w, e)
			return
		}
		writeJSON(w, 200, credential)
	case "renew":
		var b struct {
			ExpectedVersion int `json:"expected_version"`
		}
		if decode(r, &b) != nil {
			writeError(w, 400, "invalid_request")
			return
		}
		credential, e := s.federation.RenewLocal(r.Context(), token, b.ExpectedVersion)
		if e != nil {
			federationError(w, e)
			return
		}
		writeJSON(w, 200, credential)
	case "revoke":
		if err = s.federation.RevokeLocal(r.Context(), token); err != nil {
			federationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case "handoff":
		code, expiry, e := s.federation.LocalHandoff(r.Context(), token)
		if e != nil {
			federationError(w, e)
			return
		}
		writeJSON(w, 200, map[string]string{"url": strings.TrimRight(s.security.PublicURL, "/") + "/connections/handoff?code=" + url.QueryEscape(code), "expires_at": expiry.UTC().Format(time.RFC3339)})
	default:
		http.NotFound(w, r)
	}
}
func (s *Server) localHandoff(w http.ResponseWriter, r *http.Request) {
	if s.federation == nil || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	user, err := s.federation.ConsumeLocalHandoff(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		writeError(w, 401, "handoff_expired_or_used")
		return
	}
	if s.issueSession(w, r, user) != nil {
		writeError(w, 500, "session_failed")
		return
	}
	http.Redirect(w, r, "/#inbox", http.StatusSeeOther)
}

func localConnectionPath(path string) bool {
	return path == "/v1/connections/api-key" || path == "/v1/connections/renew" || path == "/v1/connections/revoke" || path == "/v1/connections/handoff"
}
