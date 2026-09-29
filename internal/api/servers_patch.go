package api

import (
	"errors"
	"net/http"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// serversPatch edits a saved server in place: PATCH /v1/servers/{name}.
// Admin-only. The body is a partial upstreams.Patch:
//
//	{"url"?, "headers"?, "env"?, "identity"?: {"header","register"} | null, "enabled"?}
//
// A field that is absent stays as it is; "identity": null switches
// forwarding off. The row is updated, never deleted and re-added, so the
// server's OAuth client and tokens survive the edit, except when the url
// moves to another origin: then they are dropped first (a bearer must not
// follow the server to a new host) and the response carries
// "oauth_reset": true. Responds with the masked server, 404 for an
// unknown name, 400 for invalid input and 403 for a reserved built-in.
func (s *Server) serversPatch(w http.ResponseWriter, r *http.Request, name string) {
	if _, err := s.requireAdmin(r); err != nil {
		writeAuthError(w, err)
		return
	}
	var patch upstreams.Patch
	if err := decode(r, &patch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	srv, err := s.upstreams.Update(r.Context(), name, patch)
	if srv != nil && srv.OAuthReset {
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType:     "oauth.disconnect",
			ResultSummary: name + " (url moved to another origin)",
		})
	}
	if err != nil {
		switch {
		case errors.Is(err, upstreams.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, upstreams.ErrReserved):
			writeError(w, http.StatusForbidden, "reserved built-in upstream cannot be edited")
		case errors.Is(err, upstreams.ErrInvalid):
			writeError(w, http.StatusBadRequest, err.Error())
		case srv != nil:
			// Saved, but the reconnect failed: hand back the row with its
			// recorded error, as POST /v1/servers does.
			writeJSON(w, http.StatusAccepted, map[string]any{"server": upstreams.Masked(*srv), "warning": err.Error()})
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, upstreams.Masked(*srv))
}
