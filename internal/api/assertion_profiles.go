package api

import (
	"net/http"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
)

// assertionOAuthRefused prevents an assertion credential profile from acquiring
// an OAuth/PAT credential by dashboard, callback, paste or connection routes.
func (s *Server) assertionOAuthRefused(w http.ResponseWriter, r *http.Request, name string) bool {
	if s.upstreams == nil {
		return false
	}
	srv, err := s.upstreams.Get(r.Context(), name)
	if err != nil || srv.AssertionProfile == "" {
		return false
	}
	if s.audit != nil {
		_ = s.audit.Write(r.Context(), audit.Event{EventType: "upstream.credential_refused", UpstreamName: name, ResultSummary: "assertion profile refuses OAuth and static tokens"})
	}
	writeError(w, http.StatusBadRequest, "assertion profiles do not accept OAuth or static tokens")
	return true
}
